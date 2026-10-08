package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The destructive-action fence (CONTRACT §8, P4 spec §3.5) on main's runtime
// seams. An effect evaluates it on fresh probes immediately before a
// destructive provider call (C8.7), through the routed leaf backend that then
// makes the call, so the probes and the call cannot split across backends. It
// never reads the observation cache.
//
// A leaf is hardened when it implements runtime.LivenessObserverWithError.
// CONTRACT Appendix A pairs that observer with the backend's other P1.9 items:
// k8s, ssh, exec and herdr lack it, and they have terminals a human can attach
// to without reporting it (k8s and ssh report no TTY capability at all). So
// an unhardened leaf's attach tier is terminal-no-report, and its absence is
// never confirmed.

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
	fenceLivenessUnconfirmed  = "absent_unconfirmed"
	fenceLivenessUnobservable = "unknown"
)

// fenceRequest is one destructive action on row's runtime.
type fenceRequest struct {
	Row  session.Info
	Legs fenceLegs
	// Escalates marks a C8.5 escalation class (max age, drain timeout,
	// config drift): on a terminal-no-report backend it escalates and never
	// proceeds.
	Escalates bool
	// Work answers L5: whether the row has live assigned work. A request
	// that keeps L5 with no Work fails closed.
	Work func(ctx context.Context) (bool, error)
}

// fenceVerdict is the fence's answer. Leaf is the routed backend the
// destructive call must go through.
type fenceVerdict struct {
	Proceed  bool
	Escalate bool // defer, and escalate (C8.3, C8.5)
	Reason   string
	Liveness string
	Leaf     runtime.Provider
}

// fenceDestructive evaluates req's legs for its row's runtime on sp.
func fenceDestructive(ctx context.Context, sp runtime.Provider, req fenceRequest, now time.Time) fenceVerdict {
	name := strings.TrimSpace(req.Row.SessionName)
	leaf, _, known := runtime.ResolveBackend(sp, name)
	if !known || name == "" {
		return fenceVerdict{Reason: fenceRouteUnknown}
	}
	v := fenceVerdict{Leaf: leaf}
	if req.Legs&legLiveness != 0 {
		v.Liveness = leafLiveness(ctx, leaf, name)
		switch v.Liveness {
		case fenceLivenessPresent:
		case fenceLivenessUnobservable:
			v.Reason = fenceLivenessUnknown
			return v
		default:
			v.Reason = fenceNothingToStop
			return v
		}
	}
	if req.Legs&legToken != 0 {
		if v.Reason, v.Escalate = tokenLeg(leaf, name, req.Row); v.Reason != "" {
			return v
		}
	}
	if req.Legs&legAttach != 0 {
		if v.Reason, v.Escalate = attachLeg(ctx, leaf, name, req.Escalates, now); v.Reason != "" {
			return v
		}
	}
	if req.Legs&legPending != 0 {
		switch boundedPending(ctx, leaf, name) {
		case pendingInteractionYes:
			v.Reason = fencePending
			return v
		case pendingInteractionUnknown:
			v.Reason = fencePendingUnknown
			return v
		}
	}
	if req.Legs&legWork != 0 {
		has, err := false, errors.New("no work reader")
		if req.Work != nil {
			has, err = req.Work(ctx)
		}
		switch {
		case err != nil:
			v.Reason = fenceWorkUnknown
			return v
		case has:
			v.Reason = fenceHasWork
			return v
		}
	}
	v.Proceed = true
	return v
}

// stopFenced fences req and, if it proceeds, stops the runtime through the
// fenced leaf, then confirms the stop through the composite (C8.8) and drops
// the name's route on every hop that holds one, as the composite's own Stop
// would have. confirmed reports a confirmed stop; a fence that held stops
// nothing and confirms nothing, except that nothing to stop is confirmed only
// by a three-outcome absent read.
func stopFenced(ctx context.Context, sp runtime.Provider, req fenceRequest, now time.Time) (v fenceVerdict, confirmed bool) {
	v = fenceDestructive(ctx, sp, req, now)
	name := strings.TrimSpace(req.Row.SessionName)
	switch {
	case v.Proceed:
	case v.Reason == fenceNothingToStop:
		return v, confirmStopped(ctx, sp, name, time.Now())
	default:
		return v, false
	}
	if err := v.Leaf.Stop(name); err != nil && !errors.Is(err, runtime.ErrSessionNotFound) {
		v.Proceed, v.Reason = false, fenceStopFailed
		return v, false
	}
	if !confirmStopped(ctx, sp, name, time.Now()) {
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

// confirmStopped reports a confirmed stop (C8.8, v5 F3): a complete,
// error-free fresh read of name through the composite sp, taken after since
// (the last stop issued), that reports no live pane: not present, or present
// as a corpse. The composite falls through to its other backend only on a
// confirmed absence, so a stale route (mc-zndi7.24) cannot confirm on the
// wrong backend. A provider any backend of which answers only bool liveness
// never confirms.
func confirmStopped(ctx context.Context, sp runtime.Provider, name string, since time.Time) bool {
	live, cause := freshLiveness(ctx, sp, name, since)
	return cause == "" && !live.Running
}

// causeLivenessUnsupported refuses a fresh read from a backend that cannot
// give a three-outcome answer; causeLivenessUnknown one with no answer.
const causeLivenessUnsupported = "liveness-unsupported"

// freshLiveness is v5 O1's fresh read of name through sp: LL2's
// ObserveLivenessBoundedSince, so a tmux read reflects a refresh that began
// at or after since, never the cached snapshot (whose server check can read
// a live server as dead). cause is empty only for a complete, error-free
// answer from a provider that observes liveness with errors on every
// backend.
func freshLiveness(ctx context.Context, sp runtime.Provider, name string, since time.Time) (runtime.Liveness, string) {
	if !threeOutcome(sp) {
		return runtime.Liveness{}, causeLivenessUnsupported
	}
	live, status, err := runtime.ObserveLivenessBoundedSince(ctx, sp, name, nil, since, fenceProbeTimeout)
	if status != runtime.ObservationComplete || err != nil {
		return runtime.Liveness{}, causeLivenessUnknown
	}
	return live, ""
}

// threeOutcome reports whether sp, and every backend under it, observes
// liveness with errors.
func threeOutcome(sp runtime.Provider) bool {
	if _, ok := sp.(runtime.LivenessObserverWithError); !ok {
		return false
	}
	if b, ok := sp.(runtime.BackendsProvider); ok {
		for _, backend := range b.Backends() {
			if !threeOutcome(backend.Provider) {
				return false
			}
		}
	}
	return true
}

// leafLiveness is L1 on leaf: present, absent or unknown from a hardened
// leaf; present or absent-unconfirmed from a bool-only one; unknown when the
// probe errs or runs past its bound.
func leafLiveness(ctx context.Context, leaf runtime.Provider, name string) string {
	_, hardened := leaf.(runtime.LivenessObserverWithError)
	live, status, err := runtime.ObserveLivenessBounded(ctx, leaf, name, nil, fenceProbeTimeout)
	switch {
	case status != runtime.ObservationComplete || err != nil:
		return fenceLivenessUnobservable
	case live.Running:
		return fenceLivenessPresent
	case hardened:
		return fenceLivenessAbsent
	default:
		return fenceLivenessUnconfirmed
	}
}

// tokenLeg is L2 (§8.1, C0.8): only a matching GC_INSTANCE_TOKEN passes. A
// different non-empty token or a foreign GC_SESSION_ID is a mismatch, which
// never passes. A token that is absent, unreadable or unsupported, or any
// token on an unhardened leaf (statically unverifiable until its Appendix A
// items land), defers and escalates (C8.3), attributable by name or not; the
// named exceptions that accept attribution evaluate it themselves. It
// returns the reason that holds the action, or "".
func tokenLeg(leaf runtime.Provider, name string, row session.Info) (reason string, escalate bool) {
	sid, sidErr := leaf.GetMeta(name, "GC_SESSION_ID")
	if sid = strings.TrimSpace(sid); sidErr == nil && sid != "" && sid != row.ID {
		return fenceTokenMismatch, false
	}
	token, err := leaf.GetMeta(name, "GC_INSTANCE_TOKEN")
	token = strings.TrimSpace(token)
	_, hardened := leaf.(runtime.LivenessObserverWithError)
	switch {
	case errors.Is(err, runtime.ErrSessionNotFound):
		return fenceNothingToStop, false
	case err == nil && token != "" && token != strings.TrimSpace(row.InstanceToken):
		return fenceTokenMismatch, false
	case err != nil || !hardened:
		return fenceTokenUnverifiable, true
	case token == "":
		return fenceTokenAbsent, true
	}
	return "", false
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
// bound expires first; the probe's goroutine is then abandoned.
func boundedProbe[T any](ctx context.Context, probe func() T) (T, bool) {
	ctx, cancel := context.WithTimeout(ctx, fenceProbeTimeout)
	defer cancel()
	out := make(chan T, 1)
	go func() { out <- probe() }()
	select {
	case v := <-out:
		return v, true
	case <-ctx.Done():
		var zero T
		return zero, false
	}
}
