package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
//
// The previous column is the pending baseline of the caller's previous
// completed sweep (nil = no previous sweep, the one-shot and first --follow
// case); a verbatim repeat of it stays quiet even without the source-side
// MarkQuietControllerRetry mark.
func TestDrainWorkflowServeWorkQuietRetryDoesNotReportPending(t *testing.T) {
	clearGCEnv(t)

	blocked := errors.New(`pl-pujtf: completing workflow head: updating bead "pl-pujtf": exit status 1: cannot close blocked issue: pl-pujtf is blocked by [pl-mmneh]`)
	pending := fmt.Errorf("%w: rig %q not found in city config", dispatch.ErrControlPending, "ghostrig")

	tests := []struct {
		name            string
		serveErr        error
		previous        workflowServePendingRefusals
		wantPendingAny  bool
		wantProcessed   bool
		wantSweepGrowth bool
		// wantPendingReason is the refusal text the drain must record for
		// bead pl-mmneh in result.pending; empty means the drain must record
		// no pending refusal at all.
		wantPendingReason string
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
			// The mark wraps the same refusal text an unmarked repeat of this
			// pending would give, so the recorded baseline compares equal.
			wantPendingReason: pending.Error(),
		},
		{
			name:              "first pending refusal still wakes the loop",
			serveErr:          pending,
			wantPendingAny:    true,
			wantSweepGrowth:   false,
			wantPendingReason: pending.Error(),
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
		{
			// A verbatim repeat of the previous completed sweep's refusal is
			// quiet without any mark: ordinary pending is zero-write by contract
			// (the ErrControlDriftPending doc), so the loop, not the source,
			// recognizes the repeat (gastownhall/gascity#6025).
			name:              "verbatim repeat of the previous sweep stays quiet",
			serveErr:          pending,
			previous:          workflowServePendingRefusals{"pl-mmneh": pending.Error()},
			wantPendingAny:    false,
			wantSweepGrowth:   true,
			wantPendingReason: pending.Error(),
		},
		{
			name:              "changed refusal text reports pending",
			serveErr:          pending,
			previous:          workflowServePendingRefusals{"pl-mmneh": fmt.Errorf("%w: rig %q not found in city config", dispatch.ErrControlPending, "oldrig").Error()},
			wantPendingAny:    true,
			wantSweepGrowth:   false,
			wantPendingReason: pending.Error(),
		},
		{
			name:              "another bead's baseline does not quiet this bead",
			serveErr:          pending,
			previous:          workflowServePendingRefusals{"pl-other": pending.Error()},
			wantPendingAny:    true,
			wantSweepGrowth:   false,
			wantPendingReason: pending.Error(),
		},
		{
			// A pending baseline never quiets a transient refusal: those keep
			// their own (persisted) repeat story.
			name:            "pending baseline does not quiet a transient refusal",
			serveErr:        blocked,
			previous:        workflowServePendingRefusals{"pl-mmneh": pending.Error()},
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
				config.Agent{Name: "control-dispatcher"}, cityPath, cityPath, "bd ready", nil, tt.previous, io.Discard)
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
			if got := result.pending["pl-mmneh"]; got != tt.wantPendingReason {
				t.Fatalf("result.pending[pl-mmneh] = %q, want %q", got, tt.wantPendingReason)
			}
			if tt.wantPendingReason == "" && len(result.pending) != 0 {
				t.Fatalf("result.pending = %v, want no recorded refusal for this outcome", result.pending)
			}
			if tt.wantPendingReason != "" && len(result.pending) != 1 {
				t.Fatalf("result.pending = %v, want exactly the one recorded refusal", result.pending)
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

// TestRunWorkflowServeFollowNewPendingResetsBackoff pins that only a verbatim
// repeat is quiet: any pending outcome the previous completed sweep did not
// end with — a changed refusal, a new pending bead, or the same bead re-pending
// after a sweep where it was absent, skipped or processed — reports pending
// again and resets the idle backoff, exactly as a fresh pending onset does.
func TestRunWorkflowServeFollowNewPendingResetsBackoff(t *testing.T) {
	clearGCEnv(t)

	refusalA := fmt.Errorf("%w: waiting on gc-a", dispatch.ErrControlPending)
	refusalB := fmt.Errorf("%w: waiting on gc-b", dispatch.ErrControlPending)
	joinRefusal := fmt.Errorf("%w: waiting on gate gc-gate", dispatch.ErrControlPending)

	blockedRow := hookBead{ID: "gc-blocked", Metadata: hookBeadMetadata{"gc.kind": "workflow-finalize"}}
	joinerRow := hookBead{ID: "gc-joiner", Metadata: hookBeadMetadata{"gc.kind": "workflow-finalize"}}
	blocked := func() []hookBead { return []hookBead{blockedRow} }

	tests := []struct {
		name    string
		attempt int
		queue   func(listCall int) ([]hookBead, error)
		serve   func(listCall int, beadID string) error
		want    []serveFollowWaitCall
	}{
		{
			// Three identical refusals let the backoff grow, then the refusal
			// text changes and the next wait is back at the 1s floor.
			name:  "changed refusal text reports pending again",
			queue: func(int) ([]hookBead, error) { return blocked(), nil },
			serve: func(c int, _ string) error {
				if c <= 3 {
					return refusalA
				}
				return refusalB
			},
			want: []serveFollowWaitCall{
				{idleSweeps: 0, sleepDur: 1 * time.Second},
				{idleSweeps: 0, sleepDur: 1 * time.Second},
				{idleSweeps: 1, sleepDur: 2 * time.Second},
				{idleSweeps: 0, sleepDur: 1 * time.Second},
			},
		},
		{
			// A second bead joining the queue is a new pending onset, even
			// though the first bead keeps repeating its own refusal.
			name: "a new pending bead reports pending again",
			queue: func(c int) ([]hookBead, error) {
				if c <= 3 {
					return blocked(), nil
				}
				return []hookBead{blockedRow, joinerRow}, nil
			},
			serve: func(_ int, beadID string) error {
				if beadID == "gc-joiner" {
					return joinRefusal
				}
				return refusalA
			},
			want: []serveFollowWaitCall{
				{idleSweeps: 0, sleepDur: 1 * time.Second},
				{idleSweeps: 0, sleepDur: 1 * time.Second},
				{idleSweeps: 1, sleepDur: 2 * time.Second},
				{idleSweeps: 0, sleepDur: 1 * time.Second},
			},
		},
		{
			// Absent for one sweep, then pending again with the same text: a
			// fresh onset, because the baseline only holds the sweep before it.
			name: "absent for one sweep then re-pending reports pending again",
			queue: func(c int) ([]hookBead, error) {
				if c == 4 {
					return nil, nil
				}
				return blocked(), nil
			},
			serve: func(int, string) error { return refusalA },
			want: []serveFollowWaitCall{
				{idleSweeps: 0, sleepDur: 1 * time.Second},
				{idleSweeps: 0, sleepDur: 1 * time.Second},
				{idleSweeps: 1, sleepDur: 2 * time.Second},
				{idleSweeps: 2, sleepDur: 4 * time.Second},
				{idleSweeps: 0, sleepDur: 1 * time.Second},
			},
		},
		{
			// Skipped for one sweep (bead no longer open at dispatch time),
			// then pending again with the same text.
			name:  "skipped for one sweep then re-pending reports pending again",
			queue: func(int) ([]hookBead, error) { return blocked(), nil },
			serve: func(c int, _ string) error {
				if c == 4 {
					return errControlNotProcessed
				}
				return refusalA
			},
			want: []serveFollowWaitCall{
				{idleSweeps: 0, sleepDur: 1 * time.Second},
				{idleSweeps: 0, sleepDur: 1 * time.Second},
				{idleSweeps: 1, sleepDur: 2 * time.Second},
				{idleSweeps: 2, sleepDur: 4 * time.Second},
				{idleSweeps: 0, sleepDur: 1 * time.Second},
			},
		},
		{
			// Processed for one sweep (the drain re-queries once, hence the
			// empty queue on the following list call), then pending again
			// with the same text.
			name:    "processed for one sweep then re-pending reports pending again",
			attempt: 0,
			queue: func(c int) ([]hookBead, error) {
				if c == 4 {
					return blocked(), nil
				}
				if c == 5 {
					return nil, nil
				}
				return blocked(), nil
			},
			serve: func(c int, _ string) error {
				if c == 4 {
					return nil
				}
				return refusalA
			},
			want: []serveFollowWaitCall{
				{idleSweeps: 0, sleepDur: 1 * time.Second},
				{idleSweeps: 0, sleepDur: 1 * time.Second},
				{idleSweeps: 1, sleepDur: 2 * time.Second},
				{idleSweeps: 0, sleepDur: 1 * time.Second},
				{idleSweeps: 0, sleepDur: 1 * time.Second},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prevAttempts := workflowServeIdlePollAttempts
			t.Cleanup(func() { workflowServeIdlePollAttempts = prevAttempts })
			workflowServeIdlePollAttempts = tt.attempt

			waits, stopErr := stubServeFollowLoop(t, tt.queue, tt.serve, len(tt.want))
			err := runWorkflowServeFollow(config.Agent{Name: "control-dispatcher"}, t.TempDir(), t.TempDir(), "bd ready", nil, io.Discard)
			if !errors.Is(err, stopErr) {
				t.Fatalf("runWorkflowServeFollow error = %v, want %v", err, stopErr)
			}
			if !slices.Equal(*waits, tt.want) {
				t.Fatalf("waits = %+v, want %+v", *waits, tt.want)
			}
		})
	}
}

// TestDrainWorkflowServeWorkRepeatedPendingDoesNotPinBackoff is the
// ErrControlPending twin of the quiet-retry fix above (gastownhall/gascity#6025).
// A workflow-finalize whose `blocks` dependency is still open returns a bare
// ErrControlPending (internal/dispatch processWorkflowFinalize) on every sweep
// for as long as the blocker stays open, which can be days. The first sweep
// must still report pending, but a verbatim repeat of the same bead's pending
// refusal on the next sweep must not, or runWorkflowServeFollow resets
// idleSweeps on every sweep and followSleepDuration never leaves its 1s floor.
func TestDrainWorkflowServeWorkRepeatedPendingDoesNotPinBackoff(t *testing.T) {
	clearGCEnv(t)

	prevList := workflowServeList
	prevControl := controlDispatcherServe
	t.Cleanup(func() {
		workflowServeList = prevList
		controlDispatcherServe = prevControl
	})

	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		return []hookBead{{ID: "gc-blockedfin", Metadata: hookBeadMetadata{"gc.kind": "workflow-finalize"}}}, nil
	}
	controlDispatcherServe = func(_, _, _ string, _, _ io.Writer, _ *executionEmitDeferral) error {
		return dispatch.ErrControlPending
	}

	cityPath := t.TempDir()
	idleSweeps := 0
	// The --follow loop feeds each completed sweep's pending set to the next
	// drain as its baseline; the sweeps here do the same.
	var previous workflowServePendingRefusals
	for sweep := 1; sweep <= 3; sweep++ {
		result, err := drainWorkflowServeWork(
			config.Agent{Name: "control-dispatcher"}, cityPath, cityPath, "bd ready", nil, previous, io.Discard)
		if err != nil {
			t.Fatalf("sweep %d: drainWorkflowServeWork: %v", sweep, err)
		}
		previous = result.pending
		if sweep == 1 {
			if !result.pendingAny {
				t.Fatalf("sweep 1: pendingAny = false, want true for the first pending refusal")
			}
			continue
		}
		if result.pendingAny {
			t.Fatalf("sweep %d: pendingAny = true for a verbatim repeat of the same bead's ErrControlPending; "+
				"runWorkflowServeFollow resets idleSweeps on it, pinning followSleepDuration at %s forever",
				sweep, followSleepDuration(0))
		}
		if !result.processedAny {
			idleSweeps++
		}
	}
	if followSleepDuration(idleSweeps) <= followSleepDuration(0) {
		t.Fatalf("idle backoff stayed at %s after repeated identical pending sweeps, want it to grow", followSleepDuration(idleSweeps))
	}
}

// TestRunWorkflowServeFollowTracesPendingOnsetOnce pins the trace contract of
// the repeat suppression: the first pending sweep writes "serve pending", and
// every verbatim repeat after it writes "serve pending-repeat", so a long-lived
// gate reads as one onset line in the trace instead of one line per second.
func TestRunWorkflowServeFollowTracesPendingOnsetOnce(t *testing.T) {
	clearGCEnv(t)
	tracePath := filepath.Join(t.TempDir(), "serve-trace.log")
	t.Setenv("GC_WORKFLOW_TRACE", tracePath)

	blockedRow := hookBead{ID: "gc-blocked", Metadata: hookBeadMetadata{"gc.kind": "workflow-finalize"}}
	_, stopErr := stubServeFollowLoop(t,
		func(int) ([]hookBead, error) { return []hookBead{blockedRow}, nil },
		func(int, string) error { return dispatch.ErrControlPending },
		3)
	if err := runWorkflowServeFollow(config.Agent{Name: "control-dispatcher"}, t.TempDir(), t.TempDir(), "bd ready", nil, io.Discard); !errors.Is(err, stopErr) {
		t.Fatalf("runWorkflowServeFollow error = %v, want %v", err, stopErr)
	}

	traceRaw, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("reading workflow trace: %v", err)
	}
	trace := string(traceRaw)
	onsets := strings.Count(trace, "serve pending bead=gc-blocked kind=workflow-finalize")
	repeats := strings.Count(trace, "serve pending-repeat bead=gc-blocked kind=workflow-finalize")
	if onsets != 1 {
		t.Fatalf("trace shows %d \"serve pending\" lines for one blocked bead, want 1 (the onset):\n%s", onsets, trace)
	}
	if repeats != 2 {
		t.Fatalf("trace shows %d \"serve pending-repeat\" lines after the onset, want 2 (one per repeat sweep):\n%s", repeats, trace)
	}
}

// TestRunWorkflowServeFollowPendingBaselineSurvivesTransientFailure pins that a
// transient drain failure between two sweeps does not clear the repeat
// baseline: the drain observed no complete pending set, so clearing it would
// put a dispatcher behind a saturated store back on the 1s floor after every
// failed sweep.
func TestRunWorkflowServeFollowPendingBaselineSurvivesTransientFailure(t *testing.T) {
	clearGCEnv(t)

	// The work-query timeout drainWorkflowServeWork wraps verbatim when the
	// store is briefly saturated (TestRunWorkflowServeFollowSurvivesTransient
	// WorkQueryTimeout's shape).
	transientErr := fmt.Errorf("querying control work: running work query %q: timed out after 30s: %w", "bd ready", context.DeadlineExceeded)
	blockedRow := hookBead{ID: "gc-blocked", Metadata: hookBeadMetadata{"gc.kind": "workflow-finalize"}}
	waits, stopErr := stubServeFollowLoop(t,
		func(c int) ([]hookBead, error) {
			if c == 2 {
				return nil, transientErr
			}
			return []hookBead{blockedRow}, nil
		},
		func(int, string) error { return dispatch.ErrControlPending },
		3)
	if err := runWorkflowServeFollow(config.Agent{Name: "control-dispatcher"}, t.TempDir(), t.TempDir(), "bd ready", nil, io.Discard); !errors.Is(err, stopErr) {
		t.Fatalf("runWorkflowServeFollow error = %v, want %v", err, stopErr)
	}
	want := []serveFollowWaitCall{
		{idleSweeps: 0, sleepDur: 1 * time.Second},
		{idleSweeps: 0, sleepDur: 1 * time.Second},
		{idleSweeps: 1, sleepDur: 2 * time.Second},
	}
	if !slices.Equal(*waits, want) {
		t.Fatalf("waits = %+v, want %+v (the failed sweep must not clear the repeat baseline)", *waits, want)
	}
}
