# Auth Package

A flexible, role-based access control (RBAC) library for HTTP services with support for multi-tenancy and JWT authentication.

## Overview

This package provides a complete authentication and authorization solution with:

- **JWT-based authentication** using HMAC signing
- **Role-based access control (RBAC)** with configurable permissions
- **Multi-tenant support** with tenant and entity isolation
- **Dynamic path matching** with template variables
- **Public endpoint support** for unauthenticated access

## Core Concepts

### Claims

User authentication information stored in JWT tokens:

```go
type Claims struct {
    User      User                 // User identifier
    Entities  RolesPerEntity       // Entities with their associated roles (map[Entity]Roles)
    Tenants   Tenants              // Tenants the user belongs to
    Roles     Roles                // Base roles assigned to the user
    Additions map[string]any       // Additional custom claims
}
```

### Configuration

Access control is defined through a JSON-serializable policy configuration:

```go
type Policy struct {
    Roles       RolePermissions   // Maps roles to permissions
    Permissions PermissionActions // Maps permissions to actions
    Public      Actions           // Public endpoints (no auth required)
}
```

## Usage

### 1. Define Access Control Policy

```go
policy := auth.Policy{
    Roles: auth.RolePermissions{
        "user": auth.Permissions{"read_own_data"},
        "admin": auth.Permissions{"read_all_data", "write_all_data"},
    },
    Permissions: auth.PermissionActions{
        "read_own_data": auth.Actions{
            "GET /tenants/{tenant}/users/{user}/data",
        },
        "read_all_data": auth.Actions{
            "GET /tenants/{tenant}/users/{any}/data",
        },
        "write_all_data": auth.Actions{
            "PUT /tenants/{tenant}/users/{any}/data",
        },
    },
    Public: auth.Actions{
        "GET /health",
        "POST /login",
    },
}
```

### 2. Create Authorization Checker

```go
check, err := auth.NewCheck(policy)
if err != nil {
    log.Fatal(err)
}
```

### 3. Use in HTTP Middleware

```go
func authMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if err := check(r); err != nil {
            if err == auth.ErrMissingAuthorizationHeader {
                http.Error(w, "Unauthorized", http.StatusUnauthorized)
                return
            }
            http.Error(w, "Forbidden", http.StatusForbidden)
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

### 4. Generate and Decode Tokens

```go
// Generate a token
claims := auth.Claims{
    User:    "john.doe",
    Tenants: auth.Tenants{"tenant1"},
    Entities: auth.RolesPerEntity{
        "entity1": auth.Roles{"entity_admin"},  // entity1 with extra roles
        "entity2": auth.Roles{},                // entity2 with no extra roles
    },
    Roles: auth.Roles{"user"},
}

token, err := auth.GenerateToken(claims)
if err != nil {
    log.Fatal(err)
}

// Add token to HTTP request
request.Header.Set(auth.HttpHeaderAuthorization, "Bearer " + token)

// Or use helper function
err = auth.EncodeHTTPRequestClaims(request, claims)

// Decode token from request
claims, err := auth.DecodeHTTPRequestClaims(request)
```

## Path Templates

Actions support dynamic path matching with the following templates:

- `{user}` - Matches the authenticated user's identifier
- `{tenant}` - Matches any tenant the user belongs to
- `{entity}` - Matches any entity the user has access to
- `{any}` - Matches any single path segment
- `{any...}` - Matches any remaining path segments (wildcard suffix)

The request path is split into segments before it is unescaped (`PathSegments`), so an encoded slash (`%2F`) stays inside its segment and never acts as a separator. The convention API router splits paths with the same function, so a policy always evaluates the route that runs: `GET /assets/secret%2F@public` is one `{any}` segment under `/assets/`, never the action `GET /assets/{any}/@public`.

A policy segment (fixed, `{any}`, `{user}`, `{tenant}`, `{entity}`) matches only one plain segment: a segment that decodes to something containing `/` or `\`, or to `.` or `..`, matches nothing. An open-route handler reads the decoded path, so without this `GET /files/private%2Fdoc/hero.webp` would pass a public `GET /files/{any}/hero.webp` grant and serve a file one folder deeper. The remainder after an `{any...}` is not compared and is not checked.

### Examples

```go
"GET /tenants/{tenant}/users/{user}/data"
// Matches: /tenants/tenant1/users/john.doe/data
// Only if user is "john.doe" and belongs to "tenant1"

"GET /tenants/{tenant}/users/{any}/data"
// Matches: /tenants/tenant1/users/*/data
// For any user in "tenant1"

"GET /public/{any...}"
// Matches: /public/anything/else/here
```

## Action Specificity

When multiple actions could match a request, the system evaluates them in order of specificity (most specific first). This ensures that exact matches take precedence over wildcard patterns.

**Specificity order** (most to least specific):

1. **Fixed segments** (`users`, `data`, `specific`) - Exact string match
2. **`{user}`** - Matches only the authenticated user
3. **`{tenant}`** - Matches any tenant the user belongs to
4. **`{entity}`** - Matches any entity the user has access to
5. **`{any}`** - Matches any single segment
6. **`{any...}`** - Matches any remaining segments (wildcard suffix)

### Why This Matters

Consider a configuration with overlapping paths:

```go
Permissions: auth.PermissionActions{
    "view_specific": auth.Actions{"GET /reports/summary"},
    "view_any":      auth.Actions{"GET /reports/{any}"},
}
```

A request to `/reports/summary` will match the more specific `view_specific` permission first. This allows you to grant broad access with wildcards while restricting specific paths to different roles.

## Entity-Specific Roles

Entities can have associated roles that augment the user's base roles when accessing that entity:

```go
claims := auth.Claims{
    User:    "john.doe",
    Tenants: auth.Tenants{"tenant1"},
    Roles:   auth.Roles{"user"},           // Base roles
    Entities: auth.RolesPerEntity{
        "entity1": auth.Roles{"entity_admin"}, // Additional roles for entity1
        "entity2": auth.Roles{},               // No additional roles for entity2
    },
}
```

When accessing `/entities/entity1/...`:
- Effective roles = `["user", "entity_admin"]` (merged)

When accessing `/entities/entity2/...`:
- Effective roles = `["user"]` (base roles only)

Entity-specific roles are **additive** - they combine with (never replace) the user's base roles. Entity roles only apply when the action path contains `{entity}` and the entity is matched.

## Configuration

The package requires the `communication_secret` configuration value for JWT signing. This should be set using the `github.com/sofmon/convention/lib/cfg` package:

```bash
# .secret file or environment
communication_secret=your-secret-key-here
```

## Error Handling

The package defines specific errors for different failure scenarios:

- `ErrMissingRequest` - HTTP request is nil
- `ErrMissingAuthorizationHeader` - No valid Bearer token in Authorization header
- `ErrInvalidAuthorizationToken` - Token is invalid or cannot be verified
- `ErrForbidden` - User authenticated but lacks required permissions

## Security Considerations

- Store `communication_secret` securely and never commit to version control
- Use strong, random secrets for JWT signing
- Tokens don't expire by default - implement expiration in your application if needed
- Always use HTTPS in production to protect tokens in transit
- Public endpoints bypass authentication - use sparingly

## Example Configuration

See [eval_test.go](eval_test.go) for a complete example showing:
- User-specific access control
- Tenant-wide permissions
- Cross-tenant admin access
- Public endpoint configuration
- Wildcard path matching
