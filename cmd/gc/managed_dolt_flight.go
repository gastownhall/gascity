package main

import (
	"context"
	"sync"
	"time"
)

// managedDoltFlightWindow is the longest a caller waits for the managed-Dolt
// preflight (the provider health ladder and, when it finds Dolt down, the
// recover) before it moves on and leaves the recover running.
const managedDoltFlightWindow = 30 * time.Second

// managedDoltFlights holds the preflight recover that is running for a city,
// keyed by normalizePathForCompare(cityPath).
var managedDoltFlights sync.Map // normalized cityPath → *managedDoltFlight

// managedDoltFlight is one run of the managed-Dolt preflight.
type managedDoltFlight struct {
	started time.Time
	done    chan struct{}
	err     error
}

// joinOrStartManagedDoltFlight is a placeholder that starts nothing: it
// returns an already-finished flight and never calls run. The real registry
// replaces it.
func joinOrStartManagedDoltFlight(_ context.Context, _ string, _ func(context.Context) error) *managedDoltFlight {
	f := &managedDoltFlight{started: time.Now(), done: make(chan struct{})}
	close(f.done)
	return f
}

// wait is a placeholder that reports the flight finished at once.
func (f *managedDoltFlight) wait(_ context.Context) (finished bool, err error) {
	<-f.done
	return true, f.err
}

// managedDoltFlightStartedAt is a placeholder that reports no flight.
func managedDoltFlightStartedAt(_ string) (time.Time, bool) {
	return time.Time{}, false
}
