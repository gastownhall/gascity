package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The destructive-action fence (CONTRACT §8, P4 spec §3.5). An effect
// evaluates it in a section, on the transaction's fresh reads (readRuntime,
// readLegs) of the routed leaf that then makes the destructive call, so the
// reads and the call cannot split across backends and no cached read reaches
// it (C8.7). The stop itself runs in the effect's Call (stopFenced).
//
// A backend that cannot read fresh with errors (k8s, ssh, exec, herdr: no
// fresh liveness read, and terminals a human can attach to without
// reporting it) proves nothing to the fence: its L1 is unknown, its L2
// unverifiable, its attach tier terminal-no-report, and its stop never
// confirmed.

// fenceLegs is a set of fence legs. A named exception (§8.4) evaluates a
// subset; every leg it keeps is evaluated in full.
type fenceLegs uint8

const (
	legLiveness fenceLegs = 1 << iota // L1
	legToken                          // L2
	legAttach                         // L3
	legPending                        // L4
	legWork                           // L5, only where the action requires it

	legsFull = legLiveness | legToken | legAttach | legPending
)

// Fence timing. fenceProbeTimeout bounds each probe; an expired probe is
// unknown. fenceQuietWindow is how long a terminal-no-report runtime must have
// been quiet before a destructive action may proceed (DRAIN-524).
const (
	fenceProbeTimeout = 5 * time.Second
	fenceQuietWindow  = 10 * time.Minute
)

// Fence reasons, for the trace. A fence that does not proceed names the first
// leg that held it.
const (
	fenceRouteUnknown         = "route_unknown"
	fenceNothingToStop        = "nothing_to_stop"
	fenceLivenessUnknown      = "liveness_unknown"
	fenceTokenMismatch        = "token_mismatch"
	fenceTokenAbsent          = "token_absent"
	fenceTokenUnverifiable    = "token_unverifiable"
	fenceAttached             = "attached"
	fenceTerminalNotQuiet     = "terminal_not_quiet"
	fenceTerminalNoReport     = "terminal_no_report_escalation"
	fencePending              = "pending"
	fencePendingUnknown       = "pending_unknown"
	fenceHasWork              = "has_work"
	fenceWorkUnknown          = "work_unknown"
	fenceStopUnconfirmed      = "stop_unconfirmed"
	fenceStopFailed           = "stop_failed"
	fenceLivenessPresent      = "present"
	fenceLivenessAbsent       = "absent"
	fenceLivenessUnobservable = "unknown"
)

// fenceRequest is one destructive action on row's runtime, with the legs
// it keeps. A C8.5 escalation class reads its legs with needs.Escalates.
type fenceRequest struct {
	Row  session.Info
	Legs fenceLegs
}

// fenceVerdict is the fence's answer, which only fenceDestructive makes:
// Leaf is the routed backend the destructive call must go through (the one
// the runtime read used), Row the row it fenced. Its zero value holds.
type fenceVerdict struct {
	Proceed  bool
	Escalate bool // defer, and escalate (C8.3, C8.5)
	Reason   string
	Liveness string
	Leaf     runtime.Provider
	Row      session.Info
	attach   bool // L3 kept: re-checked immediately before the stop (C8.7)
}

// fenceDestructive evaluates req's legs on the transaction's fresh reads of
// its row's runtime: L1 and L2 from rt (readRuntime, on the routed leaf
// since the attempt began), L3 to L5 from f (readLegs). It reads nothing
// itself, so no cached liveness or identity read reaches it (C8.7). A kept
// leg the transaction did not read holds, as unknown. A backend that cannot
// read fresh proves nothing, so the fence holds whatever legs it keeps. rt
// is nil when no backend resolves the name.
func fenceDestructive(rt *txRuntime, f txFence, req fenceRequest) fenceVerdict {
	if rt == nil {
		return fenceVerdict{Reason: fenceRouteUnknown, Row: req.Row}
	}
	v := fenceVerdict{Leaf: rt.leaf, Row: req.Row, Liveness: rtLiveness(rt), attach: req.Legs&legAttach != 0}
	switch {
	case rt.Class == rtUnsupported, req.Legs&legLiveness != 0 && v.Liveness == fenceLivenessUnobservable:
		v.Reason = fenceLivenessUnknown
		return v
	case req.Legs&legLiveness != 0 && v.Liveness == fenceLivenessAbsent:
		v.Reason = fenceNothingToStop
		return v
	}
	if req.Legs&legToken != 0 {
		if v.Reason, v.Escalate = tokenVerdict(rt, req.Row); v.Reason != "" {
			return v
		}
	}
	unread := req.Legs &^ f.Read
	switch w := f.Work; {
	case unread&legAttach != 0:
		v.Reason = fenceAttached
	case req.Legs&legAttach != 0 && f.Attach != "":
		v.Reason, v.Escalate = f.Attach, f.AttachEscalate
	case unread&legPending != 0:
		v.Reason = fencePendingUnknown
	case req.Legs&legPending != 0 && f.Pending != "":
		v.Reason = f.Pending
	case req.Legs&legWork != 0 && (w == nil || w.Err != nil):
		v.Reason = fenceWorkUnknown // an unread L5 leaves Work nil
	case req.Legs&legWork != 0 && !w.Free:
		v.Reason = fenceHasWork
	default:
		v.Proceed = true
	}
	return v
}

// rtLiveness is L1 on the read: present while something runs under the
// name on the leaf (alive, or its agent dead), absent when nothing runs
// there or on any fall-through backend (absent, or a corpse), unknown on an
// incomplete read or a backend that cannot read fresh.
func rtLiveness(rt *txRuntime) string {
	switch rt.Class {
	case rtAlive, rtZombie:
		return fenceLivenessPresent
	case rtAbsent, rtCorpse:
		return fenceLivenessAbsent
	}
	return fenceLivenessUnobservable
}

// tokenVerdict is L2 (§8.1, C0.8; v5 F1, O2): it passes only on a Current
// verdict (compareIdentity) of the identity read on one present object
// (Same), so a stale or newer incarnation is never stopped as its own, and
// identity read off a runtime not present (acp's leftover sidecar) proves
// nothing. Foreign or another token is a mismatch, which never passes. An
// unread identity, no present object, or no token defers and escalates
// (C8.3). It returns the reason that holds the action, or "".
func tokenVerdict(rt *txRuntime, row session.Info) (reason string, escalate bool) {
	switch v := compareIdentity(row, rt.Identity); {
	case rtLiveness(rt) != fenceLivenessPresent || !rt.Same || !rt.Identity.Known:
		return fenceTokenUnverifiable, true
	case v == identityCurrent:
		return "", false
	case v != identityForeign && strings.TrimSpace(rt.Identity.Token) == "":
		return fenceTokenAbsent, true
	}
	return fenceTokenMismatch, false
}

// stopFenced runs a fenced stop of v's row, from a Call: when v proceeds it
// re-checks a kept L3's attach probe on a reporter leaf (C8.7), stops the
// runtime through v.Leaf, the leaf the fence's reads used, then confirms
// the stop on a fresh read since now() (C8.8, confirmStopped) and drops the
// name's route on every hop that holds one, as the composite's own Stop
// would have. confirmed reports a confirmed stop; a fence that held stops
// nothing and confirms nothing, except that nothing to stop is confirmed by
// a fresh read too. A stop error is not yet read as a C8.8 read (D3 step 2,
// C6b2's).
func stopFenced(ctx context.Context, sp runtime.Provider, v fenceVerdict, processNames []string, now func() time.Time) (_ fenceVerdict, confirmed bool) {
	name := strings.TrimSpace(v.Row.SessionName)
	switch {
	case v.Proceed:
	case v.Reason == fenceNothingToStop:
		return v, confirmStopped(ctx, sp, processNames, name, now())
	default:
		return v, false
	}
	if _, reports := v.Leaf.(runtime.AttachmentObserverWithError); v.attach && reports && v.Leaf.Capabilities().CanReportAttachment && boundedAttachHolds(ctx, v.Leaf, name) {
		v.Proceed, v.Reason = false, fenceAttached
		return v, false
	}
	if err := v.Leaf.Stop(name); err != nil && !errors.Is(err, runtime.ErrSessionNotFound) {
		v.Proceed, v.Reason = false, fenceStopFailed
		return v, false
	}
	if !confirmStopped(ctx, sp, processNames, name, now()) {
		v.Reason = fenceStopUnconfirmed
		return v, false
	}
	unroute(sp, name)
	return v, true
}

// unroute drops name's route on every router hop from sp to its leaf that
// keeps routes (auto's ACP table). The hops are resolved before any is
// changed: dropping an outer route re-routes the name.
func unroute(sp runtime.Provider, name string) {
	var hops []runtime.Provider
	for {
		router, ok := sp.(runtime.Router)
		if !ok {
			break
		}
		hops = append(hops, sp)
		sp = router.RouteFor(name).Provider
	}
	for _, hop := range hops {
		if u, ok := hop.(interface{ Unroute(name string) }); ok {
			u.Unroute(name)
		}
	}
}

// confirmStopped reports a confirmed stop (C8.8): a fresh read of name on
// its routed leaf since since finds nothing running, confirmed through
// every fall-through hop (hopsQuiet: absent, or a corpse). A stale route
// cannot confirm on the wrong backend, and a backend that cannot read fresh
// never confirms.
func confirmStopped(ctx context.Context, sp runtime.Provider, processNames []string, name string, since time.Time) bool {
	name = strings.TrimSpace(name)
	leaf, _, known := runtime.ResolveBackend(sp, name)
	if sp == nil || name == "" || !known || leaf == nil || !freshReadable(leaf) {
		return false
	}
	live, ok := freshRead(ctx, leaf, name, processNames, since)
	return ok && !live.Running && hopsQuiet(ctx, sp, name, processNames, since, !live.Present())
}

// attachLeg is L3 by the leaf's attach tier (§8.1). reporter: the error probe
// holds on attached or any error but a vanished session. terminal-no-report:
// passes only after a readable quiet window, and C8.5 classes escalate.
// no-terminal: passes.
func attachLeg(ctx context.Context, leaf runtime.Provider, name string, escalates bool, now time.Time) (reason string, escalate bool) {
	caps := leaf.Capabilities()
	_, observes := leaf.(runtime.AttachmentObserverWithError)
	_, hardened := leaf.(runtime.LivenessObserverWithError)
	switch {
	case observes && caps.CanReportAttachment:
		if boundedAttachHolds(ctx, leaf, name) {
			return fenceAttached, false
		}
		return "", false
	case !caps.CanAttachTTY && !caps.CanReportAttachment && hardened:
		return "", false
	case escalates:
		return fenceTerminalNoReport, true
	}
	if caps.CanReportActivity {
		last, ok := boundedProbe(ctx, func() time.Time {
			at, err := leaf.GetLastActivity(name)
			if err != nil {
				return time.Time{}
			}
			return at
		})
		if ok && !last.IsZero() && now.Sub(last) >= fenceQuietWindow {
			return "", false
		}
	}
	return fenceTerminalNotQuiet, false
}

// boundedAttachHolds runs the attach error probe on a reporter leaf; an
// expired probe holds.
func boundedAttachHolds(ctx context.Context, leaf runtime.Provider, name string) bool {
	holds, ok := boundedProbe(ctx, func() bool {
		return runtime.AttachProbeHolds(runtime.IsAttachedWithError(leaf, name))
	})
	return !ok || holds
}

// boundedPending is L4 on leaf: unsupported reads no (it is not unknown); an
// error or an expired probe reads unknown, which holds.
func boundedPending(ctx context.Context, leaf runtime.Provider, name string) pendingInteractionAnswer {
	answer, ok := boundedProbe(ctx, func() pendingInteractionAnswer {
		a, _ := pendingInteractionProbe(leaf, name)
		return a
	})
	if !ok {
		return pendingInteractionUnknown
	}
	return answer
}

// boundedProbe runs probe under fenceProbeTimeout and ctx. ok is false when the
// bound expires first, the probe's goroutine then abandoned, or when the probe
// panics: a panicking probe proves nothing and never takes the process down.
func boundedProbe[T any](ctx context.Context, probe func() T) (T, bool) {
	return boundedProbeCtx(ctx, func(context.Context) T { return probe() })
}

// boundedProbeCtx is boundedProbe for a probe that takes the bounded context.
func boundedProbeCtx[T any](ctx context.Context, probe func(context.Context) T) (T, bool) {
	ctx, cancel := context.WithTimeout(ctx, fenceProbeTimeout)
	defer cancel()
	out, failed := make(chan T, 1), make(chan struct{})
	go func() {
		defer func() {
			if recover() != nil {
				close(failed)
			}
		}()
		out <- probe(ctx)
	}()
	var zero T
	select {
	case v := <-out:
		return v, true
	case <-failed:
		return zero, false
	case <-ctx.Done():
		return zero, false
	}
}
