package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// recordingPoolAllocationSink captures the exact keys the sweep's pool-fill arm
// hands to the pool-allocation admission, and can refuse them the way a
// saturated 256-slot hint channel does.
type recordingPoolAllocationSink struct {
	keys    []readyRoutedWorkEntry
	saturte bool
}

func (s *recordingPoolAllocationSink) enqueue(entry readyRoutedWorkEntry) bool {
	if s.saturte {
		return false
	}
	s.keys = append(s.keys, entry)
	return true
}

func poolFillSweepInput(routed []readyRoutedWorkEntry, desired map[string]int, rows []sessionpkg.ReconcileSession, sink *recordingPoolAllocationSink) detectorSweepInput {
	in := detectorSweepInput{
		CityPath:    "test-city",
		CityName:    "test-city",
		Cfg:         &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}},
		Rows:        rows,
		RoutedWork:  routed,
		PoolDesired: desired,
		Clock:       &clock.Fake{Time: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)},
		Trigger:     "patrol",
	}
	if sink != nil {
		in.EnqueuePoolAllocation = sink.enqueue
	}
	return in
}

func poolFillEntry(workID string) readyRoutedWorkEntry {
	return readyRoutedWorkEntry{
		SourceStore: "city:test-city",
		WorkID:      workID,
		PoolTarget:  "worker",
		Status:      "open",
		Type:        "task",
	}
}

// TestDetectorFillsPoolUnderMinByExactKey is WD.10b's primary RED: a template
// under its desired count with unallocated routed work raises D-WAKE's
// pool-under-min FILL arm and hands the exact
// (workID, poolTarget, sourceStore) key to the pool-allocation admission.
func TestDetectorFillsPoolUnderMinByExactKey(t *testing.T) {
	sink := &recordingPoolAllocationSink{}
	in := poolFillSweepInput(
		[]readyRoutedWorkEntry{poolFillEntry("wk-1")},
		map[string]int{"worker": 1},
		nil,
		sink,
	)

	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	fills := poolFillConditions(result)
	if len(fills) != 1 {
		t.Fatalf("pool-fill conditions = %+v, want exactly one for the under-filled pool", fills)
	}
	if fills[0].Outcome != TraceOutcomeStartCandidate || fills[0].SessionID != "" {
		t.Fatalf("pool-fill condition = %+v, want a start candidate with no session key", fills[0])
	}
	if fills[0].AdmissionOutcome != detectorAdmissionQueuedPoolAllocation || !fills[0].routedToKeyed() {
		t.Fatalf("pool-fill routing = %q (keyed=%t), want the key handed to the pool-allocation admission",
			fills[0].AdmissionOutcome, fills[0].routedToKeyed())
	}
	if len(sink.keys) != 1 || sink.keys[0] != poolFillEntry("wk-1") {
		t.Fatalf("pool-allocation keys = %+v, want exactly the routed work's exact key", sink.keys)
	}
}

// TestDetectorNeverFillsPoolAtOrAboveMin is the first negative: a pool already
// at its desired count is never enqueued and starts nothing, however much routed
// work is waiting. The open member is counted the way the pool planner counts it
// — a member in creating/start-pending has already spent the demand.
func TestDetectorNeverFillsPoolAtOrAboveMin(t *testing.T) {
	sink := &recordingPoolAllocationSink{}
	in := poolFillSweepInput(
		[]readyRoutedWorkEntry{poolFillEntry("wk-1"), poolFillEntry("wk-2")},
		map[string]int{"worker": 1},
		[]sessionpkg.ReconcileSession{{Info: sessionpkg.Info{
			ID:                  "gc-open",
			Template:            "worker",
			SessionNameMetadata: "worker-gc-open",
			MetadataState:       string(sessionpkg.StateStartPending),
			PoolManaged:         true,
		}}},
		sink,
	)

	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	if fills := poolFillConditions(result); len(fills) != 0 {
		t.Fatalf("pool-fill conditions for a filled pool = %+v, want none", fills)
	}
	if len(sink.keys) != 0 {
		t.Fatalf("pool-allocation keys for a filled pool = %+v, want none", sink.keys)
	}
}

// TestDetectorPoolFillNeverOverfillsOneSweep pins that a pool short by ONE never
// gets two members from one sweep just because two work items are waiting: the
// arm accrues its own fills against the desired count as it goes.
func TestDetectorPoolFillNeverOverfillsOneSweep(t *testing.T) {
	sink := &recordingPoolAllocationSink{}
	in := poolFillSweepInput(
		[]readyRoutedWorkEntry{poolFillEntry("wk-1"), poolFillEntry("wk-2"), poolFillEntry("wk-3")},
		map[string]int{"worker": 2},
		nil,
		sink,
	)

	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	if len(sink.keys) != 2 {
		t.Fatalf("pool-allocation keys = %+v, want exactly the two the desired count is short by", sink.keys)
	}
}

// TestDetectorPoolFillOverflowIsCensusOwedAndRefills is the second negative and
// Q2's whole degradation contract: a saturated pool-allocation channel drops the
// hint, the sweep records the overflow and returns without retrying or blocking,
// no legacy fallback is invoked, and the NEXT sweep re-detects the same durable
// condition and fills it. Work is preserved; only latency is lost.
func TestDetectorPoolFillOverflowIsCensusOwedAndRefills(t *testing.T) {
	saturated := &recordingPoolAllocationSink{saturte: true}
	in := poolFillSweepInput(
		[]readyRoutedWorkEntry{poolFillEntry("wk-1")},
		map[string]int{"worker": 1},
		nil,
		saturated,
	)

	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	fills := poolFillConditions(result)
	if len(fills) != 1 || fills[0].AdmissionOutcome != detectorAdmissionRefusedOverflow {
		t.Fatalf("saturated-channel routing = %+v, want one traced overflow refusal", fills)
	}
	if len(saturated.keys) != 0 {
		t.Fatalf("saturated channel accepted %+v; the drop must not be retried", saturated.keys)
	}
	if fills[0].routedToKeyed() {
		t.Fatal("a dropped key was recorded as keyed-owned; nobody owns it until it is re-detected")
	}

	// The condition is level-triggered off durable state, so the next sweep
	// re-detects the same key and fills it. That IS the recovery — there is no
	// retry loop and no legacy poke anywhere on this path.
	drained := &recordingPoolAllocationSink{}
	next := poolFillSweepInput(
		[]readyRoutedWorkEntry{poolFillEntry("wk-1")},
		map[string]int{"worker": 1},
		nil,
		drained,
	)
	nextResult := detectSessionConditions(context.Background(), next)
	routeDetectorConditions(next, &nextResult)
	if len(drained.keys) != 1 || drained.keys[0].WorkID != "wk-1" {
		t.Fatalf("re-detected keys = %+v, want the dropped key filled on the next sweep", drained.keys)
	}
}

// TestDetectorPoolFillRefusesWithoutAnAdmissionSeam pins the traced refusal for a
// city whose session-keyed families route but whose pool-allocation admission is
// unavailable: the FILL arm records refused_uncertifiable and enqueues nothing,
// whatever the act constant says. (A call site with NO sink at all -- `gc start`,
// the control dispatcher -- never reaches the seam and stays fully read-only.)
func TestDetectorPoolFillRefusesWithoutAnAdmissionSeam(t *testing.T) {
	in := poolFillSweepInput(
		[]readyRoutedWorkEntry{poolFillEntry("wk-1")},
		map[string]int{"worker": 1},
		nil,
		nil,
	)
	in.Admit = func(string, sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error) {
		return sessionStartAdmissionAccepted, nil
	}

	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	fills := poolFillConditions(result)
	if len(fills) != 1 || fills[0].AdmissionOutcome != detectorAdmissionRefusedUncertifiable {
		t.Fatalf("seam-less routing = %+v, want one traced uncertifiable refusal", fills)
	}
}

// TestDetectorPoolFillStartsARealSession is the AC's started-session upgrade of
// the request-count-only legacy pool assertion: the key the sweep produced,
// driven through the unchanged pool-allocation handler, materializes a real
// member with a new instance token and a live runtime.
func TestDetectorPoolFillStartsARealSession(t *testing.T) {
	isolateKeyedRoutedWorkAllocations(t)
	fixture := newRoutedWorkPoolAllocationFixture(t, beads.NewMemStore())
	work, err := fixture.store.Create(beads.Bead{
		Title:    "routed work for an empty pool",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create routed work: %v", err)
	}

	sink := &recordingPoolAllocationSink{}
	in := poolFillSweepInput(
		[]readyRoutedWorkEntry{{SourceStore: "city:test-city", WorkID: work.ID, PoolTarget: "worker", Status: "open", Type: "task"}},
		map[string]int{"worker": 1},
		nil,
		sink,
	)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	if len(sink.keys) != 1 {
		t.Fatalf("sweep produced %+v, want exactly one fill key", sink.keys)
	}

	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), routedWorkPoolAllocationHint{
		WorkID:      sink.keys[0].WorkID,
		PoolTarget:  sink.keys[0].PoolTarget,
		SourceStore: sink.keys[0].SourceStore,
	})

	snapshot, err := loadSessionBeadSnapshot(fixture.store)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}
	open := snapshot.OpenInfos()
	if len(open) != 1 {
		t.Fatalf("open sessions after the keyed fill = %d, want exactly one member; stderr=%s", len(open), fixture.stderr.String())
	}
	member := open[0]
	if member.InstanceToken == "" {
		t.Fatalf("filled member %+v carries no instance token", member)
	}
	if member.TriggerBeadID != work.ID {
		t.Fatalf("filled member trigger = %q, want the routed work %q", member.TriggerBeadID, work.ID)
	}
	awaitCond(t, func() bool {
		return fixture.provider.CountCalls("Start", member.SessionName) == 1
	}, "the filled pool member to reach a live runtime")
}

func poolFillConditions(result detectorSweepResult) []detectorCondition {
	var fills []detectorCondition
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyWake && cond.Reason == detectorReasonWakePoolFill {
			fills = append(fills, cond)
		}
	}
	return fills
}

// TestDetectorRefusesQuarantinedWakeTargetWithATrace is delta-8 arm 3: legacy
// drops a wake target inside a live quarantine window with NO trace record at
// all, so the parity join sees a wake that never happened and nothing saying
// why. The detector records the blocker and enqueues nothing. It cannot
// double-act -- legacy already skips the row and the keyed admission chain
// blocks it again at the handler -- so the refusal is a non-action on both
// sides.
func TestDetectorRefusesQuarantinedWakeTargetWithATrace(t *testing.T) {
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		quarantined string
		held        string
		pinAwake    string
		wakeReason  string
		wantBlocker string
	}{
		{name: "quarantine", quarantined: now.Add(time.Hour).Format(time.RFC3339), wantBlocker: "quarantine"},
		{name: "hold", held: now.Add(time.Hour).Format(time.RFC3339), wantBlocker: "user_hold"},
		{name: "expired quarantine routes", quarantined: now.Add(-time.Hour).Format(time.RFC3339)},
		{
			// The pin is not a blocker HERE, and refusing on it inverts what a
			// pin means. This guard was written when the only blockers were
			// user_hold and quarantine, both of which ComputeAwakeSet forces
			// ShouldWake=false for, so it was unreachable. ComputeAwakeSet has
			// no pin suppression at all — its durable pin override SETS
			// ShouldWake=true with Reason="pin" for an asleep pinned row — so
			// ShouldWake && blocker=="pinned" is reachable for exactly the
			// pin-revive case the override exists to serve. Refusing it turns
			// `gc session pin` from "always keep awake" into "never restart".
			name: "pin revives rather than blocks", pinAwake: "true", wakeReason: "pin",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			info := sessionpkg.Info{
				ID:                  "gc-held",
				Template:            "worker",
				SessionNameMetadata: "worker",
				MetadataState:       string(sessionpkg.StateAsleep),
				QuarantinedUntil:    test.quarantined,
				HeldUntil:           test.held,
				PinAwake:            test.pinAwake,
			}
			base := detectorCondition{SessionID: info.ID, SessionName: info.SessionNameMetadata, Template: "worker"}
			emit := newDetectorConditionSink(false)
			in := poolFillSweepInput(nil, nil, nil, nil)
			wakeReason := test.wakeReason
			if wakeReason == "" {
				wakeReason = "assigned-work"
			}
			awake := map[string]AwakeDecision{"worker": {ShouldWake: true, Reason: wakeReason}}

			detectWakeOrSleep(in, emit, base, info, awake, nil, detectorLivenessBits{}, nil, &clock.Fake{Time: now})

			if len(emit.conditions) != 1 {
				t.Fatalf("wake conditions = %+v, want exactly one", emit.conditions)
			}
			cond := emit.conditions[0]
			if test.wantBlocker == "" {
				if cond.Outcome != TraceOutcomeStartCandidate {
					t.Fatalf("expired blocker condition = %+v, want an ordinary start candidate", cond)
				}
				return
			}
			if cond.Reason != detectorReasonWakeBlocked || cond.Outcome != TraceOutcomeSkipped {
				t.Fatalf("blocked wake condition = %+v, want a traced refusal", cond)
			}
			if cond.Fields["blocker"] != test.wantBlocker {
				t.Fatalf("recorded blocker = %v, want %q", cond.Fields["blocker"], test.wantBlocker)
			}
			if _, routed := detectorAdmissionSourceFor(cond); routed {
				t.Fatalf("a blocked wake target was routed: %+v", cond)
			}
		})
	}
}
