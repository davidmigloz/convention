package db_test

import (
	"sync"
	"testing"

	convDB "github.com/sofmon/convention/lib/db"
)

// Test_concurrent_first_use_of_object_sets_does_not_race pins the boot-time
// invariant: many goroutines making the first call on object sets of a
// not-yet-registered type must not corrupt the package's type registry.
//
// objectSet.prepare() reads and writes that registry (typeToTable, a map of
// maps) from whichever goroutine issues the first query. At service boot
// several handlers do exactly that at once, and the unguarded map access
// surfaced as "fatal error: concurrent map writes" in a few services per
// rollout. Close() empties the registry, so every set below starts
// unregistered; the sets span two vaults so both the per-vault map creation
// and the per-type registration are exercised. The failure is a data race:
// run with -race.
func Test_concurrent_first_use_of_object_sets_does_not_race(t *testing.T) {

	if err := convDB.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if err := convDB.Open(); err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const goroutines = 32

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				errs <- messagesDB.Tenant("test").EnsurePrepared()
			} else {
				errs <- complexDB.Tenant("test").EnsurePrepared()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("EnsurePrepared failed: %v", err)
		}
	}
}
