// Package backendnames holds the process-local set of registered storage
// backend names.
//
// This small, standard-library-only package lets configfile classify names
// without importing the typed backend registry and the storage interfaces it
// depends on. The backends package is the only writer.
package backendnames

import "sync"

var names sync.Map

// Add records a registered backend name.
func Add(name string) {
	names.Store(name, struct{}{})
}

// Remove drops a registered backend name.
func Remove(name string) {
	names.Delete(name)
}

// Has reports whether name is registered.
func Has(name string) bool {
	_, ok := names.Load(name)
	return ok
}
