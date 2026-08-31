package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jonbaldie/beads/internal/beads"
)

func runImportInner(args []string) error {
	return runImportInnerWithOptions(importOptions{}, args)
}

func runImportInnerWithOptions(opts importOptions, args []string) error {
	ctx := getRootContext()
	source, err := resolveImportSource(opts, args)
	if err != nil {
		return err
	}
	if source.fromStdin {
		return runImportFromReader(ctx, os.Stdin, "stdin", opts)
	}

	return importFromFile(ctx, source.path, opts)
}

type importSource struct {
	path      string
	fromStdin bool
}

func resolveImportSource(opts importOptions, args []string) (importSource, error) {
	if opts.input != "" && len(args) > 0 {
		return importSource{}, fmt.Errorf("use either --input or a positional file, not both")
	}
	if opts.input == "-" || (len(args) > 0 && args[0] == "-") {
		return importSource{fromStdin: true}, nil
	}
	path, err := resolveImportFilePath(opts, args)
	if err != nil {
		return importSource{}, err
	}
	return importSource{path: path}, nil
}

func resolveImportFilePath(opts importOptions, args []string) (string, error) {
	if opts.input != "" {
		return opts.input, nil
	}
	if len(args) > 0 {
		return args[0], nil
	}
	return defaultImportFilePath()
}

func defaultImportFilePath() (string, error) {
	// bd-axluy: `bd import < file` (or `... | bd import`) without "-"
	// used to silently ignore stdin and import the default JSONL — a
	// mutating command diverging from what the user piped. Demand an
	// explicit source instead. /dev/null (the stdin subprocesses get by
	// default) is a character device, so scripted bare `bd import` with
	// no redirection still works.
	if fi, statErr := os.Stdin.Stat(); statErr == nil && fi.Mode()&os.ModeCharDevice == 0 {
		return "", fmt.Errorf("stdin is redirected, but without \"-\" bd import ignores it and imports the default JSONL instead; use 'bd import -' to import what you piped, or name a file explicitly")
	}
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return "", fmt.Errorf("%s — %s", activeWorkspaceNotFoundError(), diagHint())
	}
	if isGlobalFlag() {
		return filepath.Join(beadsDir, "global-issues.jsonl"), nil
	}
	return configuredImportJSONLPath(beadsDir), nil
}

func importFromFile(ctx context.Context, jsonlPath string, opts importOptions) error {
	info, err := os.Stat(jsonlPath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", jsonlPath, err)
	}
	if info.Size() == 0 {
		if isJSONOutput() {
			return outputJSON(importResultJSON{Source: jsonlPath})
		}
		fmt.Fprintf(os.Stderr, "Empty file: %s\n", jsonlPath)
		return nil
	}

	f, err := os.Open(jsonlPath) //nolint:gosec // G304: CLI argument
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", jsonlPath, err)
	}
	defer f.Close()

	return runImportFromReader(ctx, f, jsonlPath, opts)
}

type importResultJSON struct {
	Source              string         `json:"source"`
	Created             int            `json:"created"`
	Updated             int            `json:"updated,omitempty"`
	Unchanged           int            `json:"unchanged,omitempty"`
	Skipped             int            `json:"skipped"`
	DedupHits           int            `json:"dedup_skipped,omitempty"`
	Memories            int            `json:"memories,omitempty"`
	IDs                 []string       `json:"ids,omitempty"`
	UpdatedIssues       []ImportChange `json:"updated_issues,omitempty"`
	TieKeptLocalIDs     []string       `json:"tie_kept_local_ids,omitempty"`
	StaleSkippedIDs     []string       `json:"stale_skipped_ids,omitempty"`
	SkippedDependencies []string       `json:"skipped_dependencies,omitempty"`
	DryRun              bool           `json:"dry_run,omitempty"`
}

func runImportFromReader(ctx context.Context, r io.Reader, source string, opts importOptions) error {
	issues, memories, err := parseImportRecords(r)
	if err != nil {
		return err
	}

	if usesProxiedServer() {
		return runImportRecordsProxied(ctx, issues, memories, source, opts)
	}

	if getStore() == nil {
		return fmt.Errorf("no database — run 'bd init' or 'bd bootstrap' first")
	}
	return runImportRecordsClassic(ctx, issues, memories, source, opts)
}
