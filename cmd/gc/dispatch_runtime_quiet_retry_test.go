package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
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
	pending := fmt.Errorf("%w: rig %q not found in city config", dispatch.ErrControlPending, "ghostrig")

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
		{
			// Pending took the same arm the semantic tier took before the quiet
			// mark existed, and it can last far longer: it waits on config drift
			// a human heals, so a removed rig holds the loop at its 1s floor for
			// days while it re-runs a full dispatch — config load, store open,
			// outcome read, source-chain preflight — to re-learn the same answer.
			name:            "quiet pending repeat does not pin the backoff",
			serveErr:        dispatch.MarkQuietControllerRetry(pending),
			wantPendingAny:  false,
			wantSweepGrowth: true,
		},
		{
			name:            "first pending refusal still wakes the loop",
			serveErr:        pending,
			wantPendingAny:  true,
			wantSweepGrowth: false,
		},
		{
			// A no-op dispatch (the bead is no longer open, a spawned fanout
			// waiting on its finalize) advanced nothing and waits on nothing.
			name:            "not-processed skip neither processes nor pends",
			serveErr:        errControlNotProcessed,
			wantPendingAny:  false,
			wantProcessed:   false,
			wantSweepGrowth: true,
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
			controlDispatcherServe = func(_, _, _ string, _, _ io.Writer, _ *executionEmitDeferral) error {
				serveCalls++
				// The stubbed queue never empties, so a drain that re-queries
				// after this row would spin forever: fail on the re-dispatch.
				if serveCalls > 1 {
					t.Fatalf("controlDispatcherServe called %d times, want 1: the drain re-queried instead of ending the pass", serveCalls)
				}
				return tt.serveErr
			}

			cityPath := t.TempDir()
			result, err := drainWorkflowServeWork(
				config.Agent{Name: "control-dispatcher"}, cityPath, cityPath, "bd ready", nil, nil, io.Discard)
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

// serveFollowWaitCall records the idle-sweep count and sleep duration of one
// workflowServeWaitForWake call.
type serveFollowWaitCall struct {
	idleSweeps int
	sleepDur   time.Duration
}

// stubServeFollowLoop installs the four workflow-serve seams around
// runWorkflowServeFollow. queue and serve answer by a 1-based list-call
// counter shared across drains, and the wait stub records every
// (idleSweeps, sleep) request and stops the loop after the wait count reaches
// stopAfter. It returns a pointer to the recorded waits and the error that
// ends the loop, so the caller can assert both the wait sequence and the
// loop's exit.
func stubServeFollowLoop(t *testing.T, queue func(listCall int) ([]hookBead, error), serve func(listCall int, beadID string) error, stopAfter int) (*[]serveFollowWaitCall, error) {
	t.Helper()
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevProvider := workflowServeOpenEventsProvider
	prevWait := workflowServeWaitForWake
	t.Cleanup(func() {
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeOpenEventsProvider = prevProvider
		workflowServeWaitForWake = prevWait
	})
	listCall := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		listCall++
		return queue(listCall)
	}
	controlDispatcherServe = func(_, _, beadID string, _, _ io.Writer, _ *executionEmitDeferral) error {
		return serve(listCall, beadID)
	}
	ep := newTestProvider(t, t.TempDir())
	workflowServeOpenEventsProvider = func(io.Writer) (events.Provider, error) { return ep, nil }
	waits := []serveFollowWaitCall{}
	stopErr := fmt.Errorf("stop after %d waits", stopAfter)
	workflowServeWaitForWake = func(_ <-chan workflowWatchResult, sleepDur time.Duration, idleSweeps int) (bool, error) {
		waits = append(waits, serveFollowWaitCall{idleSweeps: idleSweeps, sleepDur: sleepDur})
		if len(waits) == stopAfter {
			return false, stopErr
		}
		return false, nil
	}
	return &waits, stopErr
}

// TestRunWorkflowServeFollowBacksOffOnRepeatedPending is the ErrControlPending
// twin of the quiet-retry fix (gastownhall/gascity#6025): a control bead
// blocked on an open `blocks` dependency returns a bare dispatch.ErrControlPending
// on every sweep for as long as the blocker stays open, which is an ordinary
// state that can last days. The first sweep reports pending as today; a
// verbatim repeat must still be dispatched every sweep but must not reset the
// idle backoff, or followSleepDuration stays pinned at its 1s floor forever
// (one blocked finalize held 95.7% of a city's serve sweeps for 34 hours).
func TestRunWorkflowServeFollowBacksOffOnRepeatedPending(t *testing.T) {
	clearGCEnv(t)
	blockedRow := hookBead{ID: "gc-blocked", Metadata: hookBeadMetadata{"gc.kind": "workflow-finalize"}}

	serveCalls := 0
	waits, stopErr := stubServeFollowLoop(t,
		func(int) ([]hookBead, error) { return []hookBead{blockedRow}, nil },
		func(int, string) error {
			serveCalls++
			return dispatch.ErrControlPending
		},
		6)

	err := runWorkflowServeFollow(config.Agent{Name: "control-dispatcher"}, t.TempDir(), t.TempDir(), "bd ready", nil, io.Discard)
	if !errors.Is(err, stopErr) {
		t.Fatalf("runWorkflowServeFollow error = %v, want %v", err, stopErr)
	}
	want := []serveFollowWaitCall{
		{idleSweeps: 0, sleepDur: 1 * time.Second},
		{idleSweeps: 0, sleepDur: 1 * time.Second},
		{idleSweeps: 1, sleepDur: 2 * time.Second},
		{idleSweeps: 2, sleepDur: 4 * time.Second},
		{idleSweeps: 3, sleepDur: 5 * time.Second},
		{idleSweeps: 4, sleepDur: 5 * time.Second},
	}
	if !slices.Equal(*waits, want) {
		t.Fatalf("waits = %+v, want %+v (a verbatim pending repeat must not reset the idle backoff)", *waits, want)
	}
	if serveCalls != 6 {
		t.Fatalf("control dispatches = %d, want 6 (a repeat must still be retried on every sweep)", serveCalls)
	}
}
