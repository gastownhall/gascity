package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// closeHookStore is a MemStore with an atomic terminal close that runs hook
// before each close attempt: the moment between a close's read and its
// revision-fenced write, where a concurrent reconciler tick lands.
type closeHookStore struct {
	*beads.MemStore
	closer   beads.AtomicConditionalCloser
	hook     func(attempt int)
	attempts int
}

func (s *closeHookStore) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (beads.Bead, error) {
	s.attempts++
	if s.hook != nil {
		s.hook(s.attempts)
	}
	return s.closer.CloseWithMetadataIfMatch(id, expectedRevision, metadata)
}

// TestOperatorCloseIsNoCrashToTheExitAccrual is ga-u2f94s: an operator's
// close stops the runtime under its runtime lease, then commits the close
// fenced on the row's revision. A reconciler tick that observes the runtime
// dead in between read a rapid crash (the wake is seconds old) and accrued
// it in three writes: the conversation reset, wake_attempts, and the
// last_woke_at clear. Each write moved the revision under the close, so on a
// slow host the close lost its fence three times and the API answered 500
// "lost the revision fence 3 times", leaving the row open with its
// conversation reset. A runtime a live holder's lease covers is that
// holder's to stop: the accrual writes nothing, and the close lands on its
// first fence.
func TestOperatorCloseIsNoCrashToTheExitAccrual(t *testing.T) {
	for _, c := range []struct {
		name    string
		wokeAgo time.Duration
	}{
		{"a close inside the stability window", 10 * time.Second},
		{"a close in the churn band", time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			mem := beads.NewMemStore()
			store := &closeHookStore{MemStore: mem, closer: beads.AtomicCloserOver(mem)}
			env.store = store
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			env.addDesired("worker", "worker", true)
			row := env.createSessionBead("worker", "worker")
			env.setSessionMetadata(&row, map[string]string{
				"state": "active", "session_key": "conv-1", "started_config_hash": "hash-1",
				"last_woke_at": env.clk.Now().Add(-c.wokeAgo).UTC().Format(time.RFC3339),
			})
			snapshot := mustGetBead(t, env.store, row.ID) // the tick's read

			var accrued map[string]string
			store.hook = func(attempt int) {
				if attempt != 1 {
					return
				}
				env.reconcile([]beads.Bead{snapshot})
				accrued = mustGetBead(t, mem, row.ID).Metadata
			}
			mgr := sessionpkg.NewManagerWithOptions(env.store, env.sp, sessionpkg.WithCityPath(env.city))
			if err := mgr.Close(context.Background(), testOperatorIn(t, env.city), row.ID); err != nil {
				t.Fatalf("operator close: %v", err)
			}
			if accrued == nil {
				t.Fatal("no reconciler tick ran between the close's stop and its commit")
			}
			if accrued["wake_attempts"] != "" || accrued["churn_count"] != "" || accrued["session_key"] != "conv-1" || accrued["last_woke_at"] == "" {
				t.Fatalf("row after a tick inside an operator's close = %v, want no wake failure or churn accrued: the dead runtime is the close's stop", accrued)
			}
			if store.attempts != 1 {
				t.Fatalf("close fence attempts = %d, want 1: the tick wrote under the close", store.attempts)
			}
			if got := mustGetBead(t, mem, row.ID); got.Status != "closed" {
				t.Fatalf("row status = %q, want closed", got.Status)
			}
		})
	}
}
