package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// managedDoltFlightWindow is the longest a caller waits for the managed-Dolt
// preflight (the provider health ladder and, when it finds Dolt down, the
// recover) before it moves on and leaves the recover running. The window runs
// from the flight's start, not from each caller's arrival.
const managedDoltFlightWindow = 30 * time.Second

// managedDoltFlights holds the preflight flight that is running for a city,
// keyed by normalizePathForCompare(cityPath). It is process-wide rather than a
// CityRuntime field so that code holding only a cityPath can join or read it.
var managedDoltFlights sync.Map // normalized cityPath → *managedDoltFlight

// managedDoltFlight is one run of the managed-Dolt preflight. It runs on a
// goroutine of its own, under the context of the caller that started it, so a
// caller that stops waiting for it leaves it running. At most one runs per
// city.
type managedDoltFlight struct {
	started time.Time
	done    chan struct{} // closed once the flight has ended and left the registry
	err     error         // what the flight function returned; set before done closes

	// expiryLogged records that a caller has already logged this flight outliving
	// managedDoltFlightWindow, so one slow recover is one line, not one per caller.
	expiryLogged atomic.Bool
}

// joinOrStartManagedDoltFlight returns the flight running for cityPath,
// starting one that calls run under ctx when none is. A caller that joins a
// running flight does not change the context it runs under. No lock is held
// while run executes, so run may itself consult the registry. The flight has
// left the registry before anyone can see it end, so the next call after that
// starts a fresh one.
func joinOrStartManagedDoltFlight(ctx context.Context, cityPath string, run func(context.Context) error) *managedDoltFlight {
	key := normalizePathForCompare(cityPath)
	flight := &managedDoltFlight{started: time.Now(), done: make(chan struct{})}
	if running, loaded := managedDoltFlights.LoadOrStore(key, flight); loaded {
		return running.(*managedDoltFlight)
	}
	go func() {
		defer func() {
			managedDoltFlights.CompareAndDelete(key, flight)
			close(flight.done)
		}()
		flight.err = run(ctx)
	}()
	return flight
}

// wait blocks until the flight ends or ctx does. It reports finished with the
// error the flight ended with, or (false, nil) when ctx ended first; the flight
// keeps running either way.
func (f *managedDoltFlight) wait(ctx context.Context) (finished bool, err error) {
	select {
	case <-f.done:
		return true, f.err
	case <-ctx.Done():
	}
	// Both may be ready at once; a flight that has ended reports as ended.
	select {
	case <-f.done:
		return true, f.err
	default:
		return false, nil
	}
}

// managedDoltFlightStartedAt reports when the flight running for cityPath
// started. It never blocks and never starts a flight; ok is false when none is
// running.
func managedDoltFlightStartedAt(cityPath string) (started time.Time, ok bool) {
	running, ok := managedDoltFlights.Load(normalizePathForCompare(cityPath))
	if !ok {
		return time.Time{}, false
	}
	return running.(*managedDoltFlight).started, true
}
