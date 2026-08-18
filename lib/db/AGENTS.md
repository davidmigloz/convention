# Database Package Implementation Details (for AI Agents)

This document provides implementation details for AI agents working on the `lib/util/db` package.

> IMPORTANT: AI agents must treat AGENTS.md and README.md as authoritative living documents. Any change to the implementation that affects behaviors must be mirrored in both files. The code and documentation must never drift apart. When the implementation changes, these documents must be updated immediately so they always reflect the current system.

## Package Architecture

### File Organization

- **[database.go](database.go)**: Database connection management, sharding logic
- **[object.go](object.go)**: Object set initialization, table creation, type registration
- **[where.go](where.go)**: Query builder implementation
- **[select.go](select.go)**: Select operations (all variants)
- **[insert.go](insert.go)**: Insert and upsert operations; classifies runtime-table primary-key violations as `ErrDuplicateID`
- **[update.go](update.go)**: Update operations (normal and safe)
- **[mutate.go](mutate.go)**: Optimistic-retry combinator over SafeUpdate
- **[delete.go](delete.go)**: Delete operations
- **[lock.go](lock.go)**: Locking mechanisms
- **[metadata.go](metadata.go)**: Metadata types and operations
- **[process.go](process.go)**: Stream processing operations

### Core Types and Interfaces

#### Object Interface
```go
type Object[idT, shardKeyT ~string] interface {
    DBKey() Key[idT, shardKeyT]
}
```

Every database object must implement this interface. The `DBKey()` method returns:
- `ID`: Unique identifier (primary key)
- `ShardKey`: Key used for shard distribution (can be same as ID)

#### Key Structure
```go
type Key[idT, shardKeyT ~string] struct {
    ID       idT
    ShardKey shardKeyT
}
```

#### ObjectSet Flow
```go
NewObjectSet[objT]("vault") → ObjectSetSetup → Ready() → ObjectSetReady → Tenant(t) → TenantObjectSet
```

1. `objectSet[objT]`: Internal struct holding configuration
2. `ObjectSetSetup`: Interface for fluent configuration
3. `ObjectSetReady`: Interface exposing `Tenant()` method
4. `TenantObjectSet`: Final interface with CRUD operations

## Database Schema

### Runtime Table Structure
```sql
CREATE TABLE IF NOT EXISTS "{table_name}" (
    "id" text PRIMARY KEY,
    "created_at" timestamp NOT NULL,
    "created_by" text NOT NULL,
    "updated_at" timestamp NOT NULL,
    "updated_by" text NOT NULL,
    "object" JSONB NULL,
    "text_search" tsvector GENERATED ALWAYS AS (jsonb_to_tsvector('english', "object", '["all"]')) STORED  -- optional
);
```

### History Table Structure
```sql
CREATE TABLE IF NOT EXISTS "{table_name}_history" (
    "id" text NOT NULL,  -- NOT a primary key (multiple versions)
    "created_at" timestamp NOT NULL,
    "created_by" text NOT NULL,
    "updated_at" timestamp NOT NULL,
    "updated_by" text NOT NULL,
    "object" JSONB NULL  -- NULL indicates deletion
);
```

### Lock Table Structure
```sql
CREATE TABLE IF NOT EXISTS "{table_name}_lock" (
    "id" text PRIMARY KEY,
    "created_at" timestamp NOT NULL,
    "description" text NOT NULL
);
```

## Implementation Details

### Table Name Generation
[object.go:49-53](object.go#L49-L53)

Table names are derived from type names via `toSnakeCase()`:
- `Message` → `message`
- `UserProfile` → `user_profile`
- Suffixes: `_history`, `_lock`

### Sharding Algorithm
[database.go:114-116](database.go#L114-L116)

```go
func indexByShardKey(key string, count int) int {
    return int(crc32.ChecksumIEEE([]byte(key)) % uint32(count))
}
```

Simple modulo distribution using CRC32 checksum. This provides:
- Deterministic shard assignment
- Reasonable distribution for most data
- Fast computation

**Important**: Resharding is NOT supported. Changing shard count requires data migration.

### Connection Management

- `dbs map[Vault]map[Tenant][]engineDB`: Global connection registry. Each
  `engineDB` pairs a `*sql.DB` with its `Engine` so dialect-aware primitives
  (notably `SafeUpdate`'s lock clause) can pick the right SQL fragment.
- Public `DBs(vault, tenant) ([]*sql.DB, error)` projects to `*sql.DB` only —
  signature is preserved for downstream callers. Engine-aware access is
  internal-only via `dbByShardKeyWithEngine`.
- Tenant-bound raw access should use `TenantObjectSet.RawDBs()` or
  `TenantObjectSet.RawDBForShardKey()`. Both prepare the object set before
  returning handles and preserve its configured vault, tenant, shard order,
  and CRC32 routing.
- Raw SQL must obtain its derived runtime table name from
  `TenantObjectSet.RuntimeTableName()` rather than duplicating the snake-case
  type-name convention. The accessor is deterministic and does not require
  preparation.
- `TenantObjectSet.EnsurePrepared()` is for bootstrap code and for preparing
  every participating object set before `Begin()`. Runtime raw reads should
  use `RawDBs` or `RawDBForShardKey` rather than pairing `EnsurePrepared` with
  the global `DBs` function.
- `Open()`: Lazy initialization, idempotent (guarded by `sync.Once`).
- `Close()`: Closes all connections, sets `dbs = nil`, resets the once-guard.
- Connections are opened from config at `configKeyDatabase = "database"`.

### Configuration Loading
[database.go:74](database.go#L74)

```go
cfg, err := convCfg.Object[config](configKeyDatabase)
```

Expected config structure:
```go
type config map[Vault]map[convAuth.Tenant]connections
type connections []connection
type connection struct {
    Engine   Engine `json:"engine"`      // "postgres" or "sqlite3"
    Host     string `json:"host"`
    Port     int    `json:"port"`
    InMemory bool   `json:"in_memory"`   // For sqlite3
    Database string `json:"database"`
    Username string `json:"username"`
    Password string `json:"password"`
}
```

### ObjectSet Preparation
[object.go:114-216](object.go#L114-L216)

The `prepare()` method:
1. Checks if already prepared (idempotent)
2. Calls `Open()` to ensure connections
3. Registers type in `typeToTable` if not already registered
4. Generates table names from type name
5. Creates all tables (runtime, history, lock) with `IF NOT EXISTS`
6. Creates btree expression indexes for the fields passed to `WithIndexes` (see [Field Indexes](#field-indexes-withindexes))
7. Stores table metadata in `typeToTable[vault][objType]`

**Important**: Table creation happens on first access, not at program start.
Call `EnsurePrepared()` when transaction setup must guarantee the tables exist
before opening the transaction.

### Where Builder Pattern
[where.go](where.go)

The where builder uses a fluent interface with type state transitions:
1. `whereExpectingFirstStatement`: Initial state, or after logical operator
2. `whereExpectingOperators`: After `Key()` call
3. `whereExpectingValue`: After a value comparison operator; `IsNull()` and
   `IsNotNull()` return directly to the logical-operator state
4. `whereExpectingValues`: After `In()` or `NotIn()`
5. `whereExpectingLogicalOperator`: After value, can add `And()`/`Or()` or ordering

**SQL Generation**:
- Predicate and `ORDER BY` / `LIMIT` / `OFFSET` tail are built separately,
  then combined by `statement()` for callers that need the original complete
  statement
- Parameters stored in slice
- Parameter placeholders: `$1`, `$2`, etc. (PostgreSQL style)
- `statement()` method returns `(query, params, error)`
- Filtered collection reads use the separated parts to emit
  `"object" IS NOT NULL AND (<predicate>) <tail>`, preserving the caller's
  top-level `OR` semantics without allowing it to bypass the live-object guard

**Error latching**: the builder latches the first error into `w.err` (empty
key, `json.Marshal` failure, error propagated from an inner `Expression`) and
every subsequent builder method is a no-op — nothing more is written to the
query and no params are appended. `strings.Builder` writes never fail, so
their results are discarded rather than assigned to `w.err` (assigning would
clobber a previously latched error with `nil`). `statement()` surfaces the
latched error.

**JSON Column Access**:
```go
func keyToJsonColumn(key string) string
```
Converts dot notation to PostgreSQL JSONB operators:
- `"field"` → `"object"->'field'`
- `"address.city"` → `"object"->'address'->'city'`

`IsNull()` / `IsNotNull()` apply SQL null predicates directly to that JSON
path expression. Missing keys yield SQL `NULL`; explicit JSON `null` remains
a JSONB value and does not match `IsNull()`. A nil Go pointer without
`omitempty` is stored as explicit JSON `null`; query that representation with
`Key("field").Equals().Value(nil)`.

**Parameter Marshaling**:
[where.go:270-281](where.go#L270-L281)

All values are JSON-marshaled before being added to params:
```go
jsonValue, err := json.Marshal(value)
if err != nil {
    w.err = err
    return w
}
w.query.WriteString(`$` + strconv.Itoa(len(w.params)+1))
w.params = append(w.params, string(jsonValue))
```

This ensures proper type handling in JSONB comparisons. A marshal failure
latches the error before anything is emitted, so neither a placeholder nor a
parameter is added for the failing value.

### Transaction Patterns

#### Insert/Update/Delete Pattern
[insert.go:25-38](insert.go#L25-L38)

```go
tx, err := db.Begin()
defer func() {
    if err != nil {
        err = errors.Join(err, tx.Rollback())
        return
    }
    err = tx.Commit()
}()
```

All write operations use transactions with deferred commit/rollback.

#### History Recording
After every runtime table modification:
```go
_, err = tx.Exec(`INSERT INTO "`+tos.table.HistoryTableName+`"
    SELECT "id", "created_at", "created_by", "updated_at", "updated_by", "object"
    FROM "`+tos.table.RuntimeTableName+`" WHERE "id"=$1`, key.ID)
```

This creates a snapshot of the current state in history.

### Duplicate-Key Classification (Insert)

`Insert`'s runtime-table `tx.Exec` error site classifies a primary-key
violation and wraps it before returning:

```go
if isDuplicateInsertErr(err, engine) {
    err = fmt.Errorf("%w: id=%s: %w", ErrDuplicateID, key.ID, err)
}
```

- `Insert` resolves its DB handle via `dbByShardKeyWithEngine` (not
  `dbByShardKey`) specifically so `engine` is available for classification.
- `isDuplicateInsertErr` (insert.go) uses the same `hasSQLState` probe as the
  55P03 lock-contention classifier: Postgres SQLSTATE `23505`, checked
  unconditionally on both engines; SQLite (test-only) additionally falls
  back, gated to `engine == EngineSqlite3`, to matching the driver's
  `"UNIQUE constraint failed"` message substring (mattn/go-sqlite3 exposes
  its error code as struct fields, not through an interface).
- The deferred `errors.Join(err, tx.Rollback())` preserves `errors.Is`
  reachability (`errors.Join` implements `Unwrap() []error`), and the
  underlying driver error stays reachable too, via the wrap's second `%w`.
- Only the runtime-table `Exec` is classified — the history table has no
  PK/unique constraint (see the History Table Structure above), so `23505`
  is structurally impossible there. `Upsert`/`UpsertWithMetadata` are
  unaffected: `ON CONFLICT ("id") DO UPDATE` on the real PK is atomic and
  cannot raise `23505` for that target.
- `MutateOrInsert` propagates `ErrDuplicateID` from its own `Insert` call
  into its retry budget rather than classifying anything itself — see
  [May-create: MutateOrInsert](#may-create-mutateorinsert) below.

### Metadata Handling

#### Insert
[insert.go:40-44](insert.go#L40-L44)

```go
var md Metadata
md.CreatedAt = ctx.Now()
md.CreatedBy = ctx.User()
md.UpdatedAt = md.CreatedAt
md.UpdatedBy = md.CreatedBy
```

#### Update
[update.go:43-44](update.go#L43-L44)

Fetches existing metadata, only updates `UpdatedAt` and `UpdatedBy`.

#### Upsert
[insert.go:106-119](insert.go#L106-L119)

Queries for existing metadata:
- If `sql.ErrNoRows`: Treat as insert (all fields set to current)
- If exists: Only update `UpdatedAt` and `UpdatedBy`

### Compute Functions
[object.go:84-87](object.go#L84-L87)

Compute functions run during select operations:
```go
for _, compute := range tos.compute {
    err = compute(ctx, md, &obj)
    if err != nil {
        return
    }
}
```

**Execution points**:
- After unmarshaling object from database
- Before returning to caller
- Applied to all select operations (Select, SelectByID, SelectAll, Process)
- Runtime rows whose `object` column is SQL `NULL` are absent from live-object
  reads. `SelectByID` and `SelectByIDWithMetadata` return `(nil, nil)` for the
  same SQL-NULL row shape as for a missing ID.

**Use cases**:
- Copy metadata fields to object (e.g., `obj.CreatedAt = md.CreatedAt`)
- Compute derived fields
- Validate or transform data

### Locking Mechanisms

#### Choosing a concurrency primitive

| You need | Use |
|---|---|
| Last-writer-wins overwrite, single writer assumed | `Update` |
| Reject-if-changed, caller handles conflict (HTTP 409) | `SafeUpdate` |
| Read-modify-write, row must exist | `Mutate` |
| Read-modify-write, row may not exist yet | `MutateOrInsert` |
| Short exclusive critical section, never stolen | sticky `Lock` / `SelectByIDAndLock` |
| Long-running holder surviving crashes, writes only while owned | `Lock(WithLease)` + `Renew` + `UpdateGuarded` |

#### Optimistic Locking (SafeUpdate)

```go
row := tx.QueryRow(`SELECT "object", "created_at", ...
    FROM "`+tos.table.RuntimeTableName+`"
    WHERE "id"=$1 AND "object" IS NOT NULL`+lockClause, fromKey.ID)
```

- Engine-aware lock clause: `FOR UPDATE NOWAIT` on Postgres, empty on SQLite
  (the registry tracks `Engine` alongside each `*sql.DB`).
- Scans the JSONB column into `[]byte`, then `json.Unmarshal` into `objT` — same
  pattern as `SelectByID`. `database/sql` cannot `Scan` JSONB bytes directly
  into a struct.
- Comparator normalizes BOTH sides through the same `decode → compute-hook →
  marshal` pipeline (with the just-loaded row metadata) before comparing:
  the current row (`cmp`) and a clone of the caller's `from`. Comparing
  Go-marshalled bytes on both sides avoids false-conflicts from Postgres JSONB
  key-reordering / whitespace normalization, and running the compute hooks on
  both sides makes the guard insensitive to embedded metadata (e.g. audit
  stamps) that legitimately differs by load path and timestamp precision. Net:
  the CAS guard compares **business state only**, so `from` may be loaded via
  any path (`SelectByID`, `Process`, hand-built) without false-conflicting.
  Trade-off: a write that only advances metadata (no business change) does not
  trip the guard (no ABA-via-metadata detection).
- Returns exported sentinels (`errors.Is`-friendly):
  - `ErrObjectNotFound` when the row is missing or its runtime `object` is SQL
    `NULL`. This is classified before the caller-provided `from` value enters
    the comparison pipeline.
  - `ErrLockNotAvailable` when `FOR UPDATE NOWAIT` raises SQLSTATE 55P03
    (classified via the shared `hasSQLState` probe over the unexported
    `sqlStateProvider` interface — works for both `lib/pq.Error` and
    `pgx`-style errors without a direct driver dep; the same probe backs
    `Insert`'s SQLSTATE 23505 duplicate-key classification. Requires
    the registered Postgres driver's errors to implement `SQLState()` —
    `lib/pq` gained it in v1.10.5).
  - `ErrCASConflict` when the marshal-compare disagrees (or, defensively, when
    a SQLite-mode UPDATE affects zero rows).

**Dialect split:**

- **Postgres (production):** real CAS. `FOR UPDATE NOWAIT` blocks concurrent
  writers between SELECT and UPDATE; contention surfaces as
  `ErrLockNotAvailable`.
- **SQLite (in-memory test only):** lock clause elided. The comparator
  still catches stale-`from` callers, but the real reason no conflict is
  ever *missed* here is this package's single-connection pool for in-memory
  SQLite (see the Connection pooling paragraph under Testing Patterns,
  below) — two SQLite writers can never truly race at the driver level, so a
  stale `from` is always caught, by the comparator, as `ErrCASConflict`. See
  `SafeUpdate`'s doc comment (update.go) for the full story.

Callers must not mutate `from`'s business state between load and call. The
comparator is metadata-insensitive (both sides are compute-normalized), so
embedded audit stamps need not match — only business fields do.

#### Optimistic retry (Mutate)

```go
func (tos TenantObjectSet[objT, idT, shardKeyT]) Mutate(
    ctx convCtx.Context,
    id idT,
    fn func(cur objT) (objT, error),
    shardKeys ...shardKeyT,
) (obj objT, err error)
```

`Mutate` wraps the SelectByID → fn → SafeUpdate cycle every `SafeUpdate`
caller would otherwise hand-roll, retrying on the two conflict sentinels, for
a row that must already exist. For a row that may not exist yet, see
[MutateOrInsert](#may-create-mutateorinsert) below — both share one internal
loop, so their retry/backoff/cancellation mechanics cannot drift apart.

| Error | Retried? |
|---|---|
| `ErrCASConflict` | yes |
| `ErrLockNotAvailable` | yes (incl. wrapped) |
| `ErrObjectNotFound` | no — returned immediately (`MutateOrInsert` instead absorbs a mid-attempt delete race — see below) |
| fn's own error | no — returned immediately, fn is never retried for its own errors |

`fn` must not be `nil`: `mutateLoop` checks this before any I/O and returns a
plain error if it is. Unlike `fn`, a `nil` `seed` on `MutateOrInsert` is
blessed (see below) — the two are not symmetric.

**Return contract**: on success, `obj` is the value a subsequent `SelectByID`
would observe — including any `WithCompute` stamps — never `fn`'s return
value echoed back. `SafeUpdate` takes the object by value and stamps compute
fields internally, so none of that propagates back out of it; `Mutate`
re-reads the row after a successful write to give an honest answer. The
re-read is also the only faithful option on Postgres, where the JSONB column
keeps nanosecond timestamps but the `created_at`/`updated_at` columns are
microsecond-truncated — echoing `fn`'s return value would drift from what a
real subsequent read reports. On any error, `obj` is the zero value.

An error return does not always mean nothing persisted, though: if the write
itself committed but the post-write re-read failed (`ErrObjectVanished`, or a
transient read error), the mutation IS durably applied even though `Mutate`
returns an error. Callers needing exactly-once effects must make `fn`
idempotent — don't treat `err != nil` as proof nothing happened.

**Freshness caveat**: the returned `obj` may already reflect a concurrent
writer's later mutation — it is "what a subsequent `SelectByID` would
observe", not "your write echoed back verbatim". A caller returning it
directly in an HTTP response must not assume it is exactly what it just
wrote. A no-op-skipped return (below) is even less fresh: it is the row as of
Mutate's own read, not re-verified at return.

**No-op skip (default behaviour)**: if `fn` returns an object byte-identical
(canonical JSON marshal) to the row `Mutate` just read, `Mutate` performs no
write at all — no `SafeUpdate` call, no re-read, no further attempts — and
returns that row as of that read. `updated_at` is left untouched. Want a bump
even when nothing meaningfully changed? Change a field before returning from
`fn` — touch it to bump it. Because the comparison is a plain marshal-byte
compare, an `fn` that touches a `WithCompute`-owned field (e.g. re-stamps a
timestamp a compute hook already owns) always looks changed to it — forcing
an unnecessary write whose hooks then overwrite that same field again on the
way back out. Don't touch compute-owned fields from `fn`.

**A successful return does not prove a write happened**: the no-op skip
returns `(row, nil)` exactly as a real write does, so `(obj, err)` carries no
wrote/skipped signal. "Did *I* transition this row?" is therefore
unanswerable from `Mutate`'s return alone — if a rival already stored
precisely what `fn` produced, this call writes nothing and still succeeds.
The CAS guard does not supply the answer either: it rejects a *stale* `from`,
and a caller that read after the rival committed has a fresh one, so
`SafeUpdate` proceeds and overwrites rather than conflicting. So when a
transition implemented with `Mutate` has to have exactly one winner — a
claim, an enqueue, a one-shot flag — decide that inside `fn`: re-check the
row it is handed and return a sentinel error when another writer already owns
it. `mutateLoop` returns at the `fn` call site, *before* any retry
classification, so that holds even for a sentinel wrapping `ErrCASConflict`
or `ErrObjectNotFound` — errors the loop would otherwise retry (its retry set
is wider than `mutateRetryable`: `MutateOrInsert` also absorbs
`ErrDuplicateID`, and `ErrObjectNotFound` when `seed != nil`). The caller
receives the exact value `fn` returned, so `==` identity, `errors.As` on a
concrete type, and an unpolluted message all survive. The conflict arrives as
ordinary control flow instead of as a lost race that looks like a win.

On `MutateOrInsert`'s **insert branch** `fn` is handed `seed`'s object rather
than a stored row, so there is nothing there to re-check — and a claim or
enqueue row usually does not exist yet, which makes that the likelier branch.
The one-winner property still holds by another route: `Insert` raises
`ErrDuplicateID`, the loop absorbs it into the same retry budget, and the next
attempt takes the update branch where the re-check does apply.

This covers one optimistic mutation, which is the whole of what `fn` can
guard. Ownership that has to outlive the call — a holder writing repeatedly
while it works, or one that must survive its own crash without wedging the
row — is a lease, not a `Mutate`: use `Lock(WithLease)` + `Renew` +
`UpdateGuarded` (see [Pessimistic Locking](#pessimistic-locking--two-modes)
and the primitive-selection table above). `fn` cannot stand in for it — it
sees a single attempt and nothing after the call returns.

**Wrong shard-key hint** (must-exist `Mutate`): `shardKeys` is a
query-routing hint for `SelectByID` only. A wrong hint makes an existing row
look absent, so `Mutate` returns `ErrObjectNotFound` immediately — same
failure mode as a genuinely missing `id`. Contrast `MutateOrInsert`, where a
wrong hint fails safely instead (see below).

- **Attempts**: `mutateMaxAttempts = 5`.
- **Backoff**: exponential from `mutateBackoffBase = 10ms`, doubling each
  attempt, then jittered uniformly over `[d/2, d)` (`math/rand/v2`). At the
  current `mutateMaxAttempts = 5` this produces 4 waits (attempts 1-4; none
  after the final attempt) of roughly `[5,10) + [10,20) + [20,40) + [40,80)`
  ms, jittered. `mutateBackoffCap = 160ms` bounds the doubling but is
  headroom only — it never actually engages at the current attempt count;
  it exists in case `mutateMaxAttempts` is ever raised. Wall-clock
  (`time.NewTimer`), not `ctx.Now()`-driven — this is about real contention,
  not simulated time. `mutateBackoffBase`/`mutateBackoffCap` are `var`, not
  `const`, purely so `export_test.go`'s `StubMutateBackoffForTest` can
  shrink them for fast sleeping tests.
- **Deep-clone guarantee**: before every fn call, the just-loaded row is
  cloned via `cloneViaJSON` (marshal→unmarshal, same technique as
  `SafeUpdate`'s own `from`-clone in update.go — both now share this one
  helper) to become the CAS baseline. A fn that mutates its `cur` argument's
  slices/maps in place is safe — it cannot corrupt the baseline the way a
  shallow copy would, and the no-op skip returns that pristine clone, never
  an alias of `cur`. The clone drops unexported fields, same as
  `SafeUpdate` (encoding/json's rule); a `MarshalJSON` failure on the clone
  aborts immediately — fn never runs, no retry.
- **fn contract**: may run up to 5 times; must be pure or otherwise strictly
  retry-safe — no irreversible or externally visible side effects inside.
  Those belong after `Mutate` returns success: persist first, then
  side-effect. That fn runs with no open transaction is an implementation
  detail (useful for test machinery — see mutate_test.go's racer sub-tests),
  not an invitation to do DB work inside fn.
- **Cancellation**: checked at the top of every loop iteration and during the
  backoff wait (house `time.NewTimer` + `select` on `ctx.Done()` pattern,
  mirroring `lib/job`'s scheduler loop). A cancellation
  observed during backoff returns `errors.Join(ctx.Err(), lastErr)`, so
  callers can `errors.Is` for both `context.Canceled` and the conflict
  sentinel that triggered the wait.
- **Exhaustion**: after `mutateMaxAttempts` failed attempts, returns the last
  conflict error wrapped with an attempt count — still `errors.Is`-matchable
  against whichever conflict recurred last: `ErrCASConflict`,
  `ErrLockNotAvailable`, `ErrDuplicateID` (`MutateOrInsert`'s insert branch),
  or `ErrObjectNotFound` (`MutateOrInsert`'s delete-race absorption).
- **SQLite caveat**: the test-only engine never raises the Postgres NOWAIT
  SQLSTATE. That's not reachable via genuine cross-connection contention in
  this harness either — see the dialect-split note above (single-connection
  pool serializes SQLite writers; a stale `from` is still caught by the
  comparator, as `ErrCASConflict`).
- No functional options — the variadic slot is already `shardKeys`;
  additive later if a caller need appears.

#### May-create: MutateOrInsert

```go
func (tos TenantObjectSet[objT, idT, shardKeyT]) MutateOrInsert(
    ctx convCtx.Context,
    id idT,
    seed func() (objT, error),
    fn func(cur objT) (objT, error),
    shardKeys ...shardKeyT,
) (obj objT, err error)
```

`MutateOrInsert` is `Mutate` for a row that may not exist yet. Both share one
internal retry loop selected by whether `seed` is `nil`: `Mutate(...)` is
exactly `MutateOrInsert` with a `nil` seed, which degrades it to must-exist
semantics — there is no separate must-exist code path to drift out of sync.

- **Branch selection**: each attempt starts with `SelectByID`. Row found →
  the ordinary `Mutate` update branch (`fn` runs on `cur`, no-op skip
  applies, `SafeUpdate`). Row missing → `seed()` builds the initial object,
  `fn` runs on it, and the result is `Insert`ed — the no-op skip does not
  apply here (there is nothing yet-persisted to compare a freshly-seeded row
  against).
- **`fn` runs on BOTH branches** — put merge/validation logic in one place
  there. `seed`'s only job is producing the initial base for a
  not-yet-existing row; it never runs on the update branch. Both must be
  pure/retry-safe, same contract as `Mutate`'s `fn`. `seed` may run once per
  insert-branch attempt.
- **ID guard**: on the insert branch, `fn`'s returned object's `DBKey().ID`
  must equal `id` — unlike `SafeUpdate`, plain `Insert` has no ID guard of
  its own, so `MutateOrInsert` checks it. A mismatch aborts immediately with
  a plain (non-sentinel) error: no retry, no row written under either ID.
- **ShardKey guard**: on the insert branch, `fn`'s returned object's
  `DBKey().ShardKey` must equal `seed`'s — mirroring `SafeUpdate`'s own
  from/to shard-mismatch rejection, which this combinator's update branch
  already inherits but the insert branch previously lacked. Without it, an
  `fn` that changes the shard-key field after `seed` would make `Insert`
  land on a shard other than the one `SelectByID` checked — a true
  cross-shard duplicate. A mismatch aborts immediately with a plain error:
  no retry, no row written under either shard.
- **Duplicate-key race**: if another caller inserts the same `id` between
  this attempt's `SelectByID` and its own `Insert`, `Insert` itself already
  classifies the resulting primary-key violation as `ErrDuplicateID` (see
  [Duplicate-Key Classification (Insert)](#duplicate-key-classification-insert)
  above) — `mutateLoop` only checks `errors.Is(err, ErrDuplicateID)` and, if
  so, absorbs it into the same `mutateMaxAttempts` budget and backoff as a
  CAS conflict, rather than surfacing it as a raw driver error. The next
  attempt's `SelectByID` normally finds the row the other caller just
  created and converges through the ordinary update branch from there.
  Duplicate-key **exhaustion** specifically is not deterministically
  constructible in a single-threaded test: after one collision, the next
  attempt's `SelectByID` necessarily finds the racer's row and moves to the
  update branch, so mutate_test.go's exhaustion test exercises the CAS path
  (a racer `Update` on a pre-existing row) instead — same budget, same code
  path, reached through this wrapper.
- **Delete race** (the mirror image): another caller deleting the row between
  this attempt's `SelectByID` and `SafeUpdate`'s guarded read surfaces as
  `ErrObjectNotFound` — absorbed into the same budget and backoff, retrying
  into the insert branch. Our write never committed, so this is linearizable
  as "their delete, then our insert" — ordinary upsert semantics, unrelated
  to `ErrObjectVanished` (which protects the post-commit re-read, where the
  write DID commit). Only `MutateOrInsert` absorbs it; must-exist `Mutate`
  still aborts with `ErrObjectNotFound` in the same window, as its contract
  requires. This absorption assumes the race is transient — a permanently
  misfiled row (`ShardKey` routing to a shard other than the one it actually
  lives on, unsupported-resharding territory) recurs every attempt and
  simply exhausts instead of converging.
- **Wrong shard-key hint**: `shardKeys` is a query-routing hint for
  `SelectByID` only, never validated against the object's real `ShardKey`. A
  wrong hint fails safely — it does not heal, and it does not corrupt: every
  attempt misses the existing row on `SelectByID` (routed by the hint), takes
  the insert branch, and `Insert` (which routes by the object's own
  `ShardKey`, not the hint) hits the row's real shard and duplicate-key-fails
  there — exhausting the budget with `ErrDuplicateID`, never creating a
  duplicate row. The one caller-bug this specific check cannot defend
  against — `seed` itself building the object with a different `ShardKey`
  than the existing row's — is now caught by the ShardKey guard above
  instead, which aborts before `Insert` is ever attempted.
- **SQL-NULL-object row** (deliberately unresolved, not a bug): a runtime row
  whose `object` column is SQL `NULL` (see the live-object guard elsewhere in
  this document) reads as absent to `SelectByID`, so every attempt takes the
  insert branch and collides with the still-present primary key — exhausting
  with `ErrDuplicateID` rather than reviving the row, even though a caller
  reading the `id` sees it as absent. Intentional: silently resurrecting such
  a row via a may-create combinator would contradict the deliberate-delete
  rationale this package defends elsewhere (upstream issue #15 will forbid
  the state outright). `Upsert`, not `MutateOrInsert`, is the primitive that
  can overwrite a SQL-NULL-object row.
- **Return contract, no-op skip, freshness, and retry/backoff mechanics** are
  identical to `Mutate`'s (above).
- **Sentinels**:
  - `ErrDuplicateID` — produced by `Insert` (see
    [Duplicate-Key Classification (Insert)](#duplicate-key-classification-insert)
    above), absorbed by `mutateLoop` into the retry budget and surfaced
    after exhaustion (normally converged away before that point).
  - `ErrObjectVanished` — returned by the shared post-write re-read (used by
    both the update-success and insert-success paths) when `SelectByID`
    reports the row missing immediately after a successful write. The write
    committed, so this is deliberately not `ErrObjectNotFound`, which would
    invite a may-create caller to treat the row as never having existed and
    re-create it.

#### Pessimistic Locking — two modes

`Lock(ctx, obj, desc, opts ...LockOption)` has two modes selected by `WithLease`.

`SelectByIDAndLock` treats a runtime row whose `object` column is SQL `NULL` as
absent and does not acquire a lock for it. It checks the live-object predicate
again after acquiring the sticky lock to close the read-to-lock race. If the
post-acquisition fetch reports a missing or SQL-NULL row, it returns
`ErrObjectNotFound` and releases the lock. This differs deliberately from an
initially absent row's `(nil, nil, nil)`: the error records that a live row
vanished after this call won lock acquisition. JSON decode failures use the
same cleanup path. A cleanup failure is joined with the original error, logged
at the failure site with the object ID, and returns the non-nil lock so the
caller can retry cleanup.
`RowsAffected()==0` means another caller owns the lock and never triggers
cleanup.

**Default mode (sticky mutex — unchanged):**
```go
res, err := db.Exec(`INSERT INTO "`+tos.table.LockTableName+`"
    ("id","created_at","description")
    VALUES($1,$2,$3)
    ON CONFLICT ("id") DO NOTHING;`, key.ID, ctx.Now(), desc)
```
- `ON CONFLICT DO NOTHING`; `RowsAffected()==0` → returns `nil` (already locked); never stolen.
- Lock persists until `lock.Unlock()` (unconditional `DELETE WHERE id`).
- This is what request-scoped callers rely on (e.g. a resend cooldown, a "someone is already processing" guard). **Do not change it.**

**Lease mode (`WithLease(d)` — for long-lived / scheduled holders):**
```go
res, err := db.ExecContext(ctx.Context, `INSERT INTO "`+lockTable+`" AS l
    ("id","created_at","description") VALUES($1,$2,$3)
    ON CONFLICT ("id") DO UPDATE SET "created_at"=$2, "description"=$3
    WHERE l."created_at" < $4;`, key.ID, now, desc, now.Add(-d))
```
- A lock whose `created_at` is older than `now-lease` is **stolen** (so a holder that crashed without unlocking does not block forever). `RowsAffected()==1` ⇒ acquired or stole; `==0` ⇒ a live (recently-renewed) lock is held.
- `created_at` doubles as the heartbeat timestamp. The holder must `Renew(ctx)` well within `lease` (advances `created_at WHERE id AND description=owner`; returns `ErrLeaseLost` on 0 rows).
- `Unlock()` is **owner-safe** for lease locks (`DELETE WHERE id AND description=owner`, bounded context); returns `ErrLeaseLost` if the lock was stolen away.
- The stored `description` is the caller's text plus a **per-acquisition token** (`desc + "\x1f" + uuid`), so `Renew`/`Unlock` match one specific acquisition — two holders that pass the same `desc` are never confused. `PreviousOwner()` strips the token back to the caller text.
- `UpdateGuarded(ctx, obj)` persists the locked object **only while this caller still owns the lease**, with no read-then-write window. In one transaction it first renews the lease row with an owner-checked `UPDATE lock SET created_at=now WHERE id AND description=owner` — 0 rows ⇒ `ErrLeaseLost`; otherwise it holds that row's write lock, so a concurrent steal (the `ON CONFLICT DO UPDATE`) blocks until commit. It renews the lease **once more right before commit**, so even a long transaction leaves a timestamp newer than any blocked waiter's cutoff (the waiter called `Lock` before this commit) — that waiter cannot take over. The runtime write + ownership claim are thus mutually exclusive with stealing. Not context-cancellable, so it still lands and decides ownership during shutdown.
- `Stolen()` / `PreviousOwner()` expose advisory acquisition metadata (set when an expired row was replaced) for logging.
- The explicit target alias `AS l` keeps the predicate bound to the existing row (never `excluded.created_at`) and is accepted by both Postgres and SQLite. The cutoff is computed in Go and bound as a parameter (no SQL interval arithmetic) for cross-engine parity. Steal comparison assumes NTP-synced clocks.

**Lock struct**:
```go
type Lock[objT, idT, shardKeyT] struct {
    tos   TenantObjectSet[objT, idT, shardKeyT]
    si    int           // Shard index
    id    idT
    owner string        // "" => legacy lock; else desc + per-acquisition token
    stolen        bool   // advisory: replaced an expired row
    previousOwner string // advisory: description of the replaced row
}
```

Stores shard index to unlock on the correct database instance; `owner` makes Renew/Unlock owner-safe in lease mode.

### Multi-Shard Operations

#### Select Operations
[select.go:252](select.go#L252)

```go
for _, db := range dbs {
    var rows *sql.Rows
    rows, err = db.Query(...)
    if err != nil {
        return
    }
    defer rows.Close() // accumulates across shards — see Connection safety below
    // ... accumulate results
    if err = rows.Err(); err != nil {
        return
    }
}
```

Results from all shards are combined into a single slice.

**Important**: `LimitPerShard(n)` applies limit to EACH shard, so total results = `n * shard_count`.

**Connection safety**: `rows.Close()` is deferred immediately after the
`Query` error checks in all four `Select*` variants (and both `Process*`
variants, below). Go defers are function-scoped, not loop-iteration-scoped,
so they accumulate across every shard in the `for _, db := range dbs` loop
and only run when the function itself returns — that is safe because (a) a
happy-path shard's rows already auto-close the moment `Next()` reports
exhaustion, releasing the connection immediately, and the later deferred
`Close` on those same, already-closed rows is a documented no-op; (b) an
early error return runs every accumulated defer, closing whichever single
cursor is still open. `rows.Err()` is checked right after each `for
rows.Next()` loop so a mid-iteration driver error surfaces as a real error
instead of silently truncating the result set.

#### Delete Operations
[delete.go:23-47](delete.go#L23-L47)

Uses transactions across all potential shards:
```go
txs := make([]*sql.Tx, len(dbs))
for i, db := range dbs {
    txs[i], err = db.Begin()
}
defer func() {
    if err != nil {
        for _, tx := range txs {
            err = errors.Join(err, tx.Rollback())
        }
    } else {
        for _, tx := range txs {
            err = errors.Join(err, tx.Commit())
        }
    }
}()
```

Deletes from all shards, commits all or rolls back all.

#### Shard Key Optimization
[database.go:174-201](database.go#L174-L201)

```go
func dbsByShardKeys(vault Vault, tenant convAuth.Tenant, keys ...string) ([]*sql.DB, error)
```

When shard keys provided:
1. Compute unique shard indexes for all keys
2. Return only those database connections
3. Reduces query fan-out

Without shard keys: Returns all databases for tenant.

### Text Search
[where.go:380-399](where.go#L380-L399)

```go
func (w *where) Search(text string) whereExpectingLogicalOperator {
    if w.err != nil {
        return w
    }
    w.query.WriteString(`"text_search" @@ plainto_tsquery('english', $` + strconv.Itoa(len(w.params)+1) + `)`)
    w.params = append(w.params, sanitizeSearchText(text))
    return w
}
```

**The caller's text is bound as data. There is no query-string building.**
`Search` used to normalise whitespace, join the terms with `&` and hand the
result to `to_tsquery`. `to_tsquery` parses its argument as tsquery *syntax*, so
a dangling operator (`hello &`), an unbalanced parenthesis (`hello )`), a leading
apostrophe (`'tis`) or a trailing backslash (`C:\path\`) raised SQLSTATE 42601
and failed the whole statement — an HTTP 500 from a search box.
`plainto_tsquery` runs the same parser and dictionaries but treats its argument
as plain text and never reads a tsquery operator, so **no input can produce a
tsquery syntax error**.

Do not reintroduce a Go-side tsquery builder. An escaper is only ever as
exhaustive as the grammar it was written against; not parsing is correct by
construction. (Contrast `escapeJSONKeySegment`, which escapes because a key
segment is interpolated into SQL text and has no bound-parameter alternative.)

**Input hardening** ([where.go:343-378](where.go#L343-L378)): `sanitizeSearchText`
replaces NUL bytes and invalid UTF-8 with a space and caps the text at
`maxSearchTextBytes = 64KiB`, cutting on a rune boundary. Both guards close
remaining user-triggerable 500s that `plainto_tsquery` does *not* cover:

- a NUL byte cannot exist in a PostgreSQL text value at all (`54000 null
  character not permitted`), and invalid UTF-8 fails when the parameter is
  decoded;
- a tsquery holds at most ~1 MB of lexemes, past which PostgreSQL raises
  `54000 value is too big in tsquery`. Measured on PostgreSQL 16: 339 KB of
  distinct words is accepted, 1.06 MB is not. 64 KiB leaves a wide margin and
  also bounds GIN key fan-out per shard. Text over the cap is silently truncated.

An over-long single *word* needs no guard — PostgreSQL drops it with a notice
("word is too long to be indexed") rather than failing, in `to_tsquery`,
`plainto_tsquery` and `to_tsvector` alike.

Semantics:

- Words are AND-ed after stemming and stop-word removal: `"hello world"` →
  `'hello' & 'world'`. For ordinary whitespace-separated input this is identical
  to what the old `to_tsquery` path produced.
- Empty, whitespace-only, stop-word-only and pure-punctuation input all yield the
  empty tsquery, which matches **no rows**. `Search("")` is a zero-row match, not
  a no-op. Pure punctuation used to error.
- **tsquery operators are ignored, not obeyed.** `:*` (prefix), `!` (negation),
  `|` (or), `<->` (phrase) and parentheses reached `to_tsquery` intact whenever
  the input contained no spaces, and worked. They no longer do anything. In
  particular `Search("!x")` now matches rows containing `x` rather than rows
  lacking it, and `Search("sm:*")` no longer prefix-matches. A caller needing
  those must get an explicit API — do not route query syntax through user-facing
  free text.
- `plainto_tsquery(regconfig, text)` is IMMUTABLE, like `to_tsquery`, so the GIN
  index is used exactly as before.

**Generated column** ([object.go:197-200](object.go#L197-L200)):
```sql
"text_search" tsvector GENERATED ALWAYS AS (jsonb_to_tsvector('english', "object", '["all"]')) STORED
```

Automatically indexes all text in the JSONB object; the GIN index is created at
[object.go:232-236](object.go#L232-L236). The `'english'` configuration is
hard-coded here and in `Search`; the two must stay in sync.

### Field Indexes (`WithIndexes`)

`WithIndexes("state", "grants.allow_pro_supply", …)` creates one **btree expression
index per field** on the exact JSONB expression the query builder targets:

```sql
CREATE INDEX IF NOT EXISTS "<table>_<sanitized>_<hash>"
ON "<table>" (("object"->'state'));
-- nested keys use the same -> chain as keyToJsonColumn:
ON "<table>" (("object"->'grants'->'allow_pro_supply'));
```

Contract:
- **Honours the passed field names** (a previous version discarded them and indexed
  the object type name — an always-NULL key; `prepare()` now `DROP INDEX IF EXISTS`
  that obsolete `<table>_<TypeName>` index once on upgrade).
- **btree, not GIN.** The builder compares with `=`/`IN`/range on `"object"->'k'`,
  which the default jsonb btree opclass serves and GIN `jsonb_ops` does not. (GIN
  stays only for `WithTextSearch`'s tsvector.)
- **Dotted keys** become nested `->` paths via the same `keyToJsonColumn` the builder
  uses, so the index expression equals the queried expression.
- **Index names** are sanitised and always carry a hash suffix (≤63 chars), so keys
  that sanitise to the same text (`a.b` vs `a_b`) never collide.
- **Scalar leaf fields only** — btree has an index-entry size limit; do not index a
  whole nested object.

### Process vs Select

**Select** ([select.go:252-320](select.go#L252-L320)):
- Loads all results into memory
- Returns slice
- `Select`, `SelectWithMetadata`, `SelectAll`, and
  `SelectAllWithMetadata` exclude runtime rows whose `object` column is SQL
  `NULL`; the same defensive check is applied after scanning
- Good for small/medium result sets
- Every shard's `*sql.Rows` is closed on every early-return error path, not
  only at loop exhaustion, and a mid-iteration driver error is surfaced via
  `rows.Err()` rather than silently truncating the results — see
  [Connection safety](#select-operations) under Multi-Shard Operations

**Process** ([process.go:11-79](process.go#L11-L79)):
- Streams results via callback
- No intermediate storage
- Returns count of processed items
- `Process` and `ProcessWithMetadata` apply the same SQL and post-scan
  live-object checks as `Select`
- Good for large result sets or when transformation is needed
- Same close-on-error and `rows.Err()` contract as `Select` (above);
  `count` stands as the number of rows successfully processed before
  whichever error — including a `rows.Err()` failure — ended the loop

```go
count, err := objSet.Tenant(t).Process(ctx, where,
    func(ctx convCtx.Context, obj MyObject) error {
        // Process one object at a time
        return nil  // Return error to abort
    },
)
```

## Type Registration System

### Global Registry
[object.go:44](object.go#L44)

```go
typeToTable = map[Vault]map[reflect.Type]dbTable{}
```

**Why?**
- Each `ObjectSet` instance is stateless (just configuration)
- Need to track which types have been initialized per vault
- Prevents duplicate table creation
- Stores computed table names for reuse

### dbTable Structure
[object.go:24-31](object.go#L24-L31)

```go
type dbTable struct {
    ObjectType       reflect.Type
    ObjectTypeName   string
    RuntimeTableName string
    HistoryTableName string
    LockTableName    string
    TextSearch       bool
}
```

Cached metadata about registered types.

## SQL Query Construction

All queries are manually constructed using string concatenation. No ORM or query builder library is used.

**Pattern**:
```go
query := `SELECT "object", "created_at", ... FROM "` + tos.table.RuntimeTableName + `" WHERE id=$1`
db.Query(query, params...)
```

**Parameter handling**:
- Always use PostgreSQL-style placeholders (`$1`, `$2`, etc.)
- Parameters passed separately to prevent SQL injection
- JSONB values are JSON-marshaled before passing as parameters

## Error Handling Patterns

### Common Patterns

**`sql.ErrNoRows` handling**:
```go
err = db.QueryRow(...).Scan(...)
if err == sql.ErrNoRows {
    err = nil  // Treat as "not found", not an error
    continue   // Check next shard
}
if err != nil {
    return     // Real error
}
```

**Multi-error aggregation**:
```go
for _, tx := range txs {
    err = errors.Join(err, tx.Rollback())
}
```

Uses Go 1.20+ `errors.Join()` to combine multiple errors.

## Testing Patterns

Tests use in-memory SQLite for fast execution:

```json
{
  "database": {
    "messages": {
      "test": [
        {"engine": "sqlite3", "in_memory": true},
        {"engine": "sqlite3", "in_memory": true}
      ]
    }
  }
}
```

Two connections simulate sharding.

**Connection pooling**: `connection.Open()`'s in-memory sqlite branch calls
`db.SetMaxOpenConns(1)`. mattn/go-sqlite3's `:memory:` is otherwise a trap —
each pooled connection opens an independent in-memory database, so a pool
size >1 silently fragments reads/writes across separate DBs (a write on one
connection is invisible to a query that lands on another). This is set at
`Open()` time, not once in `TestMain`, so it survives `Test_open_and_close`,
which closes and reopens every `*sql.DB` — a bug that
would otherwise revert the pool to unlimited and make the callback-racer
tests in mutate_test.go (`Mutate`'s and `MutateOrInsert`'s conflict/race
sub-tests) order-dependent, since they rely on an out-of-band write during
`fn` landing on the same connection the combinator itself is using.

The flip side of the cap: DB work issued from *inside* a held-connection
window — a `Process`/`ProcessWithMetadata` callback, or a compute hook
running during `Select` iteration, on the same shard — no longer lands on a
fragmented second connection the way it silently did before this fix; with
the pool capped at one, it now blocks forever waiting on the exhausted pool
instead. `Mutate`/`MutateOrInsert`'s `fn` is unaffected by this — it holds no
connection while it runs (see the fn contract in
[Optimistic retry (Mutate)](#optimistic-retry-mutate) above). This hazard is
now confined to that narrow window — DB work genuinely concurrent with a
still-open cursor, from inside a running callback or compute hook. It no
longer extends past the erroring call itself: every collection read
(`Select`, `SelectWithMetadata`, `SelectAll`, `SelectAllWithMetadata`,
`Process`, `ProcessWithMetadata`) closes its `*sql.Rows` on every
early-return error path (see [Process vs Select](#process-vs-select) and
[Select Operations](#select-operations)), so a leaked cursor can no longer
outlive the call and starve every later, unrelated query on the same shard
the way it could before this fix.

## Extension Points

When modifying this package, consider:

1. **Adding new operations**: Follow existing transaction patterns in insert/update/delete
2. **New where clauses**: Add methods following the type state pattern
3. **New metadata fields**: Update `Metadata` struct and all table creation scripts
4. **Index support**: Extend `WithIndexes()` and table creation logic
5. **New database engines**: Add case in `connection.Open()` method

## Known Limitations

1. **No resharding**: Changing shard count requires manual data migration
2. **PostgreSQL-specific**: Query syntax uses PostgreSQL placeholders and JSONB operators
3. **SQLite limitations**: Only in-memory mode supported, no file-based SQLite
4. **No migrations**: Schema changes require manual ALTER TABLE statements
5. **No query plan analysis**: No automatic index recommendations
6. **Limit is per-shard**: `LimitPerShard(n)` returns up to `n * shard_count` results
7. **Text search is hardened on the query side only**: `plainto_tsquery` plus
   `sanitizeSearchText` make `Search()` total for any caller text, but the
   *write* side is untouched — `jsonb_to_tsvector` in the generated column
   raises `54000 string is too long for tsvector` on INSERT/UPDATE for any
   object whose indexable text exceeds ~1 MB in total (verified on PostgreSQL
   16). That makes the object unstorable and needs no adversarial input. A
   single over-long token is fine; it is the total that matters. Separately,
   calling `Search()` on an object set that was not configured with
   `WithTextSearch()` fails with SQLSTATE 42703; the `where` builder has no
   handle on the object set and structurally cannot detect it.
8. **`WithTextSearch()` is PostgreSQL-only**: the generated `tsvector` column
   uses `GENERATED ALWAYS AS ... STORED` and cannot be created on the SQLite
   engine, so text search is not exercised by this repo's test suite. Changes
   to `Search()` must be verified against a real PostgreSQL manually.
9. **Mutate/MutateOrInsert backoff is wall-clock**: not `ctx.Now()`-driven, and bounded at 5 attempts
10. **No SQL-level CAS token**: `SafeUpdate`'s guard is the marshal-compare comparator plus the Postgres row lock, not a version column that every writer advances (tracked as a separate version-column design issue)

## Performance Considerations

1. **Shard key provision**: Always provide shard keys when possible to reduce query fan-out
2. **Indexes**: Use `WithIndexes()` for frequently queried fields
3. **Process for streaming**: Use `Process()` instead of `Select()` for large result sets
4. **Text search indexes**: GIN indexes on tsvector can be expensive; use selectively
5. **History table growth**: History tables grow indefinitely; consider archival strategy
