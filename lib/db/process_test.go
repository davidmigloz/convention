package db_test

import (
	"testing"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
)

func Test_Process(t *testing.T) {

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

	count, err := messagesDB.Tenant("test").Process(
		ctx,
		convDB.Where().Noop(),
		func(ctx convCtx.Context, obj Message) error {
			// do nothing
			return nil
		},
	)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	if count != len(msgs) {
		t.Fatalf("Unexpected count: %v", count)
	}

	for _, msg := range msgs {
		err := messagesDB.Tenant("test").Delete(ctx, msg.MessageID)
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	}

}

func Test_ProcessWithMetadata(t *testing.T) {

	ctx := convCtx.New(
		convAuth.Claims{
			User: "Test_ProcessWithMetadata",
		},
	)

	msgs := generateTestMessages()

	for _, msg := range msgs {
		err := messagesDB.Tenant("test").Insert(ctx, msg)
		if err != nil {
			t.Fatalf("Insert failed: %v", err)
		}
	}

	count, err := messagesDB.Tenant("test").ProcessWithMetadata(
		ctx,
		convDB.Where().Noop(),
		func(ctx convCtx.Context, obj convDB.ObjectWithMetadata[Message]) error {
			// do nothing
			return nil
		},
	)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	if count != len(msgs) {
		t.Fatalf("Unexpected count: %v", count)
	}

	for _, msg := range msgs {
		err := messagesDB.Tenant("test").Delete(ctx, msg.MessageID)
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	}

}

func TestProcess_ignoresNullRuntimeObjects(t *testing.T) {
	ctx := convCtx.New(convAuth.Claims{User: convAuth.User(t.Name())})
	clearMessagesDB(ctx)
	if _, err := messagesDB.Tenant("test").Select(ctx, convDB.Where().Noop()); err != nil {
		t.Fatalf("prepare message ObjectSet: %v", err)
	}
	insertNullMessageRows(t)

	t.Run("without metadata", func(t *testing.T) {
		callbackCount := 0
		count, err := messagesDB.Tenant("test").Process(
			ctx,
			convDB.Where().Noop(),
			func(_ convCtx.Context, _ Message) error {
				callbackCount++
				return nil
			},
		)
		if err != nil {
			t.Fatalf("Process failed for SQL NULL runtime object: %v", err)
		}
		if count != 0 {
			t.Fatalf("Process counted %d SQL NULL runtime objects", count)
		}
		if callbackCount != 0 {
			t.Fatalf("Process invoked callback %d times for SQL NULL runtime objects", callbackCount)
		}
	})

	t.Run("with metadata", func(t *testing.T) {
		callbackCount := 0
		count, err := messagesDB.Tenant("test").ProcessWithMetadata(
			ctx,
			convDB.Where().Noop(),
			func(_ convCtx.Context, _ convDB.ObjectWithMetadata[Message]) error {
				callbackCount++
				return nil
			},
		)
		if err != nil {
			t.Fatalf("ProcessWithMetadata failed for SQL NULL runtime object: %v", err)
		}
		if count != 0 {
			t.Fatalf("ProcessWithMetadata counted %d SQL NULL runtime objects", count)
		}
		if callbackCount != 0 {
			t.Fatalf(
				"ProcessWithMetadata invoked callback %d times for SQL NULL runtime objects",
				callbackCount,
			)
		}
	})
}
