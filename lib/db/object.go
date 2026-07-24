package db

import (
	"database/sql"
	"errors"
	"hash/crc32"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
)

type Key[idT, shardKeyT ~string] struct {
	ID       idT
	ShardKey shardKeyT
}

type Object[idT, shardKeyT ~string] interface {
	DBKey() Key[idT, shardKeyT]
}

type dbTable struct {
	ObjectType       reflect.Type
	ObjectTypeName   string
	RuntimeTableName string
	HistoryTableName string
	LockTableName    string
	TextSearch       bool
}

const (
	historySuffix = "_history"
	lockSuffix    = "_lock"

	textSearchIndex = "text_search"
)

var (
	matchFirstCap = regexp.MustCompile("(.)([A-Z][a-z]+)")
	matchAllCap   = regexp.MustCompile("([a-z0-9])([A-Z])")

	typeToTable = map[Vault]map[reflect.Type]dbTable{}

	ErrObjectTypeNotRegistered = errors.New("object type not registered - use NewObjectSet to access vaults")
)

func toSnakeCase(str string) string {
	snake := matchFirstCap.ReplaceAllString(str, "${1}_${2}")
	snake = matchAllCap.ReplaceAllString(snake, "${1}_${2}")
	return strings.ToLower(snake)
}

// sanitizeIndexPart lowercases and replaces every non-identifier rune (including
// the '.' of a nested key path) with '_', yielding a readable index-name prefix.
func sanitizeIndexPart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// indexName builds a collision-safe Postgres identifier (<=63 chars) for a
// per-key index. It ALWAYS appends a hash of table+key so that distinct keys
// which sanitise to the same readable text (e.g. "a.b" vs "a_b") cannot collide
// and have one silently skipped by CREATE INDEX IF NOT EXISTS.
func indexName(table, key string) string {
	sum := crc32.ChecksumIEEE([]byte(table + "\x00" + key))
	suffix := "_" + strconv.FormatUint(uint64(sum), 16)
	readable := table + "_" + sanitizeIndexPart(key)
	if max := 63 - len(suffix); len(readable) > max {
		readable = readable[:max]
	}
	return readable + suffix
}

// jsonIndexStatement returns the DDL for a btree expression index on the JSONB
// path `key` (dotted paths become nested ->, matching keyToJsonColumn used by
// the query builder, so the index expression equals the queried expression).
// btree — not GIN — because the builder compares with `=`/`IN`/range on
// `"object"->'k'`, which a GIN jsonb_ops index cannot serve but the default
// jsonb btree opclass can. Keys must be scalar leaf fields (btree entry-size
// limit).
func jsonIndexStatement(table, key string) string {
	return `CREATE INDEX IF NOT EXISTS "` + indexName(table, key) + `"
ON "` + table + `" ((` + keyToJsonColumn(key) + `));
`
}

func dbsForShardKeys[shardKeyT ~string](vault Vault, tenant convAuth.Tenant, sks ...shardKeyT) ([]*sql.DB, error) {
	s := make([]string, len(sks))
	for i, sk := range sks {
		s[i] = string(sk)
	}
	return dbsByShardKeys(vault, tenant, s...)
}

func NewObjectSet[objT Object[idT, shardKeyT], idT ~string, shardKeyT ~string](vault Vault) ObjectSetSetup[objT, idT, shardKeyT] {

	obj := new(objT)
	objType := reflect.TypeOf(*obj)

	return &objectSet[objT, idT, shardKeyT]{
		vault:   vault,
		objType: objType,
	}
}

func (os *objectSet[objT, idT, shardKeyT]) WithTextSearch() ObjectSetSetup[objT, idT, shardKeyT] {
	os.textSearch = true
	return os
}

func (os *objectSet[objT, idT, shardKeyT]) WithIndexes(indexes ...string) ObjectSetSetup[objT, idT, shardKeyT] {
	os.indexes = append(os.indexes, indexes...)
	return os
}

func (os *objectSet[objT, idT, shardKeyT]) WithCompute(compute func(ctx convCtx.Context, md Metadata, obj *objT) error) ObjectSetSetup[objT, idT, shardKeyT] {
	os.compute = append(os.compute, compute)
	return os
}

func (os *objectSet[objT, idT, shardKeyT]) Ready() ObjectSetReady[objT, idT, shardKeyT] {
	return os
}

type ObjectSetSetup[objT Object[idT, shardKeyT], idT, shardKeyT ~string] interface {
	WithTextSearch() ObjectSetSetup[objT, idT, shardKeyT]
	WithIndexes(indexes ...string) ObjectSetSetup[objT, idT, shardKeyT]
	WithCompute(compute func(ctx convCtx.Context, md Metadata, obj *objT) error) ObjectSetSetup[objT, idT, shardKeyT]
	Ready() ObjectSetReady[objT, idT, shardKeyT]
}

type ObjectSetReady[objT Object[idT, shardKeyT], idT, shardKeyT ~string] interface {
	Tenant(tenant convAuth.Tenant) TenantObjectSet[objT, idT, shardKeyT]
}

type objectSet[objT Object[idT, shardKeyT], idT, shardKeyT ~string] struct {
	prepared   bool
	vault      Vault
	textSearch bool
	indexes    []string
	compute    []func(ctx convCtx.Context, md Metadata, obj *objT) error
	objType    reflect.Type
	table      dbTable
}

func (os *objectSet[objT, idT, shardKeyT]) prepare() (err error) {

	if os.prepared {
		return nil
	}

	err = Open()
	if err != nil {
		return
	}

	tenantDBs, ok := dbs[os.vault]
	if !ok {
		err = ErrNoDBVault
		return
	}

	if _, ok := typeToTable[os.vault]; !ok {
		typeToTable[os.vault] = map[reflect.Type]dbTable{}
	}

	os.table, ok = typeToTable[os.vault][os.objType]
	if ok {
		return // object already registered for that vault
	}

	runtimeTableName := toSnakeCase(os.objType.Name())
	historyTableName := runtimeTableName + historySuffix
	lockTableName := runtimeTableName + lockSuffix

	createScript := `CREATE TABLE IF NOT EXISTS "` + runtimeTableName + `" (
"id" text PRIMARY KEY,
"created_at" timestamp NOT NULL,
"created_by" text NOT NULL,
"updated_at" timestamp NOT NULL,
"updated_by" text NOT NULL,
"object" JSONB NULL`

	if os.textSearch {
		createScript += `,
"text_search" tsvector GENERATED ALWAYS AS (jsonb_to_tsvector('english', "object", '["all"]')) STORED`
	}

	createScript += `
);
CREATE TABLE IF NOT EXISTS "` + historyTableName + `" (
"id" text NOT NULL,
"created_at" timestamp NOT NULL,
"created_by" text NOT NULL,
"updated_at" timestamp NOT NULL,
"updated_by" text NOT NULL,
"object" JSONB NULL
);
CREATE TABLE IF NOT EXISTS "` + lockTableName + `" (
"id" text PRIMARY KEY,
"created_at" timestamp NOT NULL,
"description" text NOT NULL
);
`

	if len(os.indexes) != 0 {
		// One-time cleanup of the always-NULL GIN index a previous version of
		// WithIndexes created under the object type name; the corrected btree
		// indexes below use different (hashed) names, so this self-heals on the
		// first prepare after upgrade. No-op where it never existed.
		createScript += `DROP INDEX IF EXISTS "` + runtimeTableName + `_` + os.objType.Name() + `";
`
	}

	for _, index := range os.indexes {
		createScript += jsonIndexStatement(runtimeTableName, index)
	}

	if os.textSearch {
		createScript += `CREATE INDEX IF NOT EXISTS "` + runtimeTableName + `_` + textSearchIndex + `"
ON "` + runtimeTableName + `" USING gin ("text_search");
`
	}

	for _, entries := range tenantDBs {
		for _, entry := range entries {
			_, err = entry.db.Exec(createScript)
			if err != nil {
				return
			}
		}
	}

	os.table = dbTable{
		ObjectType:       os.objType,
		ObjectTypeName:   os.objType.Name(),
		RuntimeTableName: runtimeTableName,
		HistoryTableName: historyTableName,
		LockTableName:    lockTableName,
		TextSearch:       os.textSearch,
	}

	typeToTable[os.vault][os.objType] = os.table

	return
}

func (os objectSet[objT, idT, shardKeyT]) Tenant(tenant convAuth.Tenant) TenantObjectSet[objT, idT, shardKeyT] {
	return TenantObjectSet[objT, idT, shardKeyT]{
		objectSet: os,
		tenant:    convAuth.Tenant(tenant),
	}
}

type TenantObjectSet[objT Object[idT, shardKeyT], idT, shardKeyT ~string] struct {
	objectSet[objT, idT, shardKeyT]
	tenant convAuth.Tenant
}

func (tos TenantObjectSet[objT, idT, shardKeyT]) EnsurePrepared() error {
	if err := tos.prepare(); err != nil {
		return err
	}
	_, err := DBs(tos.vault, tos.tenant)
	return err
}

func (tos TenantObjectSet[objT, idT, shardKeyT]) RawDBs() ([]*sql.DB, error) {
	if err := tos.prepare(); err != nil {
		return nil, err
	}
	return DBs(tos.vault, tos.tenant)
}

func (tos TenantObjectSet[objT, idT, shardKeyT]) RawDBForShardKey(shardKey shardKeyT) (*sql.DB, error) {
	if err := tos.prepare(); err != nil {
		return nil, err
	}
	return dbByShardKey(tos.vault, tos.tenant, string(shardKey))
}
