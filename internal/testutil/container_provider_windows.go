//go:build windows

package testutil

import "fmt"

// ContainerProvider is not supported on Windows.
type ContainerProvider struct{}

// NewContainerProvider is not supported on Windows.
func NewContainerProvider() (*ContainerProvider, error) {
	return nil, fmt.Errorf("ContainerProvider not available on Windows")
}

// Port returns 0 on Windows.
func (p *ContainerProvider) Port() int { return 0 }

// WritePortFile is a no-op on Windows.
func (p *ContainerProvider) WritePortFile(_ string) error { return nil }

// Stop is a no-op on Windows.
func (p *ContainerProvider) Stop() error { return nil }
