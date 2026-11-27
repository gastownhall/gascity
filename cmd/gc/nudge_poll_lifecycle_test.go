package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func TestCmdNudgePollLifecycleRetiresDormantSession(t *testing.T) {
	for _, state := range []session.State{session.StateAsleep, session.StateSuspended, session.StateDrained, session.StateArchived} {
		t.Run(string(state), func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			markNudgePollCityRunningForTest(t)
			t.Setenv("GC_BEADS", "file")
			dir := t.TempDir()
			writeNamedSessionCityTOML(t, dir)
			t.Setenv("GC_CITY", dir)
			store, err := openCityStoreAt(dir)
			if err != nil {
				t.Fatal(err)
			}
			b, err := store.Create(beads.Bead{
				Title: "Session: worker", Type: session.BeadType,
				Status: "open", Labels: []string{session.LabelSession},
				Metadata: map[string]string{
					"session_name": "worker-session", "agent_name": "worker",
					"template": "worker", "state": string(state),
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			original := nudgeObserveTarget
			calls := 0
			nudgeObserveTarget = func(nudgeTarget, beads.Store, runtime.Provider) (worker.LiveObservation, error) {
				calls++
				if calls > 1 {
					return worker.LiveObservation{Running: false}, nil
				}
				return worker.LiveObservation{Running: true, LifecycleState: state}, nil
			}
			t.Cleanup(func() { nudgeObserveTarget = original })
			var out, diag bytes.Buffer
			code := cmdNudgePoll([]string{b.ID}, "worker-session", time.Millisecond, 0, true, &out, &diag)
			if code != 0 {
				t.Fatalf("poll returned %d; stderr=%s", code, diag.String())
			}
			if calls != 1 {
				t.Fatalf("dormant session observed %d times (want once, poll must exit, no churn)", calls)
			}
		})
	}
}

func TestCmdNudgePollLifecycleKeepsActiveWorker(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	markNudgePollCityRunningForTest(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	writeNamedSessionCityTOML(t, dir)
	t.Setenv("GC_CITY", dir)
	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Create(beads.Bead{
		Title: "Session: worker", Type: session.BeadType,
		Status: "open", Labels: []string{session.LabelSession},
		Metadata: map[string]string{"session_name": "worker-session", "agent_name": "worker", "template": "worker", "state": string(session.StateActive)},
	})
	if err != nil {
		t.Fatal(err)
	}
	original := nudgeObserveTarget
	calls := 0
	nudgeObserveTarget = func(nudgeTarget, beads.Store, runtime.Provider) (worker.LiveObservation, error) {
		calls++
		if calls == 1 {
			return worker.LiveObservation{Running: true, LifecycleState: session.StateActive}, nil
		}
		return worker.LiveObservation{Running: false}, nil
	}
	t.Cleanup(func() { nudgeObserveTarget = original })
	var out, diag bytes.Buffer
	code := cmdNudgePoll([]string{b.ID}, "worker-session", time.Millisecond, 0, true, &out, &diag)
	if code != 0 {
		t.Fatalf("poll %d: %s", code, diag.String())
	}
	if calls != 2 {
		t.Fatalf("active worker observations=%d, want two (keep polling until runtime stops)", calls)
	}
}
