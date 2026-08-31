package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

// Scope note:
// `bd batch` is intentionally a narrow, write-oriented command runner. It does
// NOT re-use the top-level cobra handlers (close/update/create/dep) because
// those handlers read the global `store` and perform a full connection-
// per-operation workflow. Instead, batch parses a simple line-oriented grammar
// and dispatches directly against a shared storage.Transaction so the entire
// batch executes as a single dolt transaction (one DOLT_COMMIT).
//
// The supported grammar is a documented subset that matches what a downstream
// consumer's shell-script "orders" (gate-sweep.sh, spawn-storm-detect.sh,
// cross-rig-deps.sh) actually call in loops. See the Long help below for the
// exact list. Unsupported commands error out loudly.

var batchCmd = &cobra.Command{
	Use:     "batch",
	GroupID: "maint",
	Short:   "Run multiple write operations in a single database transaction",
	Long: `Run multiple write operations in a single database transaction.

Commands are read from stdin (one per line) or from a file via -f/--file.
All operations execute inside a single dolt transaction: on any error the
whole batch is rolled back, otherwise it is committed with one DOLT_COMMIT.

This is intended for shell scripts that currently invoke 'bd' many times in
a loop, which causes severe write amplification on a dolt sql-server backed
by btrfs+compression. Batching collapses N invocations into one transaction
and one dolt commit.

Grammar (one command per line):
  close <id> [reason...]
  update <id> <key>=<value> [<key>=<value> ...]
  create <type> <priority> <title...>
  dep add <from-id> <to-id> [type]
  dep remove <from-id> <to-id>
  #comment  (blank lines and '# ...' comments are ignored)

Supported 'update' keys: status, priority, title, assignee, force
Supported dependency types: see 'bd dep add --help' (default: blocks)

'force' is not a field. An update whose status moves the issue into closed
(or a configured done status) is refused when it still has open children or
a live blocker, the same as 'bd close'; 'force=true' overrides that refusal.
Because the batch is one transaction, an unforced refusal rolls back EVERY
operation in the batch, not just the offending line. Note the asymmetry with
'close <id>', which does not apply that policy at all.

Tokens are whitespace-separated. Double-quoted strings ("like this") may
contain spaces; use \" to embed a quote and \\ for a backslash.

Examples:
  # From a pipe
  bd list --status stale -q | awk '{print "close",$1," stale"}' | bd batch

  # From a file
  bd batch -f operations.txt

  # Inline
  printf 'close bd-1 done\nupdate bd-2 status=in_progress\n' | bd batch

On success, exits 0 and prints a summary (or JSON with --json). On any error,
rolls back the entire transaction and exits non-zero with the failing line.

NOTE: This is a narrow subset. Commands like 'show', 'list', 'ready', 'sync',
complex create flows, or any flag not listed above are NOT accepted. Use
normal 'bd' subcommands for interactive/read operations.`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: false,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := CheckReadonly("batch"); err != nil {
			return err
		}

		evt := metrics.NewCommandEvent("batch")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		proxied := usesProxiedServer()
		if !proxied && getStore() == nil {
			return fmt.Errorf("no database connection available (%s)", diagHint())
		}

		filePath, _ := cmd.Flags().GetString("file")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		commitMsg, _ := cmd.Flags().GetString("message")

		var reader io.Reader
		if filePath != "" {
			f, err := os.Open(filePath) // #nosec G304 -- user-supplied batch file
			if err != nil {
				return fmt.Errorf("open batch file: %w", err)
			}
			defer f.Close()
			reader = f
		} else {
			reader = cmd.InOrStdin()
		}

		ops, err := parseBatchScript(reader)
		if err != nil {
			return fmt.Errorf("parsing batch input: %w", err)
		}

		if dryRun {
			// In dry-run mode, just echo what would run. This is helpful for
			// shell script authors verifying their scripts before running.
			for _, op := range ops {
				fmt.Fprintf(cmd.OutOrStdout(), "line %d: %s\n", op.line, op.raw)
			}
			if isJSONOutput() {
				if err := outputJSON(map[string]interface{}{
					"dry_run":    true,
					"operations": len(ops),
				}); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%d operations parsed (dry-run, nothing executed)\n", len(ops))
			}
			return nil
		}

		if len(ops) == 0 {
			// Empty input is a no-op success, matching 'bd list | bd batch' on
			// an empty list.
			if isJSONOutput() {
				if err := outputJSON(map[string]interface{}{
					"operations": 0,
					"status":     "ok",
				}); err != nil {
					return err
				}
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "batch: 0 operations (no-op)")
			}
			return nil
		}

		if strings.TrimSpace(commitMsg) == "" {
			commitMsg = fmt.Sprintf("bd: batch %d ops by %s", len(ops), getActor())
		}

		ctx := getRootContext()
		if ctx == nil {
			ctx = context.Background()
		}

		// One transaction, one commit message, whole-batch rollback on the first
		// failing line — the contract is the same on both backends; only the
		// transaction primitive differs (uow.RunTx there, transact here), and
		// both wrap a per-op dispatch that shares this file's parser.
		var results []batchOpResult
		if proxied {
			results, err = runBatchProxiedServer(ctx, ops, commitMsg)
		} else {
			results = make([]batchOpResult, 0, len(ops))
			err = transact(ctx, getStore(), commitMsg, func(tx storage.Transaction) error {
				for _, op := range ops {
					res, rerr := runBatchOp(ctx, tx, op)
					if rerr != nil {
						return fmt.Errorf("line %d (%s): %w", op.line, op.raw, rerr)
					}
					results = append(results, res)
				}
				return nil
			})
		}
		if err != nil {
			if isJSONOutput() {
				if jerr := outputJSONError(err, "batch_error"); jerr != nil {
					return errors.Join(err, jerr)
				}
			}
			return err
		}

		commandDidWrite.Store(true)

		if isJSONOutput() {
			if err := outputJSON(map[string]interface{}{
				"operations": len(results),
				"status":     "ok",
				"results":    results,
			}); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "batch: %d operations committed\n", len(results))
			for _, r := range results {
				fmt.Fprintf(cmd.OutOrStdout(), "  line %d: %s %s\n", r.Line, r.Op, r.Target)
			}
		}
		return nil
	},
}

func init() {
	batchCmd.Flags().StringP("file", "f", "", "Read commands from file instead of stdin")
	batchCmd.Flags().Bool("dry-run", false, "Parse input and echo commands without executing")
	batchCmd.Flags().StringP("message", "m", "", "DOLT_COMMIT message (default: 'bd: batch N ops by <actor>')")
	rootCmd.AddCommand(batchCmd)
}

// batchOp is one parsed command line.
type batchOp struct {
	line int      // 1-based source line number
	raw  string   // original source line (for error messages)
	cmd  string   // canonical command name, e.g. "close", "update", "dep.add"
	args []string // remaining tokens
}

// batchOpResult is emitted per executed op for JSON reporting.
type batchOpResult struct {
	Line   int    `json:"line"`
	Op     string `json:"op"`
	Target string `json:"target,omitempty"`
}

// parseBatchScript reads the whole input and tokenizes each non-empty,
// non-comment line. It rejects unknown commands immediately so a bad script
// fails before any writes.
func parseBatchScript(r io.Reader) ([]batchOp, error) {
	scanner := bufio.NewScanner(r)
	// Allow long lines (descriptions, multi-token updates).
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var ops []batchOp
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		op, include, err := parseBatchLine(lineNo, scanner.Text())
		if err != nil {
			return nil, err
		}
		if include {
			ops = append(ops, op)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return ops, nil
}

func parseBatchLine(lineNo int, raw string) (batchOp, bool, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return batchOp{}, false, nil
	}
	tokens, err := tokenizeBatchLine(trimmed)
	if err != nil {
		return batchOp{}, false, fmt.Errorf("line %d: %w", lineNo, err)
	}
	if len(tokens) == 0 {
		return batchOp{}, false, nil
	}
	command, args, err := parseBatchCommand(lineNo, tokens)
	if err != nil {
		return batchOp{}, false, err
	}
	return batchOp{line: lineNo, raw: trimmed, cmd: command, args: args}, true, nil
}

func parseBatchCommand(lineNo int, tokens []string) (string, []string, error) {
	switch tokens[0] {
	case "close", "update", "create":
		return tokens[0], tokens[1:], nil
	case "dep":
		return parseBatchDependencyCommand(lineNo, tokens)
	default:
		return "", nil, fmt.Errorf("line %d: unsupported batch command %q (supported: close, update, create, dep add, dep remove)", lineNo, tokens[0])
	}
}

func parseBatchDependencyCommand(lineNo int, tokens []string) (string, []string, error) {
	if len(tokens) < 2 {
		return "", nil, fmt.Errorf("line %d: 'dep' requires a subcommand (add|remove)", lineNo)
	}
	switch tokens[1] {
	case "add":
		return "dep.add", tokens[2:], nil
	case "remove", "rm":
		return "dep.remove", tokens[2:], nil
	default:
		return "", nil, fmt.Errorf("line %d: unknown dep subcommand %q (want add|remove)", lineNo, tokens[1])
	}
}

// tokenizeBatchLine splits a line into whitespace-separated tokens with
// support for double-quoted strings. Escape sequences inside quotes: \" and
// \\. Anything else after a backslash inside quotes is treated literally.
func tokenizeBatchLine(s string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inQuote := false
	hasToken := false
	length := len(s)
	for i := 0; i < length; i++ {
		c := s[i]
		if inQuote {
			var consumed bool
			i, consumed = consumeQuotedBatchChar(s, i, &cur)
			if consumed {
				continue
			}
			inQuote = false
			continue
		}
		if c == '"' {
			inQuote = true
			hasToken = true
			continue
		}
		if c == ' ' || c == '\t' {
			tokens = appendBatchToken(tokens, &cur, hasToken)
			hasToken = false
			continue
		}
		hasToken = true
		cur.WriteByte(c)
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quoted string")
	}
	if hasToken {
		tokens = append(tokens, cur.String())
	}
	return tokens, nil
}

func consumeQuotedBatchChar(s string, i int, cur *strings.Builder) (int, bool) {
	c := s[i]
	if c == '\\' && i+1 < len(s) {
		next := s[i+1]
		if next == '"' || next == '\\' {
			cur.WriteByte(next)
			return i + 1, true
		}
		cur.WriteByte(c)
		return i, true
	}
	if c == '"' {
		return i, false
	}
	cur.WriteByte(c)
	return i, true
}

func appendBatchToken(tokens []string, cur *strings.Builder, hasToken bool) []string {
	if !hasToken {
		return tokens
	}
	tokens = append(tokens, cur.String())
	cur.Reset()
	return tokens
}

// runBatchOp dispatches a single parsed op against the shared transaction.
// It intentionally does NOT call any of the non-tx cobra handlers; it talks
// straight to storage.Transaction so everything joins the same SQL tx.
func runBatchOp(ctx context.Context, tx storage.Transaction, op batchOp) (batchOpResult, error) {
	actorName := getActor()
	result := batchOpResult{Line: op.line, Op: op.cmd}
	switch op.cmd {
	case "close":
		return runBatchClose(ctx, tx, actorName, result, op.args)
	case "update":
		return runBatchUpdate(ctx, tx, actorName, result, op.args)
	case "create":
		return runBatchCreate(ctx, tx, actorName, result, op.args)
	case "dep.add":
		return runBatchDependencyAdd(ctx, tx, actorName, result, op.args)
	case "dep.remove":
		return runBatchDependencyRemove(ctx, tx, actorName, result, op.args)
	}
	return result, fmt.Errorf("internal: unhandled batch op %q", op.cmd)
}

func runBatchClose(ctx context.Context, tx storage.Transaction, actorName string, result batchOpResult, args []string) (batchOpResult, error) {
	if len(args) < 1 {
		return result, fmt.Errorf("close requires <id>")
	}
	id := args[0]
	reason := "Closed"
	if len(args) > 1 {
		reason = strings.Join(args[1:], " ")
	}
	if err := tx.CloseIssue(ctx, id, reason, actorName, ""); err != nil {
		return result, err
	}
	result.Target = id
	return result, nil
}

func runBatchUpdate(ctx context.Context, tx storage.Transaction, actorName string, result batchOpResult, args []string) (batchOpResult, error) {
	if len(args) < 2 {
		return result, fmt.Errorf("update requires <id> and at least one key=value")
	}
	id := args[0]
	updates, err := parseUpdateKVs(args[1:])
	if err != nil {
		return result, err
	}
	if err := tx.UpdateIssue(ctx, id, updates, actorName); err != nil {
		return result, err
	}
	result.Target = id
	return result, nil
}

func runBatchCreate(ctx context.Context, tx storage.Transaction, actorName string, result batchOpResult, args []string) (batchOpResult, error) {
	if len(args) < 3 {
		return result, fmt.Errorf("create requires <type> <priority> <title>")
	}
	issueType := types.IssueType(args[0])
	// Accept common custom types too; fall back to type validation by the
	// storage layer which knows about configured custom types. We only
	// reject obviously-empty input here.
	if strings.TrimSpace(args[0]) == "" {
		return result, fmt.Errorf("create: type cannot be empty")
	}
	priority, err := strconv.Atoi(args[1])
	if err != nil {
		return result, fmt.Errorf("create: invalid priority %q: %w", args[1], err)
	}
	title := strings.Join(args[2:], " ")
	if strings.TrimSpace(title) == "" {
		return result, fmt.Errorf("create: title cannot be empty")
	}
	issue := &types.Issue{
		IssueContent: types.IssueContent{
			Title: title,
		},
		IssueWorkflow: types.IssueWorkflow{
			IssueType: issueType,
			Status:    types.StatusOpen,
			Priority:  priority,
		},
	}
	if err := tx.CreateIssue(ctx, issue, actorName); err != nil {
		return result, err
	}
	result.Target = issue.ID
	return result, nil
}

func runBatchDependencyAdd(ctx context.Context, tx storage.Transaction, actorName string, result batchOpResult, args []string) (batchOpResult, error) {
	if len(args) < 2 {
		return result, fmt.Errorf("dep add requires <from-id> <to-id>")
	}
	from, to := args[0], args[1]
	depType := "blocks"
	if len(args) >= 3 {
		depType = args[2]
	}
	dt := types.DependencyType(depType)
	if !dt.IsValid() {
		return result, fmt.Errorf("dep add: invalid dependency type %q", depType)
	}
	dep := &types.Dependency{IssueID: from, DependsOnID: to, Type: dt}
	if err := tx.AddDependency(ctx, dep, actorName); err != nil {
		return result, err
	}
	result.Target = fmt.Sprintf("%s->%s", from, to)
	return result, nil
}

func runBatchDependencyRemove(ctx context.Context, tx storage.Transaction, actorName string, result batchOpResult, args []string) (batchOpResult, error) {
	if len(args) < 2 {
		return result, fmt.Errorf("dep remove requires <from-id> <to-id>")
	}
	from, to := args[0], args[1]
	if err := tx.RemoveDependency(ctx, from, to, actorName); err != nil {
		return result, err
	}
	result.Target = fmt.Sprintf("%s->%s", from, to)
	return result, nil
}

// parseUpdateKVs walks a slice of "key=value" tokens and builds the updates
// map accepted by storage.Transaction.UpdateIssue. Only a small, documented
// subset of fields is allowed — anything else is a hard error so typos in
// scripts never silently drop updates.
func parseUpdateKVs(kvs []string) (map[string]interface{}, error) {
	updates := make(map[string]interface{}, len(kvs))
	for _, kv := range kvs {
		key, value, err := splitUpdateKV(kv)
		if err != nil {
			return nil, err
		}
		update, err := parseUpdateKV(key, value)
		if err != nil {
			return nil, err
		}
		updates[key] = update
		if key == "force" {
			delete(updates, key)
			updates[issueops.OpForceClosePolicy] = update
		}
	}
	return updates, nil
}

func splitUpdateKV(kv string) (string, string, error) {
	eq := strings.IndexByte(kv, '=')
	if eq <= 0 {
		return "", "", fmt.Errorf("update: expected key=value, got %q", kv)
	}
	return strings.TrimSpace(kv[:eq]), kv[eq+1:], nil
}

func parseUpdateKV(key, value string) (interface{}, error) {
	switch key {
	case "status":
		return parseBatchStatus(value)
	case "priority":
		return parseBatchPriority(value)
	case "title":
		return parseBatchTitle(value)
	case "assignee":
		return value, nil
	case "force":
		return parseBatchForce(value)
	default:
		return nil, fmt.Errorf("update: unsupported key %q (allowed: status, priority, title, assignee, force)", key)
	}
}

func parseBatchStatus(value string) (interface{}, error) {
	if !types.Status(value).IsValid() && strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("update: status cannot be empty")
	}
	return value, nil
}

func parseBatchPriority(value string) (interface{}, error) {
	p, err := strconv.Atoi(value)
	if err != nil {
		return nil, fmt.Errorf("update: invalid priority %q: %w", value, err)
	}
	return p, nil
}

func parseBatchTitle(value string) (interface{}, error) {
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("update: title cannot be empty")
	}
	return value, nil
}

func parseBatchForce(value string) (interface{}, error) {
	force, err := strconv.ParseBool(value)
	if err != nil {
		return nil, fmt.Errorf("update: invalid force %q: %w", value, err)
	}
	return force, nil
}
