package db

import (
	"errors"
	"fmt"
	"testing"
)

func Test_mutateRetryable(t *testing.T) {

	t.Run("cas_conflict_retryable", func(t *testing.T) {
		if !mutateRetryable(ErrCASConflict) {
			t.Fatalf("expected ErrCASConflict to be retryable")
		}
	})

	t.Run("wrapped_cas_conflict_retryable", func(t *testing.T) {
		wrapped := fmt.Errorf("wrapped: %w", ErrCASConflict)
		if !mutateRetryable(wrapped) {
			t.Fatalf("expected wrapped ErrCASConflict to be retryable")
		}
	})

	// SQLite (the test-only engine) cannot raise SQLSTATE 55P03 (mirroring
	// update_test.go's lock_not_available_integration skip note), so this
	// classification is exercised only here, in-harness.
	t.Run("lock_not_available_retryable", func(t *testing.T) {
		if !mutateRetryable(ErrLockNotAvailable) {
			t.Fatalf("expected ErrLockNotAvailable to be retryable")
		}
	})

	t.Run("wrapped_lock_not_available_retryable", func(t *testing.T) {
		wrapped := fmt.Errorf("wrapped: %w", ErrLockNotAvailable)
		if !mutateRetryable(wrapped) {
			t.Fatalf("expected wrapped ErrLockNotAvailable to be retryable")
		}
	})

	t.Run("object_not_found_not_retryable", func(t *testing.T) {
		if mutateRetryable(ErrObjectNotFound) {
			t.Fatalf("expected ErrObjectNotFound to not be retryable")
		}
	})

	t.Run("plain_error_not_retryable", func(t *testing.T) {
		if mutateRetryable(errors.New("plain")) {
			t.Fatalf("expected plain error to not be retryable")
		}
	})

	t.Run("nil_not_retryable", func(t *testing.T) {
		if mutateRetryable(nil) {
			t.Fatalf("expected nil to not be retryable")
		}
	})
}
