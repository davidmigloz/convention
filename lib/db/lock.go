package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

// unlockTimeout bounds the owner-safe DELETE in a lease lock's Unlock so a stalled
// DB cannot hang a job's teardown indefinitely. The legacy (non-lease) path is
// unchanged and uses Exec without a deadline.
const unlockTimeout = 5 * time.Second

// ownerTokenSep separates the caller's human-readable lock description from a unique
// per-acquisition token in the stored "description". Ownership (Renew/Unlock) matches the
// whole stored value, so two acquisitions that pass the same description are still distinct
// owners. It is a control byte that does not occur in a human description, so PreviousOwner
// can strip the token back off for logging.
const ownerTokenSep = "\x1f"

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

	// stolen / previousOwner are advisory acquisition metadata (logging only),
	// set when the lease acquire replaced an existing (expired) row.
	stolen        bool
	previousOwner string
}

// Stolen reports whether this lease lock was acquired by replacing an existing
// expired lock (i.e. the previous holder likely crashed without unlocking).
func (l Lock[objT, idT, shardKeyT]) Stolen() bool { return l.stolen }

// PreviousOwner returns the human-readable description of the expired lock this
// acquisition replaced (the per-acquisition token is stripped), or "" if none. Advisory
// (best-effort), for logging.
func (l Lock[objT, idT, shardKeyT]) PreviousOwner() string {
	if i := strings.Index(l.previousOwner, ownerTokenSep); i >= 0 {
		return l.previousOwner[:i]
	}
	return l.previousOwner
}

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

// UpdateGuarded persists obj exactly like Update, but only while this caller still owns the
// lease — and it does so without any read-then-write window. Inside one transaction it first
// renews the lease row with an owner-checked UPDATE, which (a) returns ErrLeaseLost if the
// row no longer names this owner and (b) takes the lease row's write lock for the rest of the
// transaction. A concurrent steal (the ON CONFLICT DO UPDATE in Lock) must lock the same row,
// so it blocks until this transaction commits. The lease is renewed once more immediately
// before commit, so even if the transaction ran longer than the lease a blocked waiter sees a
// timestamp newer than its cutoff (it called Lock before our commit) and cannot steal. The
// object write and the ownership claim are therefore mutually exclusive with stealing, so a
// holder whose lease was taken over cannot persist a stale write. Lease locks only (acquired
// WithLease); obj must be the locked object.
//
// Not context-cancellable (it uses db.Begin/tx.Exec like Update), intentionally: the guarded
// write must still land — and decide ownership — even when the caller's context is already
// cancelled (e.g. during scheduler shutdown).
func (l Lock[objT, idT, shardKeyT]) UpdateGuarded(ctx convCtx.Context, obj objT) (err error) {

	if l.owner == "" {
		return fmt.Errorf("convention/db: UpdateGuarded requires a lease lock (use WithLease)")
	}

	tos := l.tos
	if err = tos.prepare(); err != nil {
		return
	}

	key := obj.DBKey()
	if key.ID != l.id {
		return fmt.Errorf("convention/db: UpdateGuarded object id %q does not match locked id %q", key.ID, l.id)
	}

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
			err = errors.Join(err, tx.Rollback())
			return
		}
		err = tx.Commit()
	}()

	// Take the lease row first: this owner-checked renew refreshes the heartbeat AND holds
	// the lease row's write lock for the rest of the transaction, so a concurrent steal blocks
	// until commit (then sees a fresh timestamp and cannot take over). 0 rows ⇒ the lease was
	// already stolen/cleared ⇒ ErrLeaseLost, before any runtime write.
	leaseRes, err := tx.Exec(`UPDATE "`+tos.table.LockTableName+`" SET "created_at"=$1 WHERE "id"=$2 AND "description"=$3`,
		ctx.Now(), l.id, l.owner)
	if err != nil {
		return
	}
	if n, _ := leaseRes.RowsAffected(); n == 0 {
		err = ErrLeaseLost
		return
	}

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

	// Ownership is already guaranteed (and held under the lease row lock) by the renew above,
	// so this is a plain owned write.
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

	// Refresh the lease one last time before commit. The early renew's timestamp could be
	// older than `lease` by now if this transaction ran long; a waiter that called Lock after
	// that early renew computed its cutoff against the wall clock and would, the moment we
	// release the row lock at commit, see a stale created_at and steal — then run the job we
	// just advanced. Renewing here makes the timestamp this waiter observes (it called Lock
	// before our commit, so before this `now`) newer than its cutoff, so it cannot steal. We
	// still hold the row lock, so this matches our own row (1 row).
	_, err = tx.Exec(`UPDATE "`+tos.table.LockTableName+`" SET "created_at"=$1 WHERE "id"=$2 AND "description"=$3`,
		ctx.Now(), l.id, l.owner)
	if err != nil {
		return
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

	// Lease path: steal an expired lock and tag it with this owner.
	now := ctx.Now()
	cutoff := now.Add(-cfg.lease)

	// Per-acquisition owner tag: the caller's desc plus a unique token. Renew/Unlock match
	// the whole stored value, so two acquisitions sharing a description can never be mistaken
	// for the same owner (a stale holder cannot renew/unlock a row a new holder took over).
	ownerTag := desc + ownerTokenSep + uuid.NewString()

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
		key.ID, now, ownerTag, cutoff)
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
		owner: ownerTag,
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
		err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM "`+tos.table.RuntimeTableName+`" WHERE id = $1 AND "object" IS NOT NULL)`, id).
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

		releaseAcquiredLock := func(cause error) error {
			obj = nil
			cleanupErr := lock.Unlock()
			if cleanupErr == nil {
				lock = nil
			} else {
				ctx.Logger().Error(
					"failed to release acquired lock after object load failure",
					"object_id", id,
					"error", cleanupErr,
					"cause", cause,
				)
			}
			return errors.Join(cause, cleanupErr)
		}

		var bytes []byte

		err = db.
			QueryRow(`SELECT "object" FROM "`+tos.table.RuntimeTableName+`" WHERE id=$1 AND "object" IS NOT NULL`, id).
			Scan(&bytes)
		if err == sql.ErrNoRows {
			err = fmt.Errorf("%w: id=%s", ErrObjectNotFound, id)
		}
		if err != nil {
			err = releaseAcquiredLock(err)
			return
		}

		obj = new(objT)
		err = json.Unmarshal(bytes, obj)
		if err != nil {
			err = releaseAcquiredLock(err)
			return
		}

		return
	}
	return
}
