package main

import (
	"errors"
	"reflect"
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
			backing := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
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
			if got := ledger.row(); !reflect.DeepEqual(got, want) {
				t.Fatalf("raw row = %+v, want %+v", got, want)
			}
			if len(ledger.updates) != 1 || hasBdFlag(ledger.updates[0], "--status") || hasBdFlag(ledger.updates[0], "--set-metadata") {
				t.Fatalf("not assignee-only: %v", ledger.updates)
			}
			if got := testFlagValue(ledger.updates[0], "--if-status"); got != "blocked" {
				t.Fatalf("raw status guard = %q", got)
			}
		})
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
			ledger := newBdGuardLedger("blocked", "dead-session", map[string]string{beadmeta.RoutedToMetadataKey: "repo/worker"})
			store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
			snapshot, err := store.Get(ledger.bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			ledger.beforeUpdate = tc.change
			ledger.noGuards = tc.unsupported
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

// An open-row read consumes the remaining budget without sleeping. Started
// claims must already be released, in their original relative order.
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
	if got, want := visits, []string{"bd-started1", "bd-started2", "bd-open1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("store reads = %v, want %v", got, want)
	}
	if len(out.Released) != 2 || out.Released[0].bead.ID != "bd-started1" || out.Released[1].bead.ID != "bd-started2" || len(out.Kept) != 1 || len(out.Failed) != 1 || out.Why["bd-open2"] != failedBudget {
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
