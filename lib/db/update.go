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
// Classification (55P03 for lock contention, and 23505 for duplicate keys —
// see isDuplicateInsertErr in insert.go) requires the registered Postgres
// driver's errors to implement this method; lib/pq gained SQLState() in
// v1.10.5.
type sqlStateProvider interface {
	SQLState() string
}

// hasSQLState reports whether err (or something it wraps, via errors.As)
// implements sqlStateProvider and reports the given SQLSTATE. Shared probe
// behind classifyContentionErr (55P03) and mutate.go's isDuplicateInsertErr
// (23505).
func hasSQLState(err error, state string) bool {
	if err == nil {
		return false
	}
	var sqlErr sqlStateProvider
	return errors.As(err, &sqlErr) && sqlErr.SQLState() == state
}

func classifyContentionErr(err error) (mapped error, ok bool) {
	if hasSQLState(err, "55P03") {
		return ErrLockNotAvailable, true
	}
	return nil, false
}

// cloneViaJSON returns a deep copy of v via marshal→unmarshal, plus the
// intermediate marshaled form (raw) for callers that also need it as a
// canonical-JSON comparison baseline — mutateLoop's CAS baseline clone does;
// callers that don't (SafeUpdate's from-clone below) discard it. The
// round-trip is what makes the clone safe against a caller mutating v's
// slices/maps in place after cloning: they no longer share backing storage.
// encoding/json drops unexported fields on the round-trip — same semantics
// both call sites already relied on before this was extracted.
func cloneViaJSON[T any](v T) (clone T, raw []byte, err error) {
	raw, err = json.Marshal(v)
	if err != nil {
		return
	}
	err = json.Unmarshal(raw, &clone)
	return
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
// On conflict, the caller reloads and retries — Mutate does exactly that
// loop (reload → fn → SafeUpdate, with backoff) for the common case of a
// read-modify-write that should converge under contention; reach for it
// before hand-rolling the retry here.
//
// True CAS is Postgres-only — the `FOR UPDATE NOWAIT` row lock is required to
// block concurrent writers between the SELECT and the UPDATE. SQLite mode
// elides that lock, but this package's in-memory SQLite pool is capped at a
// single connection (SetMaxOpenConns(1), see AGENTS.md's Connection pooling
// section), so two SQLite writers can never truly race at the driver level
// in the first place — every write serializes through that one connection.
// A writer whose `from` snapshot went stale while queued behind another
// writer's commit is still caught, just by the comparator rather than a
// lock, and surfaces as the ordinary ErrCASConflict. The real Postgres/
// SQLite difference is narrower than "SQLite can lose an update": Postgres
// additionally blocks a second writer from even starting its comparison
// while the first holds the row lock; SQLite lets both comparisons run and
// relies entirely on the comparator to reject whichever one goes stale. Use
// SQLite for tests, Postgres for production.
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
	row := tx.QueryRow(`SELECT "object", "created_at", "created_by", "updated_at", "updated_by" FROM "`+tos.table.RuntimeTableName+`" WHERE "id"=$1 AND "object" IS NOT NULL`+lockClause, fromKey.ID)
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
	if cmpData == nil {
		err = fmt.Errorf("%w: id=%s", ErrObjectNotFound, fromKey.ID)
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

	fromCmp, _, err := cloneViaJSON(from)
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

	// On Postgres this is unreachable: FOR UPDATE NOWAIT above already holds
	// the row lock, so no concurrent writer can have changed it since the
	// comparator's SELECT, and the UPDATE by "id" always affects exactly one
	// row. On SQLite (lock clause elided, writers serialized by this
	// package's single-connection pool — see the dialect-split note above)
	// it is a defensive backstop against future refactors rather than a
	// live race: no other write can be concurrently in flight to race this
	// one out from under it.
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
