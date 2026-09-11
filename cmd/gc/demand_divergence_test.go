package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/pkg/eventexport"
)

// T-G / T-E: a demand-spawned seat that drains empty is either the agreement
// invariant breaking or pull working correctly, and only a classification read
// after the fact can say which. These rows pin that the diagnostics never touch
// the drain they report on.

func divergenceOptions(env ...string) hookClaimOptions {
	return hookClaimOptions{
		Assignee:           "gc__worker-1",
		IdentityCandidates: []string{"gc__worker-1"},
		RouteTargets:       []string{"rig/worker"},
		Env: append([]string{
			"GC_SESSION_ID=gcs-1",
			"GC_SESSION_NAME=gc__worker-1",
			"GC_TEMPLATE=rig/worker",
		}, env...),
		DrainAck: true,
		JSON:     true,
	}
}

// demandSpawnEnv is the env a seat the controller minted from counted demand
// carries: the presence-only origin marker plus the trigger id the diagnostics
// classify against.
const demandSpawnTriggerID = "wb-1"

func demandSpawnEnv() []string {
	return []string{"GC_SPAWN_ORIGIN=demand", "GC_TRIGGER_WORK_BEAD_ID=" + demandSpawnTriggerID}
}

// captureDivergence swaps the emitter seam and records what it was called with,
// plus whether the drain result had already been written by then.
type divergenceCapture struct {
	calls     int
	reason    string
	stdoutAt  string
	classArgs []hookClaimOps
}

func captureDivergenceEmitter(t *testing.T, stdout *bytes.Buffer) *divergenceCapture {
	t.Helper()
	capture := &divergenceCapture{}
	prev := hookRecordDemandClaimDivergence
	hookRecordDemandClaimDivergence = func(reason, _ string, _ hookClaimOptions, ops hookClaimOps, _ io.Writer) {
		capture.calls++
		capture.reason = reason
		capture.stdoutAt = stdout.String()
		capture.classArgs = append(capture.classArgs, ops)
	}
	t.Cleanup(func() { hookRecordDemandClaimDivergence = prev })
	return capture
}

// TestDivergenceIsRecordedOnlyAfterTheDrainResult pins the ordering that makes
// this safe to ship: by the time the emitter runs, the drain result is already
// on stdout and the exit code is already decided.
func TestDivergenceIsRecordedOnlyAfterTheDrainResult(t *testing.T) {
	var stdout, stderr bytes.Buffer
	capture := captureDivergenceEmitter(t, &stdout)
	opts := divergenceOptions(demandSpawnEnv()...)

	code := writeHookClaimNoWork(opts, hookClaimOps{DrainAck: func(io.Writer) error { return nil }},
		false, "/rig", &stdout, &stderr)

	if code != 0 {
		t.Fatalf("drain exit = %d, want 0", code)
	}
	if capture.calls != 1 {
		t.Fatalf("divergence emitter calls = %d, want 1", capture.calls)
	}
	if !strings.Contains(capture.stdoutAt, `"action":"drain"`) {
		t.Fatalf("stdout at emit time = %q, want the drain result already written", capture.stdoutAt)
	}
	if capture.reason != hookClaimReasonNoWork {
		t.Fatalf("reported reason = %q, want %q", capture.reason, hookClaimReasonNoWork)
	}
}

// Control: a claims_errored drain is a write failure, not a demand/claim
// disagreement, so the counter must not fire for it.
func TestDivergenceIsNotRecordedForAClaimsErroredDrain(t *testing.T) {
	var stdout, stderr bytes.Buffer
	capture := captureDivergenceEmitter(t, &stdout)
	opts := divergenceOptions(demandSpawnEnv()...)

	writeHookClaimNoWork(opts, hookClaimOps{DrainAck: func(io.Writer) error { return nil }},
		true, "/rig", &stdout, &stderr)

	if capture.calls != 0 {
		t.Fatalf("divergence emitter calls = %d, want 0 for a claims_errored drain", capture.calls)
	}
}

// TestDivergenceClassification is the verdict table. Only a row that is STILL
// claimable counts as divergence; everything else is pull working.
func TestDivergenceClassification(t *testing.T) {
	routed := map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"}
	for _, tt := range []struct {
		name      string
		bead      beads.Bead
		readErr   error
		triggerID string
		wantClass string
		wantStat  string
	}{
		{
			name: "still open and claimable", triggerID: "wb-1",
			bead:      beads.Bead{ID: "wb-1", Status: "open", Type: "task", Metadata: routed},
			wantClass: events.DemandClaimDivergence, wantStat: "open",
		},
		{
			name: "claimed by a sibling", triggerID: "wb-1",
			bead:      beads.Bead{ID: "wb-1", Status: "in_progress", Type: "task", Assignee: "gc__worker-2", Metadata: routed},
			wantClass: events.DemandClaimBenign, wantStat: "in_progress",
		},
		{
			name: "closed", triggerID: "wb-1",
			bead:      beads.Bead{ID: "wb-1", Status: "closed", Type: "task", Metadata: routed},
			wantClass: events.DemandClaimBenign, wantStat: "closed",
		},
		{
			name: "open but held", triggerID: "wb-1",
			bead: beads.Bead{
				ID: "wb-1", Status: "open", Type: "task", Metadata: routed,
				Labels: []string{beadmeta.DispatchHoldLabels[0]},
			},
			wantClass: events.DemandClaimBenign, wantStat: "open",
		},
		{
			name: "unreadable", triggerID: "wb-1", readErr: errors.New("store read failed"),
			wantClass: events.DemandClaimUnknown, wantStat: "unreadable",
		},
		{
			name: "no trigger recorded", triggerID: "",
			wantClass: events.DemandClaimUnknown, wantStat: "",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := divergenceOptions()
			ops := demandDivergenceOpsForBead(tt.bead, tt.readErr)
			status, class := classifyDemandTrigger(tt.triggerID, "/rig", opts, ops)
			if class != tt.wantClass {
				t.Errorf("classification = %q, want %q", class, tt.wantClass)
			}
			if status != tt.wantStat {
				t.Errorf("status = %q, want %q", status, tt.wantStat)
			}
		})
	}
}

// T-E: the sibling race is correct pull. The loser drains, acks, is reaped, and
// its diagnostics say benign — nothing here may make that look like a defect.
func TestSiblingRaceLoserClassifiesBenign(t *testing.T) {
	rec := events.NewFake()
	prev := demandDivergenceRecorder
	demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
	t.Cleanup(func() { demandDivergenceRecorder = prev })

	opts := divergenceOptions(demandSpawnEnv()...)
	winner := beads.Bead{
		ID: "wb-1", Status: "in_progress", Type: "task", Assignee: "gc__worker-2",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	}

	var stderr bytes.Buffer
	recordDemandClaimDivergence(hookClaimReasonNoWork, "/rig", opts, demandDivergenceOpsForBead(winner, nil), &stderr)

	if len(rec.Events) != 1 {
		t.Fatalf("events = %d, want exactly 1", len(rec.Events))
	}
	var payload events.SessionDemandClaimDivergencePayload
	if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if payload.Classification != events.DemandClaimBenign {
		t.Fatalf("classification = %q, want benign: losing a race to a sibling is correct pull", payload.Classification)
	}
	if payload.TriggerBeadID != "wb-1" || payload.SessionID != "gcs-1" || payload.Template != "rig/worker" {
		t.Fatalf("payload = %+v, want the seat and its trigger named", payload)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want silence for a benign classification", stderr.String())
	}
}

// The divergence case is the one that gets an operator-facing line, because it
// is the invariant breaking rather than the system working.
func TestDivergenceCaseIsReportedToTheWorkerStderr(t *testing.T) {
	rec := events.NewFake()
	prev := demandDivergenceRecorder
	demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
	t.Cleanup(func() { demandDivergenceRecorder = prev })

	opts := divergenceOptions(demandSpawnEnv()...)
	stillThere := beads.Bead{
		ID: "wb-1", Status: "open", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	}

	var stderr bytes.Buffer
	recordDemandClaimDivergence(hookClaimReasonNoWork, "/rig", opts, demandDivergenceOpsForBead(stillThere, nil), &stderr)

	if len(rec.Events) != 1 || rec.Events[0].Message != events.DemandClaimDivergence {
		t.Fatalf("events = %+v, want one divergence-classified event", rec.Events)
	}
	if !strings.Contains(stderr.String(), "wb-1") {
		t.Fatalf("stderr = %q, want the still-claimable row named", stderr.String())
	}
}

// Control: a seat the controller did NOT spawn from demand evidence produces no
// event at all. A named session or a manual hook draining empty says nothing
// about the agreement invariant, and counting it would poison the metric.
func TestNonDemandSpawnedSeatRecordsNoDivergence(t *testing.T) {
	rec := events.NewFake()
	prev := demandDivergenceRecorder
	demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
	t.Cleanup(func() { demandDivergenceRecorder = prev })

	var stderr bytes.Buffer
	// Same trigger id, no spawn-origin marker.
	opts := divergenceOptions("GC_TRIGGER_WORK_BEAD_ID=wb-1")
	stillThere := beads.Bead{
		ID: "wb-1", Status: "open", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	}

	recordDemandClaimDivergence(hookClaimReasonNoWork, "/rig", opts, demandDivergenceOpsForBead(stillThere, nil), &stderr)

	if len(rec.Events) != 0 {
		t.Fatalf("events = %+v, want none for a seat not spawned from demand", rec.Events)
	}
}

// The egress decision, pinned rather than implied — same treatment as
// execution.step_stalled. This is an internal diagnostics counter carrying a
// bead ref and a template name; widening a redaction boundary is its own
// reviewable change.
func TestDemandClaimDivergenceStaysOffTheExportAllowlist(t *testing.T) {
	if eventexport.IsAllowed(events.SessionDemandClaimDivergence) {
		t.Fatal("session.demand_claim_divergence is on the redacted-export allowlist; that is an egress-surface change and needs its own review")
	}
}
