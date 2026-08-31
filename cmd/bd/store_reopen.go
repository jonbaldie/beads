package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/utils"
)

// openReadOnlyStoreForDBPath reopens a read-only store from an existing dbPath
// while preserving repo-local metadata such as dolt_database and the resolved
// Dolt server port. Falls back to deriving the beads directory from the dbPath
// parent when no matching metadata.json can be found.
func openReadOnlyStoreForDBPath(ctx context.Context, dbPath string) (storage.DoltStorage, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("no database path available")
	}

	if beadsDir := resolveBeadsDirForDBPath(dbPath); beadsDir != "" {
		return newReadOnlyStoreFromConfig(ctx, beadsDir)
	}

	// Fallback: derive beads dir from dbPath parent directory.
	return newReadOnlyStoreFromConfig(ctx, filepath.Dir(dbPath))
}

// resolveBeadsDirForDBPath maps a database path back to its owning .beads
// directory when metadata.json is available. This is needed for repos that use
// non-default dolt_database names or custom dolt_data_dir locations.
func resolveBeadsDirForDBPath(dbPath string) string {
	actualDBPath := utils.CanonicalizePath(dbPath)
	if beadsDir := beadsDirWithMetadata(filepath.Dir(dbPath)); beadsDir != "" {
		return beadsDir
	}
	if beadsDir := beadsDirWithMetadata(filepath.Dir(actualDBPath)); beadsDir != "" {
		return beadsDir
	}
	for _, beadsDir := range beadsDirCandidates(dbPath, actualDBPath) {
		if matchBeadsDirForDBPath(beadsDir, dbPath, actualDBPath) {
			return beadsDir
		}
	}
	return ""
}

func beadsDirWithMetadata(parent string) string {
	if filepath.Base(parent) != ".beads" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(parent, "metadata.json")); err != nil {
		return ""
	}
	return parent
}

type beadsDirCandidateSet struct {
	seen       map[string]struct{}
	candidates []string
}

func (s *beadsDirCandidateSet) add(path string) {
	if path == "" {
		return
	}
	key := utils.NormalizePathForComparison(path)
	if key == "" {
		return
	}
	if _, ok := s.seen[key]; ok {
		return
	}
	s.seen[key] = struct{}{}
	s.candidates = append(s.candidates, path)
}

func (s *beadsDirCandidateSet) addAncestors(path string) {
	for dir := path; dir != "" && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		s.add(filepath.Join(dir, ".beads"))
		if filepath.Base(dir) == ".beads" {
			s.add(dir)
		}
	}
}

func (s *beadsDirCandidateSet) addIfDir(path string) {
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		s.add(path)
	}
}

func beadsDirCandidates(dbPath, actualDBPath string) []string {
	s := beadsDirCandidateSet{seen: map[string]struct{}{}, candidates: make([]string, 0, 16)}
	s.addIfDir(dbPath)
	s.addIfDir(actualDBPath)
	s.add(filepath.Dir(dbPath))
	s.add(filepath.Dir(actualDBPath))
	s.addAncestors(filepath.Dir(dbPath))
	s.addAncestors(filepath.Dir(actualDBPath))
	if found := beads.FindBeadsDir(); found != "" {
		s.add(found)
		s.add(utils.CanonicalizePath(found))
	}
	return s.candidates
}

func matchBeadsDirForDBPath(beadsDir, dbPath, actualDBPath string) bool {
	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil {
		return false
	}
	if utils.PathsEqual(beadsDir, dbPath) || utils.PathsEqual(beadsDir, actualDBPath) {
		return true
	}
	// dbPath is the data directory living directly inside beadsDir
	// (<beadsDir>/embeddeddolt for embedded, <beadsDir>/dolt for server).
	// Match on that parent relationship so resolution works for embedded
	// stores regardless of the beads dir name; Config.DatabasePath alone
	// returns the "dolt" name and misses embeddeddolt/ (GH#4574).
	if utils.PathsEqual(filepath.Dir(dbPath), beadsDir) || utils.PathsEqual(filepath.Dir(actualDBPath), beadsDir) {
		return true
	}
	return utils.PathsEqual(cfg.DatabasePath(beadsDir), dbPath) || utils.PathsEqual(cfg.DatabasePath(beadsDir), actualDBPath)
}
