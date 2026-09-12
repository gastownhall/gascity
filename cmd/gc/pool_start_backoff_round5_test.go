package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Codex round-5 pins (evidence 07-codex-r5.md).

// TestPoolStartBackoffUnsupportedConditionalWriteTakesTheReReadPath: a store
// that offers a ConditionalWriter but cannot serve it (DoltliteReadStore
// answers ErrConditionalWriteUnsupported) is an unfenced store and takes the
// SAME plain path as one with no writer: a re-read right before the write,
// refused when the row moved — never an unconditional write of the stale
// computation.
func TestPoolStartBackoffUnsupportedConditionalWriteTakesTheReReadPath(t *testing.T) {
	h := newStartBackoffHarness(t, 5)
	h.advance(15*time.Second, errPreStartFailure) // failures at 0s and 10s → count 2
	if got := readWorkStartFailureState(h.reload().Metadata).Failures; got != 2 {
		t.Fatalf("count = %d", got)
	}
	racing := &unsupportedCASStore{resetOnFirstGetStore: resetOnFirstGetStore{Store: h.env.store, id: h.work.ID}}
	h.policy.workStore = racing
	h.policy.resolveWriter = probeConditionalWriter
	h.policy.recordStartFailure(workTrigger{BeadID: h.work.ID, StoreRef: "city"}, backoffHarnessTemplate, errPreStartFailure, h.env.clk.Now().Add(time.Minute))
	state := readWorkStartFailureState(h.reload().Metadata)
	if state.Failures != 1 {
		t.Fatalf("the failure must be charged against the RESET row (count 1), not written unconditionally from the stale snapshot (count 3): %+v\nstderr:\n%s", state, h.env.stderr.String())
	}
	if racing.updateIfMatch != 1 {
		t.Fatalf("the writer is asked once and then treated as unfenced, got %d UpdateIfMatch calls", racing.updateIfMatch)
	}
	if racing.gets < 4 {
		t.Fatalf("the plain path must re-read before the write and retry from a fresh row after the mismatch: want >= 4 gets, got %d", racing.gets)
	}
}

// unsupportedCASStore is resetOnFirstGetStore whose conditional writer
// answers ErrConditionalWriteUnsupported — the DoltliteReadStore shape.
type unsupportedCASStore struct {
	resetOnFirstGetStore
	updateIfMatch int
}

func (s *unsupportedCASStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	inner, ok := beads.ConditionalWriterFor(s.Store)
	if !ok {
		return nil, false
	}
	return unsupportedConditionalWriter{ConditionalWriter: inner, calls: &s.updateIfMatch}, true
}

type unsupportedConditionalWriter struct {
	beads.ConditionalWriter
	calls *int
}

func (w unsupportedConditionalWriter) UpdateIfMatch(string, int64, beads.UpdateOpts) error {
	*w.calls++
	return beads.ErrConditionalWriteUnsupported
}

// TestPoolStartBackoffLegacyBoundRouteIsNotAReroute: a bead still carrying
// the legacy bound identity of an agent that went bound→unbound
// ("rig/old.worker" for "rig/worker") is routed to that agent — pool demand
// normalizes the route the same way — so its failed starts are charged, not
// skipped as "now routed elsewhere". A genuine re-route still is skipped.
func TestPoolStartBackoffLegacyBoundRouteIsNotAReroute(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", Dir: "rig", MaxActiveSessions: intPtr(1)}},
	}
	work, err := store.Create(beads.Bead{Title: "migrated route", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/old.worker"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := poolTemplateForWorkBead(cfg, work); got != "rig/worker" {
		t.Fatalf("poolTemplateForWorkBead(rig/old.worker) = %q, want the agent's canonical name rig/worker", got)
	}
	var stderr bytes.Buffer
	cr := &CityRuntime{cityPath: t.TempDir(), cityName: "test-city", cfg: cfg, sp: runtime.NewFake(), rec: events.Discard, stderr: &stderr, storageRoutes: messagingSplitRoutes(store)}
	policy := cr.workStartFailurePolicy(store, store, nil)
	reload := func() workStartFailureState {
		b, err := store.Get(work.ID)
		if err != nil {
			t.Fatal(err)
		}
		return readWorkStartFailureState(b.Metadata)
	}
	policy.recordStartFailure(workTrigger{BeadID: work.ID, StoreRef: "city"}, "rig/worker", errPreStartFailure, time.Now().UTC())
	if got := reload().Failures; got != 1 {
		t.Fatalf("a failed start on the legacy bound route must be charged: count=%d\nstderr:\n%s", got, stderr.String())
	}
	if err := store.SetMetadataBatch(work.ID, map[string]string{beadmeta.RoutedToMetadataKey: "rig/other"}); err != nil {
		t.Fatal(err)
	}
	policy.recordStartFailure(workTrigger{BeadID: work.ID, StoreRef: "city"}, "rig/worker", errPreStartFailure, time.Now().UTC())
	if got := reload().Failures; got != 1 {
		t.Fatalf("a bead re-routed to another template must not be charged: count=%d", got)
	}
	if !strings.Contains(stderr.String(), "now routed to rig/other") {
		t.Fatalf("the skip must be said:\n%s", stderr.String())
	}
}

// TestCityRuntimeDemandSnapshotRefreshesWhenABackoffExpires: a backed-off
// routed bead re-enters demand when its deadline passes with no write
// anywhere — neither the session nor the ready-bead fingerprint moves — so
// the cached snapshot must expire at that deadline, not at the backstop age
// (a 10s backoff must not last five minutes).
func TestCityRuntimeDemandSnapshotRefreshesWhenABackoffExpires(t *testing.T) {
	buildCalls := 0
	now := time.Date(2026, 9, 12, 1, 0, 0, 0, time.UTC)
	deadline := now.Add(10 * time.Second)
	cr := &CityRuntime{
		cityName:  "test-city",
		cityPath:  t.TempDir(),
		cfg:       &config.City{Workspace: config.Workspace{Name: "test-city"}},
		cs:        &controllerState{eventProv: events.NewFake()},
		stderr:    io.Discard,
		demandNow: func() time.Time { return now },
	}
	cr.buildFnWithSessionBeads = func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
		buildCalls++
		return DesiredStateResult{StartDeferredUntil: deadline}
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{})
	_ = cr.loadDemandSnapshot(sessionBeads, nil, "patrol", false)
	now = deadline.Add(-time.Second)
	_ = cr.loadDemandSnapshot(sessionBeads, nil, "patrol", false)
	if buildCalls != 1 {
		t.Fatalf("before the deadline the snapshot is reused: builds=%d, want 1", buildCalls)
	}
	now = deadline
	_ = cr.loadDemandSnapshot(sessionBeads, nil, "patrol", false)
	if buildCalls != 2 {
		t.Fatalf("at the backoff deadline the snapshot must be rebuilt: builds=%d, want 2", buildCalls)
	}
}

// TestParkMailUsesTheServicesCapturedWhenThePolicyWasBuilt: the park's own
// send runs on the async commit goroutine while a reload may republish the
// controller's config and provider; the mailer and its lookup use the
// services captured when the policy was built, never cr's live fields.
func TestParkMailUsesTheServicesCapturedWhenThePolicyWasBuilt(t *testing.T) {
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Workspace:     config.Workspace{Name: "test-city"},
			Agents:        []config.Agent{{Name: "mayor", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)}, {Name: "worker", MaxActiveSessions: intPtr(2)}},
			NamedSessions: []config.NamedSession{{Name: "mayor", Template: "mayor"}},
		},
		sp:            runtime.NewFake(),
		rec:           events.Discard,
		stderr:        &stderr,
		storageRoutes: messagingSplitRoutes(store),
	}
	policy := cr.workStartFailurePolicy(store, store, nil)
	// The reload: cr's services are replaced after the policy was built.
	cr.cfg, cr.sp, cr.storageRoutes, cr.rec, cr.stderr = nil, nil, nil, nil, io.Discard
	notice := parkedWorkNotice{BeadID: "gp-1", Title: "t", Agent: "worker", Failures: 5, Reason: "boom", ParkedAt: time.Now(), ParkID: "0123456789abcdef"}
	if err := policy.notify(notice); err != nil {
		t.Fatalf("notify after a reload: %v (stderr: %s)", err, stderr.String())
	}
	if found, err := policy.lookup(notice); err != nil || !found {
		t.Fatalf("lookup after a reload: found=%v err=%v", found, err)
	}
	inbox, err := newMailProviderWithSessionStore(store, store).Inbox("mayor")
	if err != nil || len(inbox) != 1 {
		t.Fatalf("mayor inbox = %d messages, err=%v; want 1", len(inbox), err)
	}
	if !strings.Contains(stderr.String(), "park mail "+inbox[0].ID+" sent to mayor") {
		t.Fatalf("the send goes to the captured stderr:\n%s", stderr.String())
	}
}

// TestBuildDesiredState_RoutedDemandBindsTheWorkBeadOnTheNamedHolder: a
// retained on-demand named holder woken through NamedSessionRoutedDemand
// carries the routed row as its trigger — ONLY the trigger keys, its pack,
// workspace and work dir untouched — so a start that fails is charged to
// that bead (pool_start_backoff.go) instead of vanishing.
func TestBuildDesiredState_RoutedDemandBindsTheWorkBeadOnTheNamedHolder(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "solo",
			StartCommand:      "true",
			WorkQuery:         "printf ''",
			MaxActiveSessions: intPtr(1),
		}},
		NamedSessions: []config.NamedSession{{Template: "solo", Mode: "on_demand"}},
	}
	identity := cfg.NamedSessions[0].QualifiedName()
	work, err := store.Create(beads.Bead{Title: "solo routed work", Type: "task", Status: "open", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "solo"}})
	if err != nil {
		t.Fatal(err)
	}
	holder, err := store.Create(beads.Bead{
		Title:  "solo holder",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":                           "solo",
			"agent_name":                         identity,
			"alias":                              identity,
			"session_name":                       "solo-holder",
			"state":                              string(session.StateAsleep),
			session.NamedSessionMetadataKey:      "true",
			session.NamedSessionIdentityMetadata: identity,
			session.NamedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
	if !dsResult.NamedSessionRoutedDemand[identity] {
		t.Fatalf("the routed row must wake the holder: routed_demand=%v scale_counts=%v", dsResult.NamedSessionRoutedDemand, dsResult.ScaleCheckCounts)
	}
	got, err := store.Get(holder.ID)
	if err != nil {
		t.Fatal(err)
	}
	if trigger := got.Metadata[beadmeta.TriggerBeadIDMetadataKey]; trigger != work.ID {
		t.Fatalf("the holder woken for routed work %s must carry it as its trigger, got %q (metadata %v)", work.ID, trigger, got.Metadata)
	}
	if ref := got.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]; ref == "" {
		t.Fatalf("the trigger's store ref must be recorded too (metadata %v)", got.Metadata)
	}
	for _, key := range []string{beadmeta.PackMetadataKey, beadmeta.PackWorkspaceMetadataKey, beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
		if v := got.Metadata[key]; v != "" {
			t.Fatalf("a named holder's %s is its own, never derived from a wake trigger: %q", key, v)
		}
	}
}

// TestDefaultScaleCheckCountsAndDemandReportsTheEarliestBackoffDeadline: the
// probe reports the earliest backoff deadline among the rows it held back (a
// park has none), the moment the cached demand snapshot must expire.
func TestDefaultScaleCheckCountsAndDemandReportsTheEarliestBackoffDeadline(t *testing.T) {
	const template = "hello-world/polecat"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "polecat", Dir: "hello-world", MaxActiveSessions: intPtr(3)}},
	}
	now := time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC)
	store := beads.NewMemStore()
	for _, meta := range []map[string]string{
		{},
		{beadmeta.ParkedAtMetadataKey: now.Format(time.RFC3339)},
		{beadmeta.StartBackoffUntilMetadataKey: now.Add(time.Minute).Format(time.RFC3339)},
		{beadmeta.StartBackoffUntilMetadataKey: now.Add(30 * time.Second).Format(time.RFC3339)},
	} {
		meta[beadmeta.RoutedToMetadataKey] = template
		if _, err := store.Create(beads.Bead{Title: "w", Type: "task", Status: "open", Metadata: meta}); err != nil {
			t.Fatal(err)
		}
	}
	counts, demand, _, errs := defaultScaleCheckCountsAndDemand(cfg, now, []defaultScaleCheckTarget{{template: template, storeKey: "city", store: store}})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if counts[template] != 1 || demand[template].Deferred != 3 {
		t.Fatalf("count=%d deferred=%d, want 1 and 3", counts[template], demand[template].Deferred)
	}
	if got, want := demand[template].DeferredUntil, now.Add(30*time.Second); !got.Equal(want) {
		t.Fatalf("DeferredUntil = %s, want the earliest backoff deadline %s", got, want)
	}
	// The same reduction over an assigned-work snapshot, and the zero cases.
	all, err := store.List(beads.ListQuery{Type: "task", AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := earliestStartDeferralDeadline(all, now); !got.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("earliestStartDeferralDeadline = %s", got)
	}
	if got := earliestStartDeferralDeadline(nil, now); !got.IsZero() {
		t.Fatalf("no rows → zero, got %s", got)
	}
	if got := earlierDeadline(time.Time{}, now); !got.Equal(now) {
		t.Fatalf("earlierDeadline(zero, now) = %s", got)
	}
	if got := earlierDeadline(now, time.Time{}); !got.Equal(now) {
		t.Fatalf("earlierDeadline(now, zero) = %s", got)
	}
}
