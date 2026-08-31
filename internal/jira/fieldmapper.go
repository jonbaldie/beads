package jira

import "strings"

// jiraFieldMapper implements tracker.FieldMapper for Jira.
type jiraFieldMapper struct {
	apiVersion       string                            // "2" or "3" (default: "3")
	statusMap        map[string]string                 // beads status → Jira status name (from jira.status_map.* config)
	typeMap          map[string]string                 // beads type → Jira type (from jira.type_map.* config)
	priorityMap      map[string]string                 // beads priority (as string "0"-"4") → Jira priority name (from jira.priority_map.* config)
	customFields     map[string]interface{}            // Jira field name/id → value (from jira.custom_fields.* config)
	typeCustomFields map[string]map[string]interface{} // Jira issue type → field name/id → value
}

func newJiraFieldMapper(apiVersion string, statusMap, typeMap, priorityMap map[string]string, customFields map[string]interface{}, typeCustomFields map[string]map[string]interface{}) *jiraFieldMapper {
	return &jiraFieldMapper{
		apiVersion:       apiVersion,
		statusMap:        statusMap,
		typeMap:          typeMap,
		priorityMap:      priorityMap,
		customFields:     customFields,
		typeCustomFields: typeCustomFields,
	}
}

func defaultJiraPriority(name string) (int, bool) {
	switch name {
	case "Highest":
		return 0, true
	case "High":
		return 1, true
	case "Medium":
		return 2, true
	case "Low":
		return 3, true
	case "Lowest":
		return 4, true
	default:
		return 0, false
	}
}

func addJiraDescription(fields map[string]interface{}, apiVersion, description string) {
	if description == "" {
		return
	}
	if apiVersion == "2" {
		fields["description"] = description
		return
	}
	fields["description"] = PlainTextToADF(description)
}

func addJiraType(fields map[string]interface{}, typeValue interface{}) string {
	name, ok := typeValue.(string)
	if ok {
		fields["issuetype"] = map[string]string{"name": name}
	}
	return name
}

func addJiraPriority(fields map[string]interface{}, priorityValue interface{}) {
	if name, ok := priorityValue.(string); ok {
		fields["priority"] = map[string]string{"name": name}
	}
}

func addJiraLabels(fields map[string]interface{}, labels []string) {
	if len(labels) > 0 {
		fields["labels"] = labels
	}
}

func addJiraCustomFields(fields map[string]interface{}, customFields map[string]interface{}) {
	for fieldName, value := range customFields {
		fields[fieldName] = value
	}
}

func addJiraTypeCustomFields(fields map[string]interface{}, customFields map[string]map[string]interface{}, typeName string) {
	if typeName == "" {
		return
	}
	for jiraType, fieldsForType := range customFields {
		if !strings.EqualFold(jiraType, typeName) {
			continue
		}
		addJiraCustomFields(fields, fieldsForType)
	}
}

// Helper functions for safe field extraction from Jira issues.

func priorityName(ji *Issue) string {
	if ji.Fields.Priority != nil {
		return ji.Fields.Priority.Name
	}
	return ""
}

func statusName(ji *Issue) string {
	if ji.Fields.Status != nil {
		return ji.Fields.Status.Name
	}
	return ""
}

func typeName(ji *Issue) string {
	if ji.Fields.IssueType != nil {
		return ji.Fields.IssueType.Name
	}
	return ""
}

// extractBrowseURL builds the human-readable browse URL from a Jira issue.
// Self is "https://company.atlassian.net/rest/api/3/issue/10001";
// we need "https://company.atlassian.net/browse/PROJ-123".
func extractBrowseURL(ji *Issue) string {
	if ji.Self == "" || ji.Key == "" {
		return ""
	}
	if idx := strings.Index(ji.Self, "/rest/api/"); idx > 0 {
		return ji.Self[:idx] + "/browse/" + ji.Key
	}
	return ""
}
