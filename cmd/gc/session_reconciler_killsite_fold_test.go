package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// sleepWriteFailingStore fails the durable sleep write (the SetMetadataBatch
// carrying slept_at) a bounded number of times, delegating every other op. It
// isolates "the runtime kill succeeds but the sleep metadata write fails" so the
// kill-site fold behavior can be characterized without perturbing any other
// reconciler write. This is the failing-store wrapper pattern the report's
// differential probes use (cf. failingWakeMetadataStore).
type sleepWriteFailingStore struct {
	beads.Store
	err        error
	failsLeft  int
	sleepFails int
}

func (s *sleepWriteFailingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if _, isSleep := kvs["slept_at"]; isSleep && s.failsLeft != 0 {
		if s.failsLeft > 0 {
			s.failsLeft--
		}
		s.sleepFails++
		return s.err
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// maxAgeReconcileCount runs a full reconcile tick with a max-session-age tracker
// installed and returns the planned-wake (respawn) count, so a test can observe
// same-tick respawns.
func maxAgeReconcileCount(e *reconcilerTestEnv, sessions []beads.Bead, tr maxSessionAgeTracker) int {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	return reconcileSessionBeadsTraced(
		context.Background(), "", sessions, e.desiredState, cfgNames, e.cfg, e.sp,
		e.store, nil, nil, nil, nil, e.dt, poolDesired, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr, nil,
		withMaxSessionAgeTracker(tr),
	)
}

// maxAgeReconcileSnapshot runs a full reconcile tick against a carrier snapshot so
// the caller can read the post-tick, coherently-folded Info back out. The
// reconciler writes back its post-tick infoByID onto the snapshot
// (WriteBackReconcileInfos), so snapshot.OpenInfos() after the call reflects every
// forward-pass fold — including a kill-site sleep fold whose durable write failed.
func maxAgeReconcileSnapshot(e *reconcilerTestEnv, sessions []beads.Bead, tr maxSessionAgeTracker) *sessionBeadSnapshot {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	snap := newSessionBeadSnapshotFromReconcileRows(sessionpkg.ReconcileRowsFromBeads(sessions))
	reconcileSessionBeadsTracedWithNamedDemand(
		context.Background(), "", snap.OpenForReconcile(), snap, e.desiredState, cfgNames, e.cfg, e.sp,
		beads.SessionStore{Store: e.store}, nil, nil, nil, nil, e.dt, nil, poolDesired, nil, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr, nil,
		withMaxSessionAgeTracker(tr),
	)
	return snap
}

func snapshotInfoByID(snap *sessionBeadSnapshot, id string) (sessionpkg.Info, bool) {
	for _, info := range snap.OpenInfos() {
		if info.ID == id {
			return info, true
		}
	}
	return sessionpkg.Info{}, false
}

// TestReconcileSessionBeads_MaxAgeKillSleepWriteFailureDoesNotRespawn ports the
// report's characterization (a) (council finding 2): after a successful max-age
// kill whose SleepPatch persistence fails once, the just-killed session must NOT
// respawn on the same tick. The kill-site fold is optimistic — it survives the
// failed write — so the snapshot records state=asleep and the same-tick awake scan
// leaves the session down (starts stay 1). With the pre-fix applyStore the failed
// write drops the fold, the session still looks awake, and it is respawned this
// same tick (starts 1->2).
func TestReconcileSessionBeads_MaxAgeKillSleepWriteFailureDoesNotRespawn(t *testing.T) {
	env := newReconcilerTestEnv()
	mem := beads.NewMemStore()
	failing := &sleepWriteFailingStore{Store: mem, err: context.DeadlineExceeded, failsLeft: 1}
	env.store = failing
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", MaxSessionAge: "5h"}}}
	env.addDesired("worker", "worker", true) // running: this is the initial (only) start
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"creation_complete_at": env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
	})

	tr := newMaxSessionAgeTracker()
	tr.setConfig("worker", 5*time.Hour, 0)
	env.rec = events.NewFake()

	startsBefore := providerStartCount(env, "worker")
	woken := maxAgeReconcileCount(env, []beads.Bead{session}, tr)
	startsAfter := providerStartCount(env, "worker")

	if failing.sleepFails != 1 {
		t.Fatalf("sleep write was expected to fail exactly once, got %d failures", failing.sleepFails)
	}
	if woken != 0 {
		t.Errorf("planned wakes = %d, want 0 (the just-killed session must not respawn same-tick)", woken)
	}
	if startsBefore != 1 {
		t.Fatalf("start count before reconcile = %d, want 1 (the initial start)", startsBefore)
	}
	if startsAfter != 1 {
		t.Errorf("start count after reconcile = %d, want 1 (no same-tick respawn); stderr=%q", startsAfter, env.stderr.String())
	}
	if env.sp.IsRunning("worker") {
		t.Errorf("killed session is running again after a failed sleep write — respawned same-tick; stderr=%q", env.stderr.String())
	}
}

// TestReconcileSessionBeads_MaxAgeKillFoldKeepsWakeFairnessCoherent ports the
// report's characterization (b) (council finding 2): the aged/killed session's
// optimistic fold must keep its LastWokeAt coherent even when the sleep write
// fails, because wake fairness (wakeFairnessTime -> Info.LastWokeAt) reads that
// value to order the tick's wake budget. The fold clears last_woke_at as part of
// SleepPatch; a peer session's LastWokeAt is left untouched. Because the durable
// write failed, this coherence comes ONLY from the local fold — the store row
// never received the sleep — which is exactly what the pre-fix applyStore dropped,
// leaving a stale LastWokeAt that would mis-order fairness against a peer.
func TestReconcileSessionBeads_MaxAgeKillFoldKeepsWakeFairnessCoherent(t *testing.T) {
	env := newReconcilerTestEnv()
	mem := beads.NewMemStore()
	failing := &sleepWriteFailingStore{Store: mem, err: context.DeadlineExceeded, failsLeft: 1}
	env.store = failing
	one := 1
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "aged", MaxSessionAge: "5h"},
			{Name: "peer"},
		},
		Daemon: config.DaemonConfig{MaxWakesPerTick: &one},
	}
	env.addDesired("aged", "aged", true)
	env.addDesired("peer", "peer", true)

	aged := env.createSessionBead("aged", "aged")
	env.markSessionActive(&aged)
	env.setSessionMetadata(&aged, map[string]string{
		"creation_complete_at": env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
	})
	peerWoke := env.clk.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	peer := env.createSessionBead("peer", "peer")
	env.markSessionActive(&peer)
	env.setSessionMetadata(&peer, map[string]string{"last_woke_at": peerWoke})

	tr := newMaxSessionAgeTracker()
	tr.setConfig("aged", 5*time.Hour, 0)
	env.rec = events.NewFake()

	snap := maxAgeReconcileSnapshot(env, []beads.Bead{aged, peer}, tr)

	if failing.sleepFails != 1 {
		t.Fatalf("sleep write was expected to fail exactly once, got %d failures", failing.sleepFails)
	}

	agedInfo, ok := snapshotInfoByID(snap, aged.ID)
	if !ok {
		t.Fatalf("aged session missing from post-tick snapshot")
	}
	// The optimistic fold landed despite the failed write: fairness reads a cleared
	// LastWokeAt and a coherent asleep state for the just-killed session.
	if agedInfo.LastWokeAt != "" {
		t.Errorf("aged LastWokeAt = %q, want cleared by the surviving sleep fold (fairness input)", agedInfo.LastWokeAt)
	}
	if string(agedInfo.State) != string(sessionpkg.StateAsleep) {
		t.Errorf("aged State = %q, want asleep after the max-age kill fold", agedInfo.State)
	}
	if agedInfo.SleepReason != "max-session-age" {
		t.Errorf("aged SleepReason = %q, want max-session-age", agedInfo.SleepReason)
	}
	// The peer's fairness input is untouched.
	peerInfo, ok := snapshotInfoByID(snap, peer.ID)
	if !ok {
		t.Fatalf("peer session missing from post-tick snapshot")
	}
	if peerInfo.LastWokeAt != peerWoke {
		t.Errorf("peer LastWokeAt = %q, want preserved %q", peerInfo.LastWokeAt, peerWoke)
	}
	// Coherence comes from the LOCAL fold, not persistence: the durable row never
	// received the sleep (the write failed).
	stored, err := mem.Get(aged.ID)
	if err != nil {
		t.Fatalf("Get aged bead: %v", err)
	}
	if stored.Metadata["sleep_reason"] == "max-session-age" || stored.Metadata["slept_at"] != "" {
		t.Errorf("durable row unexpectedly carries the sleep (write should have failed): %#v", stored.Metadata)
	}
}

func providerStartCount(e *reconcilerTestEnv, name string) int {
	n := 0
	for _, c := range e.sp.Calls {
		if c.Method == "Start" && c.Name == name {
			n++
		}
	}
	return n
}
