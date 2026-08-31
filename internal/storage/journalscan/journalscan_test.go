package journalscan

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestSQLWritesBeadTable pins the DML detector the completeness guards rest on.
// A false negative here silently disarms every guard, so the templated forms —
// plain %s and the explicit-argument-index %[1]s — are covered alongside the
// literal table names.
func TestSQLWritesBeadTable(t *testing.T) {
	writes := []string{
		"INSERT INTO issues (id) VALUES (?)",
		"insert into issues (id) values (?)",
		"INSERT IGNORE INTO wisp_labels (issue_id, label) VALUES (?, ?)",
		"REPLACE INTO comments (id) VALUES (?)",
		"UPDATE wisps SET status = ? WHERE id = ?",
		"DELETE FROM dependencies WHERE issue_id = ?",
		"INSERT INTO %s (issue_id, label) VALUES (?, ?)",
		"INSERT INTO %[1]s (parent_id, last_child) VALUES (?, ?)",
		"UPDATE %[2]s SET x = 1 WHERE id = ?",
		"\n\t\tDELETE FROM  wisp_comments\n\t\tWHERE issue_id = ?\n\t",
		"DELETE FROM `issues` WHERE id = ?",
	}
	for _, lit := range writes {
		if !SQLWritesBeadTable(lit) {
			t.Errorf("SQLWritesBeadTable(%q) = false, want true", lit)
		}
	}

	reads := []string{
		"SELECT id FROM issues WHERE id = ?",
		"SELECT COUNT(*) FROM dependencies",
		// Aux tables that are not work-bead state.
		"INSERT INTO events (id) VALUES (?)",
		"DELETE FROM leases WHERE issue_id = ?",
		"UPDATE config SET value = ? WHERE `key` = ?",
		"INSERT INTO bd_events_journal (seq) VALUES (?)",
		// A table whose name merely starts with a bead table's name.
		"INSERT INTO issues_archive (id) VALUES (?)",
	}
	for _, lit := range reads {
		if SQLWritesBeadTable(lit) {
			t.Errorf("SQLWritesBeadTable(%q) = true, want false", lit)
		}
	}
}

func TestParsePackageCapturesFunctionShape(t *testing.T) {
	dir := t.TempDir()
	source := `package fixture

type store struct{}

func helper() {}
func (s *store) Mutate() {
	helper()
	s.Exec("UPDATE issues SET status = ? WHERE id = ?")
}
func readOnly() { helper() }
var ignored = 1
`
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture_test.go"), []byte("package fixture\nfunc testOnly() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fns, err := ParsePackage(dir)
	if err != nil {
		t.Fatalf("ParsePackage: %v", err)
	}
	if len(fns) != 3 {
		t.Fatalf("ParsePackage returned keys %v, want three production functions", mapKeys(fns))
	}
	mutate := fns["store.Mutate"]
	if mutate == nil {
		t.Fatalf("missing store.Mutate in %v", mapKeys(fns))
	}
	if mutate.Name != "Mutate" || mutate.Recv != "store" || !mutate.Exported || !mutate.OwnBeadDML {
		t.Fatalf("store.Mutate = %+v", mutate)
	}
	if !slices.Equal(mutate.IdentCalls, []string{"helper"}) || !slices.Equal(mutate.SelCalls, []string{"Exec"}) {
		t.Fatalf("store.Mutate calls = identifiers %v, selectors %v", mutate.IdentCalls, mutate.SelCalls)
	}
	if got := mutate.AllCallNames(); !slices.Equal(got, []string{"helper", "Exec"}) {
		t.Fatalf("AllCallNames() = %v", got)
	}
	if !mutate.CallsAnyOf(map[string]bool{"Exec": true}) || mutate.CallsAnyOf(map[string]bool{"missing": true}) {
		t.Fatalf("CallsAnyOf did not distinguish present and absent calls")
	}
	if fns["readOnly"].OwnBeadDML {
		t.Fatal("readOnly incorrectly classified as writing bead state")
	}
	if _, ok := fns["testOnly"]; ok {
		t.Fatal("ParsePackage included a _test.go function")
	}
}

func TestFixpointPropagatesAcrossFreeFunctionsAndMethods(t *testing.T) {
	fns := map[string]*FuncInfo{
		"seed":         {Name: "seed", OwnBeadDML: true},
		"middle":       {Name: "middle", IdentCalls: []string{"seed"}},
		"store.Top":    {Recv: "store", Name: "Top", SelCalls: []string{"middle"}},
		"other.Middle": {Recv: "other", Name: "Middle", IdentCalls: []string{"seed"}},
		"byMethodName": {Name: "byMethodName", SelCalls: []string{"Middle"}},
		"unrelated":    {Name: "unrelated", IdentCalls: []string{"missing"}},
	}
	got := Fixpoint(fns, func(f *FuncInfo) bool { return f.OwnBeadDML }, func(f *FuncInfo) []string { return f.AllCallNames() })
	for _, key := range []string{"seed", "middle", "store.Top", "other.Middle", "byMethodName"} {
		if !got[key] {
			t.Errorf("Fixpoint omitted %q: %v", key, got)
		}
	}
	if got["unrelated"] || len(got) != 5 {
		t.Fatalf("Fixpoint result = %v, want five related functions", got)
	}
}

func TestParsePackageRejectsInvalidGo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package broken\nfunc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePackage(dir); err == nil {
		t.Fatal("ParsePackage accepted invalid Go source")
	}
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
