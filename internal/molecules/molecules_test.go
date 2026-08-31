package molecules

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/testutil"
	"github.com/jonbaldie/beads/internal/types"
)

type memoryMoleculeStore struct {
	issues    map[string]*types.Issue
	createErr error
}

func (s *memoryMoleculeStore) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	return s.issues[id], nil
}

func (s *memoryMoleculeStore) CreateIssuesWithFullOptions(ctx context.Context, issues []*types.Issue, _ string, _ storage.BatchCreateOptions) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	if s.createErr != nil {
		return s.createErr
	}
	for _, issue := range issues {
		s.issues[issue.ID] = issue
	}
	return nil
}

func TestLoaderLoadAllFromProjectWithoutExternalDatabase(t *testing.T) {
	townRoot := t.TempDir()
	userHome := t.TempDir()
	t.Setenv("GT_ROOT", townRoot)
	t.Setenv("HOME", userHome)
	beadsDir := t.TempDir()
	paths := []string{
		filepath.Join(townRoot, ".beads", MoleculeFileName),
		filepath.Join(userHome, ".beads", MoleculeFileName),
		filepath.Join(beadsDir, MoleculeFileName),
	}
	for i, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			t.Fatal(err)
		}
		content := fmt.Sprintf(`{"id":"mol-local-%d","title":"Local","issue_type":"molecule","status":"open"}`, i)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	store := &memoryMoleculeStore{issues: make(map[string]*types.Issue)}
	loader := &Loader{
		store:    store,
		builtins: []*types.Issue{{IssueID: types.IssueID{ID: "mol-built-in"}}},
	}
	result, err := loader.LoadAll(context.Background(), beadsDir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	wantSources := append([]string{"<built-in>"}, paths...)
	if result.Loaded != 4 || result.BuiltinCount != 1 || !slices.Equal(result.Sources, wantSources) {
		t.Fatalf("LoadAll result = %#v, want town, user, and project sources", result)
	}
	if issue := store.issues["mol-local-2"]; issue == nil || !issue.IsTemplate {
		t.Fatalf("stored molecule = %#v, want template", issue)
	}
}

func TestLoadBuiltinRecordsSuccessAndIgnoresStoreFailure(t *testing.T) {
	ctx := context.Background()
	molecule := func(id string) *types.Issue {
		return &types.Issue{IssueID: types.IssueID{ID: id}}
	}
	empty := &LoadResult{}
	(&Loader{store: &memoryMoleculeStore{issues: make(map[string]*types.Issue)}}).loadBuiltin(ctx, empty, nil)
	if empty.BuiltinCount != 0 || empty.Loaded != 0 || len(empty.Sources) != 0 {
		t.Fatalf("empty built-in load changed result: %#v", empty)
	}

	store := &memoryMoleculeStore{issues: make(map[string]*types.Issue)}
	result := &LoadResult{}
	(&Loader{store: store}).loadBuiltin(ctx, result, []*types.Issue{molecule("mol-built-in")})
	if result.BuiltinCount != 1 || result.Loaded != 1 || !slices.Equal(result.Sources, []string{"<built-in>"}) {
		t.Fatalf("successful built-in result = %#v", result)
	}

	store.createErr = errors.New("write failed")
	failed := &LoadResult{}
	(&Loader{store: store}).loadBuiltin(ctx, failed, []*types.Issue{molecule("mol-failed")})
	if failed.BuiltinCount != 0 || failed.Loaded != 0 || len(failed.Sources) != 0 {
		t.Fatalf("failed built-in load changed result: %#v", failed)
	}
}

func TestLoadSourceIgnoresStoreFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), MoleculeFileName)
	if err := os.WriteFile(path, []byte(`{"id":"mol-failed","title":"Failed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	store := &memoryMoleculeStore{
		issues:    make(map[string]*types.Issue),
		createErr: errors.New("write failed"),
	}
	result := &LoadResult{}
	(&Loader{store: store}).loadSource(context.Background(), result, path, "test")
	if result.Loaded != 0 || len(result.Sources) != 0 {
		t.Fatalf("failed source load changed result: %#v", result)
	}
}

// newTestMoleculeStore creates a dolt store on the shared database with branch isolation.
func newTestMoleculeStore(t *testing.T) *dolt.DoltStore {
	t.Helper()
	if testSharedDB == "" {
		t.Skip("shared DB not available")
	}
	ctx := context.Background()
	store, err := dolt.New(ctx, &dolt.Config{
		Path: t.TempDir(),
		ServerOptions: dolt.ServerOptions{
			ServerHost: "127.0.0.1",
			ServerPort: testServerPort,
		},
		Database: testSharedDB,
		PoolOptions: dolt.PoolOptions{
			MaxOpenConns: 1,
		},
	})
	if err != nil {
		t.Fatalf("Failed to create dolt store: %v", err)
	}

	_, branchCleanup := testutil.StartTestBranch(t, store.DB(), testSharedDB)

	t.Cleanup(func() {
		branchCleanup()
		store.Close()
	})
	return store
}

func TestLoadMoleculesFromFile(t *testing.T) {
	// Create a temporary directory
	tempDir := t.TempDir()

	// Create a test molecules.jsonl file
	moleculesPath := filepath.Join(tempDir, "molecules.jsonl")
	content := `{"id":"mol-test-1","title":"Test Molecule 1","issue_type":"molecule","status":"open"}
{"id":"mol-test-2","title":"Test Molecule 2","issue_type":"molecule","status":"open"}`
	if err := os.WriteFile(moleculesPath, []byte(content), 0600); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Load molecules
	molecules, err := loadMoleculesFromFile(moleculesPath)
	if err != nil {
		t.Fatalf("Failed to load molecules: %v", err)
	}

	if len(molecules) != 2 {
		t.Errorf("Expected 2 molecules, got %d", len(molecules))
	}

	// Check that IsTemplate is set
	for _, mol := range molecules {
		if !mol.IsTemplate {
			t.Errorf("Molecule %s should have IsTemplate=true", mol.ID)
		}
	}

	// Check specific fields
	if molecules[0].ID != "mol-test-1" {
		t.Errorf("Expected ID 'mol-test-1', got '%s'", molecules[0].ID)
	}
	if molecules[0].Title != "Test Molecule 1" {
		t.Errorf("Expected Title 'Test Molecule 1', got '%s'", molecules[0].Title)
	}
}

func TestLoadMoleculesFromNonexistentFile(t *testing.T) {
	molecules, err := loadMoleculesFromFile("/nonexistent/path/molecules.jsonl")
	if err != nil {
		t.Errorf("Expected nil error for nonexistent file, got: %v", err)
	}
	if molecules != nil {
		t.Errorf("Expected nil molecules for nonexistent file, got: %v", molecules)
	}
}

func TestLoader_LoadAll(t *testing.T) {
	ctx := context.Background()
	store := newTestMoleculeStore(t)

	// Create temporary directories
	tempDir := t.TempDir()
	beadsDir := filepath.Join(tempDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0750); err != nil {
		t.Fatalf("Failed to create beads dir: %v", err)
	}

	// Create a project-level molecules.jsonl
	moleculesPath := filepath.Join(beadsDir, "molecules.jsonl")
	content := `{"id":"mol-feature","title":"Feature Template","issue_type":"molecule","status":"open","description":"Standard feature workflow"}
{"id":"mol-bugfix","title":"Bugfix Template","issue_type":"molecule","status":"open","description":"Bug fix workflow"}`
	if err := os.WriteFile(moleculesPath, []byte(content), 0600); err != nil {
		t.Fatalf("Failed to write molecules file: %v", err)
	}

	// Load molecules
	loader := NewLoader(store)
	result, err := loader.LoadAll(ctx, beadsDir)
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}

	if result.Loaded != 2 {
		t.Errorf("Expected 2 loaded molecules, got %d", result.Loaded)
	}

	// Verify molecules are in the database
	mol1, err := store.GetIssue(ctx, "mol-feature")
	if err != nil {
		t.Fatalf("Failed to get mol-feature: %v", err)
	}
	if mol1 == nil {
		t.Fatal("mol-feature not found in database")
	}
	if !mol1.IsTemplate {
		t.Error("mol-feature should be marked as template")
	}
	if mol1.Title != "Feature Template" {
		t.Errorf("Expected title 'Feature Template', got '%s'", mol1.Title)
	}

	mol2, err := store.GetIssue(ctx, "mol-bugfix")
	if err != nil {
		t.Fatalf("Failed to get mol-bugfix: %v", err)
	}
	if mol2 == nil {
		t.Fatal("mol-bugfix not found in database")
	}
	if !mol2.IsTemplate {
		t.Error("mol-bugfix should be marked as template")
	}
}

func TestLoader_SkipExistingMolecules(t *testing.T) {
	ctx := context.Background()
	store := newTestMoleculeStore(t)

	// Create temporary directories
	tempDir := t.TempDir()
	beadsDir := filepath.Join(tempDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0750); err != nil {
		t.Fatalf("Failed to create beads dir: %v", err)
	}

	// Pre-create a molecule in the database (skip prefix validation for mol-* IDs)
	existingMol := &types.Issue{
		IssueID: types.IssueID{
			ID: "mol-existing",
		},
		IssueContent: types.IssueContent{
			Title: "Existing Molecule",
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: "molecule",
			Status:    types.StatusOpen,
		},
		IssueWisp: types.IssueWisp{
			IsTemplate: true,
		},
	}
	opts := storage.BatchCreateOptions{SkipPrefixValidation: true}
	if err := store.CreateIssuesWithFullOptions(ctx, []*types.Issue{existingMol}, "test", opts); err != nil {
		t.Fatalf("Failed to create existing molecule: %v", err)
	}

	// Create a molecules.jsonl with the same ID
	moleculesPath := filepath.Join(beadsDir, "molecules.jsonl")
	content := `{"id":"mol-existing","title":"Updated Molecule","issue_type":"molecule","status":"open"}
{"id":"mol-new","title":"New Molecule","issue_type":"molecule","status":"open"}`
	if err := os.WriteFile(moleculesPath, []byte(content), 0600); err != nil {
		t.Fatalf("Failed to write molecules file: %v", err)
	}

	// Load molecules
	loader := NewLoader(store)
	result, err := loader.LoadAll(ctx, beadsDir)
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}

	// Should only load the new one (existing one is skipped)
	if result.Loaded != 1 {
		t.Errorf("Expected 1 loaded molecule, got %d", result.Loaded)
	}

	// Verify the existing molecule wasn't updated
	mol, err := store.GetIssue(ctx, "mol-existing")
	if err != nil {
		t.Fatalf("Failed to get mol-existing: %v", err)
	}
	if mol.Title != "Existing Molecule" {
		t.Errorf("Expected title 'Existing Molecule' (unchanged), got '%s'", mol.Title)
	}
}

func TestGetBuiltinMolecules(t *testing.T) {
	molecules := getBuiltinMolecules()
	// For now, we expect no built-in molecules (can be added later)
	if molecules == nil {
		// This is expected for now
		return
	}
	// When built-in molecules are added, verify they all have IsTemplate=true
	for _, mol := range molecules {
		if !mol.IsTemplate {
			t.Errorf("Built-in molecule %s should have IsTemplate=true", mol.ID)
		}
	}
}
