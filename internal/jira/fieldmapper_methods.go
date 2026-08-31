package jira

import (
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
)

func (m *jiraFieldMapper) PriorityToBeads(trackerPriority interface{}) int {
	name, ok := trackerPriority.(string)
	if !ok {
		return 2
	}
	if priority, ok := m.customPriority(name); ok {
		return priority
	}
	if priority, ok := defaultJiraPriority(name); ok {
		return priority
	}
	return 2
}

func (m *jiraFieldMapper) PriorityToTracker(beadsPriority int) interface{} {
	// Check custom map first (beads priority as string key → Jira name).
	if m.priorityMap != nil {
		key := strconv.Itoa(beadsPriority)
		if name, ok := m.priorityMap[key]; ok {
			return name
		}
	}
	// Jira defaults.
	switch beadsPriority {
	case 0:
		return "Highest"
	case 1:
		return "High"
	case 2:
		return "Medium"
	case 3:
		return "Low"
	case 4:
		return "Lowest"
	default:
		return "Medium"
	}
}

func (m *jiraFieldMapper) StatusToBeads(trackerState interface{}) types.Status {
	if state, ok := trackerState.(string); ok {
		// Check custom map first (inverted: jira name → beads status).
		for beadsStatus, jiraName := range m.statusMap {
			if strings.EqualFold(state, jiraName) {
				return types.Status(beadsStatus)
			}
		}
		switch state {
		case "To Do", "Open", "Backlog", "New":
			return types.StatusOpen
		case "In Progress", "In Review":
			return types.StatusInProgress
		case "Blocked":
			return types.StatusBlocked
		case "Done", "Closed", "Resolved":
			return types.StatusClosed
		}
	}
	return types.StatusOpen
}

func (m *jiraFieldMapper) StatusToTracker(beadsStatus types.Status) interface{} {
	// Check custom map first.
	if name, ok := m.statusMap[string(beadsStatus)]; ok {
		return name
	}
	switch beadsStatus {
	case types.StatusOpen:
		return "To Do"
	case types.StatusInProgress:
		return "In Progress"
	case types.StatusBlocked:
		return "Blocked"
	case types.StatusClosed:
		return "Done"
	default:
		return "To Do"
	}
}

func (m *jiraFieldMapper) TypeToBeads(trackerType interface{}) types.IssueType {
	t, ok := trackerType.(string)
	if !ok {
		return types.TypeTask
	}

	// Check custom map first (inverted: Jira type → beads type).
	for beadsType, jiraType := range m.typeMap {
		if strings.EqualFold(t, jiraType) {
			return types.IssueType(beadsType)
		}
	}

	// Jira defaults.
	switch t {
	case "Bug":
		return types.TypeBug
	case "Story", "Feature":
		return types.TypeFeature
	case "Epic":
		return types.TypeEpic
	case "Task", "Sub-task":
		return types.TypeTask
	}
	return types.TypeTask
}

func (m *jiraFieldMapper) TypeToTracker(beadsType types.IssueType) interface{} {
	if name, ok := m.typeMap[string(beadsType)]; ok {
		return name
	}
	switch beadsType {
	case types.TypeBug:
		return "Bug"
	case types.TypeFeature:
		return "Story"
	case types.TypeEpic:
		return "Epic"
	default:
		return "Task"
	}
}

func (m *jiraFieldMapper) IssueToTracker(issue *types.Issue) map[string]interface{} {
	fields := map[string]interface{}{
		"summary": issue.Title,
	}
	addJiraDescription(fields, m.apiVersion, issue.Description)
	typeName := addJiraType(fields, m.TypeToTracker(issue.IssueType))
	addJiraPriority(fields, m.PriorityToTracker(issue.Priority))
	addJiraLabels(fields, issue.Labels)
	addJiraCustomFields(fields, m.customFields)
	addJiraTypeCustomFields(fields, m.typeCustomFields, typeName)

	return fields
}

func (m *jiraFieldMapper) customPriority(name string) (int, bool) {
	for beadsPri, jiraName := range m.priorityMap {
		if strings.EqualFold(name, jiraName) {
			value, err := strconv.Atoi(beadsPri)
			if err == nil && value >= 0 && value <= 4 {
				return value, true
			}
		}
	}
	return 0, false
}
func (m *jiraFieldMapper) IssueToBeads(ti *tracker.TrackerIssue) *tracker.IssueConversion {
	ji, ok := ti.Raw.(*Issue)
	if !ok || ji == nil {
		return nil
	}

	issue := &types.Issue{
		IssueContent: types.IssueContent{
			Title:       ji.Fields.Summary,
			Description: DescriptionToPlainText(ji.Fields.Description),
		},
		IssueWorkflow: types.IssueWorkflow{
			Priority:  m.PriorityToBeads(priorityName(ji)),
			Status:    m.StatusToBeads(statusName(ji)),
			IssueType: m.TypeToBeads(typeName(ji)),
		},
	}

	if ji.Fields.Assignee != nil {
		issue.Owner = ji.Fields.Assignee.DisplayName
	}

	if ji.Fields.Labels != nil {
		issue.Labels = ji.Fields.Labels
	}

	// Set external ref from issue URL
	if ji.Self != "" {
		ref := extractBrowseURL(ji)
		issue.ExternalRef = &ref
	}

	return &tracker.IssueConversion{
		Issue: issue,
	}
}
