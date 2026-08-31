package main

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestCleanupCandidateIndexesDropsByDefault(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, idx := range indexes {
		mock.ExpectExec(regexp.QuoteMeta(idx.Drop)).
			WillReturnResult(sqlmock.NewResult(0, 0))
	}

	if err := cleanupCandidateIndexes(context.Background(), db, false); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupCandidateIndexesHonorsKeepIndexes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := cleanupCandidateIndexes(context.Background(), db, true); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestParseBenchConfigDefaultsAndOverrides(t *testing.T) {
	defaults, err := parseBenchConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.concurrency != 64 || defaults.iterations != 2 || defaults.keepIndexes {
		t.Fatalf("defaults = %#v", defaults)
	}
	if defaults.dsn == "" {
		t.Fatal("default DSN is empty")
	}

	overrides, err := parseBenchConfig([]string{"--dsn", "custom", "--concurrency", "3", "--iterations", "4", "--keep-indexes"})
	if err != nil {
		t.Fatal(err)
	}
	if overrides != (benchConfig{dsn: "custom", concurrency: 3, iterations: 4, keepIndexes: true}) {
		t.Fatalf("overrides = %#v", overrides)
	}
	if _, err := parseBenchConfig([]string{"--concurrency", "not-a-number"}); err == nil {
		t.Fatal("malformed concurrency returned nil error")
	}
}

func TestOpenBenchDBReportsOpenAndPingFailures(t *testing.T) {
	original := sqlOpen
	t.Cleanup(func() { sqlOpen = original })
	openCause := errors.New("open failed")
	sqlOpen = func(string, string) (*sql.DB, error) { return nil, openCause }
	if _, err := openBenchDB(context.Background(), benchConfig{}); !errors.Is(err, openCause) {
		t.Fatalf("open error = %v, want %v", err, openCause)
	}

	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	pingCause := errors.New("ping failed")
	mock.ExpectPing().WillReturnError(pingCause)
	mock.ExpectClose()
	sqlOpen = func(string, string) (*sql.DB, error) { return db, nil }
	if _, err := openBenchDB(context.Background(), benchConfig{concurrency: 3}); !errors.Is(err, pingCause) {
		t.Fatalf("ping error = %v, want %v", err, pingCause)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInstallIndexStateNamesEveryInstalledIndexAndWrapsFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if name, err := installIndexState(context.Background(), db, nil); err != nil || name != "baseline" {
		t.Fatalf("baseline = %q, %v", name, err)
	}
	state := indexes[:2]
	for _, idx := range state {
		mock.ExpectExec(regexp.QuoteMeta(idx.SQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	name, err := installIndexState(context.Background(), db, state)
	if err != nil {
		t.Fatal(err)
	}
	if want := state[0].Name + "+" + state[1].Name; name != want {
		t.Fatalf("state name = %q, want %q", name, want)
	}

	cause := errors.New("create failed")
	mock.ExpectExec(regexp.QuoteMeta(state[0].SQL)).WillReturnError(cause)
	if _, err := installIndexState(context.Background(), db, state[:1]); !errors.Is(err, cause) {
		t.Fatalf("install error = %v, want wrapped %v", err, cause)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBenchmarkStatesIncludeBaselineSinglesAndAll(t *testing.T) {
	states := benchmarkStates()
	if len(states) != len(indexes)+2 {
		t.Fatalf("states = %d, want %d", len(states), len(indexes)+2)
	}
	if states[0] != nil {
		t.Fatalf("first state = %#v, want baseline", states[0])
	}
	for i, idx := range indexes {
		if len(states[i+1]) != 1 || states[i+1][0] != idx {
			t.Fatalf("state %d = %#v, want %#v", i+1, states[i+1], idx)
		}
	}
	if len(states[len(states)-1]) != len(indexes) {
		t.Fatalf("all-index state length = %d, want %d", len(states[len(states)-1]), len(indexes))
	}
}

func TestRunCleansUpCandidateIndexesAfterCreateFailure(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}

	oldSQLOpen := sqlOpen
	sqlOpen = func(driverName, dataSourceName string) (*sql.DB, error) {
		if driverName != "mysql" {
			t.Fatalf("driverName = %q, want mysql", driverName)
		}
		if dataSourceName != "sqlmock-dsn" {
			t.Fatalf("dataSourceName = %q, want sqlmock-dsn", dataSourceName)
		}
		return db, nil
	}
	t.Cleanup(func() {
		sqlOpen = oldSQLOpen
	})

	mock.ExpectPing()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM issues WHERE status IN ('open','in_progress') ORDER BY priority ASC, created_at DESC, id ASC LIMIT 100`)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("bd-a"))

	expectDropAll(mock) // baseline
	for _, idx := range indexes {
		expectDropAll(mock)
		mock.ExpectExec(regexp.QuoteMeta(idx.SQL)).
			WillReturnResult(sqlmock.NewResult(0, 0))
	}
	expectDropAll(mock) // all-indexes state starts from a clean slate
	mock.ExpectExec(regexp.QuoteMeta(indexes[0].SQL)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(indexes[1].SQL)).
		WillReturnError(errors.New("create failed"))
	expectDropAll(mock) // deferred cleanup still runs after the create error
	mock.ExpectClose()

	err = runWithArgs(context.Background(), []string{
		"--dsn", "sqlmock-dsn",
		"--concurrency", "0",
		"--iterations", "1",
	})
	if err == nil {
		t.Fatal("expected create-index error")
	}
	if !strings.Contains(err.Error(), "create "+indexes[1].Name) {
		t.Fatalf("error = %v, want create failure for %s", err, indexes[1].Name)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildQueriesUsesTypedDependencyTargetProjection(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM issues WHERE status IN ('open','in_progress') ORDER BY priority ASC, created_at DESC, id ASC LIMIT 100`)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("bd-a"))

	queries, err := buildQueries(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, qc := range queries {
		if qc.Name != "candidate_blocking_deps" {
			continue
		}
		found = true
		if !strings.Contains(qc.Query, dependencyTargetExpr) {
			t.Fatalf("candidate query = %q, want typed target projection", qc.Query)
		}
		if strings.Contains(qc.Query, "depends_on"+"_id") {
			t.Fatalf("candidate query still references generated column: %q", qc.Query)
		}
	}
	if !found {
		t.Fatal("candidate_blocking_deps query not found")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectDropAll(mock sqlmock.Sqlmock) {
	for _, idx := range indexes {
		mock.ExpectExec(regexp.QuoteMeta(idx.Drop)).
			WillReturnResult(sqlmock.NewResult(0, 0))
	}
}
