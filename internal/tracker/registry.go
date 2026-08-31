package tracker

import (
	"fmt"
	"sort"
	"sync"
)

// TrackerFactory creates a new IssueTracker instance.
type TrackerFactory func() IssueTracker

type trackerRegistry struct {
	mu        sync.RWMutex
	factories map[string]TrackerFactory
}

var registry = &trackerRegistry{factories: make(map[string]TrackerFactory)}

func (r *trackerRegistry) register(name string, factory TrackerFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = factory
}

func (r *trackerRegistry) get(name string) TrackerFactory {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.factories[name]
}

func (r *trackerRegistry) list() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.factories))
	for name := range r.factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Register adds a tracker factory to the global registry.
// Typically called from an init() function in each tracker adapter package.
func Register(name string, factory TrackerFactory) {
	registry.register(name, factory)
}

// Get returns the factory for the named tracker, or nil if not registered.
func Get(name string) TrackerFactory {
	return registry.get(name)
}

// List returns the names of all registered trackers, sorted alphabetically.
func List() []string {
	return registry.list()
}

// NewTracker creates a new instance of the named tracker.
// Returns an error if the tracker is not registered.
func NewTracker(name string) (IssueTracker, error) {
	factory := Get(name)
	if factory == nil {
		return nil, fmt.Errorf("tracker %q not registered; available: %v", name, List())
	}
	return factory(), nil
}
