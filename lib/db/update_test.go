package db_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
)

func Test_Update(t *testing.T) {

	ctx := convCtx.New(
		convAuth.Claims{
			User: "Test_Update",
		},
	)

	msgs := generateTestMessages()

	for _, msg := range msgs {
		err := messagesDB.Tenant("test").Insert(ctx, msg)
		if err != nil {
			t.Fatalf("Insert failed: %v", err)
		}
	}

	msg, err := messagesDB.Tenant("test").SelectByID(ctx, msgs[0].MessageID)
	if err != nil {
		t.Fatalf("SelectByID failed: %v", err)
	}

	if msg == nil {
		t.Fatalf("SelectByID failed: nil")
	}

	if msg.Content == "Updated content" {
		t.Fatalf("Unexpected content: %v", msg.Content)
	}

	msg.Content = "Updated content"
	err = messagesDB.Tenant("test").Update(ctx, *msg)
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	msg, err = messagesDB.Tenant("test").SelectByID(ctx, msgs[0].MessageID)
	if err != nil {
		t.Fatalf("SelectByID failed: %v", err)
	}

	if msg == nil {
		t.Fatalf("SelectByID failed: nil")
	}

	if msg.Content != "Updated content" {
		t.Fatalf("Unexpected content: %v", msg.Content)
	}

	for _, msg := range msgs {
		err := messagesDB.Tenant("test").Delete(ctx, msg.MessageID)
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	}
}

// newComplexFixture inserts a fresh ComplexObject with random ID + payload and
// returns the loaded copy so each sub-test starts from a clean slate.
func newComplexFixture(t *testing.T, ctx convCtx.Context, suffix string) ComplexObject {
	t.Helper()
	obj := ComplexObject{
		ComplexID: ComplexID(uuid.NewString()),
		Title:     "title-" + suffix,
		Nested:    ComplexNested{Label: "nested-" + suffix, Count: 7},
		Tags:      []string{"a", "b", "c"},
		Attrs:     map[string]string{"k1": "v1", "k2": "v2"},
	}
	if err := complexDB.Tenant("test").Insert(ctx, obj); err != nil {
		t.Fatalf("Insert failed: %v", err)
	}
	got, err := complexDB.Tenant("test").SelectByID(ctx, obj.ComplexID)
	if err != nil {
		t.Fatalf("SelectByID failed: %v", err)
	}
	if got == nil {
		t.Fatalf("SelectByID returned nil")
	}
	return *got
}

func historyRowCount(t *testing.T, vault convDB.Vault, runtimeTable string, id string) int {
	t.Helper()
	dbs, err := convDB.DBs(vault, "test")
	if err != nil {
		t.Fatalf("DBs failed: %v", err)
	}
	total := 0
	for _, db := range dbs {
		var n int
		err := db.QueryRow(`SELECT COUNT(*) FROM "`+runtimeTable+`_history" WHERE "id"=?`, id).Scan(&n)
		if err != nil && err != sql.ErrNoRows {
			t.Fatalf("history count failed: %v", err)
		}
		total += n
	}
	return total
}

func Test_SafeUpdate(t *testing.T) {

	ctx := convCtx.New(convAuth.Claims{User: "Test_SafeUpdate"})

	t.Run("happy_path", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "happy")
		to := from
		to.Title = "updated-happy"
		to.Tags = []string{"x", "y"}
		to.Attrs = map[string]string{"only": "value"}

		if err := complexDB.Tenant("test").SafeUpdate(ctx, from, to); err != nil {
			t.Fatalf("SafeUpdate failed: %v", err)
		}

		got, err := complexDB.Tenant("test").SelectByID(ctx, from.ComplexID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Title != "updated-happy" {
			t.Fatalf("expected updated-happy, got %+v", got)
		}
		if len(got.Tags) != 2 || got.Tags[0] != "x" {
			t.Fatalf("tags not persisted: %v", got.Tags)
		}
		if got.Attrs["only"] != "value" {
			t.Fatalf("attrs not persisted: %v", got.Attrs)
		}
	})

	t.Run("regression_md5_jsonb", func(t *testing.T) {
		// Pre-fix this row would have failed in two distinct ways: md5("object")
		// against JSONB (Postgres) / no such function: md5 (SQLite), AND a
		// Scan-into-objT mismatch in database/sql. The nested+slice+map payload
		// here ensures we genuinely exercise the JSONB-shape codepath.
		from := newComplexFixture(t, ctx, "md5")
		to := from
		to.Nested.Count = 99
		to.Tags = append(append([]string{}, from.Tags...), "extra")
		to.Attrs = map[string]string{"k1": "v1", "k2": "v2", "k3": "v3"}

		if err := complexDB.Tenant("test").SafeUpdate(ctx, from, to); err != nil {
			t.Fatalf("SafeUpdate failed: %v", err)
		}
		got, err := complexDB.Tenant("test").SelectByID(ctx, from.ComplexID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Nested.Count != 99 || got.Attrs["k3"] != "v3" {
			t.Fatalf("nested/map update not persisted: %+v", got)
		}
	})

	t.Run("compute_hooks_normalise_cmp", func(t *testing.T) {
		// Simulate the Postgres µs/ns timestamp divergence (stored JSONB keeps
		// nanoseconds while the timestamp column truncates to microseconds) by
		// manually rewriting the runtime row's "object" JSON after Insert so
		// the embedded created_at / updated_at diverge from the metadata
		// columns. SelectByID's compute hook recovers the column value onto
		// `from`; SafeUpdate must do the same to `cmp`, otherwise the
		// marshal-compare false-conflicts on every call.
		msg := Message{MessageID: MessageID(uuid.NewString()), Content: "normalise"}
		if err := messagesDB.Tenant("test").Insert(ctx, msg); err != nil {
			t.Fatalf("Insert failed: %v", err)
		}

		bogus := `{"message_id":"` + string(msg.MessageID) +
			`","content":"normalise","created_at":"1900-01-01T00:00:00Z",` +
			`"updated_at":"1900-01-01T00:00:00Z"}`
		dbs, err := convDB.DBs("messages", "test")
		if err != nil {
			t.Fatalf("DBs failed: %v", err)
		}
		var affected int64
		for _, db := range dbs {
			res, execErr := db.Exec(`UPDATE "message" SET "object"=? WHERE "id"=?`, bogus, string(msg.MessageID))
			if execErr != nil {
				t.Fatalf("raw UPDATE failed: %v", execErr)
			}
			n, _ := res.RowsAffected()
			affected += n
		}
		if affected != 1 {
			t.Fatalf("expected exactly one shard to hold the row, got %d", affected)
		}

		loaded, err := messagesDB.Tenant("test").SelectByID(ctx, msg.MessageID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if loaded.CreatedAt.Year() == 1900 {
			t.Fatalf("compute hook did not recover CreatedAt from metadata column; got %v", loaded.CreatedAt)
		}

		from := *loaded
		to := from
		to.Content = "post-normalise"
		if err := messagesDB.Tenant("test").SafeUpdate(ctx, from, to); err != nil {
			t.Fatalf("SafeUpdate should succeed via cmp normalisation; got %v", err)
		}

		after, err := messagesDB.Tenant("test").SelectByID(ctx, msg.MessageID)
		if err != nil {
			t.Fatalf("SelectByID after SafeUpdate failed: %v", err)
		}
		if after.Content != "post-normalise" {
			t.Fatalf("update did not persist, got %+v", after)
		}
	})

	t.Run("compute_hooks_run", func(t *testing.T) {
		// messagesDB has a compute hook that copies metadata onto the object;
		// running SafeUpdate against it lets us assert the hook still fires
		// post-refactor.
		msg := Message{MessageID: MessageID(uuid.NewString()), Content: "before"}
		if err := messagesDB.Tenant("test").Insert(ctx, msg); err != nil {
			t.Fatalf("Insert failed: %v", err)
		}
		loaded, err := messagesDB.Tenant("test").SelectByID(ctx, msg.MessageID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		from := *loaded
		to := from
		to.Content = "after"

		if err := messagesDB.Tenant("test").SafeUpdate(ctx, from, to); err != nil {
			t.Fatalf("SafeUpdate failed: %v", err)
		}
		after, err := messagesDB.Tenant("test").SelectByID(ctx, msg.MessageID)
		if err != nil {
			t.Fatalf("SelectByID after SafeUpdate failed: %v", err)
		}
		if after.UpdatedAt.Equal(from.UpdatedAt) || after.UpdatedAt.Before(from.UpdatedAt) {
			t.Fatalf("UpdatedAt did not advance: before=%v after=%v", from.UpdatedAt, after.UpdatedAt)
		}
		if !after.CreatedAt.Equal(from.CreatedAt) {
			t.Fatalf("CreatedAt should be stable: before=%v after=%v", from.CreatedAt, after.CreatedAt)
		}
	})

	t.Run("from_metadata_divergence_no_false_conflict", func(t *testing.T) {
		// A caller that loaded `from` via a path that did NOT run compute
		// hooks (e.g. Process, which selects only "object") hands SafeUpdate
		// a `from` whose embedded metadata diverges from the row's
		// compute-normalized metadata. Business state matches the row, so
		// SafeUpdate must succeed — the comparator normalizes `from` through
		// the same compute pipeline as the current row and compares business
		// state only.
		msg := Message{MessageID: MessageID(uuid.NewString()), Content: "orig"}
		if err := messagesDB.Tenant("test").Insert(ctx, msg); err != nil {
			t.Fatalf("Insert failed: %v", err)
		}
		loaded, err := messagesDB.Tenant("test").SelectByID(ctx, msg.MessageID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}

		// Correct business state, deliberately wrong embedded metadata.
		bogus := time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
		from := *loaded
		from.CreatedAt = bogus
		from.UpdatedAt = bogus

		to := from
		to.Content = "updated"

		if err := messagesDB.Tenant("test").SafeUpdate(ctx, from, to); err != nil {
			t.Fatalf("SafeUpdate should succeed despite divergent from metadata; got %v", err)
		}
		after, err := messagesDB.Tenant("test").SelectByID(ctx, msg.MessageID)
		if err != nil {
			t.Fatalf("SelectByID after SafeUpdate failed: %v", err)
		}
		if after.Content != "updated" {
			t.Fatalf("update did not persist: %+v", after)
		}
		if after.CreatedAt.Year() == 1900 {
			t.Fatalf("compute hook should have re-stamped CreatedAt, got %v", after.CreatedAt)
		}
	})

	t.Run("from_metadata_divergence_preserves_business_conflict", func(t *testing.T) {
		// Normalizing `from`'s metadata must NOT mask a genuine business-state
		// conflict: a `from` with both divergent metadata AND stale business
		// state still trips ErrCASConflict.
		msg := Message{MessageID: MessageID(uuid.NewString()), Content: "orig"}
		if err := messagesDB.Tenant("test").Insert(ctx, msg); err != nil {
			t.Fatalf("Insert failed: %v", err)
		}
		loaded, err := messagesDB.Tenant("test").SelectByID(ctx, msg.MessageID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}

		bogus := time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
		from := *loaded
		from.CreatedAt = bogus
		from.UpdatedAt = bogus
		from.Content = "stale-different" // business state no longer matches the row

		to := from
		to.Content = "attempted-update"

		err = messagesDB.Tenant("test").SafeUpdate(ctx, from, to)
		if !errors.Is(err, convDB.ErrCASConflict) {
			t.Fatalf("expected ErrCASConflict for stale business state, got %v", err)
		}
	})

	t.Run("history_row_inserted", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "history")
		preCount := historyRowCount(t, "complex", "complex_object", string(from.ComplexID))
		to := from
		to.Title = "history-updated"

		if err := complexDB.Tenant("test").SafeUpdate(ctx, from, to); err != nil {
			t.Fatalf("SafeUpdate failed: %v", err)
		}
		postCount := historyRowCount(t, "complex", "complex_object", string(from.ComplexID))
		if postCount != preCount+1 {
			t.Fatalf("expected one new history row, got pre=%d post=%d", preCount, postCount)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		ghost := ComplexObject{
			ComplexID: ComplexID(uuid.NewString()),
			Title:     "ghost",
		}
		err := complexDB.Tenant("test").SafeUpdate(ctx, ghost, ghost)
		if !errors.Is(err, convDB.ErrObjectNotFound) {
			t.Fatalf("expected ErrObjectNotFound, got %v", err)
		}
	})

	t.Run("sql_null_object_is_not_found_before_comparing_from", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "sql-null")

		dbs, err := convDB.DBs("complex", "test")
		if err != nil {
			t.Fatalf("DBs failed: %v", err)
		}
		if len(dbs) != 1 {
			t.Fatalf("complex test database has %d shards, want 1", len(dbs))
		}
		res, err := dbs[0].Exec(
			`UPDATE "complex_object" SET "object"=NULL WHERE "id"=$1`,
			from.ComplexID,
		)
		if err != nil {
			t.Fatalf("set runtime object to SQL NULL: %v", err)
		}
		if affected, err := res.RowsAffected(); err != nil {
			t.Fatalf("read affected rows: %v", err)
		} else if affected != 1 {
			t.Fatalf("set %d runtime objects to SQL NULL, want 1", affected)
		}

		marshalCount := 0
		from.marshalCount = &marshalCount
		from.failOn = 1
		to := from
		to.Title = "must-not-be-written"

		err = complexDB.Tenant("test").SafeUpdate(ctx, from, to)

		if !errors.Is(err, convDB.ErrObjectNotFound) {
			t.Fatalf("expected ErrObjectNotFound for SQL-NULL runtime object, got %v", err)
		}
		if marshalCount != 0 {
			t.Fatalf(
				"SafeUpdate marshalled the caller comparison side %d times after finding an absent row",
				marshalCount,
			)
		}
	})

	t.Run("cas_conflict_via_update", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "cas-update")
		racer := from
		racer.Title = "raced-by-update"
		if err := complexDB.Tenant("test").Update(ctx, racer); err != nil {
			t.Fatalf("racer Update failed: %v", err)
		}
		to := from
		to.Title = "loser"

		err := complexDB.Tenant("test").SafeUpdate(ctx, from, to)
		if !errors.Is(err, convDB.ErrCASConflict) {
			t.Fatalf("expected ErrCASConflict, got %v", err)
		}
		got, err := complexDB.Tenant("test").SelectByID(ctx, from.ComplexID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Title != "raced-by-update" {
			t.Fatalf("racer write should have stuck, got %+v", got)
		}
	})

	t.Run("cas_conflict_via_safeupdate", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "cas-safeupdate")
		racerFrom := from
		racerTo := from
		racerTo.Title = "raced-by-safeupdate"
		if err := complexDB.Tenant("test").SafeUpdate(ctx, racerFrom, racerTo); err != nil {
			t.Fatalf("racer SafeUpdate failed: %v", err)
		}
		to := from
		to.Title = "loser"

		err := complexDB.Tenant("test").SafeUpdate(ctx, from, to)
		if !errors.Is(err, convDB.ErrCASConflict) {
			t.Fatalf("expected ErrCASConflict, got %v", err)
		}
		got, err := complexDB.Tenant("test").SelectByID(ctx, from.ComplexID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Title != "raced-by-safeupdate" {
			t.Fatalf("racer write should have stuck, got %+v", got)
		}
	})

	t.Run("id_mismatch", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "id-mismatch")
		to := from
		to.ComplexID = ComplexID(uuid.NewString())
		err := complexDB.Tenant("test").SafeUpdate(ctx, from, to)
		if err == nil || err.Error() != "cannot safely update object with different IDs" {
			t.Fatalf("expected ID-mismatch error, got %v", err)
		}
	})

	t.Run("shard_key_mismatch", func(t *testing.T) {
		from := SplitKeyObject{
			SplitID:    SplitID(uuid.NewString()),
			SplitShard: SplitShard("shard-a"),
			Payload:    "p",
		}
		to := from
		to.SplitShard = SplitShard("shard-b")
		err := splitKeyDB.Tenant("test").SafeUpdate(ctx, from, to)
		if err == nil || err.Error() != "cannot safely update object with different shard keys" {
			t.Fatalf("expected shard-key-mismatch error, got %v", err)
		}
	})

	t.Run("stale_from_marshal_compare", func(t *testing.T) {
		// Caller-side mutation of `from` between load and call must surface as
		// ErrCASConflict — the comparator is intentionally strict.
		from := newComplexFixture(t, ctx, "stale-from")
		mutatedFrom := from
		mutatedFrom.Title = "caller-bug"

		to := from
		to.Title = "intended-update"
		err := complexDB.Tenant("test").SafeUpdate(ctx, mutatedFrom, to)
		if !errors.Is(err, convDB.ErrCASConflict) {
			t.Fatalf("expected ErrCASConflict for stale from, got %v", err)
		}
		got, err := complexDB.Tenant("test").SelectByID(ctx, from.ComplexID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Title != from.Title {
			t.Fatalf("row should be unchanged, got %+v", got)
		}
	})

	t.Run("transaction_rollback_on_marshal_failure", func(t *testing.T) {
		from := newComplexFixture(t, ctx, "rollback")
		preCount := historyRowCount(t, "complex", "complex_object", string(from.ComplexID))

		// Both sides of the comparator and the UPDATE call MarshalJSON on
		// values whose counter and threshold we control. cmp is decoded
		// inside lib/db with no counter, so it serialises without tripping
		// the hook. The hook fires on the 3rd Marshal call (cmp marshal
		// would be #1 on cmp's nil-counter copy, but only the from/to
		// counter advances; so threshold 2 fires on the to-marshal).
		counter := 0
		fromMutated := from
		fromMutated.marshalCount = &counter
		fromMutated.failOn = 2

		to := from
		to.marshalCount = &counter
		to.failOn = 2
		to.Title = "should-not-persist"

		err := complexDB.Tenant("test").SafeUpdate(ctx, fromMutated, to)
		if !errors.Is(err, errMarshalInjected) {
			t.Fatalf("expected injected marshal failure, got %v", err)
		}

		got, err := complexDB.Tenant("test").SelectByID(ctx, from.ComplexID)
		if err != nil {
			t.Fatalf("SelectByID failed: %v", err)
		}
		if got == nil || got.Title != from.Title {
			t.Fatalf("row should be unchanged after rollback, got %+v", got)
		}
		postCount := historyRowCount(t, "complex", "complex_object", string(from.ComplexID))
		if postCount != preCount {
			t.Fatalf("history row should not have been inserted: pre=%d post=%d", preCount, postCount)
		}
	})

	t.Run("lock_not_available_integration", func(t *testing.T) {
		t.Skip("requires Postgres for FOR UPDATE NOWAIT; covered downstream by a real-Postgres integration test")
	})
}
