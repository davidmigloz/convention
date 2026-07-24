package db_test

import (
	"errors"
	"hash/crc32"
	"testing"

	convDB "github.com/sofmon/convention/lib/db"
)

type RawAccessID string
type RawAccessShard string

type RawAccessObject struct {
	ID       RawAccessID    `json:"id"`
	ShardKey RawAccessShard `json:"shard_key"`
}

func (o RawAccessObject) DBKey() convDB.Key[RawAccessID, RawAccessShard] {
	return convDB.Key[RawAccessID, RawAccessShard]{
		ID:       o.ID,
		ShardKey: o.ShardKey,
	}
}

func Test_EnsurePrepared_is_idempotent_and_prepares_every_shard(t *testing.T) {
	objectSet := convDB.NewObjectSet[RawAccessObject]("messages").Ready()
	tenantObjectSet := objectSet.Tenant("test")

	if err := tenantObjectSet.EnsurePrepared(); err != nil {
		t.Fatalf("first EnsurePrepared failed: %v", err)
	}
	if err := tenantObjectSet.EnsurePrepared(); err != nil {
		t.Fatalf("second EnsurePrepared failed: %v", err)
	}

	dbs, err := convDB.DBs("messages", "test")
	if err != nil {
		t.Fatalf("DBs failed: %v", err)
	}
	for i, db := range dbs {
		for _, table := range []string{
			"raw_access_object",
			"raw_access_object_history",
			"raw_access_object_lock",
		} {
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM "` + table + `"`).Scan(&count); err != nil {
				t.Fatalf("shard %d table %q was not prepared: %v", i, table, err)
			}
		}
	}

	if err := objectSet.Tenant("unknown").EnsurePrepared(); !errors.Is(err, convDB.ErrNoDBTenant) {
		t.Fatalf("EnsurePrepared unknown tenant error = %v, want ErrNoDBTenant", err)
	}
	missingVault := convDB.NewObjectSet[RawAccessObject]("missing").Ready()
	if err := missingVault.Tenant("test").EnsurePrepared(); !errors.Is(err, convDB.ErrNoDBVault) {
		t.Fatalf("EnsurePrepared unknown vault error = %v, want ErrNoDBVault", err)
	}
}

func Test_RawDBs_prepares_and_preserves_vault_tenant_and_shard_order(t *testing.T) {
	objectSet := convDB.NewObjectSet[RawAccessObject]("messages").Ready()

	got, err := objectSet.Tenant("test").RawDBs()
	if err != nil {
		t.Fatalf("RawDBs failed: %v", err)
	}
	want, err := convDB.DBs("messages", "test")
	if err != nil {
		t.Fatalf("DBs failed: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("RawDBs returned %d shards, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RawDBs shard %d returned the wrong database handle", i)
		}
		var count int
		if err := got[i].QueryRow(`SELECT COUNT(*) FROM "raw_access_object"`).Scan(&count); err != nil {
			t.Errorf("RawDBs shard %d was not prepared before access: %v", i, err)
		}
	}

	if _, err := objectSet.Tenant("unknown").RawDBs(); !errors.Is(err, convDB.ErrNoDBTenant) {
		t.Fatalf("RawDBs unknown tenant error = %v, want ErrNoDBTenant", err)
	}
	missingVault := convDB.NewObjectSet[RawAccessObject]("missing").Ready()
	if _, err := missingVault.Tenant("test").RawDBs(); !errors.Is(err, convDB.ErrNoDBVault) {
		t.Fatalf("RawDBs unknown vault error = %v, want ErrNoDBVault", err)
	}
}

func Test_RawDBForShardKey_prepares_and_uses_the_standard_route(t *testing.T) {
	objectSet := convDB.NewObjectSet[RawAccessObject]("messages").Ready()
	all, err := objectSet.Tenant("test").RawDBs()
	if err != nil {
		t.Fatalf("RawDBs failed: %v", err)
	}

	for _, shardKey := range []RawAccessShard{"alpha", "bravo", "charlie", "delta"} {
		got, err := objectSet.Tenant("test").RawDBForShardKey(shardKey)
		if err != nil {
			t.Fatalf("RawDBForShardKey(%q) failed: %v", shardKey, err)
		}
		wantIndex := int(crc32.ChecksumIEEE([]byte(shardKey)) % uint32(len(all)))
		if got != all[wantIndex] {
			t.Errorf("RawDBForShardKey(%q) returned the wrong shard", shardKey)
		}
		var count int
		if err := got.QueryRow(`SELECT COUNT(*) FROM "raw_access_object"`).Scan(&count); err != nil {
			t.Errorf("RawDBForShardKey(%q) did not prepare the object set: %v", shardKey, err)
		}
	}

	oneShard := convDB.NewObjectSet[RawAccessObject]("complex").Ready()
	only, err := oneShard.Tenant("test").RawDBForShardKey("anything")
	if err != nil {
		t.Fatalf("single-shard RawDBForShardKey failed: %v", err)
	}
	complexDBs, err := convDB.DBs("complex", "test")
	if err != nil {
		t.Fatalf("complex DBs failed: %v", err)
	}
	if len(complexDBs) != 1 || only != complexDBs[0] {
		t.Fatal("single-shard RawDBForShardKey returned the wrong database")
	}

	if _, err := objectSet.Tenant("unknown").RawDBForShardKey("alpha"); !errors.Is(err, convDB.ErrNoDBTenant) {
		t.Fatalf("RawDBForShardKey unknown tenant error = %v, want ErrNoDBTenant", err)
	}
	missingVault := convDB.NewObjectSet[RawAccessObject]("missing").Ready()
	if _, err := missingVault.Tenant("test").RawDBForShardKey("alpha"); !errors.Is(err, convDB.ErrNoDBVault) {
		t.Fatalf("RawDBForShardKey unknown vault error = %v, want ErrNoDBVault", err)
	}
}
