package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/httpapi"
)

// serveCmdName is the command name, shared with the root command's post-run
// policy (runsPostCommandMaintenance) so the exclusion cannot drift from the
// command it names.
const serveCmdName = "serve"

// providerCloseTimeout bounds the shutdown close of a provider serve built
// itself. It is not the drain budget — the server has already drained by then.
const providerCloseTimeout = 10 * time.Second

// serveTokenFileEnv is the environment fallback for --auth-token-file, which an
// operator would otherwise have to thread through a container spec. It follows
// the BEADS_* convention the rest of the binary uses, and applies ONLY when the
// flag was not passed.
//
// It is read here and nowhere else. internal/httpapi never reads the
// environment, so the library, its tests and an embedded caller cannot be
// reconfigured by an exported variable in somebody's shell.
const (
	// #nosec G101 -- this is the NAME of an environment variable that holds a
	// file PATH. No credential appears in this source file, and none may: a
	// token reaches the process only by being read out of that file.
	serveTokenFileEnv = "BEADS_SERVE_TOKEN_FILE"
)

var serveCmd = &cobra.Command{
	Use:   serveCmdName,
	Short: "Serve the beads HTTP API over loopback",
	Long: `Serve the beads HTTP API — the same work surface the CLI answers, for
automation clients that would otherwise fork a bd subprocess per call.

The wire contract is described by an OpenAPI document (/v0); GET
/v0/beads/context reports which operations this build actually implements.

DEPLOYMENT

  Pass an explicit port. The default 127.0.0.1:0 takes an ephemeral one, which
  is right for ad-hoc and test use — where the bound address printed on stdout
  is read immediately — but carries no mutual exclusion: two serves against one
  workspace then run side by side on different ports with no way to enumerate
  them. On a fixed port the second one fails to bind, which is the intended
  behavior. Concurrent serves are data-safe either way; claims are arbitrated
  in the SQL server.

  Run it under a supervisor. bd shuts down gracefully on SIGHUP as well as
  SIGINT and SIGTERM, so closing the terminal of a foreground bd serve stops it.

PROBES

  GET /healthz is LIVENESS only: it answers from the process and never touches
  the database, so it stays green while the database is unreachable. For
  readiness use GET /v0/beads/ready?limit=1 — a real query, where 200 means
  ready and 503 means live but not ready.

AUTHENTICATION

  Optional, and off by default on loopback — where the trust model is the
  loopback boundary itself, the same one the database behind it already relies
  on. --auth-token-file turns it on: every operation except GET /healthz then
  requires an "Authorization: Bearer <token>" header, GET /v0/beads/context
  included, because it reports the repo root, beads directory and database name.

  The file holds ONE TOKEN PER LINE and every line is accepted. That is the
  rotation mechanism: write the new token alongside the old, roll the clients
  over, then delete the old line. The file is re-read while the server runs, so
  both the addition and the removal take effect within about a second and
  neither needs a restart. Write it atomically (a temp file plus rename; a
  Kubernetes secret mount already does this).

  There is deliberately no --auth-token flag. A credential passed as an
  argument is readable by every local user in the process listing.

  --allow-non-loopback REQUIRES a token file. Beyond loopback, reaching the
  address would otherwise be the whole authorization: any peer could read every
  issue and claim work as any actor. --insecure-no-auth is the explicit,
  auditable way to say you meant that anyway.

  The Host allowlist is what a service deployment usually trips over first. The
  DNS-rebinding check answers only to loopback spellings and the bind address,
  so a client dialing a service name gets 400 on every request; enumerate the
  names it dials with --allowed-host (repeatable). Matching is exact — no
  wildcards — and the startup log line prints the effective allowlist.

WHAT THIS DOES NOT DO

  No TLS. Even with a token, the credential and every issue body travel in
  plaintext, so a deployment beyond loopback has to supply confidentiality
  itself — a service mesh, or a network boundary you already trust.

  Hooks do not fire. A hook is a user-controlled subprocess per mutation: in a
  concurrent server that is an unbounded latency multiplier and an orphaned
  child at shutdown, and its working-directory-derived hook lookup is
  meaningless in a server process. A CLI claim runs on_update; an HTTP claim
  does not.

  The per-command auto-commit machinery does not run. Durability is per request:
  a successful claim commits inside its own transaction, exactly as a proxied
  CLI claim does today.

  An actor on an HTTP request is caller-asserted provenance for the audit trail,
  not authenticated identity — the same thing it has always been on the CLI,
  where any local process can pass any --actor.

  It does not run under --readonly, and refuses to start rather than binding.
  Every server it binds publishes the issue-claim operation, and the capability
  set it advertises is a property of the build rather than of the flags on the
  process that started it — so a read-only server would advertise a write it
  could never land.

DESTRUCTIVE OPERATIONS

  POST /v0/beads/issues:sweep deletes closed beads in bulk — the operation
  behind bd purge and bd prune — and nothing it deletes comes back. It shares
  the library surface those commands call, so it inherits their guards: pinned
  beads are never swept, and a durable sweep with neither a cutoff nor an id
  pattern is refused rather than clearing every closed bead in the workspace.
  Combined with the trust model above, that means anyone who can reach this
  address can erase closed work; bind it accordingly.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServeWithFlags(serveFlagOptionsFromCommand(cmd))
	},
}

func init() {
	registerServeFlags(serveCmd)
	rootCmd.AddCommand(serveCmd)
}

// registerServeFlags declares bd serve's own flags.
//
// It is a named function rather than a block in init so a test that runs the
// command in-process can put the flag set back the way it found it: cobra
// merges every inherited persistent flag into a command's own FlagSet the first
// time it parses one, and that mutation outlives the run.
func registerServeFlags(cmd *cobra.Command) {
	cmd.Flags().String("addr", "127.0.0.1:0",
		"Address to bind as IP:PORT; the host must be a numeric IP literal, and port 0 takes an ephemeral port")
	cmd.Flags().Bool("allow-non-loopback", false,
		"Permit a bind beyond loopback. Requires --auth-token-file, since reaching the address would otherwise be the whole authorization")
	cmd.Flags().String("auth-token-file", "",
		"Require an Authorization: Bearer token from this file, one token per line, all accepted. Re-read while running, so rewriting it rotates tokens without a restart (env "+serveTokenFileEnv+")")
	cmd.Flags().Bool("insecure-no-auth", false,
		"Serve a non-loopback bind with NO authentication. Every peer that can reach the address gets full read and claim access")
	cmd.Flags().StringArray("allowed-host", nil,
		"Additional Host header value to answer to, e.g. a service DNS name. Repeatable; matched exactly, with no wildcards")
}

type serveFlagOptions struct {
	addr             string
	allowNonLoopback bool
	authTokenFile    string
	insecureNoAuth   bool
	allowedHosts     []string
}

func serveFlagOptionsFromCommand(cmd *cobra.Command) serveFlagOptions {
	flags := cmd.Flags()
	addr, _ := flags.GetString("addr")
	allowNonLoopback, _ := flags.GetBool("allow-non-loopback")
	authTokenFile, _ := flags.GetString("auth-token-file")
	insecureNoAuth, _ := flags.GetBool("insecure-no-auth")
	allowedHosts, _ := flags.GetStringArray("allowed-host")
	return serveFlagOptions{
		addr:             addr,
		allowNonLoopback: allowNonLoopback,
		authTokenFile:    authTokenFile,
		insecureNoAuth:   insecureNoAuth,
		allowedHosts:     allowedHosts,
	}
}

// serveOptions is the part of a server's configuration that depends on NEITHER
// the workspace nor its database source: the operator's flags, and the
// environment fallbacks behind them.
//
// It is deliberately not an httpapi.Config. Every Config bd builds names the
// one database source it serves from, and that source is not known until the
// workspace has been classified — so a Config filled in this early would be a
// server configuration that cannot yet describe a server. The field names match
// Config's because they become those fields verbatim, in applyTo.
type serveOptions struct {
	Addr             string
	AllowNonLoopback bool
	InsecureNoAuth   bool
	AllowedHosts     []string
	Auth             *httpapi.TokenFileAuth
}

// applyServeOptions writes the operator's choices onto the configuration a database arm
// built. It is the ONLY place they are set, so an arm can add a source but can
// never be the one that forgot the credential or the host allowlist.
func applyServeOptions(cfg *httpapi.Config, o serveOptions) {
	cfg.Addr = o.Addr
	cfg.AllowNonLoopback = o.AllowNonLoopback
	cfg.InsecureNoAuth = o.InsecureNoAuth
	cfg.AllowedHosts = o.AllowedHosts
	cfg.Auth = o.Auth
}

// resolveServeConfig turns the flags and their environment fallbacks into the
// parts of the server configuration that depend on NEITHER the workspace nor a
// listener.
//
// It is separate from runServe, and runs first, for the reason every other
// validation in this command does: a refusal for a posture that cannot be
// served must not depend on which workspace the operator happened to be
// standing in, and must not arrive after a database has been opened.
func resolveServeConfig() (serveOptions, error) {
	return resolveServeConfigFromFlags(serveFlagOptionsFromCommand(serveCmd))
}

func resolveServeConfigFromFlags(flags serveFlagOptions) (serveOptions, error) {
	cfg := serveOptions{
		Addr:             flags.addr,
		AllowNonLoopback: flags.allowNonLoopback,
		InsecureNoAuth:   flags.insecureNoAuth,
		AllowedHosts:     flags.allowedHosts,
	}
	if _, err := httpapi.ValidateBindAddr(flags.addr, flags.allowNonLoopback); err != nil {
		return cfg, err
	}

	tokenFile := flags.authTokenFile
	if tokenFile == "" {
		tokenFile = os.Getenv(serveTokenFileEnv)
	}
	if err := httpapi.ValidateAuthPosture(flags.allowNonLoopback, tokenFile != "", flags.insecureNoAuth); err != nil {
		return cfg, err
	}
	if tokenFile != "" {
		auth, err := httpapi.NewTokenFileAuth(tokenFile)
		if err != nil {
			return cfg, fmt.Errorf("--auth-token-file: %w", err)
		}
		cfg.Auth = auth
	}

	for _, host := range flags.allowedHosts {
		if err := httpapi.ValidateAllowedHost(host); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}
