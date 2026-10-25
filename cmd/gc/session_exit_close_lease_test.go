package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
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

// TestExitAccrualCountsACrashOnceAnUnrelatedHolderReleases: a holder that did
// not stop the runtime (a stand-in for a start or a stop of the same name)
// defers the accrual, which writes nothing while it holds the lease. The
// crash is still a crash: the first tick after the release counts it.
func TestExitAccrualCountsACrashOnceAnUnrelatedHolderReleases(t *testing.T) {
	for _, c := range []struct {
		name    string
		wokeAgo time.Duration
		counter string
	}{
		{"a crash inside the stability window", 10 * time.Second, "wake_attempts"},
		{"a crash in the churn band", time.Minute, "churn_count"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			env.addDesired("worker", "worker", true)
			row := env.createSessionBead("worker", "worker")
			env.setSessionMetadata(&row, map[string]string{
				"state": "active", "session_key": "conv-1", "started_config_hash": "hash-1",
				"last_woke_at": env.clk.Now().Add(-c.wokeAgo).UTC().Format(time.RFC3339),
			})
			if err := env.sp.Stop("worker"); err != nil {
				t.Fatal(err)
			}

			release := holdName(t, env.city, "worker")
			env.reconcile([]beads.Bead{mustGetBead(t, env.store, row.ID)})
			if meta := mustGetBead(t, env.store, row.ID).Metadata; meta[c.counter] != "" || meta["last_woke_at"] == "" {
				t.Fatalf("row under a held lease = %v, want no accrual while the holder has the runtime", meta)
			}
			release()

			env.reconcile([]beads.Bead{mustGetBead(t, env.store, row.ID)})
			if meta := mustGetBead(t, env.store, row.ID).Metadata; meta[c.counter] != "1" {
				t.Fatalf("%s = %q on the first tick after the release, want 1: a crash no holder caused is counted\nrow %v", c.counter, meta[c.counter], meta)
			}
		})
	}
}

// leaseWatchStore records, for each metadata write of an exit accrual, whether
// the runtime name's lease was held when it landed. An accrual write carries
// a counter, the conversation reset or the last_woke_at clear, and never a
// state: the dead-runtime heal's batch, which moves state, is not one.
type leaseWatchStore struct {
	*beads.MemStore
	t        *testing.T
	city     string
	name     string
	unlocked []string
	writes   int
}

func (s *leaseWatchStore) watch(keys map[string]string) {
	if _, heal := keys["state"]; heal {
		return
	}
	for _, k := range []string{"wake_attempts", "churn_count", "last_woke_at", "continuation_reset_pending"} {
		if _, ok := keys[k]; ok {
			s.writes++
			if !nameHeld(s.t, s.city, s.name) {
				s.unlocked = append(s.unlocked, k)
			}
			return
		}
	}
}

func (s *leaseWatchStore) SetMetadata(id, key, value string) error {
	s.watch(map[string]string{key: value})
	return s.MemStore.SetMetadata(id, key, value)
}

func (s *leaseWatchStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.watch(kvs)
	return s.MemStore.SetMetadataBatch(id, kvs)
}

// TestExitAccrualWritesUnderTheRuntimeLease: every write of a rapid-crash or
// churn accrual lands under the runtime name's lease, so an operator's close
// cannot take the lease, stop the runtime and read the row between the
// accrual's decision and its last write; the close waits for the lease
// instead of losing its fence to the accrual (ga-u2f94s review).
func TestExitAccrualWritesUnderTheRuntimeLease(t *testing.T) {
	for _, c := range []struct {
		name    string
		wokeAgo time.Duration
	}{
		{"a rapid crash", 10 * time.Second},
		{"churn", time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			store := &leaseWatchStore{MemStore: beads.NewMemStore(), t: t, city: env.city, name: "worker"}
			env.store = store
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			env.addDesired("worker", "worker", true)
			row := env.createSessionBead("worker", "worker")
			env.setSessionMetadata(&row, map[string]string{
				"state": "active", "session_key": "conv-1", "started_config_hash": "hash-1",
				"last_woke_at": env.clk.Now().Add(-c.wokeAgo).UTC().Format(time.RFC3339),
			})
			if err := env.sp.Stop("worker"); err != nil {
				t.Fatal(err)
			}
			store.writes, store.unlocked = 0, nil
			env.reconcile([]beads.Bead{mustGetBead(t, env.store, row.ID)})
			if store.writes == 0 {
				t.Fatal("the tick accrued nothing; the test needs an accrual to watch")
			}
			if len(store.unlocked) > 0 {
				t.Fatalf("accrual writes %v landed without the runtime lease", store.unlocked)
			}
			nameFree(t, env.city, "worker")
		})
	}
}

// TestOperatorCloseIsNoCrashToTheV2DeadRuntimeHeal is ga-u2f94s for the v2
// reconciler, on one host: a v2 pass that reads the runtime gone between the
// close's stop and its fenced commit decides the dead-runtime heal (a rapid
// crash's accrual), but the heal's effect takes the runtime name's lock, which
// the close holds, so it refuses as name-busy and writes nothing. The close
// lands on its first fence. (Across hosts the v2 effect holds only its own
// host's flock; that gap is tracked separately.)
func TestOperatorCloseIsNoCrashToTheV2DeadRuntimeHeal(t *testing.T) {
	woke := gatherNow.Add(-10 * time.Second).Format(time.RFC3339)
	c := newHealCase(t, livenessGone, desireWake, "state", "active", "session_key", "k-1", "started_config_hash", "h", "last_woke_at", woke)
	if it := c.decide(); !effectSpecs[it.Kind].runs() {
		t.Fatalf("decideRow = (%q, %q), want the dead-runtime heal", it.Kind, it.Reason)
	}
	store := &closeHookStore{MemStore: c.store, closer: beads.AtomicCloserOver(c.store)}
	var s settlement
	ran := false
	store.hook = func(attempt int) {
		if attempt != 1 {
			return
		}
		ran = true
		_, s = c.run(t, gone().tmux(), nil)
	}
	mgr := sessionpkg.NewManagerWithOptions(store, runtime.NewFake(), sessionpkg.WithCityPath(c.w.CityPath))
	if err := mgr.Close(context.Background(), testOperatorIn(t, c.w.CityPath), c.k.ID); err != nil {
		t.Fatalf("operator close: %v", err)
	}
	if !ran {
		t.Fatal("no v2 pass ran between the close's stop and its commit")
	}
	if s.Outcome != settledRefused || s.Cause != causeNameBusy {
		t.Fatalf("v2 heal settlement %+v inside the close, want refused %q", s, causeNameBusy)
	}
	if store.attempts != 1 {
		t.Fatalf("close fence attempts = %d, want 1: the v2 heal wrote under the close", store.attempts)
	}
	got := mustGetBead(t, c.store, c.k.ID)
	if got.Status != "closed" || got.Metadata["wake_attempts"] != "" {
		t.Fatalf("row status %q wake_attempts %q, want closed with no accrual", got.Status, got.Metadata["wake_attempts"])
	}
}
