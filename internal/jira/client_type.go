package jira

import "net/http"

// Client provides HTTP access to a Jira instance.
type Client struct {
	URL        string
	Username   string
	APIToken   string //nolint:gosec // G117: caller-supplied Jira credential, never an embedded secret.
	APIVersion string // "2" or "3" (default: "3")
	HTTPClient *http.Client
}
