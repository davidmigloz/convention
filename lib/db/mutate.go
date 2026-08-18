package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

const (
	// mutateMaxAttempts bounds mutateLoop's read-modify-write retry loop
	// (shared by Mutate and MutateOrInsert).
	mutateMaxAttempts = 5
)

var (
	// mutateBackoffBase / mutateBackoffCap bound mutateLoop's backoff between
	// attempts: exponential doubling from base, then jittered — see
	// mutateBackoff. Vars, not consts, so export_test.go's
	// StubMutateBackoffForTest can shrink them to keep sleeping tests fast.
	mutateBackoffBase = 10 * time.Millisecond
	mutateBackoffCap  = 160 * time.Millisecond
)

// ErrObjectVanished is returned by mutateLoop's post-write re-read when
// SelectByID reports the row missing immediately after a successful
// SafeUpdate or Insert. The write committed, so this is deliberately not
// ErrObjectNotFound — that sentinel would invite a may-create caller to
// treat the row as never having existed and re-create it, when in fact
// it was deliberately removed out from under this call.
var ErrObjectVanished = errors.New("convention/db: object vanished immediately after mutate wrote it")

// mutateRetryable reports whether err is a conflict mutateLoop should retry:
// ErrCASConflict (lost the compare-and-swap) or ErrLockNotAvailable
// (Postgres NOWAIT contention), including wrapped forms. Anything else —
// notably ErrObjectNotFound or a fn error — is not retryable. SQLite (the
// test-only engine) never raises the Postgres NOWAIT SQLSTATE, so the
// ErrLockNotAvailable branch has no in-harness integration coverage beyond
// this unit test (mirrors update_test.go's lock_not_available_integration
// skip note). Scoped to SafeUpdate outcomes only — duplicate-key Insert
// races are classified separately, by isDuplicateInsertErr, and feed the
// same backoff tail through their own path in mutateLoop.
func mutateRetryable(err error) bool {
	return errors.Is(err, ErrCASConflict) || errors.Is(err, ErrLockNotAvailable)
}

// mutateBackoff returns the wait before retrying after the attempt-th failed
// attempt (1-based): exponential doubling from mutateBackoffBase, then
// jittered uniformly over [d/2, d) so concurrent retriers do not lock-step.
// mutateBackoffCap bounds the doubling but is headroom only: at the current
// mutateMaxAttempts (5), mutateLoop calls this with attempt 1..4, so d peaks
// at mutateBackoffBase<<3 (80ms) and the 160ms cap never actually engages —
// it exists in case mutateMaxAttempts is ever raised.
func mutateBackoff(attempt int) time.Duration {
	d := min(mutateBackoffBase<<(attempt-1), mutateBackoffCap)
	half := d / 2
	return half + rand.N(d-half)
}

// mutateReRead re-reads the object mutateLoop just wrote (via SafeUpdate or
// Insert), so callers observe the same value a subsequent SelectByID would
// — including compute-hook stamps, which SafeUpdate/Insert apply internally
// and never hand back to the caller. Routed by written's OWN shard key
// (DBKey().ShardKey), never by the caller's shardKeys hint: this helper is
// called after both the update-success and insert-success paths, and on the
// insert path a caller-supplied hint may not even cover the shard the write
// just landed on — routing by it here would miss the row and report a false
// ErrObjectVanished. A (nil, nil) SelectByID result here is reported as
// ErrObjectVanished, not ErrObjectNotFound — see ErrObjectVanished's doc
// comment for why.
func (tos TenantObjectSet[objT, idT, shardKeyT]) mutateReRead(ctx convCtx.Context, written objT) (obj objT, err error) {

	key := written.DBKey()

	var cur *objT
	cur, err = tos.SelectByID(ctx, key.ID, key.ShardKey)
	if err != nil {
		return
	}
	if cur == nil {
		err = fmt.Errorf("%w: id=%s", ErrObjectVanished, key.ID)
		return
	}

	obj = *cur
	return
}

// Mutate is a read-modify-write combinator over SafeUpdate for a row that
// must already exist: it loads the current row, hands it to fn, and retries
// the whole load→fn→SafeUpdate cycle (up to mutateMaxAttempts times)
// whenever the write loses the optimistic-concurrency race (ErrCASConflict)
// or finds the row's Postgres NOWAIT lock contended (ErrLockNotAvailable).
// Returns ErrObjectNotFound if the row does not exist — for a combinator
// that may also create the row, see MutateOrInsert. fn must not be nil (a
// nil fn is a plain error, checked before any I/O); unlike fn, a nil seed on
// MutateOrInsert is blessed — see its doc comment.
//
// Return contract: on success, obj is the value a subsequent SelectByID
// would observe, INCLUDING any WithCompute stamps — not fn's return value
// echoed back. SafeUpdate takes the object by value and stamps compute
// fields internally, so none of that stamping propagates back out of it;
// Mutate re-reads the row after a successful write to give an honest
// answer. The re-read is also the only faithful option on Postgres, where
// the JSONB column keeps nanosecond timestamps but the created_at/updated_at
// columns are microsecond-truncated — echoing fn's return value would drift
// from what a real subsequent read reports. On any error, obj is the zero
// value.
//
// Committed-write-with-error: an error return does not always mean nothing
// persisted. If the write itself committed but the post-write re-read
// failed (ErrObjectVanished, or a transient read error), the mutation IS
// durably applied even though Mutate returns an error — callers needing
// exactly-once effects must make fn idempotent, not assume "err != nil"
// implies "nothing happened".
//
// Freshness: the returned obj may already reflect a concurrent writer's
// later mutation — it is "what a subsequent SelectByID would observe", not
// "your write echoed back verbatim". A caller returning it directly in an
// HTTP response must not assume it is exactly what it just wrote.
//
// No-op skip (default behaviour): if fn returns an object byte-identical
// (canonical JSON marshal) to the row Mutate just read, Mutate performs no
// write at all — no SafeUpdate call, no re-read, no further attempts — and
// returns that row as of THIS read (not re-verified at return). updated_at
// is left untouched. Want a bump even when nothing meaningfully changed?
// Change a field before returning from fn — touch it to bump it. Note that
// the comparison is a plain marshal-byte compare: an fn that touches a
// WithCompute-owned field (e.g. re-stamps a timestamp a compute hook already
// owns) always looks changed to it, forcing a write even when business
// state is otherwise identical — the hooks then overwrite that touched
// field again on the way back out. Don't touch compute-owned fields from fn.
//
// A successful return does not prove a write happened: the no-op skip returns
// (row, nil) exactly as a real write does, so the (obj, err) pair carries no
// wrote/skipped signal. "Did I transition this row?" is unanswerable from
// Mutate's return alone — if a rival already stored precisely what fn
// produced, this call writes nothing and still succeeds. The CAS guard does
// not supply the answer either: it rejects a stale from, and a caller that
// read after the rival committed has a fresh one, so SafeUpdate proceeds and
// overwrites rather than conflicting. So when a transition implemented with
// Mutate has to have exactly one winner — a claim, an enqueue, a one-shot
// flag — decide that inside fn: re-check the row it is handed and return a
// sentinel error when another writer already owns it. An fn error aborts
// immediately, is never retried, and propagates unwrapped, so errors.Is
// reaches it at the call site.
//
// This is scoped to one optimistic mutation, which is all fn can guard. A
// holder that must own a row across several writes, or keep owning it while
// it works and survive its own crash, needs the lease lock instead —
// Lock(WithLease) + Renew + UpdateGuarded (lock.go) — which fn cannot
// substitute for: it sees one attempt and nothing after the call returns.
//
// fn contract: it may run up to mutateMaxAttempts times and must be pure or
// otherwise strictly retry-safe — no irreversible or externally visible side
// effects inside it. Those belong after Mutate returns success: persist
// first, then side-effect. That fn runs with no transaction open is an
// implementation detail useful for test machinery, not a licence for
// callers to do DB work inside fn. A fn error aborts immediately, with no
// retry.
//
// Before every fn call, the just-loaded row is deep-cloned via
// cloneViaJSON (marshal→unmarshal, same technique as SafeUpdate's own
// from-clone in update.go) to become SafeUpdate's `from` baseline. This
// guards against fn mutating its `cur` argument's slices/maps in place: a
// shallow copy would share backing arrays with that baseline and corrupt
// it, turning every attempt into a false conflict. As with SafeUpdate's
// clone, the marshal→unmarshal round-trip drops unexported fields
// (encoding/json's rule, not Mutate's) — same semantics callers already
// rely on. The clone happens before fn is invoked, so a MarshalJSON failure
// on it aborts immediately: fn is never invoked, and there is no retry.
//
// Retry backoff (mutateBackoff) is exponential from mutateBackoffBase,
// doubling each attempt, then jittered uniformly over [d/2, d). At the
// current mutateMaxAttempts (5) that produces 4 waits — attempts 1-4, none
// after the final attempt — of roughly [5,10) + [10,20) + [20,40) + [40,80)
// ms, jittered; mutateBackoffCap bounds the doubling but only engages if
// mutateMaxAttempts is ever raised. The wait is wall-clock (time.NewTimer),
// not ctx.Now()-driven — Mutate reasons about real contention, not
// simulated time. The wait is itself ctx-aware, using the house
// time.NewTimer + select-on-ctx.Done() pattern (as in lib/job's scheduler
// loop): cancellation observed at the top of a loop iteration, or during the
// backoff wait, returns errors.Join(ctx.Err(), lastErr), so callers can
// errors.Is for both context.Canceled and the conflict sentinel that
// triggered the wait. Exhausting mutateMaxAttempts returns the last conflict
// error wrapped with the attempt count (still errors.Is-matchable). That
// last conflict may be any of: ErrCASConflict, ErrLockNotAvailable,
// ErrDuplicateID (MutateOrInsert's insert branch), or ErrObjectNotFound
// (MutateOrInsert's delete-race absorption, below).
//
// Wrong shard-key hint on a must-exist Mutate: shardKeys is a query-routing
// hint for SelectByID only. A wrong hint makes an existing row look absent,
// so Mutate returns ErrObjectNotFound immediately — the same failure mode as
// a genuinely missing id. See MutateOrInsert's doc comment for the
// analogous, but self-healing, may-create case.
//
// SQLite (the test-only engine) never raises the Postgres NOWAIT SQLSTATE —
// see SafeUpdate's doc comment (update.go) for why cross-connection SQLite
// contention is not actually reachable in this package's test harness.
//
// No functional options: the variadic slot is already shardKeys; additive
// later if a caller need appears.
func (tos TenantObjectSet[objT, idT, shardKeyT]) Mutate(
	ctx convCtx.Context,
	id idT,
	fn func(cur objT) (objT, error),
	shardKeys ...shardKeyT,
) (obj objT, err error) {
	return tos.mutateLoop(ctx, id, nil, fn, shardKeys...)
}

// MutateOrInsert is Mutate for a row that may not exist yet: if id is
// missing, seed builds the initial object, fn is applied to it, and the
// result is inserted; if id already exists, MutateOrInsert behaves exactly
// like Mutate (seed is never called). fn thus runs on BOTH branches — put
// merge/validation logic in one place there — while seed's only job is
// producing the initial base for a not-yet-existing row. Both must be
// pure/retry-safe, same contract as Mutate's fn: no irreversible or
// externally visible side effects. seed may run once per insert-branch
// attempt (never on the update branch).
//
// A nil seed degrades MutateOrInsert to must-exist semantics — Mutate itself
// is exactly `mutateLoop(ctx, id, nil, fn, shardKeys...)`. A nil fn, unlike
// a nil seed, is not blessed: it is a plain error, checked before any I/O.
//
// Return contract, the no-op skip, freshness, and retry/backoff mechanics
// are identical to Mutate's — see its doc comment. The no-op skip applies
// only to the update branch; the insert branch always writes (there is
// nothing yet-persisted to compare a freshly-seeded row against).
//
// ID guard: on the insert branch, fn's returned object's DBKey().ID must
// equal id — unlike SafeUpdate, plain Insert has no ID guard of its own, so
// MutateOrInsert checks it. A mismatch aborts immediately with a plain
// error: no retry, no row written under either ID.
//
// ShardKey guard: on the insert branch, fn's returned object's
// DBKey().ShardKey must equal seed's — mirroring SafeUpdate's own from/to
// shard-mismatch rejection on the update branch, which this combinator's
// update branch already inherits. Without this guard, an fn that changes
// the shard-key field after seed would make Insert land on a shard other
// than the one SelectByID checked, and could create a true cross-shard
// duplicate. A mismatch aborts immediately with a plain error: no retry, no
// row written under either shard.
//
// Duplicate-key race: if another caller inserts the same id between this
// attempt's SelectByID and its own Insert, the resulting duplicate-key error
// (see isDuplicateInsertErr) is absorbed into the same mutateMaxAttempts
// budget and mutateBackoff wait as a CAS conflict, rather than surfaced
// as a raw driver error — the next attempt's SelectByID normally finds the
// row the other caller just created and converges through the ordinary
// update branch from there. Duplicate-key EXHAUSTION specifically is not
// deterministically constructible in a single-threaded test: after one
// collision, the next attempt's SelectByID necessarily finds the racer's
// row and moves to the update branch, so the exhaustion test exercises the
// CAS path instead (a racer Update on a pre-existing row) — same budget,
// same code path as Mutate's own exhaustion behaviour, reached through this
// wrapper.
//
// Delete race (the mirror image): if another caller deletes the row between
// this attempt's SelectByID and SafeUpdate's guarded read, the resulting
// ErrObjectNotFound is likewise absorbed into the same budget and backoff —
// our write never committed, so retrying into the insert branch is
// linearizable as "their delete, then our insert", ordinary upsert
// semantics. Only MutateOrInsert absorbs this; must-exist Mutate still
// aborts with ErrObjectNotFound, as its contract requires. This absorption
// assumes the race is transient: a permanently misfiled row — one whose
// ShardKey routes to a shard other than the one it actually lives on,
// unsupported-resharding territory — recurs on every attempt and simply
// exhausts the budget instead of converging.
//
// Wrong shard-key hint: shardKeys is a query-routing hint for SelectByID
// only, never validated against the object's real ShardKey. A wrong hint
// fails safely — it does not heal, and it does not corrupt: every attempt
// misses the existing row on SelectByID (routed by the hint), takes the
// insert branch, and Insert (which routes by the object's own ShardKey, not
// the hint) hits the row's real shard and duplicate-key-fails there —
// exhausting the budget with ErrDuplicateID, never creating a duplicate
// row. The one caller-bug this cannot defend against is seed itself
// building the object with a different ShardKey than the existing row's —
// but that specific case is now guarded too (see the ShardKey guard above):
// it aborts immediately instead of reaching Insert.
//
// SQL-NULL-object row (deliberately unresolved, not a bug): a runtime row
// whose "object" column is SQL NULL (an out-of-band state; see the
// package's live-object guard elsewhere) reads as absent to SelectByID, so
// every MutateOrInsert attempt takes the insert branch and collides with
// the still-present primary key — exhausting with ErrDuplicateID rather
// than reviving the row, even though a caller reading the id would see it
// as absent. This is intentional: silently resurrecting such a row via a
// may-create combinator would contradict the deliberate-delete rationale
// that shape defends elsewhere in this package (upstream issue #15 will
// forbid the state outright). Upsert, not MutateOrInsert, is the primitive
// that can overwrite a SQL-NULL-object row.
//
// Vanished row: like Mutate, a post-write re-read that comes back empty
// returns ErrObjectVanished — see its doc comment for why not
// ErrObjectNotFound.
func (tos TenantObjectSet[objT, idT, shardKeyT]) MutateOrInsert(
	ctx convCtx.Context,
	id idT,
	seed func() (objT, error),
	fn func(cur objT) (objT, error),
	shardKeys ...shardKeyT,
) (obj objT, err error) {
	return tos.mutateLoop(ctx, id, seed, fn, shardKeys...)
}

// mutateLoop is the shared read-modify-write retry loop behind Mutate
// (seed == nil) and MutateOrInsert (seed != nil) — see their doc comments
// for the full contract; this comment covers only loop shape. fn == nil is
// rejected up front (a plain error, no I/O attempted) — every other guard
// below runs only once its branch is reached, so this is the one check that
// would otherwise panic in a state-dependent way. One loop, one set of
// retry/backoff/cancellation/exhaustion mechanics, so the two entry points
// cannot drift apart: each attempt starts with SelectByID; a found row takes
// the update branch (clone baseline → fn → no-op skip → SafeUpdate); a
// missing row either aborts with ErrObjectNotFound (seed == nil) or takes
// the insert branch (seed → fn → ID guard → ShardKey guard → Insert, with
// duplicate-key races absorbed into the same backoff as a CAS conflict).
// Both success paths return through the shared mutateReRead helper so they
// cannot diverge on freshness.
func (tos TenantObjectSet[objT, idT, shardKeyT]) mutateLoop(
	ctx convCtx.Context,
	id idT,
	seed func() (objT, error),
	fn func(cur objT) (objT, error),
	shardKeys ...shardKeyT,
) (obj objT, err error) {

	if fn == nil {
		err = errors.New("convention/db: mutate requires a non-nil fn")
		return
	}

	var lastErr error

	for attempt := 1; attempt <= mutateMaxAttempts; attempt++ {

		if ctxErr := ctx.Err(); ctxErr != nil {
			if lastErr != nil {
				err = errors.Join(ctxErr, lastErr)
			} else {
				err = ctxErr
			}
			return
		}

		var cur *objT
		cur, err = tos.SelectByID(ctx, id, shardKeys...)
		if err != nil {
			return
		}

		if cur != nil {

			// Deep-clone the just-loaded row into `from` BEFORE calling fn —
			// see Mutate's doc comment for why (fn may mutate `cur` in
			// place).
			var from objT
			var raw []byte
			from, raw, err = cloneViaJSON(*cur)
			if err != nil {
				return
			}

			var to objT
			to, err = fn(*cur)
			if err != nil {
				return // fn error aborts immediately, no retry
			}

			// No-op skip (default): fn returning the row byte-identical to
			// what was just read performs no write. Compared against `raw`,
			// the same canonical marshal used for the CAS baseline, so map
			// key ordering cannot false-negative it.
			var toRaw []byte
			toRaw, err = json.Marshal(to)
			if err != nil {
				return
			}
			if string(toRaw) == string(raw) {
				// Return the pristine clone, not *cur: fn received *cur by
				// value, but its slice/map fields still alias cur's backing
				// arrays, so an fn that scratch-mutates them in place before
				// reconstructing an equal `to` must not have that mutation
				// leak into the result.
				obj = from
				return
			}

			err = tos.SafeUpdate(ctx, from, to)
			if err == nil {
				obj, err = tos.mutateReRead(ctx, to)
				return
			}

			// Delete race on the may-create path: see MutateOrInsert's doc
			// comment.
			retryable := mutateRetryable(err) || (seed != nil && errors.Is(err, ErrObjectNotFound))
			if !retryable {
				return
			}
			lastErr = err

		} else {

			if seed == nil {
				err = fmt.Errorf("%w: id=%s", ErrObjectNotFound, id)
				return
			}

			var base objT
			base, err = seed()
			if err != nil {
				return // seed error aborts immediately, no retry, no row
			}

			var to objT
			to, err = fn(base)
			if err != nil {
				return // fn error aborts immediately, no retry, no row
			}

			if to.DBKey().ID != id {
				err = fmt.Errorf("convention/db: mutate fn returned object with id=%s, want id=%s", to.DBKey().ID, id)
				return
			}

			if to.DBKey().ShardKey != base.DBKey().ShardKey {
				err = fmt.Errorf("convention/db: mutate fn returned object with shard_key=%s, want shard_key=%s (seed's)", to.DBKey().ShardKey, base.DBKey().ShardKey)
				return
			}

			err = tos.Insert(ctx, to)
			if err == nil {
				obj, err = tos.mutateReRead(ctx, to)
				return
			}

			// Duplicate-key insert race: see MutateOrInsert's doc comment.
			// Insert already classifies + wraps this as ErrDuplicateID
			// (id and underlying driver error included), so there is
			// nothing left to do here but absorb it into the retry budget.
			if !errors.Is(err, ErrDuplicateID) {
				return // other Insert errors abort, no retry
			}
			lastErr = err
		}

		if attempt == mutateMaxAttempts {
			break
		}

		wait := mutateBackoff(attempt)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			err = errors.Join(ctx.Err(), lastErr)
			return
		case <-timer.C:
		}
	}

	err = fmt.Errorf("convention/db: mutate exhausted %d attempts: %w", mutateMaxAttempts, lastErr)
	return
}
