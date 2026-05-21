package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

// unlockTimeout bounds the owner-safe DELETE in a lease lock's Unlock so a stalled
// DB cannot hang a job's teardown indefinitely. The legacy (non-lease) path is
// unchanged and uses Exec without a deadline.
const unlockTimeout = 5 * time.Second

// LockOption configures a Lock acquisition.
type LockOption func(*lockConfig)

type lockConfig struct {
	lease time.Duration
}

// WithLease enables heartbeat-lease semantics on a Lock acquisition:
//
//   - An existing lock whose created_at is older than lease is treated as expired
//     and stolen, so an owner that crashed without unlocking no longer blocks
//     forever.
//   - The returned Lock is owner-safe: Renew and Unlock only affect a row this
//     caller still owns (matched by description), so they never disturb a lock that
//     was stolen away after expiry.
//
// The holder is expected to Renew well within lease (see lib/job's scheduler).
// Without this option Lock behaves as a sticky mutex (INSERT ... ON CONFLICT DO
// NOTHING) that is never stolen — the long-standing default existing callers rely on.
func WithLease(d time.Duration) LockOption {
	return func(c *lockConfig) { c.lease = d }
}

type Lock[objT Object[idT, shardKeyT], idT, shardKeyT ~string] struct {
	tos TenantObjectSet[objT, idT, shardKeyT]
	si  int
	id  idT

	// owner is the description this caller wrote. "" marks a legacy (non-lease)
	// lock whose Unlock deletes unconditionally by id.
	owner string
	lease time.Duration

	// stolen / previousOwner are advisory acquisition metadata (logging only),
	// set when the lease acquire replaced an existing (expired) row.
	stolen        bool
	previousOwner string
}

// Stolen reports whether this lease lock was acquired by replacing an existing
// expired lock (i.e. the previous holder likely crashed without unlocking).
func (l Lock[objT, idT, shardKeyT]) Stolen() bool { return l.stolen }

// PreviousOwner returns the description of the expired lock this acquisition
// replaced, or "" if none. Advisory (best-effort), for logging.
func (l Lock[objT, idT, shardKeyT]) PreviousOwner() string { return l.previousOwner }

func (l Lock[objT, idT, shardKeyT]) Unlock() (err error) {

	db, err := dbByIndex(l.tos.vault, l.tos.tenant, l.si)
	if err != nil {
		return
	}

	if l.owner == "" {
		// Legacy lock: unconditional delete by id (unchanged behaviour).
		_, err = db.Exec(`DELETE FROM "`+l.tos.table.LockTableName+`" WHERE "id"=$1;`, l.id)
		return
	}

	// Lease lock: owner-safe delete on a bounded context so teardown/shutdown can
	// still release the lock. Never removes a row a different owner has stolen.
	dctx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
	defer cancel()

	res, err := db.ExecContext(dctx,
		`DELETE FROM "`+l.tos.table.LockTableName+`" WHERE "id"=$1 AND "description"=$2;`,
		l.id, l.owner)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseLost
	}
	return
}

// Renew refreshes a lease lock's timestamp (heartbeat). It returns ErrLeaseLost
// when the row is no longer owned by this holder (expired and stolen, or cleared),
// so the caller can stop work and let the new owner take over. Uses ExecContext so
// a cancelled context interrupts an in-flight renew.
func (l Lock[objT, idT, shardKeyT]) Renew(ctx convCtx.Context) (err error) {

	if l.owner == "" {
		return fmt.Errorf("convention/db: Renew called on a non-lease lock")
	}

	db, err := dbByIndex(l.tos.vault, l.tos.tenant, l.si)
	if err != nil {
		return
	}

	res, err := db.ExecContext(ctx.Context,
		`UPDATE "`+l.tos.table.LockTableName+`" SET "created_at"=$1 WHERE "id"=$2 AND "description"=$3;`,
		ctx.Now(), l.id, l.owner)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseLost
	}
	return
}

func (tos TenantObjectSet[objT, idT, shardKeyT]) Lock(ctx convCtx.Context, obj objT, desc string, opts ...LockOption) (lock *Lock[objT, idT, shardKeyT], err error) {

	err = tos.prepare()
	if err != nil {
		return
	}

	var cfg lockConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	key := obj.DBKey()

	dbs, err := DBs(tos.vault, tos.tenant)
	if err != nil {
		return
	}

	si := indexByShardKey(string(key.ShardKey), len(dbs))

	db := dbs[si]

	if cfg.lease <= 0 {
		// Legacy sticky-lock path — unchanged behaviour (never steals).
		var res sql.Result
		res, err = db.Exec(`INSERT INTO "`+tos.table.LockTableName+`"
("id","created_at","description")
VALUES($1,$2,$3)
ON CONFLICT ("id") DO NOTHING;`,
			key.ID, ctx.Now(), desc)
		if err != nil {
			return
		}

		var count int64
		count, err = res.RowsAffected()
		if err != nil {
			return
		}

		if count == 0 {
			return // someone was faster, no lock
		}

		lock = &Lock[objT, idT, shardKeyT]{
			tos: tos,
			si:  si,
			id:  key.ID,
		}

		return
	}

	// Lease path: steal an expired lock and tag it with this owner (desc).
	now := ctx.Now()
	cutoff := now.Add(-cfg.lease)

	// Advisory pre-read (best-effort, logging only) of the prior holder, so the
	// caller can report a steal. Correctness rests solely on the atomic upsert below.
	var prior string
	priorExists := false
	switch scanErr := db.QueryRowContext(ctx.Context,
		`SELECT "description" FROM "`+tos.table.LockTableName+`" WHERE "id"=$1`, key.ID).
		Scan(&prior); scanErr {
	case nil:
		priorExists = true
	case sql.ErrNoRows:
		priorExists = false
	default:
		err = scanErr
		return
	}

	// Explicit target alias "l" so the WHERE references the EXISTING row, never
	// excluded.created_at. Accepted by both Postgres and SQLite. RowsAffected==1
	// means inserted-or-stole; ==0 means a live (recently-renewed) lock is held.
	var res sql.Result
	res, err = db.ExecContext(ctx.Context,
		`INSERT INTO "`+tos.table.LockTableName+`" AS l ("id","created_at","description")
VALUES($1,$2,$3)
ON CONFLICT ("id") DO UPDATE SET "created_at"=$2, "description"=$3
WHERE l."created_at" < $4;`,
		key.ID, now, desc, cutoff)
	if err != nil {
		return
	}

	var count int64
	count, err = res.RowsAffected()
	if err != nil {
		return
	}

	if count == 0 {
		return // live lock held by another owner
	}

	lock = &Lock[objT, idT, shardKeyT]{
		tos:   tos,
		si:    si,
		id:    key.ID,
		owner: desc,
		lease: cfg.lease,
	}
	if priorExists {
		lock.stolen = true
		lock.previousOwner = prior
	}

	return
}

func (tos TenantObjectSet[objT, idT, shardKeyT]) SelectByIDAndLock(ctx convCtx.Context, id idT, desc string, shardKeys ...shardKeyT) (obj *objT, lock *Lock[objT, idT, shardKeyT], err error) {

	err = tos.prepare()
	if err != nil {
		return
	}

	dbs, err := DBs(tos.vault, tos.tenant)
	if err != nil {
		return
	}

	sis := make(map[int]any)
	if len(shardKeys) == 0 {
		for i := range dbs {
			sis[i] = nil
		}
	} else {
		for _, key := range shardKeys {
			i := indexByShardKey(string(key), len(dbs))
			sis[i] = nil
		}
	}

	for si, db := range dbs {

		if _, ok := sis[si]; !ok {
			continue
		}

		var exists bool
		err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM "`+tos.table.RuntimeTableName+`" WHERE id = $1)`, id).
			Scan(&exists)
		if err == sql.ErrNoRows {
			err = nil
			continue
		}
		if err != nil {
			return
		}
		if !exists {
			continue
		}

		var execRes sql.Result
		execRes, err = db.Exec(`INSERT INTO "`+tos.table.LockTableName+`"
("id","created_at","description")
VALUES($1,$2,$3)
ON CONFLICT ("id") DO NOTHING;`,
			id, ctx.Now(), desc)
		if err != nil {
			return
		}

		var count int64
		count, err = execRes.RowsAffected()
		if err != nil {
			return
		}

		if count == 0 {
			return // someone was faster, no lock, no object
		}

		lock = &Lock[objT, idT, shardKeyT]{
			tos: tos,
			si:  si,
			id:  id,
		}

		var bytes []byte

		err = db.
			QueryRow(`SELECT "object" FROM "`+tos.table.RuntimeTableName+`" WHERE id=$1`, id).
			Scan(&bytes)
		if err == sql.ErrNoRows {
			err = fmt.Errorf("object not found, even though lock was acquired")
		}
		if err != nil {
			return
		}

		obj = new(objT)
		err = json.Unmarshal(bytes, obj)
		if err != nil {
			return
		}

	}
	return
}
