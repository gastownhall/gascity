package main

import (
	"errors"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// TestDrainWorkflowServeWorkQuietRetryDoesNotReportPending is the backoff-pin
// fix. drainWorkflowServeWork reports pendingAny to runWorkflowServeFollow,
// which resets idleSweeps to 0 whenever it is set — so a permanently-stuck bead
// that reports pending on every sweep holds the loop at followSleepDuration's
// 1s floor forever. MEASURED on platform: two such beads consumed 13,419 of
// 14,125 control dispatches (95%) in a 19-hour window while the other ~700
// control beads waited behind them.
func TestDrainWorkflowServeWorkQuietRetryDoesNotReportPending(t *testing.T) {
	clearGCEnv(t)

	blocked := errors.New(`pl-pujtf: completing workflow head: updating bead "pl-pujtf": exit status 1: cannot close blocked issue: pl-pujtf is blocked by [pl-mmneh]`)

	tests := []struct {
		name            string
		serveErr        error
		wantPendingAny  bool
		wantProcessed   bool
		wantSweepGrowth bool
	}{
		{
			name:     "quiet repeat does not pin the backoff",
			serveErr: dispatch.MarkQuietControllerRetry(blocked),
			// A quiet retry is still retried on every sweep; it just stops
			// claiming the sweep made progress.
			wantPendingAny:  false,
			wantSweepGrowth: true,
		},
		{
			name:            "first semantic refusal still wakes the loop",
			serveErr:        blocked,
			wantPendingAny:  true,
			wantSweepGrowth: false,
		},
		{
			name:            "availability failure still wakes the loop",
			serveErr:        errors.New("updating bead: dial tcp 127.0.0.1:3306: connect: connection refused"),
			wantPendingAny:  true,
			wantSweepGrowth: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prevList := workflowServeList
			prevControl := controlDispatcherServe
			t.Cleanup(func() {
				workflowServeList = prevList
				controlDispatcherServe = prevControl
			})

			workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
				return []hookBead{{ID: "pl-mmneh", Metadata: hookBeadMetadata{"gc.kind": "workflow-finalize"}}}, nil
			}
			serveCalls := 0
			controlDispatcherServe = func(_, _, _ string, _, _ io.Writer) error {
				serveCalls++
				return tt.serveErr
			}

			cityPath := t.TempDir()
			result, err := drainWorkflowServeWork(
				config.Agent{Name: "control-dispatcher"}, cityPath, cityPath, "bd ready", nil, io.Discard)
			if err != nil {
				t.Fatalf("drainWorkflowServeWork: %v", err)
			}
			if serveCalls != 1 {
				t.Fatalf("controlDispatcherServe called %d times, want 1 (a quiet retry is still attempted)", serveCalls)
			}
			if result.processedAny != tt.wantProcessed {
				t.Fatalf("processedAny = %v, want %v", result.processedAny, tt.wantProcessed)
			}
			if result.pendingAny != tt.wantPendingAny {
				t.Fatalf("pendingAny = %v, want %v", result.pendingAny, tt.wantPendingAny)
			}

			// runWorkflowServeFollow's own arithmetic: pendingAny resets
			// idleSweeps, so only a non-pending sweep lets the backoff grow past
			// its floor.
			idleSweeps := 0
			if !result.processedAny && !result.pendingAny {
				idleSweeps++
			}
			grew := followSleepDuration(idleSweeps) > followSleepDuration(0)
			if grew != tt.wantSweepGrowth {
				t.Fatalf("idle backoff grew = %v (sleep %s), want %v",
					grew, followSleepDuration(idleSweeps), tt.wantSweepGrowth)
			}
		})
	}
}
