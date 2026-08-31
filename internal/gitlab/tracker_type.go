package gitlab

import "github.com/jonbaldie/beads/internal/storage"

// Tracker implements tracker.IssueTracker for GitLab.
type Tracker struct {
	client      *Client
	config      *MappingConfig
	store       storage.Storage
	filter      *IssueFilter // Optional filters for issue fetching
	projectPath string       // GitLab project path (e.g., "socwave/socwave") for GraphQL
}

// Keep the fields visible to file-local unused-code analysis; Tracker's
// behavior is implemented in tracker.go.
var _ = Tracker{
	client:      nil,
	config:      nil,
	store:       nil,
	filter:      nil,
	projectPath: "",
}
