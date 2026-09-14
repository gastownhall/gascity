package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Round 4 (the mayor-cut on gp-r8sk): family A is the operator-verb write
// path, family B the session-survivor consumers. Each test names the finding
// it pins.

// TestPoolStartBackoff_ConditionalWriteResolvesThroughThePolicyWrapper is A1
// (codex r3 finding 1): the production store is the policy wrapper, which
// embeds beads.Store (no UpdateIfMatch promotion) and declares a resolve
// target; the charge must reach the backend's conditional write through it,
// so an operator's --reassign reset between the read and the write survives
// and the charge is recomputed from the fresh row.
func TestPoolStartBackoff_ConditionalWriteResolvesThroughThePolicyWrapper(t *testing.T) {
	h := newPoolStartBackoffHarness(t, intPtr(5))
	wrapped := &beadPolicyStore{Store: h.store, cfg: h.cfg}
	if _, ok := beads.ConditionalWriterFor(wrapped); ok {
		t.Fatal("fixture: the policy wrapper must not promote UpdateIfMatch (that is the production shape the finding is about)")
	}
	if _, ok := workBeadConditionalWriter(wrapped); !ok {
		t.Fatal("fixture: the resolve-target walk must reach the MemStore's conditional writer")
	}
	h.policy.workStore = wrapped
	if err := h.store.Update(h.work.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.StartFailuresMetadataKey: "4"}}); err != nil {
		t.Fatal(err)
	}
	item := preparedStart{candidate: startCandidate{info: sessionpkg.Info{TriggerBeadID: h.work.ID}, tp: TemplateParams{TemplateName: "helper"}}}
	item.attachWorkStartPolicy(h.policy)
	resets := 0
	h.policy.testBeforeWrite = func() {
		if resets == 0 {
			resets++
			patch := map[string]string{}
			for _, key := range []string{beadmeta.StartFailuresMetadataKey, beadmeta.StartFailedAtMetadataKey, beadmeta.StartFailureMetadataKey, beadmeta.StartBackoffUntilMetadataKey} {
				patch[key] = ""
			}
			if err := h.store.Update(h.work.ID, beads.UpdateOpts{Metadata: patch}); err != nil {
				t.Fatal(err)
			}
		}
	}
	var stderr bytes.Buffer
	recordWorkStartFailure(startResult{prepared: item, err: errPreStart, outcome: TraceOutcomeProviderError}, h.clk, &stderr, nil)
	if got := h.meta(beadmeta.ParkedAtMetadataKey); got != "" {
		t.Fatalf("through the policy wrapper a stale charge parked the bead over the operator's reset (the write ran unconditionally)\nstderr:\n%s", stderr.String())
	}
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "1" {
		t.Fatalf("gc.start_failures = %q after reset-then-failure, want 1", got)
	}
	if strings.Contains(stderr.String(), "UNCONDITIONALLY") {
		t.Fatalf("the wrapped store was written unconditionally:\n%s", stderr.String())
	}
	if got := len(h.parkMails()); got != 0 {
		t.Fatalf("park mails = %d, want 0", got)
	}
	// Two changes under one charge through the wrapper: dropped, never blind.
	h.policy.testBeforeWrite = func() {
		if err := h.store.Update(h.work.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.StartFailureMetadataKey: "touched"}}); err != nil {
			t.Fatal(err)
		}
	}
	stderr.Reset()
	recordWorkStartFailure(startResult{prepared: item, err: errPreStart, outcome: TraceOutcomeProviderError}, h.clk, &stderr, nil)
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "1" {
		t.Fatalf("gc.start_failures = %q after two conflicts, want the charge dropped at 1", got)
	}
	if !strings.Contains(stderr.String(), "changed twice") {
		t.Fatalf("the dropped charge was silent:\n%s", stderr.String())
	}
}

// erroringBeadStore is a store whose every read fails (a rig store behind a
// dropped connection).
type erroringBeadStore struct {
	beads.Store
	err error
}

func (s erroringBeadStore) Get(string) (beads.Bead, error) { return beads.Bead{}, s.err }

// TestPoolStartBackoff_PartialReadWithNoStoreRefIsNotCharged is A2 (codex r3
// finding 2): with no store ref on the start request, one hit plus one store
// that could not be read never established uniqueness, so nothing is charged
// and nothing is reset; the partial read is logged once.
func TestPoolStartBackoff_PartialReadWithNoStoreRefIsNotCharged(t *testing.T) {
	city := beads.NewMemStore()
	work, err := city.Create(beads.Bead{Title: "city", Type: "task", Metadata: map[string]string{beadmeta.StartFailuresMetadataKey: "2"}})
	if err != nil {
		t.Fatal(err)
	}
	rig := erroringBeadStore{Store: beads.NewMemStore(), err: errors.New("dolt: connection reset by peer")}
	policy := newWorkStartFailurePolicy(&config.City{Agents: []config.Agent{{Name: "helper"}}}, city, map[string]beads.Store{"riga": rig}, mail.NewFake(), "mayor")
	item := preparedStart{candidate: startCandidate{info: sessionpkg.Info{TriggerBeadID: work.ID}, tp: TemplateParams{TemplateName: "helper"}}}
	item.attachWorkStartPolicy(policy)
	var stderr bytes.Buffer
	clk := &clock.Fake{Time: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	for i := 0; i < 2; i++ {
		recordWorkStartFailure(startResult{prepared: item, err: errPreStart, outcome: TraceOutcomeProviderError}, clk, &stderr, nil)
	}
	recordWorkStartSuccessFor(policy, work.ID, "", &stderr)
	got, err := city.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata[beadmeta.StartFailuresMetadataKey] != "2" || got.Metadata[beadmeta.ParkedAtMetadataKey] != "" {
		t.Fatalf("the one hit of a partial read was charged or reset: %v", got.Metadata)
	}
	if n := strings.Count(stderr.String(), "partial read"); n != 1 {
		t.Fatalf("partial-read lines = %d, want exactly 1:\n%s", n, stderr.String())
	}
	// With the store ref on the request the read is direct and the charge lands.
	direct := preparedStart{candidate: startCandidate{info: sessionpkg.Info{TriggerBeadID: work.ID, TriggerBeadStoreRef: "city"}, tp: TemplateParams{TemplateName: "helper"}}}
	direct.attachWorkStartPolicy(policy)
	recordWorkStartFailure(startResult{prepared: direct, err: errPreStart, outcome: TraceOutcomeProviderError}, clk, &stderr, nil)
	if got, _ := city.Get(work.ID); got.Metadata[beadmeta.StartFailuresMetadataKey] != "3" {
		t.Fatalf("control: a direct read with the store ref did not charge: %v", got.Metadata)
	}
}

// TestPoolStartBackoff_ResetRetriesOnceOnAConflict is A5 (codex r3 finding
// 6): the confirmed-success reset re-reads once on a revision conflict (an
// unrelated edit between its read and its write) and clears the count; a
// second conflict logs and leaves the row.
func TestPoolStartBackoff_ResetRetriesOnceOnAConflict(t *testing.T) {
	h := newPoolStartBackoffHarness(t, intPtr(5))
	charged := map[string]string{
		beadmeta.StartFailuresMetadataKey:     "4",
		beadmeta.StartFailedAtMetadataKey:     "2026-09-14T12:00:00Z",
		beadmeta.StartFailureMetadataKey:      "fatal: two branches match",
		beadmeta.StartBackoffUntilMetadataKey: "2026-09-14T12:01:20Z",
	}
	if err := h.store.Update(h.work.ID, beads.UpdateOpts{Metadata: charged}); err != nil {
		t.Fatal(err)
	}
	edits := 0
	h.policy.testBeforeWrite = func() {
		if edits == 0 {
			edits++
			title := "retitled under the reset"
			if err := h.store.Update(h.work.ID, beads.UpdateOpts{Title: &title}); err != nil {
				t.Fatal(err)
			}
		}
	}
	var stderr bytes.Buffer
	recordWorkStartSuccessFor(h.policy, h.work.ID, "", &stderr)
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "" {
		t.Fatalf("gc.start_failures = %q after a confirmed start with one conflict, want cleared (the reset gave up on its first conflict)\nstderr:\n%s", got, stderr.String())
	}
	if h.workBead().Title != "retitled under the reset" {
		t.Fatal("the reset overwrote the concurrent edit")
	}
	// Every write conflicts: the reset logs and leaves the row.
	if err := h.store.Update(h.work.ID, beads.UpdateOpts{Metadata: charged}); err != nil {
		t.Fatal(err)
	}
	h.policy.testBeforeWrite = func() {
		if err := h.store.Update(h.work.ID, beads.UpdateOpts{Metadata: map[string]string{"gc.touch": time.Now().String()}}); err != nil {
			t.Fatal(err)
		}
	}
	stderr.Reset()
	recordWorkStartSuccessFor(h.policy, h.work.ID, "", &stderr)
	if got := h.meta(beadmeta.StartFailuresMetadataKey); got != "4" {
		t.Fatalf("gc.start_failures = %q after two conflicts, want 4 (left for the next confirmed start)", got)
	}
	if !strings.Contains(stderr.String(), "changed twice") {
		t.Fatalf("the skipped reset was silent:\n%s", stderr.String())
	}
}

// TestComputeAwakeSet_DeferredTriggerSessionIsNotWoken is B2 (codex r3
// finding 5) at the awake engine: a session whose trigger the start gate holds
// back is not scaled demand, not a work-query wake and not min-active capacity,
// while its sibling for eligible work is.
func TestComputeAwakeSet_DeferredTriggerSessionIsNotWoken(t *testing.T) {
	older := time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC)
	sessions := []AwakeSessionBead{
		{ID: "S", SessionName: "helper-1", Template: "helper", State: "creating", TriggerBeadID: "W", CreatedAt: older},
		{ID: "T", SessionName: "helper-2", Template: "helper", State: "creating", TriggerBeadID: "V", CreatedAt: older.Add(time.Minute)},
	}
	base := AwakeInput{
		Agents:       []AwakeAgent{{QualifiedName: "helper"}},
		SessionBeads: sessions,
		Now:          older.Add(time.Hour),
	}
	scaled := base
	scaled.ScaleCheckCounts = map[string]int{"helper": 1}
	if got := ComputeAwakeSet(scaled); !got["helper-1"].ShouldWake {
		t.Fatalf("control: without a deferred set the older creating session is scaled demand: %+v", got)
	}
	scaled.DeferredTriggers = map[string]struct{}{"W": {}}
	got := ComputeAwakeSet(scaled)
	if got["helper-1"].ShouldWake {
		t.Fatalf("the session for the deferred trigger was woken as scaled demand: %+v", got["helper-1"])
	}
	if !got["helper-2"].ShouldWake {
		t.Fatalf("the sibling for eligible work was not woken: %+v", got["helper-2"])
	}
	workQuery := base
	workQuery.WorkSet = map[string]bool{"helper": true}
	workQuery.DeferredTriggers = map[string]struct{}{"W": {}}
	got = ComputeAwakeSet(workQuery)
	if got["helper-1"].ShouldWake || !got["helper-2"].ShouldWake {
		t.Fatalf("work-query wake picked the deferred session over its sibling: S=%+v T=%+v", got["helper-1"], got["helper-2"])
	}
	minActive := AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "helper", MinActiveSessions: 1}},
		SessionBeads: []AwakeSessionBead{
			{ID: "S", SessionName: "helper-1", Template: "helper", State: "asleep", SleepReason: string(sessionpkg.SleepReasonCityStop), TriggerBeadID: "W", CreatedAt: older},
		},
		DeferredTriggers: map[string]struct{}{"W": {}},
		Now:              older.Add(time.Hour),
	}
	if got := ComputeAwakeSet(minActive); got["helper-1"].ShouldWake {
		t.Fatalf("min-active revived the session for a deferred trigger: %+v", got["helper-1"])
	}
}

// TestPoolStartBackoff_SurvivingSessionForParkedBeadIsNeitherReusedNorWoken
// is B4, the one end-to-end test through buildDesiredStateWithSessionBeads
// (materialization) and the reconciler's awake pass (codex r3 findings 4 and
// 5, which the request-level tests stopped short of): a surviving creating
// session S whose trigger W is parked, a pending session T carrying eligible
// V, max_start_failures 1, one start per tick. Across three ticks T starts
// once in V's own directory, S is neither reused for V nor woken, and W stays
// assigned to S.
func TestPoolStartBackoff_SurvivingSessionForParkedBeadIsNeitherReusedNorWoken(t *testing.T) {
	cityPath := t.TempDir()
	wDir := filepath.Join(cityPath, "w-dir")
	vDir := filepath.Join(cityPath, "v-dir")
	for _, dir := range []string{wDir, vDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{
		Agents:    []config.Agent{{Name: "helper", MaxActiveSessions: intPtr(2), MaxStartFailures: intPtr(1), Provider: "mock"}},
		Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
		Daemon:    config.DaemonConfig{MaxWakesPerTick: intPtr(1)},
		Session:   config.SessionConfig{ParkAlertTo: "mayor"},
	}
	store := beads.NewMemStore()
	stamp := "2026-09-14T12:00:00Z"
	work, err := store.Create(beads.Bead{Title: "W parked", Type: "task", Status: "in_progress", Assignee: "helper-1", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey:     "helper",
		beadmeta.ParkedAtMetadataKey:     stamp,
		beadmeta.ParkReasonMetadataKey:   "fatal: two branches match",
		beadmeta.ParkFailuresMetadataKey: "1",
		beadmeta.WorkDirMetadataKey:      wDir,
	}})
	if err != nil {
		t.Fatal(err)
	}
	eligible, err := store.Create(beads.Bead{Title: "V eligible", Type: "task", Status: "open", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: "helper",
		beadmeta.WorkDirMetadataKey:  vDir,
	}})
	if err != nil {
		t.Fatal(err)
	}
	sessionBead := func(name, slot, trigger string, pending bool) beads.Bead {
		meta := map[string]string{
			"template":                        "helper",
			"session_name":                    name,
			"session_name_explicit":           "true",
			"pool_slot":                       slot,
			"pool_managed":                    "true",
			"state":                           "creating",
			"generation":                      "1",
			"continuation_epoch":              "1",
			"instance_token":                  "token-" + name,
			beadmeta.TriggerBeadIDMetadataKey: trigger,
		}
		if pending {
			meta["pending_create_claim"] = "true"
		}
		b, err := store.Create(beads.Bead{Title: "helper", Type: sessionBeadType, Labels: []string{sessionBeadLabel, "template:helper"}, Metadata: meta})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	survivor := sessionBead("helper-1", "1", work.ID, false)
	pending := sessionBead("helper-2", "2", eligible.ID, true)

	sp := runtime.NewFake()
	clk := &clock.Fake{Time: time.Date(2026, 9, 14, 12, 5, 0, 0, time.UTC)}
	mailer := mail.NewFake()
	policy := newWorkStartFailurePolicy(cfg, store, nil, mailer, "mayor")
	var stdout, stderr bytes.Buffer
	for tick := 0; tick < 3; tick++ {
		snap, err := loadSessionBeadSnapshot(store)
		if err != nil {
			t.Fatal(err)
		}
		ds := buildDesiredStateWithSessionBeads("gc", cityPath, clk.Now(), cfg, sp, store, nil, snap, nil, &stderr)
		openInfos := snap.OpenInfos()
		pass := newWorkStartDeferralPass(clk.Now(), nil)
		_, poolWorkBeads := poolDemandAssignedWork(cfg, cityPath, store, openInfos, ds.AssignedWorkBeads, ds.AssignedWorkStoreRefs, pass)
		poolDesired := PoolDesiredCounts(ComputePoolDesiredStatesDeferring(cfg, poolWorkBeads, openInfos, ds.ScaleCheckCounts, nil, pass.deferred, clk.Now(), nil))
		if _, deferred := pass.deferred[work.ID]; !deferred {
			t.Fatalf("tick %d: the parked bead was not deferred by the gate (assigned rows %d)", tick, len(ds.AssignedWorkBeads))
		}
		cfgNames := configuredSessionNamesWithSnapshot(cfg, "gc", snap)
		reconcileSessionBeadsAtPathWithNamedDemand(
			context.Background(), cityPath, snap.OpenForReconcile(), snap, ds.State, cfgNames, cfg, sp, store,
			nil, ds.AssignedWorkBeads, nil, nil, newDrainTracker(), nil, poolDesired,
			ds.NamedSessionDemand, ds.NamedSessionRoutedDemand, false, nil, "gc",
			nil, clk, events.Discard, 0, 0, &stdout, &stderr,
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
			withReadyAssignedFlags(readyAssignedFlagsForBeads(ds.ReadyAssigned, ds.AssignedWorkBeads, ds.AssignedWorkStoreRefs)),
			withWorkStartFailurePolicy(policy),
		)
		clk.Time = clk.Time.Add(time.Minute)
	}
	var starts []runtime.Call
	for _, call := range sp.SnapshotCalls() {
		if call.Method == "Start" {
			starts = append(starts, call)
		}
	}
	if len(starts) != 1 || starts[0].Name != "helper-2" {
		names := make([]string, 0, len(starts))
		for _, c := range starts {
			names = append(names, c.Name)
		}
		t.Fatalf("starts across three ticks = %v, want exactly one, helper-2 (the survivor for the parked bead was started or reused)\nstderr:\n%s", names, stderr.String())
	}
	// V is open and unclaimed, so its launch context is the agent's own
	// directory (the city path here) until a session claims it; the invariant
	// is that W's directory, the one whose pre_start keeps failing, never
	// leaks into T's start through a rebound S.
	if got := starts[0].Config.WorkDir; got == wDir || got != cityPath {
		t.Fatalf("helper-2 started in %q, want the agent's own directory %q, never W's %q", got, cityPath, wDir)
	}
	gotWork, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotWork.Assignee != "helper-1" || gotWork.Metadata[beadmeta.ParkedAtMetadataKey] != stamp {
		t.Fatalf("W after three ticks: assignee=%q parked_at=%q, want helper-1 / %s (the park is not cleared and W is not reassigned)", gotWork.Assignee, gotWork.Metadata[beadmeta.ParkedAtMetadataKey], stamp)
	}
	gotSurvivor, err := store.Get(survivor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := gotSurvivor.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != work.ID {
		t.Fatalf("S's trigger = %q, want W %s (S was rebound to other work)", got, work.ID)
	}
	if got, _ := store.Get(pending.ID); got.Metadata[beadmeta.TriggerBeadIDMetadataKey] != eligible.ID {
		t.Fatalf("T's trigger = %q, want V %s", got.Metadata[beadmeta.TriggerBeadIDMetadataKey], eligible.ID)
	}
	if msgs, _ := mailer.Inbox("mayor"); len(msgs) != 0 {
		t.Fatalf("park mails = %d, want 0 (nothing new was parked)", len(msgs))
	}
}
