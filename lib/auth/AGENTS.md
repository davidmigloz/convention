# Auth Package Implementation Details

This document provides implementation details for AI agents working on the auth package.

> IMPORTANT: AI agents must treat AGENTS.md and README.md as authoritative living documents. Any change to the implementation that affects behaviors must be mirrored in both files. The code and documentation must never drift apart. When the implementation changes, these documents must be updated immediately so they always reflect the current system.

## Architecture Overview

The auth package implements a two-phase authorization system:

1. **Configuration Expansion** ([eval.go:23-55](eval.go)) - Converts high-level `Config` into efficient runtime structures
2. **Request Matching** ([eval.go:137-235](eval.go)) - Evaluates HTTP requests against expanded configuration

## File Structure

- **[auth.go](auth.go)** - Core type definitions (User, Role, Permission, Action, Config)
- **[claim.go](claim.go)** - JWT claims structure and constants
- **[http.go](http.go)** - JWT encoding/decoding and HTTP integration
- **[eval.go](eval.go)** - Authorization evaluation engine
- **[path.go](path.go)** - `PathSegments`, the request path splitter shared with the `lib/api` router
- **[eval_test.go](eval_test.go)** - Comprehensive test cases
- **[path_test.go](path_test.go)** - `PathSegments` cases; an encoded `/` cannot match a longer public action, and a policy segment matches one plain segment only
- **[main_test.go](main_test.go)** - Test setup (currently minimal)

## Core Types

### Basic Types (auth.go:8-43)

All basic types are type aliases over `string` or `[]string`:

```go
type User string
type Users []User
type Entity string
type Entities []Entity
type RolesPerEntity map[Entity]Roles  // Entity -> associated roles
type Tenant string
type Tenants []Tenant
type Role string
type Roles []Role
type Permission string
type Permissions []Permission
type Action string
type Actions []Action
```

### Configuration Types (auth.go:45-53)

```go
type RolePermissions map[Role]Permissions      // Role -> list of permissions
type PermissionActions map[Permission]Actions  // Permission -> list of actions
type Config struct {
    Roles       RolePermissions
    Permissions PermissionActions
    Public      Actions  // Unauthenticated access
}
```

### Claims (claim.go:3-16)

```go
type Claims struct {
    User      User
    Entities  RolesPerEntity    // map[Entity]Roles - entities with their roles
    Tenants   Tenants
    Roles     Roles             // Base user roles
    Additions map[string]any    // Custom claims
}
```

JWT claim keys are defined as package constants to avoid typos.

## Authorization Flow

### 1. Configuration Expansion (eval.go:29-64)

`expandConfig()` transforms the declarative `Policy` into optimized runtime structures:

```go
func expandConfig(policy Policy) (actions allowedActionSources, publicActions allowedActions, err error)
```

**Process:**
- Iterates through each role's permissions
- Looks up each permission's actions
- Converts each action string into `actionSource` struct (containing action + role)
- Builds two indexes: `allowedActionSources` (authenticated) and `publicActions` (unauthenticated)
- **Sorts actions by specificity** (most specific first) to ensure correct matching order

**Key transformation:** `Action` string → `actionSource` struct via `generateAllowedAction()` (eval.go:120-154)

### 1a. Action Specificity Sorting

Actions are sorted from most specific to least specific before evaluation. This ensures that when multiple actions could match a request, the most specific one is evaluated first.

**Specificity order** (most to least specific):
1. Fixed segments (`path`, `to`, `something`) - score 0
2. `{user}` - matches only authenticated user - score 1
3. `{tenant}` - matches any user tenant - score 2
4. `{entity}` - matches any user entity - score 3
5. `{any}` - matches any single segment - score 4
6. `{any...}` (openEnd) - matches any remaining segments - score +1000 penalty

**Example**: `/users/john/data` (score: -3) is evaluated before `/users/{any}/data` (score: 1)

### 2. Action Parsing (eval.go:101-135)

`generateAllowedAction()` parses action strings like `"GET /tenants/{tenant}/users/{user}/data"`:

```go
func generateAllowedAction(a Action) (res allowedAction, err error)
```

**Steps:**
1. Split into method and path using `Action.MethodPath()` (auth.go:30-41)
2. Split path into segments by `/`
3. Detect open-end wildcard (`{any...}` suffix)
4. Convert each segment into typed `allowedSegment`:
   - `{any}` → `allowedSegmentAny{}`
   - `{user}` → `allowedSegmentUser{}`
   - `{tenant}` → `allowedSegmentTenant{}`
   - `{entity}` → `allowedSegmentEntity{}`
   - Otherwise → `allowedSegmentFixed(segment)`

### 3. Runtime Structures (eval.go:13-27)

```go
type allowedActions []allowedAction
type allowedAction struct {
    method  string
    path    allowedPath
    openEnd bool  // true if path ends with {any...}
}

// actionSource tracks which role an action came from
type actionSource struct {
    action allowedAction
    role   Role
}
type allowedActionSources []actionSource
type allowedPath []allowedSegment
```

**Segment matchers:** Each implements the `allowedSegment` interface:

```go
type allowedSegment interface {
    Match(segment string, claims Claims, target *Target) bool
}
```

Implementations:
- `allowedSegmentFixed` - Exact string match
- `allowedSegmentAny` - Always matches
- `allowedSegmentUser` - Matches `claims.User`
- `allowedSegmentTenant` - Matches any tenant in `claims.Tenants`
- `allowedSegmentEntity` - Matches any entity key in `claims.Entities` map

### 4. Request Evaluation (eval.go:74-118)

`NewCheck()` returns a closure that evaluates HTTP requests:

```go
check := func(r *http.Request) (Target, error) {
    segments, ok := PathSegments(r.URL) // same split as the lib/api router
    if !ok {
        return target, ErrForbidden
    }

    // 1. Check public endpoints (no auth required)
    if publicEndpoints.match(r.Method, segments, Claims{}, &target) {
        return target, nil
    }

    // 2. Decode JWT claims from Authorization header
    claims, err := DecodeHTTPRequestClaims(r)
    if err != nil {
        return Target{}, err
    }

    // 3. Check actions with role validation
    if actionSources.match(r.Method, segments, claims, &target) {
        return target, nil
    }

    return target, ErrForbidden
}
```

### 5. Matching Algorithm with Entity-Specific Roles (eval.go:156-197)

**Path splitting.** `PathSegments(u *url.URL)` splits the wire form of the path (`u.RawPath` when it decodes to `u.Path`, otherwise `u.EscapedPath()`) on `/` and then `url.PathUnescape`s each segment. An encoded `/` therefore stays inside one segment. The `lib/api` router (`descriptor.match`) calls the same function, so the check and the router always see the same segments. Splitting the decoded `r.URL.Path` here while the router splits the escaped path would let `/assets/secret%2F@public` pass as a public `.../{any}/@public` action and then run the protected `.../{any}` handler. Keep the two on one function.

**Plain segments only.** `allowedPath.match` refuses a segment that fails `isPlainSegment` (contains `/` or `\`, or is `.` or `..`) before any `allowedSegment.Match`. With the shared split, a policy `{any}` would otherwise match `private%2Fdoc`, and an open `{any...}` route does not run the router's param check while its handler reads the decoded `r.URL.Path` (`lib/storage/handler.go`). So a narrow grant such as `GET /files/{any}/hero.webp` would cover `/files/private/doc/hero.webp`. Only compared segments are checked; the remainder of an open-ended grant is not, and control bytes are left to the handler.

Two-pass evaluation with role validation:

```go
func (sources allowedActionSources) match(...) bool {
    for _, src := range sources {
        // 1. Try to match action against request
        if !src.action.match(method, segments, claims, &tempTarget) {
            continue
        }

        // 2. Validate role is allowed for the matched entity context
        if isRoleAllowed(src.role, tempTarget.Entity, claims) {
            return true
        }
    }
    return false
}

func isRoleAllowed(role Role, matchedEntity Entity, claims Claims) bool {
    // Check base roles first
    for _, r := range claims.Roles {
        if r == role { return true }
    }

    // If entity matched, check entity-specific roles
    if matchedEntity != "" {
        if entityRoles, ok := claims.Entities[matchedEntity]; ok {
            for _, r := range entityRoles {
                if r == role { return true }
            }
        }
    }
    return false
}
```

**Key behaviors:**
- Method must match exactly
- For `openEnd=true`: path can be longer than template
- For `openEnd=false`: path must match template length exactly
- Entity-specific roles only apply when `{entity}` is matched in the action path
- Base roles (`claims.Roles`) are always checked first
- Entity roles (`claims.Entities[entity]`) are checked only when entity is matched

## JWT Integration

### Token Generation (http.go:70-95)

```go
func GenerateToken(claims Claims) (string, error)
```

**Process:**
1. Load HMAC secret from config (cached after first load)
2. Create `jwt.MapClaims` with standard claims (user, entities, tenants, roles)
3. Add any custom claims from `Claims.Additions`
4. Standard claims overwrite additions if keys conflict
5. Sign with HS256 algorithm

### Token Decoding (http.go:97-153)

```go
func DecodeToken(tokenString string) (Claims, error)
```

**Process:**
1. Load HMAC secret from config
2. Parse and validate token (checks signature and format)
3. Extract standard claims with type assertions
4. Convert `[]any` arrays back to typed slices

**Note:** Type assertions use `.(string)` which will panic on type mismatch. Consider error handling improvements.

### HTTP Request Integration (http.go:41-68)

```go
func DecodeHTTPRequestClaims(r *http.Request) (Claims, error)
func EncodeHTTPRequestClaims(r *http.Request, claims Claims) error
```

- Expects `Authorization: Bearer <token>` header format
- Uses constant `HttpHeaderAuthorization = "Authorization"`
- Split on space, validate "Bearer" prefix

## Configuration Management

### HMAC Secret (http.go:26-39)

```go
func getHmacSecret() ([]byte, error)
```

- Lazy loads from config on first use
- Cached in package variable `hmacSecret`
- Loaded via `github.com/sofmon/convention/lib/cfg.Bytes("communication_secret")`
- Not thread-safe (assumes single-threaded initialization)

## Testing Strategy

### Test Data Structure (eval_test.go:67-195)

Comprehensive test cases in `testData` slice:

```go
var testData = []struct {
    name     string
    cfg      convAuth.Config
    user     convAuth.User
    tenants  convAuth.Tenants
    roles    convAuth.Roles
    entities convAuth.Entities
    pass     []*http.Request  // Should succeed
    block    []*http.Request  // Should fail
}
```

### Test Coverage (eval_test.go:197-238)

Single test function `TestCheck()` iterates all test cases:
1. Create checker from config
2. Build claims from test data
3. Verify all `pass` requests succeed
4. Verify all `block` requests fail

**Test scenarios:**
- User-specific access (`{user}` template)
- Tenant-scoped access (`{tenant}` template)
- Cross-tenant admin access (`{any}` template)
- Wildcard suffixes (`{any...}`)
- Public endpoints (empty claims)

## Implementation Patterns

### Type Safety

- All domain types (User, Role, etc.) are distinct types despite being strings
- Prevents accidental mixing (e.g., passing Role where User expected)
- Enables future behavior addition without breaking changes

### Interface-Based Matching

- `allowedSegment` interface enables polymorphic matching
- Each matcher type has single responsibility
- Easily extensible for new segment types

### Functional Options

- `NewCheck()` returns configured function, not struct
- Captures configuration in closure
- Immutable after creation

### Error Handling

- Distinct error types for different failure modes
- Enables appropriate HTTP status code selection
- Errors returned, not logged (caller decides logging)

## Potential Improvements

### Security
- Add JWT expiration support (exp claim)
- Add token refresh mechanism
- Consider blacklist/revocation support
- Add rate limiting per user/tenant

### Performance
- Thread-safe secret initialization (sync.Once)
- Cache expanded configurations
- Optimize string splitting (reuse buffers)

### Robustness
- Better type assertion error handling in DecodeToken
- Validate configuration on load (detect cycles, missing permissions)
- Add structured logging hooks

### Features
- Support for custom segment matchers
- Regex-based path matching
- Conditional permissions (time-based, IP-based)
- Permission inheritance/hierarchies

## Common Modification Scenarios

### Adding New Segment Type

1. Define new type implementing `allowedSegment` (eval.go)
2. Add case in `generateAllowedAction()` switch statement
3. Add validation logic in `Match()` method
4. Add test cases in eval_test.go

### Adding Custom Claims

1. Add fields to `Claims` struct (claim.go)
2. Add claim key constant (claim.go)
3. Update `GenerateToken()` to serialize (http.go)
4. Update `DecodeToken()` to deserialize (http.go)
5. Use in segment matchers if needed (eval.go)

### Changing JWT Algorithm

1. Update `GenerateToken()` signing method (http.go:89)
2. Update `DecodeToken()` validation (http.go:106)
3. Ensure secret format matches algorithm requirements

### Adding Middleware Features

1. Extend `Check` function signature or create new wrapper
2. Keep existing `Check` function for compatibility
3. Consider context.Context for request-scoped data
