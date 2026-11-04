package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// holdLeaf is a running fake session "s", quiet for an hour, whose agent
// pane reads a record in st entered at since under mode, and declares the
// mid-turn hold when declared.
func holdLeaf(t *testing.T, mode runtime.ProgramStatusMode, st runtime.ProgramState, since time.Time, declared bool) (*runtime.Fake, runtime.FullFake) {
	t.Helper()
	f := runtime.NewFake()
	if err := f.Start(context.Background(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	f.Activity = map[string]time.Time{"s": time.Now().Add(-time.Hour)}
	f.WaitForIdleErrors = map[string]error{"s": nil}
	f.SetProgramPane("s", holdPaneRow(mode, st, since))
	f.SetProgramHold("s", declared)
	return f, runtime.FullFake{Fake: f}
}

// holdPaneRow is an armed agent-pane row under mode whose record is in st,
// entered at since.
func holdPaneRow(mode runtime.ProgramStatusMode, st runtime.ProgramState, since time.Time) runtime.ProgramPaneRow {
	return runtime.ProgramPaneRow{
		Mode: mode, CaptureGate: true, PaneID: "%3", PanePipe: true, Tap: "r1.%3",
		TapLiveness: runtime.ProgramTapAlive, RecordForm: runtime.ProgramRecordDecoded,
		Record: runtime.ProgramStatus{State: st, Nonce: "r1.%3", Seq: 2, TurnStarts: 1, Progress: -1, Reported: true, Since: since},
	}
}

// holdTarget is a nudge target for session "s" under the city switch mode,
// on a provider whose program_status declares capture and, when hold is
// set, the mid-turn hold.
func holdTarget(mode string, hold bool) nudgeTarget {
	binding := config.ProgramStatusBinding{Protocol: config.ProgramStatusProtocolOSC7501, App: "agent"}
	if hold {
		binding.HoldMidTurn = &hold
	}
	return nudgeTarget{
		sessionName: "s",
		sessionID:   "gc-1",
		cfg:         &config.City{Session: config.SessionConfig{ProgramStatus: mode}},
		resolved:    &config.ResolvedProvider{Name: "agent", ProgramStatus: binding},
	}
}

// quietObs is an observation an hour quiet, which the quiescence rule
// delivers to.
func quietObs() worker.LiveObservation {
	stale := time.Now().Add(-time.Hour)
	return worker.LiveObservation{LastActivity: &stale}
}

// holdReads counts the fresh hold reads f took.
func holdReads(f *runtime.Fake) int {
	n := 0
	for _, c := range f.Calls {
		if c.Method == "ProgramStatusHoldRead" {
			n++
		}
	}
	return n
}

// Kills a queued delivery that is claimed into a running turn of a program
// that aborts on mid-turn input: under on, a fresh live working or blocked
// record closes the gate a quiet session would open, before any starvation
// bypass, and holds the conversation lane's idle reading too.
func TestQueuedDeliveryHeldWhileTheRecordSaysWorking(t *testing.T) {
	for _, st := range []runtime.ProgramState{runtime.ProgramWorking, runtime.ProgramBlocked} {
		t.Run(string(st), func(t *testing.T) {
			f, sp := holdLeaf(t, runtime.ProgramStatusOn, st, time.Now(), true)
			target := holdTarget("on", true)
			gate, err := queuedNudgeGateFor(target, sp, time.Second, quietObs(), time.Now())
			if err != nil || gate.open {
				t.Fatalf("gate = %+v, %v; want closed with no error", gate, err)
			}
			if pollerSessionIdleEnough(target, sp, time.Second, quietObs()) {
				t.Fatal("pollerSessionIdleEnough read a held session idle enough")
			}
			if holdReads(f) != 2 {
				t.Fatalf("%d hold reads, want one per decision", holdReads(f))
			}
		})
	}
}

// Kills a hold that trusts the state cache's snapshot: the snapshot still
// says working, the pane's record already says idle, and the delivery goes.
func TestQueuedDeliveryReleasedByAFreshIdleRecord(t *testing.T) {
	f, sp := holdLeaf(t, runtime.ProgramStatusOn, runtime.ProgramIdle, time.Now(), true)
	f.SetProgramCachedPane("s", holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramWorking, time.Now()))
	gate, err := queuedNudgeGateFor(holdTarget("on", true), sp, time.Second, quietObs(), time.Now())
	if err != nil || !gate.open {
		t.Fatalf("gate = %+v, %v; want open on the fresh idle record", gate, err)
	}
	if holdReads(f) != 1 {
		t.Fatalf("%d hold reads, want one", holdReads(f))
	}
}

// Kills a hold that outlives its bound: a working record no activity
// refreshed for longer than the hold limit holds nothing.
func TestQueuedDeliveryHoldIsBounded(t *testing.T) {
	_, sp := holdLeaf(t, runtime.ProgramStatusOn, runtime.ProgramWorking, time.Now().Add(-runtime.ProgramStatusHoldLimit-time.Minute), true)
	gate, err := queuedNudgeGateFor(holdTarget("on", true), sp, time.Second, quietObs(), time.Now())
	if err != nil || !gate.open {
		t.Fatalf("gate = %+v, %v; want open past the hold bound", gate, err)
	}
}

// Kills a hold that reaches a provider that does not declare it, a pane
// that does not, or a switch that is not on: each delivers as today and,
// but for a declared pane under on, takes no hold read at all.
func TestQueuedDeliveryUnheldWithoutTheDeclarationOrSwitch(t *testing.T) {
	for _, c := range []struct {
		name         string
		mode         runtime.ProgramStatusMode
		city         string
		providerHold bool
		paneHold     bool
		reads        int
	}{
		{"provider undeclared", runtime.ProgramStatusOn, "on", false, true, 0},
		{"pane undeclared", runtime.ProgramStatusOn, "on", true, false, 1},
		{"switch shadow", runtime.ProgramStatusShadow, "shadow", true, true, 0},
		{"switch off", runtime.ProgramStatusOff, "off", true, true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, sp := holdLeaf(t, c.mode, runtime.ProgramWorking, time.Now(), c.paneHold)
			gate, err := queuedNudgeGateFor(holdTarget(c.city, c.providerHold), sp, time.Second, quietObs(), time.Now())
			if err != nil || !gate.open {
				t.Fatalf("gate = %+v, %v; want open as today", gate, err)
			}
			if holdReads(f) != c.reads {
				t.Fatalf("%d hold reads, want %d", holdReads(f), c.reads)
			}
		})
	}
}
