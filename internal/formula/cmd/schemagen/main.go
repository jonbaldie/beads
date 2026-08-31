// Command schemagen regenerates internal/formula/schema_gen.go from
// internal/formula/types.go. Driven by the //go:generate directive in
// internal/formula/schema.go; not invoked at runtime.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/jonbaldie/beads/internal/formula/schemagen"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("schemagen", flag.ContinueOnError)
	typesPath := flags.String("types", "types.go", "path to types.go")
	outPath := flags.String("out", "schema_gen.go", "output path")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("schemagen: %w", err)
	}

	src, err := schemagen.Generate(*typesPath)
	if err != nil {
		return fmt.Errorf("schemagen: %w", err)
	}
	if err := os.WriteFile(*outPath, src, 0600); err != nil {
		return fmt.Errorf("schemagen: write %s: %w", *outPath, err)
	}
	return nil
}
