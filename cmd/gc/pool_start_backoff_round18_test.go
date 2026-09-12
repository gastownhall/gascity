package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// An identified pane whose agent died during startup must charge failure five,
// including when the async commit rechecks convergence after observation.
func TestPendingCreateStartupDeathParksInsteadOfClearingFailures(t *testing.T) {
	for _, asyncCommit := range []bool{false, true} {
		name := "execute"
		if asyncCommit {
			name = "async_commit"
		}
		t.Run(name, func(t *testing.T) {
			h := newStartBackoffHarness(t, 5)
			if err := h.env.store.SetMetadataBatch(h.work.ID, map[string]string{
				beadmeta.StartFailuresMetadataKey: "4", beadmeta.StartFailedAtMetadataKey: h.env.clk.Now().Add(-time.Hour).Format(time.RFC3339),
			}); err != nil {
				t.Fatal(err)
			}
			const runtimeName = "pending-zombie"
			row, err := h.env.store.Create(beads.Bead{Title: backoffHarnessTemplate, Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
				"template": backoffHarnessTemplate, "session_name": runtimeName, "state": "creating", "session_key": "stale-key", "instance_token": "startup-token", "generation": "1", "continuation_epoch": "1", "pending_create_claim": "true",
				beadmeta.TriggerBeadIDMetadataKey: h.work.ID, beadmeta.TriggerBeadStoreRefMetadataKey: "city",
			}})
			if err != nil {
				t.Fatal(err)
			}
			item := preparedStart{
				candidate:   startCandidate{info: sessionInfosFromBeads([]beads.Bead{row})[0], tp: TemplateParams{Command: "test-cmd", SessionName: runtimeName, TemplateName: backoffHarnessTemplate}},
				cfg:         runtime.Config{Command: "test-cmd", ProcessNames: []string{"agent-binary"}},
				workTrigger: workTrigger{BeadID: h.work.ID, StoreRef: "city"}, workStartFailure: h.policy,
			}
			die := func(context.Context, string) bool {
				for k, v := range map[string]string{"GC_SESSION_ID": row.ID, "GC_INSTANCE_TOKEN": "startup-token"} {
					if err := h.env.sp.SetMeta(runtimeName, k, v); err != nil {
						t.Error(err)
					}
				}
				h.env.sp.Zombies[runtimeName] = true
				return true
			}
			var result startResult
			if asyncCommit {
				if err := h.env.sp.Start(context.Background(), runtimeName, item.cfg); err != nil {
					t.Fatal(err)
				}
				die(context.Background(), runtimeName)
				result = startResult{prepared: item, err: errors.New("session died during startup"), outcome: TraceOutcomeProviderError, rollbackPending: true, started: h.env.clk.Now(), finished: h.env.clk.Now()}
			} else {
				results := executePreparedStartWave(context.Background(), []preparedStart{item}, h.env.sp, nil, time.Second, withStartStabilityWaiter(die))
				result = results[0]
				if result.err == nil || !strings.Contains(result.err.Error(), "died during startup") {
					t.Errorf("observed startup death must remain a failure: err=%v outcome=%s", result.err, result.outcome)
				}
			}
			committed := false
			if asyncCommit {
				committed = commitAsyncStartResultWithContext(context.Background(), result, h.env.sp, h.env.store, h.env.clk, h.env.rec, 0, &h.env.stdout, &h.env.stderr, nil)
			} else {
				committed = commitStartResult(result, sessionFrontDoor(h.env.store), h.env.clk, h.env.rec, 0, &h.env.stdout, &h.env.stderr)
			}
			if committed {
				t.Error("dead startup must not commit success")
			}
			h.policy.awaitParkMailRetries()
			state := readWorkStartFailureState(h.reload().Metadata)
			if !state.Parked() || state.ParkFailures != 5 {
				t.Fatalf("fifth failed startup must park, not clear: %+v\nstderr: %s", state, h.env.stderr.String())
			}
			if len(h.mails) != 1 {
				t.Fatalf("park must send one notice, got %d", len(h.mails))
			}
		})
	}
}

// A parked assignment must not cancel the excess dispatcher's no-demand drain.
func TestControlDispatcherRecountExcludesDeferredAssignedWork(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	const template = config.ControlDispatcherAgentName
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: template, StartCommand: config.ControlDispatcherStartCommandFor("{{.Agent}}"), WorkQuery: "printf ''", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2)}}}
	var stderr bytes.Buffer
	cr := &CityRuntime{cityPath: t.TempDir(), cityName: "test-city", cfg: cfg, sp: sp, dops: newDrainOps(sp), rec: events.Discard, sessionDrains: newDrainTracker(), stdout: io.Discard, stderr: &stderr}
	cr.setControllerState(&controllerState{cfg: cfg, sp: sp, cityBeadStore: store, cityName: "test-city", cityPath: cr.cityPath})
	var holders []beads.Bead
	for i := 1; i <= 2; i++ {
		name := fmt.Sprintf("dispatcher-%d", i)
		row, err := store.Create(beads.Bead{Title: template, Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"template": template, "session_name": name, "session_name_explicit": "true", "state": "active", "pool_managed": "true", "pool_slot": fmt.Sprint(i), "generation": "1", "instance_token": name,
		}})
		if err != nil {
			t.Fatal(err)
		}
		holders = append(holders, row)
		if err := sp.Start(context.Background(), name, runtime.Config{Command: "test-cmd"}); err != nil {
			t.Fatal(err)
		}
		for k, v := range map[string]string{"GC_SESSION_ID": row.ID, "GC_INSTANCE_TOKEN": name} {
			if err := sp.SetMeta(name, k, v); err != nil {
				t.Fatal(err)
			}
		}
		work, err := store.Create(beads.Bead{Title: "parked dispatcher task", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: template, beadmeta.ParkedAtMetadataKey: "2026-09-11T03:04:00Z", beadmeta.ParkReasonMetadataKey: "pre_start failed", beadmeta.ParkFailuresMetadataKey: "5", beadmeta.ParkIDMetadataKey: name, beadmeta.ParkMailedAtMetadataKey: "2026-09-11T03:04:00Z"}})
		if err != nil {
			t.Fatal(err)
		}
		status, assignee := "in_progress", row.ID
		if err := store.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
			t.Fatal(err)
		}
	}
	cr.controlDispatcherTick(context.Background())
	draining := 0
	for _, holder := range holders {
		if cr.sessionDrains.get(holder.ID) != nil {
			draining++
		}
	}
	if draining != 1 {
		t.Fatalf("only the min-active dispatcher stays awake; one surplus must drain, got %d\nstderr: %s", draining, stderr.String())
	}
}
