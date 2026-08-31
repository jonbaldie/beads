// Package main implements the bd CLI provenance event-log commands.
package main

import (
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

var provenanceCmd = &cobra.Command{
	Use:           "provenance",
	GroupID:       "issues",
	Short:         "Append-only provenance event log",
	SilenceUsage:  true,
	SilenceErrors: true,
	Long: `Record and read provenance events: typed bindings from an issue to an
opaque external artifact (a git SHA, PR, work-id, transcript, or branch).

The log is append-only — there is no update or delete. bd never interprets the
actor or ref; only kind and ref-kind are structurally validated. Recording is
idempotent on a deterministic id, so a producer firing twice is harmless.`,
}

type provenanceRecordOptions struct {
	issue   string
	kind    string
	source  string
	actor   string
	ref     string
	refKind string
	at      string
	payload string
}

func provenanceRecordOptionsFromCommand(cmd *cobra.Command) provenanceRecordOptions {
	if cmd == nil {
		return provenanceRecordOptions{}
	}
	flags := cmd.Flags()
	issue, _ := flags.GetString("issue")
	kind, _ := flags.GetString("kind")
	source, _ := flags.GetString("source")
	actor, _ := flags.GetString("actor")
	ref, _ := flags.GetString("ref")
	refKind, _ := flags.GetString("ref-kind")
	at, _ := flags.GetString("at")
	payload, _ := flags.GetString("payload")
	return provenanceRecordOptions{
		issue:   issue,
		kind:    kind,
		source:  source,
		actor:   actor,
		ref:     ref,
		refKind: refKind,
		at:      at,
		payload: payload,
	}
}

type provenanceLogOptions struct {
	kind string
}

func provenanceLogOptionsFromCommand(cmd *cobra.Command) provenanceLogOptions {
	if cmd == nil {
		return provenanceLogOptions{}
	}
	kind, _ := cmd.Flags().GetString("kind")
	return provenanceLogOptions{kind: kind}
}

var provenanceRecordCmd = &cobra.Command{
	Use:           "record --issue <id> --kind <k> --source <s>",
	Short:         "Record a provenance event (idempotent)",
	SilenceUsage:  true,
	SilenceErrors: true,
	Long: `Record a provenance event. The event is appended idempotently: a
deterministic id is computed from source:issue:kind:(ref or --at), so re-running
the same record is a no-op.

An event recorded without --ref requires --at so the id is caller-owned.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := provenanceRecordOptionsFromCommand(cmd)
		if err := CheckReadonly("provenance record"); err != nil {
			return err
		}
		ctx := getRootContext()

		issueID, err := utils.ResolvePartialID(ctx, getStore(), opts.issue)
		if err != nil {
			return HandleErrorRespectJSON("resolving %s: %v", opts.issue, err)
		}

		ev := types.ProvenanceEvent{
			IssueID: issueID,
			Kind:    types.ProvKind(opts.kind),
			Source:  opts.source,
		}
		if opts.actor != "" {
			ev.Actor = &opts.actor
		}
		if opts.ref != "" {
			ev.Ref = &opts.ref
		}
		if opts.refKind != "" {
			ev.RefKind = &opts.refKind
		}
		if opts.payload != "" {
			ev.Payload = &opts.payload
		}
		if opts.at != "" {
			at, err := time.Parse(time.RFC3339, opts.at)
			if err != nil {
				return HandleErrorRespectJSON("--at must be an RFC3339 timestamp (e.g. 2026-06-19T12:00:00Z): %v", err)
			}
			atUTC := at.UTC()
			ev.OccurredAt = &atUTC
		}

		// Fail early with the same structural rules the store enforces, so the CLI
		// gives friendly feedback before opening a transaction.
		if err := issueops.ValidateProvenanceEvent(ev); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}

		id, inserted, err := getStore().RecordProvenanceEvent(ctx, ev)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		if inserted {
			commandDidWrite.Store(true)
		}

		if isJSONOutput() {
			return outputJSON(map[string]interface{}{
				"id":       id,
				"inserted": inserted,
				"issue_id": issueID,
				"kind":     opts.kind,
			})
		}
		if inserted {
			fmt.Printf("%s Recorded %s provenance %s on %s\n", ui.RenderPass("✓"), opts.kind, id, issueID)
		} else {
			fmt.Printf("%s Provenance %s already recorded (id %s)\n", ui.RenderAccent("•"), opts.kind, id)
		}
		return nil
	},
}

var provenanceLogCmd = &cobra.Command{
	Use:               "log <issue-id>",
	Short:             "List provenance events for an issue",
	Args:              cobra.ExactArgs(1),
	SilenceUsage:      true,
	SilenceErrors:     true,
	ValidArgsFunction: issueIDCompletion,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := provenanceLogOptionsFromCommand(cmd)
		ctx := getRootContext()
		issueID, err := utils.ResolvePartialID(ctx, getStore(), args[0])
		if err != nil {
			return HandleErrorRespectJSON("resolving %s: %v", args[0], err)
		}
		events, err := getStore().GetProvenanceEvents(ctx, issueID, opts.kind)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		return outputProvenanceEvents(events)
	},
}

var provenanceByRefCmd = &cobra.Command{
	Use:           "by-ref <ref>",
	Short:         "List provenance events bound to a ref",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := getRootContext()
		events, err := getStore().GetProvenanceByRef(ctx, args[0])
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		return outputProvenanceEvents(events)
	},
}

func outputProvenanceEvents(events []types.ProvenanceEvent) error {
	if isJSONOutput() {
		if events == nil {
			events = []types.ProvenanceEvent{}
		}
		return outputJSON(events)
	}
	if len(events) == 0 {
		fmt.Println("No provenance events")
		return nil
	}
	for _, ev := range events {
		when := "—"
		if ev.OccurredAt != nil {
			when = ev.OccurredAt.Format(time.RFC3339)
		}
		line := fmt.Sprintf("%s  %-8s  %s", when, ev.Kind, ev.IssueID)
		if ev.Ref != nil {
			refKind := ""
			if ev.RefKind != nil {
				refKind = *ev.RefKind + ":"
			}
			line += fmt.Sprintf("  %s%s", refKind, *ev.Ref)
		}
		if ev.Actor != nil {
			line += fmt.Sprintf("  by %s", *ev.Actor)
		}
		line += fmt.Sprintf("  (%s)", ev.Source)
		fmt.Println(line)
	}
	return nil
}

func init() {
	provenanceRecordCmd.Flags().String("issue", "", "issue id (required)")
	provenanceRecordCmd.Flags().String("kind", "", "event kind: cut|claim|suspend|resume|handoff|commit|land|used (required)")
	provenanceRecordCmd.Flags().String("source", "", "producer of the event, e.g. git-hook, orchestrator (required)")
	provenanceRecordCmd.Flags().String("actor", "", "opaque actor identifier (optional)")
	provenanceRecordCmd.Flags().String("ref", "", "opaque external reference, e.g. a SHA or PR url (optional)")
	provenanceRecordCmd.Flags().String("ref-kind", "", "ref kind: git-sha|pr|work-id|transcript|branch (optional)")
	provenanceRecordCmd.Flags().String("at", "", "event-time as RFC3339 (required for ref-less kinds)")
	provenanceRecordCmd.Flags().String("payload", "", "opaque payload, e.g. JSON (optional)")
	_ = provenanceRecordCmd.MarkFlagRequired("issue")
	_ = provenanceRecordCmd.MarkFlagRequired("kind")
	_ = provenanceRecordCmd.MarkFlagRequired("source")

	provenanceLogCmd.Flags().String("kind", "", "filter by kind (optional)")

	provenanceCmd.AddCommand(provenanceRecordCmd)
	provenanceCmd.AddCommand(provenanceLogCmd)
	provenanceCmd.AddCommand(provenanceByRefCmd)
	rootCmd.AddCommand(provenanceCmd)
}
