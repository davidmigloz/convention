package job_test

import (
	"errors"
	"testing"
	"time"

	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
	convJob "github.com/sofmon/convention/lib/job"
)

// The heartbeat classifier: only a confirmed lost lease is fatal; transient
// errors are retried.
func TestRenewOutcome(t *testing.T) {
	if !convJob.RenewOutcomeForTest(convDB.ErrLeaseLost) {
		t.Fatal("ErrLeaseLost must be fatal (stop the job)")
	}
	if convJob.RenewOutcomeForTest(errors.New("transient db blip")) {
		t.Fatal("a transient error must NOT be fatal (heartbeat must retry)")
	}
	if convJob.RenewOutcomeForTest(nil) {
		t.Fatal("nil must not be fatal")
	}
}

// Register is idempotent: a second registration over a nil-closure in-memory entry
// (as syncJobsFromDB would leave) re-attaches the closure instead of erroring.
func TestRegisterIdempotentReattachesClosure(t *testing.T) {
	ctx := newCtx()
	const jid convJob.JobID = "idem-job"
	defer func() { _ = convJob.Unregister(ctx, testTenant, jid) }()

	if err := convJob.InsertJobRowForTest(ctx, testTenant, jid, time.Now().Add(time.Hour), 5*time.Minute); err != nil {
		t.Fatalf("insert job row: %v", err)
	}
	convJob.InjectNilClosureJobForTest(testTenant, jid, time.Now().Add(time.Hour), 5*time.Minute)

	if present, hasClosure := convJob.MemJobClosureForTest(testTenant, jid); !present || hasClosure {
		t.Fatalf("precondition: want present nil-closure entry, got present=%v hasClosure=%v", present, hasClosure)
	}

	err := convJob.Register(ctx, testTenant, jid, time.Now().Add(time.Hour), 5*time.Minute, func(convCtx.Context) error {
		return nil
	})
	if err != nil {
		t.Fatalf("idempotent Register should not error, got: %v", err)
	}

	if present, hasClosure := convJob.MemJobClosureForTest(testTenant, jid); !present || !hasClosure {
		t.Fatalf("after Register want closure attached, got present=%v hasClosure=%v", present, hasClosure)
	}
}

// When the lease is lost mid-execution, executeJob must NOT advance/persist
// next_run_at (the new owner is now responsible for scheduling).
func TestExecuteJobSkipsAdvanceOnLeaseLoss(t *testing.T) {
	ctx := newCtx()
	const jid convJob.JobID = "lease-loss-job"
	t0 := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	if err := convJob.InsertJobRowForTest(ctx, testTenant, jid, t0, time.Hour); err != nil {
		t.Fatalf("insert job row: %v", err)
	}
	defer func() { _ = convJob.Unregister(ctx, testTenant, jid) }()

	// Single in-memory connection so the heartbeat goroutine and the foreign steal
	// share one database (Postgres needs no such pinning).
	if err := convJob.PinSingleConnForTest(convDB.Vault(testVault), testTenant); err != nil {
		t.Fatalf("pin conn: %v", err)
	}

	restore := convJob.SetLeaseForTest(10*time.Second, 10*time.Millisecond)
	defer restore()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// The job body steals its own lock (as a foreign owner), then waits for
		// cancellation — which the heartbeat triggers once its Renew sees the steal.
		convJob.RunJobForTest(ctx, testTenant, jid, t0, time.Hour, func(jctx convCtx.Context) error {
			if err := convJob.StealJobLockForTest(ctx, testTenant, jid, 10*time.Second); err != nil {
				t.Errorf("steal: %v", err)
				return err
			}
			<-jctx.Done()
			return nil
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executeJob did not return; lease-loss cancellation likely not wired")
	}

	got, ok, err := convJob.ReadJobRowNextRunForTest(ctx, testTenant, jid)
	if err != nil || !ok {
		t.Fatalf("read job row: ok=%v err=%v", ok, err)
	}
	if !got.Equal(t0) {
		t.Fatalf("next_run_at advanced despite lease loss: got %v, want %v", got, t0)
	}
}
