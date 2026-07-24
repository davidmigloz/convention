package db_test

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
)

type LockCleanupID string

type LockCleanupObject struct {
	ID      LockCleanupID `json:"id"`
	Payload string        `json:"payload"`
}

func (o LockCleanupObject) DBKey() convDB.Key[LockCleanupID, LockCleanupID] {
	return convDB.Key[LockCleanupID, LockCleanupID]{
		ID:       o.ID,
		ShardKey: o.ID,
	}
}

var lockCleanupDB = convDB.NewObjectSet[LockCleanupObject]("complex").Ready()

const (
	lockCleanupRuntimeTable = "lock_cleanup_object"
	lockCleanupLockTable    = "lock_cleanup_object_lock"

	dropRuntimeTrigger  = "lock_cleanup_drop_runtime_after_lock"
	refuseUnlockTrigger = "lock_cleanup_refuse_unlock"
)

func Test_SelectByIDAndLock_treats_SQL_NULL_object_as_absent_without_acquiring_lock(t *testing.T) {
	db, ctx := setupLockCleanupTest(t)
	id := LockCleanupID("sql-null-object")
	insertLockCleanupRuntimeRow(t, db, id, nil)

	obj, lock, err := lockCleanupDB.Tenant("test").SelectByIDAndLock(ctx, id, "must not be acquired")

	if err != nil {
		t.Errorf("SelectByIDAndLock returned an error for an absent object: %v", err)
	}
	if obj != nil {
		t.Errorf("SelectByIDAndLock returned object %#v for an SQL-NULL row", obj)
	}
	if lock != nil {
		t.Errorf("SelectByIDAndLock returned lock %#v for an SQL-NULL row", lock)
	}
	assertLockCleanupRow(t, db, id, "", false)
}

func Test_SelectByIDAndLock_releases_lock_when_object_disappears_after_acquisition(t *testing.T) {
	db, ctx := setupLockCleanupTest(t)
	id := LockCleanupID("deleted-after-lock")
	insertLockCleanupRuntimeObject(t, db, LockCleanupObject{ID: id, Payload: "present"})
	mustExecLockCleanup(t, db, `
CREATE TRIGGER "`+dropRuntimeTrigger+`"
AFTER INSERT ON "`+lockCleanupLockTable+`"
WHEN NEW."id" = 'deleted-after-lock'
BEGIN
	DELETE FROM "`+lockCleanupRuntimeTable+`" WHERE "id" = NEW."id";
END`)

	obj, lock, err := lockCleanupDB.Tenant("test").SelectByIDAndLock(ctx, id, "transient owner")

	if err == nil {
		t.Fatal("SelectByIDAndLock returned nil error after the object disappeared")
	}
	if !strings.Contains(err.Error(), "object not found") {
		t.Errorf("SelectByIDAndLock lost the original fetch error: %v", err)
	}
	if obj != nil {
		t.Errorf("SelectByIDAndLock returned object %#v after the fetch failed", obj)
	}
	if lock != nil {
		t.Errorf("SelectByIDAndLock returned lock %#v after cleanup succeeded", lock)
	}
	assertLockCleanupRow(t, db, id, "", false)
}

func Test_SelectByIDAndLock_releases_lock_when_object_JSON_is_malformed(t *testing.T) {
	db, ctx := setupLockCleanupTest(t)
	id := LockCleanupID("malformed-object")
	insertLockCleanupRuntimeRow(t, db, id, "{")

	obj, lock, err := lockCleanupDB.Tenant("test").SelectByIDAndLock(ctx, id, "decode owner")

	if err == nil {
		t.Fatal("SelectByIDAndLock returned nil error for malformed JSON")
	}
	if obj != nil {
		t.Errorf("SelectByIDAndLock returned partially decoded object %#v", obj)
	}
	if lock != nil {
		t.Errorf("SelectByIDAndLock returned lock %#v after cleanup succeeded", lock)
	}
	assertLockCleanupRow(t, db, id, "", false)
}

func Test_SelectByIDAndLock_joins_fetch_and_cleanup_errors(t *testing.T) {
	db, ctx := setupLockCleanupTest(t)
	id := LockCleanupID("cleanup-failure")
	insertLockCleanupRuntimeObject(t, db, LockCleanupObject{ID: id, Payload: "present"})
	mustExecLockCleanup(t, db, `
CREATE TRIGGER "`+dropRuntimeTrigger+`"
AFTER INSERT ON "`+lockCleanupLockTable+`"
WHEN NEW."id" = 'cleanup-failure'
BEGIN
	DELETE FROM "`+lockCleanupRuntimeTable+`" WHERE "id" = NEW."id";
END`)
	mustExecLockCleanup(t, db, `
CREATE TRIGGER "`+refuseUnlockTrigger+`"
BEFORE DELETE ON "`+lockCleanupLockTable+`"
WHEN OLD."id" = 'cleanup-failure'
BEGIN
	SELECT RAISE(ABORT, 'unlock refused');
END`)

	obj, lock, err := lockCleanupDB.Tenant("test").SelectByIDAndLock(ctx, id, "cleanup owner")

	if err == nil {
		t.Fatal("SelectByIDAndLock returned nil error after fetch and cleanup failed")
	}
	if !strings.Contains(err.Error(), "object not found") {
		t.Errorf("SelectByIDAndLock lost the original fetch error: %v", err)
	}
	if !strings.Contains(err.Error(), "unlock refused") {
		t.Errorf("SelectByIDAndLock lost the cleanup error: %v", err)
	}
	if obj != nil {
		t.Errorf("SelectByIDAndLock returned object %#v after the fetch failed", obj)
	}
	if lock == nil {
		t.Error("SelectByIDAndLock discarded the lock handle after cleanup failed")
	}
	assertLockCleanupRow(t, db, id, "cleanup owner", true)
}

func Test_SelectByIDAndLock_does_not_unlock_foreign_lock_when_acquisition_loses_race(t *testing.T) {
	db, ctx := setupLockCleanupTest(t)
	id := LockCleanupID("foreign-lock")
	insertLockCleanupRuntimeObject(t, db, LockCleanupObject{ID: id, Payload: "present"})
	mustExecLockCleanup(t, db, `
INSERT INTO "`+lockCleanupLockTable+`" ("id", "created_at", "description")
VALUES ($1, $2, $3)`, id, ctx.Now(), "foreign owner")
	mustExecLockCleanup(t, db, `
CREATE TRIGGER "`+refuseUnlockTrigger+`"
BEFORE DELETE ON "`+lockCleanupLockTable+`"
WHEN OLD."id" = 'foreign-lock'
BEGIN
	SELECT RAISE(ABORT, 'unlock refused');
END`)

	obj, lock, err := lockCleanupDB.Tenant("test").SelectByIDAndLock(ctx, id, "losing owner")

	if err != nil {
		t.Errorf("SelectByIDAndLock attempted to clean up a foreign lock: %v", err)
	}
	if obj != nil {
		t.Errorf("SelectByIDAndLock returned object %#v without acquiring its lock", obj)
	}
	if lock != nil {
		t.Errorf("SelectByIDAndLock returned lock %#v after losing acquisition", lock)
	}
	assertLockCleanupRow(t, db, id, "foreign owner", true)
}

func setupLockCleanupTest(t *testing.T) (*sql.DB, convCtx.Context) {
	t.Helper()

	ctx := convCtx.New(convAuth.Claims{User: "lock-cleanup-test"}).
		WithNow(time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC))

	_, _, err := lockCleanupDB.Tenant("test").SelectByIDAndLock(
		ctx,
		LockCleanupID("prepare-only"),
		"prepare-only",
	)
	if err != nil {
		t.Fatalf("prepare LockCleanupObject tables: %v", err)
	}

	dbs, err := convDB.DBs("complex", "test")
	if err != nil {
		t.Fatalf("get complex test database: %v", err)
	}
	if len(dbs) != 1 {
		t.Fatalf("complex test database has %d shards, want 1", len(dbs))
	}

	db := dbs[0]
	resetLockCleanupTest(t, db)
	t.Cleanup(func() {
		resetLockCleanupTest(t, db)
	})
	return db, ctx
}

func resetLockCleanupTest(t *testing.T, db *sql.DB) {
	t.Helper()

	mustExecLockCleanup(t, db, `DROP TRIGGER IF EXISTS "`+refuseUnlockTrigger+`"`)
	mustExecLockCleanup(t, db, `DROP TRIGGER IF EXISTS "`+dropRuntimeTrigger+`"`)
	mustExecLockCleanup(t, db, `DELETE FROM "`+lockCleanupLockTable+`"`)
	mustExecLockCleanup(t, db, `DELETE FROM "`+lockCleanupRuntimeTable+`"`)
}

func insertLockCleanupRuntimeObject(t *testing.T, db *sql.DB, obj LockCleanupObject) {
	t.Helper()

	bytes, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal LockCleanupObject: %v", err)
	}
	insertLockCleanupRuntimeRow(t, db, obj.ID, bytes)
}

func insertLockCleanupRuntimeRow(t *testing.T, db *sql.DB, id LockCleanupID, object any) {
	t.Helper()

	at := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	mustExecLockCleanup(t, db, `
INSERT INTO "`+lockCleanupRuntimeTable+`"
	("id", "created_at", "created_by", "updated_at", "updated_by", "object")
VALUES ($1, $2, $3, $4, $5, $6)`,
		id, at, "lock-cleanup-test", at, "lock-cleanup-test", object)
}

func assertLockCleanupRow(
	t *testing.T,
	db *sql.DB,
	id LockCleanupID,
	wantDescription string,
	wantExists bool,
) {
	t.Helper()

	var description string
	err := db.QueryRow(
		`SELECT "description" FROM "`+lockCleanupLockTable+`" WHERE "id" = $1`,
		id,
	).Scan(&description)
	if !wantExists {
		if err == sql.ErrNoRows {
			return
		}
		if err != nil {
			t.Fatalf("query lock row: %v", err)
		}
		t.Errorf("lock row remains with description %q", description)
		return
	}
	if err != nil {
		t.Fatalf("query lock row: %v", err)
	}
	if description != wantDescription {
		t.Errorf("lock description = %q, want %q", description, wantDescription)
	}
}

func mustExecLockCleanup(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()

	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("execute lock cleanup test SQL: %v", err)
	}
}
