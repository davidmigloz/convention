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
		existing.f = fn
		existing.RepeatEvery = repeatEvery
		// Honour the persisted schedule when present so a redeploy doesn't reset
		// every job's clock; fall back to startAt only when there is no DB row.
		if savedJob != nil && !savedJob.NextRunAt.IsZero() {
			existing.NextRunAt = savedJob.NextRunAt
		} else {
			existing.NextRunAt = startAt
		}
		jobs[tenant][jid] = existing

		// Persist a changed interval. The pre-fix code updated RepeatEvery in
		// memory only, which the next syncJobsFromDB then reverted.
		if savedJob != nil && savedJob.RepeatEvery != repeatEvery {
			updated := *savedJob
			updated.RepeatEvery = repeatEvery
			if err = jobsDB.Tenant(tenant).Update(ctx, updated); err != nil {
				return
			}
		}

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
		if err = jobsDB.Tenant(tenant).Insert(ctx, nj); err != nil {
			return
		}
		jobs[tenant][jid] = nj
		wake()
		return
	}

	// DB row exists but no in-memory entry yet (first Register after restart):
	// keep the persisted NextRunAt as the schedule, attach the closure, and persist
	// a changed interval.
	nj := *savedJob
	nj.f = fn
	if savedJob.RepeatEvery != repeatEvery {
		nj.RepeatEvery = repeatEvery
		if err = jobsDB.Tenant(tenant).Update(ctx, nj); err != nil {
			return
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
	// honours context cancellation aborts promptly on lease loss.
	func() {
		defer func() {
			if r := recover(); r != nil {
				ctx.Logger().Error("job panicked",
					"tenant", string(tenant),
					"job_id", string(j.ID),
					"panic", fmt.Sprintf("%v", r),
				)
			}
		}()

		if err := j.f(jobCtx); err != nil {
			ctx.Logger().Error("job execution failed",
				"tenant", string(tenant),
				"job_id", string(j.ID),
				"error", err.Error(),
			)
		}
	}()

	// If the lease was lost mid-run, another instance now owns scheduling — do NOT
	// advance/persist next_run_at (that would double-advance the schedule).
	if leaseLost.Load() {
		ctx.Logger().Warn("skipping next_run_at advance: lease lost during execution",
			"tenant", string(tenant),
			"job_id", string(j.ID),
		)
		return
	}

	// Compute next run time, advancing past any missed intervals
	now := time.Now().UTC()
	nextRunAt := j.NextRunAt.Add(j.RepeatEvery)
	for !nextRunAt.After(now) {
		nextRunAt = nextRunAt.Add(j.RepeatEvery)
	}

	// Update in-memory state
	mut.Lock()
	if tenantJobs, ok := jobs[tenant]; ok {
		if memJob, ok := tenantJobs[j.ID]; ok {
			memJob.NextRunAt = nextRunAt
			tenantJobs[j.ID] = memJob
		}
	}
	mut.Unlock()

	// Update database state
	updatedJob := j
	updatedJob.NextRunAt = nextRunAt
	if err := jobsDB.Tenant(tenant).Update(ctx, updatedJob); err != nil {
		ctx.Logger().Error("failed to update job next run time in database",
			"tenant", string(tenant),
			"job_id", string(j.ID),
			"error", err.Error(),
		)
	}
}
