package db_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
)

// errInjectedProcessCallback is returned by a Process/ProcessWithMetadata
// callback to force an in-flight error without corrupting any row.
var errInjectedProcessCallback = errors.New("injected process callback failure")

// Test_RowsClosedOnErrorPaths proves the six *sql.Rows-iterating collection
// reads (SelectAll, SelectAllWithMetadata, Select, SelectWithMetadata,
// Process, ProcessWithMetadata) release the shard's connection on every
// early-return error path.
//
// Detection is via connection-pool stats rather than a mock driver: the
// "complex" vault's single in-memory SQLite shard runs with
// SetMaxOpenConns(1) (see AGENTS.md's Connection pooling section), so a
// leaked *sql.Rows pins the pool's only connection and
// RawDBForShardKey(...).Stats().InUse reads 1 instead of 0 immediately
// after the erroring call returns — deterministic, no GC forcing needed.
//
// Red-first note for whoever re-runs this before the fix lands: run each
// sub-test in isolation, e.g.
//
//	go test ./lib/db/ -run 'Test_RowsClosedOnErrorPaths/Select$' -count=1
//
// Once one sub-test leaks the pool's only connection, every later caller on
// this shard — this file's own fixture cleanup included — blocks forever
// waiting for a connection database/sql will never hand back. Each
// sub-test's cleanup checks Stats().InUse first and skips the delete when
// the pool is still (expectedly, pre-fix) exhausted, so a red run finishes
// instead of hanging; post-fix the pool is never left exhausted, so cleanup
// always runs and no corrupted or extra fixture row survives the test.
func Test_RowsClosedOnErrorPaths(t *testing.T) {

	t.Run("SelectAll", func(t *testing.T) {
		ctx := convCtx.New(convAuth.Claims{User: convAuth.User(t.Name())})
		db, _, _ := newCorruptedFixturePair(t, ctx, "select-all")

		_, err := complexDB.Tenant("test").SelectAll(ctx)
		assertDecodeErrorSurfaced(t, err)
		assertConnectionReleased(t, db)
	})

	t.Run("SelectAllWithMetadata", func(t *testing.T) {
		ctx := convCtx.New(convAuth.Claims{User: convAuth.User(t.Name())})
		db, _, _ := newCorruptedFixturePair(t, ctx, "select-all-md")

		_, err := complexDB.Tenant("test").SelectAllWithMetadata(ctx)
		assertDecodeErrorSurfaced(t, err)
		assertConnectionReleased(t, db)
	})

	t.Run("Select", func(t *testing.T) {
		ctx := convCtx.New(convAuth.Claims{User: convAuth.User(t.Name())})
		db, _, _ := newCorruptedFixturePair(t, ctx, "select")

		// Scoped by CreatedBy (a real SQL column), not a JSON-field
		// predicate: the where builder's "object"->'key' extraction
		// operator reads NULL for the corrupted row's non-JSON text, which
		// would silently exclude it from the WHERE and defeat the whole
		// point of this sub-test. CreatedBy matches on the real
		// "created_by" column, so both fixture rows (valid and corrupted)
		// are selected regardless of what their "object" column holds.
		_, err := complexDB.Tenant("test").Select(ctx,
			convDB.Where().CreatedBy(t.Name()),
		)
		assertDecodeErrorSurfaced(t, err)
		assertConnectionReleased(t, db)
	})

	t.Run("SelectWithMetadata", func(t *testing.T) {
		ctx := convCtx.New(convAuth.Claims{User: convAuth.User(t.Name())})
		db, _, _ := newCorruptedFixturePair(t, ctx, "select-md")

		// See Select above for why this scopes by CreatedBy rather than a
		// JSON-field predicate.
		_, err := complexDB.Tenant("test").SelectWithMetadata(ctx,
			convDB.Where().CreatedBy(t.Name()),
		)
		assertDecodeErrorSurfaced(t, err)
		assertConnectionReleased(t, db)
	})

	t.Run("Process", func(t *testing.T) {
		ctx := convCtx.New(convAuth.Claims{User: convAuth.User(t.Name())})
		db, id := newProcessErrorFixture(t, ctx, "process")

		_, err := complexDB.Tenant("test").Process(ctx,
			convDB.Where().Key("complex_id").Equals().Value(id),
			func(_ convCtx.Context, _ ComplexObject) error {
				return errInjectedProcessCallback
			},
		)
		if !errors.Is(err, errInjectedProcessCallback) {
			t.Fatalf("expected the injected callback error to surface, got %v", err)
		}
		assertConnectionReleased(t, db)
	})

	t.Run("ProcessWithMetadata", func(t *testing.T) {
		ctx := convCtx.New(convAuth.Claims{User: convAuth.User(t.Name())})
		db, id := newProcessErrorFixture(t, ctx, "process-md")

		_, err := complexDB.Tenant("test").ProcessWithMetadata(ctx,
			convDB.Where().Key("complex_id").Equals().Value(id),
			func(_ convCtx.Context, _ convDB.ObjectWithMetadata[ComplexObject]) error {
				return errInjectedProcessCallback
			},
		)
		if !errors.Is(err, errInjectedProcessCallback) {
			t.Fatalf("expected the injected callback error to surface, got %v", err)
		}
		assertConnectionReleased(t, db)
	})
}

// newCorruptedFixturePair inserts two ComplexObject rows on the "complex"
// vault's single shard, then corrupts the second row's "object" column to
// the invalid-JSON literal "not-json" via a raw UPDATE — mirrors
// mutate_test.go's raw-write pattern (see its
// null_object_row_exhausts_with_duplicate_id sub-test). Two rows so the
// corrupt one is hit mid-iteration, not on the cursor's very first Next().
//
// t.Cleanup always tries to delete both fixture rows: Delete issues a plain
// SQL DELETE and never decodes the "object" column, so it works fine even
// on the corrupted row. This matters most for SelectAll/
// SelectAllWithMetadata, which have no where clause to scope away from a
// row left behind by an earlier test — an un-deleted corrupt row here would
// poison every later where-less test on this vault.
func newCorruptedFixturePair(t *testing.T, ctx convCtx.Context, suffix string) (shardDB *sql.DB, id1, id2 ComplexID) {
	t.Helper()

	f1 := newComplexFixture(t, ctx, suffix+"-1")
	f2 := newComplexFixture(t, ctx, suffix+"-2")
	id1, id2 = f1.ComplexID, f2.ComplexID

	shardDB, err := complexDB.Tenant("test").RawDBForShardKey(id2)
	if err != nil {
		t.Fatalf("RawDBForShardKey failed: %v", err)
	}
	table := complexDB.Tenant("test").RuntimeTableName()

	if _, err := shardDB.Exec(`UPDATE "`+table+`" SET "object"=$1 WHERE "id"=$2`, "not-json", id2); err != nil {
		t.Fatalf("corrupt fixture row: %v", err)
	}

	t.Cleanup(func() {
		cleanupComplexFixtures(t, ctx, shardDB, id1, id2)
	})

	return shardDB, id1, id2
}

// newProcessErrorFixture inserts a single ComplexObject row and returns its
// shard handle and ID. Process/ProcessWithMetadata need no row corruption —
// their callback is what injects the error — so a where clause scoped to
// this one ID is enough to guarantee the callback runs.
func newProcessErrorFixture(t *testing.T, ctx convCtx.Context, suffix string) (shardDB *sql.DB, id ComplexID) {
	t.Helper()

	f := newComplexFixture(t, ctx, suffix)
	id = f.ComplexID

	shardDB, err := complexDB.Tenant("test").RawDBForShardKey(id)
	if err != nil {
		t.Fatalf("RawDBForShardKey failed: %v", err)
	}

	t.Cleanup(func() {
		cleanupComplexFixtures(t, ctx, shardDB, id)
	})

	return shardDB, id
}

// cleanupComplexFixtures deletes every given fixture ID, unless the pool is
// still showing a leaked connection (Stats().InUse > 0). That guard only
// ever trips on a red, pre-fix run: Delete needs the same single-connection
// pool the leaked *sql.Rows is still pinning, and would otherwise block
// forever. Post-fix, InUse is always 0 here (that is exactly what the
// sub-test just asserted), so this always deletes and no fixture — corrupt
// or otherwise — survives the test.
func cleanupComplexFixtures(t *testing.T, ctx convCtx.Context, shardDB *sql.DB, ids ...ComplexID) {
	t.Helper()

	if s := shardDB.Stats(); s.InUse > 0 {
		t.Logf("skipping fixture cleanup: pool still shows Stats().InUse=%d (expected on a pre-fix red run)", s.InUse)
		return
	}
	for _, id := range ids {
		if err := complexDB.Tenant("test").Delete(ctx, id); err != nil {
			t.Errorf("cleanup fixture %s: %v", id, err)
		}
	}
}

// assertDecodeErrorSurfaced checks that the corrupted "not-json" row
// produced the json.Unmarshal error it should, not merely a non-nil err.
func assertDecodeErrorSurfaced(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error decoding the corrupted row, got nil")
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("expected a json.SyntaxError to surface from the corrupted row, got %v (%T)", err, err)
	}
}

// assertConnectionReleased is the leak assertion shared by all six
// sub-tests: the erroring call must have returned the shard's only
// connection to the pool.
func assertConnectionReleased(t *testing.T, shardDB *sql.DB) {
	t.Helper()
	if s := shardDB.Stats(); s.InUse != 0 {
		t.Fatalf("rows leaked the connection: Stats().InUse = %d, want 0", s.InUse)
	}
}
