package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
	"github.com/jonbaldie/beads/internal/ui"
)

func testDoltConnection() error {
	beadsDir := selectedDoltBeadsDir()
	if beadsDir == "" {
		return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
	}

	cfg, err := loadDoltBackendConfig(beadsDir)
	if err != nil {
		return HandleError("%v", err)
	}

	host := cfg.GetDoltServerHost()
	port := doltserver.DefaultConfig(beadsDir).Port
	if isJSONOutput() {
		return renderDoltConnectionJSON(host, port)
	}

	fmt.Printf("Testing connection to %s:%d...\n", host, port)
	if !testServerConnection(host, port) {
		fmt.Printf("%s\n", ui.RenderWarn("✗ Connection failed"))
		fmt.Println("\nStart the server with: bd dolt start")
		return SilentExit()
	}
	fmt.Printf("%s\n", ui.RenderPass("✓ Connection successful"))
	return testDoltRemoteConnectivity()
}

// serverDialTimeout controls the TCP dial timeout for server connection tests.
// Tests may reduce this to avoid slow unreachable-host hangs in CI.
var serverDialTimeout = 3 * time.Second

func testServerConnection(host string, port int) bool {
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	_, err := doltserver.ProbeSQLServer("tcp", addr, serverDialTimeout)
	return err == nil
}

// extractSSHHost extracts the hostname from an SSH URL for connectivity testing.
func extractSSHHost(url string) string {
	// git+ssh://git@github.com/org/repo.git → github.com
	// ssh://git@github.com/org/repo.git → github.com
	// git@github.com:org/repo.git → github.com
	url = strings.TrimPrefix(url, "git+ssh://")
	url = strings.TrimPrefix(url, "ssh://")
	if idx := strings.Index(url, "@"); idx >= 0 {
		url = url[idx+1:]
	}
	// Handle colon-separated (git@host:path) or slash-separated (ssh://host/path)
	if idx := strings.Index(url, ":"); idx >= 0 && !strings.Contains(url[:idx], "/") {
		return url[:idx]
	}
	if idx := strings.Index(url, "/"); idx >= 0 {
		return url[:idx]
	}
	return url
}

// testSSHConnectivity tests if an SSH host is reachable on port 22.
// Bare dial+close (no doltserver.ProbeSQLServer): SSH, not MySQL — there is
// no handshake greeting to drain here.
func testSSHConnectivity(host string) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "22"), 5*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// httpURLToTCPAddr extracts a TCP dial address (host:port) from an HTTP(S) URL.
// Handles IPv6 addresses correctly (e.g., https://[::1]:8080/path).
func httpURLToTCPAddr(url string) string {
	host := url
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	if idx := strings.Index(host, "/"); idx >= 0 {
		host = host[:idx]
	}
	defaultPort := "443"
	if strings.HasPrefix(url, "http://") {
		defaultPort = "80"
	}
	// Use net.SplitHostPort to correctly handle IPv6 addresses (which
	// contain colons that would otherwise be confused with host:port).
	if h, p, err := net.SplitHostPort(host); err == nil {
		return net.JoinHostPort(h, p)
	}
	// No port in host string. Strip IPv6 brackets if present so
	// JoinHostPort can re-add them correctly.
	h := strings.TrimPrefix(host, "[")
	h = strings.TrimSuffix(h, "]")
	return net.JoinHostPort(h, defaultPort)
}

// testHTTPConnectivity tests if an HTTP(S) URL is reachable via TCP.
// Bare dial+close (no doltserver.ProbeSQLServer): HTTP(S), not MySQL — there
// is no handshake greeting to drain here.
func testHTTPConnectivity(url string) bool {
	addr := httpURLToTCPAddr(url)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// openDoltServerConnection opens a direct MySQL connection to the Dolt server
// using config from the beads directory. This bypasses getStore() which isn't
// initialized for dolt subcommands (beads-9vt). Connects without selecting a
// database so callers can operate on all databases (SHOW DATABASES, DROP DATABASE).
func openDoltServerConnection() (*sql.DB, func(), error) {
	beadsDir := selectedDoltBeadsDir()
	if beadsDir == "" {
		return nil, nil, HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
	}

	cfg, err := loadDoltBackendConfig(beadsDir)
	if err != nil {
		return nil, nil, HandleError("%v", err)
	}

	host := cfg.GetDoltServerHost()
	port := doltserver.DefaultConfig(beadsDir).Port
	user := cfg.GetDoltServerUser()
	password := os.Getenv("BEADS_DOLT_PASSWORD")

	connStr := doltutil.ServerDSN{
		Host:     host,
		Port:     port,
		User:     user,
		Password: password,
		TLS:      cfg.GetDoltServerTLS(),
	}.String()

	db, err := sql.Open("mysql", connStr)
	if err != nil {
		return nil, nil, HandleError("connecting to Dolt server: %v", err)
	}

	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Second)

	// Verify connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		fmt.Fprintf(os.Stderr, "Error: cannot reach Dolt server at %s:%d: %v\n", host, port, err)
		fmt.Fprintln(os.Stderr, "Start the server with: bd dolt start")
		return nil, nil, SilentExit()
	}

	return db, func() { _ = db.Close() }, nil
}

// doltServerPidFile returns the path to the PID file for the managed dolt server.
// logDoltConfigChange appends an audit entry to .beads/dolt-config.log.
// Includes the beadsDir path for debugging worktree config pollution (bd-la2cl).
func logDoltConfigChange(beadsDir, key, value string) {
	logPath := filepath.Join(beadsDir, "dolt-config.log")
	actor := os.Getenv("BEADS_ACTOR")
	if actor == "" {
		actor = os.Getenv("BD_ACTOR") // deprecated fallback
	}
	if actor == "" {
		actor = "unknown"
	}
	entry := fmt.Sprintf("%s actor=%s key=%s value=%s beads_dir=%s\n",
		time.Now().UTC().Format(time.RFC3339), actor, key, value, beadsDir)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600) //nolint:gosec // G304: fixed private log beneath the selected beads directory.
	if err != nil {
		return // best effort
	}
	defer f.Close()
	_, _ = f.WriteString(entry)
}
