package job

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
	convDB "github.com/sofmon/convention/lib/db"
)

type JobID string

type JobState string

type JobFunc func(convCtx.Context) error

type job struct {
	ID          JobID         `json:"id"`
	NextRunAt   time.Time     `json:"next_run_at"`
	RepeatEvery time.Duration `json:"repeat_every"`

	f JobFunc `json:"-"`
}

func (x job) DBKey() convDB.Key[JobID, JobID] {
	return convDB.Key[JobID, JobID]{
		ID:       x.ID,
		ShardKey: x.ID,
	}
}

var (
	jobs   map[convAuth.Tenant]map[JobID]job
	mut    sync.Mutex
	cancel context.CancelFunc
	jobsDB convDB.ObjectSetReady[job, JobID, JobID]
	wakeUp chan struct{}
)

// registerInsertRaceHookForTest, when non-nil, is invoked immediately before
// each Insert attempt Register makes (both the initial attempt and the
// bounded one-time retry below) — a test seam for deterministically
// injecting a peer replica's concurrent Insert/Delete right at the point a
// real race would land. Nil in production; see export_test.go's
// SetRegisterInsertRaceHookForTest.
var registerInsertRaceHookForTest func()

// Lease tuning for the per-execution job lock. jobRenewInterval must stay well
// below jobLease so a couple of transient renew failures don't expire a live lease;
// jobLease bounds how long a crashed holder's lock blocks others before it is
// stolen (it need NOT exceed job runtime — the heartbeat keeps long jobs alive).
// Package vars (not consts) so tests can shorten them. The steal comparison assumes
// pod clocks are NTP-synced within a few seconds.
var (
	jobLease         = 90 * time.Second
	jobRenewInterval = 30 * time.Second
)

// ownerToken uniquely identifies this process as a lock holder; set in Initialise.
var ownerToken string

// renewOutcome classifies a heartbeat Renew result: fatal==true means the lease is
// confirmed lost (stop the job and let the new owner run); otherwise the heartbeat
// retries on the next tick.
func renewOutcome(err error) (fatal bool) {
	return errors.Is(err, convDB.ErrLeaseLost)
}

// wake nudges the background loop to re-evaluate jobs (non-blocking).
func wake() {
	select {
	case wakeUp <- struct{}{}:
	default:
	}
}

func Register(ctx convCtx.Context, tenant convAuth.Tenant, jid JobID, startAt time.Time, repeatEvery time.Duration, fn JobFunc) (err error) {

	if jobsDB == nil {
		err = fmt.Errorf("job runner is not initialised - call Initialise first")
		return
	}

	mut.Lock()
	defer mut.Unlock()

	if jobs == nil {
		jobs = make(map[convAuth.Tenant]map[JobID]job)
	}

	if _, ok := jobs[tenant]; !ok {
		jobs[tenant] = make(map[JobID]job)
	}

	savedJob, err := jobsDB.Tenant(tenant).SelectByID(ctx, jid)
	if err != nil {
		return
	}

	// Idempotent re-registration. The in-memory entry may carry a nil closure that
	// syncJobsFromDB injected (it pulled the DB row in before this Register ran);
	// re-attach the closure and refresh the interval instead of erroring, so
	// callers don't need an Unregister+retry workaround.
	if existing, ok := jobs[tenant][jid]; ok {
		intervalChanged := savedJob != nil && savedJob.RepeatEvery != repeatEvery

		// Decide the schedule: re-anchor to startAt when the interval changed (so a
		// shortened interval takes effect on deploy); otherwise honour the persisted
		// NextRunAt so a same-interval redeploy doesn't reset the clock. A same-interval
		// *anchor* change (e.g. 03:00->04:00) is intentionally NOT re-anchored — it
		// requires a deliberate Unregister+Register (see lib/job AGENTS.md).
		nextRunAt := startAt
		if savedJob != nil && !savedJob.NextRunAt.IsZero() && !intervalChanged {
			nextRunAt = savedJob.NextRunAt
		}

		// Persist a schedule change BEFORE mutating memory: the scheduler goroutine is
		// already running, so an un-persisted memory change that the next syncJobsFromDB
		// reverts would flap. On Update error, leave memory untouched and return.
		if intervalChanged {
			updated := *savedJob
			updated.RepeatEvery = repeatEvery
			updated.NextRunAt = nextRunAt
			if err = jobsDB.Tenant(tenant).Update(ctx, updated); err != nil {
				return
			}
		}

		// Memory mutation (closure re-attach is memory-only and always safe — it re-arms
		// a sync-injected nil closure, the load-bearing idempotent behaviour).
		existing.f = fn
		existing.RepeatEvery = repeatEvery
		existing.NextRunAt = nextRunAt
		jobs[tenant][jid] = existing

		wake()
		return
	}

	if savedJob == nil {
		nj := job{
			ID:          jid,
			NextRunAt:   startAt,
			RepeatEvery: repeatEvery,
			f:           fn,
		}
		if registerInsertRaceHookForTest != nil {
			registerInsertRaceHookForTest()
		}
		err = jobsDB.Tenant(tenant).Insert(ctx, nj)
		if err == nil {
			jobs[tenant][jid] = nj
			wake()
			return
		}
		if !errors.Is(err, convDB.ErrDuplicateID) {
			return // other Insert errors abort, no retry
		}

		// Concurrent-insert race: a peer replica inserted the same id
		// between our SelectByID above and this Insert. Re-read to see
		// what it left behind and converge on it, the same way a second
		// Register after restart would.
		var racedJob *job
		racedJob, err = jobsDB.Tenant(tenant).SelectByID(ctx, jid)
		if err != nil {
			return
		}
		if racedJob != nil {
			return applyPersistedJob(ctx, tenant, jid, startAt, repeatEvery, fn, racedJob)
		}

		// The racer's row is no longer visible (concurrent Unregister
		// churn raced our re-read): retry the Insert exactly once.
		// Deliberately two bounded sequential blocks, NOT a loop and NOT
		// mutateLoop-style backoff — this is a one-time startup race, not
		// a hot write path.
		if registerInsertRaceHookForTest != nil {
			registerInsertRaceHookForTest()
		}
		err = jobsDB.Tenant(tenant).Insert(ctx, nj)
		if err == nil {
			jobs[tenant][jid] = nj
			wake()
			return
		}
		if errors.Is(err, convDB.ErrDuplicateID) {
			err = fmt.Errorf("job %s: concurrent register/unregister churn, giving up after one retry", jid)
		}
		return
	}

	return applyPersistedJob(ctx, tenant, jid, startAt, repeatEvery, fn, savedJob)
}

// applyPersistedJob reconciles a Register call with a DB row that already
// exists but has no in-memory entry yet: it keeps the persisted NextRunAt as
// the schedule, attaches the closure, and on an interval change re-anchors
// NextRunAt to startAt and persists that change (before the in-memory map
// assignment below). Called with mut already held. Two callers: the
// ordinary "first Register after restart" path below, and Register's
// insert-branch duplicate-key convergence above (see lib/job/AGENTS.md's
// Register section) — once a persisted-but-not-yet-in-memory row is in
// hand, the two cases are otherwise indistinguishable.
func applyPersistedJob(ctx convCtx.Context, tenant convAuth.Tenant, jid JobID, startAt time.Time, repeatEvery time.Duration, fn JobFunc, saved *job) (err error) {
	nj := *saved
	nj.f = fn
	if saved.RepeatEvery != repeatEvery {
		nj.RepeatEvery = repeatEvery
		nj.NextRunAt = startAt // re-anchor on interval change
		if err = jobsDB.Tenant(tenant).Update(ctx, nj); err != nil {
			return // memory untouched: jobs[tenant][jid] not yet set
		}
	}
	jobs[tenant][jid] = nj
	wake()
	return
}

func Unregister(ctx convCtx.Context, tenant convAuth.Tenant, jid JobID) (err error) {

	if jobsDB == nil {
		err = fmt.Errorf("job runner is not initialised - call Initialise first")
		return
	}

	mut.Lock()
	defer mut.Unlock()

	if _, ok := jobs[tenant]; !ok {
		err = fmt.Errorf("tenant %s does not exist", tenant)
		return
	}

	if _, ok := jobs[tenant][jid]; !ok {
		err = fmt.Errorf("job with id %s does not exist", jid)
		return
	}

	delete(jobs[tenant], jid)

	return jobsDB.Tenant(tenant).Delete(ctx, jid)
}

func Cancel() (err error) {
	mut.Lock()
	defer mut.Unlock()

	if cancel == nil {
		err = fmt.Errorf("job runner is not running")
		return
	}

	cancel()
	cancel = nil

	return
}

func Initialise(ctx convCtx.Context, vault convDB.Vault) (err error) {
	mut.Lock()
	defer mut.Unlock()

	if cancel != nil {
		err = fmt.Errorf("job runner is already running")
		return
	}

	jobsDB = convDB.NewObjectSet[job, JobID, JobID](vault).Ready()

	ownerToken = uuid.NewString()

	wakeUp = make(chan struct{}, 1)

	ctx.Context, cancel = context.WithCancel(ctx.Context)

	go background(ctx)

	return
}

func background(ctx convCtx.Context) {

	const syncInterval = 1 * time.Minute

	lastSync := time.Time{} // zero value forces immediate first sync

	for {
		now := time.Now().UTC()

		// (1) Sync with database periodically
		if now.Sub(lastSync) >= syncInterval {
			syncJobsFromDB(ctx)
			lastSync = time.Now().UTC()
		}

		// (2) & (3) Execute due jobs and determine next wake-up
		nextWakeUp := executeAndSchedule(ctx)

		// Compute sleep duration
		timeUntilNextSync := syncInterval - time.Since(lastSync)
		sleepDuration := timeUntilNextSync

		if !nextWakeUp.IsZero() {
			timeUntilNextJob := time.Until(nextWakeUp)
			if timeUntilNextJob < sleepDuration {
				sleepDuration = timeUntilNextJob
			}
		}

		if sleepDuration < 1*time.Second {
			sleepDuration = 1 * time.Second
		}

		timer := time.NewTimer(sleepDuration)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wakeUp:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func syncJobsFromDB(ctx convCtx.Context) {

	mut.Lock()
	defer mut.Unlock()

	if jobs == nil {
		return
	}

	for tenant, tenantJobs := range jobs {

		dbJobs, err := jobsDB.Tenant(tenant).SelectAll(ctx)
		if err != nil {
			ctx.Logger().Error("failed to sync jobs from database",
				"tenant", string(tenant),
				"error", err.Error(),
			)
			continue
		}

		dbJobMap := make(map[JobID]job, len(dbJobs))
		for _, dj := range dbJobs {
			dbJobMap[dj.ID] = dj
		}

		// Merge DB state into memory, preserving f
		for jid, memJob := range tenantJobs {
			if dbJob, ok := dbJobMap[jid]; ok {
				dbJob.f = memJob.f
				tenantJobs[jid] = dbJob
			}
		}

		// Add DB-only jobs (f will be nil — tracked but not executable by this instance)
		for jid, dbJob := range dbJobMap {
			if _, ok := tenantJobs[jid]; !ok {
				tenantJobs[jid] = dbJob
			}
		}
	}
}

func executeAndSchedule(ctx convCtx.Context) (nextWakeUp time.Time) {

	type dueJob struct {
		tenant convAuth.Tenant
		job    job
	}

	mut.Lock()
	var dueJobs []dueJob
	now := time.Now().UTC()

	for tenant, tenantJobs := range jobs {
		for _, j := range tenantJobs {
			if j.NextRunAt.IsZero() {
				continue
			}
			if !j.NextRunAt.After(now) && j.f != nil {
				dueJobs = append(dueJobs, dueJob{tenant: tenant, job: j})
			}
		}
	}
	mut.Unlock()

	// Execute due jobs outside the lock
	for _, dj := range dueJobs {
		executeJob(ctx, dj.tenant, dj.job)
	}

	// Re-scan for accurate next wake-up
	mut.Lock()
	defer mut.Unlock()

	now = time.Now().UTC()
	for _, tenantJobs := range jobs {
		for _, j := range tenantJobs {
			if j.NextRunAt.IsZero() {
				continue
			}
			if j.NextRunAt.After(now) {
				if nextWakeUp.IsZero() || j.NextRunAt.Before(nextWakeUp) {
					nextWakeUp = j.NextRunAt
				}
			}
		}
	}

	return
}

func executeJob(ctx convCtx.Context, tenant convAuth.Tenant, j job) {

	// Guard against zero/negative repeat interval
	if j.RepeatEvery <= 0 {
		ctx.Logger().Error("job has invalid repeat interval, skipping",
			"tenant", string(tenant),
			"job_id", string(j.ID),
			"repeat_every", j.RepeatEvery.String(),
		)
		return
	}

	// Acquire the per-execution lock with a heartbeat lease so a crashed holder's
	// lock is reclaimable (stolen once stale) instead of orphaning the job forever.
	desc := fmt.Sprintf("executing job %s @ %s", j.ID, ownerToken)
	lock, err := jobsDB.Tenant(tenant).Lock(ctx, j, desc, convDB.WithLease(jobLease))
	if err != nil {
		ctx.Logger().Error("failed to acquire lock for job",
			"tenant", string(tenant),
			"job_id", string(j.ID),
			"error", err.Error(),
		)
		return
	}
	if lock == nil {
		// A live lock is held by another instance — skip this tick. Logged (not
		// silent) so a genuinely stuck job is visible in logs/alerts.
		ctx.Logger().Info("job skipped: live lock held by another instance",
			"tenant", string(tenant),
			"job_id", string(j.ID),
		)
		return
	}
	if lock.Stolen() {
		ctx.Logger().Warn("job lock stolen from an expired holder (previous owner likely crashed)",
			"tenant", string(tenant),
			"job_id", string(j.ID),
			"previous_owner", lock.PreviousOwner(),
		)
	}

	// Cancellable context for the job body + heartbeat. convCtx.Context embeds
	// context.Context, so copying the struct and replacing the embedded context
	// preserves all values while adding cancellation.
	jobCtx := ctx
	var jobCancel context.CancelFunc
	jobCtx.Context, jobCancel = context.WithCancel(ctx.Context)

	var leaseLost atomic.Bool
	hbDone := make(chan struct{})

	go func() {
		defer close(hbDone)
		ticker := time.NewTicker(jobRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				rerr := lock.Renew(jobCtx)
				if rerr == nil {
					continue
				}
				if renewOutcome(rerr) {
					// Confirmed lease loss: stop the job; the new owner takes over.
					leaseLost.Store(true)
					ctx.Logger().Error("job lost its lease mid-execution (stolen or expired)",
						"tenant", string(tenant),
						"job_id", string(j.ID),
					)
					jobCancel()
					return
				}
				if jobCtx.Err() == nil {
					// Transient error (e.g. DB blip): keep the heartbeat alive and
					// retry next tick; a real loss surfaces later as ErrLeaseLost.
					ctx.Logger().Warn("job lease renew failed (transient), will retry",
						"tenant", string(tenant),
						"job_id", string(j.ID),
						"error", rerr.Error(),
					)
				}
			}
		}
	}()

	// Teardown: stop the heartbeat, join it (leak-free — Renew uses ExecContext so
	// the cancel interrupts any in-flight renew), then owner-safe unlock.
	defer func() {
		jobCancel()
		<-hbDone
		switch unlockErr := lock.Unlock(); {
		case unlockErr == nil:
		case errors.Is(unlockErr, convDB.ErrLeaseLost):
			ctx.Logger().Warn("job lease already lost at unlock (stolen mid-run)",
				"tenant", string(tenant),
				"job_id", string(j.ID),
			)
		default:
			ctx.Logger().Error("failed to unlock job",
				"tenant", string(tenant),
				"job_id", string(j.ID),
				"error", unlockErr.Error(),
			)
		}
	}()

	// Execute job function with panic recovery. j.f receives jobCtx so a job that
	// honours context cancellation aborts promptly on lease loss. Capture the outcome so
	// the completion log fires only on genuine success (not on mere schedule advance).
	var jobErr error
	var jobPanicked bool
	startedAt := time.Now()
	func() {
		defer func() {
			if r := recover(); r != nil {
				jobPanicked = true
				ctx.Logger().Error("job panicked",
					"tenant", string(tenant),
					"job_id", string(j.ID),
					"panic", fmt.Sprintf("%v", r),
				)
			}
		}()

		if jerr := j.f(jobCtx); jerr != nil {
			jobErr = jerr
			ctx.Logger().Error("job execution failed",
				"tenant", string(tenant),
				"job_id", string(j.ID),
				"error", jerr.Error(),
			)
		}
	}()
	jobDuration := time.Since(startedAt)

	// Fast path: if the heartbeat already observed a confirmed loss, skip the advance.
	if leaseLost.Load() {
		ctx.Logger().Warn("skipping next_run_at advance: lease lost during execution",
			"tenant", string(tenant),
			"job_id", string(j.ID),
		)
		return
	}

	// Compute next run time, advancing past any missed intervals.
	now := time.Now().UTC()
	nextRunAt := j.NextRunAt.Add(j.RepeatEvery)
	for !nextRunAt.After(now) {
		nextRunAt = nextRunAt.Add(j.RepeatEvery)
	}

	// Persist the advance ONLY while we still hold a non-expired lease on this job. The object
	// write and the ownership check commit in a single statement (UpdateGuarded), so a run
	// whose lease was stolen or expired — even if that is only discovered as the write lands —
	// cannot advance next_run_at; there is no window between confirming ownership and writing.
	// UpdateGuarded is intentionally not ctx-cancellable, so it also lands (and decides
	// ownership) correctly during scheduler shutdown. ErrLeaseLost ⇒ the new owner schedules.
	updatedJob := j
	updatedJob.NextRunAt = nextRunAt
	switch err := lock.UpdateGuarded(ctx, updatedJob); {
	case err == nil:
		mut.Lock()
		if tenantJobs, ok := jobs[tenant]; ok {
			if memJob, ok := tenantJobs[j.ID]; ok {
				memJob.NextRunAt = nextRunAt
				tenantJobs[j.ID] = memJob
			}
		}
		mut.Unlock()

		// Completion — emitted ONLY on genuine success (job returned nil, did not panic, and
		// the guarded advance committed, i.e. we still owned the lease). Alerting keys off the
		// ABSENCE of this log per (service, tenant, job); a failed/panicked job must NOT emit it.
		if jobErr == nil && !jobPanicked {
			ctx.Logger().Info("job execution completed",
				"tenant", string(tenant),
				"job_id", string(j.ID),
				"duration_ms", jobDuration.Milliseconds(),
			)
		}
	case errors.Is(err, convDB.ErrLeaseLost):
		ctx.Logger().Warn("skipping next_run_at advance: lease not held at schedule write",
			"tenant", string(tenant),
			"job_id", string(j.ID),
		)
	default:
		ctx.Logger().Error("failed to update job next run time in database",
			"tenant", string(tenant),
			"job_id", string(j.ID),
			"error", err.Error(),
		)
	}
}
