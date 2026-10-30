package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// admissionTestWait is the bounded wait used by the concurrency tests. It is
// generous enough that a free slot is granted promptly under -race yet short
// enough that the saturation test fails fast.
const admissionTestWait = 5 * time.Second

// TestBdAdmissionGlobalCapBoundsConcurrency asserts the global semaphore
// never admits more than its cap concurrently, even under a flood of
// goroutines across many scopes. Run with -race.
func TestBdAdmissionGlobalCapBoundsConcurrency(t *testing.T) {
	const global = 4
	a := newBdAdmission("/city", 0 /* per-scope disabled */, global, admissionTestWait)

	var concurrent atomic.Int64
	var peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			scope := "/city/rig" + string(rune('a'+n%8))
			release, ok := a.acquire(scope)
			if !ok {
				t.Errorf("acquire timed out under load with a generous wait")
				return
			}
			cur := concurrent.Add(1)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			concurrent.Add(-1)
			release()
		}(i)
	}
	wg.Wait()

	if got := peak.Load(); got > global {
		t.Fatalf("peak concurrent admissions = %d, exceeds global cap %d", got, global)
	}
	if got := a.inflightCount(); got != 0 {
		t.Fatalf("inflightCount() = %d after all releases, want 0", got)
	}
}

// TestBdAdmissionPerScopeCapBoundsConcurrency asserts each scope's
// semaphore independently bounds concurrency to the per-scope cap.
func TestBdAdmissionPerScopeCapBoundsConcurrency(t *testing.T) {
	const perScope = 2
	a := newBdAdmission("/city", perScope, 0 /* global disabled */, admissionTestWait)

	peaks := make(map[string]*atomic.Int64)
	curs := make(map[string]*atomic.Int64)
	scopes := []string{"/city/rig-a", "/city/rig-b", "/city/rig-c"}
	for _, s := range scopes {
		peaks[s] = &atomic.Int64{}
		curs[s] = &atomic.Int64{}
	}

	var wg sync.WaitGroup
	for i := 0; i < 300; i++ {
		scope := scopes[i%len(scopes)]
		wg.Add(1)
		go func(scope string) {
			defer wg.Done()
			release, ok := a.acquire(scope)
			if !ok {
				t.Errorf("acquire timed out under load with a generous wait")
				return
			}
			cur := curs[scope].Add(1)
			for {
				old := peaks[scope].Load()
				if cur <= old || peaks[scope].CompareAndSwap(old, cur) {
					break
				}
			}
			curs[scope].Add(-1)
			release()
		}(scope)
	}
	wg.Wait()

	for _, s := range scopes {
		if got := peaks[s].Load(); got > perScope {
			t.Fatalf("scope %s peak = %d, exceeds per-scope cap %d", s, got, perScope)
		}
	}
}

// TestBdAdmissionUnboundedWhenCapsDisabled asserts that non-positive caps
// admit everything without blocking (the breaker-disabled equivalent).
func TestBdAdmissionUnboundedWhenCapsDisabled(t *testing.T) {
	a := newBdAdmission("/city", 0, 0, admissionTestWait)
	releases := make([]func(), 0, 50)
	for i := 0; i < 50; i++ {
		release, ok := a.acquire("/city/rig")
		if !ok {
			t.Fatalf("acquire %d not admitted with caps disabled", i)
		}
		releases = append(releases, release)
	}
	if got := a.inflightCount(); got != 50 {
		t.Fatalf("inflightCount() = %d with caps disabled, want 50 (all admitted)", got)
	}
	for _, r := range releases {
		r()
	}
	if got := a.inflightCount(); got != 0 {
		t.Fatalf("inflightCount() = %d after releases, want 0", got)
	}
}

// TestBdInflightForCityUnknownCityIsZero asserts the gauge accessor is
// safe before any admission controller has been created for a city.
func TestBdInflightForCityUnknownCityIsZero(t *testing.T) {
	if got := bdInflightForCity("/no/such/city/ever"); got != 0 {
		t.Fatalf("bdInflightForCity(unknown) = %d, want 0", got)
	}
}

// TestBdAdmissionFailsFastUnderSaturation asserts that once the global cap is
// saturated by callers that never release (simulating bd subprocesses wedged
// on a transport timeout), the next acquire returns not-admitted within ~the
// bounded wait rather than blocking the controller tick forever.
func TestBdAdmissionFailsFastUnderSaturation(t *testing.T) {
	const global = 2
	wait := 50 * time.Millisecond
	a := newBdAdmission("/city", 0 /* per-scope disabled */, global, wait)

	// Saturate the global cap and hold the slots (never release).
	for i := 0; i < global; i++ {
		if _, ok := a.acquire("/city/rig"); !ok {
			t.Fatalf("acquire %d should be admitted before saturation", i)
		}
	}

	start := time.Now()
	release, ok := a.acquire("/city/rig")
	elapsed := time.Since(start)
	if ok {
		release()
		t.Fatalf("acquire should fail fast under saturation, but was admitted")
	}
	if elapsed < wait {
		t.Fatalf("acquire returned in %v, before the bounded wait %v elapsed", elapsed, wait)
	}
	if elapsed > 10*wait {
		t.Fatalf("acquire took %v, far longer than the bounded wait %v (did it block?)", elapsed, wait)
	}
	if got := a.inflightCount(); got != global {
		t.Fatalf("inflightCount() = %d after a timed-out acquire, want %d (timeout must not count)", got, global)
	}
}

// TestBdAdmissionReleasesGlobalOnScopeTimeout asserts that when the global
// slot is granted but the per-scope slot times out, the already-acquired
// global slot is released so it does not leak.
func TestBdAdmissionReleasesGlobalOnScopeTimeout(t *testing.T) {
	const global = 4
	const perScope = 1
	wait := 50 * time.Millisecond
	a := newBdAdmission("/city", perScope, global, wait)

	// Saturate the per-scope cap for one scope, holding the slot.
	if _, ok := a.acquire("/city/rig-a"); !ok {
		t.Fatalf("first acquire on rig-a should be admitted")
	}

	// A second acquire on the same scope grabs a global slot, then times out
	// on the saturated per-scope slot. The global slot must be released.
	release, ok := a.acquire("/city/rig-a")
	if ok {
		release()
		t.Fatalf("second acquire on the saturated scope should time out")
	}

	// All global slots except the one held by the first acquire must be free:
	// a different, uncontended scope must be admittable immediately.
	got := make(chan bool, 1)
	go func() {
		r, ok := a.acquire("/city/rig-b")
		if ok {
			r()
		}
		got <- ok
	}()
	select {
	case ok := <-got:
		if !ok {
			t.Fatalf("rig-b acquire not admitted: a global slot leaked on the scope timeout")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("rig-b acquire blocked: a global slot leaked on the scope timeout")
	}
}

// TestBdAdmissionBlocksForeverWhenWaitNonPositive asserts that a non-positive
// maxWait preserves the pre-bound opt-out: acquire blocks until a slot frees
// rather than failing fast.
func TestBdAdmissionBlocksForeverWhenWaitNonPositive(t *testing.T) {
	const global = 1
	a := newBdAdmission("/city", 0, global, 0 /* block forever */)

	release, ok := a.acquire("/city/rig")
	if !ok {
		t.Fatalf("first acquire should be admitted")
	}

	admitted := make(chan struct{})
	go func() {
		r, ok := a.acquire("/city/rig")
		if ok {
			r()
		}
		close(admitted)
	}()

	// The second acquire must still be blocked after a wait that would have
	// tripped any bounded timeout.
	select {
	case <-admitted:
		t.Fatalf("acquire returned while saturated; non-positive wait must block forever")
	case <-time.After(200 * time.Millisecond):
	}

	// Freeing the held slot lets the blocked acquire proceed.
	release()
	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		t.Fatalf("acquire did not proceed after the slot was freed")
	}
}
