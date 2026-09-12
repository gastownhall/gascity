package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The cases codex round 17 found (evidence 07-codex-r17.md).

// TestUndesiredZombieUncommittedStartClearsNothing: a kept resume whose
// agent died during startup but whose wrapper stayed (running, not alive)
// is a zombie, not a lane that starts: the undesired branch's clear reads
// the agent's liveness through the template's process-name hints and
// leaves the failed-start record alone.
func TestUndesiredZombieUncommittedStartClearsNothing(t *testing.T) {
	h := newStartBackoffHarness(t, 5)
	h.env.cfg.Agents[0].ProcessNames = []string{"agent-binary"}
	if starts := h.advance(15*time.Second, errPreStartFailure); starts != 2 {
		t.Fatalf("starts = %d, want 2\nstderr:\n%s", starts, h.env.stderr.String())
	}
	const name = "sky-zombie"
	orphan, err := h.env.store.Create(beads.Bead{
		Title:  backoffHarnessTemplate,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:" + backoffHarnessTemplate},
		Metadata: map[string]string{
			"session_name": name, "session_name_explicit": "true", "template": backoffHarnessTemplate,
			"state": string(session.StateCreating), "generation": "2", "continuation_epoch": "1", "instance_token": "zombie-token",
			beadmeta.TriggerBeadIDMetadataKey: h.work.ID, beadmeta.TriggerBeadStoreRefMetadataKey: "city",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.env.sp.Start(context.Background(), name, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatal(err)
	}
	h.env.sp.Zombies[name] = true // the wrapper runs, the agent is dead
	for key, value := range map[string]string{"GC_SESSION_ID": orphan.ID, "GC_INSTANCE_TOKEN": "zombie-token"} {
		if err := h.env.sp.SetMeta(name, key, value); err != nil {
			t.Fatal(err)
		}
	}
	h.env.desiredState = map[string]TemplateParams{}
	h.env.reconcileWithPoolDesired([]beads.Bead{orphan}, map[string]int{backoffHarnessTemplate: 0})
	if got := readWorkStartFailureState(h.reload().Metadata).Failures; got != 2 {
		t.Fatalf("a zombie is not a started lane: the record must stay at 2, got %d\nstderr:\n%s", got, h.env.stderr.String())
	}
}

// TestNamedHolderKeepsTheTriggerOfItsOwnCityStoreClaim: a rig-scoped named
// holder's own in-progress claim recorded under the city store ref is the
// work it is woken for (the wake path accepts it through the claim refs),
// so it is the trigger too — never an empty request that clears the
// trigger and leaves the start uncharged.
func TestNamedHolderKeepsTheTriggerOfItsOwnCityStoreClaim(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Rigs:          []config.Rig{{Name: "riga", Path: "riga"}},
		Agents:        []config.Agent{{Name: "worker", Dir: "riga", StartCommand: "true", MaxActiveSessions: intPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Dir: "riga", Mode: "on_demand"}},
	}
	if ref := assignedWorkStoreRefForAgent(cityPath, cfg, findAgentByTemplate(cfg, "riga/worker")); ref != "riga" {
		t.Fatalf("fixture: the holder's agent is rig-scoped to riga, got %q", ref)
	}
	identity := cfg.NamedSessions[0].QualifiedName()
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{Title: "owned city-store claim", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: identity}})
	if err != nil {
		t.Fatal(err)
	}
	status := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &identity}); err != nil {
		t.Fatal(err)
	}
	holder, err := store.Create(beads.Bead{
		Title: "retained rig holder", Type: sessionBeadType, Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template": identity, "agent_name": identity, "alias": identity,
			"session_name": "riga-holder", "state": string(session.StateAsleep),
			session.NamedSessionMetadataKey: "true", session.NamedSessionIdentityMetadata: identity, session.NamedSessionModeMetadata: "on_demand",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	res := buildDesiredState("test-city", cityPath, time.Now(), cfg, runtime.NewFake(), store, &stderr)
	tp, ok := res.State["riga-holder"]
	if !ok || tp.TriggerBeadID != work.ID || tp.TriggerBeadStoreRef != "" {
		t.Fatalf("configured-name city-store claim must bind through the production builder: params=%+v present=%v\nstderr: %s", tp, ok, stderr.String())
	}
	row, err := store.Get(holder.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Metadata[beadmeta.TriggerBeadIDMetadataKey] != work.ID {
		t.Fatalf("holder's persisted trigger = %q, want %s", row.Metadata[beadmeta.TriggerBeadIDMetadataKey], work.ID)
	}
}

// TestParkMailThrottleIsPerPark: two copies of one bead (a retained
// primary-store copy and the active class-store copy) owe different parks;
// the primary copy's stamp keeps failing. Its throttle entry must not
// silence the active copy: both are mailed in the same sweep.
func TestParkMailThrottleIsPerPark(t *testing.T) {
	const id = "gg-two-parks"
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
	now := time.Date(2026, 9, 12, 7, 0, 0, 0, time.UTC)
	park := func(parkID string) map[string]string {
		return map[string]string{beadmeta.RoutedToMetadataKey: "worker", beadmeta.ParkedAtMetadataKey: now.Format(time.RFC3339), beadmeta.ParkReasonMetadataKey: "boom", beadmeta.ParkFailuresMetadataKey: "5", beadmeta.ParkIDMetadataKey: parkID}
	}
	primaryMem := beads.NewMemStoreFrom(1, []beads.Bead{{ID: id, Title: "retained copy", Type: "task", Status: "in_progress", Metadata: park("aaaa")}}, nil)
	primary := &failMetadataOnStore{Store: primaryMem, id: id, err: errors.New("primary store: write timed out")}
	class := beads.NewMemStoreFrom(1, []beads.Bead{{ID: id, Title: "active copy", Type: "task", Status: "in_progress", Metadata: park("bbbb")}}, nil)
	var mails []string
	policy := &workStartFailurePolicy{
		workStore:         primary,
		extraStores:       []beads.Store{class},
		templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
		canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
		limitFor:          func(string) int { return 5 },
		notify:            func(n parkedWorkNotice) error { mails = append(mails, n.ParkID); return nil },
		retry:             &parkMailRetryState{},
	}
	policy.sweepUnmailedParks(now)
	policy.awaitParkMailRetries()
	if len(mails) != 2 || !round14Contains(mails, "aaaa") || !round14Contains(mails, "bbbb") {
		t.Fatalf("both parks are mailed in one sweep (the primary copy's failed stamp throttles only ITS park): %v", mails)
	}
	if row, _ := class.Get(id); readWorkStartFailureState(row.Metadata).ParkMailedAt.IsZero() {
		t.Fatalf("the active copy is stamped: %v", row.Metadata)
	}
}
