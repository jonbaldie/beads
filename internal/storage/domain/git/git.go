package git

import "github.com/jonbaldie/beads/internal/storage/domain"

func NewGitRepository(workDir string) domain.GitRepository {
	return &gitRepositoryImpl{workDir: workDir}
}

type gitRepositoryImpl struct {
	workDir string
}

var _ domain.GitRepository = (*gitRepositoryImpl)(nil)

// IsJujutsuRepo walks upward from workDir looking for a .jj directory.
// Mirrors the boundary rule in internal/git.GetJujutsuRoot: a .git directory
// found before .jj terminates the walk so a nested git repo does not inherit
// an ancestor JJ workspace.
