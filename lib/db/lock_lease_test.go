package db_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
)

const testLease = 60 * time.Second

// leaseCtx returns a context whose Now() is pinned, so lease arithmetic is fully
// deterministic (no sleeps).
func leaseCtx(user string, now time.Time) convCtx.Context {
	return convCtx.New(convAuth.Claims{User: convAuth.User(user)}).WithNow(now)
}

func leaseMsg() Message {
	return Message{MessageID: MessageID("lease-" + uuid.NewString())}
}

// A stale lease lock is stolen on acquire, and the steal is reported.
func Test_Lock_StealAfterExpiry(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	msg := leaseMsg()

	lockA, err := messagesDB.Tenant("test").Lock(leaseCtx("ownerA", base), msg, "ownerA", convDB.WithLease(testLease))
	if err != nil || lockA == nil {
		t.Fatalf("ownerA acquire: lock=%v err=%v", lockA, err)
	}
	if lockA.Stolen() {
		t.Fatalf("a fresh acquire must not be marked stolen")
	}

	// Before expiry, a second owner cannot steal.
	early, err := messagesDB.Tenant("test").Lock(leaseCtx("ownerB", base.Add(testLease/2)), msg, "ownerB", convDB.WithLease(testLease))
	if err != nil {
		t.Fatalf("ownerB early acquire err: %v", err)
	}
	if early != nil {
		t.Fatalf("ownerB must not steal a live lock")
	}

	// After expiry, the second owner steals and learns the previous owner.
	lockB, err := messagesDB.Tenant("test").Lock(leaseCtx("ownerB", base.Add(testLease+time.Second)), msg, "ownerB", convDB.WithLease(testLease))
	if err != nil || lockB == nil {
		t.Fatalf("ownerB steal: lock=%v err=%v", lockB, err)
	}
	if !lockB.Stolen() {
		t.Fatalf("expected Stolen()=true after taking over an expired lock")
	}
	if lockB.PreviousOwner() != "ownerA" {
		t.Fatalf("PreviousOwner=%q, want ownerA", lockB.PreviousOwner())
	}

	if err := lockB.Unlock(); err != nil {
		t.Fatalf("ownerB unlock: %v", err)
	}
}

// Renewing keeps the lease alive so a would-be thief sees it as live (RowsAffected==0).
func Test_Lock_RenewPreventsSteal(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	msg := leaseMsg()

	lockA, err := messagesDB.Tenant("test").Lock(leaseCtx("ownerA", base), msg, "ownerA", convDB.WithLease(testLease))
	if err != nil || lockA == nil {
		t.Fatalf("ownerA acquire: lock=%v err=%v", lockA, err)
	}

	// Heartbeat at base+lease/2 moves created_at forward.
	if err := lockA.Renew(leaseCtx("ownerA", base.Add(testLease/2))); err != nil {
		t.Fatalf("renew: %v", err)
	}

	// At base+lease the cutoff is base; the renewed created_at (base+lease/2) is
	// newer, so the thief must fail.
	thief, err := messagesDB.Tenant("test").Lock(leaseCtx("ownerB", base.Add(testLease)), msg, "ownerB", convDB.WithLease(testLease))
	if err != nil {
		t.Fatalf("thief acquire err: %v", err)
	}
	if thief != nil {
		t.Fatalf("renew should have kept the lease alive; thief must not steal")
	}

	if err := lockA.Unlock(); err != nil {
		t.Fatalf("ownerA unlock: %v", err)
	}
}

// A stale owner's Unlock returns ErrLeaseLost and must not remove the new owner's row.
func Test_Lock_OwnerSafeUnlock(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	msg := leaseMsg()

	lockA, err := messagesDB.Tenant("test").Lock(leaseCtx("ownerA", base), msg, "ownerA", convDB.WithLease(testLease))
	if err != nil || lockA == nil {
		t.Fatalf("ownerA acquire: lock=%v err=%v", lockA, err)
	}

	lockB, err := messagesDB.Tenant("test").Lock(leaseCtx("ownerB", base.Add(testLease+time.Second)), msg, "ownerB", convDB.WithLease(testLease))
	if err != nil || lockB == nil {
		t.Fatalf("ownerB steal: lock=%v err=%v", lockB, err)
	}

	// ownerA lost the lease — its Unlock reports it and leaves ownerB's row intact.
	if err := lockA.Unlock(); !errors.Is(err, convDB.ErrLeaseLost) {
		t.Fatalf("stale owner Unlock: got %v, want ErrLeaseLost", err)
	}

	// Probe: ownerB's lock is still live (a fresh thief just past the steal time fails).
	probe, err := messagesDB.Tenant("test").Lock(leaseCtx("ownerC", base.Add(testLease+2*time.Second)), msg, "ownerC", convDB.WithLease(testLease))
	if err != nil {
		t.Fatalf("probe err: %v", err)
	}
	if probe != nil {
		t.Fatalf("ownerA.Unlock must not have removed ownerB's live lock")
	}

	if err := lockB.Unlock(); err != nil {
		t.Fatalf("ownerB unlock: %v", err)
	}
}

// After a steal, the original owner's Renew reports ErrLeaseLost (the trigger the
// scheduler uses to stop advancing the schedule).
func Test_Lock_RenewAfterStealReturnsErrLeaseLost(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	msg := leaseMsg()

	lockA, err := messagesDB.Tenant("test").Lock(leaseCtx("ownerA", base), msg, "ownerA", convDB.WithLease(testLease))
	if err != nil || lockA == nil {
		t.Fatalf("ownerA acquire: lock=%v err=%v", lockA, err)
	}

	lockB, err := messagesDB.Tenant("test").Lock(leaseCtx("ownerB", base.Add(testLease+time.Second)), msg, "ownerB", convDB.WithLease(testLease))
	if err != nil || lockB == nil {
		t.Fatalf("ownerB steal: lock=%v err=%v", lockB, err)
	}

	if err := lockA.Renew(leaseCtx("ownerA", base.Add(testLease+2*time.Second))); !errors.Is(err, convDB.ErrLeaseLost) {
		t.Fatalf("stale Renew: got %v, want ErrLeaseLost", err)
	}

	if err := lockB.Unlock(); err != nil {
		t.Fatalf("ownerB unlock: %v", err)
	}
}
