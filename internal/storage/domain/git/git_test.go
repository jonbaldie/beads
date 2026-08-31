package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonbaldie/beads/internal/storage/domain"
)

func TestCommandFailuresAreReported(t *testing.T) {
	repo := &gitRepositoryImpl{workDir: filepath.Join(t.TempDir(), "missing")}
	ctx := context.Background()

	checks := []struct {
		name string
		run  func() error
	}{
		{"IsGitRepo", func() error { _, err := repo.IsGitRepo(ctx); return err }},
		{"IsBareGitRepo", func() error { _, err := repo.IsBareGitRepo(ctx); return err }},
		{"GetConfig", func() error { _, _, err := repo.GetConfig(ctx, "beads.role"); return err }},
		{"SetConfig", func() error { return repo.SetConfig(ctx, "beads.role", "maintainer") }},
		{"GetRemoteURL", func() error { _, _, err := repo.GetRemoteURL(ctx, "origin"); return err }},
		{"ListRemoteNames", func() error { _, err := repo.ListRemoteNames(ctx); return err }},
		{"CurrentBranch", func() error { _, err := repo.CurrentBranch(ctx); return err }},
		{"Add", func() error { return repo.Add(ctx, "file") }},
		{"Commit", func() error {
			_, err := repo.Commit(ctx, domain.GitCommitParams{Message: "message"})
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); err == nil {
				t.Fatal("command failure returned nil error")
			}
		})
	}
}

func (s *testSuite) TestIsGitRepo_FalseOutsideRepo() {
	ok, err := s.repo.IsGitRepo(s.Ctx())
	s.Require().NoError(err)
	s.False(ok)
}

func (s *testSuite) TestIsGitRepo_TrueInsideRepo() {
	s.gitInit()
	ok, err := s.repo.IsGitRepo(s.Ctx())
	s.Require().NoError(err)
	s.True(ok)
}

func (s *testSuite) TestIsBareGitRepo_FalseForRegularRepo() {
	s.gitInit()
	bare, err := s.repo.IsBareGitRepo(s.Ctx())
	s.Require().NoError(err)
	s.False(bare)
}

func (s *testSuite) TestIsBareGitRepo_FalseOutsideRepo() {
	bare, err := s.repo.IsBareGitRepo(s.Ctx())
	s.Require().NoError(err)
	s.False(bare)
}

func (s *testSuite) TestIsBareGitRepo_TrueForBareRepo() {
	s.run("git", "init", "--bare", "-q")
	bare, err := s.repo.IsBareGitRepo(s.Ctx())
	s.Require().NoError(err)
	s.True(bare)
}

func (s *testSuite) TestInit_CreatesRepo() {
	s.Require().NoError(s.repo.Init(s.Ctx()))
	ok, err := s.repo.IsGitRepo(s.Ctx())
	s.Require().NoError(err)
	s.True(ok)
}

func (s *testSuite) TestConfig_GetMissing() {
	s.gitInit()
	value, found, err := s.repo.GetConfig(s.Ctx(), "beads.role")
	s.Require().NoError(err)
	s.False(found)
	s.Empty(value)
}

func (s *testSuite) TestConfig_RoundTrip() {
	s.gitInit()
	s.Require().NoError(s.repo.SetConfig(s.Ctx(), "beads.role", "maintainer"))

	value, found, err := s.repo.GetConfig(s.Ctx(), "beads.role")
	s.Require().NoError(err)
	s.True(found)
	s.Equal("maintainer", value)
}

func (s *testSuite) TestConfig_SetEmptyKeyErrors() {
	s.gitInit()
	err := s.repo.SetConfig(s.Ctx(), "", "x")
	s.Require().Error(err)
}

func (s *testSuite) TestRemote_GetMissing() {
	s.gitInit()
	url, found, err := s.repo.GetRemoteURL(s.Ctx(), "origin")
	s.Require().NoError(err)
	s.False(found)
	s.Empty(url)
}

func (s *testSuite) TestRemote_AddedRemoteVisible() {
	s.gitInit()
	s.run("git", "remote", "add", "origin", "https://example.com/repo.git")

	url, found, err := s.repo.GetRemoteURL(s.Ctx(), "origin")
	s.Require().NoError(err)
	s.True(found)
	s.Equal("https://example.com/repo.git", url)
}

func (s *testSuite) TestRemote_ListEmpty() {
	s.gitInit()
	names, err := s.repo.ListRemoteNames(s.Ctx())
	s.Require().NoError(err)
	s.Empty(names)
}

func (s *testSuite) TestRemote_ListMultiple() {
	s.gitInit()
	s.run("git", "remote", "add", "origin", "https://example.com/a.git")
	s.run("git", "remote", "add", "upstream", "https://example.com/b.git")

	names, err := s.repo.ListRemoteNames(s.Ctx())
	s.Require().NoError(err)
	s.ElementsMatch([]string{"origin", "upstream"}, names)
}

func (s *testSuite) TestCurrentBranch_NoErrorOnFreshRepo() {
	s.gitInit()
	_, err := s.repo.CurrentBranch(s.Ctx())
	s.Require().NoError(err)
}

func (s *testSuite) TestBranchHasUpstream_FalseWhenUnset() {
	s.gitInit()
	s.writeFile("a.txt", "x")
	s.run("git", "add", "a.txt")
	s.run("git", "commit", "-q", "-m", "init")

	branch, err := s.repo.CurrentBranch(s.Ctx())
	s.Require().NoError(err)
	s.Require().NotEmpty(branch)

	has, err := s.repo.BranchHasUpstream(s.Ctx(), branch)
	s.Require().NoError(err)
	s.False(has)
}

func (s *testSuite) TestBranchHasUpstream_TrueWhenSet() {
	s.gitInit()
	s.writeFile("a.txt", "x")
	s.run("git", "add", "a.txt")
	s.run("git", "commit", "-q", "-m", "init")

	branch, err := s.repo.CurrentBranch(s.Ctx())
	s.Require().NoError(err)
	s.run("git", "config", "branch."+branch+".remote", "origin")
	s.run("git", "config", "branch."+branch+".merge", "refs/heads/"+branch)

	has, err := s.repo.BranchHasUpstream(s.Ctx(), branch)
	s.Require().NoError(err)
	s.True(has)
}

func (s *testSuite) TestAdd_StageAndCommitFile() {
	s.gitInit()
	s.writeFile("a.txt", "x")

	s.Require().NoError(s.repo.Add(s.Ctx(), "a.txt"))
	result, err := s.repo.Commit(s.Ctx(), domain.GitCommitParams{Message: "test"})
	s.Require().NoError(err)
	s.True(result.DidCommit)
	s.NotEmpty(result.Output)
}

func (s *testSuite) TestCommit_NoVerifyBypassesHook() {
	s.gitInit()
	hookPath := filepath.Join(s.tmpDir, ".git", "hooks", "pre-commit")
	s.Require().NoError(os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 1\n"), 0755)) //nolint:gosec // test hook
	s.writeFile("a.txt", "x")
	s.Require().NoError(s.repo.Add(s.Ctx(), "a.txt"))

	result, err := s.repo.Commit(s.Ctx(), domain.GitCommitParams{Message: "test", NoVerify: true})
	s.Require().NoError(err)
	s.True(result.DidCommit)
}

func (s *testSuite) TestCommit_SkipHooksBypassesPrepareCommitMsgHook() {
	s.gitInit()
	hookPath := filepath.Join(s.tmpDir, ".git", "hooks", "prepare-commit-msg")
	s.Require().NoError(os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 1\n"), 0755)) //nolint:gosec // test hook
	s.writeFile("a.txt", "x")
	s.Require().NoError(s.repo.Add(s.Ctx(), "a.txt"))

	result, err := s.repo.Commit(s.Ctx(), domain.GitCommitParams{Message: "test", SkipHooks: true})
	s.Require().NoError(err)
	s.True(result.DidCommit)
}

func (s *testSuite) TestCommit_NothingToCommitDidCommitFalse() {
	s.gitInit()
	s.writeFile("a.txt", "x")
	s.Require().NoError(s.repo.Add(s.Ctx(), "a.txt"))
	_, err := s.repo.Commit(s.Ctx(), domain.GitCommitParams{Message: "first"})
	s.Require().NoError(err)

	result, err := s.repo.Commit(s.Ctx(), domain.GitCommitParams{Message: "second"})
	s.Require().NoError(err)
	s.False(result.DidCommit)
	s.True(strings.Contains(string(result.Output), "nothing to commit"))
}

func (s *testSuite) TestAdd_EmptyPathsErrors() {
	s.gitInit()
	err := s.repo.Add(s.Ctx())
	s.Require().Error(err)
}

func (s *testSuite) TestIsJujutsuRepo_FalseOnTempDir() {
	ok, err := s.repo.IsJujutsuRepo(s.Ctx())
	s.Require().NoError(err)
	s.False(ok)
}

func (s *testSuite) TestIsJujutsuRepo_TrueWhenJJDirPresent() {
	s.Require().NoError(os.MkdirAll(filepath.Join(s.tmpDir, ".jj"), 0700))
	ok, err := s.repo.IsJujutsuRepo(s.Ctx())
	s.Require().NoError(err)
	s.True(ok)
}

func (s *testSuite) TestIsColocatedJJGit_FalseOnTempDir() {
	s.gitInit()
	ok, err := s.repo.IsColocatedJJGit(s.Ctx())
	s.Require().NoError(err)
	s.False(ok)
}

func (s *testSuite) TestIsColocatedJJGit_TrueWhenBothPresent() {
	s.gitInit()
	s.Require().NoError(os.MkdirAll(filepath.Join(s.tmpDir, ".jj"), 0700))
	ok, err := s.repo.IsColocatedJJGit(s.Ctx())
	s.Require().NoError(err)
	s.True(ok)
}

func (s *testSuite) TestExec_HappensInWorkDir() {
	// Confirms cmd.Dir is honored: chdir away, then init via repo bound to tmpDir.
	wd, err := os.Getwd()
	s.Require().NoError(err)
	defer func() { _ = os.Chdir(wd) }()
	s.Require().NoError(os.Chdir(s.T().TempDir())) // unrelated working directory

	s.Require().NoError(s.repo.Init(s.Ctx()))

	info, err := os.Stat(filepath.Join(s.tmpDir, ".git"))
	s.Require().NoError(err)
	s.True(info.IsDir())
}
