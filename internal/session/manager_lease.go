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

// leaseRuntime takes the lease on session id's runtime sessName, waiting up
// to wait (zero: not at all, for a caller under the session mutation lock).
// release ends it. A lease ctx carries is the caller's: it is returned with a
// no-op release, and must be on this session and runtime. A Manager whose city
// path is not absolute refuses with ErrRuntimeLeaseNoCity, unless ctx is a
// stop sweep; then the lease is nil.
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
		return nil, func() {}, fmt.Errorf("%w (%q): session %q", ErrRuntimeLeaseNoCity, m.cityPath, id)
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

type killPremiseCtxKey struct{}

// ErrKillPremiseMoved refuses a kill whose row, read fresh under the runtime
// lease, no longer carries the facts the kill was decided on.
var ErrKillPremiseMoved = errors.New("runtime lease: the row moved since the kill was decided")

// WithKillPremise makes a Manager kill under ctx decide again under the
// runtime lease: it reads the row fresh once it holds the lease, and stops
// only when premise holds for that read (an open row). A controller kill
// decided on a tick's snapshot passes one, so it spares a row an operator
// moved in between under the lease: a resume that consumed a hold or woke a
// dormant row, or a new incarnation. It cannot see a move that writes
// nothing the premise reads: a live runtime restarted in place at the same
// generation and token is told apart only by the kill's exact-object fence.
func WithKillPremise(ctx context.Context, premise func(fresh Info) bool) context.Context {
	return context.WithValue(ctx, killPremiseCtxKey{}, premise)
}

// SameKillFacts reports whether fresh still carries the facts a kill decided
// on expected rests on that an operator moves under the lease: the
// incarnation (generation, instance token), the operator's intent (the hold
// an attach consumes or a suspend sets), and dormancy (a row decided dormant
// that an attach woke is live now). Other state the controller's own tick
// heals is not compared.
func SameKillFacts(expected, fresh Info) bool {
	return !fresh.Closed && fresh.Generation == expected.Generation && fresh.InstanceToken == expected.InstanceToken &&
		fresh.SleepIntent == expected.SleepIntent && fresh.HeldUntil == expected.HeldUntil &&
		(liveMetadataState(expected.MetadataState) || !liveMetadataState(fresh.MetadataState))
}

// liveMetadataState reports whether a row's raw state claims a live or
// starting runtime.
func liveMetadataState(state string) bool {
	switch State(strings.TrimSpace(state)) {
	case StateActive, StateAwake, StateCreating, StateStartPending:
		return true
	}
	return false
}

// killPremiseHolds checks ctx's kill premise, if any, on a fresh read of id.
func (m *Manager) killPremiseHolds(ctx context.Context, id string) error {
	premise, ok := ctx.Value(killPremiseCtxKey{}).(func(Info) bool)
	if !ok {
		return nil
	}
	b, err := NewStore(beads.SessionStore{Store: m.store}).freshBead(id)
	if err != nil {
		return err
	}
	if fresh := infoFromPersistedBead(b); b.Status == "closed" || !premise(fresh) {
		return fmt.Errorf("%w: session %q", ErrKillPremiseMoved, id)
	}
	return nil
}

// LeaseRuntimeName takes the runtime name's flock alone, for a starter or
// stopper with no session row (a runtime-only worker handle), under ctx's
// lease mode: a lease ctx carries or a stop sweep takes none; the controller
// (WithoutLeaseWait) never waits and gets ErrRuntimeLeaseBusy; an operator
// waits up to RuntimeLeaseOperatorWait, then gets ErrSessionStarting. A city
// path that is not absolute refuses with ErrRuntimeLeaseNoCity.
func LeaseRuntimeName(ctx context.Context, cityPath, name string) (release func(), err error) {
	_, borrowed := ctx.Value(runtimeLeaseCtxKey{}).(*RuntimeLease)
	sweep, _ := ctx.Value(citySweepCtxKey{}).(bool)
	switch {
	case borrowed || sweep:
		return func() {}, nil
	case !filepath.IsAbs(cityPath):
		return func() {}, fmt.Errorf("%w (%q): runtime %q", ErrRuntimeLeaseNoCity, cityPath, name)
	}
	req := RuntimeLeaseRequest{City: cityPath, Name: name}
	var l *RuntimeLease
	if controller, _ := ctx.Value(noLeaseWaitCtxKey{}).(bool); controller {
		l, err = TryRuntimeLease(nil, req)
	} else {
		l, err = operatorRuntimeLease(WaitRuntimeLease(ctx, nil, req, operatorLeaseWait))
	}
	if err != nil {
		return func() {}, err
	}
	return l.Release, nil
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
