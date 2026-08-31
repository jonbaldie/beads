package fs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/jonbaldie/beads/internal/storage/domain"
)

func TestFilesystemFailuresAreReported(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	badDir := filepath.Join(blocker, "child")
	repo := &beadsDirFSRepositoryImpl{
		workDir:  badDir,
		beadsDir: badDir,
		templates: domain.BeadsDirTemplates{
			BeadsGitignore:         "dolt/\n",
			ProjectGitignoreHeader: "# beads",
			ProjectGitignorePatterns: []string{
				".beads/",
			},
			Readme: "readme",
		},
	}
	ctx := context.Background()
	checks := []struct {
		name string
		run  func() error
	}{
		{"CreateBeadsDir", func() error { return repo.CreateBeadsDir(ctx) }},
		{"BeadsDirExists", func() error { _, err := repo.BeadsDirExists(ctx); return err }},
		{"WriteBeadsGitignore", func() error { return repo.WriteBeadsGitignore(ctx) }},
		{"BeadsGitignoreExists", func() error { _, err := repo.BeadsGitignoreExists(ctx); return err }},
		{"WriteProjectGitignore", func() error { return repo.WriteProjectGitignore(ctx) }},
		{"ProjectGitignoreExists", func() error { _, err := repo.ProjectGitignoreExists(ctx); return err }},
		{"WriteInteractionsLog", func() error { return repo.WriteInteractionsLog(ctx) }},
		{"WriteReadme", func() error { return repo.WriteReadme(ctx) }},
		{"WriteMetadataJSON", func() error { return repo.WriteMetadataJSON(ctx, []byte("{}")) }},
		{"ReadMetadataJSON", func() error { _, err := repo.ReadMetadataJSON(ctx); return err }},
		{"WriteConfigYAML", func() error { return repo.WriteConfigYAML(ctx, []byte("x: y")) }},
		{"ReadConfigYAML", func() error { _, err := repo.ReadConfigYAML(ctx); return err }},
		{"ReadBeadsConfig", func() error { _, err := repo.ReadBeadsConfig(ctx); return err }},
		{"WriteProxiedServerClientInfo", func() error { return repo.WriteProxiedServerClientInfo(ctx, nil) }},
		{"ReadProxiedServerClientInfo", func() error { _, err := repo.ReadProxiedServerClientInfo(ctx); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			err := check.run()
			if err == nil {
				t.Fatal("filesystem failure returned nil error")
			}
			if !errors.Is(err, syscall.ENOTDIR) {
				t.Fatalf("error = %v, want wrapped ENOTDIR", err)
			}
		})
	}
}

func TestProjectGitignoreContentBoundaries(t *testing.T) {
	repo := &beadsDirFSRepositoryImpl{templates: domain.BeadsDirTemplates{ProjectGitignoreHeader: "# beads"}}
	tests := []struct {
		name     string
		existing string
		patterns []string
		want     string
	}{
		{"empty", "", []string{".beads/"}, "# beads\n.beads/\n"},
		{"missing trailing newline", "local", []string{".beads/"}, "local\n\n# beads\n.beads/\n"},
		{"existing newline", "local\n", []string{".beads/"}, "local\n\n# beads\n.beads/\n"},
		{"header already present", "# beads\n", []string{".beads/"}, "# beads\n.beads/\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(repo.projectGitignoreContent([]byte(tt.existing), tt.patterns)); got != tt.want {
				t.Fatalf("content = %q, want %q", got, tt.want)
			}
		})
	}
}
