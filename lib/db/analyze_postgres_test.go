package db_test

import (
	"bytes"
	"database/sql"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
)

// These tests need a throwaway Postgres: set CONV_TEST_POSTGRES_DSN to a
// libpq connection string (for example
// "host=localhost port=5432 user=u password=p dbname=scratch sslmode=disable").
// The module does not depend on a Postgres driver, so the test binary must
// link one under the name "postgres" or "pgx" (a temporary blank import in an
// untracked _test.go file is enough). They create and drop their own tables.

const pgDSNEnv = "CONV_TEST_POSTGRES_DSN"

func pgOpen(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set", pgDSNEnv)
	}
	name := ""
	for _, d := range sql.Drivers() {
		if d == "postgres" || d == "pgx" {
			name = d
		}
	}
	if name == "" {
		t.Fatalf("%s is set but no \"postgres\" or \"pgx\" database/sql driver is linked", pgDSNEnv)
	}
	db, err := sql.Open(name, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return db
}

func pgExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// pgTable creates a plain convention-shaped table with rows and registers its drop.
func pgTable(t *testing.T, db *sql.DB, table string, rows int) {
	t.Helper()
	pgExec(t, db, `DROP TABLE IF EXISTS "`+table+`"`)
	pgExec(t, db, `CREATE TABLE "`+table+`" ("id" text PRIMARY KEY, "object" JSONB NULL)`)
	t.Cleanup(func() { db.Exec(`DROP TABLE IF EXISTS "` + table + `"`) })
	if rows > 0 {
		pgExec(t, db, `INSERT INTO "`+table+`" ("id","object")
SELECT g::text, jsonb_build_object('k', 'v' || (g % 10), 'a', g % 7, 'b', g % 3, 'tags', jsonb_build_array('t' || (g % 5)))
FROM generate_series(1, $1::int) g`, rows)
	}
}

func pgHasStats(t *testing.T, db *sql.DB, index string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pg_stats WHERE tablename = $1`, index).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// pgLastAnalyze waits for the statistics collector to catch up, then reads the
// time of the table's last manual ANALYZE.
func pgLastAnalyze(t *testing.T, db *sql.DB, table string, wantSet bool) sql.NullTime {
	t.Helper()
	var last sql.NullTime
	for i := 0; i < 30; i++ {
		if err := db.QueryRow(`SELECT last_analyze FROM pg_stat_user_tables WHERE relname = $1`, table).Scan(&last); err != nil {
			t.Fatal(err)
		}
		if last.Valid == wantSet {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	return last
}

func pgCtx() (convCtx.Context, *bytes.Buffer) {
	var buf bytes.Buffer
	return convCtx.New(convAuth.Claims{User: "analyze-pg-test"}).
		WithLogger(slog.New(slog.NewJSONHandler(&buf, nil))), &buf
}

func Test_analyze_postgres_expressionIndexesGetStatisticsOnceAndOnlyOnce(t *testing.T) {
	db := pgOpen(t)
	cases := []struct{ name, index string }{
		{"btree", `CREATE INDEX "pg_an_idx" ON "pg_an_t" (("object"->'k'))`},
		{"gin_jsonb_path_ops_array", `CREATE INDEX "pg_an_idx" ON "pg_an_t" USING gin (("object"->'tags') jsonb_path_ops)`},
		{"multi_expression", `CREATE INDEX "pg_an_idx" ON "pg_an_t" (("object"->'a'), ("object"->'b'))`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, logs := pgCtx()
			pgTable(t, db, "pg_an_t", 500)
			pgExec(t, db, c.index)
			if pgHasStats(t, db, "pg_an_idx") {
				t.Fatal("index has statistics before the call")
			}

			if err := convDB.AnalyzeTablesMissingExpressionStats(ctx, db, "pg_an_t"); err != nil {
				t.Fatal(err)
			}
			if !pgHasStats(t, db, "pg_an_idx") {
				t.Fatalf("no statistics after the call; logs: %s", logs.String())
			}
			first := pgLastAnalyze(t, db, "pg_an_t", true)
			if !first.Valid {
				t.Fatal("last_analyze not set after the call")
			}

			time.Sleep(1500 * time.Millisecond)
			if err := convDB.AnalyzeTablesMissingExpressionStats(ctx, db, "pg_an_t"); err != nil {
				t.Fatal(err)
			}
			time.Sleep(1500 * time.Millisecond)
			second := pgLastAnalyze(t, db, "pg_an_t", true)
			if !second.Time.Equal(first.Time) {
				t.Fatalf("second run analysed again: %v -> %v", first.Time, second.Time)
			}
			if logs.Len() != 0 {
				t.Errorf("unexpected log output: %s", logs.String())
			}
		})
	}
}

func Test_analyze_postgres_emptyTableAndUnmatchedPartialIndexAreAnalysedEachRunWithoutError(t *testing.T) {
	db := pgOpen(t)
	ctx, logs := pgCtx()

	pgTable(t, db, "pg_an_empty", 0)
	pgExec(t, db, `CREATE INDEX "pg_an_empty_idx" ON "pg_an_empty" (("object"->'k'))`)

	pgTable(t, db, "pg_an_partial", 100)
	pgExec(t, db, `CREATE INDEX "pg_an_partial_idx" ON "pg_an_partial" (("object"->'k')) WHERE "object"->>'k' = 'never'`)

	for run := 0; run < 2; run++ {
		if err := convDB.AnalyzeTablesMissingExpressionStats(ctx, db, "pg_an_empty", "pg_an_partial"); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	if pgHasStats(t, db, "pg_an_empty_idx") || pgHasStats(t, db, "pg_an_partial_idx") {
		t.Error("expected no statistics for an empty table or an unmatched partial index")
	}
	if !pgLastAnalyze(t, db, "pg_an_empty", true).Valid || !pgLastAnalyze(t, db, "pg_an_partial", true).Valid {
		t.Error("both tables should have been analysed")
	}
	if logs.Len() != 0 {
		t.Errorf("unexpected log output: %s", logs.String())
	}
}

func Test_analyze_postgres_invalidIndexIsIgnored(t *testing.T) {
	db := pgOpen(t)
	ctx, logs := pgCtx()

	pgTable(t, db, "pg_an_invalid", 100)
	// Duplicate values make a concurrent unique build fail and leave an INVALID index.
	if _, err := db.Exec(`CREATE UNIQUE INDEX CONCURRENTLY "pg_an_invalid_idx" ON "pg_an_invalid" (("object"->'k'))`); err == nil {
		t.Fatal("expected the unique build to fail")
	}
	var valid bool
	if err := db.QueryRow(`SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('"pg_an_invalid_idx"')`).Scan(&valid); err != nil || valid {
		t.Fatalf("expected an invalid index, valid=%v err=%v", valid, err)
	}

	if err := convDB.AnalyzeTablesMissingExpressionStats(ctx, db, "pg_an_invalid"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if pgLastAnalyze(t, db, "pg_an_invalid", false).Valid {
		t.Fatal("the table was analysed because of an invalid index")
	}
	if logs.Len() != 0 {
		t.Errorf("unexpected log output: %s", logs.String())
	}
}

func Test_analyze_postgres_lockedTableTimesOutWithWarningAndStaysUsable(t *testing.T) {
	db := pgOpen(t)
	ctx, logs := pgCtx()

	pgTable(t, db, "pg_an_locked", 100)
	pgExec(t, db, `CREATE INDEX "pg_an_locked_idx" ON "pg_an_locked" (("object"->'k'))`)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`LOCK TABLE "pg_an_locked" IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err = convDB.AnalyzeTablesMissingExpressionStats(ctx, db, "pg_an_locked")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("a timeout must not be returned: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the check waited %v for the lock", elapsed)
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Errorf("no warning logged: %s", logs.String())
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM "pg_an_locked"`).Scan(&n); err != nil || n != 100 {
		t.Fatalf("table unusable: n=%d err=%v", n, err)
	}
	var setting string
	if err := db.QueryRow(`SHOW lock_timeout`).Scan(&setting); err != nil || setting != "0" {
		t.Errorf("lock_timeout leaked to the pool: %q %v", setting, err)
	}
	if err := convDB.AnalyzeTablesMissingExpressionStats(ctx, db, "pg_an_locked"); err != nil {
		t.Fatal(err)
	}
	if !pgHasStats(t, db, "pg_an_locked_idx") {
		t.Error("next run did not analyse the table")
	}
}

type PgAnalyzeA struct {
	ID string `json:"id"`
	K  string `json:"k"`
}

func (o PgAnalyzeA) DBKey() convDB.Key[string, string] {
	return convDB.Key[string, string]{ID: o.ID, ShardKey: o.ID}
}

var pgAnalyzeADB = convDB.NewObjectSet[PgAnalyzeA]("pganalyze").WithIndexes("k").Ready()

func pgUseVault(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, tb := range []string{"pg_analyze_a", "pg_analyze_a_history", "pg_analyze_a_lock"} {
		pgExec(t, db, `DROP TABLE IF EXISTS "`+tb+`"`)
		tb := tb
		t.Cleanup(func() { db.Exec(`DROP TABLE IF EXISTS "` + tb + `"`) })
	}
	convDB.UseDBForTest(t, "pganalyze", "test", db, convDB.EnginePostgres)
}

func pgAnalyzeAIndex(t *testing.T, db *sql.DB) string {
	t.Helper()
	var name string
	err := db.QueryRow(`SELECT c.relname FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
WHERE i.indrelid = to_regclass('pg_analyze_a') AND i.indexprs IS NOT NULL`).Scan(&name)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func Test_prepare_postgres_analysesOnFirstUseAfterStartAndConcurrentPreparesBothSucceed(t *testing.T) {
	db := pgOpen(t)
	pgUseVault(t, db)
	ctx := convCtx.New(convAuth.Claims{User: "prepare-pg-test"})

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- pgAnalyzeADB.Tenant("test").EnsurePrepared()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("EnsurePrepared: %v", err)
		}
	}

	for i := 0; i < 50; i++ {
		if err := pgAnalyzeADB.Tenant("test").Insert(ctx, PgAnalyzeA{ID: string(rune('a'+i%26)) + string(rune('A'+i/26)), K: "v"}); err != nil {
			t.Fatal(err)
		}
	}

	idx := pgAnalyzeAIndex(t, db)
	if pgHasStats(t, db, idx) {
		t.Fatal("the index has statistics before the first read of a fresh process")
	}

	// A fresh process (empty registry) that only reads one table: the first
	// read prepares the table and the check analyses it.
	convDB.UseDBForTest(t, "pganalyze", "test", db, convDB.EnginePostgres)
	if _, err := pgAnalyzeADB.Tenant("test").SelectByID(ctx, "aA"); err != nil {
		t.Fatalf("SelectByID: %v", err)
	}
	if !pgHasStats(t, db, idx) {
		t.Fatal("the first read did not analyse the table")
	}
}

func Test_analyze_postgres_indexCreatedByHandAfterPrepareIsFoundByDirectCall(t *testing.T) {
	db := pgOpen(t)
	pgUseVault(t, db)
	ctx, logs := pgCtx()

	if err := pgAnalyzeADB.Tenant("test").EnsurePrepared(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		pgExec(t, db, `INSERT INTO "pg_analyze_a" ("id","created_at","created_by","updated_at","updated_by","object")
VALUES ($1, now(), 'u', now(), 'u', jsonb_build_object('k','v','tags',jsonb_build_array('x')))`, i)
	}
	pgExec(t, db, `CREATE INDEX "pg_analyze_a_hand" ON "pg_analyze_a" USING gin (("object"->'tags') jsonb_path_ops)`)
	if pgHasStats(t, db, "pg_analyze_a_hand") {
		t.Fatal("hand-made index already has statistics")
	}

	dbsRaw, err := pgAnalyzeADB.Tenant("test").RawDBs()
	if err != nil || len(dbsRaw) != 1 {
		t.Fatalf("RawDBs: %v %v", dbsRaw, err)
	}
	if err := convDB.AnalyzeTablesMissingExpressionStats(ctx, dbsRaw[0], "pg_analyze_a", "pg_analyze_a_history"); err != nil {
		t.Fatal(err)
	}
	if !pgHasStats(t, db, "pg_analyze_a_hand") {
		t.Fatalf("hand-made index has no statistics; logs: %s", logs.String())
	}
}
