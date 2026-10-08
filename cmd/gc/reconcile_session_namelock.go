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

// lockRuntimeName takes the name lock on row's runtime name in w's city,
// keyed as the legacy reaper keys it (stopStillBoundClosedRuntime: the city
// path and the runtime's session name), so every v2 effect and the reaper
// exclude one another. It returns the name, and a nil unlock when the name
// is busy.
func lockRuntimeName(w *World, row session.Info) (name string, unlock func()) {
	name = strings.TrimSpace(row.SessionName)
	return name, runtimeNames.tryLock(w.CityPath, name)
}

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
