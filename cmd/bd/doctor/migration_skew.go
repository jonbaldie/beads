package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/storage/embeddeddolt"
	"github.com/jonbaldie/beads/internal/storage/schema"
)

const migrationContentSkewCheckName = "Migration Content Skew"

// CheckMigrationContentSkew detects when this database and its remote applied
// DIFFERENT content for the same migration version — the silent schema fork from
// gastownhall/beads#4259. It compares the local schema_migrations content hashes
// against the already-cached remote-tracking ref (no network fetch) and warns on
// any divergence.
//
// This is a read-only diagnostic; it never gates push/pull. It skips cleanly
// when there is no remote, no cached remote ref, or no recorded hashes to
// compare. In an embedded workspace SharedStore only ever holds a server-mode
// store (nil here), so the check falls back to opening the embedded database
// directly — embedded mode is what #4259 was reported against, and without
// the fallback the detection half of the guard never ran there (bd-578h9.13).
func CheckMigrationContentSkew(ss *SharedStore) DoctorCheck {
	if store := ss.Store(); store != nil {
		return checkMigrationContentSkew(context.Background(), store.DB(), store.RemoteName())
	}
	if check, ok := checkMigrationContentSkewEmbedded(context.Background(), sharedStoreBeadsDir(ss)); ok {
		return check
	}
	return DoctorCheck{
		Name:     migrationContentSkewCheckName,
		Status:   StatusOK,
		Message:  "N/A (no database)",
		Category: CategoryData,
	}
}

// checkMigrationContentSkewEmbedded opens the workspace's embedded Dolt
// database (read-only diagnostic queries only; nothing is written) and runs
// the skew comparison on it. Returns ok=false when there is no embedded
// database to inspect.
func checkMigrationContentSkewEmbedded(ctx context.Context, beadsDir string) (DoctorCheck, bool) {
	if beadsDir == "" {
		return DoctorCheck{}, false
	}
	dataDir := filepath.Join(beadsDir, "embeddeddolt")
	if _, err := os.Stat(dataDir); err != nil {
		return DoctorCheck{}, false
	}
	database := configfile.DefaultDoltDatabase
	if cfg, err := configfile.Load(beadsDir); err == nil && cfg != nil {
		database = cfg.GetDoltDatabase()
	}
	db, cleanup, err := embeddeddolt.OpenSQL(ctx, dataDir, database, "")
	if err != nil {
		return DoctorCheck{
			Name:     migrationContentSkewCheckName,
			Status:   StatusWarning,
			Message:  fmt.Sprintf("Could not check migration content skew (open embedded database): %v", err),
			Detail:   "The skew check failed to run; this does not mean skew exists. Re-run `bd doctor` and report the error if it persists.",
			Category: CategoryData,
		}, true
	}
	defer func() { _ = cleanup() }()
	return checkMigrationContentSkew(ctx, db, ""), true
}

// checkMigrationContentSkew compares local migration content hashes against the
// configured sync remote's cached tracking ref. remote is the sync remote name
// (DoltStore.RemoteName(), "origin" by default) — NOT whichever remote happens
// to sort first in dolt_remotes.
func checkMigrationContentSkew(ctx context.Context, db *sql.DB, remote string) DoctorCheck {
	if remote == "" {
		remote = "origin"
	}
	if check, done := checkSkewRemotePresent(ctx, db, remote); done {
		return check
	}
	local, check, done := loadSkewLocalHashes(ctx, db)
	if done {
		return check
	}
	remoteHashes, check, done := loadSkewRemoteHashes(ctx, db, remote, skewActiveBranch(ctx, db))
	if done {
		return check
	}
	return reportContentSkew(remote, local, remoteHashes)
}

func skewOK(msg string) DoctorCheck {
	return DoctorCheck{Name: migrationContentSkewCheckName, Status: StatusOK, Message: msg, Category: CategoryData}
}

func skewCannot(stage string, err error) DoctorCheck {
	// "Cannot check" is NOT "checked and matches": surface unexpected failures
	// as a warning instead of swallowing them as OK (bd-6dnrw.27 — a broken
	// query made this check a silent permanent no-op).
	return DoctorCheck{
		Name:     migrationContentSkewCheckName,
		Status:   StatusWarning,
		Message:  fmt.Sprintf("Could not check migration content skew (%s): %v", stage, err),
		Detail:   "The skew check failed to run; this does not mean skew exists. Re-run `bd doctor` and report the error if it persists.",
		Category: CategoryData,
	}
}

func checkSkewRemotePresent(ctx context.Context, db *sql.DB, remote string) (DoctorCheck, bool) {
	var remoteCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_remotes WHERE name = ?", remote).Scan(&remoteCount); err != nil {
		if dberrors.IsTableNotExist(err) {
			return skewOK("No remote configured — nothing to compare"), true
		}
		return skewCannot("read dolt_remotes", err), true
	}
	if remoteCount == 0 {
		return skewOK(fmt.Sprintf("Sync remote %q not configured — nothing to compare", remote)), true
	}
	return DoctorCheck{}, false
}

func skewActiveBranch(ctx context.Context, db *sql.DB) string {
	var active string
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&active); err == nil && active != "" {
		return active
	}
	return "main"
}

func loadSkewLocalHashes(ctx context.Context, db *sql.DB) (map[int]string, DoctorCheck, bool) {
	local, err := schema.ReadMigrationContentHashes(ctx, db, "")
	if err != nil {
		if schema.MissingMigrationObjectErr(err) {
			return nil, skewOK("No local migration content hashes recorded yet"), true
		}
		return nil, skewCannot("read local schema_migrations", err), true
	}
	if len(local) == 0 {
		return nil, skewOK("No local migration content hashes recorded yet"), true
	}
	return local, DoctorCheck{}, false
}

func loadSkewRemoteHashes(ctx context.Context, db *sql.DB, remote, branch string) (map[int]string, DoctorCheck, bool) {
	ref := "remotes/" + remote + "/" + branch
	remoteHashes, err := schema.ReadMigrationContentHashes(ctx, db, ref)
	if err == nil {
		return remoteHashes, DoctorCheck{}, false
	}
	if schema.RemoteRefUnavailableErr(err) {
		return nil, skewOK(fmt.Sprintf("No cached remote ref %q to compare", ref)), true
	}
	if schema.MissingMigrationObjectErr(err) {
		return nil, skewOK(fmt.Sprintf("Cached remote ref %q predates migration content hashes — nothing to compare", ref)), true
	}
	return nil, skewCannot(fmt.Sprintf("read schema_migrations at %q", ref), err), true
}

func reportContentSkew(remote string, local, remoteHashes map[int]string) DoctorCheck {
	skewed := schema.ContentHashSkew(local, remoteHashes)
	if len(skewed) == 0 {
		return skewOK(fmt.Sprintf("Applied migrations match remote %q", remote))
	}
	versions := make([]string, len(skewed))
	for i, v := range skewed {
		versions[i] = fmt.Sprintf("%04d", v)
	}
	return DoctorCheck{
		Name:     migrationContentSkewCheckName,
		Status:   StatusWarning,
		Message:  fmt.Sprintf("This database and remote %q applied different content for migration(s) %s", remote, strings.Join(versions, ", ")),
		Detail:   "Two clones ran different migration content for the same version number — a silent schema fork (gastownhall/beads#4259). `bd dolt pull` may fail to merge cryptically.",
		Fix:      "Upgrade every clone to a bd version that carries the schema-convergence migration. If a merge already fails, make one clone canonical and re-bootstrap the others from the remote.",
		Category: CategoryData,
	}
}
