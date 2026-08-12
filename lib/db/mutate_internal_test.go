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

func Test_isDuplicateInsertErr(t *testing.T) {

	t.Run("23505_is_duplicate_on_postgres", func(t *testing.T) {
		if !isDuplicateInsertErr(fakeSQLStateErr{state: "23505"}, EnginePostgres) {
			t.Fatalf("expected SQLSTATE 23505 to be classified as a duplicate on postgres")
		}
	})

	t.Run("23505_is_duplicate_on_sqlite", func(t *testing.T) {
		if !isDuplicateInsertErr(fakeSQLStateErr{state: "23505"}, EngineSqlite3) {
			t.Fatalf("expected SQLSTATE 23505 to be classified as a duplicate on sqlite3")
		}
	})

	t.Run("sqlite_unique_constraint_message_is_duplicate_on_sqlite", func(t *testing.T) {
		if !isDuplicateInsertErr(errors.New("UNIQUE constraint failed: complex.id"), EngineSqlite3) {
			t.Fatalf("expected the SQLite UNIQUE constraint message to be classified as a duplicate on sqlite3")
		}
	})

	// Pins the engine gate: the substring heuristic exists only because
	// SQLite's driver doesn't expose SQLState() through an interface. It
	// must never fire for Postgres, whose errors are always classified via
	// SQLState() instead.
	t.Run("sqlite_unique_constraint_message_is_not_duplicate_on_postgres", func(t *testing.T) {
		if isDuplicateInsertErr(errors.New("UNIQUE constraint failed: complex.id"), EnginePostgres) {
			t.Fatalf("expected the SQLite UNIQUE constraint message substring match to be gated off on postgres")
		}
	})

	t.Run("non_matching_error_is_not_duplicate", func(t *testing.T) {
		if isDuplicateInsertErr(errors.New("plain"), EngineSqlite3) {
			t.Fatalf("expected a plain error to not be classified as a duplicate")
		}
	})

	t.Run("nil_is_not_duplicate", func(t *testing.T) {
		if isDuplicateInsertErr(nil, EngineSqlite3) {
			t.Fatalf("expected nil to not be classified as a duplicate")
		}
	})
}
