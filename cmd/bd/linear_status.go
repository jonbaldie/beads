package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/linear"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

func runLinearStatus(_ *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("linear status is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("linear-status")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	if err := ensureStoreActive(); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	status, err := loadLinearStatus(getRootContext())
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if isJSONOutput() {
		return renderLinearStatusJSON(status)
	}
	renderLinearStatusText(status)
	return nil
}

type linearStatus struct {
	apiKey        string
	hasOAuth      bool
	configured    bool
	teamIDs       []string
	lastSync      string
	totalIssues   int
	withLinearRef int
	pendingPush   int
}

func loadLinearStatus(ctx context.Context) (linearStatus, error) {
	apiKey := getLinearConfig(ctx, "linear.api_key")
	oauthClientID := getLinearConfig(ctx, "linear.oauth_client_id")
	oauthClientSecret := getLinearConfig(ctx, "linear.oauth_client_secret")
	teamIDs := getLinearTeamIDs(ctx, nil)
	lastSync, _ := getStore().GetConfig(ctx, "linear.last_sync")
	allIssues, err := getStore().SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil {
		return linearStatus{}, err
	}
	withLinearRef, pendingPush := countLinearStatusIssues(allIssues)
	hasOAuth := oauthClientID != "" && oauthClientSecret != ""
	return linearStatus{
		apiKey:        apiKey,
		hasOAuth:      hasOAuth,
		configured:    (apiKey != "" || hasOAuth) && len(teamIDs) > 0,
		teamIDs:       teamIDs,
		lastSync:      lastSync,
		totalIssues:   len(allIssues),
		withLinearRef: withLinearRef,
		pendingPush:   pendingPush,
	}, nil
}

func countLinearStatusIssues(issues []*types.Issue) (withLinearRef, pendingPush int) {
	for _, issue := range issues {
		if issue.ExternalRef != nil && linear.IsLinearExternalRef(*issue.ExternalRef) {
			withLinearRef++
		} else if issue.ExternalRef == nil {
			pendingPush++
		}
	}
	return withLinearRef, pendingPush
}

func renderLinearStatusJSON(status linearStatus) error {
	hasAPIKey := status.apiKey != ""
	teamID := ""
	if len(status.teamIDs) > 0 {
		teamID = status.teamIDs[0]
	}
	authMode := "none"
	if status.hasOAuth {
		authMode = "oauth"
	} else if hasAPIKey {
		authMode = "api_key"
	}
	return outputJSON(map[string]interface{}{
		"configured":      status.configured,
		"has_api_key":     hasAPIKey,
		"has_oauth":       status.hasOAuth,
		"auth_mode":       authMode,
		"team_id":         teamID,
		"team_ids":        status.teamIDs,
		"last_sync":       status.lastSync,
		"total_issues":    status.totalIssues,
		"with_linear_ref": status.withLinearRef,
		"pending_push":    status.pendingPush,
	})
}

func renderLinearStatusText(status linearStatus) {
	fmt.Println("Linear Sync Status")
	fmt.Println("==================")
	fmt.Println()
	if !status.configured {
		renderUnconfiguredLinearStatus()
		return
	}
	renderConfiguredLinearStatus(status)
}

func renderUnconfiguredLinearStatus() {
	fmt.Println("Status: Not configured")
	fmt.Println()
	fmt.Println("To configure Linear integration:")
	fmt.Println("  bd config set linear.api_key \"YOUR_API_KEY\"")
	fmt.Println("  bd config set linear.team_id \"TEAM_ID\"")
	fmt.Println("  bd config set linear.team_ids \"TEAM_ID1,TEAM_ID2\"  # multiple teams")
	fmt.Println()
	fmt.Println("Or use environment variables:")
	fmt.Println("  export LINEAR_API_KEY=\"YOUR_API_KEY\"")
	fmt.Println("  export LINEAR_TEAM_ID=\"TEAM_ID\"")
	fmt.Println()
	fmt.Println("For CI/OAuth authentication:")
	fmt.Println("  export LINEAR_OAUTH_CLIENT_ID=\"...\"")
	fmt.Println("  export LINEAR_OAUTH_CLIENT_SECRET=\"...\"")
}

func renderConfiguredLinearStatus(status linearStatus) {
	if len(status.teamIDs) == 1 {
		fmt.Printf("Team ID:      %s\n", status.teamIDs[0])
	} else {
		fmt.Printf("Team IDs:     %s (%d teams)\n", strings.Join(status.teamIDs, ", "), len(status.teamIDs))
	}
	if status.hasOAuth {
		fmt.Printf("Auth:         OAuth (client_credentials)\n")
	} else {
		fmt.Printf("API Key:      %s\n", maskAPIKey(status.apiKey))
	}
	if status.lastSync != "" {
		fmt.Printf("Last Sync:    %s\n", status.lastSync)
	} else {
		fmt.Println("Last Sync:    Never")
	}
	fmt.Println()
	fmt.Printf("Total Issues: %d\n", status.totalIssues)
	fmt.Printf("With Linear:  %d\n", status.withLinearRef)
	fmt.Printf("Local Only:   %d\n", status.pendingPush)
	if status.pendingPush > 0 {
		fmt.Println()
		fmt.Printf("Run 'bd linear sync --push' to push %d local issue(s) to Linear\n", status.pendingPush)
	}
}
