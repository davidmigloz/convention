package db

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
)

// fakePG is a database/sql driver that records every statement it receives and
// answers the statistics query from a map. Its type name does not mention
// sqlite, so the function under test treats it as Postgres.
type fakePG struct {
	mu          sync.Mutex
	events      []string
	missing     map[string]bool // table argument (quoted) -> the query answers true
	queryErr    error
	failAnalyze map[string]bool // statement text -> Exec fails
}

func (f *fakePG) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, s)
}

func (f *fakePG) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

type fakePGConnector struct{ f *fakePG }

func (c fakePGConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakePGConn{f: c.f}, nil
}
func (c fakePGConnector) Driver() driver.Driver { return fakePGDriver{} }

type fakePGDriver struct{}

func (fakePGDriver) Open(string) (driver.Conn, error) { return nil, errors.New("unused") }

type fakePGConn struct{ f *fakePG }

func (c *fakePGConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *fakePGConn) Close() error                        { return nil }
func (c *fakePGConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *fakePGConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.f.record("begin")
	return &fakePGTx{f: c.f}, nil
}
func (c *fakePGConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.f.record("exec: " + q)
	if c.f.failAnalyze[q] {
		return nil, errors.New("canceling statement due to lock timeout")
	}
	return driver.RowsAffected(0), nil
}
func (c *fakePGConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	arg, _ := args[0].Value.(string)
	c.f.record("query: " + arg)
	if c.f.queryErr != nil {
		return nil, c.f.queryErr
	}
	return &fakePGRows{v: c.f.missing[arg]}, nil
}

type fakePGTx struct{ f *fakePG }

func (t *fakePGTx) Commit() error   { t.f.record("commit"); return nil }
func (t *fakePGTx) Rollback() error { t.f.record("rollback"); return nil }

type fakePGRows struct {
	v    bool
	done bool
}

func (r *fakePGRows) Columns() []string { return []string{"exists"} }
func (r *fakePGRows) Close() error      { return nil }
func (r *fakePGRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.v
	return nil
}

func newFakePG(f *fakePG) *sql.DB { return sql.OpenDB(fakePGConnector{f: f}) }

func logCtx() (convCtx.Context, *bytes.Buffer) {
	var buf bytes.Buffer
	ctx := convCtx.New(convAuth.Claims{User: "analyze-test"}).
		WithLogger(slog.New(slog.NewJSONHandler(&buf, nil)))
	return ctx, &buf
}

func Test_quoteIdentifier(t *testing.T) {
	for in, want := range map[string]string{
		"entity":   `"entity"`,
		`we"ird`:   `"we""ird"`,
		"a b;drop": `"a b;drop"`,
		"":         `""`,
	} {
		if got := quoteIdentifier(in); got != want {
			t.Errorf("quoteIdentifier(%q) = %s, want %s", in, got, want)
		}
	}
}

func Test_analyzeStatement_quotesTable(t *testing.T) {
	if got, want := analyzeStatement(`en"tity`), `ANALYZE "en""tity"`; got != want {
		t.Fatalf("analyzeStatement = %s, want %s", got, want)
	}
}

func Test_missingExpressionStatsQuery_shape(t *testing.T) {
	q := missingExpressionStatsQuery
	for _, part := range []string{
		"pg_index", "indexprs IS NOT NULL", "indisvalid", "pg_stats", "NOT EXISTS", "to_regclass($1",
	} {
		if !strings.Contains(q, part) {
			t.Errorf("catalog query lacks %q:\n%s", part, q)
		}
	}
	if strings.Contains(strings.ToUpper(q), "ANALYZE") {
		t.Errorf("catalog query must only read:\n%s", q)
	}
}

func Test_analyzeTimeouts_areShortAndBounded(t *testing.T) {
	if analyzeLockTimeout <= 0 || analyzeLockTimeout > 5*time.Second {
		t.Errorf("lock timeout %v not short", analyzeLockTimeout)
	}
	if analyzeStatementTimeout < analyzeLockTimeout || analyzeStatementTimeout > time.Minute {
		t.Errorf("statement timeout %v out of range", analyzeStatementTimeout)
	}
}

func Test_analyze_sendsExpectedStatementsPerTable(t *testing.T) {
	f := &fakePG{missing: map[string]bool{`"entity"`: true}}
	ctx, logs := logCtx()

	if err := AnalyzeTablesMissingExpressionStats(ctx, newFakePG(f), "entity", "other"); err != nil {
		t.Fatalf("err = %v", err)
	}

	want := []string{
		"begin",
		"exec: SET LOCAL lock_timeout = '2000ms'",
		"exec: SET LOCAL statement_timeout = '30000ms'",
		`query: "entity"`,
		"commit",
		"begin",
		"exec: SET LOCAL lock_timeout = '2000ms'",
		"exec: SET LOCAL statement_timeout = '30000ms'",
		`exec: ANALYZE "entity"`,
		"commit",
		"begin",
		"exec: SET LOCAL lock_timeout = '2000ms'",
		"exec: SET LOCAL statement_timeout = '30000ms'",
		`query: "other"`,
		"commit",
	}
	if got := f.snapshot(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("statements:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if logs.Len() != 0 {
		t.Errorf("unexpected log output: %s", logs.String())
	}
}

func Test_analyze_quotesTableNameInQueryArgumentAndStatement(t *testing.T) {
	f := &fakePG{missing: map[string]bool{`"we""ird"`: true}}
	ctx, _ := logCtx()

	_ = AnalyzeTablesMissingExpressionStats(ctx, newFakePG(f), `we"ird`)

	got := strings.Join(f.snapshot(), "\n")
	if !strings.Contains(got, `query: "we""ird"`) || !strings.Contains(got, `exec: ANALYZE "we""ird"`) {
		t.Fatalf("identifier not quoted:\n%s", got)
	}
}

func Test_analyze_failureIsLoggedAsWarningAndNextTableStillRuns(t *testing.T) {
	f := &fakePG{
		missing:     map[string]bool{`"a"`: true, `"b"`: true},
		failAnalyze: map[string]bool{`ANALYZE "a"`: true},
	}
	ctx, logs := logCtx()

	if err := AnalyzeTablesMissingExpressionStats(ctx, newFakePG(f), "a", "b"); err != nil {
		t.Fatalf("a failed ANALYZE must not be returned, got %v", err)
	}

	got := strings.Join(f.snapshot(), "\n")
	if !strings.Contains(got, "rollback") {
		t.Errorf("the failed transaction was not rolled back:\n%s", got)
	}
	if !strings.Contains(got, `exec: ANALYZE "b"`) {
		t.Errorf("table b was skipped after a failed:\n%s", got)
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Errorf("no warning logged: %s", logs.String())
	}
}

func Test_analyze_catalogQueryFailureIsLoggedAndReturnsNil(t *testing.T) {
	f := &fakePG{queryErr: errors.New("permission denied for table pg_index")}
	ctx, logs := logCtx()

	if err := AnalyzeTablesMissingExpressionStats(ctx, newFakePG(f), "a"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(strings.Join(f.snapshot(), "\n"), "ANALYZE") {
		t.Errorf("ANALYZE sent although the check failed")
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Errorf("no warning logged: %s", logs.String())
	}
}

func Test_analyze_isNoOpOnSQLite(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, logs := logCtx()

	if err := AnalyzeTablesMissingExpressionStats(ctx, db, "entity"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if logs.Len() != 0 {
		t.Errorf("SQLite must be silent, got: %s", logs.String())
	}
}

func Test_analyze_noTablesSendsNothing(t *testing.T) {
	f := &fakePG{}
	ctx, _ := logCtx()
	_ = AnalyzeTablesMissingExpressionStats(ctx, newFakePG(f))
	if got := f.snapshot(); len(got) != 0 {
		t.Fatalf("statements sent for no tables: %v", got)
	}
}

func Test_quoteRelation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"events", `"events"`},
		{"tenant_a.events", `"tenant_a"."events"`},
		{`we"ird`, `"we""ird"`},
		{`sch"ema.ta"ble`, `"sch""ema"."ta""ble"`},
		{"a.b.c", `"a"."b.c"`},
	}
	for _, c := range cases {
		if got := quoteRelation(c.in); got != c.want {
			t.Errorf("quoteRelation(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func Test_analyze_schemaQualifiedTableUsesSameRelationInQueryAndStatement(t *testing.T) {
	f := &fakePG{missing: map[string]bool{`"tenant_a"."events"`: true}}
	ctx, _ := logCtx()

	_ = AnalyzeTablesMissingExpressionStats(ctx, newFakePG(f), "tenant_a.events")

	got := strings.Join(f.snapshot(), "\n")
	if !strings.Contains(got, `query: "tenant_a"."events"`) || !strings.Contains(got, `exec: ANALYZE "tenant_a"."events"`) {
		t.Fatalf("relation not schema-qualified:\n%s", got)
	}
}
