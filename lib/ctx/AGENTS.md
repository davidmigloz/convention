# AGENTS.md - AI Agent Guidelines for ctx Package

## Package Purpose

The `ctx` package provides a structured context wrapper around Go's `context.Context` for consistent logging, error handling, scope tracking, and request-scoped data propagation.

> IMPORTANT: AI agents must treat AGENTS.md and README.md as authoritative living documents. Any change to the implementation that affects behaviors must be mirrored in both files. The code and documentation must never drift apart. When the implementation changes, these documents must be updated immediately so they always reflect the current system.

## Critical Patterns

### Function Signature Pattern

Every significant function MUST follow this pattern:

```go
func functionName(ctx ctx.Context, param1 Type1, param2 Type2) (result ResultType, err error) {
    ctx = ctx.WithScope("functionName", "param1", param1, "param2", param2)
    defer ctx.Exit(&err)

    // function body

    return
}
```

**Requirements:**
- `ctx ctx.Context` is ALWAYS the first parameter
- Named error return `err error` to work with `defer ctx.Exit(&err)`
- `WithScope` called immediately with function name and key parameters
- `defer ctx.Exit(&err)` called immediately after WithScope

### Functions Without Error Returns

For functions that don't return errors but still benefit from scope tracking:

```go
func processItem(ctx ctx.Context, item Item) {
    ctx = ctx.WithScope("processItem", "itemID", item.ID)
    defer ctx.Exit(nil)

    // function body
}
```

### When NOT to Use This Pattern

- Simple getters/setters (one-liners)
- Pure utility functions with no side effects

## File Structure

| File | Purpose |
|------|---------|
| `ctx.go` | Core Context struct, New(), WrapContext(), Agent() |
| `scope.go` | WithScope(), WithScopef(), Scope(), Exit(), wrapErr() |
| `workflow.go` | Workflow type, WithWorkflow(), WithNewWorkflow() |
| `log.go` | Logger(), LoggerKeys(), WithLogger(), defaultLogger() |
| `log_scan.go` | allocation-free scanner: is a line one JSON object with no repeated key? |
| `log_dedupe.go` | dedupeWriter: io.Writer that renames repeated JSON keys in each log line |
| `env.go` | Environment type, Environment(), IsProdEnv() |
| `http.go` | HTTP integration, WithRequest(), Request(), WithRequestStart(), RequestElapsed(), headers |
| `claims.go` | WithClaims(), Claims(), User() |
| `action.go` | WithAction(), Action() |
| `now.go` | WithNow(), Now() |
| `sys.go` | WithAgentClaims(), mustUseAgentClaims() |

## Key Types

```go
type Context struct {
    context.Context
}

type Agent string
type Workflow string
type Environment string
```

## Context Keys (internal)

- `contextKeyAgent` - Agent name
- `contextKeyAgentClaims` - Original agent claims
- `contextKeyUseAgentClaims` - Flag to use agent claims
- `contextKeyEnv` - Environment
- `contextKeyRequest` - HTTP request
- `contextKeyClaims` - Current user claims
- `contextKeyAction` - Current action
- `contextKeyWorkflow` - Workflow ID
- `contextKeyScope` - Scope chain string
- `contextKeyNow` - Time override
- `contextKeyLogger` - slog.Logger instance

## Logger Keys (for structured logs)

- `env`, `agent`, `workflow`, `user`, `scope`, `action`, `now`, `use_agent_claims`
- Exported as `LoggerKeys() []string` (a fresh slice, in this order). `TestLoggerKeys_MatchLoggerOutput` compares it with a real `Logger()` line, so a new logger key needs its context value set in that test too.
- `defaultLogger()` also writes the slog built-ins `time`, `level`, `msg` and `source`.

## Log Key Dedupe Writer

`defaultLogger()` is `newDefaultLogger(os.Stdout)`: a `slog.JSONHandler` whose destination is wrapped by `dedupeWriter` (`log_dedupe.go`). slog does not deduplicate keys, and a repeated key in one JSON line makes fluent-bit drop the line or corrupt the value.

- `slog.JSONHandler` writes each record with exactly one `Write` call (one JSON object plus `\n`, under a mutex shared by all derived handlers), so the writer sees whole lines.
- Guarantee: no JSON object at any depth of a line written by the default logger repeats a key. This holds for whatever the handler produced (LogValuers, empty keys, invalid UTF-8 keys, `json.RawMessage` values), because the writer checks the encoded output, not the attributes.
- Fast path: `scanner.scan(p, false)` (`log_scan.go`) scans each line without allocating and compares raw key bytes. A clean line is written as is. The scan is linear in line length, so a very long line (such as one holding a decoded response body) costs proportionally more, and one object over 64 keys sends the whole line to the allocating slow path.
- Slow path: for a line with a repeat, or one the fast scan cannot vouch for (keys with a backslash escape or invalid UTF-8, objects over 64 keys, nesting over 512), the scanner runs again in slow mode. It uses an explicit stack, so nesting depth has no limit, and it compares keys by their decoded value as `encoding/json` does (invalid UTF-8 and lone surrogates decode to U+FFFD).
- Rename rule: the first occurrence keeps its key. A later repeat in the same object becomes `attr_<key>` if that is free in the object, otherwise `attr<n>_<key>`, where `n` comes from one counter per object that starts at 2 and only grows. The prefix is inserted right after the opening quote of the key token, so the key's own escapes and bytes are kept. Key order is kept. The counter keeps renaming linear in the number of keys.
- Values are never touched: nothing is decoded or re-encoded except key tokens for comparison. The output equals the input byte for byte except for the inserted prefixes.
- Input that is not exactly one JSON object is passed through unchanged. Destination errors are returned; after a partial write of a spliced line, `Write` returns the number of input bytes that the written output holds (a partly written inserted prefix counts for nothing).
- The writer is stateless and does not retain the buffer it is given, so it is safe for concurrent use.
- Scanners and line buffers are pooled. `putScanner` clears the key maps of every frame and refuses a scanner whose stack exceeds 1024 frames or whose key or rename storage exceeds 4096 entries; line buffers over 64 KB are dropped too, so one huge line does not pin memory.
- Loggers passed to `WithLogger()` are used as given and are NOT protected.

## Code Generation Rules

### Creating New Context Methods

When adding a new context value:

1. Add context key constant to `ctx.go`:
   ```go
   contextKeyNewThing contextKey = iota
   ```

2. Add logger key if needed (in `ctx.go`):
   ```go
   loggerKeyNewThing = "new_thing"
   ```

3. Create getter and setter in a new or appropriate file:
   ```go
   func (ctx Context) WithNewThing(value NewType) Context {
       return Context{
           context.WithValue(ctx.Context, contextKeyNewThing, value),
       }
   }

   func (ctx Context) NewThing() NewType {
       obj := ctx.Value(contextKeyNewThing)
       if obj == nil {
           return defaultValue
       }
       return obj.(NewType)
   }
   ```

4. If you want the value to appear in log output, add it to `Logger()` in `log.go`:
   ```go
   if newThing, ok := ctx.Value(contextKeyNewThing).(NewType); ok {
       attrs = append(attrs, loggerKeyNewThing, newThing)
   }
   ```

   Add `loggerKeyNewThing` to `LoggerKeys()` too, and set the value in `TestLoggerKeys_MatchLoggerOutput`.

**Note:** The logger is built lazily from context values when `Logger()` is called. Do NOT call `WithLogger()` in `With*` methods - this prevents duplicate keys in log output.

### Error Handling

- Errors are wrapped with scope prefix: `✘ scope → subscope: error message`
- Use `Exit(&err, exceptedErrors...)` to exclude specific errors from wrapping
- Never wrap already-wrapped errors (checked by prefix)

### Scope String Format

- Scopes chain with ` → ` separator
- Arguments formatted as `{key=value key2=value2}`
- Example: `agent → processOrder → validateItems {itemID=123}`

## Dependencies

- `github.com/sofmon/convention/lib/auth` - Claims, User, Action types
- `github.com/sofmon/convention/lib/cfg` - Configuration for environment
- `github.com/google/uuid` - Workflow ID generation
- `log/slog` - Structured logging

## Testing Considerations

- Use `WithNow()` to inject fixed time for deterministic tests
- `WithRequestStart()` captures the context's clock together with the arrival time: stamp after `WithNow(fixed)` to freeze `RequestElapsed()` at 0; a `WithNow()` applied after the stamp, or a `Time-Now` header, does not change it
- `RequestElapsed()` is `(0, false)` on a context that was never stamped; `WithRequest` never re-stamps, so attaching a request again keeps the first arrival; unless the stamping clock is fixed it reads `time.Now` (not `Now()`, whose `UTC()` strips the monotonic reading), so a wall-clock step cannot change it
- A context built without configuration is production, which ignores the `Time-Now` header; a test of the header sets `contextKeyEnv` to a non-production value in-package
- Workflow IDs are UUIDs; mock if needed for reproducible tests
- Environment defaults to "production" if config unavailable

## Common Mistakes to Avoid

1. **Forgetting `defer ctx.Exit(&err)`** - Errors won't be wrapped with scope
2. **Not passing ctx to called functions** - Breaks scope chain
3. **Storing context in structs** - Contexts are request-scoped
4. **Using unnamed error return** - `Exit()` requires pointer to named error
5. **Calling WithScope without function name** - Scope becomes unclear
6. **Not including key parameters in WithScope** - Loses debugging context
