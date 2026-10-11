package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestExitAccrualDecidesOnAFreshRead is mc-uhvqj (J32, J35 D): a tick reads
// its snapshot, then an operator's `gc session kill` lands (its fence clears
// last_woke_at and records sleep_reason=killed) and stops the runtime, and
// then the tick observes the runtime dead. The snapshot still names the
// killed incarnation's wake, so it reads a rapid crash or churn. Accruing it
// would count the operator's kill against the session: the exit accrual
// decides again on a fresh read of the row and refuses once its facts moved
// (CONTRACT §12.5, destructive decisions on a fresh read). A death no
// operator caused still accrues.
func TestExitAccrualDecidesOnAFreshRead(t *testing.T) {
	for _, c := range []struct {
		name    string
		wokeAgo time.Duration
		killed  bool
		counter string // the counter a death of that age accrues
	}{
		{"a kill inside the stability window", 10 * time.Second, true, "wake_attempts"},
		{"a kill in the churn band", time.Minute, true, "churn_count"},
		{"a crash inside the stability window", 10 * time.Second, false, "wake_attempts"},
		{"a crash in the churn band", time.Minute, false, "churn_count"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			env.addDesired("worker", "worker", true)
			row := env.createSessionBead("worker", "worker")
			env.setSessionMetadata(&row, map[string]string{
				"state": "active", "session_origin": "manual", "session_key": "conv-1", "started_config_hash": "hash-1",
				"last_woke_at": env.clk.Now().Add(-c.wokeAgo).UTC().Format(time.RFC3339),
			})
			snapshot := mustGetBead(t, env.store, row.ID) // the tick's read

			if c.killed {
				fence, err := writeSessionKillFence(env.store, row.ID, env.clk.Now())
				if err != nil {
					t.Fatal(err)
				}
				mgr := sessionpkg.NewManagerWithOptions(env.store, env.sp, sessionpkg.WithCityPath(env.city))
				if err := mgr.Kill(row.ID); err != nil {
					t.Fatal(err)
				}
				if err := fence.clear(env.store); err != nil {
					t.Fatal(err)
				}
			} else if err := env.sp.Stop("worker"); err != nil {
				t.Fatal(err)
			}

			env.reconcile([]beads.Bead{snapshot})
			meta := mustGetBead(t, env.store, row.ID).Metadata
			if !c.killed {
				if meta[c.counter] != "1" {
					t.Fatalf("%s = %q after a crash, want 1: a death no operator caused still accrues\nrow %v", c.counter, meta[c.counter], meta)
				}
				return
			}
			if meta["wake_attempts"] != "" || meta["churn_count"] != "" || meta["session_key"] != "conv-1" || meta["continuation_reset_pending"] != "" {
				t.Fatalf("row after an operator kill = %v, want no wake failure or churn accrued and the conversation kept", meta)
			}
		})
	}
}

// unreadableRowStore fails every Get: the fresh read an accrual decides on.
type unreadableRowStore struct{ beads.Store }

func (unreadableRowStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("store unreachable")
}

// TestExitAccrualAccruesNothingOnAFailedFreshRead: an accrual whose fresh
// read fails is not decided, so nothing is written; the next tick decides
// again (mc-uhvqj).
func TestExitAccrualAccruesNothingOnAFailedFreshRead(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	for _, c := range []struct {
		name    string
		wokeAgo time.Duration
		check   func(sessionpkg.Info, *sessionpkg.Store) bool
	}{
		{"rapid crash", 10 * time.Second, func(i sessionpkg.Info, f *sessionpkg.Store) bool {
			_, stab := checkStability(i, nil, false, newDrainTracker(), f, clk, nil)
			return stab
		}},
		{"churn", time.Minute, func(i sessionpkg.Info, f *sessionpkg.Store) bool {
			_, churn := checkChurn(i, nil, false, newDrainTracker(), f, clk)
			return churn
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			row, err := store.Create(beads.Bead{Title: "w", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
				"session_name": "w-" + c.name, "state": "active", "generation": "1", "instance_token": "tok",
				"last_woke_at": clk.Now().Add(-c.wokeAgo).UTC().Format(time.RFC3339),
			}})
			if err != nil {
				t.Fatal(err)
			}
			c.check(seedSessionInfo(row), sessionFrontDoor(unreadableRowStore{store}))
			if meta := mustGetBead(t, store, row.ID).Metadata; meta["wake_attempts"] != "" || meta["churn_count"] != "" || meta["last_woke_at"] == "" {
				t.Fatalf("row after an accrual whose fresh read failed = %v, want nothing written", meta)
			}
		})
	}
}
