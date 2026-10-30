package main

import (
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// bdAdmissionScope resolves the admission-semaphore scope key for a bd call,
// mirroring bdScopeBreaker: an empty dir snaps to the city scope so the
// per-scope and breaker keys agree.
func bdAdmissionScope(cityPath, dir string) string {
	scope := strings.TrimSpace(dir)
	if scope == "" {
		scope = cityPath
	}
	return scope
}

// bdAdmission is the process-wide subprocess admission controller for bd
// CLI calls (city-scale architecture plan item 1.9). It bounds the number
// of concurrent bd subprocesses two ways: a per-scope cap and a city-wide
// global cap. Together they put a hard ceiling on the subprocess amplifier
// (≥108 spawns/min at idle, 800/tick at 100 idle sessions) so a wedged
// backend or a fan-out burst cannot pile up unbounded processes.
//
// The semaphores are buffered channels: acquiring sends a token, releasing
// receives it. A zero-capacity cap means "unbounded" (the gate is skipped).
// gc_bd_inflight is a live gauge of currently-admitted bd calls, surfaced
// through BeadsDiagnostic for status observability.
type bdAdmission struct {
	mu       sync.Mutex
	cityPath string
	perScope int
	global   int
	// maxWait bounds how long acquire blocks for a free slot before failing
	// fast. A non-positive value means "block forever" (the pre-bound
	// behavior, opt-in via config). See acquire.
	maxWait  time.Duration
	globalCh chan struct{}
	scopeChs map[string]chan struct{}
	inflight atomic.Int64
}

// bdAdmissionRegistry holds one admission controller per city, created on
// first use with that city's [beads.resilience] caps. The caps are read
// once per process; changing them requires a controller restart (matching
// the breaker-registry lifetime).
var bdAdmissionRegistry = struct {
	mu          sync.Mutex
	controllers map[string]*bdAdmission
}{controllers: make(map[string]*bdAdmission)}

// bdAdmissionForCity returns the city's admission controller, creating it
// from the city's configured caps on first use.
func bdAdmissionForCity(cityPath string) *bdAdmission {
	key := filepath.Clean(cityPath)
	bdAdmissionRegistry.mu.Lock()
	if a, ok := bdAdmissionRegistry.controllers[key]; ok {
		bdAdmissionRegistry.mu.Unlock()
		return a
	}
	bdAdmissionRegistry.mu.Unlock()

	perScope, global, wait := bdAdmissionCapsForCity(key)
	bdAdmissionRegistry.mu.Lock()
	defer bdAdmissionRegistry.mu.Unlock()
	if a, ok := bdAdmissionRegistry.controllers[key]; ok {
		return a
	}
	a := newBdAdmission(key, perScope, global, wait)
	bdAdmissionRegistry.controllers[key] = a
	return a
}

// bdAdmissionCapsForCity resolves the per-scope and global bd inflight caps
// plus the bounded admission wait from the city's [beads.resilience] config,
// falling back to the defaults (4, 16, and 30s) when the config cannot be
// loaded.
func bdAdmissionCapsForCity(cityPath string) (perScope, global int, maxWait time.Duration) {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil || cfg == nil {
		return 4, 16, 30 * time.Second
	}
	r := cfg.Beads.Resilience
	return r.MaxInflightPerScopeOrDefault(), r.MaxInflightGlobalOrDefault(), r.MaxAdmissionWaitOrDefault()
}

// newBdAdmission constructs an admission controller. A non-positive cap
// leaves the corresponding semaphore nil (unbounded). A non-positive maxWait
// makes acquire block forever (the pre-bound behavior).
func newBdAdmission(cityPath string, perScope, global int, maxWait time.Duration) *bdAdmission {
	a := &bdAdmission{
		cityPath: cityPath,
		perScope: perScope,
		global:   global,
		maxWait:  maxWait,
		scopeChs: make(map[string]chan struct{}),
	}
	if global > 0 {
		a.globalCh = make(chan struct{}, global)
	}
	return a
}

// scopeChannel returns the per-scope semaphore channel for a scope root,
// creating it on first use. Returns nil when the per-scope cap is disabled.
func (a *bdAdmission) scopeChannel(scope string) chan struct{} {
	if a.perScope <= 0 {
		return nil
	}
	scope = filepath.Clean(scope)
	a.mu.Lock()
	defer a.mu.Unlock()
	ch, ok := a.scopeChs[scope]
	if !ok {
		ch = make(chan struct{}, a.perScope)
		a.scopeChs[scope] = ch
	}
	return ch
}

// acquire admits one bd call for a scope. It returns (release, true) when
// both the global and the per-scope semaphore granted a slot, where release
// is a func the caller MUST invoke (typically via defer) when the call
// returns. It returns (nil, false) when admission could not be granted
// within the bounded wait, so the caller can fail fast exactly like an open
// breaker instead of blocking the controller tick on a saturated, wedged
// backend.
//
// Acquisition order is global-then-scope; release is scope-then-global, the
// reverse order, so the two semaphores cannot deadlock against each other.
// Each send waits at most a.maxWait for a free slot; if the GLOBAL slot is
// granted but the per-scope slot times out, the already-acquired global slot
// is released before returning failure so no slot leaks. A non-positive
// a.maxWait blocks forever (the pre-bound opt-out). The inflight gauge is
// incremented only AFTER both acquisitions succeed, so it reflects
// admitted-and-running calls and never counts a timed-out attempt.
func (a *bdAdmission) acquire(scope string) (func(), bool) {
	globalCh := a.globalCh
	scopeCh := a.scopeChannel(scope)
	if !a.send(globalCh) {
		return nil, false
	}
	if !a.send(scopeCh) {
		// Scope slot timed out after the global slot was granted: release
		// the global slot so it does not leak, then signal not-admitted.
		if globalCh != nil {
			<-globalCh
		}
		return nil, false
	}
	a.inflight.Add(1)
	return func() {
		a.inflight.Add(-1)
		if scopeCh != nil {
			<-scopeCh
		}
		if globalCh != nil {
			<-globalCh
		}
	}, true
}

// send acquires one slot on ch, returning true on success. A nil channel
// (disabled cap) always succeeds. With a positive a.maxWait the send fails
// fast after the wait elapses; with a non-positive wait it blocks forever.
func (a *bdAdmission) send(ch chan struct{}) bool {
	if ch == nil {
		return true
	}
	if a.maxWait <= 0 {
		ch <- struct{}{}
		return true
	}
	timer := time.NewTimer(a.maxWait)
	defer timer.Stop()
	select {
	case ch <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

// inflightCount returns the number of currently-admitted bd calls. Used by
// the gc_bd_inflight gauge surfaced through BeadsDiagnostic.
func (a *bdAdmission) inflightCount() int {
	return int(a.inflight.Load())
}

// bdInflightForCity returns the city's current admitted bd-call count for
// the gc_bd_inflight gauge. Safe to call before any admission controller
// has been created (returns 0).
func bdInflightForCity(cityPath string) int {
	key := filepath.Clean(cityPath)
	bdAdmissionRegistry.mu.Lock()
	a, ok := bdAdmissionRegistry.controllers[key]
	bdAdmissionRegistry.mu.Unlock()
	if !ok {
		return 0
	}
	return a.inflightCount()
}
