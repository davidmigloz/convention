package db_test

import (
	"fmt"
	"testing"
	"time"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
)

func insertNullMessageRows(t *testing.T) {
	t.Helper()

	dbs, err := convDB.DBs("messages", "test")
	if err != nil {
		t.Fatalf("get message databases: %v", err)
	}
	now := time.Now().UTC()
	for i, db := range dbs {
		id := fmt.Sprintf("%s-%d", t.Name(), i)
		if _, err := db.Exec(
			`INSERT INTO "message" ("id", "created_at", "created_by", "updated_at", "updated_by", "object")
			 VALUES ($1, $2, $3, $4, $5, NULL)`,
			id,
			now,
			t.Name(),
			now,
			t.Name(),
		); err != nil {
			t.Fatalf("insert SQL NULL message row: %v", err)
		}
		t.Cleanup(func() {
			if _, err := db.Exec(`DELETE FROM "message" WHERE "id"=$1`, id); err != nil {
				t.Errorf("delete SQL NULL message row: %v", err)
			}
		})
	}
}

func clearMessagesDB(ctx convCtx.Context) {
	obs, err := messagesDB.Tenant("test").SelectAll(ctx)
	if err != nil {
		panic(err)
	}
	for _, msg := range obs {
		err := messagesDB.Tenant("test").Delete(ctx, msg.MessageID)
		if err != nil {
			panic(err)
		}
	}
}

func Test_select(t *testing.T) {

	ctx := convCtx.New(convAuth.Claims{User: "Test_select"})

	clearMessagesDB(ctx)

	testMessages := generateTestMessages()

	for _, msg := range testMessages {
		err := messagesDB.Tenant("test").Insert(ctx, msg)
		if err != nil {
			t.Fatalf("Insert failed: %v", err)
		}
	}

	msg, err := messagesDB.Tenant("test").SelectByID(ctx, testMessages[0].MessageID)
	if err != nil {
		t.Fatalf("SelectByID failed: %v", err)
	}

	if msg == nil {
		t.Fatalf("SelectByID failed: nil")
	}

	if msg.Content != testMessages[0].Content {
		t.Fatalf("Unexpected content: %v", msg.Content)
	}

	msgs, err := messagesDB.Tenant("test").SelectAll(ctx)
	if err != nil {
		t.Fatalf("SelectAll failed: %v", err)
	}

	if len(msgs) != len(testMessages) {
		t.Fatalf("Unexpected messages count: %v", len(msgs))
	}

	msgs, err = messagesDB.Tenant("test").Select(ctx,
		convDB.Where().
			Noop().
			And().
			Key("content").Equals().Value(testMessages[1].Content).
			And().
			CreatedBetween(time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour)).
			And().
			CreatedBy("Test_select").
			And().
			UpdatedBetween(time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour)).
			And().
			UpdatedBy("Test_select").
			And().
			Expression(
				convDB.Where().
					Noop().
					Or().
					UpdatedBy("unknown"),
			).
			And().
			Key("message_id").
			Like().
			Value("%"),
	)
	if err != nil {
		t.Fatalf("Select failed: %v", err)
	}

	if len(msgs) != 1 {
		t.Fatalf("Unexpected messages count: %v", len(msgs))
	}

	for _, msg := range msgs {
		err := messagesDB.Tenant("test").Delete(ctx, msg.MessageID)
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	}

}

func Test_select_with_metadata(t *testing.T) {

	ctx := convCtx.New(convAuth.Claims{User: "Test_select_with_metadata"})

	clearMessagesDB(ctx)

	testMessages := generateTestMessages()

	for _, msg := range testMessages {
		err := messagesDB.Tenant("test").Insert(ctx, msg)
		if err != nil {
			t.Fatalf("Insert failed: %v", err)
		}
	}

	msg, err := messagesDB.Tenant("test").SelectByIDWithMetadata(ctx, testMessages[0].MessageID)
	if err != nil {
		t.Fatalf("SelectByID failed: %v", err)
	}

	if msg == nil {
		t.Fatalf("SelectByID failed: nil")
	}

	if msg.Object.Content != testMessages[0].Content {
		t.Fatalf("Unexpected content: %v", msg.Object.Content)
	}

	if msg.Metadata.CreatedBy != "Test_select_with_metadata" {
		t.Fatalf("Unexpected createdBy: %v", msg.Metadata.CreatedBy)
	}

	msgs, err := messagesDB.Tenant("test").SelectAllWithMetadata(ctx)
	if err != nil {
		t.Fatalf("SelectAll failed: %v", err)
	}

	if len(msgs) != len(testMessages) {
		t.Fatalf("Unexpected messages count: %v", len(msgs))
	}

	msgs, err = messagesDB.Tenant("test").SelectWithMetadata(ctx,
		convDB.Where().
			Noop().
			And().
			Key("content").Equals().Value(testMessages[1].Content).
			And().
			CreatedBetween(time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour)).
			And().
			CreatedBy("Test_select_with_metadata").
			And().
			UpdatedBetween(time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour)).
			And().
			UpdatedBy("Test_select_with_metadata").
			And().
			Expression(
				convDB.Where().
					Noop().
					Or().
					UpdatedBy("unknown"),
			).
			And().
			Key("message_id").
			Like().
			Value("%"),
	)
	if err != nil {
		t.Fatalf("Select failed: %v", err)
	}

	if len(msgs) != 1 {
		t.Fatalf("Unexpected messages count: %v", len(msgs))
	}

	for _, msg := range msgs {
		err := messagesDB.Tenant("test").Delete(ctx, msg.Object.MessageID)
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	}

}

func TestSelect_ignoresNullRuntimeObjects(t *testing.T) {
	ctx := convCtx.New(convAuth.Claims{User: convAuth.User(t.Name())})
	clearMessagesDB(ctx)

	if _, err := messagesDB.Tenant("test").Select(ctx, convDB.Where().Noop()); err != nil {
		t.Fatalf("prepare message ObjectSet: %v", err)
	}
	insertNullMessageRows(t)

	messages, err := messagesDB.Tenant("test").Select(
		ctx,
		convDB.Where().Key("content").IsNull().Or().Noop(),
	)
	if err != nil {
		t.Fatalf("Select failed for SQL NULL runtime object: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("Select returned %d SQL NULL runtime objects", len(messages))
	}

	messagesWithMetadata, err := messagesDB.Tenant("test").SelectWithMetadata(
		ctx,
		convDB.Where().Key("content").IsNull().Or().Noop(),
	)
	if err != nil {
		t.Fatalf("SelectWithMetadata failed for SQL NULL runtime object: %v", err)
	}
	if len(messagesWithMetadata) != 0 {
		t.Fatalf(
			"SelectWithMetadata returned %d SQL NULL runtime objects",
			len(messagesWithMetadata),
		)
	}
}

func TestSelectAll_ignoresNullRuntimeObjects(t *testing.T) {
	ctx := convCtx.New(convAuth.Claims{User: convAuth.User(t.Name())})
	clearMessagesDB(ctx)
	if _, err := messagesDB.Tenant("test").Select(ctx, convDB.Where().Noop()); err != nil {
		t.Fatalf("prepare message ObjectSet: %v", err)
	}
	insertNullMessageRows(t)

	t.Run("without metadata", func(t *testing.T) {
		messages, err := messagesDB.Tenant("test").SelectAll(ctx)
		if err != nil {
			t.Fatalf("SelectAll failed for SQL NULL runtime object: %v", err)
		}
		if len(messages) != 0 {
			t.Fatalf("SelectAll returned %d SQL NULL runtime objects", len(messages))
		}
	})

	t.Run("with metadata", func(t *testing.T) {
		messages, err := messagesDB.Tenant("test").SelectAllWithMetadata(ctx)
		if err != nil {
			t.Fatalf("SelectAllWithMetadata failed for SQL NULL runtime object: %v", err)
		}
		if len(messages) != 0 {
			t.Fatalf(
				"SelectAllWithMetadata returned %d SQL NULL runtime objects",
				len(messages),
			)
		}
	})
}

func Test_order_limit_offset(t *testing.T) {

	ctx := convCtx.New(convAuth.Claims{User: "Test_order_limit_offset"})

	clearMessagesDB(ctx)

	testMessages := generateTestMessages()

	for _, msg := range testMessages {
		err := messagesDB.Tenant("test").Insert(ctx, msg)
		if err != nil {
			t.Fatalf("Insert failed: %v", err)
		}
	}

	msg, err := messagesDB.Tenant("test").SelectByID(ctx, testMessages[0].MessageID)
	if err != nil {
		t.Fatalf("SelectByID failed: %v", err)
	}

	if msg == nil {
		t.Fatalf("SelectByID failed: nil")
	}

	if msg.Content != testMessages[0].Content {
		t.Fatalf("Unexpected content: %v", msg.Content)
	}

	msgs, err := messagesDB.Tenant("test").SelectAll(ctx)
	if err != nil {
		t.Fatalf("SelectAll failed: %v", err)
	}

	if len(msgs) != len(testMessages) {
		t.Fatalf("Unexpected messages count: %v", len(msgs))
	}

	msgs, err = messagesDB.Tenant("test").Select(ctx,
		convDB.Where().
			Key("message_id").
			Like().
			Value("%").
			OrderByAsc("message_id").
			LimitPerShard(10).
			Offset(10),
	)
	if err != nil {
		t.Fatalf("Select failed: %v", err)
	}

	if len(msgs) != 20 { // Note as we have two shards the result is double (the limit is applied to each shard)
		t.Fatalf("Unexpected messages count: %v", len(msgs))
	}

	for _, msg := range msgs {
		err := messagesDB.Tenant("test").Delete(ctx, msg.MessageID)
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	}

}
