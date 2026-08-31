package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// registerCommonIssueFlags registers flags common to create and update commands.
func registerCommonIssueFlags(cmd *cobra.Command) {
	cmd.Flags().StringP("assignee", "a", "", "Assignee")
	cmd.Flags().StringP("description", "d", "", "Issue description")
	cmd.Flags().String("body", "", "Alias for --description (GitHub CLI convention)")
	_ = cmd.Flags().MarkHidden("body") // Hidden alias for agent/CLI ergonomics
	cmd.Flags().StringP("message", "m", "", "Alias for --description (git commit convention)")
	_ = cmd.Flags().MarkHidden("message") // Hidden alias for muscle memory from git commit -m
	cmd.Flags().String("body-file", "", "Read description from file (use - for stdin)")
	cmd.Flags().String("description-file", "", "Alias for --body-file")
	_ = cmd.Flags().MarkHidden("description-file") // Hidden alias
	cmd.Flags().Bool("stdin", false, "Read description from stdin (alias for --body-file -)")
	cmd.MarkFlagsMutuallyExclusive("stdin", "body-file")
	cmd.MarkFlagsMutuallyExclusive("stdin", "description-file")
	cmd.MarkFlagsMutuallyExclusive("stdin", "description")
	cmd.MarkFlagsMutuallyExclusive("stdin", "body")
	cmd.MarkFlagsMutuallyExclusive("stdin", "message")
	cmd.Flags().String("design", "", "Design notes")
	cmd.Flags().String("design-file", "", "Read design from file (use - for stdin)")
	cmd.MarkFlagsMutuallyExclusive("design", "design-file")
	cmd.Flags().String("acceptance", "", "Acceptance criteria")
	cmd.Flags().String("notes", "", "Additional notes")
	cmd.Flags().String("append-notes", "", "Append to existing notes (with newline separator)")
	cmd.Flags().String("external-ref", "", "External reference (e.g., 'gh-9', 'jira-ABC', Linear URL)")
}

// getDescriptionFlag retrieves the description value, checking --body-file, --description-file,
// --description, and --body (in that order of precedence).
// Supports reading from stdin via --description=- or --body=- (useful when description
// contains apostrophes or other characters that are hard to escape in shell).
// Returns the value, whether any flag was explicitly changed, and any error.
func getDescriptionFlag(cmd *cobra.Command) (string, bool, error) {
	if stdinFlag, _ := cmd.Flags().GetBool("stdin"); stdinFlag {
		return readDescriptionStdin()
	}
	bodyFileChanged := cmd.Flags().Changed("body-file")
	descFileChanged := cmd.Flags().Changed("description-file")
	if bodyFileChanged || descFileChanged {
		return descriptionFromBodyFile(cmd, bodyFileChanged, descFileChanged)
	}
	return descriptionFromInlineFlags(cmd)
}

func readDescriptionStdin() (string, bool, error) {
	content, err := readBodyFile("-")
	if err != nil {
		return "", false, HandleError("reading from stdin: %v", err)
	}
	return content, true, nil
}

func descriptionFromBodyFile(cmd *cobra.Command, bodyFileChanged, descFileChanged bool) (string, bool, error) {
	if err := rejectConflictingBodyFiles(cmd, bodyFileChanged, descFileChanged); err != nil {
		return "", false, err
	}
	filePath, _ := cmd.Flags().GetString("description-file")
	if bodyFileChanged {
		filePath, _ = cmd.Flags().GetString("body-file")
	}
	descChanged := cmd.Flags().Changed("description")
	bodyChanged := cmd.Flags().Changed("body")
	messageChanged := cmd.Flags().Changed("message")
	if descChanged || bodyChanged || messageChanged {
		return "", false, HandleError("cannot specify both --body-file and --description/--body/--message")
	}
	content, err := readBodyFile(filePath)
	if err != nil {
		return "", false, HandleError("reading body file: %v", err)
	}
	return content, true, nil
}

func rejectConflictingBodyFiles(cmd *cobra.Command, bodyFileChanged, descFileChanged bool) error {
	if !bodyFileChanged || !descFileChanged {
		return nil
	}
	bodyFile, _ := cmd.Flags().GetString("body-file")
	descFile, _ := cmd.Flags().GetString("description-file")
	if bodyFile != descFile {
		return HandleError("cannot specify both --body-file and --description-file with different values")
	}
	return nil
}

func descriptionFromInlineFlags(cmd *cobra.Command) (string, bool, error) {
	desc, _ := cmd.Flags().GetString("description")
	body, _ := cmd.Flags().GetString("body")
	message, _ := cmd.Flags().GetString("message")
	if desc == "-" || body == "-" || message == "-" {
		return descriptionFromDashAlias(cmd, desc, body, message)
	}
	return pickInlineDescription(cmd, desc, body, message)
}

func descriptionFromDashAlias(cmd *cobra.Command, desc, body, message string) (string, bool, error) {
	values := dashDescriptionValues(cmd, desc, body, message)
	if err := rejectConflictingDashDescriptions(values); err != nil {
		return "", false, err
	}
	return readDescriptionStdin()
}

func dashDescriptionValues(cmd *cobra.Command, desc, body, message string) map[string]string {
	values := make(map[string]string)
	if cmd.Flags().Changed("description") {
		values["--description"] = desc
	}
	if cmd.Flags().Changed("body") {
		values["--body"] = body
	}
	if cmd.Flags().Changed("message") {
		values["--message"] = message
	}
	return values
}

func rejectConflictingDashDescriptions(values map[string]string) error {
	if len(values) <= 1 {
		return nil
	}
	var firstVal string
	for _, v := range values {
		if firstVal == "" {
			firstVal = v
			continue
		}
		if v != firstVal {
			fmt.Fprintf(os.Stderr, "Error: cannot specify multiple description flags with different values\n")
			for flag, val := range values {
				fmt.Fprintf(os.Stderr, "  %s: %q\n", flag, val)
			}
			return SilentExit()
		}
	}
	return nil
}

func pickInlineDescription(cmd *cobra.Command, desc, body, message string) (string, bool, error) {
	descChanged := cmd.Flags().Changed("description")
	bodyChanged := cmd.Flags().Changed("body")
	messageChanged := cmd.Flags().Changed("message")
	if err := rejectConflictingInlineDescriptions(descChanged, bodyChanged, messageChanged, desc, body, message); err != nil {
		return "", false, err
	}
	if descChanged {
		return desc, true, nil
	}
	if bodyChanged {
		return body, true, nil
	}
	if messageChanged {
		return message, true, nil
	}
	return desc, descChanged, nil
}

func rejectConflictingInlineDescriptions(descChanged, bodyChanged, messageChanged bool, desc, body, message string) error {
	if err := rejectDescriptionPair(descChanged, bodyChanged, "--description", "--body", desc, body); err != nil {
		return err
	}
	firstVal, firstFlag := desc, "--description"
	if !descChanged {
		firstVal, firstFlag = body, "--body"
	}
	if !descChanged && !bodyChanged {
		return nil
	}
	return rejectDescriptionPair(true, messageChanged, firstFlag, "--message", firstVal, message)
}

func rejectDescriptionPair(firstSet, secondSet bool, firstFlag, secondFlag, firstVal, secondVal string) error {
	if !firstSet || !secondSet || secondVal == firstVal {
		return nil
	}
	fmt.Fprintf(os.Stderr, "Error: cannot specify both %s and %s with different values\n", firstFlag, secondFlag)
	fmt.Fprintf(os.Stderr, "  %s: %q\n", firstFlag, firstVal)
	fmt.Fprintf(os.Stderr, "  %s: %q\n", secondFlag, secondVal)
	return SilentExit()
}

// getDesignFlag retrieves the design value from --design-file or --design.
// Returns the value, whether any flag was explicitly changed, and any error.
func getDesignFlag(cmd *cobra.Command) (string, bool, error) {
	if cmd.Flags().Changed("design-file") {
		path, _ := cmd.Flags().GetString("design-file")
		content, err := readBodyFile(path)
		if err != nil {
			return "", false, HandleError("reading from stdin: %v", err)
		}
		return content, true, nil
	}

	if cmd.Flags().Changed("design") {
		v, _ := cmd.Flags().GetString("design")
		return v, true, nil
	}

	return "", false, nil
}

// readBodyFile reads the description content from a file.
// If filePath is "-", reads from stdin.
func readBodyFile(filePath string) (string, error) {
	var reader io.Reader

	if filePath == "-" {
		reader = os.Stdin
	} else {
		// #nosec G304 - filePath comes from user flag, validated by caller
		file, err := os.Open(filePath)
		if err != nil {
			return "", fmt.Errorf("failed to open file: %w", err)
		}
		defer file.Close()
		reader = file
	}

	content, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	return string(content), nil
}

// textSources names the places a command can take body text from: stdin, a
// file path, an explicit text flag, then positional args. A non-nil stdin
// means --stdin was given. flagName names the text flag (e.g. "--response")
// in conflict errors and always accompanies flagText. flagSet marks the text
// flag as explicitly passed (cobra's Changed), so an empty flag value still
// counts as a source — like a blank positional — rather than as absent.
type textSources struct {
	stdin      io.Reader
	filePath   string
	flagText   string
	flagName   string
	flagSet    bool
	positional []string
}

// textFromSources resolves body text from the single provided source: stdin,
// file path, flag value, or positional args joined with spaces. Combining any
// two sources is an error rather than a silent drop of one of them — commands
// normally reject flag combinations earlier via registerTextSourceFlags'
// mutual exclusion, and the error here keeps a command that forgets that
// registration from silently ignoring a source. Blank positional tokens and
// an explicitly-set-but-empty text flag never resolve to text, but still
// count as provided — the user attempted to pass text (e.g. an empty "$VAR"),
// so an empty result is "cannot be empty" rather than "no source provided".
// Trailing newlines (including CRLF) are trimmed from stdin — shells append
// one (echo, heredocs) — while file content is passed through verbatim like
// every other file-input flag. Returns "" with provided=false when no source
// is given at all.
type textSource struct {
	name    string
	resolve func() (string, error)
}

func textFromSources(src textSources) (text string, provided bool, err error) {
	sources := collectTextSources(src)
	provided = len(sources) > 0 || len(src.positional) > 0
	switch len(sources) {
	case 0:
		return "", provided, nil
	case 1:
		text, err = sources[0].resolve()
		return text, provided, err
	default:
		return "", provided, combinedTextSourceError(sources)
	}
}

func collectTextSources(src textSources) []textSource {
	var sources []textSource
	sources = appendPositionalTextSource(sources, src.positional)
	sources = appendStdinTextSource(sources, src.stdin)
	sources = appendFileTextSource(sources, src.filePath)
	return appendFlagTextSource(sources, src)
}

func appendPositionalTextSource(sources []textSource, positional []string) []textSource {
	joined := strings.Join(positional, " ")
	if strings.TrimSpace(joined) == "" {
		return sources
	}
	return append(sources, textSource{fmt.Sprintf("positional text %q", joined), func() (string, error) {
		return joined, nil
	}})
}

func appendStdinTextSource(sources []textSource, stdin io.Reader) []textSource {
	if stdin == nil {
		return sources
	}
	return append(sources, textSource{"--stdin", func() (string, error) {
		content, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("reading from stdin: %w", err)
		}
		return strings.TrimRight(string(content), "\r\n"), nil
	}})
}

func appendFileTextSource(sources []textSource, filePath string) []textSource {
	if filePath == "" {
		return sources
	}
	return append(sources, textSource{"--file", func() (string, error) {
		// Verbatim, like every other file-input flag (--body-file,
		// --design-file, --reason-file): a file is a deliberate payload,
		// so its trailing newlines are preserved.
		return readBodyFile(filePath)
	}})
}

func appendFlagTextSource(sources []textSource, src textSources) []textSource {
	if !src.flagSet && src.flagText == "" {
		return sources
	}
	return append(sources, textSource{src.flagName, func() (string, error) {
		return src.flagText, nil
	}})
}

func combinedTextSourceError(sources []textSource) error {
	names := make([]string, len(sources))
	for i, s := range sources {
		names[i] = s.name
	}
	return fmt.Errorf("cannot combine %s", strings.Join(names, " with "))
}

// cmdTextSources builds textSources from a command's --stdin and --file
// flags (either may be unregistered) plus its positional text args. Callers
// with a command-specific text flag fill in flagText/flagName themselves.
func cmdTextSources(cmd *cobra.Command, positional []string) textSources {
	src := textSources{positional: positional}
	if stdinFlag, _ := cmd.Flags().GetBool("stdin"); stdinFlag {
		src.stdin = os.Stdin
	}
	src.filePath, _ = cmd.Flags().GetString("file")
	return src
}

// registerTextSourceFlags registers the shared text-source flags — --stdin
// and --file, read back by name in cmdTextSources — and marks them mutually
// exclusive with each other and with any command-specific text flags (e.g.
// "response", registered by the caller beforehand). noun names the text in
// the flag help (e.g. "comment text").
func registerTextSourceFlags(cmd *cobra.Command, noun string, textFlags ...string) {
	cmd.Flags().Bool("stdin", false, "Read "+noun+" from stdin")
	cmd.Flags().String("file", "", "Read "+noun+" from file")
	cmd.MarkFlagsMutuallyExclusive(append([]string{"stdin", "file"}, textFlags...)...)
}

// requireTextFromSources resolves body text like textFromSources and owns the
// shared empty-text policy: text from an explicit source must be non-blank
// ("<noun> cannot be empty"), and with no source at all the error lists the
// command's accepted sources via hint (e.g. "use positional args, --stdin, or
// --file").
func requireTextFromSources(noun, hint string, src textSources) (string, error) {
	text, provided, err := textFromSources(src)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) != "" {
		return text, nil
	}
	if provided {
		return "", fmt.Errorf("%s cannot be empty", noun)
	}
	return "", fmt.Errorf("no %s provided (%s)", noun, hint)
}

// registerPriorityFlag registers the priority flag with a specific default value.
func registerPriorityFlag(cmd *cobra.Command, defaultVal string) {
	cmd.Flags().StringP("priority", "p", defaultVal, "Priority (0-4 or P0-P4, 0=highest)")
}
