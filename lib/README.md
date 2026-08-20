# Convention Library

Reference implementation of the [Convention](../README.md) specification supporting Go and Dart.

## Overview

This library provides ready-to-use packages for building Convention-compliant agents and client applications:

- **Go**: Server-side packages for building agents with type-safe APIs, multi-tenant databases, and JWT authentication
- **Dart/Flutter**: Client-side packages for building mobile and web applications with state synchronization and dynamic forms

## Quick Start

Building a Convention-compliant agent typically involves:

1. **Configuration**: Use `cfg` to read configuration from `/etc/agent/` (certificates, secrets, database settings)
2. **Context**: Use `ctx` to create structured contexts with logging, error handling, and workflow tracking
3. **Authentication**: Use `auth` to validate JWT tokens and enforce role-based access control
4. **API Definition**: Use `api` to define type-safe HTTP endpoints as Go structs with automatic routing and OpenAPI generation
5. **Database**: Use `db` for multi-tenant, sharded database operations with automatic history tracking

See individual package documentation for detailed examples and usage patterns.

## Installation

### Go

```bash
go get github.com/sofmon/convention/lib/api
go get github.com/sofmon/convention/lib/auth
go get github.com/sofmon/convention/lib/cfg
go get github.com/sofmon/convention/lib/ctx
go get github.com/sofmon/convention/lib/db
```

### Dart

Add to your `pubspec.yaml`:

```yaml
dependencies:
  convention_dynamic_form:
    git:
      url: https://github.com/sofmon/convention.git
      path: lib/dynamic_form
  convention_state_sync:
    git:
      url: https://github.com/sofmon/convention.git
      path: lib/state_sync
```

## Packages

### Core Packages (Go)

| Package | Import As | Description |
|---------|-----------|-------------|
| [cfg](./cfg/) | `convCfg` | File-based configuration management |
| [ctx](./ctx/) | `convCtx` | Structured context with logging and error handling |
| [auth](./auth/) | `convAuth` | JWT authentication and role-based access control |
| [api](./api/) | `convAPI` | Type-safe HTTP API framework with OpenAPI generation |
| [db](./db/) | `convDB` | Multi-tenant sharded database with ORM |
| [lifecycle](./lifecycle/) | `convLifecycle` | Bounded signal-driven service shutdown |

### Cross-Platform Packages (Go + Dart)

| Package | Description |
|---------|-------------|
| [localized](./localized/) | Multi-language string storage with fallback chain |
| [money](./money/) | Monetary value handling with currency support |
| [storage](./storage/) | Cloud storage abstraction (GCS implementation included) |

### Flutter Packages (Dart)

| Package | Description |
|---------|-------------|
| [dynamic_form](./dynamic_form/) | Map-based form generation without code generation |
| [state_sync](./state_sync/) | Automatic bidirectional state synchronization with REST APIs |

## Package Details

### cfg - Configuration

File-based configuration from `/etc/agent/`. Read strings, bytes, or JSON objects with optional panic variants for fail-fast initialization.

→ See [cfg/README.md](./cfg/README.md)

### ctx - Context

Structured context wrapper with hierarchical scope tracking, automatic error wrapping, and slog-based logging. Propagates workflow IDs, claims, and time across the request lifecycle.

→ See [ctx/README.md](./ctx/README.md)

### auth - Authentication

JWT-based authentication with HMAC signing and role-based access control. Supports multi-tenancy with dynamic path matching using template placeholders (`{user}`, `{tenant}`, `{entity}`, `{any}`).

→ See [auth/README.md](./auth/README.md)

### api - API Framework

Type-safe HTTP API framework using Go generics. Define APIs as structs with automatic routing, serialization, and OpenAPI 3.0 generation. Supports pre/post check handlers for authorization.

→ See [api/README.md](./api/README.md)

### db - Database

Multi-tenant sharded database abstraction with JSONB storage, automatic history tracking, and full-text search. Supports PostgreSQL and SQLite backends with CRC32-based shard routing.

→ See [db/README.md](./db/README.md)

### lifecycle - Service Lifecycle

Run a service until SIGINT, SIGTERM, or listener completion, then execute named
shutdown stages concurrently under one deadline.

→ See [lifecycle/README.md](./lifecycle/README.md)

### localized - Localization

Multi-language string storage following IETF BCP 47 standard. Go and Dart implementations with SQL driver integration and fallback chain (exact locale → language-only → English).

→ See [localized/README.md](./localized/README.md)

### money - Money

Monetary value handling using minor units (cents) in Go and Decimal in Dart to prevent floating-point errors. Supports arithmetic operations, string parsing, and JSON serialization with cross-platform compatibility.

→ See [money/README.md](./money/README.md)

### storage - Storage

Cloud storage abstraction with provider-based architecture. Includes GCS implementation for Go and HTTP proxy support for Dart/Flutter with drag-and-drop upload widget.

→ See [storage/README.md](./storage/README.md)

### dynamic_form - Dynamic Forms (Flutter)

Generate view/edit UIs from `Map<String, dynamic>` without code generation. Supports type inference, custom field widgets, dot notation for nested fields, and validation.

→ See [dynamic_form/README.md](./dynamic_form/README.md)

### state_sync - State Sync (Flutter)

Automatic bidirectional state synchronization with REST APIs. Hash-based caching prevents unnecessary rebuilds, with support for optimistic updates and configurable refresh intervals.

→ See [state_sync/README.md](./state_sync/README.md)

## Architecture Patterns

All Go packages follow consistent conventions:

- **Import naming**: `convAPI`, `convAuth`, `convCfg`, `convCtx`, `convDB`
- **Context-first**: Functions take `ctx convCtx.Context` as the first parameter
- **Named error returns**: Use `defer ctx.Exit(&err)` for automatic error wrapping with scope
- **Configuration**: Read from `/etc/agent/` using the `cfg` package
- **Type safety**: Extensive use of generics and custom types to prevent misuse

## Documentation

Each package contains:

- **README.md**: User guide with examples
- **AGENTS.md**: AI agent implementation reference
