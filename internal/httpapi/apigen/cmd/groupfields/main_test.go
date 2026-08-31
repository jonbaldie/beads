package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGroupSourceSplitsConfiguredOversizedStruct(t *testing.T) {
	var source strings.Builder
	source.WriteString("package generated\n\n// Problem is generated.\ntype Problem struct {\n")
	for i := 1; i <= 15; i++ {
		fmt.Fprintf(&source, "\t// Field%d docs.\n\tField%d string `json:\"field%d\"`\n", i, i, i)
	}
	source.WriteString("}\n\nfunc use(p Problem) string { return p.Field1 + p.Field15 }\n")

	got, err := groupSource([]byte(source.String()))
	if err != nil {
		t.Fatalf("groupSource: %v", err)
	}
	text := string(got)
	for _, want := range []string{
		"type Problem struct {\n\tProblemFields1\n\tProblemFields2\n}",
		"type ProblemFields1 struct {",
		"type ProblemFields2 struct {",
		"// Field1 docs.",
		"// Field15 docs.",
		"return p.Field1 + p.Field15",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("grouped source missing %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "Field1 string") != 1 || strings.Count(text, "Field15 string") != 1 {
		t.Fatalf("fields duplicated or lost:\n%s", text)
	}
	if _, err := groupSource(got); err != nil {
		t.Fatalf("regrouping generated source: %v", err)
	}
}

func TestGroupSourceLeavesOtherStructsUntouched(t *testing.T) {
	source := []byte("package generated\ntype Problem struct { A int }\ntype Other struct { A, B, C, D, E, F, G, H, I, J, K, L, M, N, O int }\n")
	got, err := groupSource(source)
	if err != nil {
		t.Fatalf("groupSource: %v", err)
	}
	text := string(got)
	if !strings.Contains(text, "type Problem struct{ A int }") ||
		!strings.Contains(text, "type Other struct{ A, B, C, D, E, F, G, H, I, J, K, L, M, N, O int }") ||
		strings.Contains(text, "Fields1") {
		t.Fatalf("groupSource changed the struct shape:\n%s", got)
	}
	if _, err := groupSource([]byte("package broken\ntype")); err == nil {
		t.Fatal("groupSource accepted invalid Go source")
	}
}

func TestGroupSourceHandlesChunkAndEditBoundaries(t *testing.T) {
	for _, tt := range []struct {
		fields int
		groups int
	}{
		{fields: 14, groups: 0},
		{fields: 15, groups: 2},
		{fields: 28, groups: 2},
		{fields: 29, groups: 3},
	} {
		t.Run(fmt.Sprintf("%d fields", tt.fields), func(t *testing.T) {
			var source strings.Builder
			source.WriteString("package generated\ntype Problem struct {\n")
			for i := 1; i <= tt.fields; i++ {
				fmt.Fprintf(&source, "Field%d int\n", i)
			}
			source.WriteString("}\n")
			got, err := groupSource([]byte(source.String()))
			if err != nil {
				t.Fatalf("groupSource: %v", err)
			}
			if count := strings.Count(string(got), "type ProblemFields"); count != tt.groups {
				t.Fatalf("group count = %d, want %d:\n%s", count, tt.groups, got)
			}
		})
	}

	source := "package generated\n" + oversizedStruct("Problem", 15) + oversizedStruct("CreateIssueRequest", 15)
	got, err := groupSource([]byte(source))
	if err != nil {
		t.Fatalf("groupSource(two edits): %v", err)
	}
	for _, name := range []string{"ProblemFields1", "ProblemFields2", "CreateIssueRequestFields1", "CreateIssueRequestFields2"} {
		if !strings.Contains(string(got), "type "+name+" struct") {
			t.Errorf("two-edit output missing %s:\n%s", name, got)
		}
	}
}

func TestLineStart(t *testing.T) {
	source := []byte("first\nsecond")
	if got := lineStart(source, 3); got != 0 {
		t.Fatalf("lineStart(first line) = %d, want 0", got)
	}
	if got := lineStart(source, len(source)); got != 6 {
		t.Fatalf("lineStart(second line) = %d, want 6", got)
	}
}

func TestRunRewritesFileAndReportsFailures(t *testing.T) {
	if err := run(nil); err == nil || !strings.Contains(err.Error(), "-file is required") {
		t.Fatalf("run(nil) error = %v", err)
	}
	if err := run([]string{"-unknown"}); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("unknown flag error = %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.go")
	if err := run([]string{"-file", missing}); err == nil || !strings.Contains(err.Error(), "read "+missing) {
		t.Fatalf("missing file error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "generated.go")
	if err := os.WriteFile(path, []byte("package generated\ntype Problem struct { A int }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-file", path}); err != nil {
		t.Fatalf("run: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("rewritten mode = %o, want existing mode 600 preserved", info.Mode().Perm())
	}

	broken := filepath.Join(t.TempDir(), "broken.go")
	if err := os.WriteFile(broken, []byte("package broken\ntype"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-file", broken}); err == nil || !strings.Contains(err.Error(), "group "+broken) {
		t.Fatalf("invalid source error = %v", err)
	}
}

func oversizedStruct(name string, fields int) string {
	var source strings.Builder
	fmt.Fprintf(&source, "type %s struct {\n", name)
	for i := 1; i <= fields; i++ {
		fmt.Fprintf(&source, "Field%d int\n", i)
	}
	source.WriteString("}\n")
	return source.String()
}
