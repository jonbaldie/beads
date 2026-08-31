//go:build cgo

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"path/filepath"

	"github.com/jonbaldie/beads/internal/storage/embeddeddolt"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	dir := flag.String("dir", "", "path to .beads directory")
	database := flag.String("database", "beads", "database name")
	branch := flag.String("branch", "main", "branch name")
	flag.Parse()

	if *dir == "" {
		flag.Usage()
		return fmt.Errorf("error: --dir is required")
	}

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		return fmt.Errorf("error: resolving path: %w", err)
	}

	ctx := context.Background()
	store, err := embeddeddolt.Open(ctx, absDir, *database, *branch)
	if err != nil {
		return fmt.Errorf("error: %w", err)
	}
	_ = store.Close()
	fmt.Println("ok")
	return nil
}
