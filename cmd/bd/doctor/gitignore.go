package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GitignoreTemplate is the canonical .beads/.gitignore content
const GitignoreTemplate = `# Dolt database (managed by Dolt, not git)
dolt/
embeddeddolt/
proxieddb/

# Runtime files
bd.sock
bd.sock.startlock
sync-state.json
last-touched
.exclusive-lock

# Daemon runtime (lock, log, pid)
daemon.*

# Push state (runtime, per-machine)
push-state.json

# Lock files (various runtime locks)
*.lock

# Credential key (encryption key for federation peer auth — never commit)
.beads-credential-key

# Local version tracking (prevents upgrade notification spam after git ops)
.local_version

proxied_server_client_info.json

# Worktree redirect file (contains relative path to main repo's .beads/)
# Must not be committed as paths would be wrong in other clones
redirect

# Sync state (local-only, per-machine)
# These files are machine-specific and should not be shared across clones
.sync.lock

# Workspace operation gate (internal/workspacegate): physical-root gate
# files live beside the guarded root inside .beads (e.g. dolt.gate.lock)
*.gate.lock*
export-state/
export-state.json
last_pull

# Ephemeral store (SQLite - wisps/molecules, intentionally not versioned)
ephemeral.sqlite3
ephemeral.sqlite3-journal
ephemeral.sqlite3-wal
ephemeral.sqlite3-shm

# Dolt server management (auto-started by bd)
dolt-server.pid
dolt-server.log
dolt-server.lock
dolt-server.port
dolt-server.activity

# Debug-mode pprof artifacts (written when dolt.debug: true in config.yaml)
dolt-pprof/

# Corrupt backup directories (created by bd doctor --fix recovery)
*.corrupt.backup/

# Backup data (auto-exported JSONL, local-only)
backup/

# Per-project environment file (Dolt connection config, GH#2520)
.env

# Legacy files (from pre-Dolt versions)
*.db
*.db?*
*.db-journal
*.db-wal
*.db-shm
db.sqlite
bd.db
# NOTE: Do NOT add negation patterns here.
# They would override fork protection in .git/info/exclude.
# Config files (metadata.json, config.yaml) are tracked by git by default
# since no pattern above ignores them.
`

// ProjectGitignorePatterns are patterns that should be in the project-root .gitignore
// to prevent accidentally committing Dolt database files and credential keys.
var ProjectGitignorePatterns = []string{
	".dolt/",
	"*.db",
	".beads-credential-key",
	".beads/proxieddb/",
	// Workspace-gate artifacts (internal/workspacegate): the workspace
	// gate file sits BESIDE .beads in the project root, so .beads/
	// patterns cannot cover it.
	"*.gate.lock*",
}

// ProjectGitignoreHeader is the section header added to the project .gitignore
const ProjectGitignoreHeader = "# Beads / Dolt files (added by bd init)"

// requiredPatterns are patterns that MUST be in .beads/.gitignore
var requiredPatterns = []string{
	"*.db?*",
	".env",
	"redirect",
	"last-touched",
	"bd.sock.startlock",
	".sync.lock",
	"*.gate.lock*",
	"export-state/",
	"export-state.json",
	"last_pull",
	"dolt/",
	"embeddeddolt/",
	"proxieddb/",
	"ephemeral.sqlite3",
	"dolt-server.pid",
	"dolt-server.log",
	"dolt-server.lock",
	"dolt-server.port",
	"dolt-server.activity",
	"daemon.*",
	"*.lock",
	"*.corrupt.backup/",
	".beads-credential-key",
	"proxied_server_client_info.json",
	".local_version",
	"backup/",
}

// CheckGitignore checks if .beads/.gitignore is up to date.
// repoPath is the project root directory.
func CheckGitignore(repoPath string) DoctorCheck {
	gitignorePath := filepath.Join(ResolveBeadsDirForRepo(repoPath), ".gitignore")

	// Check if file exists
	content, err := os.ReadFile(gitignorePath) // #nosec G304 -- path is constructed from known parts
	if err != nil {
		return DoctorCheck{
			Name:    "Gitignore",
			Status:  "warning",
			Message: ".beads/.gitignore not found",
			Fix:     "Run: bd init (safe to re-run) or bd doctor --fix",
		}
	}

	// Check for required patterns
	contentStr := string(content)
	missing := missingGitignorePatterns(contentStr)
	if len(missing) > 0 {
		return DoctorCheck{
			Name:    "Gitignore",
			Status:  "warning",
			Message: "Outdated .beads/.gitignore (missing required patterns)",
			Detail:  "Missing: " + strings.Join(missing, ", "),
			Fix:     "Run: bd doctor --fix or bd init (safe to re-run)",
		}
	}

	return DoctorCheck{
		Name:    "Gitignore",
		Status:  "ok",
		Message: "Up to date",
	}
}

// EnsureGitignoreForBeadsDir writes the canonical .beads/.gitignore when it is
// missing or outdated. If the file does not exist, it writes the full template.
// If it exists but is outdated, it safely appends missing required patterns so
// local additions are preserved.
func EnsureGitignoreForBeadsDir(beadsDir string) error {
	gitignorePath := filepath.Join(beadsDir, ".gitignore")
	content, err := os.ReadFile(gitignorePath) // #nosec G304 -- caller supplies the active .beads dir
	if os.IsNotExist(err) {
		return writeGitignoreTemplate(gitignorePath)
	}
	if err != nil {
		return fmt.Errorf("read .beads/.gitignore: %w", err)
	}
	missing := missingGitignorePatterns(string(content))
	if len(missing) == 0 {
		return nil
	}
	if err := ensureGitignoreWritable(gitignorePath); err != nil {
		return err
	}
	return appendMissingGitignorePatterns(gitignorePath, string(content), missing)
}

func ensureGitignoreWritable(gitignorePath string) error {
	info, err := os.Stat(gitignorePath)
	if err != nil {
		return nil
	}
	if info.Mode().Perm()&0200 != 0 {
		return nil
	}
	if err := os.Chmod(gitignorePath, 0600); err != nil {
		return fmt.Errorf("chmod .beads/.gitignore: %w", err)
	}
	return nil
}

func appendMissingGitignorePatterns(gitignorePath, existingContent string, missing []string) error {
	newContent := existingContent
	if len(newContent) > 0 && !strings.HasSuffix(newContent, "\n") {
		newContent += "\n"
	}
	newContent += "\n# Added by bd (missing required patterns)\n"
	for _, pattern := range missing {
		newContent += pattern + "\n"
	}
	if err := os.WriteFile(gitignorePath, []byte(newContent), 0600); err != nil {
		return fmt.Errorf("ensure .beads/.gitignore: %w", err)
	}
	if err := os.Chmod(gitignorePath, 0600); err != nil {
		return fmt.Errorf("chmod .beads/.gitignore: %w", err)
	}
	return nil
}

// FixGitignore brings .beads/.gitignore up to date: the full template when
// the file is missing, append-only for missing required patterns otherwise.
// It must never rewrite an existing file wholesale — local rules (e.g.
// keep-exports-off-master negations) live in this file too, and the old
// full-template rewrite destroyed them (bd-kaaz3).
// If a redirect exists, it writes to the redirect target's .gitignore instead.
// repoPath is the project root directory.
func FixGitignore(repoPath string) error {
	return EnsureGitignoreForBeadsDir(ResolveBeadsDirForRepo(repoPath))
}

func missingGitignorePatterns(content string) []string {
	var missing []string
	for _, pattern := range requiredPatterns {
		if !containsGitignorePattern(content, pattern) {
			missing = append(missing, pattern)
		}
	}
	return missing
}

func writeGitignoreTemplate(gitignorePath string) error {
	// If file exists and is read-only, fix permissions first
	if info, err := os.Stat(gitignorePath); err == nil {
		if info.Mode().Perm()&0200 == 0 { // No write permission for owner
			if err := os.Chmod(gitignorePath, 0600); err != nil {
				return err
			}
		}
	}

	// Write canonical template with secure file permissions
	if err := os.WriteFile(gitignorePath, []byte(GitignoreTemplate), 0600); err != nil {
		return err
	}

	// Ensure permissions are set correctly (some systems respect umask)
	if err := os.Chmod(gitignorePath, 0600); err != nil {
		return err
	}

	return nil
}
