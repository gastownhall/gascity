package main

import (
	"context"
	"errors"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The transaction's fence legs (CONTRACT v5 F1, F4, C8.9): L3 and L4 read
// fresh on the routed leaf, the leaf the runtime read used; L5 read live
// through the pass's read-only stores, past their caches (live lists, and
// liveReads for a by-ID read); the idle proof on the routed leaf. Each is
// bounded, and an unknown answer holds.

// txWork is L5 read live: the row's assigned work over every store its agent
// can reach, in the form the effect asks for (readWork: open or in-progress;
// an idle stop's late claim; a rollback's in-progress work; a signal's awake
// work and live-claim veto). Only a clean read proves Free; a failed or
// expired read, and the zero value, count as work.
type txWork struct {
	Free bool // proven: no assigned work in the form read
	Err  error
}

// errNoReadStore: the pass holds no city work legs to read L5 through.
var errNoReadStore = errors.New("v2 effect: no city work legs for the live work read")

// readFence reads the legs and the idle proof the transaction needs, for the
// expected row, or refuses with a cause when no backend resolves the name.
func (t *tx) readFence(ctx context.Context) string {
	var leaf runtime.Provider
	if t.view.RT != nil {
		leaf = t.view.RT.leaf // the leaf the runtime read used
	}
	if leaf == nil && (t.needs.Legs&(legAttach|legPending) != 0 || t.needs.Idle || t.needs.Activity || t.needs.Content) { // L5 alone needs no route
		var known bool
		if leaf, _, known = runtime.ResolveBackend(t.p.Runtime, t.name); t.p.Runtime == nil || !known || leaf == nil {
			return causeRouteUnknown
		}
	}
	t.view.Fence = readLegs(ctx, leaf, t.name, t.needs, t.view.Now, t.view.World.Now, func() *txWork { return t.p.readWork(t.expect, t.view.It) })
	return ""
}

// readLegs reads the legs and the idle proof n asks for, for name on leaf:
// L3 and L4 fresh on the leaf, L5 through work, each bounded; now stamps the
// quiet window, passNow the idle proof's pass. L5 comes after L3 and L4:
// cheapest first, and nearest the CAS, so a claim landing while they are
// read is seen (v5.10a B).
func readLegs(ctx context.Context, leaf runtime.Provider, name string, n needs, now, passNow time.Time, work func() *txWork) txFence {
	f := txFence{Read: n.Legs & (legAttach | legPending | legWork)}
	if n.Legs&legAttach != 0 {
		f.Attach, f.AttachEscalate = attachLeg(ctx, leaf, name, n.Escalates, now)
	}
	if n.Legs&legPending != 0 {
		switch boundedPending(ctx, leaf, name) {
		case pendingInteractionYes:
			f.Pending = fencePending
		case pendingInteractionUnknown:
			f.Pending = fencePendingUnknown
		}
	}
	if n.Legs&legWork != 0 {
		w, ok := boundedProbe(ctx, work)
		if !ok {
			w = &txWork{Err: errProbeExpired}
		}
		f.Work = w
	}
	if n.Idle {
		f.Idle = provedIdle(ctx, leaf, name, passNow)
	}
	if n.Activity {
		a := activityRead{Reportable: sessionActivityReportable(leaf, name)}
		if a.Reportable {
			last, ok := boundedProbe(ctx, func() activityRead {
				at, err := leaf.GetLastActivity(name)
				return activityRead{Last: at, Err: err}
			})
			a.Last, a.Err = last.Last, last.Err
			if !ok {
				a.Err = errProbeExpired
			}
		}
		f.Activity = &a
	}
	if n.Content {
		if snap, ok := leaf.(runtime.IdleSnapshotProvider); ok {
			f.Content = boundedContent(ctx, snap, name)
		}
	}
	return f
}

// boundedContent is a pane snapshot read within ctx: idle, busy,
// indeterminate, an error, or unread past the bound.
func boundedContent(ctx context.Context, snap runtime.IdleSnapshotProvider, name string) contentState {
	got, ok := boundedProbe(ctx, func() contentState { return snapshotContent(snap, name) })
	if !ok {
		return contentUnread
	}
	return got
}

func (p *effectPass) readWork(row session.Info, it intent) *txWork {
	if p.reads.legs.unusable() != nil || p.World.Env == nil {
		return &txWork{Err: errNoReadStore}
	}
	if signalReadsWork(it) {
		return p.readSignalWork(row, it)
	}
	// One seat's read, live, over the pass's city work legs (NEW2-1: never
	// the sessions store standing in for the work store). A close ends a
	// dead seat, which the narrow ReleaseScope gates, as legacy's close gate
	// does; every other effect that reads L5 ends a live runtime (a drain
	// begin, a Workless rollback's stop), which the seat's RefuseScope gates
	// (O4).
	var scope workScope = releaseScope(row, p.World.Env.Cfg)
	if it.Reason == decideIdleTimeout { // legacy's late claim: in-progress work under the narrow scope (SESS-574)
		late, err := lateSeatWork(p.reads.legs, scope, []string{"in_progress"}, nil)
		return &txWork{Free: len(late) == 0 && err == nil, Err: err}
	}
	if it.Kind != intentClose {
		scope = refuseScope(row, p.World.Env.Cfg)
	}
	statuses := seatWorkStatuses
	if it.Kind == intentRollback { // a live incarnation's in-flight work (#2081): open work no stale incarnation can claim
		statuses = []string{"in_progress"}
	}
	has, err := sessionHasAssignedWorkInStoresForStatuses(seatWorkFor(p.reads.legs, scope), scope, statuses)
	return &txWork{Free: !has && err == nil, Err: err}
}

// readSignalWork is a controller signal's L5 (v5.10a B), legacy DRAIN-005's
// live read: the seat's awake assigned work, in progress or ready and not
// held (sessionHasAwakeAssignedWorkForReachableStore), under its release
// scope; with none, an orphaned drain of a seat the live-claim veto covers
// (liveClaimVetoApplies) reads the claim it stamped and whether it still
// holds it (sessionOwnsLiveClaim). An error counts as work.
func (p *effectPass) readSignalWork(row session.Info, it intent) *txWork {
	cfg := p.World.Env.Cfg
	sw := seatWorkFor(p.reads.legs, releaseScope(row, cfg))
	has, err := sessionHasAwakeAssignedWorkForReachableStore(sw, row)
	if err == nil && !has && it.Reason == decideDrainSignal+drainOrphaned && liveClaimVetoApplies(p.World.CityPath, cfg, row, p.World.Suspension) {
		has, _, err = sessionOwnsLiveClaim(p.World.CityPath, cfg, p.reads.city, sw, row)
	}
	return &txWork{Free: !has && err == nil, Err: err}
}

// provedIdle is legacy's idle probe (shouldBeginIdleDrainInfo) on leaf:
// WaitForIdle succeeds, and the runtime reports no activity after the pass
// began.
func provedIdle(ctx context.Context, leaf runtime.Provider, name string, passNow time.Time) bool {
	wp, ok := leaf.(runtime.IdleWaitProvider)
	if !ok || wp.WaitForIdle(ctx, name, idleSleepProbeTimeoutFor(leaf, name)) != nil {
		return false
	}
	last, ok := boundedProbe(ctx, func() error {
		at, err := leaf.GetLastActivity(name)
		if err == nil && !at.IsZero() && at.After(passNow) {
			return errors.New("active since the pass")
		}
		return err
	})
	return ok && last == nil
}

// ownRuntimeFence is the rollback exception's fence (v5 F2, C3): L1 alone
// on rt, the row's own runtime, which the caller has attributed
// (rollbackRuntime); L3 is waived.
func ownRuntimeFence(rt *txRuntime, row session.Info) fenceVerdict {
	return fenceDestructive(rt, txFence{Read: legLiveness}, fenceRequest{Row: row, Legs: legLiveness})
}
