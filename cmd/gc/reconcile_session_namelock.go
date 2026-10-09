package main

import (
	"strings"
	"sync"

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
// (stopStillBoundClosedRuntime) keys it. ok is false, and unlock nil, when
// the row has no runtime name or the name is busy; the caller refuses with
// causeNameBusy and never waits.
func lockRuntimeName(w *World, row session.Info) (name string, unlock func(), ok bool) {
	name = strings.TrimSpace(row.SessionName)
	if name == "" {
		return "", nil, false
	}
	if unlock = runtimeNames.tryLock(w.CityPath, name); unlock == nil {
		return name, nil, false
	}
	return name, unlock, true
}
