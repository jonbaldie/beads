package main

import (
	"errors"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/issueops"
)

// proxiedCounter hands back the guarded issue-count surface for the
// proxied-server provider, through the provider's OWN capability accessor —
// the same two-step proxiedCommenter performs, and for the same reason: the
// accessor is where each layer is added.
func proxiedCounter() (issueops.Counter, error) {
	if getUOWProvider() == nil {
		return nil, errors.New("proxied-server UOW provider not initialized")
	}
	src, ok := getUOWProvider().(uow.CounterSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the issue-count surface", getUOWProvider())
	}
	return src.Counter()
}
