package dolt

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	storeops "github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/schema"
	"github.com/jonbaldie/beads/internal/workapi"
	"github.com/jonbaldie/beads/issueops"
)

// Deleter returns the named-row erasure surface for this store.
func (s *DoltStore) Deleter() (issueops.Deleter, error) {
	if s == nil {
		return nil, &storage.ErrUnsupported{Op: "Deleter", Backend: "nil"}
	}
	return &deleter{store: s}, nil
}

// deleter erases named rows inside ONE transaction.
//
// There is no shared constructor package for this role: the work is an
// existence probe, a guard, a delete and a text rewrite that must all see one
// snapshot, and a transaction is not reachable through storage.DoltStorage. The
// sharing happens one level down instead — this body and the embedded store's
// are a few lines each around issueops.DeleteInTx — so two wrappers over one
// body is still ONE vote.
type deleter struct{ store *DoltStore }

var _ issueops.Deleter = (*deleter)(nil)

// Delete erases the request's ids.
//
// VALIDATION AND NORMALIZATION HAPPEN BEFORE THE TRANSACTION, so a malformed
// request costs no database work and the body below sees a deduplicated,
// trimmed id list. The refusals that need the graph — the missing id and the
// dependents guard — happen inside it, because they are reads.
//
// A DRY RUN TAKES A READ TRANSACTION. It writes nothing by construction, so
// giving it a write transaction and an empty commit would make a preview look
// like a mutation to everything watching the store.
//
// THE VERSION-CONTROL ENTRY IS ONE PER DELETION, recorded here rather than in
// the shared body because the two backends mint it differently: this one
// INSIDE the write transaction, where the embedded store can only publish
// after its SQL commit, on a second connection. That is why the role promises
// exactly one entry in the STEADY STATE and only this leg makes it atomic with
// the deletion. A deletion confined to the wisp tables touches only tables
// this plane ignores, so DOLT_COMMIT finds nothing to commit and records none.
func (s *deleter) Delete(ctx context.Context, req issueops.DeleteRequest) (issueops.DeleteResult, error) {
	if err := workapi.ValidateDeleteRequest(req); err != nil {
		return issueops.DeleteResult{}, err
	}
	req.IDs = workapi.NormalizeDeleteIDs(req.IDs)
	if req.DryRun {
		return s.deleteDryRun(ctx, req)
	}
	return s.deleteWrite(ctx, req)
}

func (s *deleter) deleteDryRun(ctx context.Context, req issueops.DeleteRequest) (issueops.DeleteResult, error) {
	var result issueops.DeleteResult
	if err := s.store.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = storeops.DeleteInTx(ctx, tx, req)
		return err
	}); err != nil {
		return issueops.DeleteResult{}, err
	}
	return result, nil
}

func (s *deleter) deleteWrite(ctx context.Context, req issueops.DeleteRequest) (issueops.DeleteResult, error) {
	var result issueops.DeleteResult
	if err := s.store.withWriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = storeops.DeleteInTx(ctx, tx, req)
		if err != nil {
			return err
		}
		return s.commitDelete(ctx, tx, result.Deleted)
	}); err != nil {
		return issueops.DeleteResult{}, err
	}
	return result, nil
}

func (s *deleter) commitDelete(ctx context.Context, tx *sql.Tx, deleted int) error {
	if deleted == 0 || storeops.VersionCommitDeferred(ctx) {
		return nil
	}
	for _, table := range sweptTables {
		_ = schema.DrainCall(ctx, tx, "CALL DOLT_ADD(?)", table)
	}
	msg := fmt.Sprintf("bd: delete %d issue(s)", deleted)
	if err := schema.DrainCall(ctx, tx, "CALL DOLT_COMMIT('-m', ?, '--author', ?)",
		msg, s.store.commitAuthorString()); err != nil && !isDoltNothingToCommit(err) {
		return fmt.Errorf("dolt commit: %w", err)
	}
	return nil
}
