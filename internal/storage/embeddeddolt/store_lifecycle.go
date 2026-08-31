//go:build cgo

package embeddeddolt

import (
	"context"

	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

// Close decrements the reference count if this store was opened via Open.
// When the last reference closes, the store's process-local resources are
// released and the remote cache is cleaned.
func (s *EmbeddedDoltStore) Close() error {
	if closeCached(s) {
		return nil
	}
	if s.closed.CompareAndSwap(false, true) {
		s.cleanGitRemoteCacheGarbage()
	}
	return nil
}

// DoltGC runs Dolt garbage collection to reclaim disk space.
func (s *EmbeddedDoltStore) DoltGC(ctx context.Context) error {
	return s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		return versioncontrolops.DoltGC(ctx, db)
	})
}

// ListRemoteRefs returns the names of all cached remote-tracking refs.
func (s *EmbeddedDoltStore) ListRemoteRefs(ctx context.Context) ([]string, error) {
	var refs []string
	err := s.withDBConn(ctx, func(db versioncontrolops.DBConn) error {
		var err error
		refs, err = versioncontrolops.ListRemoteRefs(ctx, db)
		return err
	})
	return refs, err
}

// PruneRemoteRefs deletes all cached remote-tracking refs so a post-squash GC
// can reclaim the history they anchor (bd-agctw). Returns the deleted names.
func (s *EmbeddedDoltStore) PruneRemoteRefs(ctx context.Context) ([]string, error) {
	var pruned []string
	err := s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		var err error
		pruned, err = versioncontrolops.PruneRemoteRefs(ctx, db)
		return err
	})
	return pruned, err
}

// ListTags returns the names of all Dolt tags.
func (s *EmbeddedDoltStore) ListTags(ctx context.Context) ([]string, error) {
	var tags []string
	err := s.withDBConn(ctx, func(db versioncontrolops.DBConn) error {
		var err error
		tags, err = versioncontrolops.ListTags(ctx, db)
		return err
	})
	return tags, err
}
