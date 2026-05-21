package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

var (
	ErrObjectNotFound   = errors.New("convention/db: object not found")
	ErrLockNotAvailable = errors.New("convention/db: row lock not available")
	ErrCASConflict      = errors.New("convention/db: object modified since read")
	// ErrLeaseLost is returned by a lease lock's Renew/Unlock when the row is no
	// longer owned by this holder (it expired and was stolen, or was force-cleared).
	ErrLeaseLost = errors.New("convention/db: lock lease lost")
)

// sqlStateProvider is implemented by both lib/pq.Error and pgx-style PgError,
// letting us classify SQLSTATEs without a direct driver dependency.
type sqlStateProvider interface {
	SQLState() string
}

func classifyContentionErr(err error) (mapped error, ok bool) {
	if err == nil {
		return nil, false
	}
	var pgErr sqlStateProvider
	if errors.As(err, &pgErr) && pgErr.SQLState() == "55P03" {
		return ErrLockNotAvailable, true
	}
	return nil, false
}

func (tos TenantObjectSet[objT, idT, shardKeyT]) Update(ctx convCtx.Context, obj objT) (err error) {

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
	if err == sql.ErrNoRows {
		err = fmt.Errorf("object with ID '%s' does not exist", key.ID)
		return
	}
	if err != nil {
		return
	}

	md.UpdatedAt = ctx.Now()
	md.UpdatedBy = ctx.User()

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

	_, err = tx.Exec(`UPDATE "`+tos.table.RuntimeTableName+`" SET "object"=$1, "updated_at"=$2, "updated_by"=$3 WHERE "id"=$4`,
		bytes, md.UpdatedAt, md.UpdatedBy, key.ID)
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

// SafeUpdate is an optimistic-concurrency primitive: persist `to` only if the
// current row still matches the caller's `from` snapshot. Returns
// ErrObjectNotFound if the row is missing, ErrLockNotAvailable on contended
// NOWAIT acquisition (Postgres only), and ErrCASConflict on a stale `from`.
//
// True CAS is Postgres-only — the `FOR UPDATE NOWAIT` row lock is required to
// block concurrent writers between the SELECT and the UPDATE. SQLite mode
// elides the lock; the comparator still catches stale-`from` callers but two
// truly concurrent SQLite writers can race past it. Use SQLite for tests,
// Postgres for production.
//
// Callers must not mutate `from`'s business state between load and call.
// Both the current row and `from` are normalized through the same
// decode→compute-hook→marshal pipeline before comparison, so the guard
// compares business state and is insensitive to embedded metadata (and to
// how `from` was loaded — SelectByID, Process, or hand-built).
func (tos TenantObjectSet[objT, idT, shardKeyT]) SafeUpdate(ctx convCtx.Context, from, to objT) (err error) {

	err = tos.prepare()
	if err != nil {
		return
	}

	fromKey, toKey := from.DBKey(), to.DBKey()

	if fromKey.ID != toKey.ID {
		err = errors.New("cannot safely update object with different IDs")
		return
	}

	if fromKey.ShardKey != toKey.ShardKey {
		err = errors.New("cannot safely update object with different shard keys")
		return
	}

	db, engine, err := dbByShardKeyWithEngine(tos.vault, tos.tenant, string(fromKey.ShardKey))
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

	var lockClause string
	if engine == EnginePostgres {
		lockClause = " FOR UPDATE NOWAIT"
	}

	var (
		cmpData []byte
		cmp     objT
		md      Metadata
	)
	row := tx.QueryRow(`SELECT "object", "created_at", "created_by", "updated_at", "updated_by" FROM "`+tos.table.RuntimeTableName+`" WHERE "id"=$1`+lockClause, fromKey.ID)
	err = row.Scan(&cmpData, &md.CreatedAt, &md.CreatedBy, &md.UpdatedAt, &md.UpdatedBy)
	if err == sql.ErrNoRows {
		err = fmt.Errorf("%w: id=%s", ErrObjectNotFound, fromKey.ID)
		return
	}
	if err != nil {
		if mapped, ok := classifyContentionErr(err); ok {
			err = fmt.Errorf("%w: id=%s", mapped, fromKey.ID)
		}
		return
	}

	err = json.Unmarshal(cmpData, &cmp)
	if err != nil {
		return
	}

	// Normalize both sides through the same pipeline before comparing:
	// decode the row, then run the compute hooks with the just-loaded
	// metadata, then marshal. `from` is cloned (marshal→unmarshal) and put
	// through the identical hooks so the comparison is insensitive to how
	// the caller loaded it. This matters because compute hooks typically
	// copy the row metadata (created/updated stamps) onto the object, and
	// that metadata legitimately differs by load path and timestamp
	// precision: a `from` from SelectByID carries column-precision stamps,
	// one from Process (which skips compute hooks) carries the raw stored
	// JSON stamps, and on Postgres the JSONB object keeps nanoseconds while
	// the columns are microseconds. Normalizing both sides with the same
	// loaded md cancels that out, so the CAS guard compares business state
	// only — matching the intent "did the row change since the caller read
	// it" rather than "do the embedded metadata bytes match".
	for _, compute := range tos.compute {
		err = compute(ctx, md, &cmp)
		if err != nil {
			return
		}
	}

	cmpBytes, err := json.Marshal(cmp)
	if err != nil {
		return
	}

	var fromCmp objT
	fromRaw, err := json.Marshal(from)
	if err != nil {
		return
	}
	err = json.Unmarshal(fromRaw, &fromCmp)
	if err != nil {
		return
	}
	for _, compute := range tos.compute {
		err = compute(ctx, md, &fromCmp)
		if err != nil {
			return
		}
	}

	fromBytes, err := json.Marshal(fromCmp)
	if err != nil {
		return
	}

	if string(cmpBytes) != string(fromBytes) {
		err = fmt.Errorf("%w: id=%s", ErrCASConflict, fromKey.ID)
		return
	}

	md.UpdatedAt = ctx.Now()
	md.UpdatedBy = ctx.User()

	for _, compute := range tos.compute {
		err = compute(ctx, md, &to)
		if err != nil {
			return
		}
	}

	toBytes, err := json.Marshal(to)
	if err != nil {
		return
	}

	res, err := tx.Exec(`UPDATE "`+tos.table.RuntimeTableName+`" SET "object"=$1, "updated_at"=$2, "updated_by"=$3 WHERE "id"=$4`,
		toBytes, md.UpdatedAt, md.UpdatedBy, toKey.ID)
	if err != nil {
		return
	}

	count, err := res.RowsAffected()
	if err != nil {
		return
	}

	if count == 0 {
		err = fmt.Errorf("%w: id=%s", ErrCASConflict, fromKey.ID)
		return
	}

	_, err = tx.Exec(`INSERT INTO "`+tos.table.HistoryTableName+`" SELECT "id", "created_at", "created_by", "updated_at", "updated_by", "object" FROM "`+tos.table.RuntimeTableName+`" WHERE "id"=$1`,
		toKey.ID)
	if err != nil {
		return
	}

	return
}
