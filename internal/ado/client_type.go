package ado

import "net/http"

// Client communicates with the Azure DevOps REST API.
type Client struct {
	PAT        SecretString
	BaseURL    string // Custom URL for on-prem; empty = cloud default
	Org        string
	Project    string
	HTTPClient *http.Client
}

// Keep the fields visible to file-local unused-code analysis; Client behavior
// is implemented across the client files.
var _ = Client{
	PAT:        SecretString{},
	BaseURL:    "",
	Org:        "",
	Project:    "",
	HTTPClient: nil,
}
