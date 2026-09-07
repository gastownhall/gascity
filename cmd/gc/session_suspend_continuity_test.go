package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Exercise the Manager-to-controller boundary: merely retaining a key when
// Suspend returns is insufficient if the next controller tick discards it.
func TestSuspendPreservesConversationThroughExitReconciliation(t *testing.T) {
	for _, age := range []time.Duration{10 * time.Second, 90 * time.Second} {
		t.Run(age.String(), func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			mgr := sessionpkg.NewManagerWithOptions(store, sp)
			info, err := mgr.CreateSession(context.Background(), sessionpkg.CreateOptions{
				Template: "helper", Command: "codex", Provider: "codex", WorkDir: t.TempDir(),
				ExtraMeta: map[string]string{"session_origin": "manual"},
			})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			clk := &clock.Fake{Time: now}
			if err := store.SetMetadataBatch(info.ID, map[string]string{
				"last_woke_at": now.Add(-age).Format(time.RFC3339),
				"session_key":  "conversation-to-resume", "started_config_hash": "configured-launch",
				"continuation_reset_pending": "", "wake_attempts": "0", "churn_count": "0",
			}); err != nil {
				t.Fatal(err)
			}
			if err := mgr.Suspend(info.ID); err != nil {
				t.Fatal(err)
			}
			if sp.IsRunning(info.SessionName) {
				t.Fatal("suspended runtime is still running")
			}
			front := sessionFrontDoor(store)
			drains := newDrainTracker()
			for tick := 0; tick < 2; tick++ {
				info, err = mgr.Get(info.ID)
				if err != nil {
					t.Fatal(err)
				}
				info = info.ApplyPatch(healStateWithRollbackInfo(info, false, front, clk, 0, true))
				var crashed, churned bool
				info, crashed = checkStability(info, nil, false, drains, front, clk, nil)
				info, churned = checkChurn(info, nil, false, drains, front, clk)
				if crashed || churned {
					t.Errorf("tick %d classified intentional suspend as crash=%v churn=%v", tick, crashed, churned)
				}
				got, err := mgr.Get(info.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.SessionKey != "conversation-to-resume" || got.ContinuationResetPending == "true" {
					t.Fatalf("tick %d lost suspended conversation: key=%q reset=%v", tick, got.SessionKey, got.ContinuationResetPending)
				}
				if got.WakeAttempts != 0 || got.ChurnCount != "0" {
					t.Fatalf("tick %d accrued failure counters: wake=%d churn=%q", tick, got.WakeAttempts, got.ChurnCount)
				}
			}
		})
	}
}
