# Convention

**Convention** defines standards for building containerized, multi-tenant, multi-entity agent architectures. This document specifies the core concepts, security practices, communication protocols, and data management guidelines required to operate and integrate with Convention-compliant systems.

A reference implementation is available in [./lib](./lib/) supporting Go and Dart.

## Glossary

- **Agent**: A program with a specific purpose in the system, striving to fulfil its purpose independently of external signals.
- **Tenant**: A unique identifier for a tenant supported by the system. Convention implements [multitenancy](https://en.wikipedia.org/wiki/Multitenancy) by default.
- **User**: A unique identifier for an authenticated user. Each agent can authenticate as a user using its name as the identifier.
- **Entity**: A unique identifier for a legal entity, with each user possibly having multiple entities. Data is stored by entity to enable a user to access multiple entities (e.g., personal and business financial accounts).
- **Role**: A unique string that defines a user's specific permissions within the system.
- **Permission**: A unique identifier for allowed actions within the system.
- **Action**: A unique identifier for an operation and resource, mapping to HTTP methods and paths, e.g., `GET /message/v1/tenants/default/entities/ecf8efa3/messages/f38ce157`.
- **Workflow**: A unique identifier for the specific workload that is being handled by an **agent**.

## Configuration and Secrets Management

### Configuration Overview

**Convention** is designed for containerized environments where secrets are mounted to the container's file system.

There is no distinction between secrets and configuration values; both must follow best practices for secret management as defined by the hosting environment.

### Configuration Keys/Files

By default, all configuration files are stored in `/etc/agent`.

The following keys/files must be automatically provided by the hosting environment:

- **environment**: Specifies the environment name, with "production" as the production environment.
- **communication_certificate**: SSL certificate for internal HTTPS communication.
- **communication_key**: SSL key for internal HTTPS communication.
- **communication_secret**: Secret used for signing and verifying authorization tokens.
- **database**: Configuration details for accessing the system's database.

## Communication Protocol

All communication uses the HTTP protocol with a JSON-formatted body.

### Secure Communication

All communication is secured with SSL (HTTPS), using the **communication_certificate** and **communication_key** located at `/etc/agent`.

The **communication_certificate** must be trusted by the container hosting OS.

### Actions and Resources

Each **action** includes an operation and a resource, corresponding to HTTP methods and paths. The **agent** name and version always appear as the first segments of a resource identifier.

In the example below, the **agent** name is "message-v1":

``` HTTP
GET /message/v1/tenants/default/entities/ecf8efa3/messages/f38ce157
```

### Error Handling

Errors are communicated in JSON format with code and message fields:

``` JSON
{
    "code": "...",
    "message": "..."
}
```

### Workflow and Agent Headers

Every task engaged by an **agent** must have a **workflow** identifier. If the task is initiated by an incoming HTTP request, the **agent** should use the **workflow** identifier from the `Workflow` HTTP header.

If no **workflow** identifier is available, the **agent** must generate a new unique **workflow** identifier and pass it along.

`Workflow` headers should be included in all outgoing HTTP requests as well as the `Agent` header containing the **agent** name.

Example:

``` HTTP
GET /message/v1/tenants/default/entities/ecf8efa3/messages/f38ce157
Workflow: {workflow identifier}
Agent: {agent name}
```

### Time Management (Test Environments Only)

In non-production environments, **agents** must adhere to the `Time-Now` header from the incoming HTTP request to simulate different times. The format follows RFC3339 (e.g., 2006-01-02T15:04:05Z07:00).

The `Time-Now` header must be ignored in production environments.

Example:

``` HTTP
GET /message/v1/tenants/default/entities/ecf8efa3/messages/f38ce157
Workflow: {workflow identifier}
Agent: {agent name}
Time-Now: 2024-01-02T15:04:05Z07:00
```

## Authentication and Authorization

### Authentication with JWT Tokens

**Convention** uses JWT tokens for internal authentication, passed in the Authorization header.

Example:

``` HTTP
GET /message/v1/tenants/default/entities/ecf8efa3/messages/f38ce157
Workflow: {workflow identifier}
Agent: {agent name}
Authorization: Bearer {token}
```

Tokens are signed with the **communication_secret** in `/etc/agent/communication_secret`.

### Required JWT Claims

The JWT tokens must include the following claims:

| Claim | Type | Description |
|-|-|-|
| **agent** | string | Agent name |
| **user** | string | Authenticated user |
| **tenants** | array of strings | User's tenants |
| **entities** | array of strings | Entities accessible by the user |
| **roles** | array of strings | User's assigned roles |

### Role-Based Access Control

In addition to the **user** claims, all **agents** have access to common configuration details about:

- **roles**: All known roles within the system.
    - **permissions**: All permissions allowed for each **role**.
        - **action templates**: All action templates allowed for each **permission**.

In JSON format a simple configuration can look like:

``` JSON
{
    "roles": {
        "user": [
            "write_tenant_messages",
            "read_own_messages"
        ],
        "admin": [
            "write_tenant_messages",
            "read_tenant_messages"
        ]
    },
    "permissions": {
        "write_tenant_messages": [
            "PUT /message/v1/tenants/{tenant}/entities/{any}/messages/{any}"
        ],
        "read_tenant_messages": [
            "GET /message/v1/tenants/{tenant}/entities/{any}/messages/{any}"
        ],
        "read_own_messages": [
            "GET /message/v1/tenants/{tenant}/entities/{entity}/messages/{any}"
        ]
    },
    "public": [
        "GET /message/v1/openapi.yaml"
    ]
}
```

Access control is achieved by extracting all allowed action templates from the **user**'s **roles** and matching them against the incoming action (HTTP request).

For example, the action template:

``` HTTP
GET /message/v1/tenants/{tenant}/entities/{entity}/messages/{any}
```

will match the action:

``` HTTP
GET /message/v1/tenants/default/entities/ecf8efa3/messages/f38ce157
```

only when the authenticated user has access to the corresponding action template (through "user" **role**) and has a **tenant** and **entity** in their authorization claims that match the values "default" and "ecf8efa3".

### Action Template Placeholders

The action template supports the following placeholders:

- `{any}`: Ignore any value in this part of the path.
- `{any...}`: Ignore any value from this point onward in the path.
- `{user}`: Identifies the user as part of the path; a check will be performed to ensure the user matches the authenticated user.
- `{tenant}`: Identifies the tenant as part of the path; a check will be performed to ensure the tenant is allowed for the authenticated user.
- `{entity}`: Identifies the entity as part of the path; a check will be performed to ensure the entity is allowed for the authenticated user.

## Data Storage and Sharding

### Vaults

First segmentation of database in **Convention** is called vault. Through different vault names, **Convention** enables access to different databases, schemas, or database servers.

This approach is also useful to facilitate schema updates or database engine changes as part of the **agent**'s operations.

### Multitenancy

The multi-tenancy is directly implemented in the database. Each **tenant** has its own database connection, ensuring data isolation and security.

### Sharding

Database sharding in **Convention** allows data to be distributed across multiple databases, schemas, or servers. This approach enhances performance and scalability by balancing the data load, ensuring that no single database becomes a bottleneck.

Each shard contains a subset of the data, and the **Convention** implementation should intelligently route queries to the appropriate shard based on the data's partitioning logic. Each **tenant** can have one or multiple shards.

Changing the number of shards requires a full data migration, which can be achieved through the multi vault mechanism. This allows each **agent** to migrate its own data as part of their operations.

`Select` and `SelectWithMetadata` exclude runtime rows whose `object` column is
SQL `NULL`. A runtime row without an object is treated as absent; deletion
tombstones use SQL `NULL` only in history tables.

### Database Configuration

Database configurations are located in the **database** key in `/etc/agent/database`, specifying database connection per vault, tenants, and shards.

``` JSON
{
    "cache": {
        "tenant1": [
            {
                "engine": "sqlite",
                "in_memory": true
            }
        ]
    },
    "messages": {
        "tenant1": [
            {
                "host": "127.0.0.1",
                "port": 5432,
                "database": "t1_messages_shard1",
                "username": "some_user",
                "password": "some_password"
            },
            {
                "host": "127.0.0.1",
                "port": 5432,
                "database": "t1_messages_shard2",
                "username": "some_user",
                "password": "some_password"
            }
        ]
    }
}
```

## Logging and Monitoring

### Logging Structure

Logs in **Convention** are collected from `stdout` in JSON format, each entry ending with a newline (`\n`).

### Log Messages

There are four log levels for messages: "error", "warning", "info", and "debug".

Logs are formatted as follows:

``` JSON
{
    "time": "2023-10-01T01:00:00Z",
    "level": "error",
    "agent": "message-v1",
    "user": "josh",
    "action": "GET messages/v1/users/josh/messages",
    "workflow": "a9ca2c1a-f993-420d-b851-726dafc35102",
    "scope": "messages-v1 > svc.ListenAndServe > svc.handleGetMessages",
    "message": "unable to connect to database"
}
```

### HTTP Trace

An additional log level, "trace," is used to log incoming and outgoing HTTP calls.

The format is as follows:

``` JSON
{
    "time": "2023-10-01T01:00:00Z",
    "level": "trace",
    "agent": "message-v1",
    "user": "josh",
    "action": "GET messages/v1/users/josh/messages",
    "workflow": "a9ca2c1a-f993-420d-b851-726dafc35102",
    "request": "GET /message/v1/tenants/default/entities/ecf8efa3/messages/f38ce157\nAuthorization: Bearer ...\nWorkflow: 005dba5e\nAgent: profile-v1...",
    "response": "HTTP/1.1 200 OK\nDate: Mon, 27 Jul 2009 12:28:53 GMT\nWorkflow: 005dba5e\nAgent: message-v1..."
}
```

The `Authorization` header must be obfuscated in the trace message request and response.

Only HTTP headers are logged for HTTP requests and responses with binary payloads.

## Reference Implementation

A reference implementation of Convention is available in [./lib](./lib/) with support for:

- **Go**: Server-side packages for building Convention-compliant agents
- **Dart/Flutter**: Client-side packages for building mobile and web applications

See [lib/README.md](./lib/README.md) for package documentation and quick start guide.

## License

This project is licensed under the [MIT License](LICENSE). See the [LICENSE](LICENSE) file for details. You are free to use, modify, and distribute this software as long as you adhere to the terms of the MIT License.
