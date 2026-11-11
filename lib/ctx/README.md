# ctx - Context Package

A structured context package that wraps Go's `context.Context` to provide consistent logging, error handling, scope tracking, and request-scoped data across the application.

## Core Concept

Every significant function should receive and propagate a `ctx.Context`. This enables:
- Hierarchical scope tracking for debugging
- Automatic error wrapping with scope context
- Structured logging with consistent fields
- Request-scoped data (claims, workflow IDs, etc.)

## Basic Usage Pattern

```go
func someFunc(ctx ctx.Context, uid string) (err error) {
    ctx = ctx.WithScope("someFunc", "uid", uid)
    defer ctx.Exit(&err)

    // ... function logic ...

    return
}
```

### Functions Without Error Returns

For functions that don't return errors but still benefit from scope tracking:

```go
func processItem(ctx ctx.Context, item Item) {
    ctx = ctx.WithScope("processItem", "itemID", item.ID)
    defer ctx.Exit(nil)

    // function body
}
```

### Why This Pattern?

1. **`ctx.WithScope()`** - Creates a new scope with the function name and key parameters. Scopes are chained with ` → ` separators, creating a breadcrumb trail.

2. **`defer ctx.Exit(&err)`** - On function exit, if an error occurred, it wraps the error with the current scope prefix (`✘ scope: original error`). This provides a full stack trace in error messages.

## Creating a Context

### New Context (for agents/services)
```go
ctx := ctx.New(claims)
```

### From HTTP Request
```go
ctx := ctx.WrapContext(r.Context(), agentClaims)
ctx = ctx.WithRequest(r)
```

## Features

### Scope Management

```go
// Add scope with key-value arguments
ctx = ctx.WithScope("processOrder", "orderID", orderID, "userID", userID)

// Add scope with formatted string
ctx = ctx.WithScopef("processing item %d of %d", i, total)

// Get current scope string
scope := ctx.Scope() // "agent → processOrder {orderID=123 userID=456}"
```

### Error Handling

```go
func fetchData(ctx ctx.Context) (data Data, err error) {
    ctx = ctx.WithScope("fetchData")
    defer ctx.Exit(&err)

    data, err = db.Query(...)
    if err != nil {
        return // Error will be wrapped: "✘ agent → fetchData: original error"
    }
    return
}

// Exclude specific errors from wrapping
defer ctx.Exit(&err, ErrNotFound, ErrAlreadyExists)
```

### Logging

The context carries a structured logger (`*slog.Logger`) that automatically includes:
- `env` - Environment (production, staging, etc.)
- `agent` - Agent/service name
- `workflow` - Workflow ID for request tracing
- `user` - Current user
- `scope` - Current scope chain
- `action` - Current action (e.g., "GET /api/users")
- `now` - Time override, when set
- `use_agent_claims` - Present when agent claims are in use

`ctx.LoggerKeys()` returns these eight keys (a new slice on each call, in this order). Together with slog's own `time`, `level`, `msg` and `source`, they are the top-level keys of a log line.

```go
ctx.Logger().Info("processing request", "itemCount", len(items))
ctx.Logger().Error("failed to connect", "error", err)
ctx.Logger().Debug("detailed info", "state", state)

// Add custom logger
ctx = ctx.WithLogger(customLogger)
```

#### Key collisions

slog does not deduplicate keys, and log agents such as fluent-bit drop or corrupt a line that repeats a key. The default logger therefore writes through a wrapper that renames a repeated key in the encoded line:

```go
ctx.Logger().Info("saved", "action", "put", "source", "x")
// {"time":...,"level":"INFO","source":{...},"msg":"saved","action":"GET /api/users","attr_action":"put","attr_source":"x"}
```

- No JSON object at any depth of a line repeats a key.
- The first occurrence keeps its key; a later repeat becomes `attr_<key>`, or `attr<n>_<key>` (n = 2, 3, ...) if that name is taken in the same object. Key order is kept.
- Values are never changed. A line is written exactly as slog produced it, except for the prefixes inserted into repeated keys.
- Loggers passed to `WithLogger` are used as given and are not protected.

### Workflow Tracking

Workflows enable distributed tracing across service boundaries.

```go
// Get current workflow ID
wf := ctx.Workflow()

// Start a new workflow
ctx = ctx.WithNewWorkflow()

// Continue an existing workflow
ctx = ctx.WithWorkflow(Workflow("existing-id"))
```

The workflow ID is automatically extracted from HTTP requests via the `Workflow` header.

### Claims & Authentication

```go
// Get current user's claims
claims := ctx.Claims()
user := ctx.User()

// Set claims
ctx = ctx.WithClaims(newClaims)

// Use agent's original claims (for internal operations)
ctx = ctx.WithAgentClaims()
```

### Time Management

```go
// Get current time (UTC)
now := ctx.Now()

// Override time (useful for testing, non-production only)
ctx = ctx.WithNow(fixedTime)
```

In non-production environments, the `Time-Now` HTTP header can override the current time.

### Request Elapsed Time

```go
// A server stamps the arrival time once, before anything else
ctx = ctx.WithRequestStart().WithRequest(r, true)

// Time since the stamp; ok is false when the context was never stamped
elapsed, ok := ctx.RequestElapsed()
```

`WithRequest` does not stamp, so attaching another request to a derived context never moves the arrival time. `RequestElapsed` reads the clock the stamped context had: `WithNow` on a derived context and the `Time-Now` header do not change it, and it is `0` when the stamped context's own clock is fixed. It is wall-clock arithmetic, so a clock step backwards can give a negative duration.

### Action Tracking

```go
// Set current action
ctx = ctx.WithAction("ProcessPayment")

// Get current action
action := ctx.Action()
```

Actions are automatically set from HTTP requests as `"METHOD /path"`.

### Environment

```go
// Get environment
env := ctx.Environment()

// Check if production
if ctx.IsProdEnv() {
    // production-specific logic
}
```

### HTTP Request

```go
// Get original HTTP request
req := ctx.Request()
```

## HTTP Headers

| Header | Purpose |
|--------|---------|
| `Authorization` | JWT token for authentication |
| `Workflow` | Workflow ID for distributed tracing |
| `Time-Now` | Time override (non-production only, RFC3339 format) |

## Best Practices

1. **Always use WithScope at function entry** - Even for small functions, it aids debugging.

2. **Always defer Exit** - Ensures errors are properly wrapped with context.

3. **Pass context as first parameter** - Follows Go conventions and ensures context propagation.

4. **Use key-value pairs in WithScope** - Makes logs searchable and structured.

5. **Don't store contexts** - Contexts are request-scoped; pass them through call chains.

6. **Use Logger() for all logging** - Ensures consistent structured logging with context.
