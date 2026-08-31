package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
)

func runCompactDoltProxiedServer(ctx context.Context, dryRun bool) error {
	start := time.Now()

	if !dryRun {
		if err := CheckReadonly("compact"); err != nil {
			return err
		}
	}

	if dryRun {
		if isJSONOutput() {
			return outputJSON(map[string]interface{}{
				"dry_run": true,
			})
		}
		fmt.Printf("DRY RUN - Dolt garbage collection\n\n")
		fmt.Printf("Run without --dry-run to perform garbage collection.\n")
		return nil
	}

	if !isJSONOutput() {
		fmt.Printf("Running Dolt garbage collection...\n")
	}

	err := runProxiedNonTx(ctx, func(ctx context.Context, conn *sql.Conn) error {
		return versioncontrolops.DoltGC(ctx, conn)
	})
	if err != nil {
		return HandleErrorRespectJSON("dolt gc failed: %v", err)
	}

	elapsed := time.Since(start)

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"success":    true,
			"elapsed_ms": elapsed.Milliseconds(),
		})
	}

	fmt.Printf("✓ Dolt garbage collection complete\n")
	fmt.Printf("  Time: %v\n", elapsed)
	return nil
}
