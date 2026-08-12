package job

import (
	"fmt"
	"testing"
	"time"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
)

// Test seams (compiled only into the package's test binary).

// RenewOutcomeForTest exposes the heartbeat error classifier.
func RenewOutcomeForTest(err error) bool { return renewOutcome(err) }

// SetLeaseForTest shortens the lease/renew interval for tests and returns a
// restore func.
func SetLeaseForTest(lease, renew time.Duration) (restore func()) {
	ol, or := jobLease, jobRenewInterval
	jobLease, jobRenewInterval = lease, renew
	return func() { jobLease, jobRenewInterval = ol, or }
}

// InjectNilClosureJobForTest mimics syncJobsFromDB pulling a DB row into the
// in-memory map with a nil closure before Register runs.
func InjectNilClosureJobForTest(tenant convAuth.Tenant, jid JobID, nextRunAt time.Time, repeat time.Duration) {
	mut.Lock()
	defer mut.Unlock()
	if jobs == nil {
		jobs = make(map[convAuth.Tenant]map[JobID]job)
	}
	if jobs[tenant] == nil {
		jobs[tenant] = make(map[JobID]job)
	}
	jobs[tenant][jid] = job{ID: jid, NextRunAt: nextRunAt, RepeatEvery: repeat} // f == nil
}

// MemJobClosureForTest reports whether an in-memory entry exists and whether it
// carries an executable closure.
func MemJobClosureForTest(tenant convAuth.Tenant, jid JobID) (present, hasClosure bool) {
	mut.Lock()
	defer mut.Unlock()
	j, ok := jobs[tenant][jid]
	if !ok {
		return false, false
	}
	return true, j.f != nil
}

// InsertJobRowForTest writes a job DB row directly.
func InsertJobRowForTest(ctx convCtx.Context, tenant convAuth.Tenant, jid JobID, nextRunAt time.Time, repeat time.Duration) error {
	return jobsDB.Tenant(tenant).Insert(ctx, job{ID: jid, NextRunAt: nextRunAt, RepeatEvery: repeat})
}

// DeleteJobRowForTest raw-deletes the persisted runtime row for jid via
// jobsDB, without touching the in-memory jobs map — simulates an out-of-band
// peer's Unregister landing between our failed Insert and our retry.
func DeleteJobRowForTest(ctx convCtx.Context, tenant convAuth.Tenant, jid JobID) error {
	return jobsDB.Tenant(tenant).Delete(ctx, jid)
}

// NullJobRowObjectForTest sets the persisted runtime row's "object" column
// to SQL NULL for jid via a raw write, bypassing jobsDB — same raw-write
// pattern as lib/db's own SQL-NULL-object-row tests (mutate_test.go).
// SelectByID then reads the row as absent while its primary key still
// blocks a plain Insert, simulating a racer's row that is mid-teardown but
// not yet fully removed.
func NullJobRowObjectForTest(tenant convAuth.Tenant, jid JobID) error {
	db, err := jobsDB.Tenant(tenant).RawDBForShardKey(jid)
	if err != nil {
		return err
	}
	table := jobsDB.Tenant(tenant).RuntimeTableName()
	_, err = db.Exec(`UPDATE "`+table+`" SET "object"=NULL WHERE "id"=$1`, jid)
	return err
}

// SetRegisterInsertRaceHookForTest installs a hook invoked immediately
// before each Insert attempt Register makes, restoring the previous hook
// (nil in production) via t.Cleanup — same precedent as SetLeaseForTest /
// lib/db's StubMutateBackoffForTest.
func SetRegisterInsertRaceHookForTest(t *testing.T, fn func()) {
	t.Helper()
	old := registerInsertRaceHookForTest
	registerInsertRaceHookForTest = fn
	t.Cleanup(func() { registerInsertRaceHookForTest = old })
}

// ReadJobRowNextRunForTest reads the persisted next_run_at for a job.
func ReadJobRowNextRunForTest(ctx convCtx.Context, tenant convAuth.Tenant, jid JobID) (time.Time, bool, error) {
	sj, err := jobsDB.Tenant(tenant).SelectByID(ctx, jid)
	if err != nil {
		return time.Time{}, false, err
	}
	if sj == nil {
		return time.Time{}, false, nil
	}
	return sj.NextRunAt, true, nil
}

// ReadJobRowForTest reads the persisted schedule (next_run_at + repeat_every) for a job.
func ReadJobRowForTest(ctx convCtx.Context, tenant convAuth.Tenant, jid JobID) (nextRunAt time.Time, repeatEvery time.Duration, ok bool, err error) {
	sj, err := jobsDB.Tenant(tenant).SelectByID(ctx, jid)
	if err != nil {
		return time.Time{}, 0, false, err
	}
	if sj == nil {
		return time.Time{}, 0, false, nil
	}
	return sj.NextRunAt, sj.RepeatEvery, true, nil
}

// StealJobLockForTest forcibly takes the execution lock for jid as a foreign
// owner. A far-future Now() makes the steal succeed regardless of the current
// holder's heartbeat, simulating a competing instance taking over.
func StealJobLockForTest(ctx convCtx.Context, tenant convAuth.Tenant, jid JobID, lease time.Duration) error {
	future := ctx.WithNow(time.Now().Add(24 * time.Hour))
	lock, err := jobsDB.Tenant(tenant).Lock(future, job{ID: jid}, "thief @ test", convDB.WithLease(lease))
	if err != nil {
		return err
	}
	if lock == nil {
		return fmt.Errorf("steal failed: lock unexpectedly still held")
	}
	return nil
}

// RunJobForTest invokes executeJob with a constructed job.
func RunJobForTest(ctx convCtx.Context, tenant convAuth.Tenant, jid JobID, nextRunAt time.Time, repeat time.Duration, fn JobFunc) {
	executeJob(ctx, tenant, job{ID: jid, NextRunAt: nextRunAt, RepeatEvery: repeat, f: fn})
}

// PinSingleConnForTest forces the vault's in-memory sqlite DBs to a single
// connection so concurrent goroutines (the heartbeat + a foreign steal) share one
// in-memory database. Postgres needs no such pinning. Call after the DBs are open
// (e.g. after a first job-row write).
func PinSingleConnForTest(vault convDB.Vault, tenant convAuth.Tenant) error {
	dbs, err := convDB.DBs(vault, tenant)
	if err != nil {
		return err
	}
	for _, db := range dbs {
		db.SetMaxOpenConns(1)
	}
	return nil
}
