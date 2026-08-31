package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
)

type helpOptions struct {
	all      bool
	doc      string
	list     bool
	docsRoot string
}

func helpOptionsFromCommand(cmd *cobra.Command) helpOptions {
	all, _ := cmd.Flags().GetBool("all")
	doc, _ := cmd.Flags().GetString("doc")
	list, _ := cmd.Flags().GetBool("list")
	docsRoot, _ := cmd.Flags().GetString("docs-root")
	return helpOptions{all: all, doc: doc, list: list, docsRoot: docsRoot}
}

// registerHelpAllFlag adds the --all, --doc, and --list flags to Cobra's auto-generated help command.
// Must be called after rootCmd.InitDefaultHelpCmd() has run (i.e., after first Execute
// or explicit init). We hook it in init() after all subcommands are registered.
func registerHelpAllFlag() {
	// Cobra lazily creates the help command. We need to find it.
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() != "help" {
			continue
		}
		configureHelpCommand(cmd)
		return
	}
}

func configureHelpCommand(cmd *cobra.Command) {
	if cmd.Flags().Lookup("all") != nil {
		return
	}
	cmd.Flags().Bool("all", false, "Show help for all commands in a single document")
	cmd.Flags().String("doc", "", "Generate markdown docs for a single command")
	cmd.Flags().Bool("list", false, "List all available commands")
	cmd.Flags().String("docs-root", "", "Generate repository CLI docs under this root")

	// Wrap the existing Run to check --all, --doc, and --list first.
	originalRun := cmd.Run
	cmd.Run = nil
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return executeHelpCommand(cmd, args, originalRun)
	}
}

func executeHelpCommand(cmd *cobra.Command, args []string, originalRun func(*cobra.Command, []string)) error {
	opts := helpOptionsFromCommand(cmd)
	if opts.docsRoot != "" {
		return generateHelpDocs(opts.docsRoot)
	}
	if opts.list {
		listAllCommands(os.Stdout, rootCmd)
		return nil
	}
	if opts.doc != "" {
		return generateSingleHelpDoc(opts.doc, args)
	}
	if opts.all {
		writeAllHelp(os.Stdout, rootCmd)
		return nil
	}
	if originalRun != nil {
		originalRun(cmd, args)
	}
	return nil
}

func generateHelpDocs(repoRoot string) error {
	if err := writeGeneratedCLIDocs(rootCmd, repoRoot); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return SilentExit()
	}
	return nil
}

func generateSingleHelpDoc(doc string, args []string) error {
	cmdPath := doc
	if len(args) > 0 {
		cmdPath = strings.Join(append([]string{doc}, args...), " ")
	}
	if err := writeSingleCommandDoc(os.Stdout, rootCmd, cmdPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintf(os.Stderr, "Available commands: %s\n", strings.Join(availableCommandNames(rootCmd), " "))
		return SilentExit()
	}
	return nil
}

// writeAllHelp writes a complete markdown reference for all commands,
// generated from the live Cobra command tree.
func writeAllHelp(w io.Writer, root *cobra.Command) {
	fmt.Fprintf(w, "# bd — Complete Command Reference\n\n")
	fmt.Fprintf(w, "Reference for bd Latest. Generated from `bd help --all`.\n\n")
	groups := collectHelpGroups(root)
	writeHelpTableOfContents(w, groups)
	writeHelpGlobalFlags(w, root)
	writeHelpCommandDetails(w, groups)
}

type helpGroup struct {
	title    string
	commands []*cobra.Command
}

type helpGroupCollection struct {
	groups    []*helpGroup
	byID      map[string]*helpGroup
	ungrouped *helpGroup
}

func collectHelpGroups(root *cobra.Command) []*helpGroup {
	collection := helpGroupCollection{byID: make(map[string]*helpGroup, len(root.Groups()))}
	for _, group := range root.Groups() {
		collection.addGroup(group.ID, group.Title)
	}
	for _, cmd := range root.Commands() {
		collection.addCommand(cmd)
	}
	return collection.allGroups()
}

func (c *helpGroupCollection) addGroup(id, title string) {
	group := &helpGroup{title: title}
	c.byID[id] = group
	c.groups = append(c.groups, group)
}

func (c *helpGroupCollection) addCommand(cmd *cobra.Command) {
	if !cmd.IsAvailableCommand() && cmd.Name() != "help" {
		return
	}
	if cmd.GroupID != "" {
		if group, ok := c.byID[cmd.GroupID]; ok {
			group.commands = append(group.commands, cmd)
		}
		return
	}
	if c.ungrouped == nil {
		c.ungrouped = &helpGroup{title: "Other Commands:"}
	}
	c.ungrouped.commands = append(c.ungrouped.commands, cmd)
}

func (c *helpGroupCollection) allGroups() []*helpGroup {
	groups := append([]*helpGroup(nil), c.groups...)
	if c.ungrouped != nil {
		groups = append(groups, c.ungrouped)
	}
	return groups
}

func writeHelpTableOfContents(w io.Writer, groups []*helpGroup) {
	fmt.Fprintf(w, "## Table of Contents\n\n")
	for _, group := range groups {
		if len(group.commands) == 0 {
			continue
		}
		fmt.Fprintf(w, "### %s\n\n", group.title)
		for _, cmd := range group.commands {
			writeHelpTOCCommand(w, cmd)
		}
		fmt.Fprintf(w, "\n")
	}
}

func writeHelpTOCCommand(w io.Writer, cmd *cobra.Command) {
	anchor := "bd-" + strings.ReplaceAll(cmd.Name(), "-", "-")
	fmt.Fprintf(w, "- [bd %s](#%s) — %s\n", cmd.Name(), anchor, escapeMDXText(cmd.Short))
	for _, sub := range cmd.Commands() {
		if !sub.IsAvailableCommand() {
			continue
		}
		subAnchor := "bd-" + cmd.Name() + "-" + strings.ReplaceAll(sub.Name(), "-", "-")
		fmt.Fprintf(w, "  - [bd %s %s](#%s) — %s\n", cmd.Name(), sub.Name(), subAnchor, escapeMDXText(sub.Short))
	}
}

func writeHelpGlobalFlags(w io.Writer, root *cobra.Command) {
	fmt.Fprintf(w, "---\n\n## Global Flags\n\n")
	fmt.Fprintf(w, "These flags apply to all commands:\n\n")
	fmt.Fprintf(w, "```\n")
	fmt.Fprintf(w, "%s", root.PersistentFlags().FlagUsages())
	fmt.Fprintf(w, "```\n\n")
}

func writeHelpCommandDetails(w io.Writer, groups []*helpGroup) {
	fmt.Fprintf(w, "---\n\n")
	for _, group := range groups {
		if len(group.commands) == 0 {
			continue
		}
		fmt.Fprintf(w, "## %s\n\n", group.title)
		for _, cmd := range group.commands {
			writeCommandHelp(w, cmd, "bd", 3)
		}
	}
}

// writeCommandHelp writes markdown help for a single command and its subcommands.
func writeCommandHelp(w io.Writer, cmd *cobra.Command, parentPath string, depth int) {
	fullPath := parentPath + " " + cmd.Name()
	heading := strings.Repeat("#", depth)

	fmt.Fprintf(w, "%s %s\n\n", heading, fullPath)

	writeCommandBody(w, cmd)

	// Subcommands
	subCmds := cmd.Commands()
	hasVisibleSubs := false
	for _, sub := range subCmds {
		if sub.IsAvailableCommand() {
			hasVisibleSubs = true
			break
		}
	}

	if hasVisibleSubs {
		for _, sub := range subCmds {
			if !sub.IsAvailableCommand() {
				continue
			}
			writeCommandHelp(w, sub, fullPath, depth+1)
		}
	}
}

// writeCommandBody writes the heading-independent parts of a command's help:
// description, usage, aliases, examples, and local flags.
func writeCommandBody(w io.Writer, cmd *cobra.Command) {
	writeCommandDescription(w, cmd)
	writeCommandUsage(w, cmd)
	writeCommandAliases(w, cmd)
	writeCommandExamples(w, cmd)
	writeCommandFlags(w, cmd)
	writeCommandSupplement(w, cmd)
}

func writeCommandDescription(w io.Writer, cmd *cobra.Command) {
	if cmd.Long != "" {
		fmt.Fprintf(w, "%s\n\n", escapeMDXText(cmd.Long))
	} else if cmd.Short != "" {
		fmt.Fprintf(w, "%s\n\n", escapeMDXText(cmd.Short))
	}
}

func writeCommandUsage(w io.Writer, cmd *cobra.Command) {
	// Usage — mirror the binary's --help output: a runnable command shows its
	// UseLine; a command with subcommands also shows `<path> [command]`.
	// UseLine() alone appends "[flags]" even on non-runnable parents, which
	// the binary's help never prints.
	var usage []string
	if cmd.Runnable() {
		usage = append(usage, strings.TrimRight(cmd.UseLine(), " "))
	}
	if cmd.HasAvailableSubCommands() {
		usage = append(usage, cmd.CommandPath()+" [command]")
	}
	if len(usage) == 0 {
		usage = append(usage, strings.TrimRight(cmd.UseLine(), " "))
	}
	fmt.Fprintf(w, "```\n%s\n```\n\n", strings.Join(usage, "\n"))
}

func writeCommandAliases(w io.Writer, cmd *cobra.Command) {
	if len(cmd.Aliases) > 0 {
		fmt.Fprintf(w, "**Aliases:** %s\n\n", strings.Join(cmd.Aliases, ", "))
	}
}

func writeCommandExamples(w io.Writer, cmd *cobra.Command) {
	if cmd.Example != "" {
		fmt.Fprintf(w, "**Examples:**\n\n```bash\n%s\n```\n\n", cmd.Example)
	}
}

func writeCommandFlags(w io.Writer, cmd *cobra.Command) {
	localFlags := cmd.NonInheritedFlags()
	if localFlags.HasFlags() {
		fmt.Fprintf(w, "**Flags:**\n\n```\n%s```\n\n", localFlags.FlagUsages())
	}
}

func writeCommandSupplement(w io.Writer, cmd *cobra.Command) {
	// Embedded doc supplement (raw Markdown, no MDX escaping).
	if sup, ok := commandDocSupplements[cmd.CommandPath()]; ok {
		fmt.Fprintf(w, "%s\n\n", strings.TrimSpace(sup))
	}
}

// writeSingleCommandDoc generates one command's documentation page as generic
// Markdown: title/description frontmatter and portable CommonMark. bd never
// emits site-generator-specific output (Docusaurus ids/slugs, Mintlify JSX,
// navigation fragments) — repo post-processors adapt these pages to whatever
// the documentation site needs.
func writeSingleCommandDoc(w io.Writer, root *cobra.Command, cmdName string) error {
	// Find the command (handle nested commands like "mol pour")
	cmd := findCommand(root, cmdName)
	if cmd == nil {
		return fmt.Errorf("Error: command not found: %s", cmdName)
	}

	docCommand := strings.TrimSpace(strings.TrimPrefix(commandPath(cmd), root.Name()))
	if docCommand == "" {
		return errors.New("Error: cannot generate docs for root command")
	}

	fmt.Fprintf(w, "---\n")
	fmt.Fprintf(w, "title: %q\n", "bd "+docCommand)
	if cmd.Short != "" {
		fmt.Fprintf(w, "description: %q\n", cmd.Short)
	}
	fmt.Fprintf(w, "---\n\n")
	fmt.Fprintf(w, "<!-- AUTO-GENERATED: do not edit manually -->\n\n")
	fmt.Fprintf(w, "Generated from `bd help --doc %s`.\n\n", docCommand)

	// The frontmatter title serves as the page heading, so the top command
	// emits its body without a duplicate heading; subcommands start at ##.
	writeCommandBody(w, cmd)

	fullPath := commandPath(cmd)
	for _, sub := range cmd.Commands() {
		if !sub.IsAvailableCommand() {
			continue
		}
		writeCommandHelp(w, sub, fullPath, 2)
	}
	return nil
}

// writeGeneratedCLIDocs writes the repository's generated CLI documentation:
// the single-file reference at docs/CLI_REFERENCE.md and a generic per-command
// staging tree at build/cli-docs/ for post-processors to consume
// (scripts/generate-cli-docs.sh runs them). The staging tree is not committed.
func writeGeneratedCLIDocs(root *cobra.Command, repoRoot string) error {
	repoRoot = filepath.Clean(repoRoot)

	var all bytes.Buffer
	writeAllHelp(&all, root)
	if err := writeMarkdownFile(filepath.Join(repoRoot, "docs", "CLI_REFERENCE.md"), all.String()); err != nil {
		return err
	}

	return writeGenericCLIDocsDir(filepath.Join(repoRoot, "build", "cli-docs"), root)
}

// writeGenericCLIDocsDir emits the generic per-command pages plus an index
// into outDir, removing any pages left over from commands that no longer
// exist.
func writeGenericCLIDocsDir(outDir string, root *cobra.Command) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	if err := removeMarkdownFiles(outDir); err != nil {
		return err
	}

	commands := availableCommandNames(root)
	if err := writeMarkdownFile(filepath.Join(outDir, "index.md"), genericCLIReferenceIndex(root, commands)); err != nil {
		return err
	}

	// commandDocID collapses punctuation, so distinct command names can map
	// to the same page file; the later write would silently win.
	seen := make(map[string]string, len(commands))
	for _, name := range commands {
		id := commandDocID(name)
		if prev, ok := seen[id]; ok {
			return fmt.Errorf("commands %q and %q both map to doc page %s.md; rename one", prev, name, id)
		}
		seen[id] = name

		var out bytes.Buffer
		if err := writeSingleCommandDoc(&out, root, name); err != nil {
			return err
		}
		path := filepath.Join(outDir, id+".md")
		if err := writeMarkdownFile(path, out.String()); err != nil {
			return err
		}
	}

	return nil
}

func genericCLIReferenceIndex(root *cobra.Command, commands []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "title: CLI Reference\n")
	fmt.Fprintf(&b, "description: Generated reference for every bd command\n")
	fmt.Fprintf(&b, "---\n\n")
	fmt.Fprintf(&b, "<!-- AUTO-GENERATED: do not edit manually -->\n\n")
	fmt.Fprintf(&b, "Generated from `bd help --docs-root`.\n\n")
	fmt.Fprintf(&b, "This reference covers all %d live top-level `bd` commands. Regenerate it with:\n\n", len(commands))
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "./scripts/generate-cli-docs.sh\n")
	fmt.Fprintf(&b, "```\n\n")
	// The per-command pages list only local flags, mirroring `--help`; the
	// global flags apply everywhere and are published once, here.
	if root.PersistentFlags().HasFlags() {
		fmt.Fprintf(&b, "## Global Flags\n\n")
		fmt.Fprintf(&b, "These flags apply to all commands:\n\n")
		fmt.Fprintf(&b, "```\n%s```\n\n", root.PersistentFlags().FlagUsages())
	}
	fmt.Fprintf(&b, "## Commands\n\n")
	for _, cmd := range commands {
		fmt.Fprintf(&b, "- [`bd %s`](./%s.md)\n", cmd, commandDocID(cmd))
	}
	return b.String()
}

func writeMarkdownFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content = strings.TrimRight(content, "\n") + "\n"
	// #nosec G306: generated repository Markdown should be readable like source files.
	return os.WriteFile(path, []byte(content), 0o644)
}

func removeMarkdownFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// findCommand finds a command by name in the command tree.
// Supports nested commands like "mol pour" by splitting on space.
func findCommand(root *cobra.Command, name string) *cobra.Command {
	// Handle nested commands (e.g., "mol pour")
	parts := strings.Split(name, " ")

	var current *cobra.Command
	for i, part := range parts {
		if i == 0 {
			// Start from root's direct commands
			current = findDirectCommand(root, part)
		} else {
			// Look in subcommands of current
			if current != nil {
				current = findDirectCommand(current, part)
			}
		}
		if current == nil {
			return nil
		}
	}
	return current
}

// findDirectCommand finds a direct child command by name.
func findDirectCommand(parent *cobra.Command, name string) *cobra.Command {
	for _, cmd := range parent.Commands() {
		if cmd.Name() == name {
			return cmd
		}
		// Also check aliases
		for _, alias := range cmd.Aliases {
			if alias == name {
				return cmd
			}
		}
	}
	return nil
}

// listAllCommands prints all available commands, one per line.
// Used by the generate-cli-docs.sh script.
func listAllCommands(w io.Writer, root *cobra.Command) {
	for _, name := range availableCommandNames(root) {
		fmt.Fprintln(w, name)
	}
}

func availableCommandNames(root *cobra.Command) []string {
	names := make([]string, 0, len(root.Commands()))
	for _, cmd := range root.Commands() {
		if cmd.IsAvailableCommand() {
			names = append(names, cmd.Name())
		}
	}
	sort.Strings(names)
	return names
}

func commandPath(cmd *cobra.Command) string {
	path := cmd.CommandPath()
	if path == "" {
		return cmd.Name()
	}
	return path
}

func commandDocID(commandPath string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(commandPath) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	id := strings.Trim(b.String(), "-")
	if id == "" {
		return "command"
	}
	return id
}

// escapeMDXText entity-escapes characters that MDX-based renderers treat as
// JSX (angle brackets, braces). The entities are plain HTML entities — valid
// CommonMark — so the generic output stays portable while remaining safe for
// MDX consumers.
func escapeMDXText(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"{", "&#123;",
		"}", "&#125;",
	)
	return replacer.Replace(s)
}
