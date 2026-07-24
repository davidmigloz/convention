# Database Package Implementation Details (for AI Agents)

This document provides implementation details for AI agents working on the `lib/util/db` package.

> IMPORTANT: AI agents must treat AGENTS.md and README.md as authoritative living documents. Any change to the implementation that affects behaviors must be mirrored in both files. The code and documentation must never drift apart. When the implementation changes, these documents must be updated immediately so they always reflect the current system.

## Package Architecture

### File Organization

- **[database.go](database.go)**: Database connection management, sharding logic
- **[object.go](object.go)**: Object set initialization, table creation, type registration
- **[where.go](where.go)**: Query builder implementation
- **[select.go](select.go)**: Select operations (all variants)
- **[insert.go](insert.go)**: Insert and upsert operations
- **[update.go](update.go)**: Update operations (normal and safe)
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

**Use cases**:
- Copy metadata fields to object (e.g., `obj.CreatedAt = md.CreatedAt`)
- Compute derived fields
- Validate or transform data

### Locking Mechanisms

#### Optimistic Locking (SafeUpdate)

```go
row := tx.QueryRow(`SELECT "object", "created_at", ...
    FROM "`+tos.table.RuntimeTableName+`" WHERE "id"=$1`+lockClause, fromKey.ID)
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
  - `ErrObjectNotFound` when the row is missing.
  - `ErrLockNotAvailable` when `FOR UPDATE NOWAIT` raises SQLSTATE 55P03
    (classified via the unexported `sqlStateProvider` interface — works for
    both `lib/pq.Error` and `pgx`-style errors without a direct driver dep).
  - `ErrCASConflict` when the marshal-compare disagrees (or, defensively, when
    a SQLite-mode UPDATE affects zero rows).

**Dialect split:**

- **Postgres (production):** real CAS. `FOR UPDATE NOWAIT` blocks concurrent
  writers between SELECT and UPDATE; contention surfaces as
  `ErrLockNotAvailable`.
- **SQLite (in-memory test only):** lock clause elided. The comparator
  still catches stale-`from` callers, but two truly concurrent writers can
  race past it with no conflict raised. Acceptable because SQLite usage is
  single-process unit testing.

Callers must not mutate `from`'s business state between load and call. The
comparator is metadata-insensitive (both sides are compute-normalized), so
embedded audit stamps need not match — only business fields do.

#### Pessimistic Locking — two modes

`Lock(ctx, obj, desc, opts ...LockOption)` has two modes selected by `WithLease`.

`SelectByIDAndLock` treats a runtime row whose `object` column is SQL `NULL` as
absent and does not acquire a lock for it. It checks the live-object predicate
again after acquiring the sticky lock to close the read-to-lock race. If the
post-acquisition fetch or JSON decode fails, it releases that lock before
returning. A cleanup failure is joined with the original error; in that case
the returned lock remains non-nil so the caller can retry cleanup.
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
[select.go:238](select.go#L238)

```go
for _, db := range dbs {
    var rows *sql.Rows
    rows, err = db.Query(...)
    // ... accumulate results
}
```

Results from all shards are combined into a single slice.

**Important**: `LimitPerShard(n)` applies limit to EACH shard, so total results = `n * shard_count`.

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
[where.go:321-328](where.go#L321-L328)

```go
func (w *where) Search(text string) whereExpectingLogicalOperator {
    if w.err != nil {
        return w
    }
    w.query.WriteString(`"text_search" @@ to_tsquery('english', $` + strconv.Itoa(len(w.params)+1) + `)`)
    w.params = append(w.params, toTSQuery(text))
    return w
}
```

**Text search preprocessing** ([where.go:431-444](where.go#L431-L444)):
```go
var whitespacePattern = regexp.MustCompile(`\s+`)

func toTSQuery(input string) string {
    // Normalize whitespace
    input = whitespacePattern.ReplaceAllString(input, " ")
    input = strings.TrimSpace(input)
    // Convert spaces to & operator
    input = strings.ReplaceAll(input, " ", " & ")
    return input
}
```

Converts `"hello world"` to `"hello & world"` for AND search.

**Generated column** ([object.go:162-163](object.go#L162-L163)):
```sql
"text_search" tsvector GENERATED ALWAYS AS (jsonb_to_tsvector('english', "object", '["all"]')) STORED
```

Automatically indexes all text in the JSONB object.

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

**Select** ([select.go:220-281](select.go#L220-L281)):
- Loads all results into memory
- Returns slice
- `Select`, `SelectWithMetadata`, `SelectAll`, and
  `SelectAllWithMetadata` exclude runtime rows whose `object` column is SQL
  `NULL`; the same defensive check is applied after scanning
- Good for small/medium result sets

**Process** ([process.go:11-70](process.go#L11-L70)):
- Streams results via callback
- No intermediate storage
- Returns count of processed items
- `Process` and `ProcessWithMetadata` apply the same SQL and post-scan
  live-object checks as `Select`
- Good for large result sets or when transformation is needed

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

## Performance Considerations

1. **Shard key provision**: Always provide shard keys when possible to reduce query fan-out
2. **Indexes**: Use `WithIndexes()` for frequently queried fields
3. **Process for streaming**: Use `Process()` instead of `Select()` for large result sets
4. **Text search indexes**: GIN indexes on tsvector can be expensive; use selectively
5. **History table growth**: History tables grow indefinitely; consider archival strategy
