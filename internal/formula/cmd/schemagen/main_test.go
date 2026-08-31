package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunGeneratesRequestedOutput(t *testing.T) {
	dir := t.TempDir()
	typesPath := filepath.Join(dir, "types.go")
	outPath := filepath.Join(dir, "schema_gen.go")
	if err := os.WriteFile(typesPath, []byte("package fixture\ntype Item struct { Name string `json:\"name\"` }\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{"-types", typesPath, "-out", outPath}); err != nil {
		t.Fatalf("run: %v", err)
	}
	generated, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(generated), `Name: "Item"`) || !strings.Contains(string(generated), `JSONName: "name"`) {
		t.Fatalf("generated output:\n%s", generated)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("output mode = %o, want 600", info.Mode().Perm())
	}
}

func TestRunReportsArgumentGenerationAndWriteErrors(t *testing.T) {
	if err := run([]string{"-unknown"}); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("unknown flag error = %v", err)
	} else if errors.Unwrap(err) == nil {
		t.Fatalf("unknown flag error should wrap the parser error: %v", err)
	}
	missingDir := t.TempDir()
	if err := run([]string{"-types", filepath.Join(missingDir, "missing.go"), "-out", filepath.Join(missingDir, "unexpected.go")}); err == nil || !strings.Contains(err.Error(), "schemagen:") {
		t.Fatalf("missing input error = %v", err)
	}

	dir := t.TempDir()
	typesPath := filepath.Join(dir, "types.go")
	if err := os.WriteFile(typesPath, []byte("package fixture\ntype Item struct{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-types", typesPath, "-out", dir}); err == nil || !strings.Contains(err.Error(), "schemagen: write "+dir) {
		t.Fatalf("write error = %v", err)
	} else if errors.Unwrap(err) == nil {
		t.Fatalf("write error should wrap the filesystem error: %v", err)
	}
}
