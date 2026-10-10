package session

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestDecidedSitesCompareProjectedKeys: a Decided compares a row as Info
// projects it, so every key a legacy or operator site compares must be one
// the Info codec projects; a key it does not would be silently uncompared.
// NoWake is a Decided site's rule only.
func TestDecidedSitesCompareProjectedKeys(t *testing.T) {
	for site, e := range factsSites {
		decided := strings.HasPrefix(string(site), "legacy.") || strings.HasPrefix(string(site), "op.")
		if !decided {
			if e.NoWake {
				t.Errorf("%s: NoWake on a site no Decided uses", site)
			}
			continue
		}
		if len(e.Only) == 0 {
			t.Errorf("%s: a Decided site lists the keys it compares (Only)", site)
		}
		for _, k := range e.Only {
			if !containsKey(factKeys, k) {
				t.Errorf("%s: compares %q, which Facts does not hold", site, k)
			}
			if len(infoKeyFields[k]) == 0 {
				t.Errorf("%s: compares %q, which Info does not project; add it to the codec", site, k)
			}
		}
		if e.Reason == "" {
			t.Errorf("%s: no reason", site)
		}
	}
}

func containsKey(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

func decidedRow(id string, meta map[string]string) Info {
	return infoFromPersistedBead(beads.Bead{ID: id, Status: "open", Type: BeadType, Metadata: meta})
}

// TestDecidedMatch is each site's row as a table: a key it compares moving
// refuses; a key it does not compare moving is the same decision.
func TestDecidedMatch(t *testing.T) {
	base := map[string]string{
		"generation": "2", "instance_token": "tok", "state": string(StateActive), "session_name": "s",
		"continuation_reset_pending": "true", "reset_committed_at": "2026-01-01T00:00:00Z",
	}
	stopPending := map[string]string{"state": string(StateDraining), "state_reason": DrainAckStopPendingReason}
	for _, c := range []struct {
		site      FactsSite
		decided   map[string]string // over base
		move      map[string]string
		closeRow  bool
		otherID   bool
		wantMatch bool
	}{
		{site: FactsLegacyKill, wantMatch: true},
		{site: FactsLegacyKill, closeRow: true},
		{site: FactsLegacyKill, otherID: true},
		{site: FactsLegacyKill, move: map[string]string{"generation": "3"}},
		{site: FactsLegacyKill, move: map[string]string{"instance_token": "tok-2"}},
		{site: FactsLegacyKill, move: map[string]string{"held_until": "2099-01-01T00:00:00Z"}},
		{site: FactsLegacyKill, move: map[string]string{"sleep_intent": "user-hold"}},
		// SameKillFacts' exact set: no widening in R8a.
		{site: FactsLegacyKill, move: map[string]string{"wait_hold": "true"}, wantMatch: true},
		{site: FactsLegacyKill, move: map[string]string{"suspended_at": "x"}, wantMatch: true},
		{site: FactsLegacyKill, decided: map[string]string{"state": string(StateAsleep)}, move: map[string]string{"state": string(StateActive)}},
		{site: FactsLegacyKill, decided: map[string]string{"state": string(StateAsleep)}, move: map[string]string{"state": string(StateSuspended)}, wantMatch: true},
		{site: FactsLegacyKill, move: map[string]string{"state": string(StateDraining)}, wantMatch: true},

		{site: FactsLegacyStopPending, wantMatch: true},
		{site: FactsLegacyStopPending, closeRow: true},
		{site: FactsLegacyStopPending, move: map[string]string{"generation": "3"}},
		{site: FactsLegacyStopPending, move: map[string]string{"instance_token": "tok-2"}},
		{site: FactsLegacyStopPending, decided: map[string]string{"held_until": "2099-01-01T00:00:00Z", "sleep_intent": "user-hold"}, move: map[string]string{"held_until": "", "sleep_intent": ""}},
		{site: FactsLegacyStopPending, move: map[string]string{"wait_hold": "true"}},
		{site: FactsLegacyStopPending, move: map[string]string{"quarantined_until": "2099-01-01T00:00:00Z"}},
		{site: FactsLegacyStopPending, move: map[string]string{"suspended_at": "2026-01-01T00:00:00Z"}},
		{site: FactsLegacyStopPending, move: map[string]string{"sleep_reason": "user-hold"}},
		{site: FactsLegacyStopPending, move: map[string]string{"wake_request": "1"}},
		{site: FactsLegacyStopPending, move: map[string]string{"restart_requested": "true"}},
		// The drain's own writes, and what the controller keeps while it drains.
		{site: FactsLegacyStopPending, move: map[string]string{"state": string(StateDraining), "state_reason": DrainAckStopPendingReason, "drain_at": "x"}, wantMatch: true},
		{site: FactsLegacyStopPending, move: map[string]string{"idle_respawn_attempts": "1", "idle_respawn_bead_id": "w-1"}, wantMatch: true},
		{site: FactsLegacyStopPending, move: map[string]string{"detached_at": "2026-01-01T00:00:00Z"}, wantMatch: true},
		{site: FactsLegacyStopPending, move: map[string]string{"continuation_reset_pending": ""}, wantMatch: true},

		{site: FactsLegacyDrainStop, decided: stopPending, move: stopPending, wantMatch: true},
		{site: FactsLegacyDrainStop, decided: stopPending, move: map[string]string{"state": string(StateActive)}},
		{site: FactsLegacyDrainStop, decided: stopPending, move: map[string]string{"state": string(StateDraining), "state_reason": "idle"}},
		{site: FactsLegacyDrainStop, decided: stopPending, move: map[string]string{"state": string(StateDraining), "state_reason": DrainAckStopPendingReason, "sleep_intent": "user-hold"}},
		{site: FactsLegacyDrainStop, decided: stopPending, move: map[string]string{"state": string(StateDraining), "state_reason": DrainAckStopPendingReason, "drain_at": "y"}, wantMatch: true},

		{site: FactsLegacyResetEvict, wantMatch: true},
		{site: FactsLegacyResetEvict, move: map[string]string{"continuation_reset_pending": ""}},
		{site: FactsLegacyResetEvict, move: map[string]string{"reset_committed_at": "2026-02-01T00:00:00Z"}},
		{site: FactsLegacyResetEvict, move: map[string]string{"generation": "3"}, wantMatch: true},
	} {
		decidedMeta, freshMeta := map[string]string{}, map[string]string{}
		for k, v := range base {
			decidedMeta[k], freshMeta[k] = v, v
		}
		for k, v := range c.decided {
			decidedMeta[k], freshMeta[k] = v, v
		}
		for k, v := range c.move {
			freshMeta[k] = v
		}
		freshID := "s-1"
		if c.otherID {
			freshID = "s-2"
		}
		fresh := decidedRow(freshID, freshMeta)
		if c.closeRow {
			fresh = fresh.MarkClosed()
		}
		if got := Decide(decidedRow("s-1", decidedMeta), c.site).Match(fresh); got != c.wantMatch {
			t.Errorf("%s decided %v, moved %v (closed %v, other row %v): Match = %v, want %v", c.site, c.decided, c.move, c.closeRow, c.otherID, got, c.wantMatch)
		}
	}
	if (Decided{}).Match(decidedRow("", nil)) {
		t.Error("a zero Decided matched a row")
	}
}

// TestCommitRefusesAStaleStopPendingOverAConsumedHold is SR3-1 at the store:
// a drain begun on a held row marks stop-pending only while the hold stands.
// An operator's resume consumed it after the decision's read, so the mark
// writes nothing, on every store kind.
func TestCommitRefusesAStaleStopPendingOverAConsumedHold(t *testing.T) {
	hold := map[string]string{"held_until": "2099-01-01T00:00:00Z", "sleep_intent": "user-hold"}
	for _, backend := range patchFenceBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created := seedPatchFenceSession(t, store, "s-hold")
			if err := store.SetMetadataBatch(created.ID, hold); err != nil {
				t.Fatal(err)
			}
			front := NewStore(beads.SessionStore{Store: store})
			snapshot, err := front.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			d := Decide(snapshot, FactsLegacyStopPending)
			if err := backing.SetMetadataBatch(created.ID, map[string]string{"held_until": "", "sleep_intent": ""}); err != nil {
				t.Fatal(err)
			}
			res, err := front.Commit(d, DrainAckStopPendingPatch(time.Unix(0, 0)))
			if err != nil {
				t.Fatalf("Commit: %v", err)
			}
			if res == CommitLanded {
				t.Fatal("the stale stop-pending mark landed over a consumed hold")
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Metadata["state"] != string(StateActive) || got.Metadata["state_reason"] != "" {
				t.Fatalf("row = state %q reason %q, want the resumed row untouched", got.Metadata["state"], got.Metadata["state_reason"])
			}
		})
	}
}

// TestCommitResults: the decision's row unmoved lands; moved, closed and a
// fence lost on every attempt refuse with their own result.
func TestCommitResults(t *testing.T) {
	patch := MetadataPatch{"drain_at": "x"}
	for _, backend := range patchFenceBackends() {
		if backend.staleCache {
			continue // a stale cached revision refuses once; covered by the lifecycle fence tests
		}
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			front := NewStore(beads.SessionStore{Store: store})
			row := seedPatchFenceSession(t, store, "s-commit")
			snapshot, err := front.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			d := Decide(snapshot, FactsLegacyStopPending)
			if res, err := front.Commit(d, patch); err != nil || res != CommitLanded {
				t.Fatalf("unmoved: Commit = %v, %v; want landed", res, err)
			}
			if err := backing.SetMetadata(row.ID, "generation", "9"); err != nil {
				t.Fatal(err)
			}
			if res, err := front.Commit(d, patch); err != nil || res != CommitMoved {
				t.Fatalf("moved: Commit = %v, %v; want moved", res, err)
			}
			if _, err := front.Commit(Decided{}, patch); err == nil {
				t.Fatal("a zero Decided committed")
			}
			if err := backing.Close(row.ID); err != nil {
				t.Fatal(err)
			}
			if res, err := front.Commit(d, patch); err != nil || res != CommitClosed {
				t.Fatalf("closed: Commit = %v, %v; want closed", res, err)
			}
			if !backend.fenced {
				return
			}
			other := seedPatchFenceSession(t, store, "s-contended")
			snapshot, err = front.Get(other.ID)
			if err != nil {
				t.Fatal(err)
			}
			racing := &everyGetStore{Store: store, afterGet: func(id string) {
				_ = backing.SetMetadata(id, "last_nudge_delivered_at", time.Now().String())
			}}
			res, err := NewStore(beads.SessionStore{Store: racing}).Commit(Decide(snapshot, FactsLegacyStopPending), patch)
			if err != nil || res != CommitContended {
				t.Fatalf("contended: Commit = %v, %v; want contended", res, err)
			}
		})
	}
}
