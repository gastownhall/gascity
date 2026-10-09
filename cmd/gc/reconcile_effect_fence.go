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
// through the pass's read-only stores; the idle proof on the routed leaf.
// Each is bounded, and an unknown answer holds.

// txWork is L5 read live: the row's open or in-progress assigned work over
// every store its agent can reach. A failed or expired read counts as work.
type txWork struct {
	Has bool
	Err error
}

// errNoReadStore: the pass holds no city store to read L5 through.
var errNoReadStore = errors.New("v2 effect: no read-only city store for the live work read")

// readFence reads the legs and the idle proof the transaction needs, for the
// expected row, or refuses with a cause when no backend resolves the name.
func (t *tx) readFence(ctx context.Context) string {
	var leaf runtime.Provider
	if t.view.RT != nil {
		leaf = t.view.RT.leaf // the leaf the runtime read used
	}
	if leaf == nil && (t.needs.Legs&(legAttach|legPending) != 0 || t.needs.Idle) { // L5 alone needs no route
		var known bool
		if leaf, _, known = runtime.ResolveBackend(t.p.Runtime, t.name); t.p.Runtime == nil || !known || leaf == nil {
			return causeRouteUnknown
		}
	}
	f := txFence{}
	if t.needs.Legs&legAttach != 0 {
		f.Attach, _ = attachLeg(ctx, leaf, t.name, false, t.view.Now)
	}
	if t.needs.Legs&legPending != 0 {
		switch boundedPending(ctx, leaf, t.name) {
		case pendingInteractionYes:
			f.Pending = fencePending
		case pendingInteractionUnknown:
			f.Pending = fencePendingUnknown
		}
	}
	if t.needs.Legs&legWork != 0 {
		w, ok := boundedProbe(ctx, func() *txWork { return t.p.readWork(t.expect) })
		if !ok {
			w = &txWork{Has: true, Err: errProbeExpired}
		}
		f.Work = w
	}
	if t.needs.Idle {
		f.Idle = provedIdle(ctx, leaf, t.name, t.view.World.Now)
	}
	t.view.Fence = f
	return ""
}

func (p *effectPass) readWork(row session.Info) *txWork {
	if p.reads.city == nil || p.World.Env == nil {
		return &txWork{Has: true, Err: errNoReadStore}
	}
	has, err := sessionHasOpenAssignedWorkForReachableStore(p.World.CityPath, p.World.Env.Cfg, p.reads.city, p.reads.rigs, row)
	return &txWork{Has: has || err != nil, Err: err}
}

// provedIdle is legacy's idle probe (shouldBeginIdleDrainInfo) on leaf:
// WaitForIdle succeeds, and the runtime reports no activity after the pass
// began.
func provedIdle(ctx context.Context, leaf runtime.Provider, name string, passNow time.Time) bool {
	wp, ok := leaf.(runtime.IdleWaitProvider)
	if !ok || wp.WaitForIdle(ctx, name, idleSleepProbeTimeout) != nil {
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
