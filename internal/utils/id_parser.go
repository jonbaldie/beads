// Package utils provides utility functions for issue ID parsing and resolution.
package utils

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/internal/types"
)

// ErrAmbiguousID is the sentinel wrapped into the error ResolvePartialID
// returns when a partial ID matches more than one issue. Callers use
// errors.Is(err, ErrAmbiguousID) to distinguish "ambiguous" from
// "not found" and surface the candidate list instead of a generic failure.
var ErrAmbiguousID = errors.New("ambiguous issue ID")

type PartialIDResolverStore interface {
	SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error)
	SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error)
	GetConfig(ctx context.Context, key string) (string, error)
}

// parseIssueID ensures an issue ID has the configured prefix.
// If the input already has the prefix (e.g., "bd-a3f8e9"), returns it as-is.
// If the input lacks the prefix (e.g., "a3f8e9"), adds the configured prefix.
// Works with hierarchical IDs too: "a3f8e9.1.2" → "bd-a3f8e9.1.2"
func parseIssueID(input string, prefix string) string {
	if prefix == "" {
		prefix = "bd-"
	}

	if strings.HasPrefix(input, prefix) {
		return input
	}

	return prefix + input
}

// ResolvePartialID resolves a potentially partial issue ID to a full ID.
// Supports:
// - Full IDs: "bd-a3f8e9" or "a3f8e9" → "bd-a3f8e9"
// - Without hyphen: "bda3f8e9" or "wya3f8e9" → "bd-a3f8e9"
// - Partial IDs: "a3f8" → "bd-a3f8e9" (if unique match)
// - Hierarchical: "a3f8e9.1" → "bd-a3f8e9.1"
//
// Returns an error if:
// - No issue found matching the ID
// - Multiple issues match (ambiguous prefix)
func ResolvePartialID(ctx context.Context, store PartialIDResolverStore, input string) (string, error) {
	if store == nil {
		return "", fmt.Errorf("cannot resolve issue ID %q: storage is nil", input)
	}
	if exact := searchExactIssueID(ctx, store, input); exact != "" {
		return exact, nil
	}
	resolver := newPartialIDResolver(ctx, store, input)
	if exact := searchExactIssueID(ctx, store, resolver.normalizedID); exact != "" {
		return exact, nil
	}
	searchPart, ok := partialIDSearchPart(resolver.hashPart)
	if !ok {
		return "", fmt.Errorf("no issue found matching %q", input)
	}
	ids, err := store.SearchIssueIDs(ctx, searchPart, types.IssueFilter{})
	if err != nil {
		return "", fmt.Errorf("failed to search issues: %w", err)
	}
	exactMatch, matches := resolver.matchDurableIDs(ids)
	if exactMatch != "" {
		return exactMatch, nil
	}
	if len(matches) == 0 {
		exactMatch, matches = resolver.matchWisps(searchPart)
		if exactMatch != "" {
			return exactMatch, nil
		}
	}
	return finishPartialIDResolution(input, matches)
}

type partialIDResolver struct {
	ctx              context.Context
	store            PartialIDResolverStore
	input            string
	prefixWithHyphen string
	knownPrefixes    []string
	normalizedID     string
	hashPart         string
}

func searchExactIssueID(ctx context.Context, store PartialIDResolverStore, id string) string {
	filter := types.IssueFilter{IssueFilterCore: types.IssueFilterCore{IDs: []string{id}}}
	issues, err := store.SearchIssues(ctx, "", filter)
	if err != nil || len(issues) == 0 {
		return ""
	}
	return issues[0].ID
}

func newPartialIDResolver(ctx context.Context, store PartialIDResolverStore, input string) partialIDResolver {
	prefix, err := store.GetConfig(ctx, "issue_prefix")
	if err != nil || prefix == "" {
		prefix = "bd"
	}
	prefixWithHyphen := prefix
	if !strings.HasSuffix(prefix, "-") {
		prefixWithHyphen += "-"
	}
	knownPrefixes := configuredIssuePrefixes(ctx, store, prefix)
	normalizedID := normalizePartialID(input, prefixWithHyphen, knownPrefixes)
	return partialIDResolver{
		ctx: ctx, store: store, input: input, prefixWithHyphen: prefixWithHyphen,
		knownPrefixes: knownPrefixes, normalizedID: normalizedID,
		hashPart: strings.TrimPrefix(normalizedID, prefixWithHyphen),
	}
}

func configuredIssuePrefixes(ctx context.Context, store PartialIDResolverStore, prefix string) []string {
	prefixes := []string{strings.TrimSuffix(prefix, "-")}
	allowed, err := store.GetConfig(ctx, "allowed_prefixes")
	if err != nil || allowed == "" {
		return prefixes
	}
	for _, candidate := range strings.Split(allowed, ",") {
		candidate = strings.TrimSuffix(strings.TrimSpace(candidate), "-")
		if candidate != "" {
			prefixes = append(prefixes, candidate)
		}
	}
	return prefixes
}

func normalizePartialID(input, prefix string, knownPrefixes []string) string {
	if strings.HasPrefix(input, prefix) || hasKnownPrefix(input, knownPrefixes) || looksLikePrefixedID(input) {
		return input
	}
	return prefix + input
}

func (r partialIDResolver) issueHash(id string) string {
	prefix := ExtractIssuePrefixKnown(id, r.knownPrefixes)
	if prefix != "" && strings.HasPrefix(id, prefix+"-") {
		return id[len(prefix)+1:]
	}
	return id
}

func (r partialIDResolver) matchDurableIDs(ids []string) (string, []string) {
	var exact string
	var matches []string
	for _, id := range ids {
		if id == r.input {
			return id, matches
		}
		hash := r.issueHash(id)
		if hash == r.hashPart {
			exact = id
		} else if strings.HasPrefix(hash, r.hashPart) {
			matches = append(matches, id)
		}
	}
	return exact, matches
}

func (r partialIDResolver) matchWisps(searchPart string) (string, []string) {
	ephemeral := true
	filter := types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{Ephemeral: &ephemeral}}
	ids, err := r.store.SearchIssueIDs(r.ctx, searchPart, filter)
	if err != nil {
		return "", nil
	}
	var exact string
	var matches []string
	for _, id := range ids {
		if id == r.input {
			return id, matches
		}
		hash := r.issueHash(id)
		wispHash := strings.TrimPrefix(hash, "wisp-")
		if hash == r.hashPart || wispHash == r.hashPart {
			exact = id
		} else if strings.HasPrefix(wispHash, r.hashPart) {
			matches = append(matches, id)
		}
	}
	return exact, matches
}

func finishPartialIDResolution(input string, matches []string) (string, error) {
	if len(matches) == 0 {
		return "", fmt.Errorf("no issue found matching %q", input)
	}
	sort.Strings(matches)
	if len(matches) > 1 {
		return "", fmt.Errorf("%w: %q matches %d issues: %v\nUse more characters to disambiguate", ErrAmbiguousID, input, len(matches), matches)
	}
	return matches[0], nil
}

func partialIDSearchPart(hashPart string) (string, bool) {
	if !looksLikePartialIDHash(hashPart) {
		return "", false
	}
	searchPart := hashPart
	if idx := strings.LastIndex(hashPart, "-"); idx >= 0 && idx < len(hashPart)-1 {
		suffix := hashPart[idx+1:]
		if looksLikePartialIDHash(suffix) {
			searchPart = suffix
		}
	}
	return searchPart, true
}

func looksLikePartialIDHash(input string) bool {
	if input == "" || strings.Contains(input, " ") {
		return false
	}
	for _, c := range input {
		if !isPartialIDRune(c) {
			return false
		}
	}
	return true
}

func isPartialIDRune(r rune) bool {
	return isASCIIAlphaNumeric(r) || r == '-' || r == '.'
}

func isASCIIAlphaNumeric(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

func isASCIILowerAlphaNumeric(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z'
}

// ResolvePartialIDs resolves multiple potentially partial issue IDs.
// Returns the resolved IDs and any errors encountered.
func ResolvePartialIDs(ctx context.Context, store PartialIDResolverStore, inputs []string) ([]string, error) {
	var resolved []string
	for _, input := range inputs {
		fullID, err := ResolvePartialID(ctx, store, input)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, fullID)
	}
	return resolved, nil
}

// looksLikePrefixedID checks if input appears to already have a prefix.
// A prefixed ID has the format "prefix-hash" where prefix is 1-8 lowercase
// letters/numbers and hash is alphanumeric (potentially with dots for hierarchical IDs).
// Examples: "aap-4ar", "bd-a3f8e9", "myproject-abc.1"
func looksLikePrefixedID(input string) bool {
	idx := strings.Index(input, "-")
	if idx <= 0 || idx > 8 {
		// No hyphen, hyphen at start, or prefix too long
		return false
	}

	prefix := input[:idx]
	suffix := input[idx+1:]

	// Prefix must be non-empty lowercase alphanumeric
	for _, c := range prefix {
		if !isASCIILowerAlphaNumeric(c) {
			return false
		}
	}

	// Suffix must be non-empty and start with alphanumeric
	if len(suffix) == 0 {
		return false
	}
	if !isASCIILowerAlphaNumeric(rune(suffix[0])) {
		return false
	}

	return true
}

// hasKnownPrefix checks if input starts with any of the known prefixes followed
// by a hyphen. Used to detect already-prefixed input before falling back to the
// looksLikePrefixedID heuristic.
func hasKnownPrefix(input string, knownPrefixes []string) bool {
	for _, p := range knownPrefixes {
		if p != "" && strings.HasPrefix(input, p+"-") {
			return true
		}
	}
	return false
}
