# Database Package (db)

A type-safe, multi-tenant database abstraction layer with sharding support, automatic history tracking, and built-in locking mechanisms.

## Overview

This package provides a high-level interface for working with sharded, multi-tenant databases. It supports PostgreSQL and SQLite (in-memory) backends, with automatic table creation, JSONB storage, full-text search, and comprehensive metadata tracking.

## Key Features

- **Multi-tenancy**: Isolated data access per tenant within vaults
- **Sharding**: Automatic data distribution across multiple database instances using CRC32-based shard key hashing
- **Type Safety**: Generic-based API with compile-time type checking
- **Automatic History**: All changes are tracked in history tables
- **Metadata Tracking**: Created/updated timestamps and user information
- **Locking**: Built-in optimistic and pessimistic locking mechanisms
- **Full-Text Search**: PostgreSQL tsvector support for text search
- **Flexible Queries**: Type-safe query builder with complex where clauses
- **JSONB Storage**: Objects stored as JSONB for flexible schema evolution

## Quick Start

### 1. Define Your Object Type

```go
type MessageID string

type Message struct {
    MessageID MessageID `json:"message_id"`
    Content   string    `json:"content"`
}

// Implement the Object interface
func (m Message) DBKey() db.Key[MessageID, MessageID] {
    return db.Key[MessageID, MessageID]{
        ID:       m.MessageID,
        ShardKey: m.MessageID, // Used for shard distribution
    }
}
```

### 2. Create an Object Set

```go
var messagesDB = db.NewObjectSet[Message]("messages_vault").
    WithTextSearch().                       // Optional: enable full-text search
    WithIndexes("status", "chat.chat_id").  // Optional: btree indexes on these JSONB fields (dotted = nested)
    WithCompute(func(ctx convCtx.Context, md db.Metadata, obj *Message) error {
        // Optional: compute derived fields
        return nil
    }).
    Ready()
```

`WithIndexes` creates one btree expression index per field on the exact JSONB
expression the query builder targets (`"object"->'status'`), so `=`/`IN`/range
filters on that field can use it. Pass **scalar leaf fields**; dotted keys index
the nested path. See the [Field Indexes](AGENTS.md#field-indexes-withindexes)
notes for the full contract.

### 3. Perform Operations

```go
ctx := convCtx.New(convAuth.Claims{User: "user@example.com"})

// Insert
msg := Message{MessageID: "msg-1", Content: "Hello"}
err := messagesDB.Tenant("tenant-1").Insert(ctx, msg)

// Select by ID
msg, err := messagesDB.Tenant("tenant-1").SelectByID(ctx, "msg-1")

// Select with where clause
msgs, err := messagesDB.Tenant("tenant-1").Select(ctx,
    db.Where().
        Key("content").Equals().Value("Hello").
        OrderByCreatedAtDesc().
        LimitPerShard(10),
)

// Update
msg.Content = "Updated"
err = messagesDB.Tenant("tenant-1").Update(ctx, msg)

// Delete
err = messagesDB.Tenant("tenant-1").Delete(ctx, "msg-1")
```

## Core Concepts

### Vaults and Tenants

- **Vault**: Logical grouping of database connections (e.g., "messages", "users")
- **Tenant**: Isolated data namespace within a vault for multi-tenancy

### Sharding

Objects are automatically distributed across database shards using CRC32 hash of the shard key:
- Shard index = `CRC32(shardKey) % numberOfShards`
- Queries can target specific shards by providing shard keys
- Without shard keys, queries run across all shards

### Metadata

All objects automatically track:
- `created_at`: Timestamp when created
- `created_by`: User who created (from context)
- `updated_at`: Timestamp of last update
- `updated_by`: User who last updated

### History Tracking

Every insert, update, and delete operation creates a history record. Deleted objects are recorded with NULL object data.

## Query Builder

The `Where()` function provides a type-safe, fluent interface for building queries:

```go
db.Where().
    Key("field").Equals().Value("value").           // Simple equality
    And().Key("deleted_at").IsNull().               // Omitted field (requires json:",omitempty")
    And().Key("count").GreaterThan().Value(10).     // Comparison
    And().Key("status").In().Values("active", "pending"). // IN clause
    And().Search("search terms").                   // Full-text search
    And().CreatedBetween(start, end).               // Time range
    Or().Expression(                                // Nested expressions
        db.Where().Key("priority").Equals().Value("high"),
    ).
    OrderByCreatedAtDesc().                         // Ordering
    LimitPerShard(20).                              // Pagination
    Offset(10)
```

### Supported Operators

- `Equals()`, `NotEquals()`
- `IsNull()`, `IsNotNull()`
- `GreaterThan()`, `GreaterThanOrEquals()`
- `LessThan()`, `LessThanOrEquals()`
- `In()`, `NotIn()`
- `Like()`
- `Search()` — full-text match; plain text only, PostgreSQL text-search
  operators are not interpreted (see [Text Search](#text-search))

`IsNull()` and `IsNotNull()` test the SQL nullity of the JSON path expression.
An absent key is SQL `NULL`; an explicitly stored JSON `null` is not. A nil Go
pointer without `omitempty` is stored as explicit JSON `null`, so match that
case with `Key("field").Equals().Value(nil)`.

### Metadata Filters

- `CreatedBetween(a, b)`, `CreatedBy(user)`
- `UpdatedBetween(a, b)`, `UpdatedBy(user)`

### Ordering

- `OrderByAsc(key)`, `OrderByDesc(key)`
- `OrderByCreatedAtAsc()`, `OrderByCreatedAtDesc()`
- `OrderByUpdatedAtAsc()`, `OrderByUpdatedAtDesc()`

## Operations

### Insert Operations

```go
// Insert: fails if object already exists. A runtime-table primary-key
// violation is classified and wrapped as ErrDuplicateID (id and the
// underlying driver error stay errors.Is/As-reachable):
//   fmt.Errorf("%w: id=%s: %w", db.ErrDuplicateID, key.ID, err)
err := objSet.Tenant(tenant).Insert(ctx, obj)
if errors.Is(err, db.ErrDuplicateID) {
    // 409 — an object with this id already exists.
}

// Upsert: Insert or update if exists
err := objSet.Tenant(tenant).Upsert(ctx, obj)

// Upsert with custom metadata
err := objSet.Tenant(tenant).UpsertWithMetadata(ctx, objWithMetadata)
```

### Select Operations

```go
// Select all
objs, err := objSet.Tenant(tenant).SelectAll(ctx)

// Select by ID (with optional shard keys for optimization)
obj, err := objSet.Tenant(tenant).SelectByID(ctx, id, shardKeys...)

// Select with where clause
objs, err := objSet.Tenant(tenant).Select(ctx, where, shardKeys...)

// Include metadata
objsWithMd, err := objSet.Tenant(tenant).SelectAllWithMetadata(ctx)
objWithMd, err := objSet.Tenant(tenant).SelectByIDWithMetadata(ctx, id)
```

Live-object reads ignore runtime rows whose `object` column is SQL `NULL`.
`SelectByID` and `SelectByIDWithMetadata` return `(nil, nil)` for those rows,
the same as for a missing ID.

`Select*` closes its cursor and releases the shard's connection on every
error path, not only when a shard's rows are exhausted, and surfaces a
mid-iteration driver error instead of silently truncating the results.

### Update Operations

```go
// Update: Fails if object doesn't exist
err := objSet.Tenant(tenant).Update(ctx, obj)

// SafeUpdate: Optimistic concurrency control.
// Only updates if 'from' matches current state. On a stale 'from' returns
// ErrCASConflict; on a row missing returns ErrObjectNotFound; on a contended
// FOR UPDATE NOWAIT (Postgres only) returns ErrLockNotAvailable.
err := objSet.Tenant(tenant).SafeUpdate(ctx, from, to)
if errors.Is(err, db.ErrCASConflict) {
    // 409 — caller's snapshot is stale; reload and retry.
}

// Mutate: does that reload-and-retry loop for you. Loads the row, hands it
// to fn, and retries (up to 5 attempts, with jittered backoff) on
// ErrCASConflict / ErrLockNotAvailable. Returns the persisted object (what a
// subsequent SelectByID would observe). A fn that returns the row unchanged
// performs no write — see the no-op note below.
obj, err = objSet.Tenant(tenant).Mutate(ctx, id, func(cur MyObject) (MyObject, error) {
    cur.Counter++ // safe to mutate cur in place — Mutate clones the CAS
                  // baseline before calling fn
    return cur, nil
})
```

### Delete Operations

```go
err := objSet.Tenant(tenant).Delete(ctx, id, shardKeys...)
```

### Process Operations

For streaming/batch processing without loading all into memory:

```go
count, err := objSet.Tenant(tenant).Process(ctx, where,
    func(ctx convCtx.Context, obj MyObject) error {
        // Process each object
        return nil
    },
    shardKeys...,
)

// With metadata
count, err := objSet.Tenant(tenant).ProcessWithMetadata(ctx, where,
    func(ctx convCtx.Context, obj db.ObjectWithMetadata[MyObject]) error {
        // Process with metadata
        return nil
    },
)
```

Same cursor/connection-release and driver-iteration-error contract as
`Select*` (above); `count` reflects rows successfully processed before
whichever error ended the loop.

## Locking

### Optimistic Locking (SafeUpdate)

```go
current, _ := objSet.Tenant(tenant).SelectByID(ctx, id)
modified := *current
modified.Field = "new value"

// Only succeeds if current hasn't changed since SelectByID.
err := objSet.Tenant(tenant).SafeUpdate(ctx, *current, modified)
switch {
case errors.Is(err, db.ErrObjectNotFound):
    // 404 — row deleted out from under us.
case errors.Is(err, db.ErrLockNotAvailable):
    // 409 — another writer holds the row (Postgres NOWAIT contention).
case errors.Is(err, db.ErrCASConflict):
    // 409 — row mutated between SelectByID and SafeUpdate; reload and retry.
}
```

**Error handling:**

| Error                 | Cause                                                         | Typical HTTP |
|-----------------------|---------------------------------------------------------------|--------------|
| `ErrObjectNotFound`   | Row missing or runtime object is SQL `NULL`                    | 404          |
| `ErrLockNotAvailable` | Another transaction holds `FOR UPDATE NOWAIT` on the row      | 409          |
| `ErrCASConflict`      | Row mutated between caller's load and `SafeUpdate`            | 409          |

`Mutate` surfaces `ErrCASConflict` / `ErrLockNotAvailable` only after
exhausting its retries — see below.

`SafeUpdate` is a true CAS only on Postgres. On SQLite the row lock is
elided, but this package's single-connection pool for in-memory SQLite
serializes writers anyway, so a stale `from` is still caught — by the
comparator, as `ErrCASConflict` — rather than racing past unnoticed. Use
SQLite for tests, Postgres for production.

The comparator normalizes both the current row and your `from` snapshot
through the object set's compute hooks before comparing, so it compares
business state and ignores embedded metadata (audit stamps). You may load
`from` via any path (`SelectByID`, `Process`, hand-built) — just don't mutate
its business fields between load and call.

#### Retrying: Mutate

Hand-rolling the reload/retry loop around `SafeUpdate` is common enough that
`Mutate` does it for you. It returns the persisted object — what a
subsequent `SelectByID` would observe, including any compute-hook stamps —
not `fn`'s return value echoed back:

```go
obj, err := objSet.Tenant(tenant).Mutate(ctx, id, func(cur MyObject) (MyObject, error) {
    cur.Field = "new value" // safe to mutate cur in place — Mutate clones
                             // the CAS baseline before calling fn
    return cur, nil
})
```

**No-op skip (default)**: if `fn` returns the row byte-identical to what
`Mutate` just read, nothing is written — no `updated_at` bump, no history
row — and the returned object is that just-read row, not re-verified against
the database at return (a written return, by contrast, is a fresh re-read).
Want a bump anyway (e.g. to record a touch)? Change a field before returning
from `fn`.

| You need | Use |
|---|---|
| Caller handles the conflict itself (HTTP 409) | `SafeUpdate` |
| Read-modify-write, row must exist | `Mutate` |
| Read-modify-write, row may not exist yet | `MutateOrInsert` |

`fn` may run up to 5 times and must be pure/retry-safe — no side effects
inside it; do those after `Mutate` returns successfully. See
[AGENTS.md](AGENTS.md#optimistic-retry-mutate) for the full retry/backoff
contract.

#### May-create: MutateOrInsert

For a row that may not exist yet, `MutateOrInsert` adds a `seed` that builds
the initial object when `id` is missing; `fn` still runs on both branches
(merge/validation in one place), while `seed`'s only job is the not-yet-existing
row's starting point:

```go
obj, err := objSet.Tenant(tenant).MutateOrInsert(ctx, id,
    func() (MyObject, error) {
        return MyObject{ID: id, Counter: 0}, nil // only used if id is missing
    },
    func(cur MyObject) (MyObject, error) {
        cur.Counter++
        return cur, nil
    },
)
```

A row that already exists is handled exactly like `Mutate` (`seed` is never
called). A duplicate-key insert race — another caller creating the same `id`
concurrently — is absorbed into the same retry budget as a CAS conflict, so
it normally converges rather than erroring; so is the mirror-image delete
race (a racer deleting the row mid-attempt reconverges through the insert
branch — only `Mutate`, whose row must exist, aborts there). See
[AGENTS.md](AGENTS.md#may-create-mutateorinsert) for the ID guard, the
wrong-shard-hint failure mode, and the duplicate-key classification detail.

**Error handling (`Mutate` / `MutateOrInsert`):**

| Error | Cause | Typical HTTP |
|---|---|---|
| `ErrDuplicateID` | A primary-key violation on `Insert`; also surfaced by `MutateOrInsert` once its retry budget is exhausted absorbing insert races (rare — these normally converge first) | 409 |
| `ErrObjectVanished` | The write succeeded, but the row was gone on the immediate post-write re-read | no clean analogue — the write committed; this is the caller's decision to make, not ours to prescribe |

`Mutate` and `MutateOrInsert` surface `ErrCASConflict` / `ErrLockNotAvailable`
only after exhausting their retries — see above. The exhaustion error is
`errors.Is`-matchable against whichever of the four conflict sentinels
recurred last: `ErrCASConflict`, `ErrLockNotAvailable`, `ErrDuplicateID`, or
(the delete-race path) `ErrObjectNotFound`.

### Pessimistic Locking

```go
// Lock and select atomically
obj, lock, err := objSet.Tenant(tenant).SelectByIDAndLock(ctx, id, "processing")
if err != nil {
    if lock != nil {
        // Automatic cleanup failed. Retry before dropping the handle.
        _ = lock.Unlock()
    }
    return
}
if lock == nil {
    // The object is absent, or someone else has the lock.
    return
}
defer lock.Unlock()

// Perform operations while holding lock
obj.Field = "updated"
err = objSet.Tenant(tenant).Update(ctx, *obj)
```

`SelectByIDAndLock` does not acquire locks for runtime rows whose `object`
column is SQL `NULL`. An object that disappears or becomes SQL `NULL` after
lock acquisition returns `ErrObjectNotFound` and triggers cleanup; the error
distinguishes that race from an object that was already absent, which returns
`(nil, nil, nil)`. JSON decode failures use the same cleanup path. If cleanup
also fails, Convention logs the failure with the object ID, returns both
errors, and keeps the lock non-nil so the caller can retry `Unlock`. Losing the
acquisition race never removes the other caller's lock.

#### Lease locks (long-running holders)

The default lock above is a sticky mutex: it is never stolen, so a holder
that crashes without unlocking blocks everyone else forever. For a holder
that may legitimately crash mid-work (e.g. a scheduled job), acquire with
`WithLease` instead — a lock whose heartbeat goes stale is stolen rather than
blocking indefinitely:

```go
lock, err := objSet.Tenant(tenant).Lock(ctx, obj, "processing", db.WithLease(90*time.Second))
if err != nil || lock == nil {
    return // a live owner already holds it
}
defer lock.Unlock() // owner-safe; returns ErrLeaseLost if this lock was stolen

// heartbeat well within the lease, e.g. from the run loop:
// if err := lock.Renew(ctx); errors.Is(err, db.ErrLeaseLost) { /* stop working */ }

obj.Field = "updated"
err = lock.UpdateGuarded(ctx, obj) // persists only while still owning the lease
```

`Renew` and `UpdateGuarded` both return `ErrLeaseLost` once another caller has
stolen the lease. See [AGENTS.md](AGENTS.md#pessimistic-locking--two-modes)
for the full semantics (`Stolen()`, `PreviousOwner()`, and `UpdateGuarded`'s
no-read-then-write-window guarantee).

## Raw SQL Access

Use tenant-bound object-set methods when an operation cannot be expressed by
the typed query API:

```go
tenantObjects := objSet.Tenant(tenant)

// Derived from the object type with the same rule used during preparation.
table := tenantObjects.RuntimeTableName()

// Bootstrap or prepare every participating object set before Begin().
err := tenantObjects.EnsurePrepared()

// Prepared, vault- and tenant-correct access to every configured shard.
dbs, err := tenantObjects.RawDBs()

// Prepared access to the shard selected by the standard CRC32 route.
db, err := tenantObjects.RawDBForShardKey(shardKey)
```

Runtime raw reads should use `RawDBs` or `RawDBForShardKey`; do not combine
`EnsurePrepared` with the global `DBs` function. For a transaction spanning
multiple object sets, call `EnsurePrepared` on every participant before
`Begin()` because preparation may issue DDL outside the transaction.
Use `RuntimeTableName` when constructing SQL instead of hardcoding the derived
name, so a Go object-type rename cannot silently leave the raw query pointing
at an old table.

Raw full-object queries remain responsible for selecting only live rows with
a root `"object" IS NOT NULL` predicate and for treating a nil scanned object
as absent.

## Configuration

Database connections are configured via the config system (typically `.secret` directory):

```json
{
  "database": {
    "vault_name": {
      "tenant_name": [
        {
          "engine": "postgres",
          "host": "localhost",
          "port": 5432,
          "database": "dbname",
          "username": "user",
          "password": "pass"
        },
        {
          "engine": "postgres",
          "host": "localhost",
          "port": 5433,
          "database": "dbname2",
          "username": "user",
          "password": "pass"
        }
      ]
    }
  }
}
```

Multiple connections per tenant enable sharding.

## Advanced Features

### Text Search

Enable text search on object sets:

```go
var docs = db.NewObjectSet[Document]("docs").
    WithTextSearch().
    Ready()

// Search using PostgreSQL full-text search
results, err := docs.Tenant(tenant).Select(ctx,
    db.Where().Search("keyword1 keyword2"),
)
```

`Search` takes **plain text, not a query language**. The text is bound as a
parameter to PostgreSQL's `plainto_tsquery('english', …)`: it is tokenised,
stemmed, stripped of stop words, and the remaining words must all match (AND).

- Any input is accepted. Apostrophes, operators and unbalanced parentheses
  (`O'Brien`, `foo &`, `bar )`) are treated as text rather than parsed, so they
  cannot fail the query.
- Search operators are **not** supported. `:*`, `!`, `|`, `<->` and parentheses
  are ordinary punctuation and are dropped.
- Empty, whitespace-only or stop-word-only text matches **no rows**.
- Text longer than 64 KiB is truncated.
- Requires `WithTextSearch()`, and therefore PostgreSQL — the generated
  `tsvector` column is not available on the SQLite engine.

### Compute Functions

Add derived/computed fields during select operations:

```go
var items = db.NewObjectSet[Item]("items").
    WithCompute(func(ctx convCtx.Context, md db.Metadata, obj *Item) error {
        obj.CreatedAt = md.CreatedAt  // Copy metadata to object
        obj.Age = time.Since(md.CreatedAt)
        return nil
    }).
    Ready()
```

### Nested JSON Queries

Query nested JSON fields using dot notation:

```go
db.Where().Key("address.city").Equals().Value("New York")
```

## Best Practices

1. **Shard Key Selection**: Choose shard keys that distribute data evenly
2. **Provide Shard Keys**: When possible, provide shard keys to queries to avoid scanning all shards
3. **Use Process for Large Sets**: For large result sets, use `Process()` instead of `Select()` to avoid memory issues
4. **Leverage Metadata**: Use `CreatedAt`, `UpdatedAt` for audit trails and time-based queries
5. **SafeUpdate for Conflicts**: Use `SafeUpdate()` when concurrent modifications are possible
6. **Lock Judiciously**: Use locks only when necessary; they impact concurrency

## Error Handling

Common errors:
- `ErrNoDBVault`: Vault not configured
- `ErrNoDBTenant`: Tenant not configured for vault
- `ErrObjectTypeNotRegistered`: Object type not initialized with `NewObjectSet`
- `sql.ErrNoRows`: Object not found (handled internally, returns nil)

## Thread Safety

- Connection pooling is handled by `database/sql`
- ObjectSet instances are safe to use concurrently
- Lock operations provide synchronization across processes/instances
