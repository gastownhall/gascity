package main

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/session/sessiontest"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
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
		{"idle-respawn, no longer eligible (C6d)", intentBegunAt(idleRespawnDrainReason, time.Minute), sleep, intentDrainVoid},
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

// attachRaceStore runs beforeClear once, before the first metadata write
// that clears the stop request's controller half (the operator's confirm).
type attachRaceStore struct {
	*beads.MemStore
	beforeClear func()
}

func (s *attachRaceStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.race(kvs)
	return s.MemStore.SetMetadataBatch(id, kvs)
}

// UpdateIfMatch is the start's confirm fenced on its incarnation
// (session.Store.ConfirmIncarnation).
func (s *attachRaceStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	s.race(opts.Metadata)
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

func (s *attachRaceStore) race(kvs map[string]string) {
	if _, ok := kvs[session.DrainIntentReasonKey]; ok && s.beforeClear != nil {
		f := s.beforeClear
		s.beforeClear = nil
		f()
	}
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
// bare ack on a live floor member cancels (its half voided, nothing
// signaled), and on a dead one A11 proposes nothing and A18 starts it; an
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
	} {
		it, _ := decideDrain(t, c.meta, c.entry)
		switch {
		case it.Kind != intentSignalAck || it.Reason != c.reason:
			t.Errorf("%s: %s %q, want the ack's %q", c.name, it.Kind, it.Reason, c.reason)
		case c.reason == decideFloorAckCancel && (it.Patch["state"] != "" || it.Patch[session.DrainAckIncarnationKey] != "" || it.Patch[session.DrainAckAtKey] != "" || len(it.Patch) != 2):
			t.Errorf("%s: patch %v, want the ack half voided alone", c.name, it.Patch)
		}
	}
	// A dead floor seat's ack proposes nothing of A11's (§12.2 row 66): A18's
	// recycle restarts the seat in place with the ack on the row, so its
	// death is a drain's and the PreWake clears the request.
	if it, _ := decideDrain(t, ackMeta("3"), func(e *selectionEntry) { onFloor(e); e.Liveness = livenessDead }); it.Kind != intentStart || it.Patch[session.DrainAckIncarnationKey] != "" && it.Patch[session.DrainAckIncarnationKey] != "3" {
		t.Errorf("a bare ack on a dead floor member: %s %q patch %v, want A18's start, the ack left to its PreWake", it.Kind, it.Reason, it.Patch)
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

// signalWorkCases are the reasons whose signal reads L5 (v5.10a B), each with
// its entry and kind.
var signalWorkCases = []struct {
	reason string
	entry  func(*selectionEntry)
	kind   string
}{
	{reasonNoWake, func(e *selectionEntry) { e.Desired = desireSleep }, intentSignalFresh},
	{drainOrphaned, undesired(drainOrphaned), intentSignal},
}

// claimAfterPass is `gc hook --claim` landing after the pass's demand read:
// a work bead in progress, assigned to the row, stamped on it.
func claimAfterPass(t *testing.T) func(beads.Store, intent) {
	return func(store beads.Store, it intent) {
		t.Helper()
		w, err := store.Create(beads.Bead{Title: "w", Type: "task", Status: "in_progress", Assignee: it.Key.ID})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetMetadata(it.Key.ID, beadmeta.CurrentClaimBeadIDMetadataKey, w.ID); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSignalReReadsWorkLive (v5.10a B, DRAIN-005). Kills a no-wake-reason or
// orphaned signal that trusts the pass's demand read: a claim landing after
// the pass refuses has-work and writes nothing, so A4 never sees a signaled
// request; with no work the signal lands.
func TestSignalReReadsWorkLive(t *testing.T) {
	for _, c := range signalWorkCases {
		s, got := runSignal(t, intentBegunAt(c.reason, time.Minute), c.entry, c.kind, claimAfterPass(t))
		row := censusRowOf(t, got)
		if s.Outcome != settledRefused || s.Cause != causeHasWork || isDrainAckStopPendingInfo(row.Info) {
			t.Fatalf("%s, claimed after the pass: %+v, want refused %q and no signal", c.reason, s, causeHasWork)
		}
		if req, ok := activeStop(row); !ok || req.Phase != stopRequested {
			t.Fatalf("%s: activeStop after the refusal = %+v, %v; want still requested (A4 takes no stop)", c.reason, req, ok)
		}
		if s, got := runSignal(t, intentBegunAt(c.reason, time.Minute), c.entry, c.kind, nil); s.Outcome != settledLanded || !isDrainAckStopPendingInfo(censusRowOf(t, got).Info) {
			t.Fatalf("%s, no work: %+v, want the signal landed", c.reason, s)
		}
	}
}

// TestNoWakeSignalCountsOnlyAwakeWork (v5.10a B: DRAIN-005's awake work).
// Kills a signal L5 that counts open work the seat cannot be served
// (blocked), which legacy's read and A19's lens (workKeepsNoWakeRow) both
// ignore, so the request would stand refused forever: the signal lands.
func TestNoWakeSignalCountsOnlyAwakeWork(t *testing.T) {
	blocked := func(store beads.Store, it intent) {
		blocker, err := store.Create(beads.Bead{Title: "blocker", Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		w, err := store.Create(beads.Bead{Title: "w", Type: "task", Status: "open", Assignee: it.Key.ID})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(w.ID, blocker.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	s, got := runSignal(t, intentBegunAt(reasonNoWake, time.Minute), signalWorkCases[0].entry, intentSignalFresh, blocked)
	if s.Outcome != settledLanded || !isDrainAckStopPendingInfo(censusRowOf(t, got).Info) {
		t.Fatalf("blocked open work only: %+v, want the signal landed", s)
	}
}

// TestNoWakeSignalReadsWorkAfterAttach (v5.10a B: L5 last, after L3 and
// L4; staging J47). Kills a no-wake-reason signal that reads L5 before its
// fresh L3 or L4: a claim landing while either is read refuses has-work.
func TestNoWakeSignalReadsWorkAfterAttach(t *testing.T) {
	for _, leg := range []string{"L3", "L4"} {
		p, it, store, leaf := admittedBeginOn(t, intentBegunAt(reasonNoWake, time.Minute), signalWorkCases[0].entry)
		if it.Kind != intentSignalFresh {
			t.Fatalf("decided %+v, want the fresh signal", it)
		}
		var once sync.Once
		claim := func(string) { // claimed, not yet stamped on the row: only L5 can see it
			once.Do(func() {
				if _, err := store.Create(beads.Bead{Title: "w", Type: "task", Status: "in_progress", Assignee: it.Key.ID}); err != nil {
					t.Fatal(err)
				}
			})
		}
		if leg == "L3" {
			leaf.OnAttachRead = claim
		} else {
			leaf.OnPendingRead = claim
		}
		s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
		got, _ := store.Get(it.Key.ID)
		if s.Outcome != settledRefused || s.Cause != causeHasWork || isDrainAckStopPendingInfo(censusRowOf(t, got).Info) {
			t.Fatalf("claimed during the %s read: %+v, want refused %q and no signal", leg, s, causeHasWork)
		}
	}
}

// staleReadyLeg is a caching work leg whose cached ready set (Ready) is
// stale while its backing (Backing) and live lists see the store.
type staleReadyLeg struct {
	beads.Store
	stale beads.Store
}

func (l staleReadyLeg) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) { return l.stale.Ready(q...) }
func (l staleReadyLeg) Backing() beads.Store                              { return l.Store }

// TestSignalReadsReadyWorkLive (v5.10b-1). Kills a signal L5 whose ready-open
// half reads a cached ready set through the effect's read-only wrappers: an
// open bead assigned after the pass, visible only live, refuses has-work.
// The leg is synthetic (a stale Ready, a live Backing): CachingStore.Ready
// with a query already reads its backing, so this pins the belt-and-braces
// peel, not a production gap.
func TestSignalReadsReadyWorkLive(t *testing.T) {
	for _, c := range signalWorkCases {
		p, it, store, _ := admittedBeginOn(t, intentBegunAt(c.reason, time.Minute), c.entry)
		if it.Kind != c.kind {
			t.Fatalf("%s: decided %+v, want %s", c.reason, it, c.kind)
		}
		p.reads.legs = testWorkLegs(p.World.CityPath, p.World.Env.Cfg, staleReadyLeg{Store: store, stale: beads.NewMemStore()}, nil).readOnly()
		if _, err := store.Create(beads.Bead{Title: "slung", Type: "task", Status: "open", Assignee: it.Key.ID}); err != nil {
			t.Fatal(err)
		}
		s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
		got, _ := store.Get(it.Key.ID)
		if s.Outcome != settledRefused || s.Cause != causeHasWork || isDrainAckStopPendingInfo(censusRowOf(t, got).Info) {
			t.Fatalf("%s, open work assigned after the pass: %+v, want refused %q and no signal", c.reason, s, causeHasWork)
		}
	}
}

// TestSignalWorkReadErrorCountsAsWork (v5.10a B, F1 L5). Kills a signal that
// reads a failed work read as no work: it refuses has-work.
func TestSignalWorkReadErrorCountsAsWork(t *testing.T) {
	for _, c := range signalWorkCases {
		p, it, store, _ := admittedBeginOn(t, intentBegunAt(c.reason, time.Minute), c.entry)
		if it.Kind != c.kind {
			t.Fatalf("%s: decided %+v, want %s", c.reason, it, c.kind)
		}
		p.reads.legs = testWorkLegs(p.World.CityPath, p.World.Env.Cfg, failingReads{store}, nil)
		s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
		got, _ := store.Get(it.Key.ID)
		if s.Outcome != settledRefused || s.Cause != causeHasWork || isDrainAckStopPendingInfo(censusRowOf(t, got).Info) {
			t.Fatalf("%s, work read failed: %+v, want refused %q and no signal", c.reason, s, causeHasWork)
		}
	}
}

// TestSignalIdleAndAckReadNoWork (v5.10a B: idle unchanged; D2's ack row).
// Kills an L5 read on an idle signal or an ack's: a claim after the pass
// leaves both to land, and neither kind's needs read L5; nor does A4's stop
// of a signaled no-wake-reason or orphaned request.
func TestSignalIdleAndAckReadNoWork(t *testing.T) {
	sleep := func(e *selectionEntry) { e.Desired, e.Reason = desireSleep, reasonIdleSleep }
	if s, got := runSignal(t, intentBegunAt(drainIdle, time.Minute), sleep, intentSignalFresh, claimAfterPass(t)); s.Outcome != settledLanded || !isDrainAckStopPendingInfo(censusRowOf(t, got).Info) {
		t.Fatalf("idle, claimed after the pass: %+v, want the signal landed", s)
	}
	ack := append(intentBegunAt(drainOrphaned, time.Minute), session.DrainAckIncarnationKey, "3", session.DrainAckAtKey, rowAt(time.Second))
	if s, got := runSignal(t, ack, undesired(drainOrphaned), intentSignalAck, claimAfterPass(t)); s.Outcome != settledLanded || !isDrainAckStopPendingInfo(censusRowOf(t, got).Info) {
		t.Fatalf("an orphaned drain's ack, claimed after the pass: %+v, want the signal landed", s)
	}
	for _, it := range []intent{
		{Kind: intentSignalFresh, Reason: decideDrainSignal + drainIdle},
		{Kind: intentSignalFresh, Reason: decideDrainSignal + idleRespawnDrainReason},
		{Kind: intentSignalAck, Reason: decideDrainSignal + drainOrphaned},
		{Kind: intentSignalAck, Reason: decideDrainSignal + reasonNoWake},
		{Kind: intentSignal, Reason: decideDrainSignal + drainSuspended},
		{Kind: intentStop, Reason: decideDrainSignal + drainOrphaned},
		{Kind: intentStop, Reason: decideDrainSignal + reasonNoWake},
	} {
		if n := effectSpecs[it.Kind].needsOf(&World{}, it); n.Legs&legWork != 0 {
			t.Errorf("%s %s reads L5: %+v", it.Kind, it.Reason, n)
		}
	}
	for _, c := range signalWorkCases {
		it := intent{Kind: c.kind, Reason: decideDrainSignal + c.reason}
		if n := effectSpecs[it.Kind].needsOf(&World{}, it); n.Legs&legWork == 0 {
			t.Errorf("%s %s reads no L5: %+v", it.Kind, it.Reason, n)
		}
	}
}

// TestOrphanedSignalConsultsTheLiveClaimVeto (v5.10a B, DRAIN-005's veto).
// Kills an orphaned signal that skips the live-claim veto: a pool seat's
// stamped claim on a bead its assignee spelling hides from the assigned-work
// read refuses has-work; a claim the bead no longer holds, or a seat the veto
// does not cover (not pool-managed), lands.
func TestOrphanedSignalConsultsTheLiveClaimVeto(t *testing.T) {
	pool := append(intentBegunAt(drainOrphaned, time.Minute), "pool_managed", "true")
	for _, c := range []struct {
		name   string
		meta   []string
		status string
		want   string
	}{
		{"pool seat, claim held", pool, "in_progress", causeHasWork},
		{"pool seat, claim closed", pool, "closed", ""},
		{"not pool-managed, claim held", intentBegunAt(drainOrphaned, time.Minute), "in_progress", ""},
	} {
		p, it, _, _ := admittedBeginOn(t, c.meta, undesired(drainOrphaned), claimStampedAtPass(t, nil, c.status))
		if it.Kind != intentSignal {
			t.Fatalf("%s: decided %+v, want the signal", c.name, it)
		}
		s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
		if s.Cause != c.want || (c.want == "") != (s.Outcome == settledLanded) {
			t.Errorf("%s: %+v, want cause %q", c.name, s, c.want)
		}
	}
}

// claimStampedAtPass stamps a claim on the pass's row before the pass reads
// it, as `gc hook --claim` does (the work bead in progress under an assignee
// the awake read does not match, then current_claim_bead_id on the row), so
// the pass and the effect see the same stamp and only the live-claim veto
// can find the claim. The bead lives in work, or the sessions store when
// work is nil; closed closes it after the stamp.
func claimStampedAtPass(t *testing.T, work beads.Store, status string) func(*World) {
	return func(w *World) {
		t.Helper()
		store := cmp.Or[beads.Store](work, w.SessionsStore)
		b, err := store.Create(beads.Bead{Title: "w", Type: "task", Status: "in_progress", Assignee: "worker-elsewhere"})
		if err != nil {
			t.Fatal(err)
		}
		if status == "closed" {
			if err := store.Close(b.ID); err != nil {
				t.Fatal(err)
			}
		}
		for k := range w.Census.Rows {
			if err := w.SessionsStore.SetMetadata(k.ID, beadmeta.CurrentClaimBeadIDMetadataKey, b.ID); err != nil {
				t.Fatal(err)
			}
		}
		w.Census = readCensus(t, gatherNow, censusLegs(rowLeg, w.SessionsStore))
	}
}

// TestSignalRefusesAClaimStampedAfterItsWorkRead (v5.10b-2; #7771 review).
// Kills a signal that lands over a `gc hook --claim` whose stamp reaches the
// row after L5 and before the CAS's row read: the stamp moved since the pass
// refuses has-work, for both reasons.
func TestSignalRefusesAClaimStampedAfterItsWorkRead(t *testing.T) {
	for _, c := range signalWorkCases {
		p, it, store, _ := admittedBeginOn(t, intentBegunAt(c.reason, time.Minute), c.entry)
		var once sync.Once
		p.seam = func(_ context.Context, s txSeam, _ intent, _, _ int) error {
			if s == seamAfterReads {
				once.Do(func() { claimAfterPass(t)(store, it) })
			}
			return nil
		}
		s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
		got, _ := store.Get(it.Key.ID)
		if s.Outcome != settledRefused || s.Cause != causeHasWork || isDrainAckStopPendingInfo(censusRowOf(t, got).Info) {
			t.Fatalf("%s, claim stamped after L5: %+v, want refused %q and no signal", c.reason, s, causeHasWork)
		}
	}
}

// TestOrphanedSignalVetoNeverOverwritesAFailedRead (#7771 review). Kills a
// veto whose clean answer replaces a failed awake read: a pool seat with no
// claim stamped, whose work read fails, refuses has-work.
func TestOrphanedSignalVetoNeverOverwritesAFailedRead(t *testing.T) {
	pool := append(intentBegunAt(drainOrphaned, time.Minute), "pool_managed", "true")
	p, it, store, _ := admittedBeginOn(t, pool, undesired(drainOrphaned))
	if it.Kind != intentSignal {
		t.Fatalf("decided %+v, want the signal", it)
	}
	p.reads.legs = testWorkLegs(p.World.CityPath, p.World.Env.Cfg, failingReads{store}, nil)
	s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
	got, _ := store.Get(it.Key.ID)
	if s.Outcome != settledRefused || s.Cause != causeHasWork || isDrainAckStopPendingInfo(censusRowOf(t, got).Info) {
		t.Fatalf("pool seat, work read failed, no stamp: %+v, want refused %q", s, causeHasWork)
	}
}

// TestOrphanedSignalVetoHonoursSuspension (#7771 review; legacy's
// liveClaimVetoApplies). Kills a veto that ignores the pass's suspension
// state: a pool seat's veto-only claim refuses has-work, but not once the
// city is suspended (operator intent), when the signal lands.
func TestOrphanedSignalVetoHonoursSuspension(t *testing.T) {
	pool := append(intentBegunAt(drainOrphaned, time.Minute), "pool_managed", "true")
	yes := true
	for _, c := range []struct {
		name string
		st   suspensionstate.State
		want string
	}{
		{"city suspended", suspensionstate.State{City: suspensionstate.Override{Suspended: &yes}}, ""},
		{"not suspended", suspensionstate.State{}, causeHasWork},
	} {
		st := c.st
		p, it, _, _ := admittedBeginOn(t, pool, undesired(drainOrphaned), claimStampedAtPass(t, nil, "in_progress"), func(w *World) { w.Suspension = st })
		if it.Kind != intentSignal {
			t.Fatalf("%s: decided %+v, want the signal", c.name, it)
		}
		s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
		if s.Cause != c.want || (c.want == "") != (s.Outcome == settledLanded) {
			t.Errorf("%s: %+v, want cause %q", c.name, s, c.want)
		}
	}
}

// TestOrphanedSignalVetoOnASplitCity (#7771 review). Kills a veto that reads
// the row from the work store: on a city whose work store is not its
// sessions store, a pool seat with no claim lands, and one whose stamped
// claim lives in the work store refuses has-work.
func TestOrphanedSignalVetoOnASplitCity(t *testing.T) {
	pool := append(intentBegunAt(drainOrphaned, time.Minute), "pool_managed", "true")
	for _, c := range []struct {
		name  string
		stamp bool
		want  string
	}{
		{"no claim", false, ""},
		{"claim held in the work store", true, causeHasWork},
	} {
		work := beads.NewMemStoreFrom(500, nil, nil)
		split := func(w *World) { w.WorkLegs = testWorkLegs(w.CityPath, w.Env.Cfg, work, nil) }
		hooks := []func(*World){split}
		if c.stamp {
			hooks = append(hooks, claimStampedAtPass(t, work, "in_progress"))
		}
		p, it, _, _ := admittedBeginOn(t, pool, undesired(drainOrphaned), hooks...)
		if it.Kind != intentSignal {
			t.Fatalf("%s: decided %+v, want the signal", c.name, it)
		}
		s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
		if s.Cause != c.want || (c.want == "") != (s.Outcome == settledLanded) {
			t.Errorf("%s: %+v, want cause %q", c.name, s, c.want)
		}
	}
}

// liveHandleStore answers Get from its own store and its Live handle from
// live, as a caching store does.
type liveHandleStore struct {
	beads.Store
	live beads.Store
}

func (s liveHandleStore) Handles() beads.StoreHandles {
	return beads.StoreHandles{Live: beads.HandlesFor(s.live).Live}
}

// TestLiveBeadReadPeelsTheEffectWrappers (v5.10a B; the veto reads live).
// Kills a live read through a v2 effect's read-only store that falls back to
// the cache: the read reaches the wrapped store's Live handle.
func TestLiveBeadReadPeelsTheEffectWrappers(t *testing.T) {
	live := beads.NewMemStore()
	b, err := live.Create(beads.Bead{Title: "claimed", Type: "task", Status: "in_progress"})
	if err != nil {
		t.Fatal(err)
	}
	cached := liveHandleStore{Store: beads.NewMemStore(), live: live}
	for name, wrapped := range map[string]beads.Store{
		"SessionStore":            beads.SessionStore{Store: cached},
		"WorkStore":               beads.WorkStore{Store: cached},
		"readOnlyStore":           readOnlyStore{cached},
		"blindWriteRefusingStore": blindWriteRefusingStore{inner: cached},
		"all":                     readOnlyStore{blindWriteRefusingStore{inner: beads.WorkStore{Store: beads.SessionStore{Store: cached}}}},
	} {
		if got, err := liveBeadRead(wrapped, b.ID); err != nil || got.ID != b.ID {
			t.Errorf("liveBeadRead through %s = %+v, %v; want the live bead %s", name, got, err, b.ID)
		}
	}
}

// pairedReadyLeg counts the backing lists and ready reads, and holds each
// ready read until a second is in flight with it, so ready reads made one
// after another never return (the fence's own bound then expires).
type pairedReadyLeg struct {
	beads.Store
	lists, readies *atomic.Int64
	mu             *sync.Mutex
	waiting        *int
	pair, stop     chan struct{}
}

func (l pairedReadyLeg) List(q beads.ListQuery) ([]beads.Bead, error) {
	l.lists.Add(1)
	return l.Store.List(q)
}

func (l pairedReadyLeg) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	l.readies.Add(1)
	l.mu.Lock()
	if *l.waiting++; *l.waiting == 2 {
		close(l.pair)
	}
	l.mu.Unlock()
	select {
	case <-l.pair:
	case <-l.stop: // the test ended
	}
	return l.Store.Ready(q...)
}

// TestSignalL5ReadsOnceThenReadyAtOnce (mc-m4fgc; the #7771 review's
// finding 2). Kills L5's serial chain, which on a slow work store (bd over a
// remote database; staging's bd CLI work store measured 0.3-0.7s a call on
// idle lanes) outran the 5s fence bound, refusing has-work forever for a
// seat holding only non-ready work: a seat holding one blocked open bead
// lists only its one concurrent seat read (as many lists as a seat with no
// work), then makes its two ready reads (one assignee, both tiers) at once,
// and the signal lands. Ready work refuses.
func TestSignalL5ReadsOnceThenReadyAtOnce(t *testing.T) {
	run := func(t *testing.T, seed func(beads.Store, string)) (settlement, int64, int64) {
		p, it, store, _ := admittedBeginOn(t, intentBegunAt(reasonNoWake, time.Minute), signalWorkCases[0].entry)
		seed(store, it.Key.ID)
		var lists, readies atomic.Int64
		waiting := 0
		leg := pairedReadyLeg{Store: store, lists: &lists, readies: &readies, mu: &sync.Mutex{}, waiting: &waiting, pair: make(chan struct{}), stop: make(chan struct{})}
		t.Cleanup(func() { close(leg.stop) })
		p.reads.legs = testWorkLegs(p.World.CityPath, p.World.Env.Cfg, leg, nil).readOnly()
		return runTx(context.Background(), p, it, effectSpecs[it.Kind], nil), lists.Load(), readies.Load()
	}
	open := func(blocked bool) func(beads.Store, string) {
		return func(store beads.Store, seat string) {
			blocker, err := store.Create(beads.Bead{Title: "blocker", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			w, err := store.Create(beads.Bead{Title: "w", Type: "task", Assignee: seat})
			if err != nil {
				t.Fatal(err)
			}
			if blocked {
				if err := store.DepAdd(w.ID, blocker.ID, "blocks"); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	_, seatLists, _ := run(t, func(beads.Store, string) {})
	s, lists, readies := run(t, open(true))
	if s.Outcome != settledLanded || lists != seatLists || readies != 2 {
		t.Errorf("blocked work only: settlement %+v, %d lists (a seat read is %d), %d ready reads; want the signal landed, one seat read, two ready reads at once", s, lists, seatLists, readies)
	}
	if s, _, _ := run(t, open(false)); s.Cause != causeHasWork {
		t.Errorf("ready work: settlement %+v, want refused %s", s, causeHasWork)
	}
}

// faultyReadyLeg fails or panics every ready read, and fails every list of
// listFails.
type faultyReadyLeg struct {
	beads.Store
	panics    bool
	listFails string
}

func (l faultyReadyLeg) List(q beads.ListQuery) ([]beads.Bead, error) {
	if l.listFails != "" && q.Status == l.listFails {
		return nil, errors.New("list failed")
	}
	return l.Store.List(q)
}

func (l faultyReadyLeg) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	if l.panics {
		panic("ready read")
	}
	return nil, errors.New("ready read failed")
}

// TestSignalL5ReadyFaultCountsAsWork (mc-m4fgc): the seat read fails
// closed: a ready read that errs or panics over the seat's open row counts
// as work, and so does a failed in-progress or open list with no open row
// read. Kills a fault dropped by the fan-out, and a failed list taken as
// no work.
func TestSignalL5ReadyFaultCountsAsWork(t *testing.T) {
	for _, panics := range []bool{false, true} {
		p, it, store, _ := admittedBeginOn(t, intentBegunAt(reasonNoWake, time.Minute), signalWorkCases[0].entry)
		if _, err := store.Create(beads.Bead{Title: "w", Type: "task", Assignee: it.Key.ID}); err != nil {
			t.Fatal(err)
		}
		p.reads.legs = testWorkLegs(p.World.CityPath, p.World.Env.Cfg, faultyReadyLeg{Store: store, panics: panics}, nil).readOnly()
		if s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil); s.Cause != causeHasWork {
			t.Errorf("panics %t: settlement %+v, want refused %s", panics, s, causeHasWork)
		}
	}
	for _, status := range []string{"in_progress", "open"} {
		p, it, store, _ := admittedBeginOn(t, intentBegunAt(reasonNoWake, time.Minute), signalWorkCases[0].entry)
		p.reads.legs = testWorkLegs(p.World.CityPath, p.World.Env.Cfg, faultyReadyLeg{Store: store, listFails: status}, nil).readOnly()
		if s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil); s.Cause != causeHasWork {
			t.Errorf("the %s list failed: settlement %+v, want refused %s", status, s, causeHasWork)
		}
	}
}

// TestSignalL5ReadsReadyWisps (mc-m4fgc): ready work on the wisp tier holds
// the seat too, as legacy's per-tier probe reads it. Kills the fan-out
// reading the issues tier alone.
func TestSignalL5ReadsReadyWisps(t *testing.T) {
	p, it, store, _ := admittedBeginOn(t, intentBegunAt(reasonNoWake, time.Minute), signalWorkCases[0].entry)
	if _, err := store.Create(beads.Bead{Title: "w", Type: "task", Assignee: it.Key.ID, Ephemeral: true}); err != nil {
		t.Fatal(err)
	}
	if s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil); s.Cause != causeHasWork {
		t.Errorf("ready wisp: settlement %+v, want refused %s", s, causeHasWork)
	}
}

// TestSeatAwakeReadyAcrossLegsAndIdentities (mc-m4fgc; the #7815 review).
// Kills a seat read whose ready fan-out probes only its first (leg,
// assignee) pair, or de-duplicates pairs by assignee alone, either of which
// would stop a seat mid-work: ready work on another leg, under another of
// the seat's identities, or a wisp there, beside a blocked open row that
// alone holds nothing, keeps the seat, on the seat's read and the tick's.
func TestSeatAwakeReadyAcrossLegsAndIdentities(t *testing.T) {
	type place struct {
		rig      bool
		identity string // "id" or "name"
		wisp     bool
	}
	for _, c := range []struct {
		name    string
		blocked place
		ready   *place
		want    bool
	}{
		{"blocked only", place{identity: "id"}, nil, false},
		{"ready on the rig leg", place{identity: "id"}, &place{rig: true, identity: "id"}, true},
		{"ready under session_name", place{identity: "id"}, &place{identity: "name"}, true},
		{"ready wisp under session_name on the rig leg", place{identity: "id"}, &place{rig: true, identity: "name", wisp: true}, true},
		{"ready on a leg with no other row", place{rig: true, identity: "id"}, &place{identity: "name"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			cityPath := t.TempDir()
			rigPath := t.TempDir()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Rigs:      []config.Rig{{Name: "riga", Path: rigPath}},
				Agents:    []config.Agent{{Name: "auditor", Scope: "city"}},
			}
			cityStore := beads.NewMemStore()
			rigStore := beads.NewMemStore()
			rigs := map[string]beads.Store{"riga": rigStore}
			sess := beads.Bead{ID: "session-1", Type: sessionBeadType, Status: "open", Metadata: map[string]string{"template": "auditor", "session_name": "auditor-session"}}
			info := sessiontest.SeedBead(t, sess)
			assignee := func(p place) string {
				if p.identity == "name" {
					return "auditor-session"
				}
				return sess.ID
			}
			storeOf := func(p place) beads.Store {
				if p.rig {
					return rigStore
				}
				return cityStore
			}
			bs := storeOf(c.blocked)
			blocker, err := bs.Create(beads.Bead{Title: "blocker", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			w, err := bs.Create(beads.Bead{Title: "blocked", Type: "task", Assignee: assignee(c.blocked)})
			if err != nil {
				t.Fatal(err)
			}
			if err := bs.DepAdd(w.ID, blocker.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			if c.ready != nil {
				if _, err := storeOf(*c.ready).Create(beads.Bead{Title: "ready", Type: "task", Assignee: assignee(*c.ready), Ephemeral: c.ready.wisp}); err != nil {
					t.Fatal(err)
				}
			}
			legs := testWorkLegs(cityPath, cfg, cityStore, rigs)
			seat := seatWorkFor(legs, releaseScope(info, cfg))
			got, err := sessionHasAwakeAssignedWorkForReachableStore(seat, info)
			if err != nil || got != c.want {
				t.Fatalf("seat read = %v (%v), want %v", got, err, c.want)
			}
			if tick, err := sessionHasAwakeAssignedWorkForReachableStore(newSeatWork(legs), info); err != nil || tick != c.want {
				t.Fatalf("tick read = %v (%v), want %v, as the seat read", tick, err, c.want)
			}
		})
	}
}

// TestAckPredatesRuntimeForeignExemption (v5.10k-2; mc-j43o4, the #7797 r3
// review's R5 and R6). Kills the Foreign exemption widened: another row's
// runtime under the name is no replacement of this row's, so a newer
// creation time voids no ack, but only when both presence reads saw one
// object (Same; R5: an identity read across a replaced object proves
// nothing of the runtime now there), and only for a Foreign identity (R6:
// the row's own runtime of another incarnation, StaleSelf or NewerSelf, is a
// replacement, so an ack older than it is void).
func TestAckPredatesRuntimeForeignExemption(t *testing.T) {
	ackAt := gatherNow.Add(-time.Minute).Truncate(time.Second)
	after := strconv.FormatInt(ackAt.Add(time.Second).Unix(), 10)
	row := session.Info{ID: "gc-1", Generation: "3", InstanceToken: "tok-3"}
	for _, c := range []struct {
		name  string
		ident runtimeIdentity
		same  bool
		void  bool
	}{
		{"another row's runtime, one object", runtimeIdentity{Known: true, SessionID: "gc-9", Epoch: "1", Token: "tok-9"}, true, false},
		{"another row's runtime, replaced mid-read (R5)", runtimeIdentity{Known: true, SessionID: "gc-9", Epoch: "1", Token: "tok-9"}, false, true},
		{"its own older incarnation (R6)", runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: "2", Token: "tok-2"}, true, true},
		{"its own newer incarnation (R6)", runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: "4", Token: "tok-4"}, true, true},
		{"its own current runtime", runtimeIdentity{Known: true, SessionID: "gc-1", Epoch: "3", Token: "tok-3"}, true, true},
	} {
		live := runtime.Liveness{Running: true, Alive: true, ObjectID: "$1", ObjectCreated: after}
		r := &rowFacts{row: censusRow{Info: row}, fresh: &txRuntime{Class: rtAlive, Live: live, Again: live, Identity: c.ident, Same: c.same}}
		if got := r.ackPredatesRuntime(ackAt); got != c.void {
			t.Errorf("%s: ack predates the runtime %t, want %t", c.name, got, c.void)
		}
	}
}
