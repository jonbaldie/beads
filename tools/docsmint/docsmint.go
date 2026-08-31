package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	// Full HTML comments on one line. bd's generic pages use these as
	// auto-generated markers; Mintlify parses .md through MDX, where HTML
	// comments are invalid, so they become JSX comments.
	htmlCommentRE = regexp.MustCompile(`<!--\s*(.*?)\s*-->`)
	// Relative page links in the generic index (./show.md). The deployed
	// Mintlify site serves extensionless routes.
	relativePageLinkRE = regexp.MustCompile(`\]\(\./([A-Za-z0-9_-]+)\.md\)`)
)

// run post-processes the generic staging tree at <root>/build/cli-docs into
// the committed Mintlify pages at <root>/docs/cli-reference and splices the
// CLI Reference pages array in <root>/docs/docs.json. When the generic tree
// is absent but the pinned bd's legacy emitter wrote
// <root>/website/docs/cli-reference (Docusaurus form, bd <= v1.1.0), those
// pages are converted to the generic form first (see legacy.go).
func run(root string) error {
	source, err := loadStagingSource(root)
	if err != nil {
		return err
	}
	target := filepath.Join(root, "docs", "cli-reference")
	docsJSON := filepath.Join(root, "docs", "docs.json")

	if err := neutralizeSingleFileReference(filepath.Join(root, "docs", "CLI_REFERENCE.md")); err != nil {
		return err
	}
	if err := prepareDocsmintTarget(target); err != nil {
		return err
	}

	navPages, err := writeStagingPages(source, target)
	if err != nil {
		return err
	}
	return spliceCLINav(docsJSON, append([]string{"cli-reference/index"}, navPages...))
}

type stagingSource struct {
	dir    string
	legacy bool
	pages  []string
}

func loadStagingSource(root string) (stagingSource, error) {
	staging := filepath.Join(root, "build", "cli-docs")
	legacyStaging := filepath.Join(root, "website", "docs", "cli-reference")
	stagingDir, legacy, entries, err := readStagingEntries(staging, legacyStaging)
	if err != nil {
		return stagingSource{}, err
	}
	pages := markdownPageNames(entries)
	if len(pages) == 0 {
		return stagingSource{}, fmt.Errorf("staging tree %s contains no markdown pages (run `bd help --docs-root` first)", stagingDir)
	}
	return stagingSource{dir: stagingDir, legacy: legacy, pages: pages}, nil
}

func readStagingEntries(staging, legacyStaging string) (string, bool, []os.DirEntry, error) {
	entries, err := os.ReadDir(staging)
	if err == nil {
		return staging, false, entries, nil
	}
	entries, err = os.ReadDir(legacyStaging)
	if err != nil {
		return "", false, nil, fmt.Errorf("no staging tree at %s or %s (run `bd help --docs-root` first)", staging, legacyStaging)
	}
	return legacyStaging, true, entries, nil
}

func markdownPageNames(entries []os.DirEntry) []string {
	var pages []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		pages = append(pages, entry.Name())
	}
	sort.Strings(pages)
	return pages
}

func prepareDocsmintTarget(target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	return removeMarkdownFiles(target)
}

func writeStagingPages(source stagingSource, target string) ([]string, error) {
	var navPages []string
	for _, name := range source.pages {
		generic, err := readStagingPage(source, name)
		if err != nil {
			return nil, err
		}
		out := transformPage(generic)
		// #nosec G306: generated repository Markdown should be readable like source files.
		if err := os.WriteFile(filepath.Join(target, name), []byte(out), 0o644); err != nil {
			return nil, err
		}
		if name != "index.md" {
			navPages = append(navPages, "cli-reference/"+strings.TrimSuffix(name, ".md"))
		}
	}
	return navPages, nil
}

func readStagingPage(source stagingSource, name string) (string, error) {
	// #nosec G304: name comes from os.ReadDir over the repo's own staging
	// tree (filtered to *.md above), not from external input.
	data, err := os.ReadFile(filepath.Join(source.dir, name))
	if err != nil {
		return "", err
	}
	if !source.legacy {
		return string(data), nil
	}
	converted, err := convertLegacyDocument(name, string(data))
	if err != nil {
		return "", fmt.Errorf("converting legacy page %s: %w", name, err)
	}
	return converted, nil
}

func convertLegacyDocument(name, content string) (string, error) {
	if name == "index.md" {
		return convertLegacyIndex(content)
	}
	return convertLegacyPage(content)
}

// transformPage converts one generic page to Mintlify form. Every rewrite
// skips fenced code blocks: fence contents are literal example text (an HTML
// comment or relative link inside a fence must render verbatim).
func transformPage(content string) string {
	lines := strings.Split(content, "\n")
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		line = htmlCommentRE.ReplaceAllString(line, "{/* $1 */}")
		line = relativePageLinkRE.ReplaceAllString(line, "](/cli-reference/$1)")
		lines[i] = neutralizeESMLine(line)
	}
	return strings.Join(lines, "\n")
}

// neutralizeESMHazards applies neutralizeESMLine to every line outside code
// fences.
func neutralizeESMHazards(content string) string {
	lines := strings.Split(content, "\n")
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		lines[i] = neutralizeESMLine(line)
	}
	return strings.Join(lines, "\n")
}

// neutralizeESMLine wraps a prose line whose first token is `export` or
// `import` in an inline code span. Such lines are legal CommonMark in bd's
// generic output (indented example lines in help text), but Mintlify's
// prebuild dedents them to column 0 and then parses them as MDX ESM blocks,
// which fails the entire page (observed on cli-reference/mail).
func neutralizeESMLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "export ") && !strings.HasPrefix(trimmed, "import ") {
		return line
	}
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	delim := "`"
	if strings.Contains(trimmed, "`") {
		delim = "``"
	}
	return indent + delim + trimmed + delim
}

// neutralizeSingleFileReference applies the ESM-hazard neutralization to
// docs/CLI_REFERENCE.md. bd emits it directly — without the per-page
// transforms — but Mintlify still builds it as a hidden page, so a bare
// indented `export ...` line fails it the same way.
func neutralizeSingleFileReference(path string) error {
	// #nosec G304: path is derived from the repo root argument this developer
	// tool is invoked with; it only ever reads the repo's own generated file.
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s (run `bd help --docs-root` first): %w", path, err)
	}
	out := neutralizeESMHazards(string(data))
	if out == string(data) {
		return nil
	}
	// #nosec G306: generated repository Markdown should be readable like source files.
	return os.WriteFile(path, []byte(out), 0o644)
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

// spliceCLINav rewrites the "pages" array of the "CLI Reference" navigation
// group in docs.json, leaving every other byte of the file untouched. A
// textual splice (rather than a JSON round-trip) preserves key order and
// formatting so regeneration is diff-stable. The pages entries are plain
// slugs, so bracket matching cannot be confused by string contents.
func spliceCLINav(docsJSONPath string, pages []string) error {
	// #nosec G304: docsJSONPath is derived from the repo root argument this
	// developer tool is invoked with; it only ever reads the repo's docs.json.
	data, err := os.ReadFile(docsJSONPath)
	if err != nil {
		return err
	}
	s := string(data)
	openIdx, closeIdx, indent, err := locateCLINavArray(s, docsJSONPath)
	if err != nil {
		return err
	}
	b := renderCLINavPages(pages, indent)

	out := s[:openIdx] + b + s[closeIdx+1:]
	var check any
	if err := json.Unmarshal([]byte(out), &check); err != nil {
		return fmt.Errorf("%s: splice produced invalid JSON, refusing to write: %w", docsJSONPath, err)
	}
	// #nosec G306: docs.json is repository source, readable like other source files.
	return os.WriteFile(docsJSONPath, []byte(out), 0o644)
}

func locateCLINavArray(s, docsJSONPath string) (int, int, string, error) {
	groupIdx := strings.Index(s, `"group": "CLI Reference"`)
	if groupIdx < 0 {
		return 0, 0, "", fmt.Errorf("%s: no \"CLI Reference\" navigation group found", docsJSONPath)
	}
	pagesIdx := strings.Index(s[groupIdx:], `"pages"`)
	if pagesIdx < 0 {
		return 0, 0, "", fmt.Errorf("%s: \"CLI Reference\" group has no \"pages\" key", docsJSONPath)
	}
	pagesIdx += groupIdx
	groupEnd := groupIdx + len(`"group": "CLI Reference"`)
	if err := verifySiblingGap(s[groupEnd:pagesIdx]); err != nil {
		return 0, 0, "", fmt.Errorf("%s: cannot safely locate the CLI Reference \"pages\" array: %w", docsJSONPath, err)
	}
	openIdx := strings.Index(s[pagesIdx:], "[")
	if openIdx < 0 {
		return 0, 0, "", fmt.Errorf("%s: \"CLI Reference\" pages key has no array", docsJSONPath)
	}
	openIdx += pagesIdx
	closeIdx := matchingArrayEnd(s, openIdx)
	if closeIdx < 0 {
		return 0, 0, "", fmt.Errorf("%s: unbalanced \"pages\" array in \"CLI Reference\" group", docsJSONPath)
	}
	lineStart := strings.LastIndex(s[:pagesIdx], "\n") + 1
	return openIdx, closeIdx, s[lineStart:pagesIdx], nil
}

func matchingArrayEnd(s string, openIdx int) int {
	depth := 0
	contentLength := len(s)
	for i := openIdx; i < contentLength; i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func renderCLINavPages(pages []string, indent string) string {
	entryIndent := indent + "  "
	var b strings.Builder
	b.WriteString("[\n")
	for i, p := range pages {
		// #nosec G705: not a web-output sink — this developer tool writes the
		// repo's own docs.json; entries are %q-quoted slugs derived from the
		// staging tree's filenames, which the same pipeline generates.
		fmt.Fprintf(&b, "%s%q", entryIndent, p)
		if i < len(pages)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(indent + "]")
	return b.String()
}

// verifySiblingGap ensures the "pages" key found after "group": "CLI
// Reference" is a sibling key of the same object: the gap between them may
// contain only scalar keys (strings, commas, whitespace). Any structural
// brace or bracket outside a string means the textual forward search crossed
// an object boundary — e.g. the group's keys were reordered so its own pages
// array precedes it — and a splice there would rewrite the WRONG group's
// pages. Fail closed instead.
func verifySiblingGap(gap string) error {
	inString := false
	escaped := false
	for _, r := range gap {
		if inString {
			switch {
			case escaped:
				escaped = false
			case r == '\\':
				escaped = true
			case r == '"':
				inString = false
			}
			continue
		}
		switch r {
		case '"':
			inString = true
		case '{', '}', '[', ']':
			return fmt.Errorf("structural %q between the group key and the next \"pages\" key; reorder docs.json so the CLI Reference group's \"group\" key precedes its \"pages\" array", r)
		}
	}
	return nil
}
