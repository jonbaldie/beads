package issueops

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestValidateReadyFlagScopeRejectsEveryUnsupportedField(t *testing.T) {
	for _, field := range readyScopeFields {
		t.Run(field.name, func(t *testing.T) {
			req := ListRequest{ListModeOptions: ListModeOptions{ReadyFlag: true}}
			setReadyScopeField(t, &req, field.name)

			err := ValidateReadyFlagScope(req)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("ValidateReadyFlagScope() error = %v, want ErrValidation", err)
			}
			for _, fragment := range []string{field.name, field.flag} {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("ValidateReadyFlagScope() error = %q, want fragment %q", err, fragment)
				}
			}
		})
	}
}

func TestValidateReadyFlagScopeAcceptsOrdinaryAndSupportedReadyRequests(t *testing.T) {
	now := time.Now()
	priority := 1
	limit := 20
	molType := MolType("molecule")
	wispType := WispType("heartbeat")

	tests := []struct {
		name string
		req  ListRequest
	}{
		{
			name: "ordinary listing may use unsupported filters",
			req: ListRequest{
				ListIdentityFilters: ListIdentityFilters{IDFilter: "bd-1"},
				ListTimeFilters:     ListTimeFilters{CreatedAfter: &now},
			},
		},
		{
			name: "empty ready request",
			req:  ListRequest{ListModeOptions: ListModeOptions{ReadyFlag: true}},
		},
		{
			name: "supported ready vocabulary",
			req: ListRequest{
				ListIdentityFilters: ListIdentityFilters{IssueType: "task", Assignee: "alice"},
				ListLabelFilters: ListLabelFilters{
					Labels: []string{"all"}, LabelsAny: []string{"any"},
					ExcludeLabels: []string{"none"}, LabelPattern: "prefix-*", LabelRegex: "^team-",
				},
				ListProjectionOptions: ListProjectionOptions{NoAssignee: true, SkipLabels: true, SkipCounts: true, Brief: true},
				ListPriorityFilters:   ListPriorityFilters{Priority: &priority},
				ListVisibilityOptions: ListVisibilityOptions{
					NoPinnedFlag: true, IncludeTemplates: true, IncludeGates: true,
					IncludeInfra: true, IncludeEphemeral: true, IncludeAllTypes: true,
					ExcludeTypes: []string{"gate"},
				},
				ListRelationFilters: ListRelationFilters{ParentID: "bd-parent", MolType: &molType, WispType: &wispType},
				ListStateFilters: ListStateFilters{
					MetadataFields: map[string]string{"team": "core"}, HasMetadataKey: "owner",
				},
				ListModeOptions: ListModeOptions{AllFlag: true, ReadyFlag: true},
				ListPageOptions: ListPageOptions{
					SortBy: "priority", Reverse: true, Limit: &limit, Offset: 2,
					MaxRows: 100, MaxRowsSource: "test",
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateReadyFlagScope(test.req); err != nil {
				t.Fatalf("ValidateReadyFlagScope() error = %v", err)
			}
		})
	}
}

func TestValidateReadyFlagScopeNamesEveryUnsupportedFieldInOrder(t *testing.T) {
	req := ListRequest{ListModeOptions: ListModeOptions{ReadyFlag: true}}
	for _, field := range readyScopeFields {
		setReadyScopeField(t, &req, field.name)
	}

	err := ValidateReadyFlagScope(req)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("ValidateReadyFlagScope() error = %v, want ErrValidation", err)
	}
	message := err.Error()
	position := -1
	for _, field := range readyScopeFields {
		fragment := field.name + " (" + field.flag + ")"
		next := strings.Index(message, fragment)
		if next <= position {
			t.Fatalf("ValidateReadyFlagScope() error = %q, want %q after position %d", message, fragment, position)
		}
		position = next
	}
}

func setReadyScopeField(t *testing.T, req *ListRequest, name string) {
	t.Helper()
	if name == "AfterCreatedAt/AfterID" {
		now := time.Now()
		req.AfterCreatedAt = &now
		req.AfterID = "bd-cursor"
		return
	}

	value := reflect.ValueOf(req).Elem().FieldByName(name)
	if !value.IsValid() {
		t.Fatalf("ListRequest has no promoted field %q", name)
	}
	switch value.Kind() {
	case reflect.String:
		value.SetString("set")
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
	default:
		t.Fatalf("unsupported ready-scope field %s kind %s", name, value.Kind())
	}
}
