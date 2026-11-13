package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// freshWakeOpenSessions returns the session beads in store that are not closed.
func freshWakeOpenSessions(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	var open []beads.Bead
	for _, row := range freshWakeSessionRows(t, store) {
		if row.Status != "closed" {
			open = append(open, row)
		}
	}
	return open
}

// TestFreshWakePool_CityStopAsleepSessionConvergesToLiveOwner is the
// end-to-end convergence proof #4868 was merged without (gastownhall/gascity#4849
// follow-up). #4868 stops a wake_mode="fresh" pool agent from resuming a stale
// asleep session bead; instead the desired-state build plans a clean create
// (a wake-known-identity request with an empty SessionBeadID) that stays bound
// to the assigned work. The hand-off of that work to the replacement session
// was only ever traced statically: the asleep bead drops out of the desired
// set, and repairStrandedPoolWorkerBead reopens the work after
// strandedRepairConfirmGrace. This test drives the real chain, through the real
// build path, on a MemStore:
//
//  1. A fresh-wake pool agent has in_progress work assigned to an asleep
//     sleep_reason=city-stop session bead. Pool realization must not re-select
//     that bead: it plans and writes exactly one clean replacement in a pool
//     slot. The session overlay still keeps the stale row desired (it holds
//     assigned work), and that is load-bearing: the reconciler only reaches
//     repairStrandedPoolWorkerBead for a desired, non-live seat.
//  2. Inside the grace window the stranded repair defers; past it, the repair
//     reopens and unassigns the work and closes the stale bead.
//  3. The replacement claims the reopened work. The next build desires exactly
//     that live session and creates nothing further: a fixed point, so the work
//     has one live owner and no session churn.
func TestFreshWakePool_CityStopAsleepSessionConvergesToLiveOwner(t *testing.T) {
	const template = "worker"
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Agents: []config.Agent{{
			Name:              template,
			StartCommand:      "true",
			WorkQuery:         "printf ''",
			WakeMode:          "fresh",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
		}},
	}

	// The stale row #4868 targets: asleep, left by a city stop, still holding
	// the work.
	stale, err := store.Create(beads.Bead{
		Title:  "stale pool session",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "template:" + template},
		Metadata: map[string]string{
			"template":             template,
			"state":                string(sessionpkg.StateAsleep),
			"sleep_reason":         string(sessionpkg.SleepReasonCityStop),
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("create stale session bead: %v", err)
	}
	staleName := PoolSessionName(template, stale.ID)
	if err := store.SetMetadata(stale.ID, "session_name", staleName); err != nil {
		t.Fatalf("name stale session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "work held by the stale session",
		Type:     "task",
		Assignee: stale.ID,
		Metadata: map[string]string{"gc.routed_to": template},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}

	build := func(t *testing.T) DesiredStateResult {
		t.Helper()
		var stderr bytes.Buffer
		snapshot := newSessionBeadSnapshot(freshWakeSessionRows(t, store))
		ds := buildDesiredStateWithSessionBeadsAt(
			"gc", cityPath, now, now, cfg, runtime.NewFake(),
			store, nil, snapshot, nil, &stderr,
		)
		if stderr.Len() > 0 {
			t.Logf("build stderr:\n%s", stderr.String())
		}
		return ds
	}

	// 1. The first build plans a clean replacement, never the stale bead.
	ds := build(t)
	open := freshWakeOpenSessions(t, store)
	if len(open) != 2 {
		t.Fatalf("open session beads = %d, want 2 (stale + one clean replacement): %#v", len(open), open)
	}
	var replacement beads.Bead
	for _, b := range open {
		if b.ID != stale.ID {
			replacement = b
		}
	}
	replacementName := replacement.Metadata["session_name"]
	if replacementName == "" || replacementName == staleName {
		t.Fatalf("replacement session_name = %q, want a name distinct from the stale %q", replacementName, staleName)
	}
	if tp, ok := ds.State[replacementName]; !ok {
		t.Fatalf("replacement %q missing from desired State: %v", replacementName, freshWakeStateKeys(ds.State))
	} else if tp.InstanceName == staleName {
		t.Fatalf("replacement realized as the stale identity %q", staleName)
	}
	if tp, ok := ds.State[staleName]; !ok {
		t.Fatalf("stale row %q dropped from desired State before the stranded repair could reach it: %v", staleName, freshWakeStateKeys(ds.State))
	} else if tp.InstanceName != staleName {
		t.Fatalf("stale row desired as pool instance %q, want the overlay's own identity %q (pool realization re-selected it)", tp.InstanceName, staleName)
	}
	if got, _ := store.Get(work.ID); got.Assignee != stale.ID || got.Status != "in_progress" {
		t.Fatalf("work moved before the stranded repair ran: status=%q assignee=%q", got.Status, got.Assignee)
	}

	// 2. The stranded repair: deferred inside the grace window, effective past it.
	staleInfo := func(emittedAt time.Time) sessionpkg.Info {
		b, err := store.Get(stale.ID)
		if err != nil {
			t.Fatalf("reload stale session bead: %v", err)
		}
		b.Metadata[strandedEventEmittedKey] = emittedAt.Format(time.RFC3339)
		return seedSessionInfo(b)
	}
	var stderr bytes.Buffer
	if repairStrandedPoolWorkerBead(testSeatWork(cityPath, cfg, store, nil), store, staleInfo(now), &clock.Fake{Time: now}, &stderr) {
		t.Fatalf("repair acted inside the %s grace window; stderr=%q", strandedRepairConfirmGrace, stderr.String())
	}
	if got, _ := store.Get(work.ID); got.Assignee != stale.ID {
		t.Fatalf("work released inside the grace window: assignee=%q", got.Assignee)
	}
	later := now.Add(strandedRepairConfirmGrace + time.Minute)
	if !repairStrandedPoolWorkerBead(testSeatWork(cityPath, cfg, store, nil), store, staleInfo(now), &clock.Fake{Time: later}, &stderr) {
		t.Fatalf("repair did not return the work past the grace window; stderr=%q", stderr.String())
	}
	got, _ := store.Get(work.ID)
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("after repair work status=%q assignee=%q, want open and unassigned", got.Status, got.Assignee)
	}
	if staleAfter, _ := store.Get(stale.ID); staleAfter.Status != "closed" {
		t.Fatalf("stale session status = %q after repair, want closed", staleAfter.Status)
	}

	// 3. The replacement claims the reopened work; the build is now a fixed point.
	if err := store.Update(work.ID, beads.UpdateOpts{Assignee: &replacement.ID, Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("replacement claims work: %v", err)
	}
	ds = build(t)
	if _, ok := ds.State[replacementName]; !ok {
		t.Fatalf("live owner %q not desired after it claimed the work: %v", replacementName, freshWakeStateKeys(ds.State))
	}
	if _, resumed := ds.State[staleName]; resumed {
		t.Fatalf("closed stale session %q is desired again", staleName)
	}
	final := freshWakeOpenSessions(t, store)
	if len(final) != 1 || final[0].ID != replacement.ID {
		t.Fatalf("open session beads after convergence = %#v, want only the replacement %s", final, replacement.ID)
	}
}

// freshWakeSessionRows returns every session bead in store, closed ones included.
func freshWakeSessionRows(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	rows, err := store.List(beads.ListQuery{Label: sessionBeadLabel})
	if err != nil {
		t.Fatalf("list session beads: %v", err)
	}
	return rows
}

func freshWakeStateKeys(m map[string]TemplateParams) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
