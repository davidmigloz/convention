# Job Package (job)

A multi-tenant, database-synchronized job scheduler that ensures exactly-once execution across multiple application instances.

## Overview

This package provides a recurring job scheduler backed by the `convDB` database layer. Jobs are registered in-memory with an associated function, while their schedule state (next run time, repeat interval) is persisted in the database. When multiple instances of the application run concurrently, the database acts as the coordination layer -- only one instance executes a given job at any point in time, using pessimistic locking.

## Key Features

- **Multi-instance safety**: Database locks prevent duplicate execution across instances
- **Multi-tenancy**: Jobs are scoped to tenants, isolated from each other
- **Persistent scheduling**: Job schedules survive restarts via database persistence
- **Periodic sync**: In-memory state syncs with the database every minute to pick up changes from other instances
- **Immediate wake-up**: Registering a new job wakes the background loop immediately
- **Panic recovery**: A panicking job function does not crash the background goroutine
- **Graceful shutdown**: Context cancellation cleanly stops the scheduler

## Quick Start

### 1. Initialise the Job Runner

```go
err := job.Initialise(ctx, "my_vault")
if err != nil {
    log.Fatal(err)
}
```

This creates the database tables (via `convDB`) and starts the background goroutine.

### 2. Register a Job

```go
err := job.Register(
    ctx,
    "my-tenant",                          // tenant
    "daily-cleanup",                      // job ID (unique per tenant)
    time.Now().Add(1*time.Hour),          // first run time
    24*time.Hour,                         // repeat interval
    func(ctx convCtx.Context) error {
        // your job logic here
        return nil
    },
)
```

If the job already exists in the database (e.g. from a previous run), the persisted schedule is used and the function is attached to it.

### 3. Unregister a Job

```go
err := job.Unregister(ctx, "my-tenant", "daily-cleanup")
```

Removes the job from both in-memory state and the database.

### 4. Shut Down

```go
err := job.Cancel()
```

Stops the background goroutine. The scheduler can be re-initialised later.

## How It Works

### Background Loop

The scheduler runs a single background goroutine that repeats the following cycle:

1. **Sync** -- Every minute, fetch all jobs from the database for each known tenant and merge into the in-memory map. This picks up schedule changes made by other instances (e.g. updated `NextRunAt` after execution).

2. **Execute** -- For each job where `NextRunAt <= now` and the job function is available locally, attempt to acquire a database lock with a **heartbeat lease**. If acquired, execute the job (renewing the lease in the background while it runs); if a live lock is held by another instance, skip it (logged, not silent).

3. **Sleep** -- Compute the time until the next job is due or the next sync interval (whichever is sooner) and sleep until then.

### Multi-Instance Coordination

When a job is due:

1. Instance A and Instance B both detect `NextRunAt <= now`
2. Both attempt a lease acquire (`INSERT ... ON CONFLICT DO UPDATE ... WHERE created_at < now-lease`)
3. Only one succeeds (`RowsAffected == 1`) -- that instance executes the job and renews the lease (heartbeat) while it runs
4. The other gets `RowsAffected == 0` (a live lease is held) and skips (logged)
5. After execution, the winning instance updates `NextRunAt` and releases the lock (owner-safe delete)
6. On the next sync cycle, all instances pick up the updated schedule

**Crash recovery:** because the lock carries a renewed lease, an instance that crashes mid-job (without unlocking) does not orphan the job forever — once the lease goes stale, another instance steals the lock on its next attempt. If the lease is lost mid-run (stolen/expired), that run does **not** advance `NextRunAt`; the new owner becomes responsible for scheduling.

**Idempotent registration:** registering a job ID that already exists re-attaches the function and refreshes the schedule (it does not error), so a sync-vs-register race needs no caller-side workaround. Changing the **interval** re-anchors `NextRunAt` so a shortened interval takes effect on deploy (persisted before the in-memory update); a same-interval **fire-time** change (e.g. 03:00→04:00) is not auto-applied — use a deliberate `Unregister`+`Register`.

**Completion signal:** a successful run emits `"job execution completed"` (`tenant`, `job_id`, `duration_ms`). A failed, panicked, lease-lost, or persist-failed run does not — so monitoring can alert on the *absence* of this log per (service, tenant, job).

### Job Functions

Job functions (`JobFunc`) are `func(convCtx.Context) error`. They:

- Receive the scheduler's context (with cancellation wired in)
- Should check `ctx.Done()` for graceful shutdown during long operations
- Errors are logged but do not prevent schedule advancement (to avoid tight retry loops)
- Panics are recovered and logged

### Schedule Advancement

After execution, `NextRunAt` is advanced by `RepeatEvery`. If multiple intervals were missed (e.g. instance was down), the schedule jumps forward to the next future time rather than executing all missed runs.

## Types

```go
type JobID string                           // Unique job identifier (per tenant)
type JobFunc func(convCtx.Context) error    // Job function signature
```

## API

| Function | Description |
|---|---|
| `Initialise(ctx, vault)` | Start the scheduler with the given database vault |
| `Register(ctx, tenant, id, startAt, repeatEvery, fn)` | Register a recurring job |
| `Unregister(ctx, tenant, id)` | Remove a job |
| `Cancel()` | Stop the scheduler |

## Configuration

The package uses `convDB` for persistence. Database connections must be configured via the config system for the vault passed to `Initialise()`. See the [db package README](../db/README.md) for configuration details.

## Error Handling

- `Initialise` returns an error if the scheduler is already running
- `Register` returns an error if the scheduler is not initialised or if a job with the same ID already exists for the tenant
- `Unregister` returns an error if the tenant or job does not exist
- `Cancel` returns an error if the scheduler is not running
- Background errors (DB sync failures, lock errors, job execution errors) are logged via `ctx.Logger()` and do not stop the scheduler

## Thread Safety

- All public functions are safe for concurrent use (protected by `sync.Mutex`)
- Job execution happens outside the mutex to avoid blocking `Register`/`Unregister` calls
- Database locks provide cross-instance synchronization
