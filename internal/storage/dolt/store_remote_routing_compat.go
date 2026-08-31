package dolt

import (
	"context"
	"log"
)

// shouldUseCLIForGitProtocol is a compatibility wrapper for tests and older
// call sites. Prefer prepareCLIRouteForGitProtocol so mutation is explicit.
func (s *DoltStore) shouldUseCLIForGitProtocol(ctx context.Context, remote string) (bool, error) {
	return s.prepareCLIRouteForGitProtocol(ctx, remote)
}

// isGitProtocolRemote reports whether the SQL-visible remote uses git wire
// protocol and the same remote is available in the local CLI directory.
func (s *DoltStore) isGitProtocolRemote(ctx context.Context, remote string) bool {
	ok, err := s.prepareCLIRouteForGitProtocol(ctx, remote)
	if err != nil {
		log.Printf("warning: %v", err)
		return false
	}
	return ok
}
