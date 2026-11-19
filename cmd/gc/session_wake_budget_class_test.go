package main

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// wakeClassSpec is one legacy start candidate for the wake-class budget
// tests: its name, last_woke_at, and whether it is a standing wake.
type wakeClassSpec struct {
	name     string
	lastWoke string
	standing bool
}

// runWakeClassBudget runs one legacy start wave over specs, in slice order,
// with max_wakes_per_tick = budget, and returns the provider and the
// lifecycle log.
func runWakeClassBudget(t *testing.T, budget int, specs []wakeClassSpec) (*runtime.Fake, string) {
	t.Helper()
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 6, 4, 3, 30, 0, 0, time.UTC)}
	cfg := &config.City{Daemon: config.DaemonConfig{MaxWakesPerTick: &budget}}
	desired := map[string]TemplateParams{}
	var candidates []startCandidate
	for i, s := range specs {
		sess, err := store.Create(beads.Bead{
			Title:  s.name,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"session_name":   s.name,
				"agent_name":     s.name,
				"template":       s.name,
				"state":          "creating",
				"generation":     "1",
				"instance_token": "test-token",
				"live_hash":      runtime.LiveFingerprint(runtime.Config{Command: "test-cmd"}),
				"last_woke_at":   s.lastWoke,
			},
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", s.name, err)
		}
		tp := TemplateParams{
			Command:      "claude",
			SessionName:  s.name,
			TemplateName: s.name,
			ResolvedProvider: &config.ResolvedProvider{
				Name: "claude", Command: "claude", PromptMode: "arg",
				ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id",
			},
		}
		desired[s.name] = tp
		candidates = append(candidates, startCandidate{info: sessiontest.SeedBead(t, sess), tp: tp, order: i, standing: s.standing})
	}
	var stderr bytes.Buffer
	executePlannedStartsTraced(context.Background(), candidates, cfg, desired, sp, store, "", t.TempDir(), clk, events.Discard,
		5*time.Second, ioDiscard{}, &stderr, nil,
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter))
	return sp, stderr.String()
}

// runningOf is the names in specs the provider runs, in spec order.
func runningOf(sp *runtime.Fake, specs []wakeClassSpec) []string {
	var out []string
	for _, s := range specs {
		if sp.IsRunning(s.name) {
			out = append(out, s.name)
		}
	}
	return out
}

// TestWakeBudgetOneClassStartsOldestFirst is AC-3 of ga-z4joq0 in the legacy
// reconciler: candidates all of one class, standing or demand, spend a
// contended budget least recently woken first, as before the classes.
func TestWakeBudgetOneClassStartsOldestFirst(t *testing.T) {
	for _, standing := range []bool{true, false} {
		t.Run(fmt.Sprintf("standing=%v", standing), func(t *testing.T) {
			specs := []wakeClassSpec{
				{"recent", "2026-06-04T03:29:00Z", standing},
				{"oldest", "2020-01-01T00:00:00Z", standing},
				{"older", "2026-06-04T01:00:00Z", standing},
			}
			sp, log := runWakeClassBudget(t, 2, specs)
			if got := runningOf(sp, specs); !slices.Equal(got, []string{"oldest", "older"}) {
				t.Fatalf("started %v, want the two least recently woken; log:\n%s", got, log)
			}
		})
	}
}

// TestAdmitOneClassStartsOldestFirst is AC-3 of ga-z4joq0 in v2 admission:
// starts all of one class are admitted in LRU rank, as before the classes.
func TestAdmitOneClassStartsOldestFirst(t *testing.T) {
	for _, standing := range []bool{true, false} {
		mk := startOn
		if standing {
			mk = standingStartOn
		}
		res := admit(admitIn(2, 4), []intent{mk(1, "", 0), mk(2, "", -2*time.Hour), mk(3, "", -time.Hour)})
		if got := fmt.Sprint(keysOf(res.Admitted)); got != "[start:gc-2 start:gc-3]" {
			t.Fatalf("standing=%v: admitted %v, want the two oldest ranks", standing, got)
		}
		if len(res.Deferred) != 1 || res.Deferred[0].Key != admitRow(1) || res.Deferred[0].Cause != causeAwaitingBudget {
			t.Fatalf("standing=%v: deferred %+v, want the newest awaiting budget", standing, res.Deferred)
		}
	}
}
