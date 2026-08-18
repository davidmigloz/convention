package job_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
	convJob "github.com/sofmon/convention/lib/job"
)

// TestRegisterConvergesOnInsertRace simulates a peer replica winning the
// race between this Register call's SelectByID and its own Insert: the hook
// (fired immediately before Register's Insert) commits the same row as if a
// peer replica had gotten there first. Register must converge on the
// racer's persisted row rather than surface the raw duplicate-key error —
// see job.go's Register doc / job.go's applyPersistedJob.
func TestRegisterConvergesOnInsertRace(t *testing.T) {
	ctx := newCtx()
	const jid convJob.JobID = "register-insert-race-converges"
	defer func() { _ = convJob.Unregister(ctx, testTenant, jid) }()

	startAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	const repeat = time.Hour

	convJob.SetRegisterInsertRaceHookForTest(t, func() {
		// Simulated peer replica: commits the same row before our own
		// Insert lands.
		if err := convJob.InsertJobRowForTest(ctx, testTenant, jid, startAt, repeat); err != nil {
			t.Fatalf("racer insert failed: %v", err)
		}
	})

	err := convJob.Register(ctx, testTenant, jid, startAt, repeat, func(convCtx.Context) error { return nil })
	if err != nil {
		t.Fatalf("expected Register to converge on the racer's row, got error: %v", err)
	}

	present, hasClosure := convJob.MemJobClosureForTest(testTenant, jid)
	if !present {
		t.Fatalf("expected an in-memory entry for %s after convergence", jid)
	}
	if !hasClosure {
		t.Fatalf("expected the in-memory entry to carry our closure after convergence")
	}
}

// TestRegisterRetriesOnceWhenRacerRowVanishes covers the edge where the
// racer's row is no longer visible by the time Register re-reads it
// (concurrent Unregister churn): the hook's first firing (before Register's
// initial Insert) leaves a row that still occupies the primary key but
// reads as absent to SelectByID (same SQL-NULL-object idiom as lib/db's
// MutateOrInsert tests) so our own Insert genuinely collides while the
// re-SelectByID sees nothing; the hook's second firing (before the bounded
// retry Insert) raw-deletes that row so the retry can actually land.
func TestRegisterRetriesOnceWhenRacerRowVanishes(t *testing.T) {
	ctx := newCtx()
	const jid convJob.JobID = "register-insert-race-vanishes"
	defer func() { _ = convJob.Unregister(ctx, testTenant, jid) }()

	startAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	const repeat = time.Hour

	calls := 0
	convJob.SetRegisterInsertRaceHookForTest(t, func() {
		calls++
		switch calls {
		case 1:
			if err := convJob.InsertJobRowForTest(ctx, testTenant, jid, startAt, repeat); err != nil {
				t.Fatalf("racer insert failed: %v", err)
			}
			if err := convJob.NullJobRowObjectForTest(testTenant, jid); err != nil {
				t.Fatalf("null racer object failed: %v", err)
			}
		case 2:
			if err := convJob.DeleteJobRowForTest(ctx, testTenant, jid); err != nil {
				t.Fatalf("raw delete racer row failed: %v", err)
			}
		default:
			t.Fatalf("unexpected extra Insert attempt (call #%d) - retry must be bounded to exactly one", calls)
		}
	})

	err := convJob.Register(ctx, testTenant, jid, startAt, repeat, func(convCtx.Context) error { return nil })
	if err != nil {
		t.Fatalf("expected the bounded retry to succeed, got error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected the hook to fire exactly twice (initial + one retry), got %d", calls)
	}

	present, hasClosure := convJob.MemJobClosureForTest(testTenant, jid)
	if !present {
		t.Fatalf("expected an in-memory entry for %s after the retry succeeds", jid)
	}
	if !hasClosure {
		t.Fatalf("expected the in-memory entry to carry our closure after the retry succeeds")
	}
}

// TestRegisterGivesUpAfterOneRetry covers the other side of the bounded
// retry: the row that made our first Insert collide is still there when the
// retry runs, so Register gives up rather than looping. The hook's first
// firing plants a row that occupies the primary key but reads as absent to
// SelectByID (SQL-NULL "object", same idiom as the vanishing-racer test
// above); its second firing does nothing, leaving that row in place so the
// retry collides identically.
//
// The give-up error is deliberately NOT sentinel-wrapped — a caller cannot
// errors.Is it back to ErrDuplicateID and mistake persistent churn for the
// ordinary, converged race — but it must still carry the underlying cause
// for on-call diagnosis.
func TestRegisterGivesUpAfterOneRetry(t *testing.T) {
	ctx := newCtx()
	const jid convJob.JobID = "register-insert-race-gives-up"
	defer func() { _ = convJob.Unregister(ctx, testTenant, jid) }()

	startAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	const repeat = time.Hour

	calls := 0
	convJob.SetRegisterInsertRaceHookForTest(t, func() {
		calls++
		switch calls {
		case 1:
			if err := convJob.InsertJobRowForTest(ctx, testTenant, jid, startAt, repeat); err != nil {
				t.Fatalf("racer insert failed: %v", err)
			}
			if err := convJob.NullJobRowObjectForTest(testTenant, jid); err != nil {
				t.Fatalf("null racer object failed: %v", err)
			}
		case 2:
			// Leave the PK-blocking, reads-as-absent row in place so the
			// bounded retry collides exactly as the first attempt did.
		default:
			t.Fatalf("unexpected extra Insert attempt (call #%d) - retry must be bounded to exactly one", calls)
		}
	})

	err := convJob.Register(ctx, testTenant, jid, startAt, repeat, func(convCtx.Context) error { return nil })
	if err == nil {
		t.Fatalf("expected Register to give up after one retry, got nil error")
	}
	if calls != 2 {
		t.Fatalf("expected the hook to fire exactly twice (initial + one retry), got %d", calls)
	}
	if errors.Is(err, convDB.ErrDuplicateID) {
		t.Fatalf("give-up error must not stay ErrDuplicateID-matchable, got %v", err)
	}
	if !strings.Contains(err.Error(), "giving up after one retry") {
		t.Fatalf("expected the churn message, got %v", err)
	}
	// The cause has to survive into the message: without it the operator
	// cannot tell genuine churn from a misclassified insert failure.
	if !strings.Contains(err.Error(), convDB.ErrDuplicateID.Error()) {
		t.Fatalf("expected the underlying duplicate-key cause in the message, got %v", err)
	}

	if present, _ := convJob.MemJobClosureForTest(testTenant, jid); present {
		t.Fatalf("expected no in-memory entry for %s after giving up", jid)
	}
}
