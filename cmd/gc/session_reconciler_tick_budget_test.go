package main

import (
	"context"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// getCountingStore counts store.Get calls, delegating everything else. It guards
// the WI-5 tick-budget invariant: bd ops cost ~2s under Dolt, and the
// write-returns-Info cutover (ApplyPatchInfo) must add ZERO Gets — a patch write
// plus a LOCAL fold, never a re-Get. A future "convenient re-Get" on the
// reconciler fast path bumps this count and fails CI.
type getCountingStore struct {
	beads.Store
	mu   sync.Mutex
	gets int
}

func (s *getCountingStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.Store.Get(id)
}

func (s *getCountingStore) reset() {
	s.mu.Lock()
	s.gets = 0
	s.mu.Unlock()
}

func (s *getCountingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

// TestReconcileSessionBeadsFastPathGetBudget pins the number of store.Get calls a
// single healthy reconcile tick issues, so the ApplyPatchInfo write-returns-Info
// cutover (WI-5 W1) provably adds none. The forward pass builds its infoByID
// snapshot from the input slice (InfoFromPersistedBead, no Get) and folds every
// mutation locally via ApplyPatch/ApplyPatchInfo/MarkClosed (SetMetadataBatch +
// local fold, no Get); the only tick-body store.Get calls are the rare NDI
// witness close (finalizeDrainAckStoppedSession), which a healthy running
// session never hits, and the one re-read an advisory status heal makes before
// it writes (session.Store.ApplyPatchIfLifecycleUnchanged). That re-read is the
// lost-update fence: the heal is decided from the tick snapshot, and must not
// overwrite a suspend, kill, or close written after the snapshot was read. It
// runs only on a tick that actually heals, and in production the city store is
// a CachingStore, so it is normally a cache hit. The expected counts are
// therefore fixed and small — a ratchet against a regressive re-Get sneaking
// onto the hot path.
func TestReconcileSessionBeadsFastPathGetBudget(t *testing.T) {
	cases := []struct {
		name     string
		state    string
		wantGets int
	}{
		// A converged session: nothing to heal, so the tick issues no Get.
		{name: "converged", state: "awake", wantGets: 0},
		// state=active heals to awake: exactly the heal's fenced re-read.
		{name: "heal", state: "active", wantGets: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, session, sessionName := newProgressStallTestEnv(t)
			env.setSessionMetadata(&session, map[string]string{"state": tc.state})
			// Recent activity so the healthy running session is neither
			// progress-stalled nor idle-killed — a clean steady-state
			// fast-path tick.
			env.sp.SetActivity(sessionName, env.clk.Now())

			counting := &getCountingStore{Store: env.store}
			cfgNames := configuredSessionNames(env.cfg, "", counting)
			poolDesired := map[string]int{"worker": 1}

			// Count only the reconcile tick itself, not the harness setup above.
			counting.reset()
			reconcileSessionBeads(
				context.Background(),
				[]beads.Bead{session},
				env.desiredState,
				cfgNames,
				env.cfg,
				env.sp,
				counting,
				nil,
				nil,
				nil,
				env.dt,
				poolDesired,
				false,
				nil,
				"",
				nil,
				env.clk,
				env.rec,
				0,
				0,
				&env.stdout,
				&env.stderr,
			)

			// If a future change reintroduces a re-Get on this path, this
			// fails — deliberately, per the WI-5 tick budget.
			if got := counting.count(); got != tc.wantGets {
				t.Fatalf("healthy reconcile tick issued %d store.Get calls, want %d — a re-Get crept onto the reconciler fast path (WI-5 tick budget: write + local fold, never a re-Get). stdout=%q stderr=%q", got, tc.wantGets, env.stdout.String(), env.stderr.String())
			}
			stored, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if stored.Metadata["state"] != "awake" {
				t.Fatalf("state = %q after the tick, want awake", stored.Metadata["state"])
			}
		})
	}
}
