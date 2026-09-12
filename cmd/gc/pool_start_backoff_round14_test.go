package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The cases codex round 14 found (evidence 07-codex-r14.md).

// TestHealLeavesAnUncommittedStartToRecovery: the heal never moves a live
// start-pending/creating session on — the state is the durable fact that
// its start is uncommitted, and only the commit that clears the work
// bead's record first confirms it. A live asleep session is still healed
// awake; a dead creating session still rolls back as before.
func TestHealLeavesAnUncommittedStartToRecovery(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC)}
	for _, state := range []session.State{session.StateCreating, session.StateStartPending} {
		for _, claim := range []bool{true, false} {
			info := session.Info{ID: "s-1", MetadataState: string(state), PendingCreateClaim: claim, CreatedAt: clk.Now().Add(-time.Hour)}
			if batch := healStatePatchWithRollbackInfo(info, true, true, clk, time.Minute, true); batch != nil {
				t.Fatalf("live %s (claim=%v): the heal must leave an uncommitted start alone, got %v", state, claim, batch)
			}
		}
	}
	asleep := session.Info{ID: "s-2", MetadataState: string(session.StateAsleep), CreatedAt: clk.Now().Add(-time.Hour)}
	if batch := healStatePatchWithRollbackInfo(asleep, true, true, clk, time.Minute, true); batch["state"] != string(session.StateAwake) {
		t.Fatalf("control: a live asleep session is healed awake, got %v", batch)
	}
	dead := session.Info{ID: "s-3", MetadataState: string(session.StateCreating), LastWokeAt: clk.Now().Add(-time.Hour).Format(time.RFC3339), CreatedAt: clk.Now().Add(-2 * time.Hour)}
	if batch := healStatePatchWithRollbackInfo(dead, false, true, clk, time.Minute, true); batch["state"] != string(session.StateAsleep) {
		t.Fatalf("control: a dead, stale creating session still rolls back, got %v", batch)
	}
}

// listCountingStore counts the scans the sweep makes.
type listCountingStore struct {
	beads.Store
	lists int
}

func (s *listCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	return s.Store.List(q)
}

// TestSweepUnmailedParksFindsParksOutsideDemand: a parked bead whose mail
// has not landed owes its mail whether or not it is demand — its agent
// suspended, a dependency added, a claim elsewhere — so the sweep lists
// the stores themselves (work store and rig stores), at most once per
// retry interval, and hands every unmailed park to the retry. A stamped
// park and an unparked bead are not mailed; a listing failure is said and
// the sweep continues.
func TestSweepUnmailedParksFindsParksOutsideDemand(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
	now := time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC)
	stamp := now.Add(-time.Hour).Format(time.RFC3339)
	mk := func(store beads.Store, title string, extra map[string]string) beads.Bead {
		meta := map[string]string{beadmeta.RoutedToMetadataKey: "worker"}
		for k, v := range extra {
			meta[k] = v
		}
		b, err := store.Create(beads.Bead{Title: title, Type: "task", Metadata: meta})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	park := map[string]string{beadmeta.ParkedAtMetadataKey: stamp, beadmeta.ParkReasonMetadataKey: "boom", beadmeta.ParkFailuresMetadataKey: "5", beadmeta.ParkIDMetadataKey: "cafe"}
	work := &listCountingStore{Store: beads.NewMemStore()}
	rig := &listCountingStore{Store: beads.NewMemStore()}
	// Not demand: assigned elsewhere, blocked, whatever — the sweep does not care.
	inCity := mk(work, "parked, unmailed, not demand", park)
	if in := "in_progress"; true {
		if err := work.Update(inCity.ID, beads.UpdateOpts{Status: &in}); err != nil {
			t.Fatal(err)
		}
	}
	inRig := mk(rig, "parked, unmailed, in a rig store", park)
	mailed := mk(work, "parked, mailed", map[string]string{beadmeta.ParkedAtMetadataKey: stamp, beadmeta.ParkMailedAtMetadataKey: stamp, beadmeta.ParkIDMetadataKey: "feed"})
	plain := mk(work, "not parked", nil)
	var mails []string
	policy := &workStartFailurePolicy{
		workStore:         work,
		rigStores:         map[string]beads.Store{"a": rig},
		templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
		canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
		limitFor:          func(string) int { return 5 },
		notify:            func(n parkedWorkNotice) error { mails = append(mails, n.BeadID); return nil },
		retry:             &parkMailRetryState{},
	}
	policy.sweepUnmailedParks(now)
	policy.awaitParkMailRetries()
	if len(mails) != 2 || !round14Contains(mails, inCity.ID) || !round14Contains(mails, inRig.ID) {
		t.Fatalf("the sweep mails every unmailed park it finds, and nothing else: %v (want %s and %s)", mails, inCity.ID, inRig.ID)
	}
	for _, id := range []string{inCity.ID} {
		row, _ := work.Get(id)
		if readWorkStartFailureState(row.Metadata).ParkMailedAt.IsZero() {
			t.Fatalf("a landed mail is stamped: %v", row.Metadata)
		}
	}
	if row, _ := rig.Get(inRig.ID); readWorkStartFailureState(row.Metadata).ParkMailedAt.IsZero() {
		t.Fatalf("the rig-store park is stamped too: %v", row.Metadata)
	}
	for _, id := range []string{mailed.ID, plain.ID} {
		if round14Contains(mails, id) {
			t.Fatalf("%s must not be mailed", id)
		}
	}
	// Inside the interval: no listing at all.
	lists := work.lists + rig.lists
	policy.sweepUnmailedParks(now.Add(parkMailRetryEvery - time.Second))
	policy.awaitParkMailRetries()
	if work.lists+rig.lists != lists {
		t.Fatal("the stores are listed at most once per retry interval")
	}
	// At the interval: listed again, nothing owed, no mail.
	policy.sweepUnmailedParks(now.Add(parkMailRetryEvery))
	policy.awaitParkMailRetries()
	if work.lists+rig.lists == lists || len(mails) != 2 {
		t.Fatalf("a due sweep lists again (%d→%d) and re-sends nothing that landed (%v)", lists, work.lists+rig.lists, mails)
	}
}

func round14Contains(ids []string, id string) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

// TestSweepUnmailedParksSaysAListingFailure: a store that fails to list is
// logged and skipped; the other stores are still swept on the same call.
func TestSweepUnmailedParksSaysAListingFailure(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
	now := time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC)
	rig := beads.NewMemStore()
	parked, err := rig.Create(beads.Bead{Title: "parked in rig", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker", beadmeta.ParkedAtMetadataKey: now.Format(time.RFC3339), beadmeta.ParkIDMetadataKey: "cafe"}})
	if err != nil {
		t.Fatal(err)
	}
	var mails []string
	var stderr bytes.Buffer
	policy := &workStartFailurePolicy{
		workStore:         round14ListErrStore{Store: beads.NewMemStore(), err: errors.New("work store: list timed out")},
		rigStores:         map[string]beads.Store{"a": rig},
		templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
		canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
		limitFor:          func(string) int { return 5 },
		notify:            func(n parkedWorkNotice) error { mails = append(mails, n.BeadID); return nil },
		stderr:            &stderr,
	}
	policy.sweepUnmailedParks(now)
	policy.awaitParkMailRetries()
	if len(mails) != 1 || mails[0] != parked.ID {
		t.Fatalf("the rig store is still swept: %v", mails)
	}
	if !strings.Contains(stderr.String(), "park-mail sweep") || !strings.Contains(stderr.String(), "list timed out") {
		t.Fatalf("the listing failure is said:\n%s", stderr.String())
	}
}

type round14ListErrStore struct {
	beads.Store
	err error
}

func (s round14ListErrStore) List(beads.ListQuery) ([]beads.Bead, error) { return nil, s.err }

// TestUndesiredLiveUncommittedStartClearsItsRecordThenDrains: a session
// whose start ran (runtime alive) but never committed, and that nothing
// wants any more (undesired, no claim, no in-flight lease): the heal leaves
// its start-pending state alone, so the orphan branch takes it — the
// failed-start record of the bead it ran for is cleared first (a runtime
// that came up is a lane that starts), and then it drains like any live
// orphan. A clear that fails keeps it open one more tick.
func TestUndesiredLiveUncommittedStartClearsItsRecordThenDrains(t *testing.T) {
	h := newStartBackoffHarness(t, 5)
	if starts := h.advance(15*time.Second, errPreStartFailure); starts != 2 {
		t.Fatalf("starts = %d, want 2\nstderr:\n%s", starts, h.env.stderr.String())
	}
	const name = "sky-orphan"
	orphan, err := h.env.store.Create(beads.Bead{
		Title:  backoffHarnessTemplate,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:" + backoffHarnessTemplate},
		Metadata: map[string]string{
			"session_name":                          name,
			"session_name_explicit":                 "true",
			"template":                              backoffHarnessTemplate,
			"state":                                 string(session.StateStartPending),
			"generation":                            "2",
			"continuation_epoch":                    "1",
			"instance_token":                        "orphan-token",
			beadmeta.TriggerBeadIDMetadataKey:       h.work.ID,
			beadmeta.TriggerBeadStoreRefMetadataKey: "city",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.env.sp.Start(context.Background(), name, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"GC_SESSION_ID": orphan.ID, "GC_INSTANCE_TOKEN": "orphan-token"} {
		if err := h.env.sp.SetMeta(name, key, value); err != nil {
			t.Fatal(err)
		}
	}
	h.env.desiredState = map[string]TemplateParams{} // undesired
	realWriter, _ := beads.ConditionalWriterFor(h.env.store)
	h.policy.resolveWriter = func(beads.Store) (beads.ConditionalWriter, error) {
		return round9FailingWriter{ConditionalWriter: realWriter, err: errors.New("work store: write timed out")}, nil
	}
	h.env.reconcileWithPoolDesired([]beads.Bead{orphan}, map[string]int{backoffHarnessTemplate: 0})
	if got := readWorkStartFailureState(h.reload().Metadata).Failures; got != 2 {
		t.Fatalf("a refused clear leaves the record: %d", got)
	}
	if h.env.dt.get(orphan.ID) != nil {
		t.Fatalf("a clear that fails keeps the orphan open one more tick (no drain)\nstderr:\n%s", h.env.stderr.String())
	}
	if !strings.Contains(h.env.stderr.String(), "before draining the uncommitted orphan") {
		t.Fatalf("the held drain is said:\n%s", h.env.stderr.String())
	}
	h.installPolicy()
	h.env.desiredState = map[string]TemplateParams{}
	row, _ := h.env.store.Get(orphan.ID)
	h.env.reconcileWithPoolDesired([]beads.Bead{row}, map[string]int{backoffHarnessTemplate: 0})
	if got := h.reload(); len(workStartFailureClearPatch(got.Metadata)) != 0 {
		t.Fatalf("the orphan's work bead record is cleared before it drains: %v\nstderr:\n%s", got.Metadata, h.env.stderr.String())
	}
	if h.env.dt.get(orphan.ID) == nil {
		t.Fatalf("the live uncommitted orphan drains once its clear landed\nstderr:\n%s", h.env.stderr.String())
	}
}
