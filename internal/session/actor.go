package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CityDir is a city's directory: absolute and existing when it was made. The
// zero value names no city; only NewCityDir makes one. A path NewCityDir
// refused is kept for the refusal's message, and names no city either.
type CityDir struct{ abs, rejected string }

// NewCityDir validates path as a city directory: absolute, and an existing
// directory. On error the CityDir names no city and keeps path as refused.
func NewCityDir(path string) (CityDir, error) {
	path = strings.TrimSpace(path)
	refused := CityDir{rejected: path}
	if !filepath.IsAbs(path) {
		return refused, fmt.Errorf("city dir %q: not an absolute path", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return refused, fmt.Errorf("city dir: %w", err)
	}
	if !fi.IsDir() {
		return refused, fmt.Errorf("city dir %q: not a directory", path)
	}
	return CityDir{abs: filepath.Clean(path)}, nil
}

// Path is the city's absolute path, "" for the zero CityDir.
func (d CityDir) Path() string { return d.abs }

// IsZero reports whether d names no city.
func (d CityDir) IsZero() bool { return d.abs == "" }

// String is d's path, or the path NewCityDir refused, for messages.
func (d CityDir) String() string {
	if d.abs != "" {
		return d.abs
	}
	return d.rejected
}

// ActorKind is who calls a Manager lifecycle verb. It decides the runtime
// lease mode and whether the call may resume a held row.
type ActorKind int

const (
	// ActorOperator is a human's own command (`gc session attach`, an API
	// request carrying resume: true). It waits up to
	// RuntimeLeaseOperatorWait for the lease (O3), and its resume consumes
	// an operator's hold (CONTRACT v5.9 D8).
	ActorOperator ActorKind = iota + 1
	// ActorAgent is a caller on an agent's behalf (a nudge, mail delivery,
	// a worker start). It waits for the lease as an operator does, but
	// starts or resumes only a row nothing holds.
	ActorAgent
	// ActorBackground is a caller beside a controller (the API without
	// resume: true). It waits for the lease as an operator does, but never
	// starts a dormant row itself: the message queues, and the caller
	// records the wake for the controller (Store.RequestWakeUnlessHeld).
	ActorBackground
	// ActorController is the controller: it never waits for the lease, and
	// defers on ErrRuntimeLeaseBusy. It resumes only a row nothing holds.
	ActorController
	// ActorSweep is a stop-every-session sweep (`gc stop`, a rig restart):
	// its stops take no runtime lease, by design.
	ActorSweep
)

// Actor is the caller of a Manager lifecycle verb: who it is, the city whose
// runtime dir holds the lease, and a lease it already holds. ctx carries only
// cancellation.
type Actor struct {
	Kind ActorKind
	// City is required wherever the verb takes the runtime lease: a zero
	// City refuses with ErrRuntimeLeaseNoCity.
	City CityDir
	// Lease is a runtime lease the caller already holds on the session the
	// verb starts or stops: the verb takes none of its own, and refuses one
	// held on another session or runtime.
	Lease *RuntimeLease
}

// ErrNoActor refuses a lifecycle verb called with no ActorKind.
var ErrNoActor = errors.New("session: the lifecycle verb names no actor")

// ErrSweepStarts refuses a start by a stop sweep: it takes no runtime lease,
// so it may only stop.
var ErrSweepStarts = errors.New("session: a stop sweep starts nothing")

// leaseMode is how a verb takes the runtime lease.
type leaseMode int

const (
	// leaseWait waits up to operatorLeaseWait; a lease still busy is
	// ErrSessionStarting.
	leaseWait leaseMode = iota + 1
	// leaseTry never waits or retries; busy is the bare ErrRuntimeLeaseBusy,
	// for the caller to defer.
	leaseTry
	// leaseNone takes no lease.
	leaseNone
)

// leaseMode derives the actor's lease mode: never configured, only derived.
func (a Actor) leaseMode() (leaseMode, error) {
	switch a.Kind {
	case ActorOperator, ActorAgent, ActorBackground:
		return leaseWait, nil
	case ActorController:
		return leaseTry, nil
	case ActorSweep:
		return leaseNone, nil
	}
	return 0, fmt.Errorf("%w (kind %d)", ErrNoActor, a.Kind)
}

// check refuses an Actor with no kind, so no lifecycle verb runs for a
// caller that did not say who it is.
func (a Actor) check() error {
	_, err := a.leaseMode()
	return err
}

// CheckStarts refuses an Actor that may not start a runtime: one that names
// no kind, or a stop sweep.
func (a Actor) CheckStarts() error {
	if err := a.check(); err != nil {
		return err
	}
	if a.Kind == ActorSweep {
		return ErrSweepStarts
	}
	return nil
}

// consumesHold reports whether the actor's resume consumes an operator's
// hold: only an operator's own does (CONTRACT v5.9 D8 rule 1).
func (a Actor) consumesHold() bool { return a.Kind == ActorOperator }
