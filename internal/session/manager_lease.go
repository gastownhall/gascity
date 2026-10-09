package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The runtime lease at the Manager's starts and stops (I-LEASE): every path
// that may call the provider's Start or Stop for a session holds the session's
// runtime lease across it. Operators wait for it (O3); the controller never
// does (WithoutLeaseWait), and defers on ErrRuntimeLeaseBusy. A Manager whose
// city path is not absolute has no runtime dir to lock in: its starts and
// stops fail with ErrRuntimeLeaseNoCity, except stop sweeps.

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

type (
	runtimeLeaseCtxKey struct{}
	noLeaseWaitCtxKey  struct{}
	citySweepCtxKey    struct{}
)

// ContextWithRuntimeLease marks ctx as running under l, a lease its caller
// holds on the runtime a Manager call starts or stops: the call takes none of
// its own, and refuses one held on another session or runtime. A nil lease
// leaves ctx as it is.
func ContextWithRuntimeLease(ctx context.Context, l *RuntimeLease) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, runtimeLeaseCtxKey{}, l)
}

// CitySweepContext marks ctx as a stop-every-session sweep (`gc stop`, a rig
// restart): its stops take no runtime lease, by design. It is the only way a
// Manager stop goes without one besides a city path that is not absolute.
func CitySweepContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, citySweepCtxKey{}, true)
}

// WithoutLeaseWait marks ctx as the controller's: a Manager start or stop
// under it takes the runtime lease without waiting or retrying, and returns
// ErrRuntimeLeaseBusy (not ErrSessionStarting) when another holder has it, for
// the caller to defer.
func WithoutLeaseWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, noLeaseWaitCtxKey{}, true)
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

// ErrRuntimeLeaseNoCity refuses a start or stop by a Manager whose city path
// is not absolute: it has no runtime dir to take the lease in.
var ErrRuntimeLeaseNoCity = errors.New("runtime lease: the session manager has no city path")

// leaselessManagersAllowed lets tests run Managers without a city path
// (AllowManagersWithoutCityForTest).
var leaselessManagersAllowed atomic.Bool

// AllowManagersWithoutCityForTest lets a test package's Managers without a
// city path start and stop runtimes without the lease, as before L1. A test
// package's init calls it; production never does.
func AllowManagersWithoutCityForTest() { leaselessManagersAllowed.Store(true) }

// leaseRuntime takes the lease on session id's runtime sessName, waiting up
// to wait (zero: not at all, for a caller under the session mutation lock).
// release ends it. A lease ctx carries is the caller's: it is returned with a
// no-op release, and must be on this session and runtime. A Manager whose city
// path is not absolute refuses with ErrRuntimeLeaseNoCity, unless ctx is a
// stop sweep or a test allowed it; then the lease is nil.
func (m *Manager) leaseRuntime(ctx context.Context, id, sessName string, wait time.Duration) (*RuntimeLease, func(), error) {
	if borrowed, ok := ctx.Value(runtimeLeaseCtxKey{}).(*RuntimeLease); ok {
		if borrowed.name != strings.TrimSpace(sessName) || (borrowed.id != "" && borrowed.id != id) {
			return nil, func() {}, fmt.Errorf("runtime lease: the caller's lease is on session %q runtime %q, not session %q runtime %q", borrowed.id, borrowed.name, id, sessName)
		}
		return borrowed, func() {}, nil
	}
	if sweep, _ := ctx.Value(citySweepCtxKey{}).(bool); sweep {
		return nil, func() {}, nil
	}
	if !filepath.IsAbs(m.cityPath) {
		if !leaselessManagersAllowed.Load() {
			return nil, func() {}, fmt.Errorf("%w (%q): session %q", ErrRuntimeLeaseNoCity, m.cityPath, id)
		}
		return nil, func() {}, nil
	}
	ttl := m.leaseTTL
	if ttl <= 0 {
		ttl = RuntimeLeaseTTLFor(nil)
	}
	front := NewStore(beads.SessionStore{Store: m.store})
	req := RuntimeLeaseRequest{City: m.cityPath, Name: sessName, ID: id, TTL: ttl}
	if controller, _ := ctx.Value(noLeaseWaitCtxKey{}).(bool); controller {
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

// leaseForStop is leaseRuntime for a stop, which a store is never allowed to
// hold hostage: a lease failure other than busy (the store unreachable, the
// row closed or renamed, no conditional writes in require mode) is logged and
// the stop runs under the name's flock alone. Under WithoutLeaseWait it never
// waits, and a busy lease is the bare ErrRuntimeLeaseBusy.
func (m *Manager) leaseForStop(ctx context.Context, id, sessName string, wait time.Duration) (func(), error) {
	controller, _ := ctx.Value(noLeaseWaitCtxKey{}).(bool)
	_, release, err := m.leaseRuntime(ctx, id, sessName, wait)
	if err != nil && !errors.Is(err, ErrRuntimeLeaseBusy) && filepath.IsAbs(m.cityPath) {
		log.Printf("runtime lease: session %q: stopping %q under the name's flock alone: %v", id, sessName, err)
		var flock *RuntimeLease
		flock, err = TryRuntimeLease(nil, RuntimeLeaseRequest{City: m.cityPath, Name: sessName})
		release = flock.Release
		if !controller {
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
// ctx carrying it, so the stop and the restart run under one lease (D8D-5).
func (m *Manager) leaseForRestartLocked(ctx context.Context, id, sessName string) (context.Context, func(), error) {
	l, release, err := m.leaseRuntime(ctx, id, sessName, 0)
	if err != nil {
		return ctx, func() {}, err
	}
	return ContextWithRuntimeLease(ctx, l), release, nil
}
