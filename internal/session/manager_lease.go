package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The runtime lease at the Manager's starts and stops (I-LEASE): every path
// that may call the provider's Start or Stop for a session holds the session's
// runtime lease across it, in the mode its Actor derives (Actor.leaseMode):
// operators wait for it (O3); the controller never does, and defers on
// ErrRuntimeLeaseBusy. An Actor with no City has no runtime dir to lock in:
// its starts and stops fail with ErrRuntimeLeaseNoCity, except stop sweeps.

// RuntimeLeaseOperatorWait bounds an operator's wait for a runtime lease
// another holder has (CONTRACT O3).
const RuntimeLeaseOperatorWait = 10 * time.Second

// ErrSessionStarting reports that another holder, usually the controller
// starting the session, kept its runtime lease past the operator's wait.
// Retrying converges on the session that start leaves.
var ErrSessionStarting = errors.New("session is starting, retry")

// operatorLeaseWait is RuntimeLeaseOperatorWait; tests shorten it.
var operatorLeaseWait = RuntimeLeaseOperatorWait

// SetOperatorLeaseWaitForTest shortens the operator's lease wait for a test;
// restore puts it back.
func SetOperatorLeaseWaitForTest(d time.Duration) (restore func()) {
	prev := operatorLeaseWait
	operatorLeaseWait = d
	return func() { operatorLeaseWait = prev }
}

// runtimeLeaseWatchEvery is how often a start under a lease reads that the
// lease is still its own (RuntimeLease.Watch).
const runtimeLeaseWatchEvery = 5 * time.Second

// RuntimeLeaseTTLFor is the runtime lease TTL for cfg's startup timeout, the
// default one's without a config.
func RuntimeLeaseTTLFor(cfg *config.City) time.Duration {
	if cfg == nil {
		cfg = &config.City{}
	}
	return RuntimeLeaseTTL(cfg.Session.StartupTimeoutDuration())
}

// WithRuntimeLeaseTTL sets the lifetime of the Manager's lease records,
// RuntimeLeaseTTLFor the city. Unset uses the default startup timeout's.
func WithRuntimeLeaseTTL(ttl time.Duration) ManagerOption {
	return func(m *Manager) { m.leaseTTL = ttl }
}

// WaitOperatorRuntimeLease takes req's lease for an operator, waiting up to
// RuntimeLeaseOperatorWait; a lease still busy then is ErrSessionStarting.
func WaitOperatorRuntimeLease(ctx context.Context, s *Store, req RuntimeLeaseRequest) (*RuntimeLease, error) {
	return operatorRuntimeLease(WaitRuntimeLease(ctx, s, req, operatorLeaseWait))
}

// ForceRuntimeLease is an operator's override for a hung holder that shares
// the flock (`gc session kill --force`): when the flock is busy but the row's
// record is free (released, expired, or malformed), it takes the record
// without the flock, and logs it. An unexpired record stays busy, as
// ErrSessionStarting. The lease it returns excludes other hosts only.
func ForceRuntimeLease(s *Store, req RuntimeLeaseRequest) (*RuntimeLease, error) {
	l, err := TryRuntimeLease(s, req)
	var busy *RuntimeLeaseBusyError
	if err == nil || req.ID == "" || !errors.As(err, &busy) || !busy.Local {
		return operatorRuntimeLease(l, err)
	}
	l = &RuntimeLease{store: s, id: req.ID, name: strings.TrimSpace(req.Name), now: time.Now, holder: newRuntimeLeaseHolder()}
	if req.now != nil {
		l.now = req.now
	}
	if err := l.acquireRecord(req.City, req.TTL); err != nil {
		return operatorRuntimeLease(nil, err)
	}
	log.Printf("runtime lease: operator override: session %q: took runtime %q's record past a live flock holder (%s)", req.ID, l.name, busy.Holder)
	return l, nil
}

func operatorRuntimeLease(l *RuntimeLease, err error) (*RuntimeLease, error) {
	if errors.Is(err, ErrRuntimeLeaseBusy) {
		return nil, fmt.Errorf("%w: %w", ErrSessionStarting, err)
	}
	return l, err
}

// ErrNoCallerLease refuses a runtime-only start (StartRuntimeOnly) whose
// caller hands it no lease: it takes none of its own.
var ErrNoCallerLease = errors.New("runtime lease: a runtime-only start runs under its caller's lease, and has none")

// ErrRuntimeLeaseNoCity refuses a start or stop by an Actor with no City:
// there is no runtime dir to take the lease in (RefuseWithoutCity).
var ErrRuntimeLeaseNoCity = errors.New("runtime lease: no absolute city path to take the lease in")

// leaseRuntime takes the lease on session id's runtime sessName for by,
// waiting up to wait in leaseWait mode (zero: not at all, for a caller under
// the session mutation lock). release ends it. by.Lease is the caller's: it is
// returned with a no-op release, and must be on this session and runtime. An
// Actor with no City refuses with ErrRuntimeLeaseNoCity, unless it is a stop
// sweep; then the lease is nil.
func (m *Manager) leaseRuntime(ctx context.Context, by Actor, id, sessName string, wait time.Duration) (*RuntimeLease, func(), error) {
	mode, _ := by.leaseMode() // every verb checked by's kind (Manager.checkActor)
	if borrowed := by.Lease; borrowed != nil {
		if err := checkBorrowed(borrowed, id, sessName); err != nil {
			return nil, func() {}, err
		}
		return borrowed, func() {}, nil
	}
	if mode == leaseNone {
		return nil, func() {}, nil
	}
	if by.City.IsZero() {
		return nil, func() {}, RefuseWithoutCity(by.City.String(), fmt.Sprintf("session %q", id))
	}
	ttl := m.leaseTTL
	if ttl <= 0 {
		ttl = RuntimeLeaseTTLFor(nil)
	}
	front := NewStore(beads.SessionStore{Store: m.store})
	req := RuntimeLeaseRequest{City: by.City.Path(), Name: sessName, ID: id, TTL: ttl}
	if mode == leaseTry {
		l, err := TryRuntimeLease(front, req) // busy stays ErrRuntimeLeaseBusy: no retry, the caller defers
		return l, l.Release, err
	}
	var l *RuntimeLease
	var err error
	if wait <= 0 {
		l, err = operatorRuntimeLease(TryRuntimeLease(front, req))
	} else {
		l, err = operatorRuntimeLease(WaitRuntimeLease(ctx, front, req, wait))
	}
	return l, l.Release, err
}

// LeaseRuntimeName takes the runtime name's flock alone, for a starter or
// stopper with no session row (a runtime-only worker handle), in by's lease
// mode: by.Lease or a stop sweep takes none; the controller never waits and
// gets ErrRuntimeLeaseBusy; any other actor waits up to
// RuntimeLeaseOperatorWait, then gets ErrSessionStarting. An Actor with no
// City refuses with ErrRuntimeLeaseNoCity.
func LeaseRuntimeName(ctx context.Context, by Actor, name string) (release func(), err error) {
	mode, err := by.leaseMode()
	switch {
	case err != nil:
		return func() {}, err
	case by.Lease != nil:
		return func() {}, checkBorrowed(by.Lease, "", name)
	case mode == leaseNone:
		return func() {}, nil
	case by.City.IsZero():
		return func() {}, RefuseWithoutCity(by.City.String(), fmt.Sprintf("runtime %q", name))
	}
	req := RuntimeLeaseRequest{City: by.City.Path(), Name: name}
	var l *RuntimeLease
	if mode == leaseTry {
		l, err = TryRuntimeLease(nil, req)
	} else {
		l, err = operatorRuntimeLease(WaitRuntimeLease(ctx, nil, req, operatorLeaseWait))
	}
	if err != nil {
		return func() {}, err
	}
	return l.Release, nil
}

// checkBorrowed refuses a lease a caller hands a verb (Actor.Lease) that is
// not on runtime name and, when both name one, session id, or that its holder
// already released.
func checkBorrowed(l *RuntimeLease, id, name string) error {
	l.mu.Lock()
	released := l.released
	l.mu.Unlock()
	switch {
	case released:
		return fmt.Errorf("runtime lease: the caller's lease on session %q runtime %q was released", l.id, l.name)
	case l.name != strings.TrimSpace(name) || (id != "" && l.id != "" && l.id != id):
		return fmt.Errorf("runtime lease: the caller's lease is on session %q runtime %q, not session %q runtime %q", l.id, l.name, id, name)
	}
	return nil
}

// CityDir is the Manager's city as a CityDir: the one an Actor driving it
// must name (checkActor).
func (m *Manager) CityDir() (CityDir, error) { return NewCityDir(m.cityPath) }

// checkActor is Actor.check, and refuses an Actor on another city than the
// Manager's own: its lease would be taken where none of the Manager's
// holders look.
func (m *Manager) checkActor(by Actor) error {
	if err := by.check(); err != nil {
		return err
	}
	if filepath.IsAbs(m.cityPath) && !by.City.IsZero() && filepath.Clean(m.cityPath) != by.City.Path() {
		return fmt.Errorf("session: the actor's city %q is not the manager's %q", by.City.Path(), m.cityPath)
	}
	return nil
}

// leaseForStop is leaseRuntime for a stop, which a store is never allowed to
// hold hostage: a lease failure other than busy (the store unreachable, the
// row closed or renamed, no conditional writes in require mode) is logged and
// the stop runs under the name's flock alone. The controller never waits, and
// a busy lease is the bare ErrRuntimeLeaseBusy.
func (m *Manager) leaseForStop(ctx context.Context, by Actor, id, sessName string, wait time.Duration) (func(), error) {
	_, release, err := m.leaseRuntime(ctx, by, id, sessName, wait)
	if err != nil && !errors.Is(err, ErrRuntimeLeaseBusy) && !by.City.IsZero() {
		log.Printf("runtime lease: session %q: stopping %q under the name's flock alone: %v", id, sessName, err)
		var flock *RuntimeLease
		flock, err = TryRuntimeLease(nil, RuntimeLeaseRequest{City: by.City.Path(), Name: sessName})
		release = flock.Release
		if by.Kind != ActorController {
			_, err = operatorRuntimeLease(nil, err)
		}
	}
	if err != nil {
		return func() {}, err
	}
	return release, nil
}

// withSessionStartLock runs fn under session id's mutation lock. fn may start
// the runtime, whose lease ensureRunning takes without waiting under the lock;
// a busy lease drops the lock and reruns fn, for up to operatorLeaseWait (O3),
// so nothing waits for a lease under the session mutation lock. fn must write
// nothing before it reaches the lease.
func withSessionStartLock(ctx context.Context, id string, fn func() error) error {
	deadline := time.Now().Add(operatorLeaseWait)
	for {
		err := withSessionMutationLock(id, fn)
		if !errors.Is(err, ErrSessionStarting) || !time.Now().Add(runtimeLeaseWaitPoll).Before(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-time.After(runtimeLeaseWaitPoll):
		}
	}
}

// leaseForRestartLocked takes the runtime lease for an interrupt that may
// fall back to a stop and a restart, before the interrupt is sent, and returns
// by carrying it, so the stop and the restart run under one lease (D8D-5).
func (m *Manager) leaseForRestartLocked(ctx context.Context, by Actor, id, sessName string) (Actor, func(), error) {
	l, release, err := m.leaseRuntime(ctx, by, id, sessName, 0)
	if err != nil {
		return by, func() {}, err
	}
	if l != nil {
		by.Lease = l
	}
	return by, release, nil
}
