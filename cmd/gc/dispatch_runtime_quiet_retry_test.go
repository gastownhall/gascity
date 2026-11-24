package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
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

	// previous is the pending set of the caller's previous completed sweep;
	// rows without one are the one-shot --serve path and the first --follow
	// sweep, which behave exactly as before the baseline existed.
	tests := []struct {
		name            string
		serveErr        error
		previous        workflowServePendingRefusals
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
		{
			// gastownhall/gascity#6025: a finalize waiting on an open blocker
			// answers the same unmarked refusal every sweep. Repeating the
			// previous sweep's refusal verbatim is retried but not pending.
			name:            "verbatim repeat of the previous sweep's pending does not pin the backoff",
			serveErr:        pending,
			previous:        workflowServePendingRefusals{"pl-mmneh": pending.Error()},
			wantPendingAny:  false,
			wantSweepGrowth: true,
		},
		{
			name:            "changed pending refusal still wakes the loop",
			serveErr:        pending,
			previous:        workflowServePendingRefusals{"pl-mmneh": dispatch.ErrControlPending.Error()},
			wantPendingAny:  true,
			wantSweepGrowth: false,
		},
		{
			name:            "another bead's pending baseline does not quiet this bead",
			serveErr:        pending,
			previous:        workflowServePendingRefusals{"pl-other": pending.Error()},
			wantPendingAny:  true,
			wantSweepGrowth: false,
		},
		{
			name:            "pending baseline does not quiet a transient refusal",
			serveErr:        blocked,
			previous:        workflowServePendingRefusals{"pl-mmneh": blocked.Error()},
			wantPendingAny:  true,
			wantSweepGrowth: false,
		},
		{
			name:            "quiet pending stays quiet whatever the baseline holds",
			serveErr:        dispatch.MarkQuietControllerRetry(pending),
			previous:        workflowServePendingRefusals{"pl-mmneh": "a different refusal"},
			wantPendingAny:  false,
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
			// The drain hands its pending set to the next sweep as that sweep's
			// baseline, recording the refusal text whether or not it was quiet.
			wantPending := workflowServePendingRefusals{}
			if errors.Is(tt.serveErr, dispatch.ErrControlPending) {
				wantPending["pl-mmneh"] = tt.serveErr.Error()
			}
			if !maps.Equal(result.pending, wantPending) {
				t.Fatalf("pending = %v, want %v", result.pending, wantPending)
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

// pendingFollowWait is one workflowServeWaitForWake call the --follow loop made.
type pendingFollowWait struct {
	idleSweeps int
	sleepDur   time.Duration
}

// pendingFollowRow is one control bead in a scripted sweep's ready queue and
// the answer controlDispatcherServe gives for it on that sweep.
type pendingFollowRow struct {
	id  string
	err error
}

// pendingFollowSweep scripts one --follow sweep: the queue the drain lists, or
// the error its work query fails with.
type pendingFollowSweep struct {
	rows    []pendingFollowRow
	listErr error
}

// runPendingFollowScript runs runWorkflowServeFollow over the scripted sweeps
// with no event wakes and stops it on the wait after the last sweep. A bead
// whose script answer is nil advanced, so it leaves the queue for the rest of
// its sweep, as a closed control bead does. It returns every wait the loop
// made and how many times each bead was dispatched.
func runPendingFollowScript(t *testing.T, sweeps []pendingFollowSweep) ([]pendingFollowWait, map[string]int) {
	t.Helper()

	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevProvider := workflowServeOpenEventsProvider
	prevWait := workflowServeWaitForWake
	prevPollInterval := workflowServeIdlePollInterval
	prevPollAttempts := workflowServeIdlePollAttempts
	t.Cleanup(func() {
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeOpenEventsProvider = prevProvider
		workflowServeWaitForWake = prevWait
		workflowServeIdlePollInterval = prevPollInterval
		workflowServeIdlePollAttempts = prevPollAttempts
	})
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0

	ep := newTestProvider(t, t.TempDir())
	workflowServeOpenEventsProvider = func(io.Writer) (events.Provider, error) { return ep, nil }

	var waits []pendingFollowWait
	stopErr := errors.New("stop after the scripted sweeps")
	workflowServeWaitForWake = func(_ <-chan workflowWatchResult, sleepDur time.Duration, idleSweeps int) (bool, error) {
		waits = append(waits, pendingFollowWait{idleSweeps: idleSweeps, sleepDur: sleepDur})
		if len(waits) == len(sweeps) {
			return false, stopErr
		}
		return false, nil
	}

	advanced := map[int]map[string]bool{}
	currentSweep := func() pendingFollowSweep {
		if len(waits) >= len(sweeps) {
			t.Fatalf("drain ran past the %d scripted sweeps", len(sweeps))
		}
		return sweeps[len(waits)]
	}
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		sweep := currentSweep()
		if sweep.listErr != nil {
			return nil, sweep.listErr
		}
		var queue []hookBead
		for _, row := range sweep.rows {
			if advanced[len(waits)][row.id] {
				continue
			}
			queue = append(queue, hookBead{ID: row.id, Metadata: hookBeadMetadata{"gc.kind": "workflow-finalize"}})
		}
		return queue, nil
	}
	dispatches := map[string]int{}
	controlDispatcherServe = func(_, _, beadID string, _, _ io.Writer, _ *executionEmitDeferral) error {
		dispatches[beadID]++
		for _, row := range currentSweep().rows {
			if row.id != beadID {
				continue
			}
			if row.err == nil {
				if advanced[len(waits)] == nil {
					advanced[len(waits)] = map[string]bool{}
				}
				advanced[len(waits)][beadID] = true
			}
			return row.err
		}
		t.Fatalf("bead %s dispatched on sweep %d, which does not list it", beadID, len(waits)+1)
		return nil
	}

	cityPath := t.TempDir()
	err := runWorkflowServeFollow(config.Agent{Name: "control-dispatcher"}, cityPath, cityPath, "bd ready", nil, io.Discard)
	if !errors.Is(err, stopErr) {
		t.Fatalf("runWorkflowServeFollow error = %v, want %v", err, stopErr)
	}
	return waits, dispatches
}

// repeatSweeps scripts n sweeps whose queue is the given rows.
func repeatSweeps(n int, rows ...pendingFollowRow) []pendingFollowSweep {
	sweeps := make([]pendingFollowSweep, n)
	for i := range sweeps {
		sweeps[i] = pendingFollowSweep{rows: rows}
	}
	return sweeps
}

// TestRunWorkflowServeFollowBacksOffOnRepeatedPending is gastownhall/gascity#6025:
// a workflow-finalize waiting on an open blocker answers the same bare
// ErrControlPending on every sweep for as long as the blocker stays open. It is
// still dispatched every sweep, but only its first refusal counts as pending,
// so the idle backoff grows to its cap instead of pinning the loop at 1s.
func TestRunWorkflowServeFollowBacksOffOnRepeatedPending(t *testing.T) {
	clearGCEnv(t)

	waits, dispatches := runPendingFollowScript(t,
		repeatSweeps(6, pendingFollowRow{id: "gc-blocked", err: dispatch.ErrControlPending}))

	want := []pendingFollowWait{
		{0, 1 * time.Second},
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 5 * time.Second},
		{4, 5 * time.Second},
	}
	if !slices.Equal(waits, want) {
		t.Fatalf("waits = %+v, want %+v", waits, want)
	}
	if got := dispatches["gc-blocked"]; got != 6 {
		t.Fatalf("gc-blocked dispatched %d times, want 6 (a repeat is still retried every sweep)", got)
	}
}

// TestRunWorkflowServeFollowNewPendingResetsBackoff pins that only a verbatim
// repeat of the previous completed sweep's pending refusal is quiet. A changed
// refusal, a newly pending bead, or a bead pending again after a sweep where it
// was absent, skipped or processed reports pending and puts the backoff back on
// its 1s floor.
func TestRunWorkflowServeFollowNewPendingResetsBackoff(t *testing.T) {
	waitingOn := func(blocker string) error {
		return fmt.Errorf("%w: waiting on %s", dispatch.ErrControlPending, blocker)
	}
	blocked := pendingFollowRow{id: "gc-blocked", err: dispatch.ErrControlPending}
	// Three repeats of the same refusal grow the backoff to idle_sweeps=2
	// before each row's change, so a reset back to {0,1s} is visible.
	grown := []pendingFollowWait{{0, 1 * time.Second}, {0, 1 * time.Second}, {1, 2 * time.Second}}

	tests := []struct {
		name   string
		sweeps []pendingFollowSweep
		want   []pendingFollowWait
	}{
		{
			name: "changed refusal",
			sweeps: append(
				repeatSweeps(3, pendingFollowRow{id: "gc-blocked", err: waitingOn("gc-a")}),
				repeatSweeps(3, pendingFollowRow{id: "gc-blocked", err: waitingOn("gc-b")})...),
			want: append(slices.Clone(grown),
				pendingFollowWait{0, 1 * time.Second},
				pendingFollowWait{0, 1 * time.Second},
				pendingFollowWait{1, 2 * time.Second}),
		},
		{
			name: "second bead joins",
			sweeps: append(
				repeatSweeps(3, blocked),
				repeatSweeps(3, blocked, pendingFollowRow{id: "gc-other", err: dispatch.ErrControlPending})...),
			want: append(slices.Clone(grown),
				pendingFollowWait{0, 1 * time.Second},
				pendingFollowWait{0, 1 * time.Second},
				pendingFollowWait{1, 2 * time.Second}),
		},
		{
			name: "absent for one sweep",
			sweeps: append(append(
				repeatSweeps(3, blocked),
				pendingFollowSweep{}),
				repeatSweeps(3, blocked)...),
			want: append(slices.Clone(grown),
				pendingFollowWait{2, 4 * time.Second},
				pendingFollowWait{0, 1 * time.Second},
				pendingFollowWait{0, 1 * time.Second},
				pendingFollowWait{1, 2 * time.Second}),
		},
		{
			name: "skipped for one sweep",
			sweeps: append(append(
				repeatSweeps(3, blocked),
				repeatSweeps(1, pendingFollowRow{id: "gc-blocked", err: errControlNotProcessed})...),
				repeatSweeps(3, blocked)...),
			want: append(slices.Clone(grown),
				pendingFollowWait{2, 4 * time.Second},
				pendingFollowWait{0, 1 * time.Second},
				pendingFollowWait{0, 1 * time.Second},
				pendingFollowWait{1, 2 * time.Second}),
		},
		{
			name: "processed for one sweep",
			sweeps: append(append(
				repeatSweeps(3, blocked),
				repeatSweeps(1, pendingFollowRow{id: "gc-blocked"})...),
				repeatSweeps(3, blocked)...),
			want: append(slices.Clone(grown),
				pendingFollowWait{0, 1 * time.Second},
				pendingFollowWait{0, 1 * time.Second},
				pendingFollowWait{0, 1 * time.Second},
				pendingFollowWait{1, 2 * time.Second}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearGCEnv(t)

			waits, _ := runPendingFollowScript(t, tt.sweeps)

			if !slices.Equal(waits, tt.want) {
				t.Fatalf("waits = %+v, want %+v", waits, tt.want)
			}
		})
	}
}
