package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
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
	hookRecordDemandClaimDivergence = func(reason string, _ hookClaimOptions, ops hookClaimOps, _ io.Writer) {
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
			status, class := classifyDemandTrigger(demandTrigger{ID: tt.triggerID, StoreRef: demandDivergenceSeatStoreRef}, opts, ops)
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
	recordDemandClaimDivergence(hookClaimReasonNoWork, opts, demandDivergenceOpsForBead(winner, nil), &stderr)

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
	recordDemandClaimDivergence(hookClaimReasonNoWork, opts, demandDivergenceOpsForBead(stillThere, nil), &stderr)

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
			recordDemandClaimDivergence(hookClaimReasonNoWork, divergenceOptions(demandSpawnEnv()...), ops, &stderr)

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
			_, class := classifyDemandTrigger(demandTrigger{ID: tt.bead.ID, StoreRef: demandDivergenceSeatStoreRef}, divergenceOptions(), demandDivergenceOpsForBlockedBead(tt.bead, nil, false, nil))
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

	recordDemandClaimDivergence(hookClaimReasonNoWork, opts, demandDivergenceOpsForBead(stillThere, nil), &stderr)

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
				return demandTrigger{ID: stillThere.ID, StoreRef: demandDivergenceSeatStoreRef}, nil
			}

			var stderr bytes.Buffer
			recordDemandClaimDivergence(hookClaimReasonNoWork, divergenceOptions(c.env...), ops, &stderr)

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
	ops.ReadSessionTrigger = func(string) (demandTrigger, error) {
		return demandTrigger{}, errors.New("session row read failed")
	}

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

// demandCountingLegs is a DemandTriggerStore double: each store ref it knows
// resolves to its leg, anything else is a ref the seat cannot place.
func demandCountingLegs(legs map[string]hookStore) func(string) (hookStore, error) {
	return func(ref string) (hookStore, error) {
		leg, ok := legs[ref]
		if !ok {
			return hookStore{}, fmt.Errorf("store ref %q is not placeable from this seat", ref)
		}
		return leg, nil
	}
}

// gastownhall/gascity#5858, the benign direction: the controller counted the
// trigger in the CITY store, where it has since moved on (closed, or claimed by
// a sibling), while the seat's own rig leg still holds an open, route-matching
// copy of the same id. The verdict is the counting store's, so it is benign;
// and the classification never reads the seat's own leg at all, neither for
// the row nor for its dependencies.
func TestDivergenceClassifiesTheCountingStoreNotTheSeatsOwnLeg(t *testing.T) {
	routed := map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"}
	openInSeatLeg := beads.Bead{ID: "canary-8a1", Status: "open", Type: "task", Metadata: routed}
	for _, tt := range []struct {
		name     string
		counted  beads.Bead
		wantStat string
	}{
		{
			name:     "closed where it was counted",
			counted:  beads.Bead{ID: "canary-8a1", Status: "closed", Type: "task", Metadata: routed},
			wantStat: "closed",
		},
		{
			name:     "claimed by a sibling where it was counted",
			counted:  beads.Bead{ID: "canary-8a1", Status: "in_progress", Type: "task", Assignee: "gc__worker-2", Metadata: routed},
			wantStat: "in_progress",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := events.NewFake()
			prev := demandDivergenceRecorder
			demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
			t.Cleanup(func() { demandDivergenceRecorder = prev })

			const seatDir, cityDir = "/rig", "/city"
			ops := demandDivergenceOpsForBlockedBead(openInSeatLeg, nil, false, nil)
			ops.ReadSessionTrigger = func(string) (demandTrigger, error) {
				return demandTrigger{ID: "canary-8a1", StoreRef: "city"}, nil
			}
			ops.DemandTriggerStore = demandCountingLegs(map[string]hookStore{
				"city": {dir: cityDir, env: []string{"GC_STORE=city"}},
			})
			var reads, depReads []string
			ops.ReadWorkMeta = func(_ context.Context, dir string, env []string, _, _ string) (beads.Bead, error) {
				reads = append(reads, dir+" "+strings.Join(env, ","))
				if dir == cityDir {
					return tt.counted, nil
				}
				return openInSeatLeg, nil
			}
			ops.ConfirmBlocked = func(_ context.Context, dir string, _ []string, _, _ string) (bool, error) {
				depReads = append(depReads, dir)
				return false, nil
			}
			ops.DrainAck = func(io.Writer) error { return nil }
			ops.ReadWorkStarted = noStartedClaims

			var stdout, stderr bytes.Buffer
			writeHookClaimNoWork(divergenceOptions(demandSpawnEnv()...), ops, false, seatDir, &stdout, &stderr)

			if len(rec.Events) != 1 {
				t.Fatalf("events = %+v, want exactly 1", rec.Events)
			}
			var payload events.SessionDemandClaimDivergencePayload
			if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
				t.Fatalf("decoding payload: %v", err)
			}
			if payload.Classification != events.DemandClaimBenign || payload.TriggerStatusAtDrain != tt.wantStat {
				t.Fatalf("payload = %+v, want benign over the counting store's %q row", payload, tt.wantStat)
			}
			if len(reads) != 1 || reads[0] != cityDir+" GC_STORE=city" {
				t.Fatalf("trigger reads = %q, want exactly one, in the counting leg %s with its env", reads, cityDir)
			}
			if len(depReads) != 0 {
				t.Fatalf("dependency reads = %q, want none for a row that moved on", depReads)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want silence for a benign classification", stderr.String())
			}
		})
	}
}

// gastownhall/gascity#5858: the controller counted the trigger in the CITY
// store (open, unassigned, route-matching), but the seat drained in a rig store
// where a same-id row is closed. The classifier must read the row in the store
// the demand was counted in, not re-read the seat's own store, or a
// cross-store demand/claim disagreement always classifies as benign. The
// operator line names the row and the store it is still claimable in.
func TestDivergenceReadsTheTriggerInTheCountingStoreNotOnlyTheWorkers(t *testing.T) {
	rec := events.NewFake()
	prev := demandDivergenceRecorder
	demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
	t.Cleanup(func() { demandDivergenceRecorder = prev })

	const seatDir, cityDir = "/rig", "/city"
	countedInCity := beads.Bead{
		ID: "canary-8a1", Status: "open", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	}
	closedInSeatStore := countedInCity
	closedInSeatStore.Status = "closed"

	ops := demandDivergenceOpsForBlockedBead(countedInCity, nil, false, nil)
	ops.ReadSessionTrigger = func(string) (demandTrigger, error) {
		return demandTrigger{ID: countedInCity.ID, StoreRef: "city"}, nil
	}
	ops.DemandTriggerStore = demandCountingLegs(map[string]hookStore{
		"city":                       {dir: cityDir},
		demandDivergenceSeatStoreRef: {dir: seatDir},
	})
	var readDirs, depDirs []string
	ops.ReadWorkMeta = func(_ context.Context, dir string, _ []string, beadID, _ string) (beads.Bead, error) {
		readDirs = append(readDirs, dir)
		if beadID != countedInCity.ID {
			return beads.Bead{}, fmt.Errorf("read %s: not the trigger", beadID)
		}
		if dir == seatDir {
			return closedInSeatStore, nil
		}
		return countedInCity, nil
	}
	ops.ConfirmBlocked = func(_ context.Context, dir string, _ []string, _, _ string) (bool, error) {
		depDirs = append(depDirs, dir)
		return false, nil
	}

	var stderr bytes.Buffer
	recordDemandClaimDivergence(hookClaimReasonNoWork, divergenceOptions(demandSpawnEnv()...), ops, &stderr)

	if len(rec.Events) != 1 {
		t.Fatalf("events = %+v, want exactly 1", rec.Events)
	}
	var payload events.SessionDemandClaimDivergencePayload
	if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if payload.Classification != events.DemandClaimDivergence || payload.TriggerStatusAtDrain != "open" {
		t.Fatalf("payload = %+v (trigger reads in %v); want divergence over the open row: it is still claimable in the city store the controller counted it in, only the seat's own store says closed",
			payload, readDirs)
	}
	if len(readDirs) != 1 || readDirs[0] != cityDir || len(depDirs) != 1 || depDirs[0] != cityDir {
		t.Fatalf("trigger reads %v, dependency reads %v; want one of each, in the counting leg %s", readDirs, depDirs, cityDir)
	}
	if line := stderr.String(); !strings.Contains(line, "canary-8a1 is still claimable in city") {
		t.Fatalf("stderr = %q, want the line naming canary-8a1 and the city store it was counted in", line)
	}
}

// demandStampCity is a city with three rigs for the stamp resolver: riga is
// served, rigb is suspended on start, and rigc's store env cannot be projected.
// The env projection is stubbed to name the leg it was built for.
func demandStampCity(t *testing.T) (string, *config.City) {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "riga", Path: filepath.Join(cityPath, "rigs", "riga")},
			{Name: "rigb", Path: filepath.Join(cityPath, "rigs", "rigb"), SuspendedOnStart: true},
			{Name: "rigc", Path: filepath.Join(cityPath, "rigs", "rigc")},
		},
	}
	prev := hookStoreQueryEnv
	hookStoreQueryEnv = func(_ string, _ *config.City, a *config.Agent) (map[string]string, error) {
		if a.Dir == "rigc" {
			return nil, errors.New("projecting rigc's store env failed")
		}
		return map[string]string{"GC_STORE": "leg-" + a.Dir}, nil
	}
	t.Cleanup(func() { hookStoreQueryEnv = prev })
	return cityPath, cfg
}

// Kills a resolver that places a stamp the census never meant: every spelling
// of this city's store and of a served rig's store resolves to that store's
// leg, and every stamp the seat cannot honestly place is an error, never a
// fallback to some other store (gastownhall/gascity#5858).
func TestDemandTriggerStoreAtResolvesEachStampSpelling(t *testing.T) {
	cityPath, cfg := demandStampCity(t)
	rigaDir := filepath.Join(cityPath, "rigs", "riga")
	for _, tt := range []struct {
		stamp   string
		wantDir string
		wantEnv string
	}{
		{stamp: "city", wantDir: cityPath, wantEnv: "GC_STORE=leg-"},
		{stamp: "city:test-city", wantDir: cityPath, wantEnv: "GC_STORE=leg-"},
		{stamp: " city ", wantDir: cityPath, wantEnv: "GC_STORE=leg-"},
		{stamp: "class:graph", wantDir: cityPath, wantEnv: "GC_STORE=leg-"},
		{stamp: "rig:riga", wantDir: rigaDir, wantEnv: "GC_STORE=leg-riga"},
		{stamp: "riga", wantDir: rigaDir, wantEnv: "GC_STORE=leg-riga"},
	} {
		t.Run("places "+tt.stamp, func(t *testing.T) {
			leg, err := demandTriggerStoreAt(cityPath, cfg, tt.stamp)
			if err != nil {
				t.Fatalf("demandTriggerStoreAt(%q): %v", tt.stamp, err)
			}
			if filepath.Clean(leg.dir) != filepath.Clean(tt.wantDir) || !slices.Contains(leg.env, tt.wantEnv) {
				t.Fatalf("leg = dir %q, env has %q: %v; want dir %q with %q", leg.dir, tt.wantEnv, slices.Contains(leg.env, tt.wantEnv), tt.wantDir, tt.wantEnv)
			}
		})
	}
	for _, stamp := range []string{
		"",                 // nothing records where the row was counted
		"city:other-city",  // another city's store
		"rig:nope",         // a rig not in city.toml
		"rig:rigb",         // a suspended rig: its Dolt may be stopped
		"rigb",             // the same, in the bare spelling
		"rig:rigc",         // a rig whose store env cannot be projected
		"0xc000123456",     // the census's %p fallback key
		"elsewhere:canary", // a scheme the census never writes
	} {
		t.Run("refuses "+stamp, func(t *testing.T) {
			if leg, err := demandTriggerStoreAt(cityPath, cfg, stamp); err == nil {
				t.Fatalf("demandTriggerStoreAt(%q) = leg at %q, nil; want an error: the seat cannot place that store", stamp, leg.dir)
			}
		})
	}
	t.Run("refuses a rig while the suspension state is unreadable", func(t *testing.T) {
		statePath := citylayout.SuspensionStateFile(cityPath)
		if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if leg, err := demandTriggerStoreAt(cityPath, cfg, "rig:riga"); err == nil {
			t.Fatalf("demandTriggerStoreAt(rig:riga) = leg at %q, nil with an unreadable suspension state; want an error", leg.dir)
		}
	})
}

// gastownhall/gascity#5858: a stamp the seat cannot place classifies unknown
// with status store_unresolved, and reads nothing: neither the trigger row nor
// its dependencies, in the counting store or in the seat's own.
func TestDivergenceUnplaceableStampIsUnknownWithoutAnyRead(t *testing.T) {
	cityPath, cfg := demandStampCity(t)
	resolver := func(ref string) (hookStore, error) { return demandTriggerStoreAt(cityPath, cfg, ref) }
	openRow := beads.Bead{
		ID: "canary-8a1", Status: "open", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	}
	for _, tt := range []struct {
		name     string
		stamp    string
		resolver func(string) (hookStore, error)
	}{
		{name: "no stamp", stamp: "", resolver: resolver},
		{name: "another city", stamp: "city:other-city", resolver: resolver},
		{name: "a rig not in city.toml", stamp: "rig:nope", resolver: resolver},
		{name: "a suspended rig", stamp: "rig:rigb", resolver: resolver},
		{name: "the census fallback key", stamp: "0xc000123456", resolver: resolver},
		{name: "an env projection failure", stamp: "rig:rigc", resolver: resolver},
		{name: "no resolver wired", stamp: "city", resolver: nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := events.NewFake()
			prev := demandDivergenceRecorder
			demandDivergenceRecorder = func(io.Writer) events.Recorder { return rec }
			t.Cleanup(func() { demandDivergenceRecorder = prev })

			ops := demandDivergenceOpsForBlockedBead(openRow, nil, false, nil)
			ops.ReadSessionTrigger = func(string) (demandTrigger, error) {
				return demandTrigger{ID: openRow.ID, StoreRef: tt.stamp}, nil
			}
			ops.DemandTriggerStore = tt.resolver
			reads := 0
			ops.ReadWorkMeta = func(context.Context, string, []string, string, string) (beads.Bead, error) {
				reads++
				return openRow, nil
			}
			ops.ConfirmBlocked = func(context.Context, string, []string, string, string) (bool, error) {
				reads++
				return false, nil
			}

			var stderr bytes.Buffer
			recordDemandClaimDivergence(hookClaimReasonNoWork, divergenceOptions(demandSpawnEnv()...), ops, &stderr)

			if len(rec.Events) != 1 {
				t.Fatalf("events = %+v, want exactly 1", rec.Events)
			}
			var payload events.SessionDemandClaimDivergencePayload
			if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
				t.Fatalf("decoding payload: %v", err)
			}
			if payload.Classification != events.DemandClaimUnknown || payload.TriggerStatusAtDrain != "store_unresolved" || payload.TriggerBeadID != openRow.ID {
				t.Fatalf("payload = %+v, want unknown with status store_unresolved for %s", payload, openRow.ID)
			}
			if reads != 0 {
				t.Fatalf("store reads = %d, want none for a counting store the seat cannot place", reads)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want silence for an unknown classification", stderr.String())
			}
		})
	}
}

// gastownhall/gascity#5858, the failure modes once the counting store
// resolves: a failed trigger read, a failed dependency settlement, and a row
// the class route served from the relocated binding whose blockedness the row
// alone cannot answer are each unknown, never divergence. A binding-served row
// gets no dependency walk at all: the walk is not class-routed, and run against
// a work ledger that does not hold the row it would read "not blocked" and
// invent a divergence. The control shows the route alone changes nothing for a
// row it did not serve.
func TestDivergenceCountingStoreFailuresAreUnknown(t *testing.T) {
	openRow := beads.Bead{
		ID: "canary-8a1", Status: "open", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	}
	servedFromBinding := &hookClaimClassRoute{resident: map[string]bool{openRow.ID: true}}
	servedFromLedger := &hookClaimClassRoute{resident: map[string]bool{"gcg-other": true}}
	for _, tt := range []struct {
		name       string
		readErr    error
		confirmErr error
		route      *hookClaimClassRoute
		wantStat   string
		wantClass  string
		wantDeps   int
	}{
		{
			name: "the counting-store read fails", readErr: errors.New("city store read failed"),
			wantStat: "unreadable", wantClass: events.DemandClaimUnknown, wantDeps: 0,
		},
		{
			name: "the dependency settlement fails", confirmErr: errors.New("dependency read failed"),
			wantStat: "open", wantClass: events.DemandClaimUnknown, wantDeps: 1,
		},
		{
			name: "the class route served the row from the binding", route: servedFromBinding,
			wantStat: "open", wantClass: events.DemandClaimUnknown, wantDeps: 0,
		},
		{
			name: "control: the class route did not serve this row", route: servedFromLedger,
			wantStat: "open", wantClass: events.DemandClaimDivergence, wantDeps: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const cityDir = "/city"
			ops := demandDivergenceOpsForBlockedBead(openRow, nil, false, nil)
			ops.ClassRoute = tt.route
			ops.DemandTriggerStore = demandCountingLegs(map[string]hookStore{"city": {dir: cityDir}})
			var reads, deps []string
			ops.ReadWorkMeta = func(_ context.Context, dir string, _ []string, _, _ string) (beads.Bead, error) {
				reads = append(reads, dir)
				return openRow, tt.readErr
			}
			ops.ConfirmBlocked = func(_ context.Context, dir string, _ []string, _, _ string) (bool, error) {
				deps = append(deps, dir)
				return false, tt.confirmErr
			}

			status, class := classifyDemandTrigger(demandTrigger{ID: openRow.ID, StoreRef: "city"}, divergenceOptions(), ops)

			if status != tt.wantStat || class != tt.wantClass {
				t.Fatalf("classification = %q (status %q), want %q (status %q)", class, status, tt.wantClass, tt.wantStat)
			}
			if len(reads) != 1 || reads[0] != cityDir {
				t.Fatalf("trigger reads = %v, want one, in the counting leg %s", reads, cityDir)
			}
			if len(deps) != tt.wantDeps || (len(deps) == 1 && deps[0] != cityDir) {
				t.Fatalf("dependency reads = %v, want %d in the counting leg %s", deps, tt.wantDeps, cityDir)
			}
		})
	}
}

// gastownhall/gascity#5858, the no-regression guard: a seat whose trigger was
// counted in its own store — a rig-pool seat stamped with its rig, a
// city-scoped seat stamped with the city — resolves the stamp, through the
// production resolver, to the leg it already queries, and every verdict is
// what it was before the counting store existed. The trigger read and the
// dependency settlement both land in that one leg.
func TestDivergenceSeatsOwnStampKeepsEveryVerdict(t *testing.T) {
	cityPath, cfg := demandStampCity(t)
	routed := map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"}
	for _, seat := range []struct {
		name    string
		stamp   string
		wantDir string
	}{
		{name: "rig-pool seat", stamp: "rig:riga", wantDir: filepath.Join(cityPath, "rigs", "riga")},
		{name: "city-scoped seat", stamp: "city", wantDir: cityPath},
	} {
		for _, tt := range []struct {
			name       string
			bead       beads.Bead
			readErr    error
			blocked    bool
			confirmErr error
			wantClass  string
			wantStat   string
			wantDeps   int
		}{
			{
				name:      "open and claimable",
				bead:      beads.Bead{ID: "wb-1", Status: "open", Type: "task", Metadata: routed},
				wantClass: events.DemandClaimDivergence, wantStat: "open", wantDeps: 1,
			},
			{
				name:      "open but dep-confirmed blocked",
				bead:      beads.Bead{ID: "wb-1", Status: "open", Type: "task", Metadata: routed},
				blocked:   true,
				wantClass: events.DemandClaimBlocked, wantStat: "open", wantDeps: 1,
			},
			{
				name:       "settlement read fails",
				bead:       beads.Bead{ID: "wb-1", Status: "open", Type: "task", Metadata: routed},
				confirmErr: errors.New("dependency read failed"),
				wantClass:  events.DemandClaimUnknown, wantStat: "open", wantDeps: 1,
			},
			{
				name:      "claimed by a sibling",
				bead:      beads.Bead{ID: "wb-1", Status: "in_progress", Type: "task", Assignee: "gc__worker-2", Metadata: routed},
				wantClass: events.DemandClaimBenign, wantStat: "in_progress",
			},
			{
				name:      "unreadable",
				readErr:   errors.New("store read failed"),
				wantClass: events.DemandClaimUnknown, wantStat: "unreadable",
			},
		} {
			t.Run(seat.name+"/"+tt.name, func(t *testing.T) {
				ops := demandDivergenceOpsForBlockedBead(tt.bead, tt.readErr, tt.blocked, tt.confirmErr)
				ops.DemandTriggerStore = func(ref string) (hookStore, error) {
					return demandTriggerStoreAt(cityPath, cfg, ref)
				}
				var reads, deps []string
				ops.ReadWorkMeta = func(_ context.Context, dir string, _ []string, _, _ string) (beads.Bead, error) {
					reads = append(reads, filepath.Clean(dir))
					return tt.bead, tt.readErr
				}
				ops.ConfirmBlocked = func(_ context.Context, dir string, _ []string, _, _ string) (bool, error) {
					deps = append(deps, filepath.Clean(dir))
					return tt.blocked, tt.confirmErr
				}

				status, class := classifyDemandTrigger(demandTrigger{ID: "wb-1", StoreRef: seat.stamp}, divergenceOptions(), ops)

				if class != tt.wantClass || status != tt.wantStat {
					t.Fatalf("classification = %q (status %q), want %q (status %q)", class, status, tt.wantClass, tt.wantStat)
				}
				if len(reads) != 1 || reads[0] != seat.wantDir {
					t.Fatalf("trigger reads = %v, want one, in the seat's own leg %s", reads, seat.wantDir)
				}
				if len(deps) != tt.wantDeps || (len(deps) == 1 && deps[0] != seat.wantDir) {
					t.Fatalf("dependency reads = %v, want %d in the seat's own leg %s", deps, tt.wantDeps, seat.wantDir)
				}
			})
		}
	}
}
