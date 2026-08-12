package db

import (
	"database/sql"
	"errors"
	"fmt"
	"hash/crc32"
	"sync"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCfg "github.com/sofmon/convention/lib/cfg"
)

type Engine string

const (
	EnginePostgres Engine = "postgres"
	EngineSqlite3  Engine = "sqlite3"

	configKeyDatabase convCfg.ConfigKey = "database"
)

type Vault string

type connection struct {
	Engine   Engine `json:"engine"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	InMemory bool   `json:"in_memory"`
	Database string `json:"database"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type connections []connection

func (conn connection) Open() (*sql.DB, error) {
	switch conn.Engine {
	case EnginePostgres:
		return sql.Open(
			"postgres",
			fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=require",
				conn.Host, conn.Port, conn.Username, conn.Password, conn.Database,
			),
		)
	case EngineSqlite3:
		if conn.InMemory {
			db, err := sql.Open("sqlite3", ":memory:")
			if err != nil {
				return nil, err
			}
			// mattn/go-sqlite3 trap: each pooled connection to ":memory:" opens an
			// otherwise-independent in-memory database — database/sql hands callers
			// whichever connection is idle, so a pool >1 silently fragments
			// reads/writes across separate DBs (a write on one connection is
			// invisible to a query that lands on another). Cap to a single
			// connection so every caller shares the same instance. Set here, at
			// Open() time, rather than once in TestMain: that survives
			// Test_open_and_close, which closes and reopens every *sql.DB and
			// would otherwise revert the pool to unlimited and make later
			// Mutate callback-racer tests order-dependent.
			db.SetMaxOpenConns(1)
			return db, nil
		} else {
			// TODO: implement file-based sqlite databases
			return nil, errors.New("sqlite engine does not support file-based databases")
		}
	default:
		return nil, fmt.Errorf("unsupported engine: %s", conn.Engine)
	}
}

type config map[Vault]map[convAuth.Tenant]connections

type engineDB struct {
	db     *sql.DB
	engine Engine
}

var (
	dbs     map[Vault]map[convAuth.Tenant][]engineDB
	dbsOnce sync.Once
	dbsErr  error

	ErrNoDBTenant = errors.New("db is not configured with provided tenant")
	ErrNoDBVault  = errors.New("db is not configured with provided vault")
)

func Open() (err error) {

	dbsOnce.Do(func() {
		dbsErr = openInternal()
	})

	return dbsErr
}

func openInternal() (err error) {

	newDbs := make(map[Vault]map[convAuth.Tenant][]engineDB)

	cfg, err := convCfg.Object[config](configKeyDatabase)
	if err != nil {
		return
	}

	for vault, vaultCfg := range cfg {
		newDbs[vault] = make(map[convAuth.Tenant][]engineDB)
		for tenant, tenantCfg := range vaultCfg {
			for _, conn := range tenantCfg {
				db, err := conn.Open()
				if err != nil {
					return err
				}
				newDbs[vault][tenant] = append(newDbs[vault][tenant], engineDB{db: db, engine: conn.Engine})
			}
		}
	}

	// Assign only after fully populated to avoid race conditions
	dbs = newDbs

	return
}

func Close() (err error) {
	for _, vault := range dbs {
		for _, entries := range vault {
			for _, entry := range entries {
				err = errors.Join(
					err,
					entry.db.Close(),
				)
			}
		}
	}
	if err != nil {
		return
	}

	dbs = nil
	dbsOnce = sync.Once{} // Reset so Open() can be called again
	dbsErr = nil
	return
}

func indexByShardKey(key string, count int) int {
	return int(crc32.ChecksumIEEE([]byte(key)) % uint32(count))
}

func DBs(vault Vault, tenant convAuth.Tenant) ([]*sql.DB, error) {

	entries, err := engineDBs(vault, tenant)
	if err != nil {
		return nil, err
	}

	out := make([]*sql.DB, len(entries))
	for i, e := range entries {
		out[i] = e.db
	}
	return out, nil
}

func engineDBs(vault Vault, tenant convAuth.Tenant) ([]engineDB, error) {

	err := Open()
	if err != nil {
		return nil, err
	}

	vdb, ok := dbs[vault]
	if !ok {
		return nil, ErrNoDBVault
	}

	tdb, ok := vdb[tenant]
	if !ok {
		return nil, ErrNoDBTenant
	}

	return tdb, nil
}

func dbByIndex(vault Vault, tenant convAuth.Tenant, index int) (*sql.DB, error) {

	dbs, err := DBs(vault, tenant)
	if err != nil {
		return nil, err
	}

	if len(dbs) <= 0 {
		return nil, ErrNoDBTenant
	}

	if index < 0 || index >= len(dbs) {
		return nil, errors.New("database index out of range")
	}

	return dbs[index], nil
}

func dbByShardKey(vault Vault, tenant convAuth.Tenant, key string) (*sql.DB, error) {

	dbs, err := DBs(vault, tenant)
	if err != nil {
		return nil, err
	}

	if len(dbs) <= 0 {
		return nil, ErrNoDBTenant
	}

	if len(dbs) > 1 {
		return dbs[indexByShardKey(key, len(dbs))], nil
	}

	return dbs[0], nil
}

func dbByShardKeyWithEngine(vault Vault, tenant convAuth.Tenant, key string) (*sql.DB, Engine, error) {

	entries, err := engineDBs(vault, tenant)
	if err != nil {
		return nil, "", err
	}

	if len(entries) <= 0 {
		return nil, "", ErrNoDBTenant
	}

	if len(entries) > 1 {
		e := entries[indexByShardKey(key, len(entries))]
		return e.db, e.engine, nil
	}

	return entries[0].db, entries[0].engine, nil
}

func dbsByShardKeys(vault Vault, tenant convAuth.Tenant, keys ...string) ([]*sql.DB, error) {

	dbs, err := DBs(vault, tenant)
	if err != nil {
		return nil, err
	}

	if len(dbs) <= 0 {
		return nil, ErrNoDBTenant
	}

	if len(keys) == 0 {
		return dbs, nil
	}

	sis := map[int]any{}

	for _, key := range keys {
		sis[indexByShardKey(key, len(dbs))] = nil
	}

	var res []*sql.DB
	for si := range sis {
		res = append(res, dbs[si])
	}

	return res, nil
}
