package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The restart and relaunch through the production effect pass
// (newEffectPass, its World stripped of the provider) on a live tmux-profile
// leaf (C7b2-2 review; the reviewer's repros).

// driftOn decides a drifted row, named or ordinary, with a full or a
// launch-only drift, on admittedBeginOn's leaf.
func driftOn(t *testing.T, named, launchOnly bool) (*effectPass, intent, beads.Store, *fenceLeaf) {
	t.Helper()
	withoutA7(t)
	tp := TemplateParams{TemplateName: "worker", Command: "run"}
	var meta []string
	if named {
		meta = append(meta, "configured_named_session", "true", "configured_named_identity", "worker", "configured_named_mode", "always")
	}
	cfg := sessionCoreConfigForHashInfo(tp, censusRowOf(t, drainRow(append([]string{"last_woke_at", rowAt(-time.Hour)}, meta...)...)).Info)
	meta = append(meta, "started_config_hash", runtime.FingerprintVersion+":stale", "session_key", "k-1")
	if launchOnly {
		meta = append(meta, "started_provision_hash", runtime.ProvisionFingerprint(cfg), "started_launch_hash", runtime.FingerprintVersion+":old-launch")
	}
	p, it, store, leaf := admittedBeginOn(t, meta, func(e *selectionEntry) { e.InDesired, e.Desired = true, desireKeep }, func(w *World) {
		w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{}}
		for _, row := range w.Census.Rows { // every row resolves (the store seeds one beside the drifted row)
			w.Templates.entries[templateMemoKeyOf(row.Info)] = templateResolution{TP: tp}
		}
	})
	if p.World.Env.SP != nil {
		t.Fatal("the effects' World holds the provider: not the production pass")
	}
	return p, it, store, leaf
}

func approval(l *fenceLeaf) {
	l.SetPendingInteraction("s-gc-1", &runtime.PendingInteraction{RequestID: "r", Kind: "approval"})
}

// Kills a relaunch the effect never runs (the review's must-fix 1, M01; the
// relaunch capability a pass fact, Relaunch, through the stripped World): a
// launch-only drift relaunches in the warm box and records its baseline,
// named or not; a failed relaunch restarts a named row (stopped, reset to
// start-pending) and begins an ordinary row's config-drift drain.
func TestDriftRelaunchThroughTheEffectPass(t *testing.T) {
	for _, tc := range []struct {
		name          string
		named, fails  bool
		stops         int
		state, drains string
	}{
		{name: "ordinary, landed"},
		{name: "named, landed", named: true},
		{name: "named, failed: the restart", named: true, fails: true, stops: 1, state: "start-pending"},
		{name: "ordinary, failed: the drain", fails: true, drains: drainConfigDrift},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, it, store, leaf := driftOn(t, tc.named, true)
			if it.Kind != intentDriftRelaunch {
				t.Fatalf("decided (%q, %q), want the relaunch", it.Kind, it.Reason)
			}
			if tc.fails {
				leaf.RelaunchErrors = map[string]error{"s-gc-1": errors.New("box gone")}
			}
			s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
			got, _ := store.Get(it.Key.ID)
			stale := got.Metadata["started_config_hash"] == runtime.FingerprintVersion+":stale"
			switch {
			case s.Outcome != settledLanded || leaf.CountCalls("Relaunch", "s-gc-1") != 1 || stops(leaf) != tc.stops:
				t.Fatalf("settled %+v, relaunches %d, stops %d; want landed, one relaunch, %d stops", s, leaf.CountCalls("Relaunch", "s-gc-1"), stops(leaf), tc.stops)
			case !tc.fails && stale:
				t.Fatalf("row %v: want the relaunch baseline", got.Metadata)
			case tc.state != "" && got.Metadata["state"] != tc.state:
				t.Fatalf("state %q, want %q", got.Metadata["state"], tc.state)
			case got.Metadata["drain_intent_reason"] != tc.drains:
				t.Fatalf("stop request %q, want %q", got.Metadata["drain_intent_reason"], tc.drains)
			}
		})
	}
}

// Kills a relaunch over a human or a prompt, and a deferral that writes
// (the review's must-fix 2, should-fix 4, M10): a pending interaction or an
// attach, read fresh, refuses the relaunch (ordinary) or the restart
// (named) with its deferral cause, relaunching or stopping nothing; three
// deferred runs write no row.
func TestDriftDeferralRefusesAndWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name              string
		named, launchOnly bool
		set               func(*fenceLeaf)
		cause             string
	}{
		{"an ordinary relaunch, pending", false, true, approval, causeDriftDeferred + fencePending},
		{"an ordinary relaunch, attached", false, true, func(l *fenceLeaf) { l.SetAttached("s-gc-1", true) }, causeDriftDeferred + fenceAttached},
		{"a named restart, pending", true, false, approval, causeDriftDeferred + fencePending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, it, store, leaf := driftOn(t, tc.named, tc.launchOnly)
			tc.set(leaf)
			before, _ := store.Get(it.Key.ID)
			for range 3 {
				if s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil); s.Outcome != settledRefused || s.Cause != tc.cause {
					t.Fatalf("settled %+v, want refused %q", s, tc.cause)
				}
			}
			after, _ := store.Get(it.Key.ID)
			if after.Revision != before.Revision || leaf.CountCalls("Relaunch", "s-gc-1")+stops(leaf) != 0 {
				t.Fatalf("revision %d -> %d, relaunches %d, stops %d: want nothing written or done", before.Revision, after.Revision, leaf.CountCalls("Relaunch", "s-gc-1"), stops(leaf))
			}
		})
	}
}

// Kills a restart stop that ignores a prompt that came up after the
// deferral read (should-fix 3, the Full fence) and a reset written over an
// unconfirmed stop (M02): the stop refuses on a fresh pending interaction;
// a stop the provider refuses leaves the row as it was.
func TestDriftRestartStopIsFenced(t *testing.T) {
	p, it, _, leaf := driftOn(t, true, false)
	p.seam = func(_ context.Context, s txSeam, _ intent, section, _ int) error {
		if s == seamAfterReads && section == 0 {
			approval(leaf)
		}
		return nil
	}
	if s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil); s.Outcome != settledRefused || stops(leaf) != 0 {
		t.Fatalf("pending after the deferral: settled %+v, stops %d; want refused, nothing stopped", s, stops(leaf))
	}
	p, it, store, leaf := driftOn(t, true, false)
	leaf.StopErrors["s-gc-1"] = errors.New("busy")
	s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
	if got, _ := store.Get(it.Key.ID); s.Outcome == settledLanded || got.Metadata["state"] == "start-pending" {
		t.Fatalf("a refused stop: settled %+v, state %q; want no reset", s, got.Metadata["state"])
	}
}

// Kills a relaunch that skips the re-decide or the L3 re-read before
// Relaunch (C8.7): a runtime gone between the deferral and the relaunch refuses as re-decided; an
// attach that lands after the relaunch section's reads holds the Relaunch,
// with no fallback.
func TestRelaunchRedecidesAndRereadsAttach(t *testing.T) {
	p, it, _, leaf := driftOn(t, false, true)
	p.seam = func(_ context.Context, s txSeam, _ intent, section, _ int) error {
		if s == seamAfterReads && section == 0 {
			return leaf.Stop("s-gc-1")
		}
		return nil
	}
	if s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil); s.Outcome != settledRefused || s.Cause != causeRedecided || leaf.CountCalls("Relaunch", "s-gc-1") != 0 {
		t.Fatalf("a runtime gone: settled %+v, relaunches %d; want refused %q, none", s, leaf.CountCalls("Relaunch", "s-gc-1"), causeRedecided)
	}
	p, it, store, leaf := driftOn(t, false, true)
	p.seam = func(_ context.Context, s txSeam, _ intent, section, _ int) error {
		if s == seamBeforeCall && section == 1 {
			leaf.SetAttached("s-gc-1", true)
		}
		return nil
	}
	s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
	if got, _ := store.Get(it.Key.ID); s.Outcome != settledRefused || leaf.CountCalls("Relaunch", "s-gc-1")+stops(leaf) != 0 || got.Metadata["drain_intent_reason"] != "" {
		t.Fatalf("attached before Relaunch: settled %+v, relaunches %d, stops %d, stop request %q; want refused, nothing done", s, leaf.CountCalls("Relaunch", "s-gc-1"), stops(leaf), got.Metadata["drain_intent_reason"])
	}
}

// Kills a relaunch baseline a heartbeat refuses (should-fix 5): the agent's
// held_until landing during the relaunch does not hold the baseline, so the
// next pass does not relaunch again.
func TestRelaunchBaselineSurvivesAHeartbeat(t *testing.T) {
	p, it, store, leaf := driftOn(t, false, true)
	p.seam = func(_ context.Context, s txSeam, _ intent, section, _ int) error {
		if s == seamAfterCall && section == 1 {
			return store.SetMetadata(it.Key.ID, "held_until", rowAt(45*time.Minute))
		}
		return nil
	}
	s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
	if got, _ := store.Get(it.Key.ID); s.Outcome != settledLanded || leaf.CountCalls("Relaunch", "s-gc-1") != 1 || got.Metadata["started_config_hash"] == runtime.FingerprintVersion+":stale" {
		t.Fatalf("settled %+v, started_config_hash %q: want the baseline written", s, got.Metadata["started_config_hash"])
	}
}

// Kills a relaunch over work (the round-two review's R1; SESS-559): an
// ordinary seat whose live L5 read holds work, claimed since the pass,
// refuses the relaunch; work claimed during a failed relaunch refuses the
// fallback drain (should-fix 6).
func TestRelaunchHoldsOnLiveWork(t *testing.T) {
	p, it, store, leaf := driftOn(t, false, true)
	assignWork(t, store, it.Key.ID)
	if s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil); s.Outcome != settledRefused || s.Cause != causeHasWork || leaf.CountCalls("Relaunch", "s-gc-1") != 0 {
		t.Fatalf("work since the pass: settled %+v, relaunches %d; want refused %q, none", s, leaf.CountCalls("Relaunch", "s-gc-1"), causeHasWork)
	}
	p, it, store, leaf = driftOn(t, false, true)
	leaf.RelaunchErrors = map[string]error{"s-gc-1": errors.New("box gone")}
	p.seam = func(_ context.Context, s txSeam, _ intent, section, _ int) error {
		if s == seamAfterCall && section == 1 {
			assignWork(t, store, it.Key.ID)
		}
		return nil
	}
	s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
	if got, _ := store.Get(it.Key.ID); s.Outcome != settledRefused || s.Cause != causeHasWork || got.Metadata["drain_intent_reason"] != "" || leaf.CountCalls("Relaunch", "s-gc-1") != 1 {
		t.Fatalf("settled %+v, stop request %q: want refused %q and no drain", s, got.Metadata["drain_intent_reason"], causeHasWork)
	}
}

// Kills a drift Call made without the runtime lease record (the
// round-two review's R5; D02, D04): the record is not held in the deferral
// section, is held through the restart's stop Call and the relaunch's
// Relaunch, and is cleared after.
func TestDriftLeaseRecordSpansTheCalls(t *testing.T) {
	for _, tc := range []struct {
		name       string
		named      bool
		launchOnly bool
	}{{"the restart's stop", true, false}, {"the relaunch", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			p, it, store, _ := driftOn(t, tc.named, tc.launchOnly)
			holder := func() string { b, _ := store.Get(it.Key.ID); return b.Metadata[session.RuntimeLeaseHolderKey] }
			var deferring, calling string
			p.seam = func(_ context.Context, s txSeam, _ intent, section, _ int) error {
				switch {
				case s == seamAfterReads && section == 0:
					deferring = holder()
				case s == seamBeforeCall && calling == "":
					calling = holder()
				}
				return nil
			}
			s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
			if s.Outcome != settledLanded || deferring != "" || calling == "" || holder() != "" {
				t.Fatalf("settled %+v; holder deferring %q, at the Call %q, after %q: want none, held, cleared", s, deferring, calling, holder())
			}
		})
	}
}

// Kills a lease record taken past the effect's deadline (the round-two
// review's R2): a deadline that passes in the deferral section writes no
// record and no row.
func TestDriftRecordNotTakenPastTheDeadline(t *testing.T) {
	p, it, store, leaf := driftOn(t, true, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.seam = func(_ context.Context, s txSeam, _ intent, section, _ int) error {
		if s == seamAfterRowRead && section == 0 {
			cancel()
		}
		return nil
	}
	before, _ := store.Get(it.Key.ID)
	s := runTx(ctx, p, it, effectSpecs[it.Kind], nil)
	if after, _ := store.Get(it.Key.ID); after.Revision != before.Revision || s.Outcome == settledLanded || stops(leaf) != 0 {
		t.Fatalf("settled %+v, revision %d -> %d, stops %d: want nothing written or stopped", s, before.Revision, after.Revision, stops(leaf))
	}
}

// Kills a relaunch fallback's stop without the full fence (D16): a named
// row whose relaunch fails and whose prompt comes up during it refuses the
// restart's stop.
func TestRelaunchFallbackStopIsFenced(t *testing.T) {
	p, it, _, leaf := driftOn(t, true, true)
	leaf.RelaunchErrors = map[string]error{"s-gc-1": errors.New("box gone")}
	p.seam = func(_ context.Context, s txSeam, _ intent, section, _ int) error {
		if s == seamAfterCall && section == 1 {
			approval(leaf)
		}
		return nil
	}
	if s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil); s.Outcome != settledRefused || stops(leaf) != 0 {
		t.Fatalf("settled %+v, stops %d; want refused, nothing stopped", s, stops(leaf))
	}
}

// Kills the activity read's expiry and its reportability dropped (M05,
// M06): a leaf whose activity read runs past its bound reads expired; one
// that cannot report activity reads unreportable.
func TestReadLegsActivity(t *testing.T) {
	leaf := newFenceLeaf(runtime.ProfileTmux)
	startRuntime(t, leaf.Fake, "s-gc-1", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if f := readLegs(ctx, leaf.P, "s-gc-1", needs{Activity: true}, gatherNow, gatherNow, nil); f.Activity == nil || f.Activity.Reportable && !errors.Is(f.Activity.Err, errProbeExpired) {
		t.Fatalf("a read past its bound: %+v, want expired", f.Activity)
	}
	if f := readLegs(context.Background(), noActivityLeaf{leaf.P}, "s-gc-1", needs{Activity: true}, gatherNow, gatherNow, nil); f.Activity == nil || f.Activity.Reportable {
		t.Fatalf("a leaf that cannot report activity: %+v, want unreportable", f.Activity)
	}
}

// noActivityLeaf is a leaf whose sleep capability reports no activity.
type noActivityLeaf struct{ runtime.Provider }

func (noActivityLeaf) SleepCapability(string) runtime.SessionSleepCapability {
	return runtime.SessionSleepCapabilityDisabled
}
