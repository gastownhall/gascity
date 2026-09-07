package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func selectedStartBeforeSuspend(t *testing.T) (*reconcilerTestEnv, startCandidate, *sessionpkg.Manager) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.addDesired("worker", "worker", false)
	b := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&b, map[string]string{
		"state": "active", "session_key": "retained-conversation", "started_config_hash": "prior-launch",
		"last_woke_at": "2026-03-08T11:59:00Z", "session_origin": "manual",
	})
	candidate := startCandidate{info: env.sessionInfo(b.ID), tp: env.desiredState["worker"]}
	return env, candidate, sessionpkg.NewManagerWithOptions(env.store, env.sp)
}

func TestSelectedStartCandidateCannotUndoLaterSuspend(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "cached"}[cached], func(t *testing.T) {
			env, candidate, mgr := selectedStartBeforeSuspend(t)
			counted := &countingWakeMetadataStore{MemStore: env.store.(*beads.MemStore)}
			var store beads.Store = counted
			if cached {
				cache := beads.NewCachingStore(counted, nil)
				if err := cache.Prime(context.Background()); err != nil {
					t.Fatal(err)
				}
				store = cache
			}
			if err := mgr.Suspend(candidate.info.ID); err != nil {
				t.Fatal(err)
			}
			before, err := env.store.Get(candidate.info.ID)
			if err != nil {
				t.Fatal(err)
			}
			woken := executePlannedStarts(context.Background(), []startCandidate{candidate}, env.cfg,
				env.desiredState, env.sp, store, "", env.clk, env.rec, 0,
				&env.stdout, &env.stderr, env.startOptions...)
			after, err := env.store.Get(candidate.info.ID)
			if err != nil {
				t.Fatal(err)
			}
			if woken != 0 || env.sp.CountCalls("Start", "worker") != 0 {
				t.Errorf("stale wake selection launched after Suspend: woken=%d", woken)
			}
			if !reflect.DeepEqual(before.Metadata, after.Metadata) {
				t.Errorf("stale wake selection changed suspended metadata: before=%v after=%v", before.Metadata, after.Metadata)
			}
			if counted.singleCalls != 0 || counted.batchCalls != 0 {
				t.Errorf("deferred selection performed writes after rejecting newer intent: single=%d batch=%d", counted.singleCalls, counted.batchCalls)
			}
		})
	}
}

func TestSelectedStartCandidateAllowsNewExplicitWakeAfterSuspend(t *testing.T) {
	env, candidate, mgr := selectedStartBeforeSuspend(t)
	if err := mgr.Suspend(candidate.info.ID); err != nil {
		t.Fatal(err)
	}
	front := sessionFrontDoor(env.store)
	if _, err := front.WakeSession(candidate.info.ID, env.clk.Now(), sessionpkg.WakeOpts{}); err != nil {
		t.Fatal(err)
	}
	current := env.sessionInfo(candidate.info.ID)
	if current.SleepReason != "" || current.WakeRequest != string(sessionpkg.WakeCauseExplicit) {
		t.Fatal("new explicit wake did not clear the suspended hold")
	}
	// The older selection may satisfy the newer explicit demand once the live
	// read proves the hold was deliberately released; no request is discarded.
	woken := executePlannedStarts(context.Background(), []startCandidate{candidate}, env.cfg,
		env.desiredState, env.sp, env.store, "", env.clk, env.rec, 0,
		&env.stdout, &env.stderr, env.startOptions...)
	if woken != 1 || env.sp.CountCalls("Start", "worker") != 1 {
		t.Fatalf("newer explicit wake was blocked: woken=%d stderr=%s", woken, env.stderr.String())
	}
}
