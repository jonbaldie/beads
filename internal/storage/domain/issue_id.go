package domain

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/idgen"
	"github.com/jonbaldie/beads/internal/types"
)

// resolveTopLevelPrefix picks the prefix for a freshly-minted top-level ID,
// mirroring the embedded path's precedence (issueops/create.go:88-96 and
// dolt/wisps.go wispPrefix). Reads issue_prefix from config once and trims
// the trailing hyphen so a config value of "bd-" yields "bd-<hash>" rather
// than "bd--<hash>".
func (u *issueCreateModule) resolveTopLevelPrefix(ctx context.Context, issue *types.Issue, useWisp bool) (string, error) {
	if issue.PrefixOverride != "" {
		return issue.PrefixOverride, nil
	}

	configPrefix, err := u.cfgRepo.GetConfig(ctx, "issue_prefix")
	if err != nil {
		return "", fmt.Errorf("read issue_prefix: %w", err)
	}
	configPrefix = strings.TrimSuffix(configPrefix, "-")
	if configPrefix == "" {
		return "", fmt.Errorf("issue_prefix config is missing")
	}

	switch {
	case issue.IDPrefix != "":
		return configPrefix + "-" + issue.IDPrefix, nil
	case useWisp:
		return configPrefix + "-wisp", nil
	}
	return configPrefix, nil
}

// mintTopLevelID generates a fresh top-level ID for an issue that has no
// ExplicitID and no ParentID. Honors counter mode for non-wisps (config key
// issue_id_mode=counter); otherwise uses adaptive hash-mode IDs that mirror
// issueops.GenerateIssueIDInTable. Reads issue.CreatedAt (caller must have
// stabilized it before this call so retries hash the same value).
func (u *issueCreateModule) mintTopLevelID(ctx context.Context, issue *types.Issue, actor string, useWisp bool) (string, error) {
	prefix, err := u.resolveTopLevelPrefix(ctx, issue, useWisp)
	if err != nil {
		return "", err
	}

	// Counter mode applies only to the issues table — wisps always hash-mint
	// because there is no wisp_counter table and ephemeral churn would make
	// a monotonic counter meaningless.
	if counterID, ok, err := counterTopLevelID(ctx, u.cfgRepo, u.issueRepo, prefix, useWisp); err != nil {
		return "", err
	} else if ok {
		return counterID, nil
	}

	cfg, err := u.cfgRepo.GetAdaptiveIDConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("read adaptive id config: %w", err)
	}
	tableOpts := IssueTableOpts{UseWispsTable: useWisp}

	count, err := u.issueRepo.CountForPrefix(ctx, prefix, tableOpts)
	if err != nil {
		return "", err
	}
	baseLength := ComputeAdaptiveLength(count, cfg)
	if baseLength > cfg.MaxLength {
		baseLength = cfg.MaxLength
	}
	return findAvailableTopLevelID(ctx, u.issueRepo, prefix, issue, actor, baseLength, cfg.MaxLength, useWisp)
}

func counterTopLevelID(ctx context.Context, cfgRepo ConfigSQLRepository, issueRepo IssueSQLRepository, prefix string, useWisp bool) (string, bool, error) {
	if useWisp {
		return "", false, nil
	}
	mode, err := cfgRepo.GetConfig(ctx, "issue_id_mode")
	if err != nil {
		return "", false, fmt.Errorf("read issue_id_mode: %w", err)
	}
	if mode != "counter" {
		return "", false, nil
	}
	n, err := issueRepo.NextCounterID(ctx, prefix)
	if err != nil {
		return "", false, err
	}
	return fmt.Sprintf("%s-%d", prefix, n), true, nil
}

func findAvailableTopLevelID(ctx context.Context, issueRepo IssueSQLRepository, prefix string, issue *types.Issue, actor string, baseLength, maxLength int, useWisp bool) (string, error) {
	tableOpts := IssueTableOpts{UseWispsTable: useWisp}
	for length := baseLength; length <= maxLength; length++ {
		for nonce := 0; nonce < 10; nonce++ {
			candidate := idgen.GenerateHashID(prefix, issue.Title, issue.Description, actor, issue.CreatedAt, length, nonce)
			exists, err := issueRepo.Exists(ctx, candidate, tableOpts)
			if err != nil {
				return "", err
			}
			if !exists {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("failed to generate unique ID for prefix %q after lengths %d..%d with 10 nonces each", prefix, baseLength, maxLength)
}
