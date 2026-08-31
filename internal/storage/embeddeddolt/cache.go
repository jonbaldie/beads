//go:build cgo

package embeddeddolt

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
)

var cache = newStoreCache()

type storeCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
}

func newStoreCache() *storeCache {
	return &storeCache{entries: make(map[string]*cacheEntry)}
}

type cacheEntry struct {
	store    *EmbeddedDoltStore
	refCount int // guarded by cacheMu
}

// Open returns a cached EmbeddedDoltStore for the given data directory, creating
// one via newStore if no cached instance exists. Subsequent calls with the same
// resolved dataDir return the existing store and increment a reference count.
//
// Each Open must be paired with a Close. The underlying store is only truly
// closed when the last reference calls Close.
//
// This prevents redundant engine initializations when multiple code paths open
// connectors against the same data directory in the same process.
func Open(ctx context.Context, beadsDir, database, branch string) (*EmbeddedDoltStore, error) {
	return openCached(ctx, beadsDir, database, branch, openStrict)
}

// OpenForReadOnlyCommand opens like Open, except that a #4259 remote-migrate
// gate refusal skips the pending migrations with a stderr warning instead of
// failing the open (bd-578h9.5): the gate exists to stop in-place migration,
// not reads, and server mode's read-only opens already skip migration. The
// returned store is otherwise a normal writable store (read-only commands
// can still write incidentally, e.g. the post-command autocommit net).
func OpenForReadOnlyCommand(ctx context.Context, beadsDir, database, branch string) (*EmbeddedDoltStore, error) {
	return openCached(ctx, beadsDir, database, branch, openReadOnlyCommand)
}

// OpenForWorkingSetReconcile opens like Open, except that a schema.
// DirtyTablesError refusal skips the pending migrations with a stderr
// warning instead of failing the open (#4566): commands whose entire purpose
// is to reconcile the Dolt working set (bd dolt commit, bd vc commit) must be
// able to open the store even when pending migrations touch dirty tables -
// otherwise the commit that would clear the dirty state and unblock the
// migration can never run. The returned store is otherwise a normal writable
// store.
func OpenForWorkingSetReconcile(ctx context.Context, beadsDir, database, branch string) (*EmbeddedDoltStore, error) {
	return openCached(ctx, beadsDir, database, branch, openWorkingSetReconcile)
}

func openCached(ctx context.Context, beadsDir, database, branch string, intent openIntent) (*EmbeddedDoltStore, error) {
	key, err := cacheKey(beadsDir)
	if err != nil {
		return nil, err
	}

	if store, ok := cache.acquire(key); ok {
		return store, nil
	}

	// Slow path: create a new store outside the lock.
	s, err := newStore(ctx, beadsDir, database, branch, intent)
	if err != nil {
		return nil, err
	}

	if store, reused := cache.insertOrAcquire(key, s); reused {
		// Discard the store we just created; use the cached one.
		_ = s.Close()
		return store, nil
	}
	return s, nil
}

// closeCached decrements the reference count for a cached store.
// Returns true when the cache absorbed the close (refs remain, suppress real
// close). Returns false when the caller must run Close — either the
// entry was evicted (last ref) or the store was never cached.
func closeCached(s *EmbeddedDoltStore) bool {
	return cache.release(s)
}

func (c *storeCache) release(s *EmbeddedDoltStore) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	for key, entry := range c.entries {
		if entry.store == s {
			entry.refCount--
			if entry.refCount <= 0 {
				delete(c.entries, key)
				// Actual close happens after releasing cacheMu (via caller).
				return false
			}
			// Other references remain — suppress the real close.
			return true
		}
	}
	// Not in cache — let the caller close normally.
	return false
}

func (c *storeCache) acquire(key string) (*EmbeddedDoltStore, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	// Cache hit: the requested intent is ignored - the store keeps whatever
	// intent it was opened with on the slow path below. This is safe today
	// because intent is derived once per process from command classification,
	// so a single process never opens the same data directory under two
	// different intents.
	entry.refCount++
	return entry.store, true
}

func (c *storeCache) insertOrAcquire(key string, store *EmbeddedDoltStore) (*EmbeddedDoltStore, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[key]; ok {
		entry.refCount++
		return entry.store, true
	}
	c.entries[key] = &cacheEntry{store: store, refCount: 1}
	return store, false
}

// cacheKey resolves beadsDir to an absolute dataDir path for use as a cache key.
func cacheKey(beadsDir string) (string, error) {
	absBeadsDir, err := filepath.Abs(beadsDir)
	if err != nil {
		return "", fmt.Errorf("embeddeddolt: resolving beads dir for cache key: %w", err)
	}
	return filepath.Join(absBeadsDir, "embeddeddolt"), nil
}
