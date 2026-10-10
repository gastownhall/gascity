package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// runtimeNameLocks serializes, per runtime name, the v2 start effect's
// provider Start with a runtime-keyed reaper's identity re-read and Stop
// (P4 F14). Named sessions keep stable runtime names, so without it a reaper
// that read a closed row's GC_SESSION_ID could stop the fresh runtime a new
// start put under the same name between that read and its Stop. The lock is
// process-wide because the reapers run on the legacy maintenance path, and it
// is never held across anything but the one name's provider calls. It is
// keyed by city as well as name: a supervisor runs several cities, whose
// runtime names may coincide.
type runtimeNameLocks struct {
	mu   sync.Mutex
	held map[runtimeNameKey]bool
}

// runtimeNameKey is one city's runtime name.
type runtimeNameKey struct{ city, name string }

var runtimeNames = &runtimeNameLocks{held: make(map[runtimeNameKey]bool)}

// tryLock takes city's lock on name if it is free, or returns nil. A reaper
// never waits: it skips the name, and the next pass reconsiders it.
func (l *runtimeNameLocks) tryLock(city, name string) (unlock func()) {
	k := runtimeNameKey{city, name}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[k] {
		return nil
	}
	l.held[k] = true
	return func() {
		l.mu.Lock()
		delete(l.held, k)
		l.mu.Unlock()
	}
}

// tryRuntimeLease takes cityPath's runtime lease on name for a legacy
// starter or stopper (I-LEASE): the in-process name lock, then the session
// runtime lease, which is the name's flock and, when id is set, the record on
// that open row. It never waits: a busy name is session.ErrRuntimeLeaseBusy,
// and the caller defers. A city path that is not absolute has no runtime dir
// to lock in: ErrRuntimeLeaseNoCity. release is idempotent.
func tryRuntimeLease(store beads.Store, cityPath, name, id string, ttl time.Duration) (lease *session.RuntimeLease, release func(), err error) {
	if !filepath.IsAbs(cityPath) {
		return nil, nil, session.RefuseWithoutCity(cityPath, fmt.Sprintf("runtime %q", name))
	}
	unlock := runtimeNames.tryLock(cityPath, name)
	if unlock == nil {
		return nil, nil, &session.RuntimeLeaseBusyError{Name: name, Holder: "this process", Local: true}
	}
	var front *session.Store
	if store != nil && id != "" {
		front = sessionFrontDoor(store)
	} else {
		id = ""
	}
	if lease, err = session.TryRuntimeLease(front, session.RuntimeLeaseRequest{City: cityPath, Name: name, ID: id, TTL: ttl}); err != nil {
		unlock()
		return nil, nil, err
	}
	var once sync.Once
	return lease, func() { once.Do(func() { lease.Release(); unlock() }) }, nil
}

// controllerStopLease takes, without waiting, the runtime lease a controller
// stop sequence holds across all its kills (a kill, its confirm-dead
// re-kills, an escalation's process-table kill), and returns ctx carrying it
// to each. A lease failure other than busy (the store unreachable, the row
// closed) is logged, and the sequence runs under the name's flock alone: a
// store never holds a stop hostage. Busy is session.ErrRuntimeLeaseBusy, which
// the caller defers to a later tick. A city path that is not absolute has no
// runtime dir to lock in: the stop refuses with session.ErrRuntimeLeaseNoCity
// and kills nothing.
func controllerStopLease(store beads.Store, cityPath, name, sessionID string, stderr io.Writer) (context.Context, func(), error) {
	if !filepath.IsAbs(cityPath) {
		return nil, func() {}, session.RefuseWithoutCity(cityPath, fmt.Sprintf("the controller's stop of runtime %q", name))
	}
	lease, release, err := tryRuntimeLease(store, cityPath, name, sessionID, session.RuntimeLeaseTTL(0))
	if err != nil && !errors.Is(err, session.ErrRuntimeLeaseBusy) {
		fmt.Fprintf(stderr, "session reconciler: stopping %s under the name's flock alone: %v\n", name, err) //nolint:errcheck
		lease, release, err = tryRuntimeLease(nil, cityPath, name, "", 0)
	}
	if err != nil {
		return nil, func() {}, err
	}
	return session.ContextWithRuntimeLease(session.WithoutLeaseWait(context.Background()), lease), release, nil
}

// The v2 effects' shared refusal causes for a row's runtime. A refusal backs
// the row off (P4).
//
//nolint:unused // the wave-7 effects (C4c2, C5a1, C5c1, C5d, C6a) refuse with them
const (
	causeNameBusy        = "name-busy"        // another effect, or a reaper, holds the runtime name
	causeRouteUnknown    = "route-unknown"    // no backend resolves the runtime name
	causeLivenessUnknown = "liveness-unknown" // the fresh liveness read was incomplete or failed
	causeNotPresent      = "not-present"      // the runtime the intent acts on is gone
)

// lockRuntimeName takes w's city lock on row's runtime name, as every v2
// effect that reads or calls the provider for a row takes it: keyed by the
// city path and the runtime name (Info.SessionName), as the legacy reaper
// (stopStillBoundClosedRuntime) keys it. It is tryRuntimeLease without the
// row's record, which PR B adds with the record's epoch in the premise. ok is
// false, and unlock nil, when the row has no runtime name or the name is
// busy; the caller refuses with causeNameBusy and never waits.
func lockRuntimeName(w *World, row session.Info) (name string, unlock func(), ok bool) {
	name = strings.TrimSpace(row.SessionName)
	if name == "" {
		return "", nil, false
	}
	if _, unlock, err := tryRuntimeLease(nil, w.CityPath, name, "", 0); err == nil {
		return name, unlock, true
	}
	return name, nil, false
}
