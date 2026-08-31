package httpapi

import (
	"net/url"
)

// query decodes one request's query string against the operation's parameter
// table, and is the single place the document's unknown-parameter rule is
// enforced for an operation that HAS parameters.
//
// The rule is strict on purpose. Silently ignoring an unrecognized FILTER
// parameter WIDENS the result set, so a client one version ahead of the server
// would receive — and act on — rows it believed it had filtered out. It is
// also a client's only per-parameter capability probe, since `capabilities` is
// operation-level.
//
// Every accessor records the name it read, so the allowlist is the parameter
// table itself rather than a second copy of it that can drift: a parameter the
// handler never asks for is, by construction, one this server does not know.
type query struct {
	values url.Values
	read   map[string]bool
	// res holds the FIRST refusal. First rather than last so the answer does
	// not depend on the order the handler happens to read its parameters in;
	// `param` is what a client dispatches on.
	res *Result
}

func newQuery(values url.Values) *query {
	return &query{values: values, read: map[string]bool{}, res: nil}
}
