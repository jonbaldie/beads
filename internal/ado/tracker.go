package ado

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/tracker"
)

// Compile-time interface check.
var _ tracker.IssueTracker = (*Tracker)(nil)

func init() {
	tracker.Register("ado", func() tracker.IssueTracker {
		return &Tracker{}
	})
}

// adoWorkItemPattern matches ADO work item URLs containing /_workitems/edit/{digits}.
var adoWorkItemPattern = regexp.MustCompile(`/_workitems/edit/(\d+)`)

// adoShorthandPattern matches the "ado:{digits}" shorthand produced by BuildExternalRef
// when a full URL cannot be constructed (e.g., missing org/project config).
var adoShorthandPattern = regexp.MustCompile(`^ado:([1-9]\d*)$`)

// Tracker implements tracker.IssueTracker for Azure DevOps. It is registered
// under the name "ado" and supports bidirectional sync of work items between
// ADO and the local beads database.
type Tracker struct {
	client   *Client
	store    storage.Storage
	mapper   tracker.FieldMapper
	baseURL  string // Resolved base URL for external ref matching
	org      string
	projects []string     // one or more project names (first is primary)
	filters  *PullFilters // Optional pull filters for WIQL queries
}

// SetFilters configures pull filters for WIQL queries.
// When set, FetchIssues will only return work items matching these filters.
func (t *Tracker) SetFilters(f *PullFilters) { t.filters = f }

// getConfig reads a config value from storage, falling back to an environment variable.
// For yaml-only keys (e.g. ado.pat), reads from config.yaml first
// to avoid leaking secrets when pushing the Dolt database to remotes.
func (t *Tracker) getConfig(ctx context.Context, key, envVar string) string {
	// Secret keys are stored in config.yaml, not the Dolt database,
	// to avoid leaking secrets when pushing to remotes.
	if config.IsYamlOnlyKey(key) {
		if val := config.GetString(key); val != "" {
			return val
		}
		if envVar != "" {
			if envVal := os.Getenv(envVar); envVal != "" {
				return envVal
			}
		}
		return ""
	}

	val, err := t.store.GetConfig(ctx, key)
	if err == nil && val != "" {
		return val
	}
	if envVar != "" {
		if envVal := os.Getenv(envVar); envVal != "" {
			return envVal
		}
	}
	return ""
}

// SetProjects sets project names before Init(). When set, Init() uses these
// instead of reading from config. This supports the --project CLI flag.

// Projects returns the list of configured project names.

// PrimaryProject returns the first configured project name.

// Name returns the lowercase identifier for this tracker.

// DisplayName returns the human-readable name for this tracker.

// ConfigPrefix returns the config key prefix for this tracker.

// Init initializes the tracker with configuration from the beads config store.
// No network calls are made during initialization.

type adoInitConfig struct {
	pat       string
	customURL string
}

func loadADOInitConfig(t *Tracker, ctx context.Context) (adoInitConfig, error) {
	pat := t.getConfig(ctx, "ado.pat", "AZURE_DEVOPS_PAT")
	if pat == "" {
		return adoInitConfig{}, fmt.Errorf("Azure DevOps PAT not configured (set ado.pat or AZURE_DEVOPS_PAT)")
	}

	t.org = t.getConfig(ctx, "ado.org", "AZURE_DEVOPS_ORG")
	customURL := t.getConfig(ctx, "ado.url", "AZURE_DEVOPS_URL")

	if t.org == "" && customURL == "" {
		return adoInitConfig{}, fmt.Errorf("Azure DevOps organization not configured (set ado.org or AZURE_DEVOPS_ORG)")
	}

	resolveADOProjects(t, ctx)
	if len(t.projects) == 0 {
		return adoInitConfig{}, fmt.Errorf("Azure DevOps project not configured (set ado.project, ado.projects, or AZURE_DEVOPS_PROJECT)")
	}
	if err := validateADOInitConfig(t); err != nil {
		return adoInitConfig{}, err
	}
	return adoInitConfig{pat: pat, customURL: customURL}, nil
}

func resolveADOProjects(t *Tracker, ctx context.Context) {
	if len(t.projects) != 0 {
		return
	}
	pluralVal := t.getConfig(ctx, "ado.projects", "AZURE_DEVOPS_PROJECTS")
	singularVal := t.getConfig(ctx, "ado.project", "AZURE_DEVOPS_PROJECT")
	t.projects = tracker.ResolveProjectIDs(nil, pluralVal, singularVal)
}

func validateADOInitConfig(t *Tracker) error {
	if t.org != "" {
		if err := ValidateOrg(t.org); err != nil {
			return fmt.Errorf("invalid Azure DevOps organization: %w", err)
		}
	}
	for _, project := range t.projects {
		if err := ValidateProject(project); err != nil {
			return fmt.Errorf("invalid Azure DevOps project %q: %w", project, err)
		}
	}
	return nil
}

func configureADOMappings(t *Tracker, ctx context.Context) {
	// Read custom state/type mappings from config.
	// Uses prefix-scan to support custom types (e.g., ado.type_map.story).
	stateMap := t.readMappingConfigByPrefix(ctx, "ado.state_map.")
	typeMap := t.readMappingConfigByPrefix(ctx, "ado.type_map.")

	t.mapper = NewFieldMapper(stateMap, typeMap)
}

func initializeADOClient(t *Tracker, config adoInitConfig) error {
	// Create client with primary project for API URL construction.
	t.client = NewClient(NewSecretString(config.pat), t.org, t.PrimaryProject())
	if config.customURL != "" {
		var err error
		t.client, err = t.client.WithBaseURL(config.customURL)
		if err != nil {
			return fmt.Errorf("invalid Azure DevOps URL: %w", err)
		}
		t.baseURL = strings.TrimSuffix(config.customURL, "/")
	} else if t.org != "" {
		t.baseURL = DefaultBaseURL + "/" + t.org
	}

	return nil
}

// Validate checks that the tracker is properly configured and can connect
// to the Azure DevOps API.

// Close releases any resources held by the tracker.

// ADOClient returns the underlying ADO API client.
// Callers use this for operations like link sync that need direct API access.

// FetchIssues retrieves work items from Azure DevOps. If opts.Since is set,
// only work items changed after that time are fetched (incremental sync);
// otherwise all matching work items in the project are returned (full sync).

// FetchIssue retrieves a single work item by its numeric ID.
// Returns nil, nil if the work item doesn't exist.

// CreateIssue creates a new work item in Azure DevOps. If the target state
// is not a valid initial state (e.g., "Closed"), the work item is created
// without a state (ADO assigns its default) and then transitioned through
// intermediate states to reach the target.

// UpdateIssue updates an existing work item in Azure DevOps.

// FieldMapper returns the bidirectional field mapper used to convert priorities,
// statuses, types, and issue data between ADO and beads representations.

// IsExternalRef checks if a URL belongs to this Azure DevOps tracker.
// It recognizes both full ADO URLs and the "ado:{id}" shorthand format
// produced by BuildExternalRef when org/project config is unavailable.

// ExtractIdentifier extracts the work item ID from an ADO URL or shorthand ref.

// BuildExternalRef constructs an Azure DevOps web URL for the given tracker issue.
// It prefers the issue's existing URL, then falls back to constructing one from
// the configured org/project or base URL. Returns an "ado:{id}" URI as a last resort.

// getConfig reads a config value from storage, falling back to an environment variable.
// For yaml-only keys (e.g. ado.pat), reads from config.yaml first
// to avoid leaking secrets when pushing the Dolt database to remotes.

// readMappingConfigByPrefix reads all config keys with the given prefix and
// returns a map of suffix → value. This supports both built-in and custom
// types/states (e.g., ado.type_map.story → "User Story").

// adoWorkItemToTrackerIssue converts a WorkItem to a tracker.TrackerIssue.
func adoWorkItemToTrackerIssue(wi *WorkItem) tracker.TrackerIssue {
	ti := tracker.TrackerIssue{
		ID:          strconv.Itoa(wi.ID),
		Identifier:  strconv.Itoa(wi.ID),
		URL:         buildExternalRef(wi),
		Title:       wi.GetStringField(FieldTitle),
		Description: wi.GetStringField(FieldDescription),
		State:       wi.GetStringField(FieldState),
		Type:        wi.GetStringField(FieldWorkItemType),
		Labels:      parseTags(wi.GetStringField(FieldTags)),
		Raw:         wi,
	}

	ti.Priority = wi.GetIntField(FieldPriority)
	applyADOTimeFields(&ti, wi)
	applyADOAssignee(&ti, wi)
	applyADOMetadata(&ti, wi)
	return ti
}

func applyADOTimeFields(ti *tracker.TrackerIssue, wi *WorkItem) {
	if created := wi.GetStringField(FieldCreatedDate); created != "" {
		if ts, err := time.Parse(time.RFC3339Nano, created); err == nil {
			ti.CreatedAt = ts
		}
	}
	if updated := wi.GetStringField(FieldChangedDate); updated != "" {
		if ts, err := time.Parse(time.RFC3339Nano, updated); err == nil {
			ti.UpdatedAt = ts
		}
	}
}

func applyADOAssignee(ti *tracker.TrackerIssue, wi *WorkItem) {
	// AssignedTo can be a string or identity object.
	switch v := wi.GetField(FieldAssignedTo).(type) {
	case string:
		ti.Assignee = v
	case map[string]interface{}:
		if name, ok := v["displayName"].(string); ok {
			ti.Assignee = name
		}
		if uid, ok := v["uniqueName"].(string); ok {
			ti.AssigneeEmail = uid
		}
	}
}

func applyADOMetadata(ti *tracker.TrackerIssue, wi *WorkItem) {
	ti.Metadata = map[string]interface{}{
		"ado.rev": wi.Rev,
	}
	if ap := wi.GetStringField(FieldAreaPath); ap != "" {
		ti.Metadata["ado.area_path"] = ap
	}
	if ip := wi.GetStringField(FieldIterationPath); ip != "" {
		ti.Metadata["ado.iteration_path"] = ip
	}
	if sp := wi.GetField(FieldStoryPoints); sp != nil {
		ti.Metadata["ado.story_points"] = sp
	}
}
