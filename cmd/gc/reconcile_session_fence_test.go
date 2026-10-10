package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/session"
)

// The destructive-action fence's tests (CONTRACT §8, P4 spec §3.5) over the
// fence test provider (P4 N12): runtime.Fake with three-outcome liveness and
// injectable liveness errors, composed under the production auto router,
// fenced as an effect fences: on the transaction's fresh reads (fenceRun).

// fenceOpts are a fenceRun's options: an escalation class, the L5 reader
// (none answers nil), the stop, the clock (time.Now when zero; clock, when
// set, overrides it), and the legs the transaction reads (req's when
// none).
type fenceOpts struct {
	escalates, stop bool
	work            func() *txWork
	now             time.Time
	clock           func() time.Time
	read            *fenceLegs
}

// fenceRun is the fence as an effect runs it: the transaction's fresh reads
// of req's runtime on sp (readRuntime, then readLegs on the read's leaf),
// the verdict on them, and, with o.stop, the Call's fenced stop.
func fenceRun(t *testing.T, sp runtime.Provider, req fenceRequest, o fenceOpts) (fenceVerdict, bool) {
	t.Helper()
	ctx, name := context.Background(), req.Row.SessionName
	if o.now.IsZero() {
		o.now = time.Now()
	}
	if o.work == nil {
		o.work = func() *txWork { return nil }
	}
	clock := o.clock
	if clock == nil {
		clock = func() time.Time { return o.now }
	}
	legs := req.Legs
	if o.read != nil {
		legs = *o.read
	}
	rt, _ := readRuntime(ctx, sp, nil, name, clock(), clock)
	var f txFence
	if rt != nil && rt.leaf != nil {
		f = readLegs(ctx, rt.leaf, name, needs{Legs: legs & (legAttach | legPending | legWork), Escalates: o.escalates}, o.now, o.now, o.work)
	}
	v := fenceDestructive(rt, f, req)
	if !o.stop {
		return v, false
	}
	return stopFenced(ctx, sp, v, nil, clock)
}

// fenceLeaf is a hardened leaf: runtime.Fake (a reporter: it reports
// attachment through an error probe) plus error-bearing liveness.
type fenceLeaf struct {
	*runtime.Fake
	LivenessErrors map[string]error
}

func newFenceLeaf() *fenceLeaf {
	return &fenceLeaf{Fake: runtime.NewFake(), LivenessErrors: map[string]error{}}
}

// LocalIdentitySidecar makes the leaf's identity readable, as acp's and
// subprocess's are (readRuntimeIdentity).
func (*fenceLeaf) LocalIdentitySidecar() bool { return true }

// LivenessReadsFresh makes its liveness read fresh by construction, as
// acp's and subprocess's are (freshReadable).
func (*fenceLeaf) LivenessReadsFresh() bool { return true }

func (l *fenceLeaf) ObserveLivenessWithError(name string, _ []string) (runtime.Liveness, error) {
	if err := l.LivenessErrors[name]; err != nil {
		return runtime.Liveness{}, err
	}
	running := l.IsRunning(name)
	return runtime.Liveness{Running: running, Alive: running}, nil
}

// terminalLeaf is a hardened leaf with a terminal it cannot report: the
// embedded interface hides the Fake's attach error probe and interactions.
type terminalLeaf struct {
	runtime.Provider
	leaf *fenceLeaf
}

func newTerminalLeaf() *terminalLeaf {
	l := newFenceLeaf()
	return &terminalLeaf{Provider: l, leaf: l}
}

func (l *terminalLeaf) ObserveLivenessWithError(name string, pn []string) (runtime.Liveness, error) {
	return l.leaf.ObserveLivenessWithError(name, pn)
}

func (*terminalLeaf) LocalIdentitySidecar() bool { return true }

func (*terminalLeaf) LivenessReadsFresh() bool { return true }

func (l *terminalLeaf) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{CanAttachTTY: true, CanReportActivity: true}
}

// boolLeaf is an unhardened leaf (k8s, ssh, exec, herdr): bool liveness, no
// attach probe, and the capabilities k8s and ssh report.
type boolLeaf struct{ runtime.Provider }

func (boolLeaf) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{CanReportActivity: true}
}

// fenceRow is row id whose runtime rt_<id> carries token tok.
func fenceRow(id, tok string) session.Info {
	return session.Info{ID: id, Template: "worker", SessionName: "rt_" + id, SessionNameMetadata: "rt_" + id, InstanceToken: tok}
}

// startRuntime starts name on leaf with the given runtime metadata.
func startRuntime(t *testing.T, leaf *runtime.Fake, name string, meta map[string]string) {
	t.Helper()
	if err := leaf.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	for k, v := range meta {
		if err := leaf.SetMeta(name, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

func ours(id string) map[string]string {
	return map[string]string{"GC_SESSION_ID": id, "GC_INSTANCE_TOKEN": "tok-" + id}
}

// Kills: trusting the composite's capabilities or probing anything while
// the route is unknown (R28): every leg is unknown and nothing is stopped.
func TestFenceUnknownRouteAllLegsUnknown(t *testing.T) {
	tmux, acp := newFenceLeaf(), newFenceLeaf()
	startRuntime(t, tmux.Fake, "rt_a", ours("a"))
	sp := auto.New(tmux, acp) // never seeded: the default route is not known
	v, confirmed := fenceRun(t, sp, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull}, fenceOpts{stop: true})
	if v.Proceed || v.Reason != fenceRouteUnknown || confirmed {
		t.Fatalf("verdict %+v confirmed=%v, want route_unknown and nothing done", v, confirmed)
	}
	if before := 3; len(tmux.Calls) != before || len(acp.Calls) != 0 { // Start and two SetMeta
		t.Fatalf("the fence probed under an unknown route: %v %v", tmux.Calls[before:], acp.Calls)
	}
}

// Kills: a terminal-no-report backend read as detached through the bool
// IsAttached fallback, so an operator's pane is killed (P4 F6, C8.1); a k8s
// or ssh leaf read as no-terminal; an escalation class proceeding.
func TestFenceTerminalNoReportUsesQuietWindowNotBoolAttach(t *testing.T) {
	now := time.Now()
	term := newTerminalLeaf()
	startRuntime(t, term.leaf.Fake, "rt_a", ours("a"))
	term.leaf.Activity = map[string]time.Time{"rt_a": now.Add(-time.Minute)}
	req := fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull}
	if v, _ := fenceRun(t, term, req, fenceOpts{now: now}); v.Proceed || v.Reason != fenceTerminalNotQuiet {
		t.Fatalf("recent activity: %+v, want a quiet-window defer", v)
	}
	term.leaf.Activity["rt_a"] = now.Add(-fenceQuietWindow - time.Second)
	if v, _ := fenceRun(t, term, req, fenceOpts{now: now}); !v.Proceed {
		t.Fatalf("quiet past the window: %+v, want proceed", v)
	}
	if v, _ := fenceRun(t, term, req, fenceOpts{now: now, escalates: true}); v.Proceed || !v.Escalate {
		t.Fatalf("escalation class: %+v, want escalate, never proceed", v)
	}

	k8s := boolLeaf{runtime.NewFake()}
	startRuntime(t, k8s.Provider.(*runtime.Fake), "rt_b", ours("b"))
	if reason, _ := attachLeg(context.Background(), k8s, "rt_b", false, now); reason != fenceTerminalNotQuiet {
		t.Fatalf("unhardened leaf with no activity: L3 %q, want terminal-no-report's defer", reason)
	}
}

// Kills: an attach probe error read as detached on a reporter backend.
func TestFenceReporterAttachErrorHolds(t *testing.T) {
	leaf := newFenceLeaf()
	startRuntime(t, leaf.Fake, "rt_a", ours("a"))
	req := fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull}
	for _, c := range []struct {
		err  error
		want bool
	}{
		{runtime.ErrRuntimeUnavailable, false},
		{errors.New("display-message: no server"), false},
		{runtime.ErrSessionNotFound, true},
		{nil, true},
	} {
		leaf.AttachedErrors = map[string]error{"rt_a": c.err}
		if v, _ := fenceRun(t, leaf, req, fenceOpts{}); v.Proceed != c.want {
			t.Errorf("attach error %v: %+v, want proceed=%v", c.err, v, c.want)
		}
	}
	leaf.AttachedErrors = nil
	leaf.SetAttached("rt_a", true)
	if v, _ := fenceRun(t, leaf, req, fenceOpts{}); v.Proceed || v.Reason != fenceAttached {
		t.Fatalf("attached: %+v, want a defer", v)
	}
}

// Kills: L2 passing on any verdict but Current (v5 F1, O2; INC-025): a stop
// of a newer incarnation (NewerSelf) or of a stale one before its rekey
// (StaleSelf), a foreign runtime, an empty or unread token, or a token read
// on an unhardened leaf trusted; a legacy-adopted runtime carrying the row's
// token refused (v5.2 M2); and a non-match that does not escalate (C8.3).
func TestFenceTokenLegPassesOnlyOnCurrent(t *testing.T) {
	poolName := PoolSessionName("worker", "p")
	row := fenceRow("a", "tok-a")
	row.Generation = "2"
	cases := []struct {
		name    string
		row     session.Info
		meta    map[string]string
		metaErr error
		want    string // "" = proceed
	}{
		{"current", row, ours("a"), nil, ""},
		{"the row's token under another session id", row, map[string]string{"GC_SESSION_ID": "z", "GC_INSTANCE_TOKEN": "tok-a"}, nil, ""},
		{"legacy-adopted, no session id", row, map[string]string{"GC_INSTANCE_TOKEN": "tok-a"}, nil, ""},
		{"foreign", row, map[string]string{"GC_SESSION_ID": "z", "GC_INSTANCE_TOKEN": "tok-z"}, nil, fenceTokenMismatch},
		{"stale self", row, map[string]string{"GC_SESSION_ID": "a", "GC_RUNTIME_EPOCH": "2", "GC_INSTANCE_TOKEN": "tok-old"}, nil, fenceTokenMismatch},
		{"newer self", row, map[string]string{"GC_SESSION_ID": "a", "GC_RUNTIME_EPOCH": "3", "GC_INSTANCE_TOKEN": "tok-new"}, nil, fenceTokenMismatch},
		{"own session id, epoch unreadable", row, map[string]string{"GC_SESSION_ID": "a", "GC_INSTANCE_TOKEN": "tok-old"}, nil, fenceTokenMismatch},
		{"no session id, another token", row, map[string]string{"GC_INSTANCE_TOKEN": "tok-z"}, nil, fenceTokenMismatch},
		{"absent, own session id", row, map[string]string{"GC_SESSION_ID": "a"}, nil, fenceTokenAbsent},
		{"ownerless, owned pool name", session.Info{ID: "p", Template: "worker", SessionName: poolName, SessionNameMetadata: poolName, InstanceToken: "tok-p"}, nil, nil, fenceTokenAbsent},
		{"unsupported runtime metadata", row, nil, runtime.ErrMetaUnsupported, fenceTokenUnverifiable},
		{"unreadable", row, nil, errors.New("tmux: server busy"), fenceTokenUnverifiable},
	}
	for _, c := range cases {
		leaf := newFenceLeaf()
		name := c.row.SessionName
		startRuntime(t, leaf.Fake, name, c.meta)
		if c.metaErr != nil {
			leaf.GetMetaErrors = map[string]map[string]error{name: {"GC_INSTANCE_TOKEN": c.metaErr, "GC_SESSION_ID": c.metaErr}}
		}
		v, _ := fenceRun(t, leaf, fenceRequest{Row: c.row, Legs: legsFull}, fenceOpts{})
		if v.Proceed != (c.want == "") || v.Reason != c.want {
			t.Errorf("%s: %+v, want reason %q", c.name, v, c.want)
		}
		if v.Escalate != (c.want == fenceTokenAbsent || c.want == fenceTokenUnverifiable) {
			t.Errorf("%s: escalate=%v; only an unverified token escalates (C8.3)", c.name, v.Escalate)
		}
	}
	// A leaf that cannot read fresh proves nothing: the fence holds on it
	// whatever legs it keeps, and stops nothing (no nil leaf reaches Stop).
	k8s := boolLeaf{runtime.NewFake()}
	startRuntime(t, k8s.Provider.(*runtime.Fake), "rt_b", ours("b"))
	if v, _ := fenceRun(t, k8s, fenceRequest{Row: fenceRow("b", "tok-b"), Legs: legsFull}, fenceOpts{}); v.Proceed || v.Reason != fenceLivenessUnknown {
		t.Fatalf("unhardened leaf: %+v, want liveness_unknown", v)
	}
	for _, legs := range []fenceLegs{legsFull &^ legLiveness, legAttach} { // whatever legs it keeps
		if v, _ := fenceRun(t, k8s, fenceRequest{Row: fenceRow("b", "tok-b"), Legs: legs}, fenceOpts{stop: true}); v.Proceed || v.Reason != fenceLivenessUnknown {
			t.Fatalf("unhardened leaf, legs %b: %+v, want liveness_unknown and nothing stopped", legs, v)
		}
	}
}

// Kills: the GetMeta/Stop split (session_wake.go verifiedStop): probing one
// backend and stopping through the composite, whose Stop falls through to
// the other backend.
func TestFenceProbesAndStopsThroughSameLeaf(t *testing.T) {
	tmux, acp := newFenceLeaf(), newFenceLeaf()
	startRuntime(t, acp.Fake, "rt_a", ours("a"))
	startRuntime(t, tmux.Fake, "rt_a", map[string]string{"GC_SESSION_ID": "other"}) // a namesake on the default backend
	sp := auto.New(tmux, acp)
	sp.SeedRoutes([]string{"rt_a"})
	v, confirmed := fenceRun(t, sp, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull}, fenceOpts{stop: true})
	if !v.Proceed || v.Leaf != runtime.Provider(acp) {
		t.Fatalf("verdict %+v, want the ACP leaf fenced", v)
	}
	if acp.CountCalls("Stop", "rt_a") != 1 || tmux.CountCalls("Stop", "rt_a") != 0 || tmux.CountCalls("GetMeta", "rt_a") != 0 {
		t.Fatal("probes and the stop did not all go through the routed ACP leaf")
	}
	if confirmed {
		t.Fatal("confirmed a stop while the default backend still runs the name")
	}

	// A failing Stop on the fenced leaf ends there: the composite's Stop
	// would fall through to the default backend's namesake.
	startRuntime(t, acp.Fake, "rt_a", ours("a"))
	acp.StopErrors = map[string]error{"rt_a": errors.New("acp: stop refused")}
	if v, _ := fenceRun(t, sp, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull}, fenceOpts{stop: true}); v.Reason != fenceStopFailed {
		t.Fatalf("failing stop: %+v, want stop_failed", v)
	}
	if tmux.CountCalls("Stop", "rt_a") != 0 {
		t.Fatal("a failed stop on the fenced leaf fell through to the namesake on the other backend")
	}
}

// Kills: confirming a stop (C8.8, R27) from Stop returning nil, from a
// bool-only absence, through a stale route (mc-zndi7.24), or on a name that
// runs on the backend it is not routed to.
func TestConfirmedStopNeedsThreeOutcomeAbsentViaComposite(t *testing.T) {
	ctx := context.Background()
	t.Run("stop returned nil but the runtime stayed", func(t *testing.T) {
		leaf := newFenceLeaf()
		startRuntime(t, leaf.Fake, "rt_a", ours("a"))
		leaf.StopLeavesRunning = map[string]bool{"rt_a": true}
		if v, confirmed := fenceRun(t, leaf, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull}, fenceOpts{stop: true}); confirmed || v.Reason != fenceStopUnconfirmed {
			t.Fatalf("%+v confirmed=%v, want stop_unconfirmed", v, confirmed)
		}
	})
	t.Run("bool-only absence", func(t *testing.T) {
		leaf := boolLeaf{runtime.NewFake()}
		if confirmStopped(ctx, leaf, nil, "rt_a", time.Now()) {
			t.Fatal("a bool liveness answer confirmed a stop")
		}
	})
	t.Run("stale route", func(t *testing.T) {
		tmux, acp := newFenceLeaf(), newFenceLeaf()
		startRuntime(t, tmux.Fake, "rt_a", ours("a")) // really on the default backend
		sp := auto.New(tmux, acp)
		sp.SeedRoutes([]string{"rt_a"}) // but routed to ACP
		if confirmStopped(ctx, sp, nil, "rt_a", time.Now()) {
			t.Fatal("a stale route's absence on the ACP leaf confirmed the stop")
		}
		if err := tmux.Stop("rt_a"); err != nil {
			t.Fatal(err)
		}
		if !confirmStopped(ctx, sp, nil, "rt_a", time.Now()) {
			t.Fatal("three-outcome absence on every backend must confirm")
		}
	})
	t.Run("liveness unknown", func(t *testing.T) {
		leaf := newFenceLeaf()
		leaf.LivenessErrors["rt_a"] = runtime.ErrRuntimeUnavailable
		if confirmStopped(ctx, leaf, nil, "rt_a", time.Now()) {
			t.Fatal("an unknown liveness confirmed a stop")
		}
	})
	t.Run("plain liveness error", func(t *testing.T) {
		leaf := newFenceLeaf()
		leaf.LivenessErrors["rt_a"] = errors.New("tmux: server busy") // a complete observation that erred
		if confirmStopped(ctx, leaf, nil, "rt_a", time.Now()) {
			t.Fatal("a liveness error confirmed a stop")
		}
	})
	t.Run("a composite with a bool-only backend", func(t *testing.T) {
		sp := auto.New(boolLeaf{runtime.NewFake()}, newFenceLeaf())
		sp.SeedRoutes(nil)
		if confirmStopped(ctx, sp, nil, "rt_a", time.Now()) {
			t.Fatal("a composite whose default backend answers only bool liveness confirmed a stop")
		}
	})
	t.Run("nothing to stop, but running on another backend", func(t *testing.T) {
		tmux, acp := newFenceLeaf(), newFenceLeaf()
		startRuntime(t, tmux.Fake, "rt_a", ours("a")) // really on the default backend
		sp := auto.New(tmux, acp)
		sp.SeedRoutes([]string{"rt_a"}) // but routed to ACP, where it is absent
		v, confirmed := fenceRun(t, sp, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull}, fenceOpts{stop: true})
		if v.Reason != fenceLivenessUnknown || confirmed || tmux.CountCalls("Stop", "rt_a") != 0 {
			t.Fatalf("%+v confirmed=%v, want liveness_unknown (absent on the leaf, running elsewhere), nothing stopped", v, confirmed)
		}
		if err := tmux.Stop("rt_a"); err != nil {
			t.Fatal(err)
		}
		if _, confirmed := fenceRun(t, sp, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull}, fenceOpts{stop: true}); !confirmed {
			t.Fatal("nothing to stop with a three-outcome absence on every backend must confirm")
		}
	})
}

// Kills: unknown L1 read as nothing to stop (which a caller may confirm and
// close on), rather than holding the action.
func TestFenceUnknownLivenessIsNotNothingToStop(t *testing.T) {
	leaf := newFenceLeaf()
	startRuntime(t, leaf.Fake, "rt_a", ours("a"))
	leaf.LivenessErrors["rt_a"] = runtime.ErrRuntimeUnavailable
	v, confirmed := fenceRun(t, leaf, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull}, fenceOpts{stop: true})
	if v.Proceed || v.Reason != fenceLivenessUnknown || confirmed || leaf.CountCalls("Stop", "rt_a") != 0 {
		t.Fatalf("%+v confirmed=%v, want liveness_unknown and nothing stopped", v, confirmed)
	}
}

// Kills: a confirmed stop through the ACP leaf that leaves auto's route
// entry behind (auto.Stop would have dropped it), so the name keeps routing
// to ACP; and a stop that was not confirmed dropping the route.
func TestFenceConfirmedStopUnroutesLeafRoute(t *testing.T) {
	tmux, acp := newFenceLeaf(), newFenceLeaf()
	startRuntime(t, acp.Fake, "rt_a", ours("a"))
	sp := auto.New(tmux, acp)
	sp.SeedRoutes([]string{"rt_a"})
	acp.StopLeavesRunning = map[string]bool{"rt_a": true}
	if _, confirmed := fenceRun(t, sp, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull}, fenceOpts{stop: true}); confirmed || sp.RouteFor("rt_a").Label != "acp" {
		t.Fatalf("unconfirmed stop: confirmed=%v route=%s, want the ACP route kept", confirmed, sp.RouteFor("rt_a").Label)
	}
	acp.StopLeavesRunning = nil
	v, confirmed := fenceRun(t, sp, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull}, fenceOpts{stop: true})
	if !v.Proceed || !confirmed {
		t.Fatalf("%+v confirmed=%v, want a confirmed stop", v, confirmed)
	}
	if r := sp.RouteFor("rt_a"); r.Label != "default" || !r.Known {
		t.Fatalf("route after a confirmed stop: %s known=%v, want the ACP entry dropped", r.Label, r.Known)
	}
}

// Kills: L4 reading unsupported as unknown (blocking every action on
// runtimes with no interactions) or a probe error as no (CONTRACT §8.1).
func TestPendingUnsupportedPassesUnknownHolds(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{runtime.ErrInteractionUnsupported, ""},
		{runtime.ErrSessionNotFound, ""},
		{errors.New("acp: socket refused"), fencePendingUnknown},
	} {
		leaf := newFenceLeaf()
		startRuntime(t, leaf.Fake, "rt_a", ours("a"))
		leaf.PendingErrors = map[string]error{"rt_a": c.err}
		if v, _ := fenceRun(t, leaf, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull}, fenceOpts{}); v.Reason != c.want {
			t.Errorf("pending error %v: %+v, want reason %q", c.err, v, c.want)
		}
	}
	leaf := newFenceLeaf()
	startRuntime(t, leaf.Fake, "rt_a", ours("a"))
	leaf.PendingInteractions = map[string]*runtime.PendingInteraction{"rt_a": {RequestID: "r", Kind: "approval"}}
	if v, _ := fenceRun(t, leaf, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull}, fenceOpts{}); v.Reason != fencePending {
		t.Fatalf("pending interaction: %+v, want a defer", v)
	}
	// L5, where an action requires it: a read error counts as has-work (P-2).
	work := func() *txWork { return &txWork{Err: errors.New("store down")} }
	leaf.PendingInteractions = nil
	if v, _ := fenceRun(t, leaf, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull | legWork}, fenceOpts{work: work}); v.Reason != fenceWorkUnknown {
		t.Fatalf("work read error: %+v, want work_unknown", v)
	}
	// A request that keeps L5 with no work read fails closed.
	if v, _ := fenceRun(t, leaf, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull | legWork}, fenceOpts{}); v.Proceed || v.Reason != fenceWorkUnknown {
		t.Fatalf("no work read: %+v, want work_unknown", v)
	}
	if v, _ := fenceRun(t, leaf, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull | legWork}, fenceOpts{work: func() *txWork { return &txWork{} }}); v.Reason != fenceHasWork {
		t.Fatalf("work found: %+v, want has_work", v)
	}
}

// Kills L2 reading identity off a runtime that is not present (v5 O2) and an
// absent runtime confirmed as nothing to stop with L1 waived: with L1 waived,
// a name never started, and one whose runtime is gone but whose sidecar
// still carries the row's identity (acp's leftover sidecar, which reads
// Current), both hold as unverifiable and stop nothing.
func TestFenceL1WaivedAbsentRuntimeNeverProceeds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		leftover bool
	}{{"never started", false}, {"gone, sidecar left", true}} {
		leaf := newFenceLeaf()
		if tc.leftover {
			startRuntime(t, leaf.Fake, "rt_a", ours("a"))
			if err := leaf.Stop("rt_a"); err != nil {
				t.Fatal(err)
			}
		}
		stops := leaf.CountCalls("Stop", "rt_a")
		req := fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull &^ legLiveness}
		v, confirmed := fenceRun(t, leaf, req, fenceOpts{stop: true})
		if v.Proceed || v.Reason == fenceNothingToStop || v.Reason != fenceTokenUnverifiable || confirmed || leaf.CountCalls("Stop", "rt_a") != stops {
			t.Errorf("%s: %+v confirmed=%v, want held as token_unverifiable, nothing stopped", tc.name, v, confirmed)
		}
	}
}

// Kills the read's classes mapped onto the wrong L1 or L2 answer: a runtime
// whose agent is dead is still present (to stop), a corpse is nothing to
// stop and confirms a stop, identity off a corpse proves nothing with L1
// waived, and an identity read across two objects (replaced between the
// read's brackets) is unverifiable.
func TestFenceReadsTheRuntimeClass(t *testing.T) {
	row := fenceRow("a", "tok-a")
	for _, c := range []struct {
		name    string
		set     func(sp *simProvider, leaf *recordingLeaf)
		legs    fenceLegs
		reason  string // "" proceeds
		stopped bool   // confirmStopped
	}{
		{"agent dead", func(sp *simProvider, _ *recordingLeaf) { sp.rts["rt_a"].zombie = true }, legLiveness, "", false},
		{"corpse", func(sp *simProvider, _ *recordingLeaf) { sp.rts["rt_a"].corpse = true }, legLiveness, fenceNothingToStop, true},
		{"corpse, L1 waived", func(sp *simProvider, _ *recordingLeaf) { sp.rts["rt_a"].corpse = true }, legToken, fenceTokenUnverifiable, true},
		{"replaced mid-read", func(sp *simProvider, leaf *recordingLeaf) {
			leaf.during = func() {
				if len(leaf.sinces) == 2 {
					sp.put("rt_a", "a", "1", "tok-a") // another object under the name
				}
			}
		}, legLiveness | legToken, fenceTokenUnverifiable, false},
	} {
		sp := newSimProvider()
		sp.put("rt_a", "a", "1", "tok-a")
		leaf := &recordingLeaf{simProvider: sp}
		c.set(sp, leaf)
		v, _ := fenceRun(t, leaf, fenceRequest{Row: row, Legs: c.legs}, fenceOpts{})
		if v.Proceed != (c.reason == "") || v.Reason != c.reason {
			t.Errorf("%s: %+v, want reason %q", c.name, v, c.reason)
		}
		leaf.during = nil
		if got := confirmStopped(context.Background(), leaf, nil, "rt_a", time.Now()); got != c.stopped {
			t.Errorf("%s: stop confirmed %t, want %t", c.name, got, c.stopped)
		}
	}
}

// Kills an unread kept leg read as a pass (review must-fix): a spec whose
// needs read fewer legs than the request keeps holds the fence on each one
// it did not read, attached, pending-unknown or work-unknown, over a
// detached runtime with nothing pending.
func TestFenceUnreadLegsHold(t *testing.T) {
	leaf := newFenceLeaf()
	startRuntime(t, leaf.Fake, "rt_a", ours("a"))
	free := func() *txWork { return &txWork{Free: true} }
	for _, c := range []struct {
		read fenceLegs
		want string
	}{
		{0, fenceAttached},
		{legAttach, fencePendingUnknown},
		{legAttach | legPending, fenceWorkUnknown},
		{legAttach | legPending | legWork, ""},
	} {
		read := c.read
		v, _ := fenceRun(t, leaf, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull | legWork}, fenceOpts{read: &read, work: free})
		if v.Proceed != (c.want == "") || v.Reason != c.want {
			t.Errorf("legs read %b: %+v, want reason %q", c.read, v, c.want)
		}
	}
}

// stopStamp is a leaf whose Stop moves the clock on, so a read since the
// stop is told from one since before it.
type stopStamp struct {
	*recordingLeaf
	clock *time.Time
}

func (s stopStamp) Stop(name string) error {
	*s.clock = s.clock.Add(time.Minute)
	return s.recordingLeaf.Stop(name)
}

// Kills a C8.8 confirm or L1 read that is not fresh (review item 2): a
// runtime up since the leaf's last listing reads present and confirms no
// stop (a cached read, or the deleted threeOutcome confirm, reads it
// absent), and the confirm after a stop reads since the stop.
func TestFenceConfirmReadsFreshSinceTheStop(t *testing.T) {
	sp := newSimProvider()
	sp.listed() // absent when the leaf last listed
	sp.put("rt_a", "a", "1", "tok-a")
	leaf := &recordingLeaf{simProvider: sp}
	if v, _ := fenceRun(t, leaf, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legLiveness}, fenceOpts{}); v.Liveness != fenceLivenessPresent {
		t.Fatalf("up since the listing: %+v, want present", v)
	}
	if confirmStopped(context.Background(), leaf, nil, " rt_a ", time.Now()) {
		t.Fatal("a runtime up since the listing confirmed a stop (its name padded)")
	}
	at := gatherNow
	stamped := stopStamp{recordingLeaf: leaf, clock: &at}
	leaf.sinces = nil
	v, confirmed := fenceRun(t, stamped, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legLiveness}, fenceOpts{stop: true, clock: func() time.Time { return at }})
	if !v.Proceed || !confirmed || len(leaf.sinces) == 0 || !leaf.sinces[len(leaf.sinces)-1].Equal(gatherNow.Add(time.Minute)) {
		t.Fatalf("%+v confirmed=%t, sinces %v; want the confirm read since the stop (%v)", v, confirmed, leaf.sinces, gatherNow.Add(time.Minute))
	}
}

// Kills a stop that skips C8.7's re-check: a reporter leaf detached at the
// fence and attached by the stop is not stopped.
func TestFenceRechecksAttachBeforeTheStop(t *testing.T) {
	leaf := newFenceLeaf()
	startRuntime(t, leaf.Fake, "rt_a", ours("a"))
	v, _ := fenceRun(t, leaf, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull}, fenceOpts{})
	if !v.Proceed {
		t.Fatalf("fence: %+v, want proceed", v)
	}
	leaf.SetAttached("rt_a", true) // an operator attached between the fence and the stop
	if v, confirmed := stopFenced(context.Background(), leaf, v, nil, time.Now); v.Proceed || v.Reason != fenceAttached || confirmed || leaf.CountCalls("Stop", "rt_a") != 0 {
		t.Fatalf("%+v confirmed=%t, want held attached and nothing stopped", v, confirmed)
	}
}
