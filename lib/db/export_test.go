package db

import (
	"testing"
	"time"
)

// StubMutateBackoffForTest shrinks mutateBackoffBase/mutateBackoffCap to
// microseconds for the duration of t, restoring the originals via
// t.Cleanup. Tests that deliberately exhaust mutateLoop's retry budget (or
// otherwise force at least one backoff wait) sleep through the real
// millisecond-scale backoff by default; this hook keeps them fast without
// touching the retry/backoff logic itself — invocation counts are unaffected
// (precedent: lib/job's PinSingleConnForTest test hook).
func StubMutateBackoffForTest(t *testing.T) {
	t.Helper()
	origBase, origCap := mutateBackoffBase, mutateBackoffCap
	mutateBackoffBase = 1 * time.Microsecond
	mutateBackoffCap = 16 * time.Microsecond
	t.Cleanup(func() {
		mutateBackoffBase = origBase
		mutateBackoffCap = origCap
	})
}
