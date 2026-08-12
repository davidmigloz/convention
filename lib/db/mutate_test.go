package db_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"reflect"
	"testing"

	"github.com/google/uuid"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
)

var errMutateFnInjected = errors.New("mutate fn injected error")
var errMutateOrInsertSeedInjected = errors.New("mutate-or-insert seed injected error")
var errMutateOrInsertFnInjected = errors.New("mutate-or-insert fn injected error")

// mutateCloneFixture is a dedicated type (registered on the shared "complex"
// vault, same pattern as SplitKeyObject) whose MarshalJSON consults a
// package-level arm flag. It exists solely for clone_marshal_failure_aborts:
// ComplexObject's marshalCount/failOn hook is an unexported field, so it is
// lost on the DB round-trip and would already be nil by the time Mutate
// reloads and clones the row.
type mutateCloneFixtureID string

type mutateCloneFixture struct {
	ID    mutateCloneFixtureID `json:"id"`
	Value string               `json:"value"`
}

func (o mutateCloneFixture) DBKey() convDB.Key[mutateCloneFixtureID, mutateCloneFixtureID] {
	return convDB.Key[mutateCloneFixtureID, mutateCloneFixtureID]{ID: o.ID, ShardKey: o.ID}
}

var errMutateCloneMarshalInjected = errors.New("injected clone marshal failure")

// mutateFailNextMarshal arms/disarms the injected marshal failure. Tests must
// disarm it via t.Cleanup — MarshalJSON does not auto-reset.
var mutateFailNextMarshal bool

func (o mutateCloneFixture) MarshalJSON() ([]byte, error) {
	if mutateFailNextMarshal {
		return nil, errMutateCloneMarshalInjected
	}
	type alias mutateCloneFixture
	return json.Marshal(alias(o))
}

var mutateCloneFixtureDB = convDB.NewObjectSet[mutateCloneFixture]("complex").Ready()

// distinctShardMessageID returns a MessageID that provably routes to a
// different shard of the "messages" vault than avoid, checked (not assumed)
// against the actual shard count via the same crc32.ChecksumIEEE(key)%count
// routing formula the package uses (database.go's indexByShardKey).
func distinctShardMessageID(t *testing.T, avoid MessageID) MessageID {
	t.Helper()

	dbs, err := messagesDB.Tenant("test").RawDBs()
	if err != nil {
		t.Fatalf("RawDBs failed: %v", err)
	}
	shardCount := uint32(len(dbs))
	if shardCount < 2 {
		t.Fatalf("expected the messages vault to have at least 2 shards, got %d", shardCount)
	}

	avoidShard := crc32.ChecksumIEEE([]byte(avoid)) % shardCount
	for {
		candidate := MessageID(uuid.NewString())
		if crc32.ChecksumIEEE([]byte(candidate))%shardCount != avoidShard {
			return candidate
		}
	}
}

func Test_Mutate(t *testing.T) {

	ctx := convCtx.New(convAuth.Claims{User: "Test_Mutate"})

	t.Run("happy_path_no_conflict", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "mutate-happy")

		invocations := 0
		obj, err := complexDB.Tenant("test").Mutate(ctx, from.ComplexID, func(cur ComplexObject) (ComplexObject, error) {
			invocations++
			cur.Title = "mutated-happy"
			return cur, nil
		})
		if err != nil {
			t.Fatalf("Mutate failed: %v", err)
		}
		if invocations != 1 {
			t.Fatalf("expected fn invoked exactly once, got %d", invocations)
		}

		got, err := complexDB.Tenant("test").SelectByID(ctx, from.ComplexID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Title != "mutated-happy" {
			t.Fatalf("expected mutated-happy, got %+v", got)
		}
		if !reflect.DeepEqual(obj, *got) {
			t.Fatalf("returned object does not match persisted object:\nreturned=%+v\npersisted=%+v", obj, *got)
		}
	})

	t.Run("retries_once_on_cas_conflict", func(t *testing.T) {
		convDB.StubMutateBackoffForTest(t)
		from := newComplexFixture(t, ctx, "mutate-retry")

		invocations := 0
		obj, err := complexDB.Tenant("test").Mutate(ctx, from.ComplexID, func(cur ComplexObject) (ComplexObject, error) {
			invocations++
			if invocations == 1 {
				// Test machinery, not a caller pattern: Mutate holds no open
				// transaction while fn runs (SafeUpdate's tx opens only after
				// fn returns), and the harness's single-connection sqlite
				// pool (database.go's SetMaxOpenConns(1)) guarantees this
				// out-of-band write lands on the same in-memory DB Mutate is
				// about to re-read. Real callers must not do DB work in fn.
				racer := cur
				racer.Title = "raced-by-mutate"
				if err := complexDB.Tenant("test").Update(ctx, racer); err != nil {
					t.Fatalf("racer Update failed: %v", err)
				}
			}
			cur.Nested.Count = cur.Nested.Count + 1
			return cur, nil
		})
		if err != nil {
			t.Fatalf("Mutate failed: %v", err)
		}
		if invocations != 2 {
			t.Fatalf("expected fn invoked exactly twice, got %d", invocations)
		}

		got, err := complexDB.Tenant("test").SelectByID(ctx, from.ComplexID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Title != "raced-by-mutate" {
			t.Fatalf("expected racer's title change to persist, got %+v", got)
		}
		if got.Nested.Count != from.Nested.Count+1 {
			t.Fatalf("expected fn's change to persist too, got %+v", got)
		}
		if !reflect.DeepEqual(obj, *got) {
			t.Fatalf("returned object does not match persisted object:\nreturned=%+v\npersisted=%+v", obj, *got)
		}
	})

	t.Run("fn_error_aborts_without_retry", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "mutate-fn-error")

		invocations := 0
		obj, err := complexDB.Tenant("test").Mutate(ctx, from.ComplexID, func(cur ComplexObject) (ComplexObject, error) {
			invocations++
			return cur, errMutateFnInjected
		})
		if !errors.Is(err, errMutateFnInjected) {
			t.Fatalf("expected injected fn error, got %v", err)
		}
		var zero ComplexObject
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
		if invocations != 1 {
			t.Fatalf("expected fn invoked exactly once, got %d", invocations)
		}

		got, err := complexDB.Tenant("test").SelectByID(ctx, from.ComplexID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Title != from.Title {
			t.Fatalf("row should be unchanged, got %+v", got)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		invocations := 0
		obj, err := complexDB.Tenant("test").Mutate(ctx, ComplexID(uuid.NewString()), func(cur ComplexObject) (ComplexObject, error) {
			invocations++
			return cur, nil
		})
		if !errors.Is(err, convDB.ErrObjectNotFound) {
			t.Fatalf("expected ErrObjectNotFound, got %v", err)
		}
		var zero ComplexObject
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
		if invocations != 0 {
			t.Fatalf("expected fn invoked 0 times, got %d", invocations)
		}
	})

	t.Run("nil_fn_returns_error", func(t *testing.T) {
		// The previously-panicking shape: mutateLoop's update branch calls
		// fn(*cur) directly, so a nil fn against an EXISTING row panicked
		// before the guard existed. Now it is a plain error.
		from := newComplexFixture(t, ctx, "mutate-nil-fn")

		obj, err := complexDB.Tenant("test").Mutate(ctx, from.ComplexID, nil)
		if err == nil {
			t.Fatalf("expected an error for a nil fn, got success obj=%+v", obj)
		}
		var zero ComplexObject
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
	})

	t.Run("exhausted_attempts_returns_cas_conflict", func(t *testing.T) {
		convDB.StubMutateBackoffForTest(t)
		from := newComplexFixture(t, ctx, "mutate-exhaust")

		invocations := 0
		obj, err := complexDB.Tenant("test").Mutate(ctx, from.ComplexID, func(cur ComplexObject) (ComplexObject, error) {
			invocations++
			// Test machinery (see retries_once_on_cas_conflict): races the
			// CAS baseline on every attempt so Mutate exhausts all
			// mutateMaxAttempts retries instead of converging.
			racer := cur
			racer.Title = fmt.Sprintf("raced-%d", invocations)
			if err := complexDB.Tenant("test").Update(ctx, racer); err != nil {
				t.Fatalf("racer Update failed: %v", err)
			}
			cur.Title = "attempted"
			return cur, nil
		})
		if !errors.Is(err, convDB.ErrCASConflict) {
			t.Fatalf("expected ErrCASConflict after exhaustion, got %v", err)
		}
		var zero ComplexObject
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
		if invocations != 5 {
			t.Fatalf("expected fn invoked mutateMaxAttempts(5) times, got %d", invocations)
		}
	})

	t.Run("context_cancellation_before_first_attempt", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "mutate-pre-cancel")

		cancellable, cancel := context.WithCancel(context.Background())
		cancel()
		cctx := convCtx.WrapContext(cancellable, convAuth.Claims{User: "Test_Mutate_pre_cancel"})

		invocations := 0
		obj, err := complexDB.Tenant("test").Mutate(cctx, from.ComplexID, func(cur ComplexObject) (ComplexObject, error) {
			invocations++
			return cur, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		var zero ComplexObject
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
		if invocations != 0 {
			t.Fatalf("expected fn invoked 0 times, got %d", invocations)
		}
	})

	t.Run("context_cancellation_during_backoff", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "mutate-cancel-backoff")

		cancellable, cancel := context.WithCancel(context.Background())
		defer cancel()
		cctx := convCtx.WrapContext(cancellable, convAuth.Claims{User: "Test_Mutate_cancel_backoff"})

		invocations := 0
		obj, err := complexDB.Tenant("test").Mutate(cctx, from.ComplexID, func(cur ComplexObject) (ComplexObject, error) {
			invocations++
			// Test machinery (see retries_once_on_cas_conflict): force a CAS
			// conflict AND cancel the context inside the callback, so
			// cancellation deterministically precedes the backoff wait
			// Mutate is about to enter (no race with the timer).
			racer := cur
			racer.Title = "raced-cancel"
			if err := complexDB.Tenant("test").Update(cctx, racer); err != nil {
				t.Fatalf("racer Update failed: %v", err)
			}
			cancel()
			cur.Title = "attempted-cancel"
			return cur, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		if !errors.Is(err, convDB.ErrCASConflict) {
			t.Fatalf("expected ErrCASConflict, got %v", err)
		}
		var zero ComplexObject
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
		if invocations != 1 {
			t.Fatalf("expected fn invoked exactly once, got %d", invocations)
		}
	})

	t.Run("in_place_mutation_keeps_baseline", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "mutate-in-place")

		invocations := 0
		obj, err := complexDB.Tenant("test").Mutate(ctx, from.ComplexID, func(cur ComplexObject) (ComplexObject, error) {
			invocations++
			// In-place mutation of cur's slice/map fields. Red-first proof of
			// the deep clone: without it, this mutation corrupts the shared
			// backing array of the CAS baseline `from`, every attempt
			// false-conflicts, and this test fails by exhaustion.
			cur.Tags[0] = "mutated-in-place"
			cur.Attrs["k1"] = "mutated-in-place"
			return cur, nil
		})
		if err != nil {
			t.Fatalf("Mutate failed (deep clone missing?): %v", err)
		}
		if invocations != 1 {
			t.Fatalf("expected fn invoked exactly once, got %d", invocations)
		}

		got, err := complexDB.Tenant("test").SelectByID(ctx, from.ComplexID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Tags[0] != "mutated-in-place" || got.Attrs["k1"] != "mutated-in-place" {
			t.Fatalf("in-place mutation not persisted, got %+v", got)
		}
		if !reflect.DeepEqual(obj, *got) {
			t.Fatalf("returned object does not match persisted object:\nreturned=%+v\npersisted=%+v", obj, *got)
		}
	})

	t.Run("clone_marshal_failure_aborts", func(t *testing.T) {
		obj := mutateCloneFixture{ID: mutateCloneFixtureID(uuid.NewString()), Value: "orig"}
		if err := mutateCloneFixtureDB.Tenant("test").Insert(ctx, obj); err != nil {
			t.Fatalf("Insert failed: %v", err)
		}

		mutateFailNextMarshal = true
		t.Cleanup(func() { mutateFailNextMarshal = false })

		invocations := 0
		got, err := mutateCloneFixtureDB.Tenant("test").Mutate(ctx, obj.ID, func(cur mutateCloneFixture) (mutateCloneFixture, error) {
			invocations++
			cur.Value = "mutated"
			return cur, nil
		})
		if !errors.Is(err, errMutateCloneMarshalInjected) {
			t.Fatalf("expected injected clone marshal error, got %v", err)
		}
		var zero mutateCloneFixture
		if !reflect.DeepEqual(got, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", got)
		}
		if invocations != 0 {
			t.Fatalf("expected fn invoked 0 times, got %d", invocations)
		}
	})

	t.Run("shard_key_passthrough", func(t *testing.T) {
		msg := Message{MessageID: MessageID(uuid.NewString()), Content: "shard-orig"}
		if err := messagesDB.Tenant("test").Insert(ctx, msg); err != nil {
			t.Fatalf("Insert failed: %v", err)
		}

		invocations := 0
		obj, err := messagesDB.Tenant("test").Mutate(ctx, msg.MessageID, func(cur Message) (Message, error) {
			invocations++
			cur.Content = "shard-updated"
			return cur, nil
		}, msg.MessageID)
		if err != nil {
			t.Fatalf("Mutate failed: %v", err)
		}
		if invocations != 1 {
			t.Fatalf("expected fn invoked exactly once, got %d", invocations)
		}

		got, err := messagesDB.Tenant("test").SelectByID(ctx, msg.MessageID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Content != "shard-updated" {
			t.Fatalf("expected shard-updated, got %+v", got)
		}
		if !reflect.DeepEqual(obj, *got) {
			t.Fatalf("returned object does not match persisted object:\nreturned=%+v\npersisted=%+v", obj, *got)
		}

		// messagesDB is shared with process_test.go's exact-count assertions;
		// clean up so this fixture does not leak into them (mirrors
		// Test_Update's end-of-test Delete loop).
		if err := messagesDB.Tenant("test").Delete(ctx, msg.MessageID); err != nil {
			t.Fatalf("cleanup Delete failed: %v", err)
		}
	})

	t.Run("returns_persisted_object", func(t *testing.T) {
		msg := Message{MessageID: MessageID(uuid.NewString()), Content: "returns-persisted-orig"}
		if err := messagesDB.Tenant("test").Insert(ctx, msg); err != nil {
			t.Fatalf("Insert failed: %v", err)
		}
		t.Cleanup(func() {
			if err := messagesDB.Tenant("test").Delete(ctx, msg.MessageID); err != nil {
				t.Errorf("cleanup Delete failed: %v", err)
			}
		})

		before, err := messagesDB.Tenant("test").SelectByIDWithMetadata(ctx, msg.MessageID)
		if err != nil || before == nil {
			t.Fatalf("SelectByIDWithMetadata (before) failed: %v", err)
		}

		obj, err := messagesDB.Tenant("test").Mutate(ctx, msg.MessageID, func(cur Message) (Message, error) {
			cur.Content = "returns-persisted-updated"
			return cur, nil
		})
		if err != nil {
			t.Fatalf("Mutate failed: %v", err)
		}

		after, err := messagesDB.Tenant("test").SelectByIDWithMetadata(ctx, msg.MessageID)
		if err != nil || after == nil {
			t.Fatalf("SelectByIDWithMetadata (after) failed: %v", err)
		}

		// Red-first proof of the post-write re-read: if Mutate instead
		// returned fn's pre-compute `to` verbatim, obj.UpdatedAt would carry
		// the PRE-WRITE stamp already present on `cur` (compute hooks stamp
		// on every load, not just on write) — NOT the zero value. The
		// assertions below catch this via stamp-equality checks (must equal
		// the fresh post-write stamp, must differ from the pre-write one),
		// not via zero-ness.
		if !obj.UpdatedAt.Equal(after.Metadata.UpdatedAt) {
			t.Fatalf("expected returned UpdatedAt to equal persisted UpdatedAt, got %v vs %v", obj.UpdatedAt, after.Metadata.UpdatedAt)
		}
		if obj.UpdatedAt.Equal(before.Metadata.UpdatedAt) {
			t.Fatalf("expected returned UpdatedAt to differ from the pre-write stamp, both %v", obj.UpdatedAt)
		}
	})

	t.Run("noop_fn_skips_write", func(t *testing.T) {
		msg := Message{MessageID: MessageID(uuid.NewString()), Content: "noop-orig"}
		if err := messagesDB.Tenant("test").Insert(ctx, msg); err != nil {
			t.Fatalf("Insert failed: %v", err)
		}
		t.Cleanup(func() {
			if err := messagesDB.Tenant("test").Delete(ctx, msg.MessageID); err != nil {
				t.Errorf("cleanup Delete failed: %v", err)
			}
		})

		before, err := messagesDB.Tenant("test").SelectByIDWithMetadata(ctx, msg.MessageID)
		if err != nil || before == nil {
			t.Fatalf("SelectByIDWithMetadata (before) failed: %v", err)
		}

		invocations := 0
		obj, err := messagesDB.Tenant("test").Mutate(ctx, msg.MessageID, func(cur Message) (Message, error) {
			invocations++
			return cur, nil // identity: no change
		})
		if err != nil {
			t.Fatalf("Mutate failed: %v", err)
		}
		if invocations != 1 {
			t.Fatalf("expected fn invoked exactly once, got %d", invocations)
		}

		after, err := messagesDB.Tenant("test").SelectByIDWithMetadata(ctx, msg.MessageID)
		if err != nil || after == nil {
			t.Fatalf("SelectByIDWithMetadata (after) failed: %v", err)
		}
		if !after.Metadata.UpdatedAt.Equal(before.Metadata.UpdatedAt) {
			t.Fatalf("expected updated_at unchanged by no-op skip, before=%v after=%v", before.Metadata.UpdatedAt, after.Metadata.UpdatedAt)
		}
		if !reflect.DeepEqual(obj, after.Object) {
			t.Fatalf("returned object does not match stored object:\nreturned=%+v\nstored=%+v", obj, after.Object)
		}
	})

	t.Run("noop_revert_detected", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "mutate-noop-revert")

		before, err := complexDB.Tenant("test").SelectByIDWithMetadata(ctx, from.ComplexID)
		if err != nil || before == nil {
			t.Fatalf("SelectByIDWithMetadata (before) failed: %v", err)
		}

		invocations := 0
		obj, err := complexDB.Tenant("test").Mutate(ctx, from.ComplexID, func(cur ComplexObject) (ComplexObject, error) {
			invocations++
			// Mutate map/slice fields then revert to equal values —
			// canonical marshal (sorted map keys) must still detect this as
			// a no-op.
			origTag := cur.Tags[0]
			origAttr := cur.Attrs["k1"]
			cur.Tags[0] = "temporarily-different"
			cur.Attrs["k1"] = "temporarily-different"
			cur.Tags[0] = origTag
			cur.Attrs["k1"] = origAttr
			return cur, nil
		})
		if err != nil {
			t.Fatalf("Mutate failed: %v", err)
		}
		if invocations != 1 {
			t.Fatalf("expected fn invoked exactly once, got %d", invocations)
		}

		after, err := complexDB.Tenant("test").SelectByIDWithMetadata(ctx, from.ComplexID)
		if err != nil || after == nil {
			t.Fatalf("SelectByIDWithMetadata (after) failed: %v", err)
		}
		if !after.Metadata.UpdatedAt.Equal(before.Metadata.UpdatedAt) {
			t.Fatalf("expected updated_at unchanged by reverted no-op, before=%v after=%v", before.Metadata.UpdatedAt, after.Metadata.UpdatedAt)
		}
		if !reflect.DeepEqual(obj, after.Object) {
			t.Fatalf("returned object does not match stored object:\nreturned=%+v\nstored=%+v", obj, after.Object)
		}
	})

	t.Run("noop_skip_returns_unaliased_row", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "mutate-noop-unaliased")

		before, err := complexDB.Tenant("test").SelectByIDWithMetadata(ctx, from.ComplexID)
		if err != nil || before == nil {
			t.Fatalf("SelectByIDWithMetadata (before) failed: %v", err)
		}

		invocations := 0
		obj, err := complexDB.Tenant("test").Mutate(ctx, from.ComplexID, func(cur ComplexObject) (ComplexObject, error) {
			invocations++
			// Poison cur's slice/map fields in place — cur shares backing
			// storage with mutateLoop's own *cur (the no-op skip's aliasing
			// hazard), NOT with the deep-cloned `from` baseline built before
			// this call. Then return a SEPARATELY reconstructed object
			// (never derived from cur) that is byte-identical to the row as
			// read, so the no-op skip must fire. Red-first proof that the
			// skip's returned value is the pristine clone, not an alias of
			// this poisoned cur: pre-fix, the skip returned *cur verbatim
			// and this poisoning would leak into the result even though the
			// skip fired because "nothing changed".
			cur.Tags[0] = "poisoned"
			cur.Attrs["k1"] = "poisoned"
			return ComplexObject{
				ComplexID: from.ComplexID,
				Title:     from.Title,
				Nested:    ComplexNested{Label: from.Nested.Label, Count: from.Nested.Count},
				Tags:      []string{"a", "b", "c"},
				Attrs:     map[string]string{"k1": "v1", "k2": "v2"},
			}, nil
		})
		if err != nil {
			t.Fatalf("Mutate failed: %v", err)
		}
		if invocations != 1 {
			t.Fatalf("expected fn invoked exactly once, got %d", invocations)
		}
		if !reflect.DeepEqual(obj, from) {
			t.Fatalf("expected the pristine original row (unaliased), got %+v want %+v", obj, from)
		}
		if obj.Tags[0] == "poisoned" || obj.Attrs["k1"] == "poisoned" {
			t.Fatalf("no-op skip returned a row aliased to fn's in-place mutation: %+v", obj)
		}

		after, err := complexDB.Tenant("test").SelectByIDWithMetadata(ctx, from.ComplexID)
		if err != nil || after == nil {
			t.Fatalf("SelectByIDWithMetadata (after) failed: %v", err)
		}
		if !after.Metadata.UpdatedAt.Equal(before.Metadata.UpdatedAt) {
			t.Fatalf("expected updated_at unchanged by no-op skip, before=%v after=%v", before.Metadata.UpdatedAt, after.Metadata.UpdatedAt)
		}
	})

	t.Run("real_change_still_writes", func(t *testing.T) {
		msg := Message{MessageID: MessageID(uuid.NewString()), Content: "real-change-orig"}
		if err := messagesDB.Tenant("test").Insert(ctx, msg); err != nil {
			t.Fatalf("Insert failed: %v", err)
		}
		t.Cleanup(func() {
			if err := messagesDB.Tenant("test").Delete(ctx, msg.MessageID); err != nil {
				t.Errorf("cleanup Delete failed: %v", err)
			}
		})

		before, err := messagesDB.Tenant("test").SelectByIDWithMetadata(ctx, msg.MessageID)
		if err != nil || before == nil {
			t.Fatalf("SelectByIDWithMetadata (before) failed: %v", err)
		}

		obj, err := messagesDB.Tenant("test").Mutate(ctx, msg.MessageID, func(cur Message) (Message, error) {
			cur.Content = "real-change-updated"
			return cur, nil
		})
		if err != nil {
			t.Fatalf("Mutate failed: %v", err)
		}

		after, err := messagesDB.Tenant("test").SelectByIDWithMetadata(ctx, msg.MessageID)
		if err != nil || after == nil {
			t.Fatalf("SelectByIDWithMetadata (after) failed: %v", err)
		}
		if !after.Metadata.UpdatedAt.After(before.Metadata.UpdatedAt) {
			t.Fatalf("expected updated_at to advance, before=%v after=%v", before.Metadata.UpdatedAt, after.Metadata.UpdatedAt)
		}
		if !reflect.DeepEqual(obj, after.Object) {
			t.Fatalf("returned object does not match stored object:\nreturned=%+v\nstored=%+v", obj, after.Object)
		}
	})

	t.Run("delete_race_aborts_not_found", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "mutate-delete-race")

		invocations := 0
		obj, err := complexDB.Tenant("test").Mutate(ctx, from.ComplexID, func(cur ComplexObject) (ComplexObject, error) {
			invocations++
			// Test machinery (see retries_once_on_cas_conflict): a racer
			// deletes the row between Mutate's read and SafeUpdate's guarded
			// read. Must-exist semantics: Mutate aborts with
			// ErrObjectNotFound — only MutateOrInsert absorbs this race.
			if err := complexDB.Tenant("test").Delete(ctx, from.ComplexID); err != nil {
				t.Fatalf("racer Delete failed: %v", err)
			}
			cur.Title = "attempted-after-delete" // differ from baseline so the no-op skip cannot bypass SafeUpdate
			return cur, nil
		})
		if !errors.Is(err, convDB.ErrObjectNotFound) {
			t.Fatalf("expected ErrObjectNotFound, got %v", err)
		}
		var zero ComplexObject
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
		if invocations != 1 {
			t.Fatalf("expected fn invoked exactly once (no retry), got %d", invocations)
		}
	})
}

func Test_MutateOrInsert(t *testing.T) {

	ctx := convCtx.New(convAuth.Claims{User: "Test_MutateOrInsert"})

	t.Run("insert_when_missing", func(t *testing.T) {
		id := MessageID(uuid.NewString())

		seedInvocations := 0
		fnInvocations := 0
		obj, err := messagesDB.Tenant("test").MutateOrInsert(ctx, id,
			func() (Message, error) {
				seedInvocations++
				return Message{MessageID: id, Content: "insert-when-missing-seeded"}, nil
			},
			func(cur Message) (Message, error) {
				fnInvocations++
				cur.Content = "insert-when-missing-final"
				return cur, nil
			},
		)
		if err != nil {
			t.Fatalf("MutateOrInsert failed: %v", err)
		}
		t.Cleanup(func() {
			if err := messagesDB.Tenant("test").Delete(ctx, id); err != nil {
				t.Errorf("cleanup Delete failed: %v", err)
			}
		})
		if seedInvocations != 1 {
			t.Fatalf("expected seed invoked exactly once, got %d", seedInvocations)
		}
		if fnInvocations != 1 {
			t.Fatalf("expected fn invoked exactly once, got %d", fnInvocations)
		}

		got, err := messagesDB.Tenant("test").SelectByIDWithMetadata(ctx, id)
		if err != nil || got == nil {
			t.Fatalf("SelectByIDWithMetadata failed: %v", err)
		}
		if got.Object.Content != "insert-when-missing-final" {
			t.Fatalf("expected insert-when-missing-final, got %+v", got.Object)
		}
		if !reflect.DeepEqual(obj, got.Object) {
			t.Fatalf("returned object does not match persisted object:\nreturned=%+v\npersisted=%+v", obj, got.Object)
		}
		// "incl. stamps": compute-hook stamps must be present on the
		// returned object too, not just on the persisted row.
		if obj.CreatedAt.IsZero() || obj.UpdatedAt.IsZero() {
			t.Fatalf("expected returned object to carry compute-hook stamps, got %+v", obj)
		}
	})

	t.Run("existing_row_updates", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "moi-existing")

		seedInvocations := 0
		fnInvocations := 0
		obj, err := complexDB.Tenant("test").MutateOrInsert(ctx, from.ComplexID,
			func() (ComplexObject, error) {
				seedInvocations++
				t.Fatalf("seed should not be invoked for an existing row")
				return ComplexObject{}, nil
			},
			func(cur ComplexObject) (ComplexObject, error) {
				fnInvocations++
				cur.Title = "moi-existing-updated"
				return cur, nil
			},
		)
		if err != nil {
			t.Fatalf("MutateOrInsert failed: %v", err)
		}
		if seedInvocations != 0 {
			t.Fatalf("expected seed invoked 0 times, got %d", seedInvocations)
		}
		if fnInvocations != 1 {
			t.Fatalf("expected fn invoked exactly once, got %d", fnInvocations)
		}

		got, err := complexDB.Tenant("test").SelectByID(ctx, from.ComplexID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Title != "moi-existing-updated" {
			t.Fatalf("expected moi-existing-updated, got %+v", got)
		}
		if !reflect.DeepEqual(obj, *got) {
			t.Fatalf("returned object does not match persisted object:\nreturned=%+v\npersisted=%+v", obj, *got)
		}
	})

	t.Run("seed_error_aborts_no_row", func(t *testing.T) {
		id := ComplexID(uuid.NewString())

		seedInvocations := 0
		fnInvocations := 0
		obj, err := complexDB.Tenant("test").MutateOrInsert(ctx, id,
			func() (ComplexObject, error) {
				seedInvocations++
				return ComplexObject{}, errMutateOrInsertSeedInjected
			},
			func(cur ComplexObject) (ComplexObject, error) {
				fnInvocations++
				return cur, nil
			},
		)
		if !errors.Is(err, errMutateOrInsertSeedInjected) {
			t.Fatalf("expected injected seed error, got %v", err)
		}
		var zero ComplexObject
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
		if seedInvocations != 1 {
			t.Fatalf("expected seed invoked exactly once, got %d", seedInvocations)
		}
		if fnInvocations != 0 {
			t.Fatalf("expected fn invoked 0 times, got %d", fnInvocations)
		}

		got, err := complexDB.Tenant("test").SelectByID(ctx, id)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got != nil {
			t.Fatalf("expected no row created, got %+v", got)
		}
	})

	t.Run("fn_error_on_insert_branch_aborts_no_row", func(t *testing.T) {
		id := ComplexID(uuid.NewString())

		seedInvocations := 0
		fnInvocations := 0
		obj, err := complexDB.Tenant("test").MutateOrInsert(ctx, id,
			func() (ComplexObject, error) {
				seedInvocations++
				return ComplexObject{ComplexID: id, Title: "seeded"}, nil
			},
			func(cur ComplexObject) (ComplexObject, error) {
				fnInvocations++
				return cur, errMutateOrInsertFnInjected
			},
		)
		if !errors.Is(err, errMutateOrInsertFnInjected) {
			t.Fatalf("expected injected fn error, got %v", err)
		}
		var zero ComplexObject
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
		if seedInvocations != 1 {
			t.Fatalf("expected seed invoked exactly once, got %d", seedInvocations)
		}
		if fnInvocations != 1 {
			t.Fatalf("expected fn invoked exactly once, got %d", fnInvocations)
		}

		got, err := complexDB.Tenant("test").SelectByID(ctx, id)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got != nil {
			t.Fatalf("expected no row created, got %+v", got)
		}
	})

	t.Run("id_mismatch_on_insert_branch_aborts_no_row", func(t *testing.T) {
		id := ComplexID(uuid.NewString())
		wrongID := ComplexID(uuid.NewString())

		seedInvocations := 0
		fnInvocations := 0
		obj, err := complexDB.Tenant("test").MutateOrInsert(ctx, id,
			func() (ComplexObject, error) {
				seedInvocations++
				return ComplexObject{ComplexID: id, Title: "seeded"}, nil
			},
			func(cur ComplexObject) (ComplexObject, error) {
				fnInvocations++
				cur.ComplexID = wrongID // triggers the DBKey().ID != id guard
				return cur, nil
			},
		)
		if err == nil {
			t.Fatalf("expected an error for an ID-mismatched insert, got success obj=%+v", obj)
		}
		if errors.Is(err, convDB.ErrCASConflict) || errors.Is(err, convDB.ErrObjectNotFound) || errors.Is(err, convDB.ErrDuplicateID) {
			t.Fatalf("expected a plain guard error, not a sentinel, got %v", err)
		}
		var zero ComplexObject
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
		if seedInvocations != 1 {
			t.Fatalf("expected seed invoked exactly once, got %d", seedInvocations)
		}
		if fnInvocations != 1 {
			t.Fatalf("expected fn invoked exactly once (no retry), got %d", fnInvocations)
		}

		got, err := complexDB.Tenant("test").SelectByID(ctx, id)
		if err != nil {
			t.Fatalf("SelectByID(id) failed: %v", err)
		}
		if got != nil {
			t.Fatalf("expected no row created under id, got %+v", got)
		}
		got, err = complexDB.Tenant("test").SelectByID(ctx, wrongID)
		if err != nil {
			t.Fatalf("SelectByID(wrongID) failed: %v", err)
		}
		if got != nil {
			t.Fatalf("expected no row created under wrongID either, got %+v", got)
		}
	})

	t.Run("nil_fn_returns_error", func(t *testing.T) {
		// The previously-panicking shape: mutateLoop's insert branch calls
		// fn(base) directly, so a nil fn against a MISSING id (with a
		// non-nil seed available to run) panicked before the guard existed.
		// Now it is a plain error, and seed never runs — the guard fires
		// before any I/O.
		id := ComplexID(uuid.NewString())

		seedInvocations := 0
		obj, err := complexDB.Tenant("test").MutateOrInsert(ctx, id,
			func() (ComplexObject, error) {
				seedInvocations++
				return ComplexObject{ComplexID: id, Title: "seeded"}, nil
			},
			nil,
		)
		if err == nil {
			t.Fatalf("expected an error for a nil fn, got success obj=%+v", obj)
		}
		var zero ComplexObject
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
		if seedInvocations != 0 {
			t.Fatalf("expected seed invoked 0 times (nil fn guard fires before any I/O), got %d", seedInvocations)
		}

		got, err := complexDB.Tenant("test").SelectByID(ctx, id)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got != nil {
			t.Fatalf("expected no row created, got %+v", got)
		}
	})

	t.Run("shard_key_mismatch_on_insert_branch_aborts_no_row", func(t *testing.T) {
		// ComplexObject can't express this (ID == ShardKey there); use
		// SplitKeyObject, whose ID and ShardKey are distinct fields.
		id := SplitID(uuid.NewString())

		seedInvocations := 0
		fnInvocations := 0
		obj, err := splitKeyDB.Tenant("test").MutateOrInsert(ctx, id,
			func() (SplitKeyObject, error) {
				seedInvocations++
				return SplitKeyObject{SplitID: id, SplitShard: SplitShard("shard-a"), Payload: "seeded"}, nil
			},
			func(cur SplitKeyObject) (SplitKeyObject, error) {
				fnInvocations++
				cur.SplitShard = SplitShard("shard-b") // triggers the ShardKey guard
				return cur, nil
			},
		)
		if err == nil {
			t.Fatalf("expected an error for a shard-key-mismatched insert, got success obj=%+v", obj)
		}
		if errors.Is(err, convDB.ErrCASConflict) || errors.Is(err, convDB.ErrObjectNotFound) || errors.Is(err, convDB.ErrDuplicateID) {
			t.Fatalf("expected a plain guard error, not a sentinel, got %v", err)
		}
		var zero SplitKeyObject
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
		if seedInvocations != 1 {
			t.Fatalf("expected seed invoked exactly once, got %d", seedInvocations)
		}
		if fnInvocations != 1 {
			t.Fatalf("expected fn invoked exactly once (no retry), got %d", fnInvocations)
		}

		got, err := splitKeyDB.Tenant("test").SelectByID(ctx, id)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got != nil {
			t.Fatalf("expected no row created under id on any shard, got %+v", got)
		}
	})

	t.Run("null_object_row_exhausts_with_duplicate_id", func(t *testing.T) {
		convDB.StubMutateBackoffForTest(t)

		from := newComplexFixture(t, ctx, "moi-null-object")
		id := from.ComplexID

		db, err := complexDB.Tenant("test").RawDBForShardKey(id)
		if err != nil {
			t.Fatalf("RawDBForShardKey failed: %v", err)
		}
		table := complexDB.Tenant("test").RuntimeTableName()
		res, err := db.Exec(`UPDATE "`+table+`" SET "object"=NULL WHERE "id"=$1`, id)
		if err != nil {
			t.Fatalf("set runtime object to SQL NULL: %v", err)
		}
		if affected, err := res.RowsAffected(); err != nil {
			t.Fatalf("read affected rows: %v", err)
		} else if affected != 1 {
			t.Fatalf("set %d runtime objects to SQL NULL, want 1", affected)
		}

		seedInvocations := 0
		fnInvocations := 0
		obj, err := complexDB.Tenant("test").MutateOrInsert(ctx, id,
			func() (ComplexObject, error) {
				seedInvocations++
				return ComplexObject{ComplexID: id, Title: "resurrected"}, nil
			},
			func(cur ComplexObject) (ComplexObject, error) {
				fnInvocations++
				return cur, nil
			},
		)
		// SelectByID treats the SQL-NULL-object row as absent, so every
		// attempt takes the insert branch and collides with the
		// still-present primary key: exhausts with ErrDuplicateID rather
		// than reviving the row. See MutateOrInsert's doc comment
		// ("SQL-NULL-object row") — this is intentional, not a bug.
		if !errors.Is(err, convDB.ErrDuplicateID) {
			t.Fatalf("expected ErrDuplicateID for a SQL-NULL-object row, got %v", err)
		}
		var zero ComplexObject
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
		if seedInvocations != 5 {
			t.Fatalf("expected seed invoked mutateMaxAttempts(5) times, got %d", seedInvocations)
		}
		if fnInvocations != 5 {
			t.Fatalf("expected fn invoked mutateMaxAttempts(5) times, got %d", fnInvocations)
		}

		var objColumn sql.NullString
		err = db.QueryRow(`SELECT "object" FROM "`+table+`" WHERE "id"=$1`, id).Scan(&objColumn)
		if err != nil {
			t.Fatalf("verify object column: %v", err)
		}
		if objColumn.Valid {
			t.Fatalf("expected object column to remain SQL NULL, got %q", objColumn.String)
		}
	})

	t.Run("duplicate_key_race_converges", func(t *testing.T) {
		convDB.StubMutateBackoffForTest(t)
		id := ComplexID(uuid.NewString())

		seedInvocations := 0
		fnInvocations := 0
		obj, err := complexDB.Tenant("test").MutateOrInsert(ctx, id,
			func() (ComplexObject, error) {
				seedInvocations++
				return ComplexObject{
					ComplexID: id,
					Title:     "seeded",
					Nested:    ComplexNested{Label: "seeded-nested", Count: 1},
					Tags:      []string{"seed"},
					Attrs:     map[string]string{"k1": "seed"},
				}, nil
			},
			func(cur ComplexObject) (ComplexObject, error) {
				fnInvocations++
				if fnInvocations == 1 {
					// Test machinery, same idiom as
					// Test_Mutate's retries_once_on_cas_conflict: an
					// out-of-band Insert racing our own about-to-happen
					// Insert of the same id, landing on the same in-memory
					// sqlite DB (database.go's SetMaxOpenConns(1)).
					racer := cur
					racer.Title = "racer-inserted"
					if err := complexDB.Tenant("test").Insert(ctx, racer); err != nil {
						t.Fatalf("racer Insert failed: %v", err)
					}
					cur.Title = "fn-attempt-1"
					return cur, nil
				}
				// Attempt 2 runs the update branch against the racer's row
				// (cur here IS the racer's persisted row) and must differ
				// from it — otherwise the no-op skip would silently turn
				// the expected SafeUpdate into a skip and this test would
				// exercise less than it claims.
				cur.Title = "converged"
				return cur, nil
			},
		)
		if err != nil {
			t.Fatalf("MutateOrInsert failed: %v", err)
		}
		if seedInvocations != 1 {
			t.Fatalf("expected seed invoked exactly once, got %d", seedInvocations)
		}
		if fnInvocations != 2 {
			t.Fatalf("expected fn invoked exactly twice, got %d", fnInvocations)
		}

		got, err := complexDB.Tenant("test").SelectByID(ctx, id)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Title != "converged" {
			t.Fatalf("expected converged, got %+v", got)
		}
		if !reflect.DeepEqual(obj, *got) {
			t.Fatalf("returned object does not match persisted object:\nreturned=%+v\npersisted=%+v", obj, *got)
		}
	})

	t.Run("delete_race_converges_via_insert", func(t *testing.T) {
		convDB.StubMutateBackoffForTest(t)
		from := newComplexFixture(t, ctx, "moi-delete-race")
		id := from.ComplexID

		seedInvocations := 0
		fnInvocations := 0
		obj, err := complexDB.Tenant("test").MutateOrInsert(ctx, id,
			func() (ComplexObject, error) {
				seedInvocations++
				return ComplexObject{
					ComplexID: id,
					Title:     "reseeded",
					Nested:    ComplexNested{Label: "reseeded-nested", Count: 1},
					Tags:      []string{"reseed"},
					Attrs:     map[string]string{"k1": "reseed"},
				}, nil
			},
			func(cur ComplexObject) (ComplexObject, error) {
				fnInvocations++
				if fnInvocations == 1 {
					// Test machinery (mirror of duplicate_key_race_converges,
					// inverted): a racer deletes the row between our read and
					// SafeUpdate's guarded read. The resulting
					// ErrObjectNotFound is absorbed like a CAS conflict —
					// linearizable as "their delete, then our insert" — and
					// the next attempt converges through the insert branch.
					if err := complexDB.Tenant("test").Delete(ctx, id); err != nil {
						t.Fatalf("racer Delete failed: %v", err)
					}
				}
				// Differ from the read baseline on attempt 1 so the no-op
				// skip cannot bypass SafeUpdate.
				cur.Title = cur.Title + "-converged"
				return cur, nil
			},
		)
		if err != nil {
			t.Fatalf("MutateOrInsert failed: %v", err)
		}
		if seedInvocations != 1 {
			t.Fatalf("expected seed invoked exactly once, got %d", seedInvocations)
		}
		if fnInvocations != 2 {
			t.Fatalf("expected fn invoked exactly twice, got %d", fnInvocations)
		}

		got, err := complexDB.Tenant("test").SelectByID(ctx, id)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Title != "reseeded-converged" {
			t.Fatalf("expected the reseeded row from the insert branch, got %+v", got)
		}
		if !reflect.DeepEqual(obj, *got) {
			t.Fatalf("returned object does not match persisted object:\nreturned=%+v\npersisted=%+v", obj, *got)
		}
	})

	t.Run("wrong_shard_hint_fails_safely_with_duplicate_id", func(t *testing.T) {
		convDB.StubMutateBackoffForTest(t)
		id := MessageID(uuid.NewString())
		wrongHint := distinctShardMessageID(t, id)

		orig := Message{MessageID: id, Content: "wrong-hint-orig"}
		if err := messagesDB.Tenant("test").Insert(ctx, orig); err != nil {
			t.Fatalf("Insert failed: %v", err)
		}
		t.Cleanup(func() {
			if err := messagesDB.Tenant("test").Delete(ctx, id); err != nil {
				t.Errorf("cleanup Delete failed: %v", err)
			}
		})

		seedInvocations := 0
		fnInvocations := 0
		obj, err := messagesDB.Tenant("test").MutateOrInsert(ctx, id,
			func() (Message, error) {
				seedInvocations++
				return Message{MessageID: id, Content: "wrong-hint-seeded"}, nil
			},
			func(cur Message) (Message, error) {
				fnInvocations++
				cur.Content = "wrong-hint-attempted"
				return cur, nil
			},
			wrongHint,
		)
		if !errors.Is(err, convDB.ErrDuplicateID) {
			t.Fatalf("expected ErrDuplicateID after exhaustion, got %v", err)
		}
		var zero Message
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
		if seedInvocations != 5 {
			t.Fatalf("expected seed invoked mutateMaxAttempts(5) times, got %d", seedInvocations)
		}
		if fnInvocations != 5 {
			t.Fatalf("expected fn invoked mutateMaxAttempts(5) times, got %d", fnInvocations)
		}

		// No duplicate/corrupt row: the original content is untouched.
		got, err := messagesDB.Tenant("test").SelectByID(ctx, id)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Content != "wrong-hint-orig" {
			t.Fatalf("expected the original row untouched, got %+v", got)
		}
	})

	t.Run("insert_with_wrong_hint_still_returns_object", func(t *testing.T) {
		id := MessageID(uuid.NewString())
		wrongHint := distinctShardMessageID(t, id)

		obj, err := messagesDB.Tenant("test").MutateOrInsert(ctx, id,
			func() (Message, error) {
				return Message{MessageID: id, Content: "wrong-hint-insert-seeded"}, nil
			},
			func(cur Message) (Message, error) {
				cur.Content = "wrong-hint-insert-final"
				return cur, nil
			},
			wrongHint,
		)
		if err != nil {
			t.Fatalf("MutateOrInsert failed: %v", err)
		}
		t.Cleanup(func() {
			if err := messagesDB.Tenant("test").Delete(ctx, id); err != nil {
				t.Errorf("cleanup Delete failed: %v", err)
			}
		})

		// Red-first proof of shard-key routing: if the re-read were routed
		// by the caller's wrong hint instead of the written object's own
		// ShardKey, MutateOrInsert would instead have returned a false
		// ErrObjectVanished above.
		got, err := messagesDB.Tenant("test").SelectByID(ctx, id)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Content != "wrong-hint-insert-final" {
			t.Fatalf("expected wrong-hint-insert-final, got %+v", got)
		}
		if !reflect.DeepEqual(obj, *got) {
			t.Fatalf("returned object does not match persisted object:\nreturned=%+v\npersisted=%+v", obj, *got)
		}
	})

	t.Run("exhaustion_wraps_last_error", func(t *testing.T) {
		convDB.StubMutateBackoffForTest(t)
		from := newComplexFixture(t, ctx, "moi-exhaust")

		seedInvocations := 0
		fnInvocations := 0
		obj, err := complexDB.Tenant("test").MutateOrInsert(ctx, from.ComplexID,
			func() (ComplexObject, error) {
				seedInvocations++
				t.Fatalf("seed should not be invoked for an existing row")
				return ComplexObject{}, nil
			},
			func(cur ComplexObject) (ComplexObject, error) {
				fnInvocations++
				// Test machinery (see Test_Mutate's
				// exhausted_attempts_returns_cas_conflict): this is the CAS
				// path, not the insert path — duplicate-key exhaustion is
				// not deterministically constructible single-threaded (see
				// MutateOrInsert's doc comment).
				racer := cur
				racer.Title = fmt.Sprintf("moi-raced-%d", fnInvocations)
				if err := complexDB.Tenant("test").Update(ctx, racer); err != nil {
					t.Fatalf("racer Update failed: %v", err)
				}
				cur.Title = "moi-attempted"
				return cur, nil
			},
		)
		if !errors.Is(err, convDB.ErrCASConflict) {
			t.Fatalf("expected ErrCASConflict after exhaustion, got %v", err)
		}
		var zero ComplexObject
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
		if seedInvocations != 0 {
			t.Fatalf("expected seed invoked 0 times, got %d", seedInvocations)
		}
		if fnInvocations != 5 {
			t.Fatalf("expected fn invoked mutateMaxAttempts(5) times, got %d", fnInvocations)
		}
	})

	t.Run("cancellation_during_backoff", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "moi-cancel-backoff")

		cancellable, cancel := context.WithCancel(context.Background())
		defer cancel()
		cctx := convCtx.WrapContext(cancellable, convAuth.Claims{User: "Test_MutateOrInsert_cancel_backoff"})

		seedInvocations := 0
		fnInvocations := 0
		obj, err := complexDB.Tenant("test").MutateOrInsert(cctx, from.ComplexID,
			func() (ComplexObject, error) {
				seedInvocations++
				t.Fatalf("seed should not be invoked for an existing row")
				return ComplexObject{}, nil
			},
			func(cur ComplexObject) (ComplexObject, error) {
				fnInvocations++
				// Test machinery (see Test_Mutate's
				// context_cancellation_during_backoff): force a CAS conflict
				// AND cancel the context inside the callback, so
				// cancellation deterministically precedes the backoff wait.
				racer := cur
				racer.Title = "moi-raced-cancel"
				if err := complexDB.Tenant("test").Update(cctx, racer); err != nil {
					t.Fatalf("racer Update failed: %v", err)
				}
				cancel()
				cur.Title = "moi-attempted-cancel"
				return cur, nil
			},
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		if !errors.Is(err, convDB.ErrCASConflict) {
			t.Fatalf("expected ErrCASConflict, got %v", err)
		}
		var zero ComplexObject
		if !reflect.DeepEqual(obj, zero) {
			t.Fatalf("expected zero-value object on error, got %+v", obj)
		}
		if seedInvocations != 0 {
			t.Fatalf("expected seed invoked 0 times, got %d", seedInvocations)
		}
		if fnInvocations != 1 {
			t.Fatalf("expected fn invoked exactly once, got %d", fnInvocations)
		}
	})

	t.Run("shard_key_passthrough", func(t *testing.T) {
		id := MessageID(uuid.NewString())

		obj, err := messagesDB.Tenant("test").MutateOrInsert(ctx, id,
			func() (Message, error) {
				return Message{MessageID: id, Content: "shard-passthrough-seeded"}, nil
			},
			func(cur Message) (Message, error) {
				cur.Content = "shard-passthrough-final"
				return cur, nil
			},
			id, // explicit shard key hint, matching the object's own ShardKey
		)
		if err != nil {
			t.Fatalf("MutateOrInsert failed: %v", err)
		}

		got, err := messagesDB.Tenant("test").SelectByID(ctx, id)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Content != "shard-passthrough-final" {
			t.Fatalf("expected shard-passthrough-final, got %+v", got)
		}
		if !reflect.DeepEqual(obj, *got) {
			t.Fatalf("returned object does not match persisted object:\nreturned=%+v\npersisted=%+v", obj, *got)
		}

		// messagesDB is shared with process_test.go's exact-count assertions;
		// clean up so this fixture does not leak into them (mirrors
		// Test_Mutate's shard_key_passthrough end-of-test Delete).
		if err := messagesDB.Tenant("test").Delete(ctx, id); err != nil {
			t.Fatalf("cleanup Delete failed: %v", err)
		}
	})
}
