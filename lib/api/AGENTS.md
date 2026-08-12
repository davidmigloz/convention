# API Package - Agent Reference

This document provides implementation details for AI agents working on the `lib/util/api` package.

> IMPORTANT: AI agents must treat AGENTS.md and README.md as authoritative living documents. Any change to the implementation that affects behaviors must be mirrored in both files. The code and documentation must never drift apart. When the implementation changes, these documents must be updated immediately so they always reflect the current system.

## Import Convention

This package follows the `conv` prefix naming convention for imports:

```go
import (
    convAPI "github.com/sofmon/convention/lib/api"
    convAuth "github.com/sofmon/convention/lib/auth"
    convCtx "github.com/sofmon/convention/lib/ctx"
    convCfg "github.com/sofmon/convention/lib/cfg"
)
```

All code examples in this document and in README.md must use these import aliases.

## Package Overview

A type-safe HTTP API framework using Go generics. The same API struct definition is used for:
- Server-side routing and handler execution
- Client-side RPC calls
- OpenAPI 3.0 schema generation

## File Structure

| File | Purpose |
|------|---------|
| `server.go` | Server creation, HTTP handler, endpoint discovery |
| `client.go` | Client creation via reflection |
| `descriptor.go` | URL parsing, path matching, request building, type introspection |
| `endpoint.go` | Common endpoint interface |
| `in.go`, `out.go`, `inout.go`, `trigger.go`, `raw.go` | Base handler types |
| `in_p1.go` - `in_p5.go` | Input handlers with 1-5 path parameters |
| `out_p1.go` - `out_p5.go` | Output handlers with 1-5 path parameters |
| `inout_p1.go` - `inout_p5.go` | Input/output handlers with 1-5 path parameters |
| `trigger_p1.go` - `trigger_p5.go` | Trigger handlers with 1-5 path parameters |
| `raw_p1.go` - `raw_p5.go` | Raw handlers with 1-5 path parameters |
| `openapi.go` | OpenAPI schema generation |
| `error.go` | Error types and utilities |
| `check.go` | Pre/post check function type |
| `http.go` | HTTP utilities (JSON helpers, context headers) |
| `values.go` | URL parameter value storage |

## Core Interfaces

### endpoint (internal)

All handler types implement this interface:

```go
type endpoint interface {
    execIfMatch(ctx convCtx.Context, w http.ResponseWriter, r *http.Request) bool
    setDescriptor(desc descriptor)
    getDescriptor() descriptor
    getInOutTypes() (in, out reflect.Type)
    setEndpoints(eps endpoints)
}
```

## Key Types

### descriptor

Internal type storing endpoint metadata:

```go
type descriptor struct {
    host     string
    port     int
    method   string          // HTTP method
    segments []urlSegment    // Path segments (static or parameterized)
    query    []queryParam    // Query parameters for OpenAPI
    weight   int             // Number of static segments (routing priority)
    open     bool            // True if path ends with {any...}
    in, out  *object         // Input/output type schemas
}
```

### object

Schema representation for types:

```go
type object struct {
    ID        string             // Package path + name
    Name      string             // Friendly name (snake_case)
    Type      objectType         // string, integer, number, boolean, array, map, object, time, enum
    Mandatory bool               // Required field
    Elem      *object            // For arrays/maps: element type
    Key       *object            // For maps: key type
    Fields    map[string]*object // For structs: field schemas
}
```

### Error

API error with full context:

```go
type Error struct {
    URL      string    `json:"url,omitempty"`
    Method   string    `json:"method,omitempty"`
    Status   int       `json:"status,omitempty"`
    Code     ErrorCode `json:"code,omitempty"`
    Scope    string    `json:"scope,omitempty"`
    Workflow string    `json:"workflow,omitempty"`
    Message  string    `json:"message,omitempty"`
    Inner    *Error    `json:"inner,omitempty"`

    detail error // internal cause; never serialised
}
```

#### Client visible projection

`serveError` never writes the struct above to the response. It writes
`publicView()`, which carries **only** `url`, `method`, `status`, `code`,
`message` and `workflow`. `Scope`, `Inner` and `detail` are dropped, because they
carry the internal call graph, scope arguments (frequently PII) and driver level
text such as Postgres table and column names.

The full chain is written to the log instead, by `serveError`, at `Error` level
for 5xx and `Warn` level for 4xx. **This log record is the only remaining copy of
the diagnostic** — do not remove or downgrade it.

`Workflow` is retained on the wire on purpose: it is the correlation key between
a sanitised client response and the complete server side log record.

#### Passing causes

`NewError`/`ServeError` accept any `inner error`:

- a `*Error` or `Error` becomes the structured `Inner` (kept server-side)
- anything else is retained in the unexported `detail` field

`Message` is never mutated by the inner error. `Error()` renders the whole chain
for logging; `errors.Is`/`errors.As` traverse it via `Unwrap`, so services can
still branch on sentinel causes such as `convDB.ErrObjectNotFound`.

Because the library sanitises unconditionally, a handler that bare-returns a
database error (`if err != nil { return }`) is no longer a disclosure risk.

## Handler Pattern

All handlers follow this pattern (example: `InP2`):

```go
type InP2[inT any, p1T, p2T ~string] struct {
    descriptor descriptor
    fn         func(ctx convCtx.Context, p1 p1T, p2 p2T, in inT) error
}

func NewInP2[inT any, p1T, p2T ~string](fn func(ctx convCtx.Context, p1 p1T, p2 p2T, in inT) error) InP2[inT, p1T, p2T] {
    return InP2[inT, p1T, p2T]{fn: fn}
}

// Server-side execution
func (x *InP2[inT, p1T, p2T]) execIfMatch(ctx convCtx.Context, w http.ResponseWriter, r *http.Request) bool {
    vals, match := x.descriptor.match(r)
    if !match {
        return false
    }

    var in inT
    json.NewDecoder(r.Body).Decode(&in)

    err := x.fn(ctx.WithRequest(r), p1T(vals.GetByIndex(0)), p2T(vals.GetByIndex(1)), in)
    // Handle response...
    return true
}

// Client-side RPC
func (x *InP2[inT, p1T, p2T]) Call(ctx convCtx.Context, p1 p1T, p2 p2T, in inT) error {
    vals := values{}
    vals.Add("", string(p1))
    vals.Add("", string(p2))

    body, _ := json.Marshal(in)
    req, _ := x.descriptor.newRequest(vals, bytes.NewReader(body))
    // Execute request...
}
```

## Endpoint Discovery

Both server and client use reflection to discover endpoints:

```go
for _, f := range reflect.VisibleFields(reflect.TypeOf(api).Elem()) {
    ep, ok := reflect.ValueOf(api).Elem().FieldByName(f.Name).Addr().Interface().(endpoint)
    if !ok {
        continue
    }

    apiTag := f.Tag.Get("api")
    in, out := ep.getInOutTypes()
    desc := newDescriptor(host, port, apiTag, in, out)
    ep.setDescriptor(desc)
}
```

## Routing

1. Endpoints sorted by weight (static segment count) - most specific first
2. Each request checked against endpoints in order
3. First matching endpoint handles the request
4. Returns 404 if no match

Weight calculation: count of non-parameter segments
- `/users/{id}/posts` has weight 2 (`users`, `posts`)
- `/users/{id}` has weight 1 (`users`)

## API Tag Parsing

Format: `METHOD /path/segments/{param}?query=type|description`

```go
// In newDescriptor():

// 1. Extract method (defaults to GET)
methodSplit := strings.Split(pattern, " ")
if len(methodSplit) > 1 && strings.HasPrefix(methodSplit[1], "/") {
    desc.method = methodSplit[0]
}

// 2. Extract query params
querySplit := strings.Split(pattern, "?")
if len(querySplit) > 1 {
    values, _ := url.ParseQuery(querySplit[1])
    // Parse name=type|description
}

// 3. Parse path segments
for _, s := range strings.Split(path, "/") {
    if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
        // Parameter segment
    } else {
        // Static segment, increment weight
    }
}
```

## Type Introspection

`objectFromType()` recursively builds schema from Go types:

- Handles pointers (marks as optional)
- Handles structs (recursively processes fields)
- Handles anonymous/embedded structs (flattens fields)
- Handles slices, arrays, maps
- Respects `json` tags for field names
- Respects `omitempty` for optional fields
- Tracks known objects to prevent infinite recursion

## OpenAPI Generation

1. Collects all endpoint schemas via `populateSchemas()`
2. Applies type substitutions (custom marshal types)
3. Applies enum definitions
4. Generates YAML with:
   - Component schemas (sorted alphabetically)
   - Path definitions (grouped by path, sorted)
   - Parameters (path and query)
   - Request/response bodies

## Common Modification Patterns

### Adding a New Handler Variant

1. Create new file (e.g., `in_p6.go`)
2. Follow existing pattern from `in_p5.go`
3. Add parameter type to generic constraints
4. Update `execIfMatch` to extract additional parameter
5. Update `Call` to accept and include additional parameter
6. Implement `getInOutTypes` returning input/output types

### Adding New Error Code

In `error.go`:
```go
const (
    ErrorCodeNewCode ErrorCode = "new_code"
)
```

### Adding OpenAPI Features

In `openapi.go`:
- Add new builder method: `func (o OpenAPI) WithFeature(...) OpenAPI`
- Store in OpenAPI struct fields
- Apply in YAML generation section

### Modifying Request/Response Handling

- JSON encoding/decoding in handler `execIfMatch` and `Call` methods
- Context headers in `http.go` `setContextHttpHeaders()`
- Error responses via `serveError()` and `parseRemoteError()`

## Dependencies

| Package | Import Alias | Purpose |
|---------|--------------|---------|
| `github.com/sofmon/convention/lib/ctx` | `convCtx` | Context with request, scope, claims |
| `github.com/sofmon/convention/lib/auth` | `convAuth` | Authentication policy and check |
| `github.com/sofmon/convention/lib/cfg` | `convCfg` | Configuration (TLS certificates) |

## Testing

Test files:
- `api_test.go` - Integration tests
- `openapi_test.go` - OpenAPI generation tests
- `descriptor_test.go` - URL parsing/matching tests
- `error_test.go` - Error handling tests
- `error_leak_test.go` - Regression tests asserting no internal detail is serialised
- `error_remote_internal_test.go` - Service-to-service relay sanitisation (internal package)
- `main_test.go` - Test setup

`TestMain` calls `convCfg.SetConfigLocation("../../.secret")` and **panics if that
folder is missing**. `.secret` is gitignored, so a fresh clone has to create it.
`mkdir .secret` is enough for the error tests; the server and OpenAPI tests
additionally need `communication_secret` plus a system-trusted
`communication_certificate`/`communication_key` pair.

## Implementation Notes

1. **Path parameters must be `~string`**: Constraint ensures type safety while allowing custom string types
2. **Handlers are value receivers for builders, pointer receivers for execution**: `WithPreCheck` returns new instance, `execIfMatch` modifies internal state
3. **OpenAPI caches YAML**: First request generates, subsequent requests return cached
4. **Errors preserve chain server-side only**: `Inner` and `detail` maintain the error hierarchy in-process and in logs; the client only ever receives the outer `code`/`message`/`status` plus `workflow` (see [Error](#error))
5. **Context propagated via headers**: Workflow ID, agent, claims sent in HTTP headers
6. **TLS required for server**: Uses certificates from config paths `communication_certificate` and `communication_key`
