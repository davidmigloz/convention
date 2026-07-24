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
// Insert: Fails if object already exists
err := objSet.Tenant(tenant).Insert(ctx, obj)

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
| `ErrObjectNotFound`   | Row missing for the given ID                                  | 404          |
| `ErrLockNotAvailable` | Another transaction holds `FOR UPDATE NOWAIT` on the row      | 409          |
| `ErrCASConflict`      | Row mutated between caller's load and `SafeUpdate`            | 409          |

`SafeUpdate` is a true CAS only on Postgres. On SQLite the row lock is elided;
the comparator guards against stale-`from` callers but two truly concurrent
writers can race past it. Use SQLite for tests, Postgres for production.

The comparator normalizes both the current row and your `from` snapshot
through the object set's compute hooks before comparing, so it compares
business state and ignores embedded metadata (audit stamps). You may load
`from` via any path (`SelectByID`, `Process`, hand-built) — just don't mutate
its business fields between load and call.

### Pessimistic Locking

```go
// Lock and select atomically
obj, lock, err := objSet.Tenant(tenant).SelectByIDAndLock(ctx, id, "processing")
if lock == nil {
    // Someone else has the lock
    return
}
defer lock.Unlock()

// Perform operations while holding lock
obj.Field = "updated"
err = objSet.Tenant(tenant).Update(ctx, *obj)
```

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
