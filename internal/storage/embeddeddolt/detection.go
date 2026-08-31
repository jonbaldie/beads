package embeddeddolt

import (
	"os"
	"path/filepath"
)

// HasRepository reports whether beadsDir contains an embedded Dolt repository.
// It owns the adapter's coarse .dolt marker probe: the marker must be a
// non-symlink directory with at least one entry, but entry names and private
// repository files are never interpreted or opened.
func HasRepository(beadsDir string) bool {
	root := filepath.Join(beadsDir, "embeddeddolt")
	if !isRepositoryRoot(root) {
		return false
	}
	databases, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, database := range databases {
		if hasDatabaseRepository(root, database) {
			return true
		}
	}
	return false
}

func isRepositoryRoot(root string) bool {
	info, err := os.Lstat(root)
	return err == nil && info.IsDir()
}

func hasDatabaseRepository(root string, database os.DirEntry) bool {
	if !database.IsDir() {
		return false
	}
	markerPath := filepath.Join(root, database.Name(), ".dolt")
	marker, err := os.Lstat(markerPath)
	if err != nil || !marker.IsDir() {
		return false
	}
	entries, err := os.ReadDir(markerPath)
	if err != nil {
		return false
	}
	return len(entries) > 0
}
