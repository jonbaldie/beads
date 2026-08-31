//go:build cgo

package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jonbaldie/beads/internal/types"
)

func TestValidateIssueUpdatable(t *testing.T) {
	if err := validateIssueUpdatable("x", nil); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if err := validateIssueUpdatable("x", &types.Issue{IssueWisp: types.IssueWisp{IsTemplate: false}}); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if err := validateIssueUpdatable("bd-1", &types.Issue{IssueWisp: types.IssueWisp{IsTemplate: true}}); err == nil {
		t.Fatalf("expected error")
	}
}

func TestValidateIssueClosable(t *testing.T) {
	if err := validateIssueClosable("x", nil, "alice", false); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if err := validateIssueClosable("bd-1", &types.Issue{IssueWisp: types.IssueWisp{IsTemplate: true}}, "alice", false); err == nil {
		t.Fatalf("expected template close error")
	}
	if err := validateIssueClosable("bd-2", &types.Issue{IssueWorkflow: types.IssueWorkflow{Status: types.StatusPinned}}, "alice", false); err == nil {
		t.Fatalf("expected pinned close error")
	}
	if err := validateIssueClosable("bd-2", &types.Issue{IssueWorkflow: types.IssueWorkflow{Status: types.StatusPinned}}, "alice", true); err != nil {
		t.Fatalf("expected pinned close to succeed with force, got %v", err)
	}

	// ga-z3vht: pinned=true protects the bead independently of status, so
	// `bd close` refuses it without --force on both the direct and proxied path.
	booleanPinned := &types.Issue{IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen}, IssueWisp: types.IssueWisp{Pinned: true}}
	if err := validateIssueClosable("bd-6", booleanPinned, "alice", false); err == nil {
		t.Fatalf("expected boolean-pinned close error")
	}
	if err := validateIssueClosable("bd-6", booleanPinned, "alice", true); err != nil {
		t.Fatalf("expected boolean-pinned close to succeed with force, got %v", err)
	}

	// ga-ktn9pe.4.8: a closed row carrying pinned=true is the residue left by a
	// forced close, and this guard still refuses it — closed status earns no
	// exemption from the boolean trigger. Idempotent re-close was restored by
	// ORDERING instead: cmd/bd/close.go and close_proxied_server.go skip close
	// validation entirely for a row already closed at resolve time, so this guard
	// is never reached on the no-op retry. Do not "simplify" it by exempting
	// closed here — that would also disarm the guard on live status transitions.
	closedPinnedResidue := &types.Issue{IssueWorkflow: types.IssueWorkflow{Status: types.StatusClosed}, IssueWisp: types.IssueWisp{Pinned: true}}
	if err := validateIssueClosable("bd-7", closedPinnedResidue, "alice", false); err == nil {
		t.Fatalf("expected closed+pinned residue to refuse a plain close")
	}
	if err := validateIssueClosable("bd-7", closedPinnedResidue, "alice", true); err != nil {
		t.Fatalf("expected closed+pinned residue close to succeed with force, got %v", err)
	}

	// be-035: actor != assignee must be refused without --force.
	mismatched := &types.Issue{IssueWorkflow: types.IssueWorkflow{Assignee: "bob"}}
	if err := validateIssueClosable("bd-3", mismatched, "alice", false); err == nil {
		t.Fatalf("expected actor/assignee mismatch error")
	}
	// --force overrides the authority check.
	if err := validateIssueClosable("bd-3", mismatched, "alice", true); err != nil {
		t.Fatalf("expected close to succeed with force despite mismatch, got %v", err)
	}
	// Same-actor close is allowed.
	if err := validateIssueClosable("bd-4", &types.Issue{IssueWorkflow: types.IssueWorkflow{Assignee: "alice"}}, "alice", false); err != nil {
		t.Fatalf("expected matching-assignee close to succeed, got %v", err)
	}
	// Unassigned beads can be closed by anyone (lots of bd's flow involves
	// closing beads nobody claimed).
	if err := validateIssueClosable("bd-5", &types.Issue{IssueWorkflow: types.IssueWorkflow{Assignee: ""}}, "alice", false); err != nil {
		t.Fatalf("expected unassigned close to succeed, got %v", err)
	}
}

func TestFindRepliesToAndReplies_WorksWithDoltStorage(t *testing.T) {
	ctx := context.Background()
	st := newTestStoreWithPrefix(t, filepath.Join(t.TempDir(), "test.db"), "test")

	root := &types.Issue{IssueContent: types.IssueContent{Title: "root"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: "message", Assignee: "b"}, IssueWisp: types.IssueWisp{Sender: "a"}}
	reply1 := &types.Issue{IssueContent: types.IssueContent{Title: "r1"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: "message", Assignee: "a"}, IssueWisp: types.IssueWisp{Sender: "b"}}
	reply2 := &types.Issue{IssueContent: types.IssueContent{Title: "r2"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, Priority: 2, IssueType: "message", Assignee: "b"}, IssueWisp: types.IssueWisp{Sender: "a"}}
	if err := st.CreateIssue(ctx, root, "tester"); err != nil {
		t.Fatalf("CreateIssue(root): %v", err)
	}
	if err := st.CreateIssue(ctx, reply1, "tester"); err != nil {
		t.Fatalf("CreateIssue(reply1): %v", err)
	}
	if err := st.CreateIssue(ctx, reply2, "tester"); err != nil {
		t.Fatalf("CreateIssue(reply2): %v", err)
	}

	if err := st.AddDependency(ctx, &types.Dependency{IssueID: reply1.ID, DependsOnID: root.ID, Type: types.DepRepliesTo}, "tester"); err != nil {
		t.Fatalf("AddDependency(reply1->root): %v", err)
	}
	if err := st.AddDependency(ctx, &types.Dependency{IssueID: reply2.ID, DependsOnID: reply1.ID, Type: types.DepRepliesTo}, "tester"); err != nil {
		t.Fatalf("AddDependency(reply2->reply1): %v", err)
	}

	if got := findRepliesTo(ctx, root.ID, st); got != "" {
		t.Fatalf("expected root replies-to to be empty, got %q", got)
	}
	if got := findRepliesTo(ctx, reply2.ID, st); got != reply1.ID {
		t.Fatalf("expected reply2 parent %q, got %q", reply1.ID, got)
	}

	rootReplies := findReplies(ctx, root.ID, st)
	if len(rootReplies) != 1 || rootReplies[0].ID != reply1.ID {
		t.Fatalf("expected root replies [%s], got %+v", reply1.ID, rootReplies)
	}
	r1Replies := findReplies(ctx, reply1.ID, st)
	if len(r1Replies) != 1 || r1Replies[0].ID != reply2.ID {
		t.Fatalf("expected reply1 replies [%s], got %+v", reply2.ID, r1Replies)
	}
}
