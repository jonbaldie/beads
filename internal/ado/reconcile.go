package ado

import (
	"github.com/jonbaldie/beads/internal/storage"
)

const (
	// DefaultReconcileInterval is how many syncs between reconciliation scans.
	DefaultReconcileInterval = 10

	// configReconcileInterval is the config key for reconcile interval.
	configReconcileInterval = "ado.reconcile_interval"

	// configSyncsSinceReconcile tracks syncs since last reconciliation.
	configSyncsSinceReconcile = "ado.syncs_since_reconcile"
)

// ReconcileResult holds the outcome of a reconciliation scan.
type ReconcileResult struct {
	Checked int      // Total work items checked
	Deleted []string // Work item IDs confirmed deleted (404)
	Denied  []string // Work item IDs with permission denied (403)
	Errors  []error  // Non-fatal errors encountered
}

// Reconciler detects deleted or inaccessible ADO work items.
type Reconciler struct {
	Client *Client
	Store  storage.Storage
}

// NewReconciler creates a new Reconciler.
func NewReconciler(client *Client, store storage.Storage) *Reconciler {
	return &Reconciler{Client: client, Store: store}
}
