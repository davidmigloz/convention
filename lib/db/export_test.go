package db

import (
	"database/sql"
	"reflect"
	"sync"
	"testing"
	"time"

	convAuth "github.com/sofmon/convention/lib/auth"
)

// StubMutateBackoffForTest shrinks mutateBackoffBase/mutateBackoffCap to
// microseconds for the duration of t, restoring the originals via
// t.Cleanup. Tests that deliberately exhaust mutateLoop's retry budget (or
// otherwise force at least one backoff wait) sleep through the real
// millisecond-scale backoff by default; this hook keeps them fast without
// touching the retry/backoff logic itself — invocation counts are unaffected
// (precedent: lib/job's PinSingleConnForTest test hook).
func StubMutateBackoffForTest(t *testing.T) {
	t.Helper()
	origBase, origCap := mutateBackoffBase, mutateBackoffCap
	mutateBackoffBase = 1 * time.Microsecond
	mutateBackoffCap = 16 * time.Microsecond
	t.Cleanup(func() {
		mutateBackoffBase = origBase
		mutateBackoffCap = origCap
	})
}

// UseDBForTest makes db the only database of vault/tenant (without closing any other) and empties the type
// registry, as a fresh process would see it. On cleanup it forgets db without
// closing it, so the next Open() reads the configuration again.
func UseDBForTest(t *testing.T, vault Vault, tenant convAuth.Tenant, db *sql.DB, engine Engine) {
	t.Helper()
	resetRegistryForTest()
	dbs = map[Vault]map[convAuth.Tenant][]engineDB{
		vault: {tenant: {{db: db, engine: engine}}},
	}
	dbsOnce.Do(func() {})
	t.Cleanup(func() {
		dbs = nil
		dbsOnce = sync.Once{}
		dbsErr = nil
		resetRegistryForTest()
	})
}

func resetRegistryForTest() {
	typeToTableMu.Lock()
	typeToTable = map[Vault]map[reflect.Type]dbTable{}
	typeToTableMu.Unlock()
}
