//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	doltembed "github.com/dolthub/driver/v2"
)

// validIdentifier matches safe SQL identifiers (letters, digits, underscores).
// Hyphens are excluded because database names are interpolated into system
// variable identifiers (@@<db>_head_ref) where hyphens are invalid.
var validIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

const (
	commitName  = "beads"
	commitEmail = "beads@local"
)

// OpenSQL opens an embedded Dolt database at dir. The returned cleanup
// function closes both the *sql.DB and the underlying connector.
func OpenSQL(ctx context.Context, dir, database, branch string) (*sql.DB, func() error, error) {
	connector, err := openConnector(buildDSN(dir, database))
	if err != nil {
		return nil, nil, err
	}

	db := sql.OpenDB(connector)
	configureConnectionPool(db)

	cleanup := func() error { return closeEmbeddedDatabase(db, connector) }

	if err := db.PingContext(ctx); err != nil {
		return nil, nil, errors.Join(err, cleanup())
	}
	if err := configureDatabase(ctx, db, database, branch); err != nil {
		return nil, nil, errors.Join(err, cleanup())
	}

	return db, cleanup, nil
}

func openConnector(dsn string) (*doltembed.Connector, error) {
	cfg, err := doltembed.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	bo := backoff.NewExponentialBackOff()
	bo.MaxElapsedTime = 0 // wait until ctx cancellation
	bo.MaxInterval = 5 * time.Second
	cfg.BackOff = bo
	return doltembed.NewConnector(cfg)
}

func configureConnectionPool(db *sql.DB) {
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)
}

func closeEmbeddedDatabase(db *sql.DB, connector *doltembed.Connector) error {
	dbErr := db.Close()
	connErr := connector.Close()
	// connector.Close → engine.Close → BackgroundThreads.Shutdown
	// always returns context.Canceled because Shutdown cancels its
	// own parent context then returns parentCtx.Err().  This is
	// a spurious error from a clean shutdown; filter it from each
	// result individually so real close errors are still surfaced.
	if errors.Is(dbErr, context.Canceled) {
		dbErr = nil
	}
	if errors.Is(connErr, context.Canceled) {
		connErr = nil
	}
	return errors.Join(dbErr, connErr)
}

func configureDatabase(ctx context.Context, db *sql.DB, database, branch string) error {
	if strings.TrimSpace(database) == "" {
		return nil
	}
	if err := validateDatabaseName(database); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "USE `"+database+"`"); err != nil {
		return err
	}
	if strings.TrimSpace(branch) == "" {
		return nil
	}
	_, err := db.ExecContext(ctx, fmt.Sprintf("SET @@%s_head_ref = %s", database, sqlStringLiteral(branch)))
	return err
}

func validateDatabaseName(database string) error {
	if validIdentifier.MatchString(database) {
		return nil
	}
	msg := fmt.Sprintf("invalid database name: %q", database)
	if strings.ContainsRune(database, '-') {
		msg += "; hyphens are not allowed in embedded mode — replace with underscores in .beads/metadata.json dolt_database field, or run 'bd doctor'"
	}
	return errors.New(msg)
}

func buildDSN(dir, database string) string {
	v := url.Values{}
	v.Set(doltembed.CommitNameParam, commitName)
	v.Set(doltembed.CommitEmailParam, commitEmail)
	v.Set(doltembed.MultiStatementsParam, "true")
	if strings.TrimSpace(database) != "" {
		v.Set(doltembed.DatabaseParam, database)
	}
	// Build the DSN string manually instead of using url.URL.String(),
	// which percent-encodes the path (spaces → %20). The embedded Dolt
	// driver's ParseDataSource strips the "file://" prefix and uses the
	// remainder as a literal filesystem path, so encoding breaks paths
	// that contain spaces. See #2920.
	path := dir
	if os.PathSeparator == '\\' {
		path = strings.ReplaceAll(path, `\`, `/`)
	}
	return "file://" + path + "?" + v.Encode()
}

func sqlStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(strings.TrimSpace(s), "'", "''") + "'"
}
