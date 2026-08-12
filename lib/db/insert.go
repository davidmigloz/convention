package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

// ErrDuplicateID is Insert's classification of a duplicate-primary-key
// violation on the runtime table (see isDuplicateInsertErr): wrapped
// together with the id AND the underlying driver error (Go's multi-%w
// support keeps both errors.Is/As-reachable). MutateOrInsert propagates it
// from its own Insert call into its retry budget — absorbed while attempts
// remain, surfaced after exhaustion wrapped with the attempt count on top.
var ErrDuplicateID = errors.New("convention/db: object with this id already exists")

// isDuplicateInsertErr reports whether err is a duplicate-primary-key
// violation from an Insert that ran against engine. Postgres: SQLSTATE
// 23505, via the same sqlStateProvider probe classifyContentionErr uses —
// checked unconditionally, on both engines. SQLite (test-only):
// mattn/go-sqlite3 exposes Code/ExtendedCode as struct fields, not through
// an interface, so — short of importing the driver into production code —
// classification falls back to matching the driver's "UNIQUE constraint
// failed" message substring. That substring match is gated to
// engine == EngineSqlite3: without the gate, a Postgres error whose message
// happens to contain that text would misclassify; with it, the heuristic can
// only ever affect the test-only engine, matching what this comment already
// claims.
func isDuplicateInsertErr(err error, engine Engine) bool {
	if err == nil {
		return false
	}
	if hasSQLState(err, "23505") {
		return true
	}
	return engine == EngineSqlite3 && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func (tos TenantObjectSet[objT, idT, shardKeyT]) Insert(ctx convCtx.Context, obj objT) (err error) {

	err = tos.prepare()
	if err != nil {
		return
	}

	key := obj.DBKey()

	db, engine, err := dbByShardKeyWithEngine(tos.vault, tos.tenant, string(key.ShardKey))
	if err != nil {
		return
	}

	tx, err := db.Begin()
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			err = errors.Join(
				err,
				tx.Rollback(),
			)
			return
		}
		err = tx.Commit()
	}()

	var md Metadata
	md.CreatedAt = ctx.Now()
	md.CreatedBy = ctx.User()
	md.UpdatedAt = md.CreatedAt
	md.UpdatedBy = md.CreatedBy

	for _, compute := range tos.compute {
		err = compute(ctx, md, &obj)
		if err != nil {
			return
		}
	}

	bytes, err := json.Marshal(obj)
	if err != nil {
		return
	}

	_, err = tx.Exec(`INSERT INTO "`+tos.table.RuntimeTableName+`"
("id","created_at","created_by","updated_at","updated_by","object")
VALUES($1,$2,$3,$4,$5,$6)`,
		key.ID, md.CreatedAt, md.CreatedBy, md.UpdatedAt, md.UpdatedBy, bytes)
	if err != nil {
		// Runtime-table PK violation: classify + wrap so callers can
		// errors.Is(err, ErrDuplicateID) instead of matching the raw
		// driver error. The deferred errors.Join(err, tx.Rollback())
		// above preserves errors.Is reachability (Join implements
		// Unwrap() []error). Never applies to the history-table Exec
		// below — the history table has no PK/unique constraint
		// (object.go), so SQLSTATE 23505 is structurally impossible
		// there.
		if isDuplicateInsertErr(err, engine) {
			err = fmt.Errorf("%w: id=%s: %w", ErrDuplicateID, key.ID, err)
		}
		return
	}

	_, err = tx.Exec(`INSERT INTO "`+tos.table.HistoryTableName+`" SELECT "id", "created_at", "created_by", "updated_at", "updated_by", "object" FROM "`+tos.table.RuntimeTableName+`" WHERE "id"=$1`,
		key.ID)
	if err != nil {
		return
	}

	return
}

func (tos TenantObjectSet[objT, idT, shardKeyT]) Upsert(ctx convCtx.Context, obj objT) (err error) {

	err = tos.prepare()
	if err != nil {
		return
	}

	key := obj.DBKey()

	db, err := dbByShardKey(tos.vault, tos.tenant, string(key.ShardKey))
	if err != nil {
		return
	}

	tx, err := db.Begin()
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			err = errors.Join(
				err,
				tx.Rollback(),
			)
			return
		}
		err = tx.Commit()
	}()

	var md Metadata

	err = tx.QueryRow(`SELECT "created_at", "created_by", "updated_at", "updated_by" FROM "`+tos.table.RuntimeTableName+`" WHERE id=$1`, key.ID).
		Scan(&md.CreatedAt, &md.CreatedBy, &md.UpdatedAt, &md.UpdatedBy)
	if err != nil && err != sql.ErrNoRows {
		return
	}
	if err == sql.ErrNoRows {
		md.CreatedAt = ctx.Now()
		md.CreatedBy = ctx.User()
		md.UpdatedAt = md.CreatedAt
		md.UpdatedBy = md.CreatedBy
	} else {
		md.UpdatedAt = ctx.Now()
		md.UpdatedBy = ctx.User()
	}

	for _, compute := range tos.compute {
		err = compute(ctx, md, &obj)
		if err != nil {
			return
		}
	}

	bytes, err := json.Marshal(obj)
	if err != nil {
		return
	}

	_, err = tx.Exec(`INSERT INTO "`+tos.table.RuntimeTableName+`"
("id","created_at","created_by","updated_at","updated_by","object")
VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT ("id")
DO UPDATE SET "updated_at"=$4,"updated_by"=$5,"object"=$6`,
		key.ID, md.CreatedAt, md.CreatedBy, md.UpdatedAt, md.UpdatedBy, bytes)
	if err != nil {
		return
	}

	_, err = tx.Exec(`INSERT INTO "`+tos.table.HistoryTableName+`" SELECT "id", "created_at", "created_by", "updated_at", "updated_by", "object" FROM "`+tos.table.RuntimeTableName+`" WHERE "id"=$1`,
		key.ID)
	if err != nil {
		return
	}

	return
}

func (tos TenantObjectSet[objT, idT, shardKeyT]) UpsertWithMetadata(ctx convCtx.Context, obj ObjectWithMetadata[objT]) (err error) {

	err = tos.prepare()
	if err != nil {
		return
	}

	key := obj.Object.DBKey()

	db, err := dbByShardKey(tos.vault, tos.tenant, string(key.ShardKey))
	if err != nil {
		return
	}

	tx, err := db.Begin()
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			err = errors.Join(
				err,
				tx.Rollback(),
			)
			return
		}
		err = tx.Commit()
	}()

	for _, compute := range tos.compute {
		err = compute(ctx, obj.Metadata, &obj.Object)
		if err != nil {
			return
		}
	}

	bytes, err := json.Marshal(obj.Object)
	if err != nil {
		return
	}

	_, err = tx.Exec(`INSERT INTO "`+tos.table.RuntimeTableName+`"
("id","created_at","created_by","updated_at","updated_by","object")
VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT ("id")
DO UPDATE SET "updated_at"=$4,"updated_by"=$5,"object"=$6`,
		key.ID, obj.Metadata.CreatedAt, obj.Metadata.CreatedBy, obj.Metadata.UpdatedAt, obj.Metadata.UpdatedBy, bytes)
	if err != nil {
		return
	}

	_, err = tx.Exec(`INSERT INTO "`+tos.table.HistoryTableName+`" SELECT "id", "created_at", "created_by", "updated_at", "updated_by", "object" FROM "`+tos.table.RuntimeTableName+`" WHERE "id"=$1`,
		key.ID)
	if err != nil {
		return
	}

	return
}
