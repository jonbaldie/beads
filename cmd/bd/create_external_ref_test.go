package main

import (
	"testing"

	"github.com/jonbaldie/beads/internal/types"
)

func TestBuildCreateIssueExternalRefPrelink(t *testing.T) {
	ref := "https://linear.app/team/issue/TEAM-123/fix-login"

	issue := buildCreateIssue(createIssueParams{
		ident: createIssueIdentity{
			Title:       "Pre-linked Linear issue",
			ExternalRef: ref,
		},
		class: createIssueClass{
			Priority:  2,
			IssueType: types.TypeTask,
		},
	})

	if issue.ExternalRef == nil {
		t.Fatal("ExternalRef is nil, want pre-linked Linear URL")
	}
	if *issue.ExternalRef != ref {
		t.Fatalf("ExternalRef = %q, want %q", *issue.ExternalRef, ref)
	}
}

func TestBuildCreateIssueEmptyExternalRefIsNil(t *testing.T) {
	issue := buildCreateIssue(createIssueParams{
		ident: createIssueIdentity{Title: "Local-only issue"},
		class: createIssueClass{Priority: 2, IssueType: types.TypeTask},
	})

	if issue.ExternalRef != nil {
		t.Fatalf("ExternalRef = %v, want nil", issue.ExternalRef)
	}
}
