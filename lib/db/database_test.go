package db_test

import (
	"testing"

	"github.com/google/uuid"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
)

func Test_open_and_close(t *testing.T) {

	err := convDB.Open()
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	dbs, err := convDB.DBs("messages", "test")
	if err != nil {
		t.Fatalf("DBs failed: %v", err)
	}

	if dbs == nil {
		t.Fatalf("DBs failed: nil")
	}

	if len(dbs) != 2 {
		t.Fatalf("DBs failed: %v", len(dbs))
	}

	err = convDB.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	err = convDB.Open()
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

}

// Test_object_set_usable_after_close_and_reopen pins the invariant that makes
// the suite runnable more than once in a process: after Close() drops the
// connections and Open() rebuilds them, an ObjectSet that was already
// prepared against the *old* connections must still work.
//
// It did not. Close() reset dbs/dbsOnce so Open() could run again but left
// the global typeToTable registry populated, so the next prepare() found its
// hit and returned before issuing its CREATE TABLE IF NOT EXISTS. A reopened
// in-memory SQLite database is empty, so every later query hit "no such
// table". That is the whole of the `go test ./lib/db/ -count=2` failure:
// pass two runs against databases whose tables were never recreated.
//
// That registry is the only preparation state that can go stale here.
// objectSet's `prepared` field looks like a second one, but nothing in the
// package ever assigns it, and Tenant() takes a value receiver — prepare()
// only ever mutates a per-call copy, so an assignment would not persist
// anyway. Wiring that field up (or giving Tenant a pointer receiver) would
// reintroduce prepared state Close() has no handle on, and this test would
// go red again.
func Test_object_set_usable_after_close_and_reopen(t *testing.T) {

	ctx := convCtx.New(convAuth.Claims{User: convAuth.User(t.Name())})

	msg := Message{MessageID: MessageID(uuid.NewString()), Content: "before-close"}
	if err := messagesDB.Tenant("test").Insert(ctx, msg); err != nil {
		t.Fatalf("Insert before close failed: %v", err)
	}

	if err := convDB.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if err := convDB.Open(); err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// The same ObjectSet value, already prepared against the connections
	// Close() just dropped. Its tables must exist on the new ones.
	after := Message{MessageID: MessageID(uuid.NewString()), Content: "after-reopen"}
	if err := messagesDB.Tenant("test").Insert(ctx, after); err != nil {
		t.Fatalf("Insert after close+reopen failed: %v", err)
	}

	// Registered here rather than written as the last statement of the test:
	// t.Fatalf runs cleanups but never the rest of the body, so every
	// assertion below — the ones this test exists to make — would otherwise
	// leak this row on failure. Several tests in this package assert an
	// absolute row count over the whole "messages" vault, and they run later
	// in this same pass, so the leak turns one real failure into four
	// (101/201/301) with three of them pointing at innocent files. (The
	// pre-close row needs no delete: its in-memory database was destroyed
	// with the connection.)
	t.Cleanup(func() {
		if err := messagesDB.Tenant("test").Delete(ctx, after.MessageID); err != nil {
			t.Errorf("cleanup of the post-reopen fixture failed: %v", err)
		}
	})

	got, err := messagesDB.Tenant("test").SelectByID(ctx, after.MessageID)
	if err != nil {
		t.Fatalf("SelectByID after close+reopen failed: %v", err)
	}
	if got == nil || got.Content != "after-reopen" {
		t.Fatalf("expected the row written after reopen, got %+v", got)
	}
}
