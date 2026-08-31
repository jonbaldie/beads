package issueops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// PrepareIssueForInsert normalizes timestamps, validates, and computes the content hash.
func PrepareIssueForInsert(issue *types.Issue, customStatuses, customTypes []string) error {
	if err := ValidateMetadataIfConfigured(issue.Metadata); err != nil {
		return fmt.Errorf("metadata validation failed for issue %s: %w", issue.ID, err)
	}

	// Normalize timestamps to UTC, defaulting to now.
	now := time.Now().UTC()
	if issue.CreatedAt.IsZero() {
		issue.CreatedAt = now
	} else {
		issue.CreatedAt = issue.CreatedAt.UTC()
	}
	if issue.UpdatedAt.IsZero() {
		issue.UpdatedAt = now
	} else {
		issue.UpdatedAt = issue.UpdatedAt.UTC()
	}

	// Ensure closed issues have a closed_at timestamp.
	if issue.Status == types.StatusClosed && issue.ClosedAt == nil {
		maxTime := issue.CreatedAt
		if issue.UpdatedAt.After(maxTime) {
			maxTime = issue.UpdatedAt
		}
		closedAt := maxTime.Add(time.Second)
		issue.ClosedAt = &closedAt
	}

	if err := issue.ValidateWithCustom(customStatuses, customTypes); err != nil {
		return fmt.Errorf("validation failed for issue %s: %w", issue.ID, err)
	}
	if issue.ContentHash == "" {
		issue.ContentHash = issue.ComputeContentHash()
	}
	return nil
}

// ValidateIssueIDPrefix validates that the issue ID matches the configured prefix
// or any of the allowed_prefixes.
func ValidateIssueIDPrefix(id, prefix, allowedPrefixes string) error {
	if strings.HasPrefix(id, prefix+"-") {
		return nil
	}
	if allowedPrefixes != "" {
		for _, allowed := range strings.Split(allowedPrefixes, ",") {
			allowed = strings.TrimSpace(allowed)
			if allowed != "" && strings.HasPrefix(id, allowed+"-") {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: issue ID %s does not match configured prefix %s", storage.ErrPrefixMismatch, id, prefix)
}

// ParseHierarchicalID checks if an ID is hierarchical (e.g., "bd-abc.1")
// and returns the parent ID and child number.
func ParseHierarchicalID(id string) (parentID string, childNum int, ok bool) {
	lastDot := strings.LastIndex(id, ".")
	if lastDot == -1 {
		return "", 0, false
	}
	parentID = id[:lastDot]
	var num int
	if _, err := fmt.Sscanf(id[lastDot+1:], "%d", &num); err != nil {
		return "", 0, false
	}
	return parentID, num, true
}

// AllWisps returns true if every issue in the slice should be routed to the
// wisps table (i.e., is ephemeral or no-history). Used to gate the fast path
// that skips Dolt versioning in batch creates.
func AllWisps(issues []*types.Issue) bool {
	for _, issue := range issues {
		if !issue.Ephemeral && !issue.NoHistory {
			return false
		}
	}
	return true
}

// checkCrossTableIDCollision rejects a create whose ID already lives in the
// sibling table (GH#4455). Issues and wisps share one ID space but live in
// separate tables; an ID present in both makes the merge-based lookups
// (bd ready/search) hard-error for the whole store. The target-table
// existence check in InsertIssueIfNew only sees one table, so nothing else in
// the create path closes this hole.
//
// Promotion (PromoteFromEphemeralInTx) deliberately inserts into issues while
// the wisp row still exists, then deletes the wisp — but it calls
// InsertIssueIfNew directly and never routes through here, so its transient
// dual-presence window is unaffected.
//
// ConflictSkip is the auto-import upgrade-recovery path (GH#3955), which must
// never hard-fail; there we skip the colliding row instead (lookups stay
// tolerant via GH#4163).
//
//nolint:gosec // G201: siblingTable is one of two hardcoded constants
func checkCrossTableIDCollision(ctx context.Context, tx DBTX, id, issueTable string, opts storage.BatchCreateOptions) (skip bool, err error) {
	if id == "" {
		return false, nil
	}
	siblingTable := "wisps"
	if issueTable == "wisps" {
		siblingTable = "issues"
	}
	var siblingCount int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE id = ?`, siblingTable), id).Scan(&siblingCount); err != nil {
		return false, fmt.Errorf("failed to check cross-table ID collision for %s: %w", id, err)
	}
	if siblingCount == 0 {
		return false, nil
	}
	if opts.ConflictSkip {
		return true, nil
	}
	return false, fmt.Errorf("cannot create %q: ID already exists in the %s table (issues and wisps share one ID space)", id, siblingTable)
}

// InsertIssueIfNew inserts the issue and returns whether it was genuinely new,
// and whether the RejectStaleUpserts guard rejected it.
//
// When opts.ConflictSkip is true and an issue with the same ID already exists,
// the row is left untouched (no UPSERT) and isNew is false. This is the
// auto-import upgrade-recovery guarantee (GH#3955): even if the emptiness
// guard in maybeAutoImportJSONL regresses, a stale issues.jsonl can never
// overwrite live rows — worst case is a no-op. Otherwise the INSERT … ON
// DUPLICATE KEY UPDATE runs, so explicit `bd import` keeps UPSERT semantics;
// with opts.RejectStaleUpserts the update half is conditional on the incoming
// row being strictly newer than the stored one (bd-pkim8, bd-hj85c).
// Staleness is decided by an explicit in-transaction read (stored updated_at
// strictly newer ⇒ rejected) so callers can skip aux persistence and count
// the row as skipped instead of created (bd-578h9.8). Equal-timestamp rows
// are deliberately NOT rejected here, even though the ODKU's
// VALUES(updated_at) > updated_at condition keeps every stored column for
// them: updated_at has second granularity, so a tie may be two distinct
// same-second updates — the local row must win the tie (an incoming row with
// an empty notes field must not wipe local notes), but its aux data
// (labels/comments/deps, which never bump updated_at) still merges
// additively (bd-hj85c).
//
//nolint:gosec // G201: table is a hardcoded constant
func InsertIssueIfNew(ctx context.Context, tx DBTX, issueTable string, issue *types.Issue, opts storage.BatchCreateOptions) (isNew bool, staleRejected bool, err error) {
	existingCount, err := existingIssueCount(ctx, tx, issueTable, issue.ID)
	if err != nil {
		return false, false, err
	}
	if opts.ConflictSkip && existingCount > 0 {
		return false, false, nil // issue already exists — skip, never overwrite
	}
	if opts.CreateOnly {
		return insertCreateOnlyIssue(ctx, tx, issueTable, issue)
	}
	stale, err := issueIsStale(ctx, tx, issueTable, issue, opts.RejectStaleUpserts, existingCount)
	if err != nil {
		return false, false, err
	}
	if stale {
		return false, true, nil
	}
	if err := insertIssueIntoTable(ctx, tx, issueTable, issue, opts.RejectStaleUpserts); err != nil {
		return false, false, fmt.Errorf("failed to insert issue %s: %w", issue.ID, err)
	}
	return existingCount == 0, false, nil
}

//nolint:gosec // G201: issueTable is a hardcoded caller-selected table.
func existingIssueCount(ctx context.Context, tx DBTX, issueTable, issueID string) (int, error) {
	if issueID == "" {
		return 0, nil
	}
	var existingCount int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE id = ?`, issueTable), issueID).Scan(&existingCount); err != nil {
		return 0, fmt.Errorf("failed to check issue existence for %s: %w", issueID, err)
	}
	return existingCount, nil
}

func insertCreateOnlyIssue(ctx context.Context, tx DBTX, issueTable string, issue *types.Issue) (bool, bool, error) {
	if err := insertIssueCreateOnly(ctx, tx, issueTable, issue); err != nil {
		if isCreateOnlyDuplicateError(err) {
			return false, false, fmt.Errorf("%w: %s", storage.ErrAlreadyExists, issue.ID)
		}
		return false, false, err
	}
	return true, false, nil
}

//nolint:gosec // G201: issueTable is a hardcoded caller-selected table.
func issueIsStale(ctx context.Context, tx DBTX, issueTable string, issue *types.Issue, rejectStale bool, existingCount int) (bool, error) {
	if !rejectStale || existingCount == 0 {
		return false, nil
	}
	var storedNewer int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE id = ? AND updated_at > ?`, issueTable), issue.ID, issue.UpdatedAt).Scan(&storedNewer); err != nil {
		return false, fmt.Errorf("failed to check issue staleness for %s: %w", issue.ID, err)
	}
	// The conditional ODKU would keep every stored column anyway; skipping the
	// no-op insert makes the rejection observable.
	return storedNewer > 0, nil
}

func isCreateOnlyDuplicateError(err error) bool {
	var mysqlError *mysql.MySQLError
	if errors.As(err, &mysqlError) && mysqlError.Number == 1062 {
		return true
	}
	return isEmbeddedDuplicateError(err)
}

// InsertIssueStrictInTx inserts one issue without probing either storage plane.
// Callers that move an aggregate use it while the source row necessarily still
// occupies the shared ID, so cross-plane create guards would reject a valid move.
func InsertIssueStrictInTx(ctx context.Context, tx DBTX, table string, issue *types.Issue) error {
	if err := insertIssueCreateOnly(ctx, tx, table, issue); err != nil {
		if isCreateOnlyDuplicateError(err) {
			return fmt.Errorf("%w: %s", storage.ErrAlreadyExists, issue.ID)
		}
		return err
	}
	return nil
}
