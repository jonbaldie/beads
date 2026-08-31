package fs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/utils"
)

func (r *beadsDirFSRepositoryImpl) ResolveBeadsDirPath(_ context.Context) domain.BeadsDirResolution {
	return domain.BeadsDirResolution{BeadsDir: r.beadsDir, HasExplicit: r.hasExplicit}
}
func (r *beadsDirFSRepositoryImpl) BeadsDirIsLocal(_ context.Context) bool {
	workDir := filepath.Clean(utils.CanonicalizePath(r.workDir))
	beadsDir := filepath.Clean(utils.CanonicalizePath(r.beadsDir))
	if beadsDir == workDir {
		return true
	}
	rel, err := filepath.Rel(workDir, beadsDir)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func (r *beadsDirFSRepositoryImpl) CreateBeadsDir(_ context.Context) error {
	if r.beadsDir == "" {
		return fmt.Errorf("fs: CreateBeadsDir: beadsDir not resolved")
	}
	if err := os.MkdirAll(r.beadsDir, config.BeadsDirPerm); err != nil {
		return fmt.Errorf("fs: CreateBeadsDir: mkdir %s: %w", r.beadsDir, err)
	}
	if _, err := config.FixBeadsDirPermissions(r.beadsDir); err != nil {
		return fmt.Errorf("fs: CreateBeadsDir: fix perms %s: %w", r.beadsDir, err)
	}
	return nil
}
func (r *beadsDirFSRepositoryImpl) BeadsDirExists(_ context.Context) (bool, error) {
	info, err := os.Stat(r.beadsDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("fs: BeadsDirExists: stat %s: %w", r.beadsDir, err)
	}
	return info.IsDir(), nil
}
func (r *beadsDirFSRepositoryImpl) WriteBeadsGitignore(_ context.Context) error {
	if r.templates.BeadsGitignore == "" {
		return fmt.Errorf("fs: WriteBeadsGitignore: template not configured")
	}
	path := filepath.Join(r.beadsDir, ".gitignore")
	// #nosec G304 -- path joined under bound beadsDir
	existing, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if werr := os.WriteFile(path, []byte(r.templates.BeadsGitignore), 0600); werr != nil {
			return fmt.Errorf("fs: WriteBeadsGitignore: %w", werr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("fs: WriteBeadsGitignore: read: %w", err)
	}
	// Existing file: append-only. A wholesale rewrite to the template
	// destroys local rules (e.g. export negations) the user added — the
	// gitignore is shared state, not bd-owned (bd-kaaz3).
	missing := missingTemplatePatternLines(string(existing), r.templates.BeadsGitignore)
	if len(missing) == 0 {
		return nil
	}
	content := string(existing)
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += "\n# Added by bd (missing required patterns)\n" + strings.Join(missing, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return fmt.Errorf("fs: WriteBeadsGitignore: %w", err)
	}
	return nil
}
func (r *beadsDirFSRepositoryImpl) BeadsGitignoreExists(_ context.Context) (bool, error) {
	return fileExists(filepath.Join(r.beadsDir, ".gitignore"), "fs: BeadsGitignoreExists")
}
func (r *beadsDirFSRepositoryImpl) WriteProjectGitignore(_ context.Context) error {
	if err := r.validateProjectGitignore(); err != nil {
		return err
	}
	existing, err := r.readProjectGitignore()
	if err != nil {
		return err
	}
	toAdd := r.missingProjectGitignorePatterns(existing)
	if len(toAdd) == 0 {
		return nil
	}
	content := r.projectGitignoreContent(existing, toAdd)
	path := filepath.Join(r.workDir, ".gitignore")
	// #nosec G306 -- .gitignore must be world-readable so users can read/edit it
	if err := os.WriteFile(path, content, 0644); err != nil {
		return fmt.Errorf("fs: WriteProjectGitignore: write: %w", err)
	}
	return nil
}
func (r *beadsDirFSRepositoryImpl) validateProjectGitignore() error {
	if r.workDir == "" {
		return fmt.Errorf("fs: WriteProjectGitignore: workDir not set")
	}
	if len(r.templates.ProjectGitignorePatterns) == 0 {
		return fmt.Errorf("fs: WriteProjectGitignore: patterns not configured")
	}
	return nil
}
func (r *beadsDirFSRepositoryImpl) readProjectGitignore() ([]byte, error) {
	path := filepath.Join(r.workDir, ".gitignore")
	// #nosec G304 -- path joined under bound workDir
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("fs: WriteProjectGitignore: read: %w", err)
	}
	return existing, nil
}
func (r *beadsDirFSRepositoryImpl) missingProjectGitignorePatterns(existing []byte) []string {
	var toAdd []string
	for _, pattern := range r.templates.ProjectGitignorePatterns {
		if !containsLine(existing, pattern) {
			toAdd = append(toAdd, pattern)
		}
	}
	return toAdd
}
func (r *beadsDirFSRepositoryImpl) projectGitignoreContent(existing []byte, patterns []string) []byte {
	var buf bytes.Buffer
	buf.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		buf.WriteByte('\n')
	}
	if header := r.templates.ProjectGitignoreHeader; header != "" && !containsLine(existing, header) {
		if len(existing) > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(header + "\n")
	}
	for _, pattern := range patterns {
		buf.WriteString(pattern + "\n")
	}
	return buf.Bytes()
}
func (r *beadsDirFSRepositoryImpl) ProjectGitignoreExists(_ context.Context) (bool, error) {
	return fileExists(filepath.Join(r.workDir, ".gitignore"), "fs: ProjectGitignoreExists")
}
func (r *beadsDirFSRepositoryImpl) WriteInteractionsLog(_ context.Context) error {
	path := filepath.Join(r.beadsDir, "interactions.jsonl")
	switch _, err := os.Stat(path); {
	case err == nil:
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("fs: WriteInteractionsLog: stat: %w", err)
	}
	// #nosec G306 -- interactions log is consumed by user tooling
	if err := os.WriteFile(path, []byte{}, 0644); err != nil {
		return fmt.Errorf("fs: WriteInteractionsLog: write: %w", err)
	}
	return nil
}
func (r *beadsDirFSRepositoryImpl) WriteReadme(_ context.Context) error {
	if r.templates.Readme == "" {
		return fmt.Errorf("fs: WriteReadme: template not configured")
	}
	path := filepath.Join(r.beadsDir, "README.md")
	if _, err := os.Stat(path); err == nil {
		return nil // preserve any user edits
	}
	// #nosec G306 -- README should be world-readable
	if err := os.WriteFile(path, []byte(r.templates.Readme), 0644); err != nil {
		return fmt.Errorf("fs: WriteReadme: %w", err)
	}
	return nil
}
func (r *beadsDirFSRepositoryImpl) WriteMetadataJSON(_ context.Context, content []byte) error {
	path := filepath.Join(r.beadsDir, "metadata.json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		return fmt.Errorf("fs: WriteMetadataJSON: %w", err)
	}
	return nil
}
func (r *beadsDirFSRepositoryImpl) ReadMetadataJSON(_ context.Context) ([]byte, error) {
	path := filepath.Join(r.beadsDir, "metadata.json")
	// #nosec G304 -- path joined under bound beadsDir
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fs: ReadMetadataJSON: %w", err)
	}
	return data, nil
}
func (r *beadsDirFSRepositoryImpl) WriteConfigYAML(_ context.Context, content []byte) error {
	path := filepath.Join(r.beadsDir, "config.yaml")
	if err := os.WriteFile(path, content, 0600); err != nil {
		return fmt.Errorf("fs: WriteConfigYAML: %w", err)
	}
	return nil
}
func (r *beadsDirFSRepositoryImpl) ReadConfigYAML(_ context.Context) ([]byte, error) {
	path := filepath.Join(r.beadsDir, "config.yaml")
	// #nosec G304 -- path joined under bound beadsDir
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fs: ReadConfigYAML: %w", err)
	}
	return data, nil
}
func (r *beadsDirFSRepositoryImpl) ReadBeadsConfig(_ context.Context) (*configfile.Config, error) {
	if r.beadsDir == "" {
		return nil, fmt.Errorf("fs: ReadBeadsConfig: beadsDir not resolved")
	}
	cfg, err := configfile.Load(r.beadsDir)
	if err != nil {
		return nil, fmt.Errorf("fs: ReadBeadsConfig: %w", err)
	}
	return cfg, nil
}
func (r *beadsDirFSRepositoryImpl) WriteProxiedServerClientInfo(_ context.Context, info *configfile.ProxiedServerClientInfo) error {
	if r.beadsDir == "" {
		return fmt.Errorf("fs: WriteProxiedServerClientInfo: beadsDir not resolved")
	}
	if err := configfile.SaveProxiedServerClientInfo(r.beadsDir, info); err != nil {
		return fmt.Errorf("fs: WriteProxiedServerClientInfo: %w", err)
	}
	return nil
}
func (r *beadsDirFSRepositoryImpl) ReadProxiedServerClientInfo(_ context.Context) (*configfile.ProxiedServerClientInfo, error) {
	if r.beadsDir == "" {
		return nil, fmt.Errorf("fs: ReadProxiedServerClientInfo: beadsDir not resolved")
	}
	info, err := configfile.LoadProxiedServerClientInfo(r.beadsDir)
	if err != nil {
		return nil, fmt.Errorf("fs: ReadProxiedServerClientInfo: %w", err)
	}
	return info, nil
}
