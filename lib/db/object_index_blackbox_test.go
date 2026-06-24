package db_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
)

type tsIndexID string

type tsIndexObject struct {
	ID         tsIndexID `json:"id"`
	TextSearch string    `json:"text_search"`
}

func (o tsIndexObject) DBKey() convDB.Key[tsIndexID, tsIndexID] {
	return convDB.Key[tsIndexID, tsIndexID]{ID: o.ID, ShardKey: o.ID}
}

// tsIndexDB declares an index on a field literally named "text_search" — which
// the old reservation in prepare() rejected. It must now be accepted.
var tsIndexDB = convDB.NewObjectSet[tsIndexObject]("complex").
	WithIndexes("text_search").
	Ready()

// Test_WithIndexes_textSearch_notReserved exercises prepare() (via an Insert) to
// prove the stale "reserved for text search" rejection is gone. Asserting on the
// helpers alone would not catch a lingering reservation. If the SQLite test
// build cannot create the JSONB expression index, prepare() may fail for an
// unrelated DDL reason — that still proves the reservation is gone.
func Test_WithIndexes_textSearch_notReserved(t *testing.T) {
	ctx := convCtx.New(convAuth.Claims{User: "Test_WithIndexes_textSearch_notReserved"})

	err := tsIndexDB.Tenant("test").Insert(ctx, tsIndexObject{
		ID:         tsIndexID(uuid.NewString()),
		TextSearch: "hello world",
	})
	if err != nil && strings.Contains(err.Error(), "reserved for text search") {
		t.Fatalf("WithIndexes(\"text_search\") still hits the stale reservation: %v", err)
	}
}
