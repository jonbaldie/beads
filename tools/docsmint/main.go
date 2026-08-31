// Command docsmint post-processes bd's generic CLI documentation into the
// Mintlify site. bd itself emits only vendor-neutral Markdown (see
// `bd help --docs-root`, which writes the generic pages to build/cli-docs/);
// everything Mintlify-specific — MDX-safe comment markers, extensionless
// route links, and the CLI Reference pages array inside docs/docs.json —
// happens here, in repo tooling, so the OSS binary stays free of
// site-generator formats.
//
// Usage: go run ./tools/docsmint [repo-root]
// (scripts/generate-cli-docs.sh runs it right after bd help --docs-root.)
package main

import (
	"fmt"
	"os"
)

func main() {
	runMain(os.Args, os.Exit)
}

func runMain(args []string, exit func(int)) {
	root := "."
	if len(args) > 1 {
		root = args[1]
	}
	if len(args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: docsmint [repo-root]")
		exit(2)
		return
	}
	if err := run(root); err != nil {
		fmt.Fprintln(os.Stderr, "docsmint:", err)
		exit(1)
	}
}
