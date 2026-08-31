package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/jonbaldie/beads/internal/audit"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/spf13/cobra"
)

type auditRecordFlags struct {
	kind     string
	model    string
	prompt   string
	response string
	issueID  string
	toolName string
	exitCode int
	err      string
	stdin    bool
}

type auditLabelFlags struct {
	value  string
	reason string
}

func readAuditRecordFlags(cmd *cobra.Command) auditRecordFlags {
	flags := cmd.Flags()
	kind, _ := flags.GetString("kind")
	model, _ := flags.GetString("model")
	prompt, _ := flags.GetString("prompt")
	response, _ := flags.GetString("response")
	issueID, _ := flags.GetString("issue-id")
	toolName, _ := flags.GetString("tool-name")
	exitCode, _ := flags.GetInt("exit-code")
	errValue, _ := flags.GetString("error")
	stdin, _ := flags.GetBool("stdin")
	return auditRecordFlags{
		kind: kind, model: model, prompt: prompt, response: response,
		issueID: issueID, toolName: toolName, exitCode: exitCode,
		err: errValue, stdin: stdin,
	}
}

func readAuditLabelFlags(cmd *cobra.Command) auditLabelFlags {
	value, _ := cmd.Flags().GetString("label")
	reason, _ := cmd.Flags().GetString("reason")
	return auditLabelFlags{value: value, reason: reason}
}

func newAuditCmd() *cobra.Command {
	auditCmd := &cobra.Command{
		Use:   "audit",
		Short: "Record and label agent interactions (append-only JSONL)",
		Long: `Record explicit agent/tool interaction audit entries in .beads/interactions.jsonl.

This optional JSONL sidecar is disabled by default. Enable it with:

  bd config set audit.enabled true

Issue history is always recorded in the database and is visible with
bd history <id> --events. The JSONL sidecar is for explicit interaction capture:
- auditing ("why did the agent do that?")
- dataset generation (SFT/RL fine-tuning)

Entries are append-only. Labeling creates a new "label" entry that references a parent entry.`,
	}
	auditCmd.ValidArgsFunction = issueIDCompletion
	auditCmd.AddCommand(newAuditRecordCmd())
	auditCmd.AddCommand(newAuditLabelCmd())
	return auditCmd
}

func newAuditRecordCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "record",
		Short:         "Append an audit interaction entry",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runAuditRecord,
	}
	cmd.Flags().String("kind", "", "Entry kind (e.g. llm_call, tool_call, label)")
	cmd.Flags().String("model", "", "Model name (llm_call)")
	cmd.Flags().String("prompt", "", "Prompt text (llm_call)")
	cmd.Flags().String("response", "", "Response text (llm_call)")
	cmd.Flags().String("issue-id", "", "Related issue id (bd-...)")
	cmd.Flags().String("tool-name", "", "Tool name (tool_call)")
	cmd.Flags().Int("exit-code", -1, "Exit code (tool_call)")
	cmd.Flags().String("error", "", "Error string (llm_call/tool_call)")
	cmd.Flags().Bool("stdin", false, "Read a JSON object from stdin (must match audit.Entry schema)")
	return cmd
}

func newAuditLabelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "label <entry-id>",
		Short:         "Append a label entry referencing an existing interaction",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runAuditLabel,
	}
	cmd.Flags().String("label", "", `Label value (e.g. "good" or "bad")`)
	cmd.Flags().String("reason", "", "Reason for label")
	return cmd
}

func runAuditRecord(cmd *cobra.Command, _ []string) error {
	evt := metrics.NewCommandEvent("audit-record")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	e, err := auditRecordEntry(cmd)
	if err != nil {
		return err
	}
	id, err := audit.AppendIfEnabled(e)
	if err != nil {
		return HandleError("%v", err)
	}
	if isJSONOutput() {
		return outputJSON(map[string]any{"id": id, "kind": e.Kind})
	}
	fmt.Println(id)
	return nil
}

func auditRecordEntry(cmd *cobra.Command) (*audit.Entry, error) {
	flags := readAuditRecordFlags(cmd)
	fi, _ := os.Stdin.Stat()
	stdinPiped := fi != nil && (fi.Mode()&os.ModeCharDevice) == 0
	if flags.stdin || (stdinPiped && auditRecordFlagsEmpty(flags)) {
		return readAuditEntry()
	}
	if flags.kind == "" {
		return nil, HandleError("--kind is required")
	}
	e := &audit.Entry{
		Kind: flags.kind, Actor: getActor(), IssueID: flags.issueID, Model: flags.model,
		Prompt: flags.prompt, Response: flags.response, ToolName: flags.toolName, Error: flags.err,
	}
	if flags.exitCode >= 0 {
		exit := flags.exitCode
		e.ExitCode = &exit
	}
	return e, nil
}

func auditRecordFlagsEmpty(flags auditRecordFlags) bool {
	return flags.kind == "" && flags.model == "" && flags.prompt == "" &&
		flags.response == "" && flags.issueID == "" && flags.toolName == "" &&
		flags.exitCode < 0 && flags.err == ""
}

func readAuditEntry() (*audit.Entry, error) {
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, HandleError("failed to read stdin: %v", err)
	}
	e := &audit.Entry{}
	if err := json.Unmarshal(b, e); err != nil {
		return nil, HandleError("invalid JSON on stdin: %v", err)
	}
	if getActor() != "" {
		e.Actor = getActor()
	}
	return e, nil
}

func runAuditLabel(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("audit-label")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	flags := readAuditLabelFlags(cmd)
	if flags.value == "" {
		return HandleError("--label is required")
	}
	e := &audit.Entry{Kind: "label", Actor: getActor(), ParentID: args[0], Label: flags.value, Reason: flags.reason}
	id, err := audit.AppendIfEnabled(e)
	if err != nil {
		return HandleError("%v", err)
	}
	if isJSONOutput() {
		return outputJSON(map[string]any{"id": id, "parent_id": args[0], "label": flags.value})
	}
	fmt.Println(id)
	return nil
}

func init() {
	rootCmd.AddCommand(newAuditCmd())
}
