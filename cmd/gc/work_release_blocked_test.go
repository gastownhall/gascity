package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/gastownhall/gascity/internal/session"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// BdStore.Get supplies the normalized snapshot; assertions use the fake CLI's
// raw row, so MemStore's status projection cannot conceal a blocked-to-open edit.
func TestReleaseListedPreservesRawBlockedHold(t *testing.T) {
	for _, why := range []releaseWhy{releaseDrainAck, releaseCloseCascade, releaseStrandedRepair, releaseOrphan, releaseRetired} {
		t.Run(string(rune('a'+why)), func(t *testing.T) {
			ledger := newBdGuardLedger("blocked", "dead-session", map[string]string{beadmeta.RoutedToMetadataKey: "repo/worker", "hold_reason": "needs input", beadmeta.ContinuationGroupMetadataKey: "group"})
			run := func(dir, name string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "list" {
					return ledger.render()
				}
				if len(args) > 0 && args[0] == "query" {
					return []byte("[]"), nil
				}
				return ledger.run(dir, name, args...)
			}
			backing := beads.NewBdStoreWithPrefix(t.TempDir(), run, "bd")
			snapshot, err := backing.Get(ledger.bead.ID)
			if err != nil || snapshot.Status != "open" {
				t.Fatalf("normalized snapshot = %+v, %v", snapshot, err)
			}
			before := ledger.row()
			// The cache must forward the raw guard, even though its own row is open.
			store := beads.NewCachingStore(backing, nil)
			out := releaseListed([]seatWorkHit{{store: store, bead: snapshot}}, why, releaseSeat{}, &config.City{Agents: []config.Agent{persistentWorker()}}, ReleaseOpts{})
			if len(out.Released) != 1 || len(out.Failed) != 0 {
				t.Fatalf("release = %+v", out)
			}
			want := before
			want.Assignee = ""
			want.Metadata = make(map[string]string, len(before.Metadata))
			for key, value := range before.Metadata {
				want.Metadata[key] = value
			}
			for _, key := range beadmeta.SessionAffinityMetadataKeys {
				want.Metadata[key] = ""
			}
			if got := ledger.row(); !reflect.DeepEqual(got, want) {
				t.Fatalf("raw row = %+v, want %+v", got, want)
			}
			if len(ledger.updates) != 1 || hasBdFlag(ledger.updates[0], "--status") || !hasBdFlag(ledger.updates[0], "--set-metadata") {
				t.Fatalf("not one status-preserving release: %v", ledger.updates)
			}
			if got := testFlagValue(ledger.updates[0], "--if-status"); got != "blocked" {
				t.Fatalf("raw status guard = %q", got)
			}
		})
	}
}

func TestReleaseListedBlockedRouteAndLifecycle(t *testing.T) {
	for _, why := range []releaseWhy{releaseCloseCascade, releaseRetired} {
		for _, routeKey := range []string{"", beadmeta.RoutedToMetadataKey, beadmeta.RunTargetMetadataKey} {
			t.Run(string(rune('a'+why))+routeKey, func(t *testing.T) {
				ledger := newBdGuardLedger("blocked", "dead-session", nil)
				delete(ledger.bead.Metadata, beadmeta.RoutedToMetadataKey)
				if routeKey != "" {
					ledger.bead.Metadata[routeKey] = "repo/worker"
				}
				for _, key := range beadmeta.SessionAffinityMetadataKeys {
					ledger.bead.Metadata[key] = "obsolete-session"
				}
				ledger.bead.Metadata["hold_reason"] = "needs human input"
				ledger.bead.Metadata["work_result"] = "retained"
				ledger.bead.Labels = []string{"hold:external"}
				ledger.bead.Description = "retained description"
				backing := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
				cache := beads.NewCachingStore(backing, nil)
				snapshot, err := cache.Get(ledger.bead.ID)
				if err != nil {
					t.Fatal(err)
				}
				out := releaseListed([]seatWorkHit{{store: cache, bead: snapshot}}, why, releaseSeat{fallback: "repo/worker"}, &config.City{Agents: []config.Agent{persistentWorker()}}, ReleaseOpts{})
				if len(out.Released) != 1 || len(out.Kept) != 0 || len(out.Failed) != 0 || len(ledger.updates) != 1 {
					t.Fatalf("release = %+v, updates %v", out, ledger.updates)
				}
				row := ledger.row()
				if row.Status != "blocked" || row.Assignee != "" || row.Metadata["hold_reason"] != "needs human input" || row.Metadata["work_result"] != "retained" || !reflect.DeepEqual(row.Labels, []string{"hold:external"}) || row.Description != "retained description" {
					t.Fatalf("hold or work changed: %+v", row)
				}
				wantTarget := "repo/worker"
				if routeKey == beadmeta.RoutedToMetadataKey {
					wantTarget = ""
				}
				if row.Metadata[beadmeta.RunTargetMetadataKey] != wantTarget || row.Metadata[beadmeta.RoutedToMetadataKey] != snapshot.Metadata[beadmeta.RoutedToMetadataKey] {
					t.Fatalf("route = %v", row.Metadata)
				}
				cached, err := cache.Get(row.ID)
				if err != nil || cached.Assignee != "" || cached.Metadata[beadmeta.RunTargetMetadataKey] != wantTarget {
					t.Fatalf("cache did not absorb combined patch: %+v, %v", cached, err)
				}
				for _, key := range beadmeta.SessionAffinityMetadataKeys {
					if row.Metadata[key] != "" || cached.Metadata[key] != "" {
						t.Fatalf("stale %s: raw %v, cached %v", key, row.Metadata, cached.Metadata)
					}
				}
			})
		}
	}
}

func TestReleaseListedBlockedGuards(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		change                         func(*beads.Bead)
		unsupported, started, unserved bool
	}{
		{name: "owner changed", change: func(b *beads.Bead) { b.Assignee = "live-session" }},
		{name: "status changed", change: func(b *beads.Bead) { b.Status = "in_progress" }},
		{name: "reopened during blocked guard", change: func(b *beads.Bead) { b.Status = "open" }},
		{name: "unsupported", unsupported: true},
		{name: "killed seat started guard", started: true},
		{name: "unserved route", unserved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newBdGuardLedger("blocked", "dead-session", map[string]string{beadmeta.RoutedToMetadataKey: "repo/worker", beadmeta.ContinuationGroupMetadataKey: "group", "hold_reason": "needs input"})
			store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
			snapshot, err := store.Get(ledger.bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			ledger.beforeUpdate = tc.change
			ledger.noGuards = tc.unsupported
			beforeMetadata := ledger.row().Metadata
			seat := releaseSeat{}
			why := releaseCloseCascade
			if tc.started {
				why = releaseKilledSeat
				seat.started = map[string]bool{snapshot.ID: true}
			}
			if tc.unserved {
				snapshot.Metadata[beadmeta.RoutedToMetadataKey] = "missing"
			}
			out := releaseListed([]seatWorkHit{{store: store, bead: snapshot}}, why, seat, &config.City{Agents: []config.Agent{persistentWorker()}}, ReleaseOpts{})
			if len(out.Released) != 0 {
				t.Fatalf("released protected hold: %+v", out)
			}
			if ledger.row().Assignee == "" {
				t.Fatal("lost protected owner")
			}
			if !reflect.DeepEqual(ledger.row().Metadata, beforeMetadata) {
				t.Fatalf("metadata changed on refusal: %v", ledger.row().Metadata)
			}
			if tc.unsupported && len(out.Failed) != 1 {
				t.Fatalf("unsupported guard was not surfaced: %+v", out)
			}
			if (tc.started || tc.unserved) && len(ledger.updates) != 0 {
				t.Fatalf("write before gate: %v", ledger.updates)
			}
		})
	}
}

func TestReleaseListedDrainKeepsOrdinaryOpenPreassignment(t *testing.T) {
	ledger := newBdGuardLedger("open", "dead-session", map[string]string{beadmeta.RoutedToMetadataKey: "repo/worker"})
	store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
	snapshot, err := store.Get(ledger.bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := releaseListed([]seatWorkHit{{store: store, bead: snapshot}}, releaseDrainAck, releaseSeat{}, &config.City{Agents: []config.Agent{persistentWorker()}}, ReleaseOpts{})
	if len(out.Released) != 0 || ledger.row().Assignee != "dead-session" {
		t.Fatalf("drain released continuation preassignment: %+v", out)
	}
}

func TestReleaseListedOrdinaryOpenChildClaim(t *testing.T) {
	ledger := newBdGuardLedger("open", "dead-session", map[string]string{beadmeta.RoutedToMetadataKey: "repo/worker"})
	ledger.bead.ID = "bd-7.1"
	store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
	snapshot, err := store.Get(ledger.bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := releaseListed([]seatWorkHit{{store: store, bead: snapshot}}, releaseCloseCascade, releaseSeat{}, &config.City{Agents: []config.Agent{persistentWorker()}}, ReleaseOpts{})
	if len(out.Released) != 1 || len(out.Kept) != 0 || len(out.Failed) != 0 || ledger.row().Assignee != "" || ledger.row().Status != "open" {
		t.Fatalf("ordinary open child not released: %+v, row %+v", out, ledger.row())
	}
	if len(ledger.updates) != 1 || testFlagValue(ledger.updates[0], "--if-status") != "open" {
		t.Fatalf("ordinary release lost its guard: %v", ledger.updates)
	}
}

func TestReleaseListedKilledBlockedClaim(t *testing.T) {
	for _, tc := range []struct {
		name, otherStatus      string
		seatStarted, guardMiss bool
	}{
		{name: "no started evidence"},
		{name: "other claim named started", otherStatus: "open", seatStarted: true},
		{name: "other claim in progress", otherStatus: "in_progress"},
		{name: "blocked guard lost", guardMiss: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newBdGuardLedger("blocked", "dead-session", map[string]string{beadmeta.RoutedToMetadataKey: "repo/worker", "hold_reason": "needs input"})
			store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
			snapshot, err := store.Get(ledger.bead.ID)
			if err != nil || snapshot.Status != "open" {
				t.Fatalf("snapshot = %+v, %v", snapshot, err)
			}
			before := ledger.row()
			claims := []seatWorkHit{{store: store, bead: snapshot}}
			seat := releaseSeat{}
			if tc.otherStatus != "" {
				other := snapshot
				other.ID, other.Status = "bd-8", tc.otherStatus
				claims = append(claims, seatWorkHit{store: store, bead: other})
				if tc.seatStarted {
					seat.started = map[string]bool{other.ID: true}
				}
			}
			if tc.guardMiss {
				ledger.beforeUpdate = func(b *beads.Bead) { b.Assignee = "live-session" }
			}
			out := releaseListed(claims, releaseKilledSeat, seat, &config.City{Agents: []config.Agent{persistentWorker()}}, ReleaseOpts{})
			if tc.otherStatus != "" {
				if len(out.Released) != 0 || len(out.Failed) != 0 || len(out.Kept) != len(claims) || len(ledger.updates) != 0 || !reflect.DeepEqual(ledger.row(), before) {
					t.Fatalf("started seat lost a claim: %+v, row %+v", out, ledger.row())
				}
				for _, claim := range claims {
					if out.Why[claim.bead.ID] != keptStarted {
						t.Fatalf("keep reason = %v", out.Why)
					}
				}
				return
			}
			if tc.guardMiss {
				if len(out.Released) != 0 || len(out.Failed) != 0 || len(out.Kept) != 1 || out.Why[snapshot.ID] != keptMoved || ledger.row().Assignee != "live-session" {
					t.Fatalf("lost guard counted as release: %+v, row %+v", out, ledger.row())
				}
				return
			}
			want := before
			want.Assignee = ""
			want.Metadata = make(map[string]string, len(before.Metadata))
			for key, value := range before.Metadata {
				want.Metadata[key] = value
			}
			for _, key := range beadmeta.SessionAffinityMetadataKeys {
				want.Metadata[key] = ""
			}
			if len(out.Released) != 1 || len(out.Kept) != 0 || len(out.Failed) != 0 || !reflect.DeepEqual(ledger.row(), want) {
				t.Fatalf("unstarted killed seat still owns hold: %+v, row %+v", out, ledger.row())
			}
			if len(ledger.updates) != 1 || testFlagValue(ledger.updates[0], "--if-status") != "blocked" || hasBdFlag(ledger.updates[0], "--status") || !hasBdFlag(ledger.updates[0], "--set-metadata") {
				t.Fatalf("not one guarded blocked release: %v", ledger.updates)
			}
		})
	}
}

// Drain releases started claims in order and never probes ordinary open
// preassignments or reports them as budget failures.
func TestReleaseListedDrainPrioritizesStartedClaimsWithinBudget(t *testing.T) {
	now := time.Unix(100, 0)
	deadline := now.Add(time.Second)
	var visits []string
	var ledgers []*bdGuardLedger
	var claims []seatWorkHit
	for i, status := range []string{"open", "in_progress", "open", "in_progress"} {
		ledger := newBdGuardLedger(status, "dead-session", map[string]string{
			beadmeta.RoutedToMetadataKey:          "repo/worker",
			beadmeta.ContinuationGroupMetadataKey: "group",
		})
		ledger.bead.ID = []string{"bd-open1", "bd-started1", "bd-open2", "bd-started2"}[i]
		run := func(dir, name string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "show" {
				visits = append(visits, ledger.bead.ID)
				if status == "open" {
					now = deadline.Add(time.Second)
				}
			}
			return ledger.run(dir, name, args...)
		}
		store := beads.NewBdStoreWithPrefix(t.TempDir(), run, "bd")
		ledgers = append(ledgers, ledger)
		claims = append(claims, seatWorkHit{store: store, bead: ledger.row()})
	}
	out := releaseListedWithClock(claims, releaseDrainAck, releaseSeat{}, &config.City{Agents: []config.Agent{persistentWorker()}}, ReleaseOpts{Deadline: deadline}, func() time.Time { return now })
	if got, want := visits, []string{"bd-started1", "bd-started2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("store reads = %v, want %v", got, want)
	}
	if len(out.Released) != 2 || out.Released[0].bead.ID != "bd-started1" || out.Released[1].bead.ID != "bd-started2" || len(out.Kept) != 2 || len(out.Failed) != 0 || out.Why["bd-open1"] != keptPreassigned || out.Why["bd-open2"] != keptPreassigned {
		t.Fatalf("release = %+v", out)
	}
	for i, ledger := range ledgers {
		want := "dead-session"
		if i == 1 || i == 3 {
			want = ""
		}
		if got := ledger.row().Assignee; got != want {
			t.Fatalf("claim %s owner = %q, want %q", ledger.bead.ID, got, want)
		}
	}
}

func TestReleaseListedOtherModesPreserveSnapshotOrder(t *testing.T) {
	for _, why := range []releaseWhy{releaseKilledSeat, releaseCloseCascade, releaseStrandedRepair, releaseOrphan, releaseRetired} {
		t.Run(string(rune('a'+why)), func(t *testing.T) {
			store := beads.NewMemStore()
			var claims []seatWorkHit
			for _, status := range []string{"open", "in_progress", "open", "in_progress"} {
				b, err := store.Create(beads.Bead{Status: status, Assignee: "dead-session", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "repo/worker"}})
				if err != nil {
					t.Fatal(err)
				}
				claims = append(claims, seatWorkHit{store: store, bead: b})
			}
			out := releaseListed(claims, why, releaseSeat{}, &config.City{Agents: []config.Agent{persistentWorker()}}, ReleaseOpts{})
			got := out.Released
			if why == releaseKilledSeat {
				got = out.Kept
			}
			if len(got) != len(claims) {
				t.Fatalf("release = %+v", out)
			}
			for i := range claims {
				if got[i].bead.ID != claims[i].bead.ID {
					t.Fatalf("claim %d = %s, want %s", i, got[i].bead.ID, claims[i].bead.ID)
				}
			}
		})
	}
}

func TestReleaseListedPreservesOtherNormalizedOpenHolds(t *testing.T) {
	for _, status := range []string{"review", "testing", "deferred", "custom"} {
		t.Run(status, func(t *testing.T) {
			ledger := newBdGuardLedger(status, "dead-session", map[string]string{beadmeta.RoutedToMetadataKey: "repo/worker"})
			store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
			snapshot, err := store.Get(ledger.bead.ID)
			if err != nil || snapshot.Status != "open" {
				t.Fatalf("snapshot = %+v, %v", snapshot, err)
			}
			before := ledger.row()
			out := releaseListed([]seatWorkHit{{store: store, bead: snapshot}}, releaseCloseCascade, releaseSeat{}, &config.City{Agents: []config.Agent{persistentWorker()}}, ReleaseOpts{})
			if len(out.Kept) != 1 || out.Why[snapshot.ID] != keptMoved || len(out.Released) != 0 || len(out.Failed) != 0 || len(ledger.updates) != 0 || !reflect.DeepEqual(ledger.row(), before) {
				t.Fatalf("changed %s hold: %+v, row %+v, updates %v", status, out, ledger.row(), ledger.updates)
			}
		})
	}
}

func TestBlockedOrphanRetainsLiveOrUnreadableHolder(t *testing.T) {
	for _, unreadable := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "unreadable"}[unreadable], func(t *testing.T) {
			ledger := newBdGuardLedger("blocked", "live-session", map[string]string{beadmeta.RoutedToMetadataKey: "repo/worker"})
			store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
			snapshot, err := store.Get(ledger.bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			mem := beads.NewMemStore()
			_, err = mem.Create(beads.Bead{ID: "live-session", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"session_name": "live-session"}})
			if err != nil {
				t.Fatal(err)
			}
			var sessions beads.Store = mem
			if unreadable {
				sessions = &blockedSessionReadFailure{Store: mem}
			}
			released := releaseOrphanedPoolAssignments(store, beads.SessionStore{Store: sessions}, &config.City{Agents: []config.Agent{persistentWorker()}}, t.TempDir(), nil, []beads.Bead{snapshot}, nil, nil, nil, nil, nil, nil)
			if len(released) != 0 || len(ledger.updates) != 0 {
				t.Fatalf("released live/unknown holder: %+v, %v", released, ledger.updates)
			}
		})
	}
}

func TestBlockedOrphanReleasesConfirmedClosedHolder(t *testing.T) {
	ledger := newBdGuardLedger("blocked", "closed-session", map[string]string{beadmeta.RoutedToMetadataKey: "repo/worker"})
	run := func(dir, name string, args ...string) ([]byte, error) {
		if name == "bd" && len(args) > 0 && args[0] == "list" {
			return ledger.render()
		}
		if name == "bd" && len(args) > 0 && args[0] == "query" {
			return []byte("[]"), nil
		}
		return ledger.run(dir, name, args...)
	}
	store := beads.NewBdStoreWithPrefix(t.TempDir(), run, "bd")
	snapshot, err := store.Get(ledger.bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	mem := beads.NewMemStore()
	b, err := mem.Create(beads.Bead{ID: "closed-session", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"session_name": "closed-session", "state": "closed"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	released := releaseOrphanedPoolAssignments(store, beads.SessionStore{Store: mem}, &config.City{Agents: []config.Agent{persistentWorker()}}, t.TempDir(), nil, []beads.Bead{snapshot}, nil, nil, nil, nil, nil, nil)
	if len(released) != 1 || ledger.row().Status != "blocked" || ledger.row().Assignee != "" {
		t.Fatalf("closed holder still owns hold: %+v, %+v", released, ledger.row())
	}
}

type blockedSessionReadFailure struct{ beads.Store }

func (s *blockedSessionReadFailure) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("session store unreadable")
}

func TestReleaseListedBlockedOrphanClearsDetachedAtomically(t *testing.T) {
	for _, tc := range []struct {
		name, tmux   string
		errorsBefore int
		change       func(*beads.Bead)
		alive        bool
	}{
		{name: "dead", tmux: "exit 1"},
		{name: "error threshold", tmux: "exit 2", errorsBefore: 2},
		{name: "alive", tmux: "exit 0", alive: true},
		{name: "owner guard lost", tmux: "exit 1", change: func(b *beads.Bead) { b.Assignee = "live-session" }},
		{name: "status guard lost", tmux: "exit 1", change: func(b *beads.Bead) { b.Status = "open" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetDetachedProbeErrorCountsForTest()
			installFakeTmux(t, tc.tmux)
			ledger := newBdGuardLedger("blocked", "dead-session", map[string]string{
				beadmeta.RoutedToMetadataKey: "repo/worker",
				detachedProbeMetadataKey:     testDetachedPoolProbeSpec,
				"hold_reason":                "needs human input", "work_result": "retained",
			})
			ledger.bead.Labels = []string{"hold:external"}
			ledger.bead.Description = "retained description"
			for _, key := range beadmeta.SessionAffinityMetadataKeys {
				ledger.bead.Metadata[key] = "obsolete-session"
			}
			run := func(dir, name string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "list" {
					return ledger.render()
				}
				if len(args) > 0 && args[0] == "query" {
					return []byte("[]"), nil
				}
				return ledger.run(dir, name, args...)
			}
			store := beads.NewCachingStore(beads.NewBdStoreWithPrefix(t.TempDir(), run, "bd"), nil)
			snapshot, err := store.Get(ledger.bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			before := ledger.row()
			ledger.beforeUpdate = tc.change
			for i := 0; i < tc.errorsBefore; i++ {
				incrementDetachedProbeErrorCount(snapshot.ID)
			}
			out := releaseListed([]seatWorkHit{{store: store, bead: snapshot}}, releaseOrphan, releaseSeat{}, &config.City{Agents: []config.Agent{persistentWorker()}}, ReleaseOpts{})
			if tc.alive || tc.change != nil {
				if len(out.Released) != 0 || len(out.Kept) != 1 || len(out.Failed) != 0 {
					t.Fatalf("protected release = %+v", out)
				}
				if !reflect.DeepEqual(ledger.row().Metadata, before.Metadata) {
					t.Fatalf("lost guard changed metadata: %v", ledger.row().Metadata)
				}
				if tc.alive && len(ledger.updates) != 0 {
					t.Fatalf("alive probe reached update: %v", ledger.updates)
				}
				return
			}
			want := before
			want.Assignee = ""
			want.Metadata = make(map[string]string, len(before.Metadata))
			for k, v := range before.Metadata {
				want.Metadata[k] = v
			}
			for _, k := range beadmeta.SessionAffinityMetadataKeys {
				want.Metadata[k] = ""
			}
			want.Metadata[detachedProbeMetadataKey] = ""
			if len(out.Released) != 1 || len(out.Kept) != 0 || len(out.Failed) != 0 || !reflect.DeepEqual(ledger.row(), want) {
				t.Fatalf("release = %+v, row %+v, want %+v", out, ledger.row(), want)
			}
			if len(ledger.updates) != 1 || hasBdFlag(ledger.updates[0], "--status") || testFlagValue(ledger.updates[0], "--if-status") != "blocked" || testFlagValue(ledger.updates[0], "--if-assignee") != "dead-session" {
				t.Fatalf("not one guarded update: %v", ledger.updates)
			}
			cached, err := store.Get(snapshot.ID)
			if err != nil || cached.Assignee != "" || cached.Metadata[detachedProbeMetadataKey] != "" {
				t.Fatalf("cache retained detached assignment: %+v, %v", cached, err)
			}
		})
	}
}

func TestReleaseListedOrphanChecksLiveAssignmentBeforeDetachedProbe(t *testing.T) {
	for _, status := range []string{"open", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			resetDetachedProbeErrorCountsForTest()
			ledger := newBdGuardLedger(status, "dead-session", map[string]string{beadmeta.RoutedToMetadataKey: "repo/worker", detachedProbeMetadataKey: "invalid"})
			run := func(dir, name string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "list" {
					return ledger.render()
				}
				if len(args) > 0 && args[0] == "query" {
					return []byte("[]"), nil
				}
				return ledger.run(dir, name, args...)
			}
			store := beads.NewCachingStore(beads.NewBdStoreWithPrefix(t.TempDir(), run, "bd"), nil)
			snapshot, err := store.Get(ledger.bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			ledger.bead.Assignee = "live-session"
			out := releaseListed([]seatWorkHit{{store: store, bead: snapshot}}, releaseOrphan, releaseSeat{}, &config.City{Agents: []config.Agent{persistentWorker()}}, ReleaseOpts{})
			if len(out.Released) != 0 || len(out.Kept) != 1 || out.Why[snapshot.ID] != keptMoved || len(ledger.updates) != 0 {
				t.Fatalf("stale assignment = %+v, updates %v", out, ledger.updates)
			}
			detachedProbeErrorCounts.Lock()
			count := detachedProbeErrorCounts.byBead[snapshot.ID]
			detachedProbeErrorCounts.Unlock()
			if count != 0 {
				t.Fatalf("stale claim incurred detached probe errors: %d", count)
			}
		})
	}
}

func TestReleaseListedWithoutBlockedProbeAccountsForObservedAssignment(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		apply, readFailure, missing bool
	}{
		{name: "lost fence"},
		{name: "applied", apply: true},
		{name: "read failed", readFailure: true},
		{name: "missing after write", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			b, err := mem.Create(beads.Bead{Status: "open", Assignee: "dead-session", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "repo/worker"}})
			if err != nil {
				t.Fatal(err)
			}
			store := &releaseWithoutBlockedProbe{Store: mem, apply: tc.apply, readFailure: tc.readFailure, missing: tc.missing}
			out := releaseListed([]seatWorkHit{{store: store, bead: b}}, releaseCloseCascade, releaseSeat{}, &config.City{Agents: []config.Agent{persistentWorker()}}, ReleaseOpts{})
			if tc.apply {
				if len(out.Released) != 1 || len(out.Kept) != 0 || len(out.Failed) != 0 {
					t.Fatalf("applied assignment = %+v", out)
				}
			} else if tc.readFailure {
				if len(out.Released) != 0 || len(out.Failed) != 1 {
					t.Fatalf("unverified release = %+v", out)
				}
			} else if len(out.Released) != 0 || len(out.Kept) != 1 || out.Why[b.ID] != keptMoved || len(out.Failed) != 0 {
				t.Fatalf("lost fence counted as released: %+v", out)
			}
			if !tc.apply {
				got, err := mem.Get(b.ID)
				if err != nil || !reflect.DeepEqual(got, b) {
					t.Fatalf("lost fence mutated claim: %+v, %v", got, err)
				}
			}
		})
	}
}

type releaseWithoutBlockedProbe struct {
	beads.Store
	apply, readFailure, missing bool
}

func (s *releaseWithoutBlockedProbe) UpdateIfAssignment(id, status, assignee string, opts beads.UpdateOpts) (bool, error) {
	if !s.apply {
		return false, nil
	}
	b, err := s.Store.Get(id)
	if err != nil || b.Status != status || b.Assignee != assignee {
		return false, err
	}
	err = s.Store.Update(id, opts)
	return err == nil, err
}
func (s *releaseWithoutBlockedProbe) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.readFailure {
		return nil, errors.New("assignment observation failed")
	}
	if s.missing {
		return nil, nil
	}
	return s.Store.List(q)
}

// The normalized census models a provider/cache that exposes parked rows as
// open; the raw census models BdStore's actual --status filter. Both use the
// real SeatWork snapshot and BdStore guarded raw reader/writer.
func drainBlockedEntrypointFixture(t *testing.T, status string, normalizedCensus bool) (*bdGuardLedger, beads.Store, beads.Bead, *SeatWork) {
	t.Helper()
	seat := beads.Bead{ID: "seat-1", Metadata: map[string]string{"template": "repo/worker"}}
	ledger := newBdGuardLedger(status, seat.ID, map[string]string{beadmeta.RoutedToMetadataKey: "repo/worker", "hold_reason": "awaiting input", "defer_until": "2030-01-01"})
	ledger.bead.Labels = []string{"awaiting-input"}
	ledger.bead.Dependencies = []beads.Dep{{DependsOnID: "bd-6", Type: "blocks"}}
	deferred := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	ledger.bead.DeferUntil = &deferred
	run := func(dir, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "query" {
			return []byte("[]"), nil
		}
		if len(args) > 0 && args[0] == "list" {
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			listed := ledger.bead.Status
			if normalizedCensus && listed == "blocked" {
				listed = "open"
			}
			flag := func(key string) string {
				for _, arg := range args {
					if v, ok := strings.CutPrefix(arg, key+"="); ok {
						return v
					}
				}
				return testFlagValue(args, key)
			}
			if requested := flag("--status"); requested != "" && requested != listed {
				return []byte("[]"), nil
			}
			if owner := flag("--assignee"); owner != "" && owner != ledger.bead.Assignee {
				return []byte("[]"), nil
			}
			return ledger.render()
		}
		return ledger.run(dir, name, args...)
	}
	store := wrapWithCachingStore(context.Background(), beads.NewBdStoreWithPrefix(t.TempDir(), run, "bd"), nil, false)
	sw := testSeatWork(t.TempDir(), &config.City{Agents: []config.Agent{persistentWorker()}}, store, nil)
	return ledger, store, seat, sw
}

func TestReleaseClaimsDrainBlockedEntrypoint(t *testing.T) {
	for _, tc := range []struct {
		name, status, started  string
		refuse, expired        bool
		wantReleased, wantKept int
		wantRefused            bool
	}{
		{name: "blocked execution stamp self ack", status: "blocked", started: "stamp", refuse: true, wantReleased: 1},
		{name: "blocked work started self ack", status: "blocked", started: "cell", refuse: true, wantReleased: 1},
		{name: "blocked operator ack", status: "blocked", started: "stamp", wantReleased: 1},
		{name: "unreadable started open preassignment", status: "open", started: "unreadable", refuse: true, wantKept: 1},
		{name: "expired open preassignment", status: "open", expired: true, wantKept: 1},
		{name: "live in progress refuses whole ack", status: "in_progress", started: "stamp", refuse: true, wantKept: 1, wantRefused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger, _, seat, sw := drainBlockedEntrypointFixture(t, tc.status, true)
			switch tc.started {
			case "stamp":
				seat.Metadata[executionClaimStartedKey] = "instance|" + ledger.bead.ID
			case "cell":
				seat.Metadata[session.WorkStartedKey] = session.EncodeWorkStarted(session.WorkStarted{Claims: []session.WorkStartedEntry{{Bead: ledger.bead.ID}}})
			case "unreadable":
				seat.Metadata[session.WorkStartedKey] = "invalid-json"
			}
			if tc.status == "open" {
				ledger.bead.Metadata[beadmeta.RoutedToMetadataKey] = "missing/lane"
			}
			before := ledger.row()
			opts := ReleaseOpts{RefuseStarted: tc.refuse}
			if tc.expired {
				opts.Deadline = time.Now().Add(-time.Hour)
			}
			out, err := ReleaseClaims(sw, releaseScopeOwn(seat), releaseSeatOfBead(seat), releaseDrainAck, opts)
			if err != nil || out.Refused != tc.wantRefused || len(out.Released) != tc.wantReleased || len(out.Kept) != tc.wantKept || len(out.Failed) != 0 {
				t.Fatalf("entrypoint result=%+v err=%v", out, err)
			}
			want := before
			if tc.wantReleased > 0 {
				want.Assignee = ""
				want.Metadata = map[string]string{}
				for k, v := range before.Metadata {
					want.Metadata[k] = v
				}
				for _, key := range beadmeta.SessionAffinityMetadataKeys {
					want.Metadata[key] = ""
				}
				if len(ledger.updates) != 1 || testFlagValue(ledger.updates[0], "--if-status") != "blocked" || hasBdFlag(ledger.updates[0], "--status") {
					t.Fatalf("unguarded blocked write: %v", ledger.updates)
				}
			} else if len(ledger.updates) != 0 {
				t.Fatalf("kept row mutated: %v", ledger.updates)
			}
			if !reflect.DeepEqual(ledger.row(), want) {
				t.Fatalf("raw state=%+v want=%+v", ledger.row(), want)
			}
			if tc.status == "open" && out.Why[ledger.bead.ID] != "preassigned" {
				t.Fatalf("ordinary preassignment misclassified: %+v", out)
			}
		})
	}
}

func TestReleaseDrainAckBlockedEntrypointAndBdReachability(t *testing.T) {
	for _, normalized := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct bd raw filter", true: "normalized census"}[normalized], func(t *testing.T) {
			ledger, store, seat, sw := drainBlockedEntrypointFixture(t, "blocked", normalized)
			if !storeBackedBy[*beads.BdStore](store) {
				t.Fatal("fixture is not BdStore-backed")
			}
			if _, ok := storeLayer[*beads.CachingStore](store); !ok {
				t.Fatal("production cache wrapper missing")
			}
			if !normalized {
				hits, err := sw.snapshot(seatWorkQuery{scope: releaseScopeOwn(seat), statuses: openOrStarted}, false)
				if err != nil || len(hits) != 0 {
					t.Fatalf("raw status-filter baseline=%v %v", hits, err)
				}
			}
			seat.Metadata[executionClaimStartedKey] = "instance|" + ledger.bead.ID
			var stderr bytes.Buffer
			held := releaseDrainAckClaims(sw.Legs(), seat, true, time.Minute, &stderr)
			if len(held) != 0 || strings.Contains(stderr.String(), "ran out") {
				t.Fatalf("ack held=%v log=%s", held, stderr.String())
			}
			wantOwner := ""
			if got := ledger.row(); got.Assignee != wantOwner || got.Status != "blocked" {
				t.Fatalf("provider reachability row=%+v", got)
			}
			wantWrites := 1
			if len(ledger.updates) != wantWrites {
				t.Fatalf("writes=%v", ledger.updates)
			}
		})
	}
}

func TestReleaseClaimsBdCachedDeadHolderEntrypoint(t *testing.T) {
	for _, why := range []releaseWhy{releaseCloseCascade, releaseStrandedRepair, releaseRetired} {
		t.Run(string(rune('a'+why)), func(t *testing.T) {
			ledger, _, seat, sw := drainBlockedEntrypointFixture(t, "blocked", false)
			ledger.bead.Labels = []string{"hold:external"}
			before := ledger.row()
			out, err := ReleaseClaims(sw, releaseScopeOwn(seat), releaseSeatOfBead(seat), why, ReleaseOpts{Late: why == releaseCloseCascade})
			if err != nil || len(out.Released) != 1 || len(out.Kept) != 0 || len(out.Failed) != 0 {
				t.Fatalf("dead holder=%+v error=%v", out, err)
			}
			want := before
			want.Assignee = ""
			want.Metadata = map[string]string{}
			for k, v := range before.Metadata {
				want.Metadata[k] = v
			}
			for _, key := range beadmeta.SessionAffinityMetadataKeys {
				want.Metadata[key] = ""
			}
			if !reflect.DeepEqual(ledger.row(), want) {
				t.Fatalf("blocked/hold/defer/dependency state changed: got=%+v want=%+v", ledger.row(), want)
			}
		})
	}
}
