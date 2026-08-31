package main

import (
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/ui"
)

func printInitSuccess(args initFinalizeArgs, ident *initIdentityState) error {
	if args.mode.quiet {
		return nil
	}
	printInitSuccessHeader(args)
	printInitSuccessMode(args)
	printInitSuccessIdentity(args, ident)
	printInitBackupHint(args)
	printInitDiagnostics(args)
	return nil
}

func printInitSuccessHeader(args initFinalizeArgs) {
	if args.mode.bootstrappedFromRemote {
		fmt.Printf("\n%s bd initialized from git remote!\n\n", ui.RenderPass("✓"))
		return
	}
	fmt.Printf("\n%s bd initialized successfully!\n\n", ui.RenderPass("✓"))
}

func printInitSuccessMode(args initFinalizeArgs) {
	fmt.Printf("  Backend: %s\n", ui.RenderAccent(args.ident.backend))
	if !usesSQLServer() {
		fmt.Printf("  Mode: %s\n", ui.RenderAccent("embedded"))
		return
	}
	host, port, user := initSuccessServerConn(args)
	fmt.Printf("  Mode: %s\n", ui.RenderAccent("server"))
	fmt.Printf("  Server: %s\n", ui.RenderAccent(fmt.Sprintf("%s@%s:%d", user, host, port)))
	if args.ident.serverHost == "" && os.Getenv("BEADS_DOLT_SERVER_HOST") == "" {
		fmt.Fprintf(os.Stderr, "\n  %s Server host defaulted to %s.\n", ui.RenderWarn("⚠"), configfile.DefaultDoltServerHost)
		fmt.Fprintf(os.Stderr, "    If your Dolt server is remote, set BEADS_DOLT_SERVER_HOST or pass --server-host.\n")
	}
}

func initSuccessServerConn(args initFinalizeArgs) (host string, port int, user string) {
	host = args.ident.serverHost
	if host == "" {
		host = configfile.DefaultDoltServerHost
	}
	port = args.ident.serverPort
	if port == 0 {
		port = doltserver.DefaultConfig(args.ident.beadsDir).Port
	}
	user = args.ident.serverUser
	if user == "" {
		user = configfile.DefaultDoltServerUser
	}
	return host, port, user
}

func printInitSuccessIdentity(args initFinalizeArgs, ident *initIdentityState) {
	effectivePrefix := args.ident.prefix
	if ident.dbIdentity.Prefix != "" {
		effectivePrefix = ident.dbIdentity.Prefix
	}
	fmt.Printf("  Database: %s\n", ui.RenderAccent(args.ident.dbName))
	fmt.Printf("  Issue prefix: %s\n", ui.RenderAccent(effectivePrefix))
	fmt.Printf("  Issues will be named: %s\n\n", ui.RenderAccent(effectivePrefix+"-<hash> (e.g., "+effectivePrefix+"-a3f2dd)"))
	fmt.Printf("Run %s to get started.\n\n", ui.RenderAccent("bd quickstart"))
}

func printInitBackupHint(args initFinalizeArgs) {
	if args.mode.bootstrappedFromRemote || !dolt.HasBackupFiles(args.ident.beadsDir) {
		return
	}
	fmt.Printf("  %s Backup files detected in .beads/backup/\n", ui.RenderWarn("!"))
	fmt.Printf("    To restore issues from a previous backup, run:\n")
	fmt.Printf("      %s\n\n", ui.RenderAccent("bd backup restore"))
}

func printInitDiagnostics(args initFinalizeArgs) {
	if !usesSQLServer() {
		return
	}
	doctorResult := runInitDiagnostics(args.ident.cwd)
	failed := failedInitDiagnosticChecks(doctorResult)
	if len(failed) == 0 {
		return
	}
	fmt.Printf("%s Setup incomplete. Some issues were detected:\n", ui.RenderWarn("⚠"))
	for _, check := range failed {
		fmt.Printf("  • %s: %s\n", check.Name, check.Message)
	}
	fmt.Printf("\nRun %s to see details and fix these issues.\n\n", ui.RenderAccent("bd doctor --fix"))
}

func failedInitDiagnosticChecks(result doctorResult) []doctorCheck {
	var failed []doctorCheck
	for _, check := range result.Checks {
		if check.Status != statusOK {
			failed = append(failed, check)
		}
	}
	return failed
}
