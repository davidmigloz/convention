package job_test

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCfg "github.com/sofmon/convention/lib/cfg"
	convCtx "github.com/sofmon/convention/lib/ctx"
	convJob "github.com/sofmon/convention/lib/job"
)

const (
	testTenant = "test"
	testVault  = "jobs"
)

func newCtx() convCtx.Context {
	return convCtx.New(convAuth.Claims{
		User: "job_test",
	})
}

func TestMain(m *testing.M) {

	err := convCfg.SetConfigLocation("../../.secret")
	if err != nil {
		panic(fmt.Errorf("SetConfigLocation failed: %w", err))
	}

	ctx := newCtx()

	err = convJob.Initialise(ctx, testVault)
	if err != nil {
		panic(fmt.Errorf("Initialise failed: %w", err))
	}

	code := m.Run()

	_ = convJob.Cancel()

	os.Exit(code)
}

func TestInitialiseAlreadyRunning(t *testing.T) {

	ctx := newCtx()

	err := convJob.Initialise(ctx, testVault)
	if err == nil {
		t.Fatal("expected error on double Initialise")
	}
}

func TestRegisterAndUnregister(t *testing.T) {

	ctx := newCtx()

	err := convJob.Register(ctx, testTenant, "test-job", time.Now().Add(1*time.Hour), 5*time.Minute, func(convCtx.Context) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// Duplicate registration is now idempotent (re-attaches the closure / refreshes
	// the schedule) rather than an error.
	err = convJob.Register(ctx, testTenant, "test-job", time.Now().Add(1*time.Hour), 5*time.Minute, func(convCtx.Context) error {
		return nil
	})
	if err != nil {
		t.Fatalf("duplicate Register should be idempotent, got: %v", err)
	}

	err = convJob.Unregister(ctx, testTenant, "test-job")
	if err != nil {
		t.Fatalf("Unregister failed: %v", err)
	}

	// Unregister again should fail
	err = convJob.Unregister(ctx, testTenant, "test-job")
	if err == nil {
		t.Fatal("expected error on double Unregister")
	}
}

func TestUnregisterNonExistentTenant(t *testing.T) {

	ctx := newCtx()

	err := convJob.Unregister(ctx, "non-existent-tenant", "some-job")
	if err == nil {
		t.Fatal("expected error when unregistering from non-existent tenant")
	}
}

func TestJobExecution(t *testing.T) {

	ctx := newCtx()

	var execCount atomic.Int32

	// Schedule job to run immediately (startAt in the past)
	err := convJob.Register(ctx, testTenant, "exec-job", time.Now().Add(-1*time.Second), 1*time.Hour, func(convCtx.Context) error {
		execCount.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// Wait for the background loop to pick it up (min 1s sleep + execution)
	time.Sleep(3 * time.Second)

	count := execCount.Load()
	if count < 1 {
		t.Fatalf("expected job to execute at least once, got %d executions", count)
	}

	err = convJob.Unregister(ctx, testTenant, "exec-job")
	if err != nil {
		t.Fatalf("Unregister failed: %v", err)
	}
}

func TestJobRepeatExecution(t *testing.T) {

	ctx := newCtx()

	var execCount atomic.Int32

	// Schedule job to run immediately and repeat every 2 seconds
	err := convJob.Register(ctx, testTenant, "repeat-job", time.Now().Add(-1*time.Second), 2*time.Second, func(convCtx.Context) error {
		execCount.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// Wait enough for at least 2 executions
	time.Sleep(6 * time.Second)

	count := execCount.Load()
	if count < 2 {
		t.Fatalf("expected job to execute at least 2 times, got %d executions", count)
	}

	err = convJob.Unregister(ctx, testTenant, "repeat-job")
	if err != nil {
		t.Fatalf("Unregister failed: %v", err)
	}
}

func TestJobErrorDoesNotStopScheduler(t *testing.T) {

	ctx := newCtx()

	var execCount atomic.Int32

	// Register a job that always returns an error
	err := convJob.Register(ctx, testTenant, "error-job", time.Now().Add(-1*time.Second), 2*time.Second, func(convCtx.Context) error {
		execCount.Add(1)
		return fmt.Errorf("intentional test error")
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// Wait for multiple cycles
	time.Sleep(6 * time.Second)

	count := execCount.Load()
	if count < 2 {
		t.Fatalf("expected failing job to still be rescheduled and run at least 2 times, got %d", count)
	}

	err = convJob.Unregister(ctx, testTenant, "error-job")
	if err != nil {
		t.Fatalf("Unregister failed: %v", err)
	}
}

func TestJobPanicRecovery(t *testing.T) {

	ctx := newCtx()

	var execCount atomic.Int32

	// Register a job that panics
	err := convJob.Register(ctx, testTenant, "panic-job", time.Now().Add(-1*time.Second), 2*time.Second, func(convCtx.Context) error {
		execCount.Add(1)
		panic("intentional test panic")
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// Wait for the scheduler to survive the panic and reschedule
	time.Sleep(6 * time.Second)

	count := execCount.Load()
	if count < 2 {
		t.Fatalf("expected panicking job to still be rescheduled and run at least 2 times, got %d", count)
	}

	err = convJob.Unregister(ctx, testTenant, "panic-job")
	if err != nil {
		t.Fatalf("Unregister failed: %v", err)
	}
}
