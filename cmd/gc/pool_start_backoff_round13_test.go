package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The cases codex round 13 found (evidence 07-codex-r13.md).

// TestStartDeferredReadsTheLiveRow: the start-time gate's verdict is read
// off the backing behind a caching store — a park written there since the
// plan (another seat's failure, through the live write path) is invisible
// to the cache the plan read, and a start made past it would clear it on
// success.
func TestStartDeferredReadsTheLiveRow(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1), MaxStartFailures: intPtr(5)}}}
	backing := beads.NewMemStore()
	caching := beads.NewCachingStoreForTest(backing, nil)
	work, err := caching.Create(beads.Bead{Title: "cached work", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := caching.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	wrapped := &beadPolicyStore{Store: caching, cfg: cfg}
	policy := &workStartFailurePolicy{workStore: wrapped, limitFor: func(string) int { return 5 }}
	if d, _ := policy.startDeferred(workTrigger{BeadID: work.ID, StoreRef: "city"}, now); d {
		t.Fatal("control: an unparked bead is not deferred")
	}
	// The park lands on the backing, behind the cache's back.
	if err := backing.SetMetadataBatch(work.ID, map[string]string{beadmeta.ParkedAtMetadataKey: now.Format(time.RFC3339), beadmeta.ParkReasonMetadataKey: "boom", beadmeta.ParkFailuresMetadataKey: "5"}); err != nil {
		t.Fatal(err)
	}
	if cached, _ := wrapped.Get(work.ID); readWorkStartFailureState(cached.Metadata).Parked() {
		t.Fatal("fixture: the cache must still serve the unparked row")
	}
	if d, reason := policy.startDeferred(workTrigger{BeadID: work.ID, StoreRef: "city"}, now); !d || reason != "parked" {
		t.Fatalf("the gate reads the LIVE row: deferred=%v reason=%q", d, reason)
	}
}

// TestKeptSessionAsyncResumeKeepsItsUncommittedStateAcrossAnInterveningTick:
// a kept session's resume is in flight (creating, a current lease, no
// claim, runtime alive) when another tick runs. That tick's heal moves the
// state on and its recovery branch defers to the lease — but the bead must
// still say the start is uncommitted afterwards, so that when the async
// commit's clear fails (it clears only the lease) the next tick recovers.
func TestKeptSessionAsyncResumeKeepsItsUncommittedStateAcrossAnInterveningTick(t *testing.T) {
	h := newStartBackoffHarness(t, 5)
	if starts := h.advance(15*time.Second, errPreStartFailure); starts != 2 {
		t.Fatalf("starts = %d, want 2\nstderr:\n%s", starts, h.env.stderr.String())
	}
	h.env.clk.Time = h.env.clk.Time.Add(time.Minute)
	const name = "sky-async"
	kept, err := h.env.store.Create(beads.Bead{
		Title:  backoffHarnessTemplate,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:" + backoffHarnessTemplate},
		Metadata: map[string]string{
			"session_name":                          name,
			"session_name_explicit":                 "true",
			"template":                              backoffHarnessTemplate,
			"state":                                 string(session.StateCreating),
			"last_woke_at":                          h.env.clk.Now().UTC().Format(time.RFC3339), // the in-flight lease
			"generation":                            "2",
			"continuation_epoch":                    "1",
			"instance_token":                        "async-token",
			beadmeta.TriggerBeadIDMetadataKey:       h.work.ID,
			beadmeta.TriggerBeadStoreRefMetadataKey: "city",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The runtime the async start already spawned.
	if err := h.env.sp.Start(context.Background(), name, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"GC_SESSION_ID": kept.ID, "GC_INSTANCE_TOKEN": "async-token"} {
		if err := h.env.sp.SetMeta(name, key, value); err != nil {
			t.Fatal(err)
		}
	}
	desired := map[string]TemplateParams{name: {Command: "test-cmd", SessionName: name, TemplateName: backoffHarnessTemplate}}
	h.env.desiredState = desired
	// The intervening tick.
	h.env.reconcileWithPoolDesired([]beads.Bead{kept}, map[string]int{backoffHarnessTemplate: 1})
	row, _ := h.env.store.Get(kept.ID)
	info := sessionInfosFromBeads([]beads.Bead{row})[0]
	if !pendingCreateQueuedOrCreatingState(info.MetadataState) || strings.TrimSpace(info.CreationCompleteAt) != "" {
		t.Fatalf("an in-flight start stays uncommitted across a tick: state=%q creation_complete=%q\nstderr:\n%s", info.MetadataState, info.CreationCompleteAt, h.env.stderr.String())
	}
	if got := readWorkStartFailureState(h.reload().Metadata).Failures; got != 2 {
		t.Fatalf("no recovery while the lease is current: record = %d", got)
	}
	// The async commit's clear failed: it cleared only the lease.
	if err := h.env.store.SetMetadataBatch(kept.ID, map[string]string{"last_woke_at": ""}); err != nil {
		t.Fatal(err)
	}
	row, _ = h.env.store.Get(kept.ID)
	h.installPolicy()
	h.env.desiredState = desired
	h.env.clk.Time = h.env.clk.Time.Add(time.Second)
	h.env.reconcileWithPoolDesired([]beads.Bead{row}, map[string]int{backoffHarnessTemplate: 1})
	if got := h.reload(); len(workStartFailureClearPatch(got.Metadata)) != 0 {
		t.Fatalf("the next tick recovers: clear first: %v\nstderr:\n%s", got.Metadata, h.env.stderr.String())
	}
	row, _ = h.env.store.Get(kept.ID)
	info = sessionInfosFromBeads([]beads.Bead{row})[0]
	if strings.TrimSpace(info.CreationCompleteAt) == "" || session.State(strings.TrimSpace(info.MetadataState)) != session.StateActive {
		t.Fatalf("then confirms: state=%q creation_complete=%q\nstderr:\n%s", info.MetadataState, info.CreationCompleteAt, h.env.stderr.String())
	}
	if !h.env.sp.IsRunning(name) || len(h.mails) != 0 {
		t.Fatalf("same runtime, no mail (running=%v mails=%d)", h.env.sp.IsRunning(name), len(h.mails))
	}
}
