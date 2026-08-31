package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
)

func runSQLProxiedServer(ctx context.Context, query string, csvOutput bool) error {
	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}
	multiStatement := isMultiStatementSQL(query)
	if !multiStatement && sqlQueryIsRead(query) {
		return runSQLProxiedRead(ctx, query, csvOutput)
	}
	return runSQLProxiedExec(ctx, query, multiStatement)
}

func runSQLProxiedRead(ctx context.Context, query string, csvOutput bool) error {
	result, err := uow.RunTxRead(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (*domain.RawSQLResult, error) {
		return uw.RawSQLUseCase().Query(ctx, query)
	})
	if err != nil {
		return HandleErrorRespectJSON("query error: %v", err)
	}
	return renderRawSQLResult(result, csvOutput)
}

func runSQLProxiedExec(ctx context.Context, query string, multiStatement bool) error {
	if err := CheckReadonly("sql"); err != nil {
		return err
	}
	affected, err := uow.RunTxResult(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (int64, string, error) {
		affected, err := uw.RawSQLUseCase().Exec(ctx, query)
		if err != nil {
			return 0, "", err
		}
		return affected, "bd sql: " + query, nil
	})
	if err != nil {
		return HandleErrorRespectJSON("exec error: %v", err)
	}
	return renderSQLExecResult(affected, multiStatement)
}

func renderSQLExecResult(affected int64, multiStatement bool) error {
	if multiStatement {
		if isJSONOutput() {
			return outputJSON(map[string]interface{}{"status": "ok"})
		}
		fmt.Println("OK")
		return nil
	}
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{"rows_affected": affected})
	}
	fmt.Printf("OK, %d rows affected\n", affected)
	return nil
}

func isMultiStatementSQL(query string) bool {
	return topLevelStatementCount(query) > 1
}

type sqlScanState struct {
	i          int
	count      int
	hasContent bool
}

func topLevelStatementCount(query string) int {
	s := sqlScanState{}
	n := len(query)
	for s.i < n {
		advanceSQLScan(query, n, &s)
	}
	if s.hasContent {
		s.count++
	}
	return s.count
}

func advanceSQLScan(query string, n int, s *sqlScanState) {
	c := query[s.i]
	if isSQLQuote(c) {
		skipSQLQuoted(query, n, s, c)
		return
	}
	if c == '-' {
		skipSQLDash(query, n, s)
		return
	}
	if c == '#' {
		skipSQLLineComment(query, n, s)
		return
	}
	if c == '/' {
		skipSQLSlash(query, n, s)
		return
	}
	if c == ';' {
		finishSQLStatement(s)
		return
	}
	if isSQLSpace(c) {
		s.i++
		return
	}
	s.hasContent = true
	s.i++
}

func isSQLQuote(c byte) bool {
	return c == '\'' || c == '"' || c == '`'
}

func isSQLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func skipSQLQuoted(query string, n int, s *sqlScanState, quote byte) {
	s.i++
	for s.i < n {
		if query[s.i] == '\\' && quote != '`' {
			s.i += 2
			continue
		}
		if query[s.i] != quote {
			s.i++
			continue
		}
		if s.i+1 < n && query[s.i+1] == quote {
			s.i += 2
			continue
		}
		s.i++
		break
	}
	s.hasContent = true
}

func skipSQLDash(query string, n int, s *sqlScanState) {
	if isSQLDashComment(query, n, s.i) {
		skipSQLLineComment(query, n, s)
		return
	}
	s.hasContent = true
	s.i++
}

func isSQLDashComment(query string, n, i int) bool {
	if i+1 >= n || query[i+1] != '-' {
		return false
	}
	if i+2 >= n {
		return true
	}
	return isSQLSpace(query[i+2])
}

func skipSQLLineComment(query string, n int, s *sqlScanState) {
	for s.i < n && query[s.i] != '\n' && query[s.i] != '\r' {
		s.i++
	}
}

func skipSQLSlash(query string, n int, s *sqlScanState) {
	if s.i+1 < n && query[s.i+1] == '*' {
		s.i += 2
		for s.i+1 < n && !(query[s.i] == '*' && query[s.i+1] == '/') {
			s.i++
		}
		s.i += 2
		return
	}
	s.hasContent = true
	s.i++
}

func finishSQLStatement(s *sqlScanState) {
	if s.hasContent {
		s.count++
		s.hasContent = false
	}
	s.i++
}

func sqlQueryIsRead(query string) bool {
	trimmed := strings.TrimSpace(strings.ToUpper(query))
	switch {
	case strings.HasPrefix(trimmed, "SELECT"),
		strings.HasPrefix(trimmed, "EXPLAIN"),
		strings.HasPrefix(trimmed, "PRAGMA"),
		strings.HasPrefix(trimmed, "SHOW"),
		strings.HasPrefix(trimmed, "DESCRIBE"):
		return true
	case strings.HasPrefix(trimmed, "WITH"):
		return withOuterStatementIsRead(trimmed)
	default:
		return false
	}
}

func withOuterStatementIsRead(upperTrimmed string) bool {
	s := withOuterScanState{}
	n := len(upperTrimmed)
	for i := 0; i < n; i++ {
		if done, isRead := s.advance(upperTrimmed, i); done {
			return isRead
		}
	}
	return true
}

type withOuterScanState struct {
	depth     int
	quote     byte
	closedCTE bool
}

func (s *withOuterScanState) advance(upperTrimmed string, i int) (done bool, isRead bool) {
	c := upperTrimmed[i]
	if s.quote != 0 {
		s.quote = updateSQLQuote(s.quote, c)
		return false, false
	}
	if isSQLQuote(c) {
		s.quote = c
		return false, false
	}
	return s.advanceUnquoted(upperTrimmed, i, c)
}

func (s *withOuterScanState) advanceUnquoted(upperTrimmed string, i int, c byte) (bool, bool) {
	if c == '(' || c == ')' || isSQLSpace(c) || c == ',' {
		s.applyWithOuterPunct(c)
		return false, false
	}
	if s.depth != 0 || !s.closedCTE {
		return false, false
	}
	rest := strings.TrimLeft(upperTrimmed[i:], " \t\n\r")
	return true, strings.HasPrefix(rest, "SELECT") || strings.HasPrefix(rest, "EXPLAIN")
}

func (s *withOuterScanState) applyWithOuterPunct(c byte) {
	switch c {
	case '(':
		s.depth++
	case ')':
		s.depth--
		if s.depth == 0 {
			s.closedCTE = true
		}
	case ',':
		if s.depth == 0 {
			s.closedCTE = false
		}
	}
}

func updateSQLQuote(quote, c byte) byte {
	if c == quote {
		return 0
	}
	return quote
}

func renderRawSQLResult(result *domain.RawSQLResult, csvOutput bool) error {
	if isJSONOutput() {
		return renderRawSQLJSON(result)
	}
	if csvOutput {
		return renderRawSQLCSV(result)
	}
	return renderRawSQLTable(result)
}

func renderRawSQLJSON(result *domain.RawSQLResult) error {
	columns := result.Columns
	out := make([]map[string]interface{}, 0, len(result.Rows))
	for _, row := range result.Rows {
		m := make(map[string]interface{}, len(columns))
		for i, col := range columns {
			m[col] = row[i]
		}
		out = append(out, m)
	}
	return outputJSON(out)
}

func renderRawSQLCSV(result *domain.RawSQLResult) error {
	columns := result.Columns
	w := csv.NewWriter(os.Stdout)
	if err := w.Write(columns); err != nil {
		return HandleErrorRespectJSON("writing CSV header: %v", err)
	}
	for _, row := range result.Rows {
		record := make([]string, len(columns))
		for i := range columns {
			record[i] = fmt.Sprintf("%v", row[i])
		}
		if err := w.Write(record); err != nil {
			return HandleErrorRespectJSON("writing CSV row: %v", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return HandleErrorRespectJSON("flushing CSV: %v", err)
	}
	return nil
}

func renderRawSQLTable(result *domain.RawSQLResult) error {
	if len(result.Rows) == 0 {
		fmt.Println("(0 rows)")
		return nil
	}
	widths := sqlTableWidths(result)
	printSQLTableHeader(result.Columns, widths)
	printSQLTableRows(result, widths)
	fmt.Printf("(%d rows)\n", len(result.Rows))
	return nil
}

func sqlTableWidths(result *domain.RawSQLResult) []int {
	widths := make([]int, len(result.Columns))
	for i, col := range result.Columns {
		widths[i] = len(col)
	}
	for _, row := range result.Rows {
		for i := range result.Columns {
			n := len(fmt.Sprintf("%v", row[i]))
			if n > widths[i] {
				widths[i] = n
			}
		}
	}
	for i := range widths {
		if widths[i] > 60 {
			widths[i] = 60
		}
	}
	return widths
}

func printSQLTableHeader(columns []string, widths []int) {
	printSQLTableCells(columns, widths, " | ", false)
	fmt.Println()
	seps := make([]string, len(columns))
	for i := range columns {
		seps[i] = strings.Repeat("-", widths[i])
	}
	printSQLTableCells(seps, widths, "-+-", false)
	fmt.Println()
}

func printSQLTableRows(result *domain.RawSQLResult, widths []int) {
	for _, row := range result.Rows {
		cells := make([]string, len(result.Columns))
		for i := range result.Columns {
			cells[i] = fmt.Sprintf("%v", row[i])
		}
		printSQLTableCells(cells, widths, " | ", true)
		fmt.Println()
	}
}

func printSQLTableCells(cells []string, widths []int, sep string, truncate bool) {
	for i, cell := range cells {
		if i > 0 {
			fmt.Print(sep)
		}
		if truncate && len(cell) > 60 {
			cell = cell[:57] + "..."
		}
		fmt.Printf("%-*s", widths[i], cell)
	}
}
