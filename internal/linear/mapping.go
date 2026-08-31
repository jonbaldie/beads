package linear

import (
	"fmt"

	"github.com/jonbaldie/beads/internal/idgen"
	"github.com/jonbaldie/beads/internal/types"
)

// IDGenerationOptions configures Linear hash ID generation.
type IDGenerationOptions struct {
	BaseLength int             // Starting hash length (3-8)
	MaxLength  int             // Maximum hash length (3-8)
	UsedIDs    map[string]bool // Pre-populated set to avoid collisions (e.g., DB IDs)
}

const missingExplicitStateMapMessage = "linear.state_map is not configured.\nRun 'bd linear link' to configure status mapping first."

// BuildLinearDescription formats a Beads issue for Linear's description field.
// This mirrors the payload used during push to keep hash comparisons consistent.
func BuildLinearDescription(issue *types.Issue) string {
	description := issue.Description
	if issue.AcceptanceCriteria != "" {
		description += "\n\n## Acceptance Criteria\n" + issue.AcceptanceCriteria
	}
	if issue.Design != "" {
		description += "\n\n## Design\n" + issue.Design
	}
	if issue.Notes != "" {
		description += "\n\n## Notes\n" + issue.Notes
	}
	return description
}

// NormalizeIssueForLinearHash returns a copy of the issue using Linear's description
// formatting and clears fields not present in Linear's model to avoid false conflicts.
func NormalizeIssueForLinearHash(issue *types.Issue) *types.Issue {
	normalized := *issue
	normalized.Description = BuildLinearDescription(issue)
	normalized.AcceptanceCriteria = ""
	normalized.Design = ""
	normalized.Notes = ""
	if normalized.ExternalRef != nil && IsLinearExternalRef(*normalized.ExternalRef) {
		if canonical, ok := CanonicalizeLinearExternalRef(*normalized.ExternalRef); ok {
			normalized.ExternalRef = &canonical
		}
	}
	return &normalized
}

// GenerateIssueIDs generates unique hash-based IDs for issues that don't have one.
// Tracks used IDs to prevent collisions within the batch (and optionally against existing IDs).
// The creator parameter is used as part of the hash input (e.g., "linear-import").
func GenerateIssueIDs(issues []*types.Issue, prefix, creator string, opts IDGenerationOptions) error {
	usedIDs, baseLength, maxLength := normalizeIDGenerationOptions(opts)

	// First pass: record existing IDs
	for _, issue := range issues {
		if issue.ID != "" {
			usedIDs[issue.ID] = true
		}
	}

	// Second pass: generate IDs for issues without one
	for _, issue := range issues {
		if issue.ID != "" {
			continue // Already has an ID
		}

		candidate, ok := generateIssueID(issue, prefix, creator, usedIDs, baseLength, maxLength)
		if !ok {
			return fmt.Errorf("failed to generate unique ID for issue '%s' after trying lengths %d-%d with 10 nonces each",
				issue.Title, baseLength, maxLength)
		}
		issue.ID = candidate
		usedIDs[candidate] = true
	}

	return nil
}

func normalizeIDGenerationOptions(opts IDGenerationOptions) (map[string]bool, int, int) {
	usedIDs := opts.UsedIDs
	if usedIDs == nil {
		usedIDs = make(map[string]bool)
	}

	baseLength := opts.BaseLength
	if baseLength == 0 {
		baseLength = 6
	}
	maxLength := opts.MaxLength
	if maxLength == 0 {
		maxLength = 8
	}
	if baseLength < 3 {
		baseLength = 3
	}
	if maxLength > 8 {
		maxLength = 8
	}
	if baseLength > maxLength {
		baseLength = maxLength
	}
	return usedIDs, baseLength, maxLength
}

func generateIssueID(issue *types.Issue, prefix, creator string, usedIDs map[string]bool, baseLength, maxLength int) (string, bool) {
	for length := baseLength; length <= maxLength; length++ {
		for nonce := 0; nonce < 10; nonce++ {
			candidate := idgen.GenerateHashID(
				prefix,
				issue.Title,
				issue.Description,
				creator,
				issue.CreatedAt,
				length,
				nonce,
			)

			if !usedIDs[candidate] {
				return candidate, true
			}
		}
	}
	return "", false
}
