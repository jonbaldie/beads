package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const legacyShowPage = `---
id: show
title: bd show
slug: /cli-reference/show
sidebar_position: 40
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from ` + "`bd help --doc show`" + `

## bd show

Show issue details

### bd show sub

Nested subcommand help.

` + "```" + `
### fenced heading stays put
` + "```" + `
`

const legacyIndexPage = `---
id: index
title: CLI Reference
sidebar_position: 0
---

# CLI Reference

<!-- AUTO-GENERATED: do not edit manually -->
Reference for bd Latest. Generated from ` + "`bd help --docs-root`" + `.

This reference covers all 2 live top-level ` + "`bd`" + ` commands.

## Commands

- [` + "`bd show`" + `](./show.md)
`

func TestConvertLegacyPageProducesGenericForm(t *testing.T) {
	got, err := convertLegacyPage(legacyShowPage)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"title: \"bd show\"\n",
		"description: \"Show issue details\"\n",
		"Generated from `bd help --doc show`.\n",
		"\n## bd show sub\n",
		"\n### fenced heading stays put\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("converted page missing %q\n--- got ---\n%s", want, got)
		}
	}
	if strings.Contains(got, "sidebar_position") || strings.Contains(got, "slug:") {
		t.Errorf("legacy frontmatter leaked into converted page:\n%s", got)
	}
	if strings.Contains(got, "\n## bd show\n") {
		t.Errorf("top command heading should be dropped:\n%s", got)
	}
}

func TestConvertLegacyIndexProducesGenericForm(t *testing.T) {
	got, err := convertLegacyIndex(legacyIndexPage)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"title: CLI Reference\n",
		"description: Generated reference for every bd command\n",
		"Generated from `bd help --docs-root`.\n",
		"- [`bd show`](./show.md)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("converted index missing %q\n--- got ---\n%s", want, got)
		}
	}
	if strings.Contains(got, "# CLI Reference\n\n<!--") {
		t.Errorf("H1 should be dropped (frontmatter title is the H1):\n%s", got)
	}
	if strings.Contains(got, "Reference for bd Latest") {
		t.Errorf("versioned intro line should be replaced:\n%s", got)
	}
}

func TestConvertLegacyPageFailsOnUnexpectedShape(t *testing.T) {
	for name, test := range map[string]struct {
		content string
		want    string
	}{
		"no frontmatter": {
			content: "## bd show\n",
			want:    "missing frontmatter",
		},
		"no marker": {
			content: "---\nid: x\n---\n\nGenerated from `bd help --doc x`\n\n## bd x\n",
			want:    `missing "<!-- AUTO-GENERATED: do not edit manually -->" marker`,
		},
		"no generated line": {
			content: "---\nid: x\n---\n\n<!-- AUTO-GENERATED: do not edit manually -->\n",
			want:    "missing Generated-from line",
		},
		"no command head": {
			content: "---\nid: x\n---\n\n<!-- AUTO-GENERATED: do not edit manually -->\nGenerated from `bd help --doc x`\n\nprose\n",
			want:    "expected '## bd <command>' heading",
		},
	} {
		if _, err := convertLegacyPage(test.content); err == nil || err.Error() != test.want {
			t.Errorf("%s: error = %v, want %q", name, err, test.want)
		}
	}
}

func TestLegacyPageShort(t *testing.T) {
	for name, test := range map[string]struct {
		lines []string
		want  string
	}{
		"empty":         {nil, ""},
		"blank":         {[]string{"   "}, ""},
		"heading":       {[]string{"## Usage"}, ""},
		"fence":         {[]string{"```console"}, ""},
		"trimmed prose": {[]string{"  Show one issue  "}, "Show one issue"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := legacyPageShort(test.lines); got != test.want {
				t.Fatalf("legacyPageShort(%q) = %q, want %q", test.lines, got, test.want)
			}
		})
	}
}

func TestStripFrontmatterFailures(t *testing.T) {
	for name, test := range map[string]struct {
		content string
		want    string
	}{
		"missing":      {"title: x\n", "missing frontmatter"},
		"unterminated": {"---\ntitle: x\n", "unterminated frontmatter"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := stripFrontmatter(test.content); err == nil || err.Error() != test.want {
				t.Fatalf("stripFrontmatter() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRunConsumesLegacyStagingTree(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "website", "docs", "cli-reference", "index.md"), legacyIndexPage)
	writeFile(t, filepath.Join(root, "website", "docs", "cli-reference", "show.md"), legacyShowPage)
	writeFile(t, filepath.Join(root, "docs", "CLI_REFERENCE.md"), "# bd — Complete Command Reference\n")
	writeFile(t, filepath.Join(root, "docs", "docs.json"), `{
  "navigation": {
    "groups": [
      {
        "group": "CLI Reference",
        "pages": [
          "cli-reference/index"
        ]
      }
    ]
  }
}
`)

	if err := run(root); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(filepath.Join(root, "docs", "cli-reference", "show.md"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(out)
	if !strings.Contains(page, "{/* AUTO-GENERATED: do not edit manually */}") {
		t.Errorf("legacy page should get the Mintlify comment transform:\n%s", page)
	}
	if !strings.Contains(page, "title: \"bd show\"") {
		t.Errorf("legacy page should carry generic frontmatter:\n%s", page)
	}

	idx, err := os.ReadFile(filepath.Join(root, "docs", "cli-reference", "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(idx), "](/cli-reference/show)") {
		t.Errorf("legacy index links should become extensionless routes:\n%s", idx)
	}

	nav, err := os.ReadFile(filepath.Join(root, "docs", "docs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nav), `"cli-reference/show"`) {
		t.Errorf("nav should include the legacy-converted page:\n%s", nav)
	}
}
