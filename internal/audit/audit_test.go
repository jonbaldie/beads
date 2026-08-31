package audit

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAppendRejectsInvalidEntries(t *testing.T) {
	for name, entry := range map[string]*Entry{
		"nil":          nil,
		"missing kind": {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Append(entry); err == nil {
				t.Fatal("Append() error = nil, want validation error")
			}
		})
	}
}

func TestAppendPropagatesPreparationError(t *testing.T) {
	setUpAuditProject(t)
	wantErr := errors.New("prepare failed")
	_, err := appendEntry(&Entry{Kind: "test"}, func(*Entry) error { return wantErr }, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("appendEntry() error = %v, want %v", err, wantErr)
	}
}

func TestAppendPropagatesWriteError(t *testing.T) {
	setUpAuditProject(t)
	wantErr := errors.New("write failed")
	_, err := appendEntry(
		&Entry{Kind: "test"},
		prepareEntry,
		func(*os.File, []byte) (int, error) { return 0, wantErr },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("appendEntry() error = %v, want %v", err, wantErr)
	}
}

func TestPrepareEntryPropagatesIDError(t *testing.T) {
	wantErr := errors.New("ID failed")
	err := prepareEntryWithID(&Entry{Kind: "test"}, func() (string, error) { return "", wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("prepareEntryWithID() error = %v, want %v", err, wantErr)
	}
}

func setUpAuditProject(t *testing.T) {
	t.Helper()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"backend":"dolt"}`), 0644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	t.Setenv("BEADS_DIR", beadsDir)
}

func TestAppend_CreatesFileAndWritesJSONL(t *testing.T) {
	tmp := t.TempDir()
	beadsDir := filepath.Join(tmp, ".beads")
	if err := os.MkdirAll(beadsDir, 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// beads.FindBeadsDir() validates that the directory contains project files.
	// Create metadata.json so BEADS_DIR is accepted by hasBeadsProjectFiles.
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	if err := os.WriteFile(metadataPath, []byte(`{"backend":"dolt"}`), 0644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	t.Setenv("BEADS_DIR", beadsDir)
	t.Setenv("BD_AUDIT_ENABLED", "1")

	id1, err := Append(&Entry{Kind: "llm_call", Model: "test-model", Prompt: "p", Response: "r"})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if id1 == "" {
		t.Fatalf("expected id")
	}
	_, err = Append(&Entry{Kind: "label", ParentID: id1, Label: "good", Reason: "ok"})
	if err != nil {
		t.Fatalf("append label: %v", err)
	}

	p := filepath.Join(beadsDir, FileName)
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	lines := 0
	for sc.Scan() {
		lines++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if lines != 2 {
		t.Fatalf("expected 2 lines, got %d", lines)
	}
}

func TestAppendIfEnabled_DisabledDoesNotCreateFile(t *testing.T) {
	tmp := t.TempDir()
	beadsDir := filepath.Join(tmp, ".beads")
	if err := os.MkdirAll(beadsDir, 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	if err := os.WriteFile(metadataPath, []byte(`{"backend":"dolt"}`), 0644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	t.Setenv("BEADS_DIR", beadsDir)
	t.Setenv("BD_AUDIT_ENABLED", "")

	if _, err := AppendIfEnabled(&Entry{Kind: "llm_call", Model: "test-model"}); err == nil {
		t.Fatalf("expected disabled append to fail")
	}

	if _, err := os.Stat(filepath.Join(beadsDir, FileName)); !os.IsNotExist(err) {
		t.Fatalf("disabled append created %s: %v", FileName, err)
	}
}

func TestEnsureFile_DoesNotTruncateWhenConcurrentCreatorWins(t *testing.T) {
	tmp := t.TempDir()
	beadsDir := filepath.Join(tmp, ".beads")
	if err := os.MkdirAll(beadsDir, 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	if err := os.WriteFile(metadataPath, []byte(`{"backend":"dolt"}`), 0644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	t.Setenv("BEADS_DIR", beadsDir)

	wantContents := []byte("{\"id\":\"seed\"}\n")

	oldHook := ensureFileBeforeCreateHook
	defer func() { ensureFileBeforeCreateHook = oldHook }()

	var once sync.Once
	ensureFileBeforeCreateHook = func(path string) {
		once.Do(func() {
			if err := os.WriteFile(path, wantContents, 0644); err != nil {
				t.Fatalf("seed interactions log: %v", err)
			}
		})
	}

	gotPath, err := EnsureFile()
	if err != nil {
		t.Fatalf("EnsureFile: %v", err)
	}

	gotContents, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatalf("read interactions log: %v", err)
	}
	if !bytes.Equal(gotContents, wantContents) {
		t.Fatalf("EnsureFile truncated seeded content: got %q, want %q", gotContents, wantContents)
	}
}
