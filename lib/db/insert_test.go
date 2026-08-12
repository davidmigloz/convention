package db_test

import (
	"errors"
	"strings"
	"testing"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
)

func Test_Insert(t *testing.T) {

	ctx := convCtx.New(
		convAuth.Claims{
			User: "Test_Insert",
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

	for _, msg := range msgs {
		err := messagesDB.Tenant("test").Delete(ctx, msg.MessageID)
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	}
}

// Test_Insert_DuplicateID inserts a row, then inserts the same ID again and
// asserts the failure is classified as ErrDuplicateID (errors.Is-reachable)
// while the underlying driver message is still reachable via Error() — see
// PR B commit 1's plan: today (before the production change) this fails red
// (errors.Is is false, and the caller sees only the raw driver error).
func Test_Insert_DuplicateID(t *testing.T) {

	ctx := convCtx.New(
		convAuth.Claims{
			User: "Test_Insert_DuplicateID",
		},
	)

	obj := ComplexObject{
		ComplexID: ComplexID("insert-duplicate-id-fixture"),
		Title:     "first",
	}

	err := complexDB.Tenant("test").Insert(ctx, obj)
	if err != nil {
		t.Fatalf("first Insert failed: %v", err)
	}
	t.Cleanup(func() {
		_ = complexDB.Tenant("test").Delete(ctx, obj.ComplexID)
	})

	dup := ComplexObject{
		ComplexID: obj.ComplexID,
		Title:     "second",
	}
	err = complexDB.Tenant("test").Insert(ctx, dup)
	if err == nil {
		t.Fatalf("expected second Insert of the same id to fail")
	}
	if !errors.Is(err, convDB.ErrDuplicateID) {
		t.Fatalf("expected errors.Is(err, ErrDuplicateID), got %v", err)
	}
	// The underlying driver error must remain reachable, not swallowed by
	// the sentinel wrap.
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("expected the underlying driver message to remain reachable in %q", err.Error())
	}
}

func Test_Upsert(t *testing.T) {

	ctx := convCtx.New(
		convAuth.Claims{
			User: "Test_Upsert",
		},
	)

	msgs := generateTestMessages()

	for _, msg := range msgs {
		err := messagesDB.Tenant("test").Upsert(ctx, msg)
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

	for _, msg := range msgs {
		err := messagesDB.Tenant("test").Delete(ctx, msg.MessageID)
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	}
}
