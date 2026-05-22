package job_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
	convJob "github.com/sofmon/convention/lib/job"
)

// capturingHandler is a thread-safe slog handler that records message strings
// (the heartbeat goroutine logs concurrently, so it must be safe).
type capturingHandler struct {
	mu   *sync.Mutex
	msgs *[]string
}

func (h capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.msgs = append(*h.msgs, r.Message)
	return nil
}
func (h capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h capturingHandler) WithGroup(string) slog.Handler      { return h }

func capturingCtx(base convCtx.Context) (convCtx.Context, func() []string) {
	var mu sync.Mutex
	var msgs []string
	logger := slog.New(capturingHandler{mu: &mu, msgs: &msgs})
	return base.WithLogger(logger), func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(msgs))
		copy(out, msgs)
		return out
	}
}

func logged(msgs []string, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// A1: interval unchanged keeps the persisted NextRunAt (no clock reset on redeploy);
// an interval change re-anchors NextRunAt to startAt and persists the new interval.
// All times are in the real future so the background loop never executes the job.
func TestRegisterIntervalReanchor(t *testing.T) {
	ctx := newCtx()
	const jid convJob.JobID = "interval-reanchor-job"
	defer func() { _ = convJob.Unregister(ctx, testTenant, jid) }()

	noop := func(convCtx.Context) error { return nil }
	t1 := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	t2 := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	t3 := time.Now().Add(12 * time.Hour).UTC().Truncate(time.Second)

	if err := convJob.Register(ctx, testTenant, jid, t1, time.Hour, noop); err != nil {
		t.Fatalf("initial register: %v", err)
	}
	nr, re, ok, err := convJob.ReadJobRowForTest(ctx, testTenant, jid)
	if err != nil || !ok {
		t.Fatalf("read: ok=%v err=%v", ok, err)
	}
	if !nr.Equal(t1) || re != time.Hour {
		t.Fatalf("initial: nextRunAt=%v repeatEvery=%v", nr, re)
	}

	// Same interval, different startAt -> persisted NextRunAt is kept.
	if err := convJob.Register(ctx, testTenant, jid, t2, time.Hour, noop); err != nil {
		t.Fatalf("same-interval register: %v", err)
	}
	nr, re, _, _ = convJob.ReadJobRowForTest(ctx, testTenant, jid)
	if !nr.Equal(t1) {
		t.Fatalf("same-interval must keep NextRunAt=%v, got %v", t1, nr)
	}
	if re != time.Hour {
		t.Fatalf("same-interval repeatEvery=%v, want 1h", re)
	}

	// Changed (shortened) interval -> re-anchor to startAt + persist new interval.
	if err := convJob.Register(ctx, testTenant, jid, t3, 30*time.Minute, noop); err != nil {
		t.Fatalf("changed-interval register: %v", err)
	}
	nr, re, _, _ = convJob.ReadJobRowForTest(ctx, testTenant, jid)
	if !nr.Equal(t3) {
		t.Fatalf("changed-interval must re-anchor NextRunAt=%v, got %v", t3, nr)
	}
	if re != 30*time.Minute {
		t.Fatalf("changed-interval repeatEvery=%v, want 30m", re)
	}
}

// A2: the completion log fires on a clean run and is ABSENT on j.f error / panic.
// (Lease-loss absence is asserted in TestExecuteJobSkipsAdvanceOnLeaseLoss.)
func TestExecuteJobCompletionLog(t *testing.T) {
	base := newCtx()
	if err := convJob.PinSingleConnForTest(convDB.Vault(testVault), testTenant); err != nil {
		t.Fatalf("pin conn: %v", err)
	}
	t0 := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	run := func(jid convJob.JobID, fn convJob.JobFunc) []string {
		if err := convJob.InsertJobRowForTest(base, testTenant, jid, t0, time.Hour); err != nil {
			t.Fatalf("insert %s: %v", jid, err)
		}
		defer func() { _ = convJob.Unregister(base, testTenant, jid) }()
		ctx, dump := capturingCtx(base)
		convJob.RunJobForTest(ctx, testTenant, jid, t0, time.Hour, fn)
		return dump()
	}

	t.Run("success emits completion", func(t *testing.T) {
		msgs := run("completion-ok", func(convCtx.Context) error { return nil })
		if !logged(msgs, "job execution completed") {
			t.Fatalf("expected completion log; got %v", msgs)
		}
		if logged(msgs, "job execution failed") {
			t.Fatalf("unexpected failure log")
		}
	})

	t.Run("error suppresses completion", func(t *testing.T) {
		msgs := run("completion-err", func(convCtx.Context) error { return errors.New("boom") })
		if logged(msgs, "job execution completed") {
			t.Fatalf("must NOT emit completion on error; got %v", msgs)
		}
		if !logged(msgs, "job execution failed") {
			t.Fatalf("expected failure log; got %v", msgs)
		}
	})

	t.Run("panic suppresses completion", func(t *testing.T) {
		msgs := run("completion-panic", func(convCtx.Context) error { panic("boom") })
		if logged(msgs, "job execution completed") {
			t.Fatalf("must NOT emit completion on panic; got %v", msgs)
		}
		if !logged(msgs, "job panicked") {
			t.Fatalf("expected panic log; got %v", msgs)
		}
	})
}
