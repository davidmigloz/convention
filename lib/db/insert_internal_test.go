package db

import (
	"errors"
	"testing"
)

// fakeSQLStateErr is shared in-package from update_internal_test.go.

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
