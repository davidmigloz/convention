package db_test

import (
	"testing"

	convDB "github.com/sofmon/convention/lib/db"
)

type AnalyzePrepareOK struct {
	ID string `json:"id"`
	K  string `json:"k"`
}

func (o AnalyzePrepareOK) DBKey() convDB.Key[string, string] {
	return convDB.Key[string, string]{ID: o.ID, ShardKey: o.ID}
}

type AnalyzePrepareBroken struct {
	ID string `json:"id"`
}

func (o AnalyzePrepareBroken) DBKey() convDB.Key[string, string] {
	return convDB.Key[string, string]{ID: o.ID, ShardKey: o.ID}
}

var analyzePrepareOKDB = convDB.NewObjectSet[AnalyzePrepareOK]("complex").WithIndexes("k").Ready()

// SQLite has no jsonb_to_tsvector, so the generated column makes the DDL fail.
var analyzePrepareBrokenDB = convDB.NewObjectSet[AnalyzePrepareBroken]("complex").WithTextSearch().Ready()

func Test_prepare_succeedsOnSQLiteWithAnalyzeCheck(t *testing.T) {
	if err := analyzePrepareOKDB.Tenant("test").EnsurePrepared(); err != nil {
		t.Fatalf("EnsurePrepared: %v", err)
	}
}

func Test_prepare_ddlErrorStaysFatal(t *testing.T) {
	if err := analyzePrepareBrokenDB.Tenant("test").EnsurePrepared(); err == nil {
		t.Fatal("expected the DDL error to be returned")
	}
}
