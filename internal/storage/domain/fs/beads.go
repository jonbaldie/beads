package fs

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/utils"
)

func NewBeadsDirFSRepository(workDir string, templates domain.BeadsDirTemplates) domain.BeadsDirFSRepository {
	beadsDir, hasExplicit := resolveBeadsDir(workDir)
	return &beadsDirFSRepositoryImpl{
		workDir:     workDir,
		beadsDir:    beadsDir,
		hasExplicit: hasExplicit,
		templates:   templates,
	}
}

type beadsDirFSRepositoryImpl struct {
	workDir     string
	beadsDir    string
	hasExplicit bool
	templates   domain.BeadsDirTemplates
}

var _ domain.BeadsDirFSRepository = (*beadsDirFSRepositoryImpl)(nil)

func resolveBeadsDir(workDir string) (string, bool) {
	if envBeadsDir := os.Getenv("BEADS_DIR"); envBeadsDir != "" {
		return utils.CanonicalizePath(envBeadsDir), true
	}
	return beads.ResolveBeadsDirForRepo(workDir), false
}

// missingTemplatePatternLines returns the template's pattern lines (non-blank,
// non-comment) that existing does not already contain as an exact trimmed line.
func missingTemplatePatternLines(existing, template string) []string {
	have := make(map[string]bool)
	for _, line := range strings.Split(existing, "\n") {
		have[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, line := range strings.Split(template, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || have[trimmed] {
			continue
		}
		missing = append(missing, trimmed)
	}
	return missing
}

func fileExists(path, opLabel string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: stat %s: %w", opLabel, path, err)
	}
	return !info.IsDir(), nil
}

func containsLine(content []byte, line string) bool {
	s := bufio.NewScanner(bytes.NewReader(content))
	for s.Scan() {
		if strings.TrimSpace(s.Text()) == line {
			return true
		}
	}
	return false
}
