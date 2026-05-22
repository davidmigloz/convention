# Job Package Implementation Details (for AI Agents)

This document provides implementation details for AI agents working on the `lib/job` package.

> IMPORTANT: AI agents must treat AGENTS.md and README.md as authoritative living documents. Any change to the implementation that affects behaviors must be mirrored in both files. The code and documentation must never drift apart. When the implementation changes, these documents must be updated immediately so they always reflect the current system.

## File Organization

- **[job.go](job.go)** - All types, state, public API, and background loop implementation

## Core Types

### Public Types (job.go:14-18)

```go
type JobID string                           // Unique identifier per tenant
type JobState string                        // Currently unused, reserved for future use
type JobFunc func(convCtx.Context) error    // Job function signature
```

### Internal Types (job.go:20-26)

```go
type job struct {
    ID          JobID         `json:"id"`
    NextRunAt   time.Time     `json:"next_run_at"`
    RepeatEvery time.Duration `json:"repeat_every"`
    f           JobFunc       `json:"-"`  // In-memory only, not serialized
}
```

The `f` field is critical -- it is `json:"-"` and therefore never persisted. When syncing from the database, the `f` from the in-memory map must be preserved. Jobs loaded from the database without a local `f` are tracked for scheduling awareness but cannot be executed by that instance.

### DBKey (job.go:28-33)

```go
func (x job) DBKey() convDB.Key[JobID, JobID]
```

Uses `JobID` for both ID and ShardKey. This means each job is its own shard key, ensuring lock operations target the correct database shard.

## Package State (job.go:35-41)

```go
var (
    jobs   map[convAuth.Tenant]map[JobID]job    // In-memory job registry
    mut    sync.Mutex                            // Protects all access to jobs map
    cancel context.CancelFunc                    // Cancels the background goroutine
    jobsDB convDB.ObjectSetReady[job, JobID, JobID]  // Database handle
    wakeUp chan struct{}                          // Buffered channel to wake background loop
)
```

All package state is global. The package is designed as a singleton scheduler -- only one `Initialise` call is allowed at a time.

In addition, the package holds lease tuning and the per-process owner token:

```go
var (
    jobLease         = 90 * time.Second   // a lock not renewed within this is stealable
    jobRenewInterval = 30 * time.Second   // heartbeat cadence (≈ lease/3)
)
var ownerToken string                     // per-process UUID, set in Initialise
```

## Public API

### Initialise (job.go:133-149)

```go
func Initialise(ctx convCtx.Context, vault convDB.Vault) error
```

1. Acquires mutex
2. Fails if already running (`cancel != nil`)
3. Creates `jobsDB` via `convDB.NewObjectSet`
4. Generates `ownerToken` (a per-process UUID used to tag lock ownership for the heartbeat lease)
5. Creates `wakeUp` channel (buffered, capacity 1)
6. Creates cancellable context from provided context
7. Launches `background()` goroutine

### Register (job.go:42-91)

```go
func Register(ctx convCtx.Context, tenant convAuth.Tenant, jid JobID, startAt time.Time, repeatEvery time.Duration, fn JobFunc) error
```

1. Fails if `jobsDB == nil` (not initialised)
2. **Idempotent re-registration**: if the job ID already exists in memory — including a nil-closure entry that `syncJobsFromDB` injected before this `Register` ran — it re-attaches the closure and refreshes the interval instead of returning "already exists". Callers therefore never need an Unregister+retry workaround for the sync race.
3. Reads any persisted job (`SelectByID`). When a DB row exists and the interval is **unchanged**, its persisted `NextRunAt` is kept (a redeploy does not reset every job's clock). When the **interval changed**, `NextRunAt` is **re-anchored to `startAt`** so a shortened interval takes effect on deploy; `RepeatEvery` is persisted. `startAt` is otherwise only used for brand-new jobs.
4. If not in DB: creates a new job and inserts
5. **Persist-before-memory:** an interval change is written to the DB **first**; only on success is the in-memory map mutated. The scheduler goroutine is already running, so an un-persisted memory change that the next `syncJobsFromDB` reverts would flap. On `Update` error, memory is left untouched and the error returns.
6. **Anchor-change escape hatch:** a *same-interval* fire-time change (daily 03:00→04:00, weekly Monday→Tuesday — same `RepeatEvery`) is **not** auto-re-anchored (to preserve no-clock-reset on redeploy). To change a fire time, do a deliberate one-time `Unregister` + `Register` (or use a new job ID). **Do not** reintroduce a per-deploy unregister/register dance for this.
7. Sends a non-blocking signal on `wakeUp` (via `wake()`) to wake the background loop

### Unregister (job.go:93-116)

```go
func Unregister(ctx convCtx.Context, tenant convAuth.Tenant, jid JobID) error
```

1. Validates tenant and job exist in memory
2. Deletes from in-memory map
3. Deletes from database

### Cancel (job.go:118-131)

```go
func Cancel() error
```

Calls the cancel function and sets it to nil. The background goroutine detects cancellation via `ctx.Done()` and exits.

## Background Loop (job.go:151-192)

```go
func background(ctx convCtx.Context)
```

Main scheduling loop with three responsibilities:

### Loop Structure

```
syncInterval = 1 minute
lastSync = zero time (forces immediate first sync)

loop:
    if time since lastSync >= syncInterval:
        syncJobsFromDB(ctx)
        lastSync = now

    nextWakeUp = executeAndSchedule(ctx)

    sleepDuration = min(time until next sync, time until nextWakeUp)
    clamp to >= 1 second

    select:
        case <-ctx.Done(): return
        case <-wakeUp: stop timer, continue (new job registered)
        case <-timer.C: continue
```

Uses `time.Timer` (not `time.Ticker`) because sleep duration is dynamic. The 1-second minimum prevents tight loops on transient errors or when all jobs are due simultaneously.

## Helper Functions

### syncJobsFromDB (job.go:194-234)

```go
func syncJobsFromDB(ctx convCtx.Context)
```

Holds the mutex for the entire operation. For each tenant:

1. Calls `jobsDB.Tenant(tenant).SelectAll(ctx)`
2. Errors are logged and the tenant is skipped (retries next cycle)
3. Merges DB state into memory: updates schedule fields from DB while preserving `f`
4. Adds DB-only jobs with `f == nil` (tracked but not executable)

**Tenant discovery limitation**: Only syncs tenants already present in the `jobs` map. A tenant must have at least one local `Register()` call to be known. This is by design.

### executeAndSchedule (job.go:236-283)

```go
func executeAndSchedule(ctx convCtx.Context) (nextWakeUp time.Time)
```

Two-phase operation:

**Phase 1 (under mutex)**: Snapshot all due jobs (`NextRunAt <= now` and `f != nil`) into a local slice, then release the mutex.

**Execution (no mutex)**: Execute each due job via `executeJob()`. This allows `Register`/`Unregister` to proceed concurrently.

**Phase 2 (under mutex)**: Re-scan all jobs to find the earliest future `NextRunAt`. Return it as `nextWakeUp` (zero value if no future jobs exist).

### executeJob (job.go:285-370)

```go
func executeJob(ctx convCtx.Context, tenant convAuth.Tenant, j job)
```

Execution flow:

1. **Guard**: Skip if `RepeatEvery <= 0` (prevents infinite loop in schedule advancement)
2. **Acquire (heartbeat lease)**: `jobsDB.Tenant(tenant).Lock(ctx, j, "executing job <id> @ <ownerToken>", convDB.WithLease(jobLease))`. If `lock == nil`, a live lock is held by another instance — skip, but **log at Info** (no longer silent, so a genuinely stuck job is visible). If `lock.Stolen()`, log a Warn with `lock.PreviousOwner()` (the prior holder crashed without unlocking).
3. **Cancellable job context**: copy the `convCtx.Context` and replace its embedded `context.Context` with a cancellable child (`jobCtx`). `j.f` receives `jobCtx` so a job that honours cancellation aborts on lease loss.
4. **Heartbeat goroutine**: every `jobRenewInterval`, call `lock.Renew(jobCtx)`. On `ErrLeaseLost` (classified by `renewOutcome`) set `leaseLost` and `jobCancel()`; on a **transient** error, log and keep retrying (do NOT drop a live lease for a DB blip).
5. **Teardown defer**: `jobCancel()` then `<-hbDone` (join — leak-free because `Renew` uses `ExecContext`, so the cancel interrupts an in-flight renew), then owner-safe `lock.Unlock()` (an `ErrLeaseLost` here is a Warn, meaning the lock was stolen mid-run).
6. **Execute**: Run `j.f(jobCtx)` inside a nested func with `recover()`.
7. **Gate on lease ownership**: if `leaseLost`, **return without advancing/persisting `NextRunAt`** — the new owner is now responsible for scheduling.
8. **Advance schedule** (only when the lease was held throughout): `nextRunAt = NextRunAt + RepeatEvery`, advancing past missed intervals; update the in-memory map (under mutex) and persist via `Update`.
9. **Completion log** (`"job execution completed"`, with `tenant`/`job_id`/`duration_ms`): emitted **only on genuine success** — `j.f` returned nil, did not panic, the lease was held, and the `Update` succeeded. A failed/panicked/lease-lost/update-failed run does **not** emit it. This is the uniform signal for "job ran"; alerting keys off its *absence* per (service, tenant, job).

**Error policy**: `NextRunAt` is advanced even when the job function returns an error (a permanently failing job must not retry in a tight loop) — but NOT when the lease was lost mid-run. Note the advance is independent of *success*: the `"job execution completed"` log (not the schedule advance) is the success signal.

**Heartbeat lease (why)**: the lock is acquired with a TTL lease and renewed while the job runs, so a holder that crashes without unlocking is reclaimable (its lock is stolen once stale) instead of orphaning the job forever. `jobLease`/`jobRenewInterval` are package vars (default 90s / 30s = 3 heartbeats per lease window). `jobLease` need not exceed job runtime — the heartbeat keeps long jobs (e.g. minutes-long reconciliations) alive; it only bounds how long a crashed holder blocks others. The steal comparison assumes pod clocks are NTP-synced within a few seconds. `ownerToken` is a per-process UUID set in `Initialise`; it tags the lock's `description` so Renew/Unlock are owner-safe.

## Database Interaction

### Tables

The `convDB.NewObjectSet[job, JobID, JobID](vault).Ready()` call creates three tables (see [db AGENTS.md](../db/AGENTS.md)):

- `job` -- Runtime table with JSONB object column
- `job_history` -- History tracking
- `job_lock` -- Pessimistic lock table

### Operations Used

| Operation | Where Used | Purpose |
|---|---|---|
| `SelectAll` | `syncJobsFromDB` | Fetch all jobs for a tenant |
| `SelectByID` | `Register` | Check if job already exists in DB |
| `Insert` | `Register` | Persist new job |
| `Delete` | `Unregister` | Remove job from DB |
| `Lock` (`WithLease`) | `executeJob` | Acquire execution lock with a heartbeat lease |
| `Renew` | `executeJob` heartbeat | Refresh the lease while the job runs |
| `Unlock` | `executeJob` | Release execution lock (owner-safe) |
| `Update` | `executeJob` | Persist updated `NextRunAt` |

### Lock Mechanism

The scheduler uses `convDB`'s **heartbeat-lease** lock mode ([lock.go](../db/lock.go)) via `Lock(..., convDB.WithLease(jobLease))`:

```sql
INSERT INTO "job_lock" AS l ("id","created_at","description")
VALUES($1,$2,$3)
ON CONFLICT ("id") DO UPDATE SET "created_at"=$2, "description"=$3
WHERE l."created_at" < $4;     -- $4 = now - lease  (a stale lock is stolen)
```

- Non-blocking: returns immediately
- `RowsAffected == 1` means the lock was acquired, or a stale one was stolen
- `RowsAffected == 0` means a live (recently-renewed) lock is held by another instance
- `created_at` doubles as the heartbeat timestamp; `Renew` advances it; `Unlock` is owner-safe (`WHERE "id"=$1 AND "description"=$2`) and never removes a lock stolen away after expiry
- The plain `ON CONFLICT DO NOTHING` mode (no `WithLease`) is unchanged and still used by non-job callers that want a sticky, never-stolen mutex

## Concurrency Model

### Mutex Scope

The `mut` mutex protects all reads and writes to the `jobs` map. It is held:

- During `Register` and `Unregister` (entire function)
- During `syncJobsFromDB` (entire function, including DB calls)
- During `executeAndSchedule` Phase 1 (snapshot) and Phase 2 (re-scan)
- During `executeJob` in-memory update (brief, just map write)

The mutex is intentionally **not** held during job execution or database lock/update operations in `executeJob`.

### Cross-Instance Coordination

Database locks (not the in-memory mutex) provide cross-instance mutual exclusion. The mutex only protects the local `jobs` map.

## Known Limitations

1. **Tenant discovery is local**: Only tenants with at least one locally registered job are synced from the database.
2. **Sequential job execution**: Due jobs within a single loop iteration are executed sequentially, not in parallel.
3. **Mutex held during DB sync**: `syncJobsFromDB` holds the mutex while calling `SelectAll`, which blocks `Register`/`Unregister` during sync. For small job counts this is acceptable.
4. **No job execution timeout**: Job functions can run indefinitely. They now receive a context that is cancelled on lease loss, but there is still no per-job deadline; a job that ignores cancellation keeps running. Its scheduling is still gated (a lost-lease run does not advance `NextRunAt`), so the worst case is at-most-once extra side effects, not a corrupted schedule.
5. **`JobState` type is unused**: Defined but not referenced anywhere.

## Common Modification Scenarios

### Adding Job Execution Timeout

1. In `executeJob`, wrap `j.f(ctx)` call with a context deadline:
   ```go
   timeoutCtx, cancel := context.WithTimeout(ctx.Context, timeout)
   defer cancel()
   ```
2. Consider making timeout configurable per job (add field to `job` struct)

### Adding Parallel Job Execution

1. In `executeAndSchedule`, replace the sequential `for` loop with goroutines and `sync.WaitGroup`
2. Be careful with the mutex -- `executeJob` already acquires it briefly for the in-memory update

### Adding One-Shot Jobs (No Repeat)

1. Allow `RepeatEvery == 0` to mean "run once"
2. In `executeJob`, after execution: if `RepeatEvery == 0`, delete the job instead of advancing schedule
3. Update the `RepeatEvery <= 0` guard to only reject negative values
