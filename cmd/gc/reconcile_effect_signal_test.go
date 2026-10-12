package main

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The drain signal's tests (CONTRACT v5 D1, D2, F4; C6b1).

// intentBegunAt is a controller half of reason at generation 3, begun ago
// before gatherNow.
func intentBegunAt(reason string, ago time.Duration) []string {
	return []string{session.DrainIntentReasonKey, reason, session.DrainIntentAtKey, rowAt(-ago), session.DrainIntentIncarnationKey, "3"}
}

// withPatrol sets the city's patrol interval, as gather reads it into the
// World, and its drift drain timeout.
func withPatrol(patrol, drift string) func(*World) {
	return func(w *World) {
		w.Env = &reconcileEnv{Cfg: &config.City{Daemon: config.DaemonConfig{PatrolInterval: patrol, DriftDrainTimeout: drift}}}
		w.Patrol = w.Env.patrol()
	}
}

// TestSignalNeedsGraceAndANewerPass (R30). Kills an immediate signal: a
// drain begun inside its patrol grace holds, with a pass due when the grace
// ends; past it, the signal writes legacy's stop-pending projection.
func TestSignalNeedsGraceAndANewerPass(t *testing.T) {
	it, due := decideDrain(t, intentBegunAt(drainOrphaned, 10*time.Second), undesired(drainOrphaned), withPatrol("30s", ""))
	if it.Kind != "" || it.Reason != decideStopActive || !due.Equal(gatherNow.Add(20*time.Second)) {
		t.Fatalf("inside the grace: %+v due %v, want held until %v", it, due, gatherNow.Add(20*time.Second))
	}
	it, _ = decideDrain(t, intentBegunAt(drainOrphaned, 30*time.Second), undesired(drainOrphaned), withPatrol("30s", ""))
	if it.Kind != intentSignal || it.Reason != decideDrainSignal+drainOrphaned || it.Patch["state_reason"] != session.DrainAckStopPendingReason {
		t.Fatalf("past the grace: %+v, want the stop-pending signal", it)
	}
}

// TestSignalPerReasonAuthorization (D2). Kills a drift, execution-stalled or
// intent drain that never reaches stop-pending, and one signaled without its
// authorization or under Keep.
func TestSignalPerReasonAuthorization(t *testing.T) {
	sleep := func(e *selectionEntry) { e.Desired, e.Reason = desireSleep, reasonIdleSleep }
	keep := func(e *selectionEntry) { e.Desired, e.Reason = desireKeep, reasonPartialRetain }
	for _, c := range []struct {
		name   string
		meta   []string
		entry  func(*selectionEntry)
		signal string
	}{
		{"execution-stalled", intentBegunAt(executionStalledDrainReason, time.Minute), nil, intentSignal},
		{"config-drift, the drift unresolvable", intentBegunAt(drainConfigDrift, time.Minute), nil, intentSignal},
		{"an intent that stands", append(intentBegunAt("rate-limit", time.Minute), "sleep_intent", "rate-limit"), nil, intentSignal},
		{"orphaned, still undesired", intentBegunAt(drainOrphaned, time.Minute), undesired(drainOrphaned), intentSignal},
		{"idle, still asleep", intentBegunAt(drainIdle, time.Minute), sleep, intentSignalFresh},
		{"idle under Keep", intentBegunAt(drainIdle, time.Minute), keep, ""},
		{"orphaned under Keep", intentBegunAt(drainOrphaned, time.Minute), keep, ""},
		{"idle-respawn, C6d's row", intentBegunAt(idleRespawnDrainReason, time.Minute), sleep, ""},
	} {
		it, _ := decideDrain(t, c.meta, c.entry)
		if it.Kind != c.signal || (c.signal == "" && it.Reason != decideStopActive) {
			t.Errorf("%s: %+v, want kind %q", c.name, it, c.signal)
		}
	}
}

// TestDriftSignalGraceCappedByDriftDrainTimeout (S-16). Kills a drift drain
// that waits past drift_drain_timeout: its grace is min(patrol,
// drift_drain_timeout), and every other reason waits out the patrol.
func TestDriftSignalGraceCappedByDriftDrainTimeout(t *testing.T) {
	long := withPatrol("10m", "2m")
	if it, _ := decideDrain(t, intentBegunAt(drainConfigDrift, 3*time.Minute), nil, long); it.Kind != intentSignal {
		t.Fatalf("drift past drift_drain_timeout: %+v, want the signal", it)
	}
	if it, due := decideDrain(t, intentBegunAt(drainConfigDrift, time.Minute), nil, long); it.Kind != "" || !due.Equal(gatherNow.Add(time.Minute)) {
		t.Fatalf("drift inside drift_drain_timeout: %+v due %v, want held a minute", it, due)
	}
	if it, _ := decideDrain(t, intentBegunAt(drainOrphaned, 3*time.Minute), undesired(drainOrphaned), long); it.Kind != "" {
		t.Fatalf("orphaned inside its patrol: %+v, want held", it)
	}
}

// TestAckSignalsAtOnce (A11; D2's ack row, grace 0). Kills an acked drain
// that waits out a grace, and a bare ack (no controller half) never
// signaled: both are A11's ack signal, resting on the runtime.
func TestAckSignalsAtOnce(t *testing.T) {
	ack := []string{session.DrainAckIncarnationKey, "3", session.DrainAckAtKey, rowAt(-time.Second)}
	for _, c := range []struct {
		name string
		meta []string
	}{
		{"acked inside the grace", append(intentBegunAt(drainIdle, time.Second), ack...)},
		{"a bare ack", ack},
	} {
		it, _ := decideDrain(t, c.meta, func(e *selectionEntry) { e.Desired = desireWake })
		if it.Kind != intentSignalAck || it.Rests != restRuntime || it.Patch["state"] != string(session.StateDraining) {
			t.Errorf("%s: kind %s rests %d patch %v, want the ack signal at once, resting on the runtime", c.name, it.Kind, it.Rests, it.Patch)
		}
	}
}

// runSignal decides gc row with meta and entry, checks the kind, and runs
// the signal's effect after outside writes.
func runSignal(t *testing.T, meta []string, entry func(*selectionEntry), kind string, outside func(beads.Store, intent)) (settlement, beads.Bead) {
	t.Helper()
	p, it, store, _ := admittedBeginOn(t, meta, entry)
	if it.Kind != kind {
		t.Fatalf("decided %+v, want %s", it, kind)
	}
	if outside != nil {
		outside(store, it)
	}
	s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
	got, err := store.Get(it.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, got
}

// TestSignalFreshAttachDefers (R26, C8.9). Kills an idle or no-wake-reason
// signal that skips its fresh L3 or L4: attached or pending refuses with its
// cause and writes nothing; detached with nothing pending signals.
func TestSignalFreshAttachDefers(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*fenceLeaf)
		cause string
	}{
		{"attached", func(l *fenceLeaf) { l.SetAttached("s-gc-1", true) }, fenceAttached},
		{"pending", func(l *fenceLeaf) {
			l.SetPendingInteraction("s-gc-1", &runtime.PendingInteraction{RequestID: "r", Kind: "approval"})
		}, fencePending},
		{"detached, nothing pending", func(*fenceLeaf) {}, ""},
	} {
		p, it, store, leaf := admittedBeginOn(t, intentBegunAt(drainIdle, time.Minute), func(e *selectionEntry) { e.Desired, e.Reason = desireSleep, reasonIdleSleep })
		if it.Kind != intentSignalFresh {
			t.Fatalf("%s: %+v, want the fresh signal", c.name, it)
		}
		c.setup(leaf)
		s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
		got, _ := store.Get(it.Key.ID)
		pending := isDrainAckStopPendingInfo(censusRowOf(t, got).Info)
		switch {
		case c.cause != "" && (s.Outcome != settledRefused || s.Cause != c.cause || pending):
			t.Fatalf("%s: %+v (stop-pending %v), want refused %q and no signal", c.name, s, pending, c.cause)
		case c.cause == "" && (s.Outcome != settledLanded || !pending):
			t.Fatalf("%s: %+v, want the signal landed", c.name, s)
		}
	}
}

// TestSignalPatchIsLegacyReadable (I7, D1). Kills a signal legacy cannot
// read after a rollback, and one that drops the request: the landed row is
// legacy's stop-pending state, with drain_at for D6's alert, and the next
// pass reads it as the signaled request, so A20 leaves it to A4.
func TestSignalPatchIsLegacyReadable(t *testing.T) {
	s, got := runSignal(t, intentBegunAt(drainOrphaned, time.Minute), undesired(drainOrphaned), intentSignal, nil)
	if s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	row := censusRowOf(t, got)
	if !isDrainAckStopPendingInfo(row.Info) || got.Metadata["drain_at"] != gatherNow.Format(time.RFC3339) {
		t.Fatalf("row %v, want legacy's stop-pending state with drain_at", got.Metadata)
	}
	if req, ok := activeStop(row); !ok || req.Phase != stopSignaled || req.Reason != drainOrphaned || req.Synthesized {
		t.Fatalf("activeStop = %+v %v, want the orphaned request signaled", req, ok)
	}
}

// TestSignalRefusesWhenAccessorSaysNoRequest (D1 rule 2, R16). Kills a
// signal over an operator's suspend, and one sent after the drain was voided
// or canceled: the effect decides again on the fresh row and writes nothing.
func TestSignalRefusesWhenAccessorSaysNoRequest(t *testing.T) {
	for _, c := range []struct {
		name  string
		write map[string]string
	}{
		{"an operator's suspend", map[string]string{"state": string(session.StateSuspended), "sleep_intent": "user-hold"}},
		{"the drain voided", stopCancelPatch()},
	} {
		s, got := runSignal(t, intentBegunAt(drainOrphaned, time.Minute), undesired(drainOrphaned), intentSignal, func(store beads.Store, it intent) {
			if err := store.SetMetadataBatch(it.Key.ID, c.write); err != nil {
				t.Fatal(err)
			}
		})
		if s.Outcome == settledLanded || got.Metadata["state_reason"] == session.DrainAckStopPendingReason {
			t.Errorf("%s: %+v row %v, want no signal", c.name, s, got.Metadata)
		}
	}
}

// TestSignalWaitNeverPinnedBySkew (#7607 review; INC-003's rule that skew
// never pins a row). Kills a requested drain held for the whole skew of a
// drain_intent_at a clock wrote ahead of now, and one an unparseable stamp
// holds: either is due at once, and no wait exceeds one grace.
func TestSignalWaitNeverPinnedBySkew(t *testing.T) {
	ahead := []string{session.DrainIntentReasonKey, drainOrphaned, session.DrainIntentAtKey, rowAt(time.Hour), session.DrainIntentIncarnationKey, "3"}
	garbled := []string{session.DrainIntentReasonKey, drainOrphaned, session.DrainIntentAtKey, "yesterday-ish", session.DrainIntentIncarnationKey, "3"}
	for name, meta := range map[string][]string{"an hour ahead": ahead, "unparseable": garbled} {
		if it, _ := decideDrain(t, meta, undesired(drainOrphaned), withPatrol("30s", "")); it.Kind != intentSignal {
			t.Errorf("%s: %+v, want the signal at once", name, it)
		}
	}
	if it, due := decideDrain(t, intentBegunAt(drainOrphaned, time.Second), undesired(drainOrphaned), withPatrol("30s", "")); it.Kind != "" || due.After(gatherNow.Add(30*time.Second)) {
		t.Fatalf("just begun: %+v due %v, want held at most one grace", it, due)
	}
}

// TestSignalPremiseIsTheDrainBasis (#7607 review, mutant G9). Kills a signal
// behind the default premise, which an agent's heartbeat starves: a
// heartbeat between the pass and the CAS lands the signal; an operator's
// intent or a wake request refuses it.
func TestSignalPremiseIsTheDrainBasis(t *testing.T) {
	for _, c := range []struct {
		name  string
		write map[string]string
		lands bool
	}{
		{"a heartbeat", map[string]string{"held_until": gatherNow.Add(time.Minute).Format(time.RFC3339)}, true},
		{"an operator's intent", map[string]string{"sleep_intent": "user-hold"}, false},
		{"a wake request", map[string]string{"wake_request": "explicit"}, false},
	} {
		s, got := runSignal(t, intentBegunAt(drainOrphaned, time.Minute), undesired(drainOrphaned), intentSignal, func(store beads.Store, it intent) {
			if err := store.SetMetadataBatch(it.Key.ID, c.write); err != nil {
				t.Fatal(err)
			}
		})
		signaled := isDrainAckStopPendingInfo(censusRowOf(t, got).Info)
		if landed := s.Outcome == settledLanded; landed != c.lands || signaled != c.lands {
			t.Errorf("%s: %+v (signaled %v), want landed=%v", c.name, s, signaled, c.lands)
		}
	}
}

// ackMeta is a row ack of incarnation inc that landed a minute before
// gatherNow.
func ackMeta(inc string) []string {
	return []string{session.DrainAckIncarnationKey, inc, session.DrainAckAtKey, rowAt(-time.Minute)}
}

// awakeWanted is a Wake entry: nothing but the ack stops the row.
func awakeWanted(e *selectionEntry) { e.Desired = desireWake }

// TestAckForOldGenerationIgnored (INC-002, D1 rule 1). Kills a stale ack
// stopping a re-woken N+1: an ack of generation 2 on a generation-3 row
// signals nothing, and one of generation 3 does.
func TestAckForOldGenerationIgnored(t *testing.T) {
	if it, _ := decideDrain(t, ackMeta("2"), awakeWanted); it.Kind == intentSignalAck || it.Kind == intentSignal {
		t.Fatalf("an ack of generation 2: %+v, want no signal", it)
	}
	if it, _ := decideDrain(t, ackMeta("3"), awakeWanted); it.Kind != intentSignalAck {
		t.Fatalf("an ack of generation 3: %+v, want the ack signal", it)
	}
}

// TestAckVisibleNextPassWithoutEnvRead (I18, D5; and
// TestOperatorAckStopsTarget, §12.2 item 2). Kills an env read creeping
// back, and an operator's ack that does not stop its target: the runtime's
// legacy GC_DRAIN_ACK signals nothing, and the row ack E3's CLI writes, in
// the agent's form or an operator's, is the ack signal on the next pass.
func TestAckVisibleNextPassWithoutEnvRead(t *testing.T) {
	for _, operator := range []bool{false, true} {
		p, it, store, leaf := admittedBeginOn(t, nil, awakeWanted)
		if err := leaf.SetMeta("s-gc-1", "GC_DRAIN_ACK", "1"); err != nil {
			t.Fatal(err)
		}
		if it.Kind == intentSignalAck {
			t.Fatalf("operator %t: decided %+v on no row ack", operator, it)
		}
		token := "tok-3"
		if operator {
			token = ""
		}
		commit, err := checkDrainAckRow(store, it.Key.ID, operator, token, gatherNow)
		if err == nil {
			err = commit()
		}
		if err != nil {
			t.Fatalf("operator %t: the CLI's ack: %v", operator, err)
		}
		w := *p.World
		w.Census = readCensus(t, gatherNow, censusLegs(rowLeg, store))
		w.Obs = observedOwn(&w, it.Key.ID)
		if next, _ := decideRow(&w, p.Alloc, it.Key); next.Kind != intentSignalAck {
			t.Errorf("operator %t: the next pass decided %+v, want the ack signal", operator, next)
		}
	}
}

// TestAckOlderThanItsRuntimeIsVoid (C6a ruling 3, v5.9g; J27 B). Kills an
// ack that stops a runtime Manager.ensureRunning restarted at the same
// generation since: on the effect's fresh read, a runtime object created
// after the ack voids the ack half instead of signaling, and a drain the
// controller began stays requested. A runtime from before the ack, in the
// ack's second, or whose creation the backend does not report is
// signaled.
func TestAckOlderThanItsRuntimeIsVoid(t *testing.T) {
	ackAt := gatherNow.Add(-time.Minute).Truncate(time.Second)
	created := func(d time.Duration) string { return strconv.FormatInt(ackAt.Add(d).Unix(), 10) }
	for _, c := range []struct {
		name, created string
		meta          []string
		void          bool
		replaced      string // the second read's creation: a runtime replaced mid-read
	}{
		{"created before the ack", created(-time.Minute), nil, false, ""},
		{"created in the ack's second", created(0), nil, false, ""},
		{"creation unreported", "", nil, false, ""},
		{"created after the ack", created(time.Second), nil, true, ""},
		{"created after the ack, a drain begun", created(time.Second), intentBegunAt(drainConfigDrift, 2*time.Minute), true, ""},
		{"replaced by one created after the ack mid-read", created(-time.Minute), nil, true, created(time.Second)},
	} {
		p, it, store, _ := admittedBeginOn(t, append(ackMeta("3"), c.meta...), awakeWanted)
		if it.Kind != intentSignalAck || it.Rests != restRuntime {
			t.Fatalf("%s: decided %+v, want the ack signal resting on the runtime", c.name, it)
		}
		obs := &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true, ObjectID: "$1", ObjectCreated: c.created}, env: map[string]string{"GC_SESSION_ID": it.Key.ID, "GC_INSTANCE_TOKEN": "tok-3"}}
		if err := obs.Fake.Start(context.Background(), "s-gc-1", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		if c.replaced != "" {
			reads := 0
			obs.read = func() {
				if reads++; reads == 2 {
					obs.l.ObjectID, obs.l.ObjectCreated = "$2", c.replaced
				}
			}
		}
		p.Runtime = obs.tmux()
		s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
		got, err := store.Get(it.Key.ID)
		if err != nil {
			t.Fatal(err)
		}
		row := censusRowOf(t, got)
		req, active := activeStop(row)
		switch {
		case s.Outcome != settledLanded:
			t.Errorf("%s: settlement %+v, want landed", c.name, s)
		case c.void && (isDrainAckStopPendingInfo(row.Info) || got.Metadata[session.DrainAckIncarnationKey] != "" || got.Metadata[session.DrainAckAtKey] != ""):
			t.Errorf("%s: row %v, want the ack half voided and no signal", c.name, got.Metadata)
		case c.void && len(c.meta) > 0 && (!active || req.Phase != stopRequested):
			t.Errorf("%s: request %+v (active %v), want the drain still requested", c.name, req, active)
		case !c.void && !isDrainAckStopPendingInfo(row.Info):
			t.Errorf("%s: row %v, want the signal", c.name, got.Metadata)
		}
	}
}

// TestAckWithoutAReadableTimeIsVoid (#7635 review ruling, A4). Kills an ack
// whose drain_ack_at is missing or unparseable standing: no fresh read can
// prove the runtime is the one that acked, so the ack half is voided, in
// the pass and on the effect's fresh row, and nothing is signaled.
func TestAckWithoutAReadableTimeIsVoid(t *testing.T) {
	for _, at := range []string{"", "not-a-time"} {
		meta := []string{session.DrainAckIncarnationKey, "3", session.DrainAckAtKey, at}
		p, it, store, _ := admittedBeginOn(t, meta, awakeWanted)
		if it.Kind != intentSignalAck || it.Reason != decideAckVoid || it.Rests != restRuntime {
			t.Fatalf("drain_ack_at %q: decided %+v, want the ack void resting on the runtime", at, it)
		}
		s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
		got, err := store.Get(it.Key.ID)
		if err != nil {
			t.Fatal(err)
		}
		if s.Outcome != settledLanded || got.Metadata[session.DrainAckIncarnationKey] != "" || isDrainAckStopPendingInfo(censusRowOf(t, got).Info) {
			t.Errorf("drain_ack_at %q: settlement %+v, row %v; want the ack voided and no signal", at, s, got.Metadata)
		}
	}
}

// TestOperatorResumeClearsAStaleAck (#7635 review should-fix 1, ruling
// (a)). Kills an ack that survives a same-generation restart on a backend
// that reports no creation (acp, subprocess): an operator's attach that
// starts the dead runtime before the next pass clears both halves of the
// stop request in its confirm, as PreWake does, so the next pass signals
// nothing; an attach to a runtime already running clears nothing, its ack
// still the running runtime's.
func TestOperatorResumeClearsAStaleAck(t *testing.T) {
	for _, running := range []bool{false, true} {
		store, _ := stampedMem(t, gate.Require)
		b, err := store.Create(drainRow(append([]string{"provider", "claude"}, slices.Concat(ackMeta("3"), intentBegunAt(drainConfigDrift, time.Minute))...)...))
		if err != nil {
			t.Fatal(err)
		}
		sp := runtime.NewFake() // no ObjectCreated: no fresh read can date the runtime
		if running {
			if err := sp.Start(context.Background(), "s-gc-1", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
		}
		city := t.TempDir()
		if err := session.NewManagerWithOptions(store, sp, session.WithCityPath(city)).Attach(context.Background(), testActorIn(t, session.ActorOperator, city), b.ID, "claude", runtime.Config{}); err != nil {
			t.Fatalf("running %t: attach: %v", running, err)
		}
		got, err := store.Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, active := activeStop(censusRowOf(t, got))
		switch {
		case !running && (active || got.Metadata[session.DrainAckIncarnationKey] != "" || got.Metadata[session.DrainIntentReasonKey] != ""):
			t.Errorf("a resume that started the runtime: row %v, want both halves of the stop request cleared", got.Metadata)
		case running && (got.Metadata[session.DrainAckIncarnationKey] != "3" || got.Metadata[session.DrainIntentReasonKey] != drainConfigDrift):
			t.Errorf("an attach to a running runtime: row %v, want the stop request kept", got.Metadata)
		}
	}
}

// attachRaceSP is a fake runtime whose first IsRunning and first Start run
// a hook: an injection point inside an operator's attach.
type attachRaceSP struct {
	*runtime.Fake
	onIsRunning, onStart func()
}

func (h *attachRaceSP) IsRunning(name string) bool {
	if f := h.onIsRunning; f != nil {
		h.onIsRunning = nil
		f()
	}
	return h.Fake.IsRunning(name)
}

func (h *attachRaceSP) Start(ctx context.Context, name string, cfg runtime.Config) error {
	err := h.Fake.Start(ctx, name, cfg)
	if f := h.onStart; f != nil {
		h.onStart = nil
		f()
	}
	return err
}

// attachRaceStore runs beforeClear once, before the first metadata batch
// that clears the stop request's controller half (the operator's confirm).
type attachRaceStore struct {
	*beads.MemStore
	beforeClear func()
}

func (s *attachRaceStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if _, ok := kvs[session.DrainIntentReasonKey]; ok && s.beforeClear != nil {
		f := s.beforeClear
		s.beforeClear = nil
		f()
	}
	return s.MemStore.SetMetadataBatch(id, kvs)
}

// TestSignalNeverStrandsAnOperatorStart (#7638, mc-ted51; review probes
// W1-W3). Kills a plain signal that lands inside an operator's attach (gc
// session attach, its own process: the controller's session mutation lock
// is not the operator's, withRowMutationLock passed through) and leaves the
// runtime the operator started stop-pending for the next pass's A4. The
// signal runs at three points of the real Manager.Attach: W1, between the
// operator's row read and its runtime lease (the attach then reads the row
// again under the lease and refuses, starting nothing); W2, right after the
// Start, and W3, just before the confirm's write, both under the lease,
// where the signal refuses busy. No point ends with a started runtime on a
// signaled row.
func TestSignalNeverStrandsAnOperatorStart(t *testing.T) {
	for _, at := range []string{"W1", "W2", "W3"} {
		t.Run(at, func(t *testing.T) {
			p, it, store, _ := admittedBeginOn(t, append([]string{"provider", "claude"}, intentBegunAt(drainOrphaned, time.Hour)...), undesired(drainOrphaned))
			if it.Kind != intentSignal {
				t.Fatalf("decided %+v, want the plain signal", it)
			}
			saved := withRowMutationLock
			withRowMutationLock = func(_ string, fn func() error) error { return fn() }
			t.Cleanup(func() { withRowMutationLock = saved })
			var sig settlement
			ran := false
			signal := func() { ran, sig = true, runTx(context.Background(), p, it, effectSpecs[it.Kind], nil) }
			sp := &attachRaceSP{Fake: runtime.NewFake()} // the runtime died after the pass observed it
			mstore := store
			switch at {
			case "W1":
				sp.onIsRunning = signal
			case "W2":
				sp.onStart = signal
			case "W3":
				mstore = &attachRaceStore{MemStore: store.(*beads.MemStore), beforeClear: signal}
			}
			city := p.World.CityPath
			err := session.NewManagerWithOptions(mstore, sp, session.WithCityPath(city)).Attach(context.Background(), testActorIn(t, session.ActorOperator, city), it.Key.ID, "claude", runtime.Config{})
			got, gerr := store.Get(it.Key.ID)
			if gerr != nil {
				t.Fatal(gerr)
			}
			req, active := activeStop(censusRowOf(t, got))
			running := sp.Fake.IsRunning("s-gc-1")
			switch {
			case !ran:
				t.Fatal("the signal never ran")
			case running && active && req.Phase == stopSignaled:
				t.Fatalf("attach err %v, signal %+v: a started runtime on a signaled row %v, which the next pass's A4 stops", err, sig, got.Metadata)
			case at == "W1" && (!errors.Is(err, session.ErrSessionStopping) || running):
				t.Fatalf("attach err %v, running %t: want the attach to refuse the row the signal stopped, starting nothing", err, running)
			case at != "W1" && (sig.Outcome != settledRefused || sig.Cause != causeNameBusy || err != nil):
				t.Fatalf("signal %+v, attach err %v: want the signal refused busy under the operator's lease and the attach done", sig, err)
			}
		})
	}
}

// TestOperatorStartClearsARequestBegunBeforeItsLease (#7638 review). Kills
// an operator's start that clears the stop request it read before its lease:
// a drain the controller begins between the attach's first read of the row
// and its lease is cleared by the start's confirm (the row read under the
// lease), so the next pass never signals and stops the runtime the operator
// started.
func TestOperatorStartClearsARequestBegunBeforeItsLease(t *testing.T) {
	store, _ := stampedMem(t, gate.Require)
	b, err := store.Create(drainRow("provider", "claude"))
	if err != nil {
		t.Fatal(err)
	}
	begin := func() {
		if err := store.SetMetadataBatch(b.ID, map[string]string(stopBeginPatch(newCensusRow(rowKeyOf(b.ID), sessionInfoFromBead(b)), drainOrphaned, gatherNow))); err != nil {
			t.Fatal(err)
		}
	}
	sp := &attachRaceSP{Fake: runtime.NewFake(), onIsRunning: begin}
	city := t.TempDir()
	if err := session.NewManagerWithOptions(store, sp, session.WithCityPath(city)).Attach(context.Background(), testActorIn(t, session.ActorOperator, city), b.ID, "claude", runtime.Config{}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, active := activeStop(censusRowOf(t, got)); active || !sp.Fake.IsRunning("s-gc-1") {
		t.Fatalf("row %v: want the runtime started and the request begun before the lease cleared", got.Metadata)
	}
}

// onFloor makes the entry a member of its template's min_active_sessions
// floor (the allocator's floor set) and a Wake entry.
func onFloor(e *selectionEntry) { e.Desired, e.Floor = desireWake, &floorView{Rank: 0, MinActive: 1} }

// floorWith is onFloor with the allocation wanting d of the row.
func floorWith(d desire) func(*selectionEntry) {
	return func(e *selectionEntry) { onFloor(e); e.Desired = d }
}

// TestFloorAckCancels (DRAIN-010, PAR-FLOORACK; sc-j27j0d). Kills a floor
// member's own idle ack stopping it, only for the floor to recreate it: a
// bare ack on a floor member cancels (its half voided, nothing signaled); an
// ack off the floor, or one a drain the controller began explains, signals;
// an ack older than its runtime is void first.
func TestFloorAckCancels(t *testing.T) {
	for _, c := range []struct {
		name   string
		meta   []string
		entry  func(*selectionEntry)
		reason string
	}{
		{"a bare ack on a floor member", ackMeta("3"), onFloor, decideFloorAckCancel},
		{"a bare ack off the floor", ackMeta("3"), awakeWanted, decideDrainSignal},
		{"an ack a controller drain explains, on the floor", append(ackMeta("3"), intentBegunAt(drainConfigDrift, time.Minute)...), onFloor, decideDrainSignal + drainConfigDrift},
		{"an ack without a time, on the floor", []string{session.DrainAckIncarnationKey, "3"}, onFloor, decideAckVoid},
		// Legacy cancels whatever the allocation wants of a floor member (M8).
		{"a bare ack on a Keep floor member", ackMeta("3"), floorWith(desireKeep), decideFloorAckCancel},
		{"a bare ack on a Sleep floor member", ackMeta("3"), floorWith(desireSleep), decideFloorAckCancel},
		{"a bare ack on a Drain floor member", ackMeta("3"), floorWith(desireDrain), decideFloorAckCancel},
		// An operator's `gc runtime drain` explains the ack (legacy's GC_DRAIN).
		{"an ack an operator's drain explains, on the floor", append(ackMeta("3"), session.OperatorDrainKey, "3"), onFloor, decideDrainSignal + drainOperator},
		{"an ack beside an operator's drain of an older generation, on the floor", append(ackMeta("3"), session.OperatorDrainKey, "2"), onFloor, decideFloorAckCancel},
		{"a bare ack on a floor member whose runtime is dead", ackMeta("3"), func(e *selectionEntry) { onFloor(e); e.Liveness = livenessDead }, decideDrainSignal},
	} {
		it, _ := decideDrain(t, c.meta, c.entry)
		switch {
		case it.Kind != intentSignalAck || it.Reason != c.reason:
			t.Errorf("%s: %s %q, want the ack's %q", c.name, it.Kind, it.Reason, c.reason)
		case c.reason == decideFloorAckCancel && (it.Patch["state"] != "" || it.Patch[session.DrainAckIncarnationKey] != "" || it.Patch[session.DrainAckAtKey] != "" || len(it.Patch) != 2):
			t.Errorf("%s: patch %v, want the ack half voided alone", c.name, it.Patch)
		}
	}
}

// TestFloorAckCancelKeepsTheSeatWarm (DRAIN-010, through runTx). Kills a
// cancel that stops or marks the seat: the effect lands the ack half's
// void on the fresh row, which is not stop-pending, and leaves the runtime
// running; off the floor the same ack signals.
func TestFloorAckCancelKeepsTheSeatWarm(t *testing.T) {
	for _, floor := range []bool{true, false} {
		entry := awakeWanted
		if floor {
			entry = onFloor
		}
		p, it, store, leaf := admittedBeginOn(t, ackMeta("3"), entry)
		s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
		got, err := store.Get(it.Key.ID)
		if err != nil {
			t.Fatal(err)
		}
		row := censusRowOf(t, got)
		_, active := activeStop(row)
		switch {
		case s.Outcome != settledLanded:
			t.Errorf("floor %t: settlement %+v, want landed", floor, s)
		case floor && (active || isDrainAckStopPendingInfo(row.Info) || got.Metadata[session.DrainAckIncarnationKey] != "" || !leaf.IsRunning("s-gc-1")):
			t.Errorf("floor: row %v (request active %v), running %v; want the ack canceled, the seat warm", got.Metadata, active, leaf.IsRunning("s-gc-1"))
		case !floor && !isDrainAckStopPendingInfo(row.Info):
			t.Errorf("off the floor: row %v, want the signal", got.Metadata)
		case floor && (s.Facts.Transition == nil || s.Facts.Transition.Reason != "min-floor" || s.Facts.Transition.Transition != "cancel"):
			t.Errorf("floor: transition %+v, want legacy's min-floor cancel", s.Facts.Transition)
		case !floor && s.Facts.Transition != nil:
			t.Errorf("off the floor: transition %+v, want none", s.Facts.Transition)
		}
	}
}
