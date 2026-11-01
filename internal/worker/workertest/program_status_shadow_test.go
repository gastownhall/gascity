package workertest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	worker "github.com/gastownhall/gascity/internal/worker"
)

func init() {
	programStatusChecks[RequirementProgramStatusShadowIdentical] = checkShadowIdentical
}

// shadowWorkerDecisions are the worker decisions WC-PSTAT-007 compares: a
// session handle's phase, and its wait-idle, default and immediate nudges;
// a runtime-only handle's wait-idle nudge.
type shadowWorkerDecisions struct {
	phase                         worker.Phase
	waitIdle, deflt, immediate    worker.NudgeResult
	waitIdleErr, defltErr, immErr string
	runtimeWaitIdle               worker.NudgeResult
	runtimeWaitIdleErr            string
}

// shadowWorkerRow is an armed agent-pane row under mode whose program
// reports a live working root: under shadow the record disagrees with the
// fake's idle screen at every point that reads it.
func shadowWorkerRow(mode runtime.ProgramStatusMode) runtime.ProgramPaneRow {
	return runtime.ProgramPaneRow{
		Mode: mode, CaptureGate: true, PaneID: "%2", PanePipe: true, Tap: "w1.%2",
		TapLiveness: runtime.ProgramTapAlive, RecordForm: runtime.ProgramRecordDecoded,
		Record: runtime.ProgramStatus{
			State: runtime.ProgramWorking, Nonce: "w1.%2", Seq: 2, TurnStarts: 1,
			Progress: -1, Reported: true, Since: time.Now(),
		},
	}
}

// errText is err's text, "" for none.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// runShadowWorkerDecisions takes every compared decision on a tmux-profiled
// fake whose switch reads mode; under shadow its leaf hands out a sink, and
// the comparisons it receives are counted.
func runShadowWorkerDecisions(t *testing.T, mode runtime.ProgramStatusMode) (shadowWorkerDecisions, int) {
	t.Helper()
	ctx := context.Background()
	fake := runtime.NewFake()
	var mu sync.Mutex
	comparisons := 0
	if mode == runtime.ProgramStatusShadow {
		fake.ProgramShadowSink = func(runtime.ProgramStatusComparison) {
			mu.Lock()
			comparisons++
			mu.Unlock()
		}
	}
	sp := runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: fake})
	h, _ := turnConformanceSessionWithStore(t, sp, func(name string) { fake.SetProgramPane(name, shadowWorkerRow(mode)) }, nil)
	var d shadowWorkerDecisions
	state, err := h.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	d.phase = state.Phase
	by := sessionpkg.Actor{Kind: sessionpkg.ActorAgent}
	var nerr error
	d.waitIdle, nerr = h.Nudge(ctx, by, worker.NudgeRequest{Text: "hello", Delivery: worker.NudgeDeliveryWaitIdle})
	d.waitIdleErr = errText(nerr)
	d.deflt, nerr = h.Nudge(ctx, by, worker.NudgeRequest{Text: "hello"})
	d.defltErr = errText(nerr)
	d.immediate, nerr = h.Nudge(ctx, by, worker.NudgeRequest{Text: "hello", Delivery: worker.NudgeDeliveryImmediate})
	d.immErr = errText(nerr)

	rh := turnConformanceRuntimeHandle(t, sp)
	fake.SetProgramPane("turn-worker", shadowWorkerRow(mode))
	d.runtimeWaitIdle, nerr = rh.Nudge(ctx, by, worker.NudgeRequest{Text: "hello", Delivery: worker.NudgeDeliveryWaitIdle})
	d.runtimeWaitIdleErr = errText(nerr)

	mu.Lock()
	defer mu.Unlock()
	return d, comparisons
}

// checkShadowIdentical is WC-PSTAT-007's check: with the switch at shadow,
// and the program's record disagreeing with the screen, every worker
// decision is the one the switch off makes, and the shadow records its
// comparisons.
func checkShadowIdentical(t *testing.T, profile Profile) Result {
	off, offComparisons := runShadowWorkerDecisions(t, runtime.ProgramStatusOff)
	shadow, comparisons := runShadowWorkerDecisions(t, runtime.ProgramStatusShadow)
	evidence := map[string]string{"comparisons": fmt.Sprint(comparisons)}
	switch {
	case off != shadow:
		return Fail(profile.ID, RequirementProgramStatusShadowIdentical,
			fmt.Sprintf("decisions differ: off %+v, shadow %+v", off, shadow)).WithEvidence(evidence)
	case offComparisons != 0:
		return Fail(profile.ID, RequirementProgramStatusShadowIdentical,
			fmt.Sprintf("the switch off recorded %d comparisons", offComparisons)).WithEvidence(evidence)
	case comparisons == 0:
		return Fail(profile.ID, RequirementProgramStatusShadowIdentical,
			"the shadow recorded no comparison, so it compared nothing").WithEvidence(evidence)
	}
	return Pass(profile.ID, RequirementProgramStatusShadowIdentical,
		"shadow decisions identical to off").WithEvidence(evidence)
}

// TestProgramStatusShadowIdenticalCheck runs WC-PSTAT-007's check for every
// profile now, whatever it declares: the row's property is the worker's,
// which no declaration gates.
func TestProgramStatusShadowIdenticalCheck(t *testing.T) {
	for _, profile := range Phase1Profiles() {
		t.Run(string(profile.ID), func(t *testing.T) {
			if r := checkShadowIdentical(t, profile); !r.Passed() {
				t.Fatalf("%s: %s %v", r.Requirement, r.Detail, r.Evidence)
			}
		})
	}
}
