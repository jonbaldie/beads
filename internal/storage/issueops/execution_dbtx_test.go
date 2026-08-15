package issueops

import (
	"context"
	"testing"

	publicops "github.com/steveyegge/beads/issueops"
)

// TestExecuteLifecycleVerbsTakeDBTX pins the seam that lets both Lifecycle
// adapters reach one body. The store adapters pass *sql.Tx. The unit-of-work
// adapter passes domain/db.Runner. Both satisfy DBTX. A *sql.Tx-only
// signature is what forced the unit-of-work adapter to re-orchestrate create,
// update, close, and reopen.
func TestExecuteLifecycleVerbsTakeDBTX(t *testing.T) {
	t.Parallel()

	var create func(context.Context, DBTX, publicops.CreateRequest) (publicops.CreateResult, ChangedTables, error)
	create = ExecuteCreate
	if create == nil {
		t.Fatal("ExecuteCreate is nil")
	}

	var update func(context.Context, DBTX, publicops.UpdateRequest) (publicops.UpdateResult, ChangedTables, error)
	update = ExecuteUpdate
	if update == nil {
		t.Fatal("ExecuteUpdate is nil")
	}

	var closeFn func(context.Context, DBTX, publicops.CloseRequest) (publicops.CloseResult, ChangedTables, error)
	closeFn = ExecuteClose
	if closeFn == nil {
		t.Fatal("ExecuteClose is nil")
	}

	var reopen func(context.Context, DBTX, publicops.ReopenRequest) (publicops.ReopenResult, ChangedTables, error)
	reopen = ExecuteReopen
	if reopen == nil {
		t.Fatal("ExecuteReopen is nil")
	}
}
