package main

import (
	"bytes"
	"context"
	"encoding/json"
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
// carries: the presence-only origin marker and no trigger id. The diagnostics
// read the trigger off the session row (ReadSessionTrigger), never the shell.
func demandSpawnEnv() []string {
	return []string{"GC_SPAWN_ORIGIN=demand"}
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

	code := writeHookClaimNoWork(opts, hookClaimOps{DrainAck: func(io.Writer) error { return nil }, ReadWorkStarted: noStartedClaims},
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

	writeHookClaimNoWork(opts, hookClaimOps{DrainAck: func(io.Writer) error { return nil }, ReadWorkStarted: noStartedClaims},
		true, "/rig", &stdout, &stderr)

	if capture.calls != 0 {
		t.Fatalf("divergence emitter calls = %d, want 0 for a claims_errored drain", capture.calls)
	}
}

// TestDivergenceClassification is the verdict table. Only a row that is STILL
// claimable counts as divergence; everything else is pull working.
func TestDivergenceClassification(t *testing.T) {
	routed := map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"}
	blockedTrue := true
	future := time.Now().Add(time.Hour)
	for _, tt := range []struct {
		name       string
		bead       beads.Bead
		readErr    error
		blocked    bool
		confirmErr error
		triggerID  string
		wantClass  string
		wantStat   string
	}{
		{
			// The PRODUCTION shape: bd's payloads never carry is_blocked, so the
			// projection is absent and the live-dep settlement is what decides. No
			// unmet plain `blocks` edge — the row really is claimable, and this is
			// the agreement invariant breaking.
			name: "still open and claimable", triggerID: "wb-1",
			bead:      beads.Bead{ID: "wb-1", Status: "open", Type: "task", Metadata: routed},
			wantClass: events.DemandClaimDivergence, wantStat: "open",
		},
		{
			// Same absent projection, but the live-dep settlement finds an unmet
			// plain `blocks` edge. No worker could have claimed it, so it is not a
			// divergence — and not folded into benign either. Absent is what
			// production reads return, so a row can only reach this bucket at all
			// because the settlement no longer depends on the projection.
			name: "no projection but dep-confirmed blocked", triggerID: "wb-1",
			bead:      beads.Bead{ID: "wb-1", Status: "open", Type: "task", Metadata: routed},
			blocked:   true,
			wantClass: events.DemandClaimBlocked, wantStat: "open",
		},
		{
			// bd's denormalized projection reads true, but the live deps are MET —
			// the stale-true reading `bd recompute-blocked` exists to repair. The
			// row is claimable, so burying it would hide a real divergence: the
			// settlement, not the flag, has the last word.
			name: "stale is_blocked projection with met deps", triggerID: "wb-1",
			bead:      beads.Bead{ID: "wb-1", Status: "open", Type: "task", Metadata: routed, IsBlocked: &blockedTrue},
			wantClass: events.DemandClaimDivergence, wantStat: "open",
		},
		{
			// The settlement's dependency read failed. An unreadable store is not
			// evidence either way, and must never be able to manufacture the
			// divergence metric.
			name: "blockedness settlement read fails", triggerID: "wb-1",
			bead:       beads.Bead{ID: "wb-1", Status: "open", Type: "task", Metadata: routed},
			confirmErr: errors.New("dependency read failed"),
			wantClass:  events.DemandClaimUnknown, wantStat: "open",
		},
		{
			// Open and route-matching but deferred into the future: also excluded
			// by the ready query, so also not a divergence.
			name: "open but deferred", triggerID: "wb-1",
			bead:      beads.Bead{ID: "wb-1", Status: "open", Type: "task", Metadata: routed, DeferUntil: &future},
			wantClass: events.DemandClaimBenign, wantStat: "open",
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
			ops := demandDivergenceOpsForBlockedBead(tt.bead, tt.readErr, tt.blocked, tt.confirmErr)
			status, class := classifyDemandTrigger(demandTrigger{ID: tt.triggerID, StoreRef: demandDivergenceTestRigRef}, opts, ops)
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

// A routed row bd itself parked with status=blocked reads back as "open" (bd's
// raw status survives only in SourceStatus), so it used to fall through to the
// unproven-settlement path and classify "divergence" when it has no unmet blocks
// edge. The seat's ready query excludes that row whatever its dependencies, so
// it was never claimable: the right bucket is blocked, reached without any
// dependency read, and with no "demand/claim divergence" line on the worker's
// stderr.
func TestDivergenceStatusBlockedRowIsBlockedWithoutSettlement(t *testing.T) {
	routed := map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"}
	blockedTrue, blockedFalse := true, false
	for _, tt := range []struct {
		name      string
		isBlocked *bool
	}{
		{name: "no projection", isBlocked: nil},
		{name: "projection false", isBlocked: &blockedFalse},
		{name: "projection true", isBlocked: &blockedTrue},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := events.NewFake()
			prev := demandDivergenceRecorder
			demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
			t.Cleanup(func() { demandDivergenceRecorder = prev })

			bead := beads.Bead{
				ID: "wb-1", Status: "open", SourceStatus: "blocked", Type: "task",
				Metadata: routed, IsBlocked: tt.isBlocked,
			}
			ops := demandDivergenceOpsForBlockedBead(bead, nil, false, nil)
			ops.ConfirmBlocked = func(context.Context, string, []string, string, string) (bool, error) {
				t.Error("ConfirmBlocked called: a status-blocked row is proof, not a question for the dependency settlement")
				return false, nil
			}

			var stderr bytes.Buffer
			recordDemandClaimDivergence(hookClaimReasonNoWork, "/rig", divergenceOptions(demandSpawnEnv()...), ops, &stderr)

			if len(rec.Events) != 1 || rec.Events[0].Message != events.DemandClaimBlocked {
				t.Fatalf("events = %+v, want one blocked-classified event", rec.Events)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want silence: the row was never claimable", stderr.String())
			}
		})
	}

	// Controls: a row that is also held or deferred keeps its benign bucket, and
	// the same row without the recorded status still settles to divergence.
	future := time.Now().Add(time.Hour)
	for _, tt := range []struct {
		name string
		bead beads.Bead
		want string
	}{
		{
			name: "held and status-blocked",
			bead: beads.Bead{ID: "wb-1", Status: "open", SourceStatus: "blocked", Type: "task", Metadata: routed, Labels: []string{beadmeta.DispatchHoldLabels[0]}},
			want: events.DemandClaimBenign,
		},
		{
			name: "deferred and status-blocked",
			bead: beads.Bead{ID: "wb-1", Status: "open", SourceStatus: "blocked", Type: "task", Metadata: routed, DeferUntil: &future},
			want: events.DemandClaimBenign,
		},
		{
			name: "recorded status open",
			bead: beads.Bead{ID: "wb-1", Status: "open", SourceStatus: "open", Type: "task", Metadata: routed},
			want: events.DemandClaimDivergence,
		},
		{
			name: "no recorded status",
			bead: beads.Bead{ID: "wb-1", Status: "open", Type: "task", Metadata: routed},
			want: events.DemandClaimDivergence,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, class := classifyDemandTrigger(demandTrigger{ID: tt.bead.ID, StoreRef: demandDivergenceTestRigRef}, divergenceOptions(), demandDivergenceOpsForBlockedBead(tt.bead, nil, false, nil))
			if class != tt.want {
				t.Errorf("classification = %q, want %q", class, tt.want)
			}
		})
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
	// Same stamped row, no spawn-origin marker.
	opts := divergenceOptions()
	stillThere := beads.Bead{
		ID: "wb-1", Status: "open", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	}

	recordDemandClaimDivergence(hookClaimReasonNoWork, "/rig", opts, demandDivergenceOpsForBead(stillThere, nil), &stderr)

	if len(rec.Events) != 0 {
		t.Fatalf("events = %+v, want none for a seat not spawned from demand", rec.Events)
	}
}

// Kills a classifier that takes the trigger id from the seat's shell: the env
// carries none, or a stale one, and the event still names the row stamp read
// through the session row (gastownhall/gascity#5738).
func TestDivergenceClassifiesTheSessionRowStampNotTheEnv(t *testing.T) {
	stillThere := beads.Bead{
		ID: "wb-1", Status: "open", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	}
	for _, c := range []struct {
		name string
		env  []string
	}{
		{"no trigger id in the env", demandSpawnEnv()},
		{"stale trigger id in the env", append(demandSpawnEnv(), "GC_TRIGGER_WORK_BEAD_ID=wb-env")},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := events.NewFake()
			prev := demandDivergenceRecorder
			demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
			t.Cleanup(func() { demandDivergenceRecorder = prev })

			ops := demandDivergenceOpsForBlockedBead(stillThere, nil, false, nil)
			ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
				if beadID != stillThere.ID {
					return beads.Bead{}, fmt.Errorf("read %s: not the row stamp", beadID)
				}
				return stillThere, nil
			}
			var asked []string
			ops.ReadSessionTrigger = func(sessionID string) (demandTrigger, error) {
				asked = append(asked, sessionID)
				return demandTrigger{ID: stillThere.ID, StoreRef: demandDivergenceTestCityRef}, nil
			}

			var stderr bytes.Buffer
			recordDemandClaimDivergence(hookClaimReasonNoWork, "/rig", divergenceOptions(c.env...), ops, &stderr)

			if len(asked) != 1 || asked[0] != "gcs-1" {
				t.Fatalf("session trigger reads = %v, want one read of gcs-1", asked)
			}
			if len(rec.Events) != 1 {
				t.Fatalf("events = %+v, want exactly 1", rec.Events)
			}
			var payload events.SessionDemandClaimDivergencePayload
			if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
				t.Fatalf("decoding payload: %v", err)
			}
			if payload.TriggerBeadID != "wb-1" || rec.Events[0].Subject != "wb-1" {
				t.Fatalf("payload trigger %q, subject %q; want the row stamp wb-1", payload.TriggerBeadID, rec.Events[0].Subject)
			}
			if payload.Classification != events.DemandClaimDivergence || payload.TriggerStatusAtDrain != "open" {
				t.Fatalf("payload = %+v, want a divergence over the open row", payload)
			}
		})
	}
}

// Kills a failed session-row read that invents a verdict, passes for a row
// with no stamp, or reaches into the drain: the event carries no trigger, an
// "unreadable" status and an unknown verdict, and the drain's stdout and exit
// code match a seat the diagnostics skip entirely.
func TestDivergenceWithAnUnreadableSessionRowIsUnknownAndLeavesTheDrainAlone(t *testing.T) {
	rec := events.NewFake()
	prev := demandDivergenceRecorder
	demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
	t.Cleanup(func() { demandDivergenceRecorder = prev })

	stillThere := beads.Bead{
		ID: "wb-1", Status: "open", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	}
	ops := demandDivergenceOpsForBlockedBead(stillThere, nil, false, nil)
	ops.DrainAck = func(io.Writer) error { return nil }
	ops.ReadWorkStarted = noStartedClaims
	ops.ReadSessionTrigger = func(string) (demandTrigger, error) { return demandTrigger{}, errors.New("session row read failed") }

	var baseOut, baseErr bytes.Buffer
	baseCode := writeHookClaimNoWork(divergenceOptions(), ops, false, "/rig", &baseOut, &baseErr)
	if len(rec.Events) != 0 {
		t.Fatalf("events = %+v, want none for the unmarked baseline", rec.Events)
	}

	var stdout, stderr bytes.Buffer
	code := writeHookClaimNoWork(divergenceOptions(demandSpawnEnv()...), ops, false, "/rig", &stdout, &stderr)

	if code != baseCode || stdout.String() != baseOut.String() {
		t.Fatalf("drain = %d %q, want the unmarked seat's %d %q", code, stdout.String(), baseCode, baseOut.String())
	}
	if len(rec.Events) != 1 {
		t.Fatalf("events = %+v, want exactly 1", rec.Events)
	}
	var payload events.SessionDemandClaimDivergencePayload
	if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if payload.TriggerBeadID != "" || payload.TriggerStatusAtDrain != "unreadable" || payload.Classification != events.DemandClaimUnknown {
		t.Fatalf("payload = %+v, want no trigger, an unreadable status and an unknown verdict", payload)
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

// gastownhall/gascity#5858: the controller counted the trigger in the CITY
// store (open, unassigned, route-matching), but the seat drained in a rig store
// where a same-id row is closed. The classifier must read the trigger in the
// store its demand was counted in — re-reading the seat's own store is
// structurally blind to the cross-store disagreement, and classified all 2,658
// drains of the production loop benign.
func TestDivergenceReadsTheTriggerInTheStoreTheControllerCountedItFrom(t *testing.T) {
	rec := events.NewFake()
	prev := demandDivergenceRecorder
	demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
	t.Cleanup(func() { demandDivergenceRecorder = prev })

	const workerDir = demandDivergenceTestRigDir
	countedInCity := beads.Bead{
		ID: "canary-8a1", Status: "open", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	}
	closedInWorkerStore := countedInCity
	closedInWorkerStore.Status = "closed"

	// The session row (demandDivergenceOpsForBlockedBead's stamp) names the
	// city store; the seat's own leg holds only the closed copy.
	ops := demandDivergenceOpsForBlockedBead(countedInCity, nil, false, nil)
	var readDirs []string
	ops.ReadWorkMeta = func(_ context.Context, dir string, _ []string, beadID, _ string) (beads.Bead, error) {
		readDirs = append(readDirs, dir)
		if beadID != countedInCity.ID {
			return beads.Bead{}, fmt.Errorf("read %s: not the trigger", beadID)
		}
		if dir == workerDir {
			return closedInWorkerStore, nil
		}
		return countedInCity, nil
	}

	var stderr bytes.Buffer
	recordDemandClaimDivergence(hookClaimReasonNoWork, workerDir, divergenceOptions(demandSpawnEnv()...), ops, &stderr)

	if len(rec.Events) != 1 {
		t.Fatalf("events = %+v, want exactly 1", rec.Events)
	}
	var payload events.SessionDemandClaimDivergencePayload
	if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if payload.Classification != events.DemandClaimDivergence {
		t.Fatalf("classification = %q (status %q, trigger reads in dirs %v); want %q: the trigger is still open and claimable in the city store the controller counted from, only the seat's own store says closed",
			payload.Classification, payload.TriggerStatusAtDrain, readDirs, events.DemandClaimDivergence)
	}
	if payload.TriggerStatusAtDrain != "open" {
		t.Fatalf("trigger_status_at_drain = %q, want open: the counting store's copy is the status that matters", payload.TriggerStatusAtDrain)
	}
	if payload.TriggerBeadID != "canary-8a1" {
		t.Fatalf("trigger_bead_id = %q, want canary-8a1", payload.TriggerBeadID)
	}
	if !strings.Contains(stderr.String(), "canary-8a1") || !strings.Contains(stderr.String(), "city") {
		t.Fatalf("stderr = %q, want the row and the counting store named", stderr.String())
	}
}

// The judge's triage repro for gastownhall/gascity#5858, adopted verbatim:
// the trigger is closed in the seat's own store and open, unassigned and
// route-matching in the city store the demand was counted in.
func TestDivergenceReadsTheTriggerInTheCountingStoreNotOnlyTheWorkers(t *testing.T) {
	rec := events.NewFake()
	prev := demandDivergenceRecorder
	demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
	t.Cleanup(func() { demandDivergenceRecorder = prev })

	const workerDir = "/rig"
	countedInCity := beads.Bead{
		ID: "canary-8a1", Status: "open", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	}
	closedInWorkerStore := countedInCity
	closedInWorkerStore.Status = "closed"

	ops := demandDivergenceOpsForBlockedBead(countedInCity, nil, false, nil)
	ops.StoreRef = func(dir string) string {
		if dir == workerDir {
			return "rig:fed-canary"
		}
		return "city"
	}
	var readDirs []string
	ops.ReadWorkMeta = func(_ context.Context, dir string, _ []string, beadID, _ string) (beads.Bead, error) {
		readDirs = append(readDirs, dir)
		if beadID != countedInCity.ID {
			return beads.Bead{}, fmt.Errorf("read %s: not the trigger", beadID)
		}
		if dir == workerDir {
			return closedInWorkerStore, nil
		}
		return countedInCity, nil
	}

	var stderr bytes.Buffer
	recordDemandClaimDivergence(hookClaimReasonNoWork, workerDir, divergenceOptions(demandSpawnEnv()...), ops, &stderr)

	if len(rec.Events) != 1 {
		t.Fatalf("events = %+v, want exactly 1", rec.Events)
	}
	var payload events.SessionDemandClaimDivergencePayload
	if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if payload.Classification != events.DemandClaimDivergence {
		t.Fatalf("classification = %q (status %q, trigger reads in dirs %v); want %q: the trigger is still open and claimable in the city store the controller counted from, only the worker's own store says closed",
			payload.Classification, payload.TriggerStatusAtDrain, readDirs, events.DemandClaimDivergence)
	}
}

// AC-2 for gastownhall/gascity#5858: the counting store's copy is closed or
// claimed by a sibling while the seat's own leg holds an OPEN copy. The verdict
// follows the counting store (benign), and no classification read is issued
// against the seat's own leg.
func TestDivergenceBenignFollowsTheCountingStoreAndNeverReadsTheSeatsOwnLeg(t *testing.T) {
	for _, counting := range []struct {
		name string
		bead beads.Bead
	}{
		{"closed in the counting store", beads.Bead{
			ID: "canary-8a1", Status: "closed", Type: "task",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
		}},
		{"claimed by a sibling in the counting store", beads.Bead{
			ID: "canary-8a1", Status: "in_progress", Type: "task", Assignee: "gc__worker-2",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
		}},
	} {
		t.Run(counting.name, func(t *testing.T) {
			rec := events.NewFake()
			prev := demandDivergenceRecorder
			demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
			t.Cleanup(func() { demandDivergenceRecorder = prev })

			// The seat's own leg would call the row OPEN: reading it is the
			// old bug, and this row is the bait that catches it.
			openInRig := beads.Bead{
				ID: "canary-8a1", Status: "open", Type: "task",
				Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
			}
			ops := demandDivergenceOpsForBlockedBead(openInRig, nil, false, nil)
			var readDirs []string
			ops.ReadWorkMeta = func(_ context.Context, dir string, _ []string, beadID, _ string) (beads.Bead, error) {
				readDirs = append(readDirs, dir)
				if beadID != counting.bead.ID {
					return beads.Bead{}, fmt.Errorf("read %s: not the trigger", beadID)
				}
				if dir == demandDivergenceTestCityDir {
					return counting.bead, nil
				}
				return openInRig, nil
			}

			var stderr bytes.Buffer
			recordDemandClaimDivergence(hookClaimReasonNoWork, demandDivergenceTestRigDir, divergenceOptions(demandSpawnEnv()...), ops, &stderr)

			if len(rec.Events) != 1 {
				t.Fatalf("events = %+v, want exactly 1", rec.Events)
			}
			var payload events.SessionDemandClaimDivergencePayload
			if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
				t.Fatalf("decoding payload: %v", err)
			}
			if payload.Classification != events.DemandClaimBenign {
				t.Fatalf("classification = %q, want benign: the counting store says the row moved on (status %q), reads in dirs %v",
					payload.Classification, payload.TriggerStatusAtDrain, readDirs)
			}
			if payload.TriggerStatusAtDrain != counting.bead.Status {
				t.Fatalf("trigger_status_at_drain = %q, want the counting store's %q", payload.TriggerStatusAtDrain, counting.bead.Status)
			}
			for _, dir := range readDirs {
				if dir == demandDivergenceTestRigDir {
					t.Fatalf("classification read the seat's own leg %q; reads = %v", dir, readDirs)
				}
			}
		})
	}
}

// AC-3 for gastownhall/gascity#5858: demandTriggerStoreAt places every
// gc.trigger_bead_store_ref spelling a session row can carry onto the leg the
// hook fan-out's own builders read — the city spellings (and a class ref,
// which is a city-scope store) onto the city leg, a rig stamp onto that rig's
// leg — and refuses every stamp a seat cannot place: another city, an unknown
// or suspended rig, no stamp at all, the census's %p fallback key, an unknown
// scheme, or a leg whose env projection failed. A refusal is an error, never a
// substitute leg.
func TestDemandTriggerStoreAtResolvesEachStampSpelling(t *testing.T) {
	clearGCEnv(t)
	cityDir := writeFenceTestCityAgents(t, `
[[rigs]]
name = "riga"
path = "rigs/riga"

[[rigs]]
name = "rigb"
path = "rigs/rigb"
suspended_on_start = true
`)
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	prevQueryEnv := hookStoreQueryEnv
	hookStoreQueryEnv = func(_ string, _ *config.City, a *config.Agent) (map[string]string, error) {
		return map[string]string{"GC_STORE": "leg-" + a.Dir}, nil
	}
	t.Cleanup(func() { hookStoreQueryEnv = prevQueryEnv })

	t.Run("the city spellings resolve onto the city leg", func(t *testing.T) {
		for _, ref := range []string{"city", "city:test-city", "class:gmnos"} {
			leg, err := demandTriggerStoreAt(cityDir, cfg, ref)
			if err != nil {
				t.Fatalf("resolve %q: %v", ref, err)
			}
			if leg.dir != cityDir || leg.storeRef != "city:test-city" {
				t.Fatalf("ref %q resolved to dir %q store %q, want the city leg (dir %q, city:test-city)", ref, leg.dir, leg.storeRef, cityDir)
			}
			if env := strings.Join(leg.env, "\n"); !strings.Contains(env, "GC_STORE=leg-") {
				t.Fatalf("ref %q resolved to a leg without the fan-out's env projection: %v", ref, leg.env)
			}
		}
	})

	t.Run("a rig stamp resolves onto that rig's leg", func(t *testing.T) {
		leg, err := demandTriggerStoreAt(cityDir, cfg, "rig:riga")
		if err != nil {
			t.Fatalf("resolve rig:riga: %v", err)
		}
		if leg.rig != "riga" || leg.storeRef != "rig:riga" {
			t.Fatalf("rig:riga resolved to rig %q store %q, want riga's leg", leg.rig, leg.storeRef)
		}
		if env := strings.Join(leg.env, "\n"); !strings.Contains(env, "GC_STORE=leg-riga") {
			t.Fatalf("rig:riga resolved to a leg without the fan-out's env projection: %v", leg.env)
		}
	})

	t.Run("a stamp the seat cannot place is an error, never a leg", func(t *testing.T) {
		for _, tt := range []struct{ ref, want string }{
			{"rig:rigb", `rig "rigb" is suspended`},
			{"rig:unknown", `no leg for rig "unknown"`},
			{"", "stamps no store"},
			{"city:elsewhere", "another city's store"},
			{"/tmp/a-percent-p-fallback", "no leg for rig"},
			{"scope:weird", "not a store a seat can read"},
		} {
			leg, err := demandTriggerStoreAt(cityDir, cfg, tt.ref)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("resolve %q = leg %+v, err %v; want an error naming %q", tt.ref, leg, err, tt.want)
			}
			if !isZeroHookStore(leg) {
				t.Fatalf("resolve %q returned an error AND a leg (%+v); a refusal is never a substitute leg", tt.ref, leg)
			}
		}
	})

	t.Run("a leg whose env projection fails is an error, never a leg", func(t *testing.T) {
		prev := hookStoreQueryEnv
		hookStoreQueryEnv = func(string, *config.City, *config.Agent) (map[string]string, error) {
			return nil, errors.New("env projection failed")
		}
		t.Cleanup(func() { hookStoreQueryEnv = prev })
		for _, ref := range []string{"city", "rig:riga"} {
			leg, err := demandTriggerStoreAt(cityDir, cfg, ref)
			if err == nil {
				t.Fatalf("resolve %q with a failed env projection = leg %+v, want an error", ref, leg)
			}
			if !isZeroHookStore(leg) {
				t.Fatalf("resolve %q returned an error AND a leg (%+v)", ref, leg)
			}
		}
	})
}

// AC-3's classifier half: a trigger whose counted store the seat cannot place
// classifies unknown with status store_unresolved, and NOTHING is read — not
// the work, not the blockedness deps. The row below is divergence bait (open,
// servable, route-matching): had the classifier fallen back to any other leg,
// or read on error, it would have said divergence.
func TestDivergenceUnplaceableTriggerStoreClassifiesUnknownWithoutReading(t *testing.T) {
	for _, tt := range []struct {
		name     string
		storeRef string
		resolver func(string) (hookStore, error)
	}{
		{"no resolver wired", demandDivergenceTestRigRef, nil},
		{"a stamp the resolver cannot place", "rig:gone", demandDivergenceTestTriggerStore},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := events.NewFake()
			prev := demandDivergenceRecorder
			demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
			t.Cleanup(func() { demandDivergenceRecorder = prev })

			reads, confirms := 0, 0
			ops := hookClaimOps{
				ReadSessionTrigger: func(string) (demandTrigger, error) {
					return demandTrigger{ID: "canary-8a1", StoreRef: tt.storeRef}, nil
				},
				DemandTriggerStore: tt.resolver,
				ReadWorkMeta: func(context.Context, string, []string, string, string) (beads.Bead, error) {
					reads++
					return beads.Bead{
						ID: "canary-8a1", Status: "open", Type: "task",
						Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
					}, nil
				},
				ConfirmBlocked: func(context.Context, string, []string, string, string) (bool, error) {
					confirms++
					return false, nil
				},
			}
			var stderr bytes.Buffer
			recordDemandClaimDivergence(hookClaimReasonNoWork, demandDivergenceTestRigDir, divergenceOptions(demandSpawnEnv()...), ops, &stderr)

			if len(rec.Events) != 1 {
				t.Fatalf("events = %+v, want exactly 1", rec.Events)
			}
			var payload events.SessionDemandClaimDivergencePayload
			if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
				t.Fatalf("decoding payload: %v", err)
			}
			if payload.Classification != events.DemandClaimUnknown {
				t.Fatalf("classification = %q (status %q), want unknown: an unplaceable stamp is never evidence either way", payload.Classification, payload.TriggerStatusAtDrain)
			}
			if payload.TriggerStatusAtDrain != demandTriggerStoreUnresolved {
				t.Fatalf("trigger_status_at_drain = %q, want %q", payload.TriggerStatusAtDrain, demandTriggerStoreUnresolved)
			}
			if reads != 0 {
				t.Fatalf("the classification read the work %d times; an unplaceable stamp must read nothing", reads)
			}
			if confirms != 0 {
				t.Fatalf("the classification settled blockedness %d times; an unplaceable stamp must read nothing", confirms)
			}
		})
	}
}

// AC-4 for gastownhall/gascity#5858: the blockedness settlement must never be
// able to invent a divergence. Three readings that carry no verdict each stay
// unknown — a counting read that fails (even though the seat's own leg would
// happily serve the open row), a dependency settlement that errors, and a row
// the invocation's own residency frame has proved lives in a relocated class
// binding: ConfirmBlocked reads the work ledger, which does not hold
// binding-served rows, and its dependency list answers not-found with an empty
// list, so settling would manufacture divergence out of an artifact of the
// store split.
func TestDivergenceSettlementNeverInventsAVerdict(t *testing.T) {
	bait := beads.Bead{
		ID: "wb-1", Status: "open", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	}

	run := func(t *testing.T, ops hookClaimOps) events.SessionDemandClaimDivergencePayload {
		t.Helper()
		rec := events.NewFake()
		prev := demandDivergenceRecorder
		demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
		t.Cleanup(func() { demandDivergenceRecorder = prev })
		var stderr bytes.Buffer
		recordDemandClaimDivergence(hookClaimReasonNoWork, demandDivergenceTestRigDir, divergenceOptions(demandSpawnEnv()...), ops, &stderr)
		if len(rec.Events) != 1 {
			t.Fatalf("events = %+v, want exactly 1", rec.Events)
		}
		var payload events.SessionDemandClaimDivergencePayload
		if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
			t.Fatalf("decoding payload: %v", err)
		}
		return payload
	}

	t.Run("a failed counting read classifies unknown, unreadable", func(t *testing.T) {
		confirms := 0
		ops := hookClaimOps{
			ReadSessionTrigger: func(string) (demandTrigger, error) {
				return demandTrigger{ID: "wb-1", StoreRef: demandDivergenceTestCityRef}, nil
			},
			DemandTriggerStore: demandDivergenceTestTriggerStore,
			ReadWorkMeta: func(_ context.Context, dir string, _ []string, beadID, _ string) (beads.Bead, error) {
				if beadID != bait.ID {
					return beads.Bead{}, fmt.Errorf("read %s: not the trigger", beadID)
				}
				if dir == demandDivergenceTestCityDir {
					return beads.Bead{}, errors.New("counting store read failed")
				}
				// The seat's own leg would happily serve the open row: divergence
				// bait, if any fallback read the wrong store.
				return bait, nil
			},
			ConfirmBlocked: func(context.Context, string, []string, string, string) (bool, error) {
				confirms++
				return false, nil
			},
		}
		payload := run(t, ops)
		if payload.Classification != events.DemandClaimUnknown {
			t.Fatalf("classification = %q, want unknown: a failed counting read is never evidence", payload.Classification)
		}
		if payload.TriggerStatusAtDrain != demandTriggerUnreadable {
			t.Fatalf("trigger_status_at_drain = %q, want %q", payload.TriggerStatusAtDrain, demandTriggerUnreadable)
		}
		if confirms != 0 {
			t.Fatalf("blockedness was settled %d times after a failed read", confirms)
		}
	})

	t.Run("a failed blockedness settlement classifies unknown over the open status", func(t *testing.T) {
		ops := hookClaimOps{
			ReadSessionTrigger: func(string) (demandTrigger, error) {
				return demandTrigger{ID: "wb-1", StoreRef: demandDivergenceTestCityRef}, nil
			},
			DemandTriggerStore: demandDivergenceTestTriggerStore,
			ReadWorkMeta: func(context.Context, string, []string, string, string) (beads.Bead, error) {
				return bait, nil // open with no blockedness projection: the settlement arm
			},
			ConfirmBlocked: func(context.Context, string, []string, string, string) (bool, error) {
				return false, errors.New("dependency read failed")
			},
		}
		payload := run(t, ops)
		if payload.Classification != events.DemandClaimUnknown {
			t.Fatalf("classification = %q, want unknown: a failed settlement is never evidence", payload.Classification)
		}
		if payload.TriggerStatusAtDrain != "open" {
			t.Fatalf("trigger_status_at_drain = %q, want the counting store's open", payload.TriggerStatusAtDrain)
		}
	})

	t.Run("a binding-resident row never settles blockedness", func(t *testing.T) {
		confirms := 0
		ops := hookClaimOps{
			ReadSessionTrigger: func(string) (demandTrigger, error) {
				return demandTrigger{ID: "wb-1", StoreRef: demandDivergenceTestCityRef}, nil
			},
			DemandTriggerStore: demandDivergenceTestTriggerStore,
			ReadWorkMeta: func(context.Context, string, []string, string, string) (beads.Bead, error) {
				return bait, nil
			},
			ConfirmBlocked: func(context.Context, string, []string, string, string) (bool, error) {
				confirms++
				return false, nil // not blocked: the answer that would say divergence
			},
			// The invocation's own residency frame has already proved the binding
			// holds wb-1, so the work ledger this settlement reads does not carry
			// the row and the empty dependency answer would be an artifact.
			ClassRoute: &hookClaimClassRoute{resident: map[string]bool{"wb-1": true}},
		}
		payload := run(t, ops)
		if payload.Classification != events.DemandClaimUnknown {
			t.Fatalf("classification = %q, want unknown: a binding-served row's blockedness cannot be settled through the work ledger", payload.Classification)
		}
		if payload.TriggerStatusAtDrain != "open" {
			t.Fatalf("trigger_status_at_drain = %q, want the counting store's open", payload.TriggerStatusAtDrain)
		}
		if confirms != 0 {
			t.Fatalf("blockedness was settled %d times for a binding-resident row; the settlement must not run", confirms)
		}
	})
}
