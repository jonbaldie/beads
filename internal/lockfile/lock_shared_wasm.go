//go:build js && wasm

package lockfile

import "os"

// FlockSharedNonBlock is a no-op in WASM (single-process environment).
func FlockSharedNonBlock(_ *os.File) error {
	return nil
}

// FlockExclusiveNonBlock is a no-op in WASM (single-process environment).
func FlockExclusiveNonBlock(_ *os.File) error {
	return nil
}
