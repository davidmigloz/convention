package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
)

const (
	// analyzeLockTimeout bounds the wait for a table lock, so a long DDL or a
	// held lock on the table makes the check give up instead of queueing.
	analyzeLockTimeout = 2 * time.Second
	// analyzeStatementTimeout bounds the catalog query and each ANALYZE.
	analyzeStatementTimeout = 30 * time.Second

	// analyzeClientSlack is how long past the server-side timeouts the client
	// waits before it abandons a transaction whose server went silent.
	analyzeClientSlack = 5 * time.Second

	// An expression index keeps its planner statistics under the index's own
	// name in pg_stats, and only an ANALYZE of its table creates them. $1 is
	// the quoted relation name, resolved through the search path like the DDL.
	missingExpressionStatsQuery = `SELECT EXISTS (
SELECT 1
FROM pg_index i
JOIN pg_class ic ON ic.oid = i.indexrelid
JOIN pg_namespace n ON n.oid = ic.relnamespace
WHERE i.indrelid = to_regclass($1::text)
AND i.indexprs IS NOT NULL
AND i.indisvalid
AND NOT EXISTS (
SELECT 1 FROM pg_stats s
WHERE s.schemaname = n.nspname AND s.tablename = ic.relname
))`
)

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteRelation quotes "table" or "schema.table" component by component, so a
// schema-qualified name stays a qualified relation. Only the first "." splits.
func quoteRelation(name string) string {
	if schema, table, ok := strings.Cut(name, "."); ok {
		return quoteIdentifier(schema) + "." + quoteIdentifier(table)
	}
	return quoteIdentifier(name)
}

func analyzeStatement(table string) string {
	return "ANALYZE " + quoteRelation(table)
}

func analyzeTimeoutStatements() []string {
	return []string{
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", analyzeLockTimeout.Milliseconds()),
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", analyzeStatementTimeout.Milliseconds()),
	}
}

func isSQLiteDB(db *sql.DB) bool {
	return strings.Contains(strings.ToLower(fmt.Sprintf("%T", db.Driver())), "sqlite")
}

// AnalyzeTablesMissingExpressionStats runs ANALYZE once on every given table
// (each "table", resolved through the search path, or "schema.table"; the name
// is split on the first "." and each part is quoted)
// that has a valid index on an expression ("object"->'k' and the like) for
// which Postgres holds no planner statistics. Such an index is used with a
// default selectivity guess until its table has been analysed since the index
// was created, which can turn one query shape into a table scan.
//
// Call it after creating an expression index by hand. Convention calls it for
// the tables it prepares, after the DDL has committed.
//
// It does nothing on SQLite. Postgres statements run in short transactions with
// SET LOCAL lock_timeout and statement_timeout (analyzeLockTimeout,
// analyzeStatementTimeout), so nothing leaks to the pooled connection and a
// busy table is given up on, not waited for. Every failure is logged as a
// warning and the next table is still tried; the result is always nil, so a
// caller can never fail because of it. A table that holds no rows, or an index
// whose predicate matches no row, stays without statistics and is analysed
// again by the next call, which is cheap.
func AnalyzeTablesMissingExpressionStats(ctx convCtx.Context, db *sql.DB, tables ...string) error {
	if db == nil || len(tables) == 0 || isSQLiteDB(db) {
		return nil
	}
	analyzeMissingExpressionStats(ctx, db, tables)
	return nil
}

func analyzeMissingExpressionStats(ctx convCtx.Context, db *sql.DB, tables []string) {
	for _, table := range tables {
		missing, err := tableMissesExpressionStats(ctx, db, table)
		if err != nil {
			ctx.Logger().Warn("convention/db: could not check expression index statistics", "table", table, "error", err)
			continue
		}
		if !missing {
			continue
		}
		if err := analyzeTable(ctx, db, table); err != nil {
			ctx.Logger().Warn("convention/db: could not analyse table with expression index lacking statistics", "table", table, "error", err)
		}
	}
}

func tableMissesExpressionStats(ctx convCtx.Context, db *sql.DB, table string) (missing bool, err error) {
	err = inAnalyzeTx(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, missingExpressionStatsQuery, quoteRelation(table)).Scan(&missing)
	})
	return
}

func analyzeTable(ctx convCtx.Context, db *sql.DB, table string) error {
	return inAnalyzeTx(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, analyzeStatement(table))
		return err
	})
}

func inAnalyzeTx(ctx convCtx.Context, db *sql.DB, run func(ctx context.Context, tx *sql.Tx) error) error {
	c, cancel := context.WithTimeout(ctx, analyzeStatementTimeout+analyzeClientSlack)
	defer cancel()

	tx, err := db.BeginTx(c, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit

	for _, stmt := range analyzeTimeoutStatements() {
		if _, err := tx.ExecContext(c, stmt); err != nil {
			return err
		}
	}
	if err := run(c, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// analyzeAfterPrepare is the check prepare runs once its DDL has committed on
// every shard.
func analyzeAfterPrepare(tenantDBs map[convAuth.Tenant][]engineDB, tables ...string) {
	ctx := convCtx.New(convAuth.Claims{User: "convention-db-prepare"})
	for _, entries := range tenantDBs {
		for _, entry := range entries {
			if entry.engine != EnginePostgres {
				continue
			}
			analyzeMissingExpressionStats(ctx, entry.db, tables)
		}
	}
}
