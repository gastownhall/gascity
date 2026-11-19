package workertest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	worker "github.com/gastownhall/gascity/internal/worker"
)

// rppHandleProfile labels the WC-RPP results: the non-certifying
// alternate-transport proof profile, the golden reference pack driven
// through the real exec provider.
const rppHandleProfile ProfileID = "reference/rpp-ops"

// rppSessionName is the session every WC-RPP check starts on the golden
// reference.
const rppSessionName = "rpp-worker"

// TestRPPHandleConformance emits the WC-RPP rows through the SuiteReporter
// artifact path, driving a RuntimeHandle over exec.NewSeamBackedForCity.
func TestRPPHandleConformance(t *testing.T) {
	reporter := NewSuiteReporter(t, "rpp-handle", map[string]string{
		"tier": "worker-core",
	})
	reporter.Require(t, vanishedNudgeNotDeliveredResult(t))
	reporter.Require(t, typedOutcomeReachesHandleResult(t))
	reporter.Require(t, unobservedIsDeliveredResult(t))
	reporter.Require(t, firstTurnQueuedResult(t))
	reporter.Require(t, hostFSTranscriptTurnsResult(t))
	t.Run(string(RequirementRPPHostFSLeftoverReaped), func(t *testing.T) {
		requireHostFSLeftoverReaped(t, reporter, goruntime.GOOS)
	})
	reporter.Require(t, heldNudgeTypedOnceResult(t))
}

// requireHostFSLeftoverReaped records WC-RPP-006 on goos. Where the row is
// unsupported (no /proc) it records the outcome and skips; elsewhere a
// non-passing result fails t.
func requireHostFSLeftoverReaped(t *testing.T, reporter *SuiteReporter, goos string) {
	t.Helper()
	result := hostFSLeftoverReapedResult(t, goos)
	if result.Status == ResultUnsupported {
		reporter.Record(result)
		t.Skip(result.Detail)
		return
	}
	reporter.Require(t, result)
}

// TestRPPHostFSLeftoverReapedSkipsWithoutProcfs proves WC-RPP-006 is recorded
// as unsupported and skipped, not failed, on a platform without /proc.
func TestRPPHostFSLeftoverReapedSkipsWithoutProcfs(t *testing.T) {
	reporter := &SuiteReporter{suite: "rpp-handle-skip"}
	var skipped bool
	t.Run("darwin", func(t *testing.T) {
		t.Cleanup(func() { skipped = t.Skipped() })
		requireHostFSLeftoverReaped(t, reporter, "darwin")
	})
	if !skipped {
		t.Fatal("WC-RPP-006 on darwin did not skip")
	}
	if len(reporter.results) != 1 || reporter.results[0].Status != ResultUnsupported ||
		reporter.results[0].Requirement != RequirementRPPHostFSLeftoverReaped {
		t.Fatalf("recorded results = %+v, want one unsupported WC-RPP-006", reporter.results)
	}
}

func TestRPPHandleCatalogRegistersItsRequirements(t *testing.T) {
	catalog := RPPHandleCatalog()
	want := []RequirementCode{
		RequirementRPPVanishedNudgeNotDelivered, RequirementRPPTypedOutcomeReachesHandle, RequirementRPPUnobservedIsDelivered,
		RequirementRPPFirstTurnQueued, RequirementRPPHostFSTranscriptTurns, RequirementRPPHostFSLeftoverReaped, RequirementRPPHeldNudgeTypedOnce,
	}
	if len(catalog) != len(want) {
		t.Fatalf("RPPHandleCatalog() = %+v, want %v", catalog, want)
	}
	for i, req := range catalog {
		if req.Code != want[i] {
			t.Fatalf("RPPHandleCatalog()[%d] = %s, want %s", i, req.Code, want[i])
		}
		if req.Group == "" || req.Description == "" {
			t.Fatalf("requirement %s has empty group or description", req.Code)
		}
	}
}

// rppHostFSPack writes the host-filesystem golden reference (locality local
// with fs.host, or the base golden when hostFS is false) with an `exec` that
// runs nothing, so no check drives a host tmux.
func rppHostFSPack(t *testing.T, hostFS bool) string {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bodies := reference.Bodies()
	if hostFS {
		bodies = reference.HostFSBodies()
	}
	bodies["exec"] = `cat >/dev/null`
	script := filepath.Join(dir, "golden")
	if err := os.WriteFile(script, []byte(reference.Script(stateDir, bodies)), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// hostFSTranscriptTurnsResult evaluates WC-RPP-005: a Claude session on a
// pack sharing the host filesystem, whose fixture transcript is written where
// the session's key finds it, reads its turns from the transcript and keeps
// the TurnHandle contract; on the same pack without fs.host it has no turn
// source. The pack runs the session's command on this host, so the session
// is launched with `true`, not an agent.
func hostFSTranscriptTurnsResult(t *testing.T) Result {
	t.Helper()
	transcript := []string{
		`{"type":"user","isSidechain":false,"timestamp":"2026-10-13T05:00:00.123Z","message":{"role":"user","content":"a prompt"},"promptId":"7f3e9a10-1111-4aaa-8000-000000000001","origin":"user","promptSource":"prompt"}`,
		`{"type":"system","subtype":"turn_duration","isSidechain":false,"timestamp":"2026-10-13T05:00:05.456Z"}`,
		`{"type":"user","isSidechain":false,"timestamp":"2026-10-13T05:01:00.789Z","message":{"role":"user","content":"a prompt"},"promptId":"7f3e9a10-2222-4aaa-8000-000000000002","origin":"user","promptSource":"prompt"}`,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, tc := range []struct {
		hostFS bool
		want   worker.TurnSource
	}{{true, worker.TurnSourceTranscript}, {false, worker.TurnSourceNone}} {
		sp := sessionexec.NewSeamBackedForCity(rppHostFSPack(t, tc.hostFS), "rpp-conformance", t.TempDir())
		h, _ := turnConformanceSessionRunning(t, sp, "true", nil, transcript, 0)
		t.Cleanup(func() { _ = h.StopForShutdown(context.Background()) })
		caps, err := h.TurnCapabilities(ctx)
		if err != nil || caps.Source != tc.want {
			return Fail(rppHandleProfile, RequirementRPPHostFSTranscriptTurns,
				fmt.Sprintf("fs.host %t: TurnCapabilities = %+v, %v; want source %q", tc.hostFS, caps, err, tc.want))
		}
		if err := CheckTurnHandle(ctx, h); err != nil {
			return Fail(rppHandleProfile, RequirementRPPHostFSTranscriptTurns, fmt.Sprintf("fs.host %t: %v", tc.hostFS, err))
		}
		if !tc.hostFS {
			continue
		}
		hist, err := h.CurrentTurn(ctx)
		if err != nil || len(hist.Recent) != 1 {
			return Fail(rppHandleProfile, RequirementRPPHostFSTranscriptTurns,
				fmt.Sprintf("CurrentTurn = %+v, %v; want the transcript's one ended turn", hist, err))
		}
	}
	return Pass(rppHandleProfile, RequirementRPPHostFSTranscriptTurns,
		"an fs.host pack session reads its turns from its transcript; without fs.host it has no turn source")
}

// hostFSLeftoverReapedResult evaluates WC-RPP-006: a session on a pack
// sharing the host, whose agent leaves a process behind, is stopped and
// started again through a SessionHandle; the start reaps the leftover
// before the new incarnation launches. The same pack without fs.host
// claims no scanner. The process-table scan reads a snapshot of the
// leftover alone, so the start can reach no process the check did not
// start. On a goos other than linux, which has no /proc to snapshot, the
// row is unsupported.
func hostFSLeftoverReapedResult(t *testing.T, goos string) Result {
	t.Helper()
	if goos != "linux" {
		return Unsupported(rppHandleProfile, RequirementRPPHostFSLeftoverReaped, "the check reads /proc snapshots, which only linux has")
	}
	if _, ok := runtime.AsProcessTableScanner(sessionexec.NewSeamBackedForCity(rppHostFSPack(t, false), "rpp-conformance", t.TempDir())); ok {
		return Fail(rppHandleProfile, RequirementRPPHostFSLeftoverReaped, "a pack without fs.host claims a process-table scanner")
	}
	proctable.SetScanRootForTesting(t, runtimetest.SnapshotProcRoot(t))
	cityPath := t.TempDir()
	sp := sessionexec.NewSeamBackedForCity(rppHostFSPack(t, true), "rpp-conformance", cityPath)
	command, wait := runtimetest.LeftoverCommand(t)
	store := beads.NewMemStore()
	manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(store), sp, sessionpkg.WithCityPath(cityPath),
		sessionpkg.WithStaleKeyDetectionWaiter(func(context.Context, string) error { return nil }))
	handle, err := worker.NewSessionHandle(worker.SessionHandleConfig{
		Manager: manager,
		Session: worker.SessionSpec{
			Template: "probe",
			Title:    "Probe",
			Command:  command,
			WorkDir:  t.TempDir(),
			Provider: "generic",
			Env:      map[string]string{"GC_CITY_PATH": cityPath},
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	by := managerActor(t, manager, sessionpkg.ActorAgent)
	ctx := context.Background()
	if err := handle.Start(ctx, by); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = handle.StopForShutdown(context.Background()) })
	leftover := wait()
	if err := handle.Stop(ctx, by); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !pidutil.AliveWithStartTime(leftover.PID, leftover.Identity) {
		t.Fatalf("the leftover pid %d did not outlive its session's stop; the check proves nothing", leftover.PID)
	}
	proctable.SetScanRootForTesting(t, runtimetest.SnapshotProcRoot(t, leftover.PID))
	if err := handle.Start(ctx, managerActor(t, manager, sessionpkg.ActorOperator)); err != nil {
		return Fail(rppHandleProfile, RequirementRPPHostFSLeftoverReaped, fmt.Sprintf("starting the session again: %v", err))
	}
	if pidutil.AliveWithStartTime(leftover.PID, leftover.Identity) {
		return Fail(rppHandleProfile, RequirementRPPHostFSLeftoverReaped,
			fmt.Sprintf("the stopped incarnation's leftover pid %d still runs after the session started again", leftover.PID))
	}
	return Pass(rppHandleProfile, RequirementRPPHostFSLeftoverReaped,
		"a start of an fs.host pack session reaps its stopped incarnation's leftover process; without fs.host the pack claims no scanner")
}

// rppGoldenPack writes the golden reference with two changes: `exec` runs
// nothing (no check drives a host tmux), and `observe` removes the box right
// after answering while $S/vanish exists, so the session vanishes between the
// handle's liveness read and its delivery. It returns the script and the
// vanish flag.
func rppGoldenPack(t *testing.T) (script, vanish string) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bodies := reference.Bodies()
	bodies["exec"] = `cat >/dev/null`
	bodies["observe"] += `; if [ -f "$S/vanish" ]; then rm -f "$S/vanish"; rm -rf "$box"; fi`
	script = filepath.Join(dir, "golden")
	if err := os.WriteFile(script, []byte(reference.Script(stateDir, bodies)), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, filepath.Join(stateDir, "vanish")
}

// rppNudge starts a session on a fresh golden pack and nudges it through a
// RuntimeHandle; vanish makes the session disappear after the handle's
// liveness read.
func rppNudge(t *testing.T, vanish bool) (worker.NudgeResult, error) {
	t.Helper()
	script, flag := rppGoldenPack(t)
	sp := sessionexec.NewSeamBackedForCity(script, "rpp-conformance", t.TempDir())
	cfg := runtime.Config{WorkDir: t.TempDir(), Command: "sleep 300", ProcessNames: []string{"sleep"}, AcceptStartupDialogs: new(bool)}
	if err := sp.Start(context.Background(), rppSessionName, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sp.Stop(rppSessionName) })
	handle, err := worker.NewRuntimeHandle(worker.RuntimeHandleConfig{
		Provider:     sp,
		SessionName:  rppSessionName,
		ProviderName: "rpp-reference",
		ProcessNames: cfg.ProcessNames,
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}
	if vanish {
		if err := os.WriteFile(flag, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return handle.Nudge(context.Background(), testAgent(t), worker.NudgeRequest{
		Text:     "check deploy status",
		Delivery: worker.NudgeDeliveryImmediate,
		Source:   "mail",
	})
}

// vanishedNudgeNotDeliveredResult evaluates WC-RPP-001: a live session takes
// the nudge, and one that vanished after the liveness read refuses it with
// ErrSessionNotFound. Setup failures fail the test loudly; only a contract
// violation is a failing result.
func vanishedNudgeNotDeliveredResult(t *testing.T) Result {
	t.Helper()
	if result, err := rppNudge(t, false); err != nil || !result.Delivered {
		return Fail(rppHandleProfile, RequirementRPPVanishedNudgeNotDelivered,
			fmt.Sprintf("nudge to a live session = (delivered=%t, %v), want delivered", result.Delivered, err))
	}
	result, err := rppNudge(t, true)
	if result.Delivered || !errors.Is(err, runtime.ErrSessionNotFound) {
		return Fail(rppHandleProfile, RequirementRPPVanishedNudgeNotDelivered,
			fmt.Sprintf("nudge to a vanished session = (delivered=%t, %v), want undelivered with runtime.ErrSessionNotFound", result.Delivered, err))
	}
	return Pass(rppHandleProfile, RequirementRPPVanishedNudgeNotDelivered,
		"a live session takes the nudge; one that vanished after the liveness read refuses it with runtime.ErrSessionNotFound")
}

// rppOutcomePack writes the golden reference in input-outcome mode over the
// dedicated driving ops (carrier.ops), whose nudge answers the word in
// $S/answer when that file exists (exit 0 for a success word, exit 80
// otherwise), so a check can make the pack answer any word. It returns the
// script and the answer file.
func rppOutcomePack(t *testing.T) (script, answer string) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bodies := reference.InputOutcomeBodies(reference.NonTmuxBodies())
	bodies["nudge"] = `if [ -f "$S/answer" ]; then cat >/dev/null; w=$(cat "$S/answer"); printf '{"outcome":"%s","detail":"the pack answered"}\n' "$w"; case "$w" in delivered*) exit 0 ;; esac; exit 80; fi; ` + bodies["nudge"]
	script = filepath.Join(dir, "golden")
	if err := os.WriteFile(script, []byte(reference.Script(stateDir, bodies)), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, filepath.Join(stateDir, "answer")
}

// rppOutcomeHandle starts a session on script and returns a RuntimeHandle
// over the real seam-backed exec provider, and the provider.
func rppOutcomeHandle(t *testing.T, script string) (*worker.RuntimeHandle, runtime.Provider) {
	t.Helper()
	sp := sessionexec.NewSeamBackedForCity(script, "rpp-conformance", t.TempDir())
	cfg := runtime.Config{WorkDir: t.TempDir(), Command: "sleep 300", ProcessNames: []string{"sleep"}, AcceptStartupDialogs: new(bool)}
	if err := sp.Start(context.Background(), rppSessionName, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sp.Stop(rppSessionName) })
	handle, err := worker.NewRuntimeHandle(worker.RuntimeHandleConfig{
		Provider:     sp,
		SessionName:  rppSessionName,
		ProviderName: "rpp-reference",
		ProcessNames: cfg.ProcessNames,
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}
	return handle, sp
}

// rppAnsweredNudge nudges a fresh outcome pack session, whose pack answers
// word, through a RuntimeHandle.
func rppAnsweredNudge(t *testing.T, word runtime.InputOutcome) (worker.NudgeResult, error) {
	t.Helper()
	script, answer := rppOutcomePack(t)
	handle, _ := rppOutcomeHandle(t, script)
	if err := os.WriteFile(answer, []byte(word), 0o644); err != nil {
		t.Fatal(err)
	}
	return handle.Nudge(context.Background(), testAgent(t), worker.NudgeRequest{
		Text:     "check deploy status",
		Delivery: worker.NudgeDeliveryImmediate,
		Source:   "mail",
	})
}

// typedOutcomeReachesHandleResult evaluates WC-RPP-002: each non-success
// word the pack answers reaches the handle as its sentinel, undelivered.
func typedOutcomeReachesHandleResult(t *testing.T) Result {
	t.Helper()
	for _, tc := range []struct {
		word runtime.InputOutcome
		want []error
	}{
		{runtime.InputOutcomeBusy, []error{runtime.ErrSessionBusy}},
		{runtime.InputOutcomeUnavailable, []error{runtime.ErrRuntimeUnavailable}},
		{runtime.InputOutcomePendingInteraction, []error{runtime.ErrNudgeRefusedPendingInteraction, sessionpkg.ErrPendingInteraction}},
		{runtime.InputOutcomeNotSent, []error{runtime.ErrNudgeNotSent}},
		{runtime.InputOutcomeRejected, []error{runtime.ErrNudgeRejected}},
		{runtime.InputOutcomeGone, []error{runtime.ErrSessionNotFound}},
	} {
		result, err := rppAnsweredNudge(t, tc.word)
		if result.Delivered || runtime.NudgeDelivered(err) {
			return Fail(rppHandleProfile, RequirementRPPTypedOutcomeReachesHandle,
				fmt.Sprintf("%s: nudge = (delivered=%t, %v), want undelivered", tc.word, result.Delivered, err))
		}
		for _, want := range tc.want {
			if !errors.Is(err, want) {
				return Fail(rppHandleProfile, RequirementRPPTypedOutcomeReachesHandle,
					fmt.Sprintf("%s: nudge error %v does not wrap %v", tc.word, err, want))
			}
		}
	}
	return Pass(rppHandleProfile, RequirementRPPTypedOutcomeReachesHandle,
		"busy, unavailable, pending_interaction, not_sent, rejected and gone each reach the handle as their sentinel, undelivered")
}

// unobservedIsDeliveredResult evaluates WC-RPP-003: delivered_unobserved
// counts as delivered, unconfirmed does not.
func unobservedIsDeliveredResult(t *testing.T) Result {
	t.Helper()
	_, err := rppAnsweredNudge(t, runtime.InputOutcomeDeliveredUnobserved)
	if !errors.Is(err, runtime.ErrNudgeDeliveredUnobserved) || !runtime.NudgeDelivered(err) {
		return Fail(rppHandleProfile, RequirementRPPUnobservedIsDelivered,
			fmt.Sprintf("delivered_unobserved: nudge error %v, want runtime.ErrNudgeDeliveredUnobserved counted as delivered", err))
	}
	result, err := rppAnsweredNudge(t, runtime.InputOutcomeUnconfirmed)
	if result.Delivered || !errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed) || runtime.NudgeDelivered(err) {
		return Fail(rppHandleProfile, RequirementRPPUnobservedIsDelivered,
			fmt.Sprintf("unconfirmed: nudge = (delivered=%t, %v), want runtime.ErrNudgeDeliveryUnconfirmed, not counted as delivered", result.Delivered, err))
	}
	return Pass(rppHandleProfile, RequirementRPPUnobservedIsDelivered,
		"delivered_unobserved counts as delivered; unconfirmed is possibly delivered and not counted")
}

// heldNudgeTypedOnceResult evaluates WC-RPP-012: a nudge sent twice through
// the handle while the pack holds the first (hold_send) is unconfirmed both
// times and typed once after the hold is released.
func heldNudgeTypedOnceResult(t *testing.T) Result {
	t.Helper()
	script, _ := rppOutcomePack(t)
	handle, sp := rppOutcomeHandle(t, script)
	const text = "check deploy status, held"
	t.Setenv("GC_RPP_FAULT", "hold_send")
	for i := 1; i <= 2; i++ {
		result, err := handle.Nudge(context.Background(), testAgent(t), worker.NudgeRequest{
			Text:     text,
			Delivery: worker.NudgeDeliveryImmediate,
			Source:   "queue",
		})
		if result.Delivered || !errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed) {
			return Fail(rppHandleProfile, RequirementRPPHeldNudgeTypedOnce,
				fmt.Sprintf("nudge %d while held = (delivered=%t, %v), want runtime.ErrNudgeDeliveryUnconfirmed", i, result.Delivered, err))
		}
	}
	t.Setenv("GC_RPP_FAULT", "")
	screen, err := sp.Peek(rppSessionName, 0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if n := strings.Count(screen, text); n != 1 {
		return Fail(rppHandleProfile, RequirementRPPHeldNudgeTypedOnce,
			fmt.Sprintf("the held nudge was typed %d times after release, want once (screen %q)", n, screen))
	}
	return Pass(rppHandleProfile, RequirementRPPHeldNudgeTypedOnce,
		"a nudge redelivered while the pack holds the first is unconfirmed both times and typed once")
}

// rppFirstTurn is the first turn WC-RPP-004 starts its session with.
const rppFirstTurn = "start on the release checklist"

// rppFirstTurnReadyDelay is the ready_delay_ms WC-RPP-004 starts with: the
// golden in launch mode has no idle boundary, so the queued turn waits it.
const rppFirstTurnReadyDelay = 300

// rppFirstTurnBatch is the delivery key WC-RPP-004's claim pins the first
// turn to and sends it under, as the queue's claim does.
const rppFirstTurnBatch = "nudge-batch-first-turn"

// firstTurnQueuedResult evaluates WC-RPP-004: a session started through a
// SessionHandle on the golden reference in launch mode (proc.provision with
// proc.launch, no exec) starts without typing its first turn, which waits in
// the session's queue once as the user's submit, not due before
// ready_delay_ms past the start, bound to the launch the runtime reads as
// running, so a claim supplying that read (LiveLaunch) takes it alone. Sent
// as the queue sends it, through the handle under the key of the batch the
// claim pinned it to and fenced by the pre-send read, it is typed once (the
// delivered half); a pack refusing it with each refusal word answers with
// that typed refusal and types nothing, so the queue keeps it for a retry
// (the refusal half).
func firstTurnQueuedResult(t *testing.T) Result {
	t.Helper()
	for _, word := range []runtime.InputOutcome{
		"", runtime.InputOutcomeBusy, runtime.InputOutcomePendingInteraction,
		runtime.InputOutcomeRejected, runtime.InputOutcomeNotSent,
	} {
		label := "accepting"
		if word != "" {
			label = "refusing " + string(word)
		}
		dir := t.TempDir()
		stateDir := filepath.Join(dir, "state")
		if err := os.Mkdir(stateDir, 0o755); err != nil {
			t.Fatal(err)
		}
		bodies := reference.LaunchBodies(reference.InputOutcomeBodies(reference.Bodies()))
		bodies["exec"] = `cat >/dev/null; exit 2`
		script := filepath.Join(dir, "golden")
		if err := os.WriteFile(script, []byte(reference.Script(stateDir, bodies)), 0o755); err != nil {
			t.Fatal(err)
		}
		fault := ""
		if word != "" {
			fault = "refuse_" + string(word)
		}
		t.Setenv("GC_RPP_FAULT", fault)
		cityPath := t.TempDir()
		sp := sessionexec.NewSeamBackedForCity(script, "rpp-conformance", cityPath)
		manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(beads.NewMemStore()), sp, sessionpkg.WithCityPath(cityPath),
			sessionpkg.WithStaleKeyDetectionWaiter(func(context.Context, string) error { return nil }))
		handle, err := worker.NewSessionHandle(worker.SessionHandleConfig{
			Manager: manager,
			Session: worker.SessionSpec{
				Template: "probe",
				Title:    "Probe",
				Command:  "sleep 300",
				WorkDir:  t.TempDir(),
				Provider: "generic",
				Hints:    runtime.Config{Nudge: rppFirstTurn, ReadyDelayMs: rppFirstTurnReadyDelay},
			},
		})
		if err != nil {
			t.Fatalf("NewSessionHandle: %v", err)
		}
		began := time.Now()
		if err := handle.Start(context.Background(), managerActor(t, manager, sessionpkg.ActorOperator)); err != nil {
			return Fail(rppHandleProfile, RequirementRPPFirstTurnQueued,
				fmt.Sprintf("%s: Start = %v, want nil: Start types nothing, so input the pack refuses never fails it", label, err))
		}
		t.Cleanup(func() { _ = handle.StopForShutdown(context.Background()) })
		if n := typedFirstTurns(stateDir); n != 0 {
			return Fail(rppHandleProfile, RequirementRPPFirstTurnQueued,
				fmt.Sprintf("%s: Start typed the first turn %d time(s): it is queued, never typed by Start", label, n))
		}
		state, err := nudgequeue.LoadState(cityPath)
		if err != nil {
			t.Fatalf("LoadState: %v", err)
		}
		var queued []nudgequeue.Item
		for _, item := range state.Pending {
			if item.Message == rppFirstTurn && item.Source == nudgequeue.SourceSubmit {
				queued = append(queued, item)
			}
		}
		if len(queued) != 1 {
			return Fail(rppHandleProfile, RequirementRPPFirstTurnQueued,
				fmt.Sprintf("%s: the session's queue holds %d copies of the first turn as the user's submit (pending %+v), want 1", label, len(queued), state.Pending))
		}
		turn := queued[0]
		floor := began.Add(rppFirstTurnReadyDelay * time.Millisecond)
		if turn.DeliverAfter.Before(floor) {
			return Fail(rppHandleProfile, RequirementRPPFirstTurnQueued,
				fmt.Sprintf("%s: the queued first turn is due at %s, before ready_delay_ms past the start (%s): a pack without an idle boundary holds it that long", label, turn.DeliverAfter, floor))
		}
		info, err := manager.Get(turn.SessionID)
		if err != nil {
			t.Fatalf("Get(%s): %v", turn.SessionID, err)
		}
		// The claim's LiveLaunch, as cmd/gc supplies it, must name the
		// launch the turn is bound to, or the queue holds the turn until it
		// expires (or drops it as another launch's).
		bound, isTurn := nudgequeue.StartupTurnLaunch(turn)
		live, known := runtime.ReadLaunchID(sp, info.SessionName)
		judgement := nudgequeue.JudgeStartupTurns(turn.SessionID, state.Pending, nil, turn.DeliverAfter,
			nudgequeue.LiveLaunch{Asked: true, ID: live, Known: known, Seen: map[string]bool{turn.ID: true}})
		if !isTurn || !known || bound != live || judgement.Claim != turn.ID {
			return Fail(rppHandleProfile, RequirementRPPFirstTurnQueued,
				fmt.Sprintf("%s: the queued first turn is bound to launch %q (startup turn %v) and the live launch reads %q (%v): a claim judges it %+v, want it claimed alone", label, bound, isTurn, live, known, judgement))
		}
		// The pre-send read the queue fences the send with.
		fence, ok := runtime.ReadInputFence(sp, info.SessionName)
		if !ok || runtime.LaunchID(fence.Token) != bound {
			return Fail(rppHandleProfile, RequirementRPPFirstTurnQueued,
				fmt.Sprintf("%s: the pre-send fence read = %s, %v; want the turn's launch, or the queue releases it on every pass", label, fence, ok))
		}
		// The queue claims the turn before it sends it, pinned to the batch
		// it delivers under: in flight, the turn still holds every send but
		// the one under that key, which carries it.
		if err := nudgequeue.WithState(cityPath, func(state *nudgequeue.State) error {
			state.Pending = slices.DeleteFunc(state.Pending, func(item nudgequeue.Item) bool { return item.ID == turn.ID })
			claimed := turn
			claimed.DeliveryBatch = rppFirstTurnBatch
			claimed.LeaseUntil = time.Now().Add(time.Minute)
			state.InFlight = append(state.InFlight, claimed)
			return nil
		}); err != nil {
			t.Fatalf("claiming the queued first turn: %v", err)
		}
		result, err := handle.Nudge(context.Background(), managerActor(t, manager, sessionpkg.ActorAgent), worker.NudgeRequest{
			Text:          turn.Message,
			Delivery:      worker.NudgeDeliveryDefault,
			Source:        "queue",
			Wake:          worker.NudgeWakeLiveOnly,
			DeliveryKey:   rppFirstTurnBatch,
			Fence:         fence,
			AllowUnfenced: true,
		})
		switch {
		case word == "" && (err != nil || !result.Delivered):
			return Fail(rppHandleProfile, RequirementRPPFirstTurnQueued,
				fmt.Sprintf("%s: sending the queued first turn = %+v, %v; want delivered", label, result, err))
		case word == "" && typedFirstTurns(stateDir) != 1:
			return Fail(rppHandleProfile, RequirementRPPFirstTurnQueued,
				fmt.Sprintf("%s: the queued first turn was typed %d times, want once", label, typedFirstTurns(stateDir)))
		case word != "" && (result.Delivered || runtime.NudgeDelivered(err) || !errors.Is(err, word.Sentinel())):
			return Fail(rppHandleProfile, RequirementRPPFirstTurnQueued,
				fmt.Sprintf("%s: sending the queued first turn = %+v, %v; want the %s refusal, which the queue retries", label, result, err, word))
		case word != "" && typedFirstTurns(stateDir) != 0:
			return Fail(rppHandleProfile, RequirementRPPFirstTurnQueued,
				fmt.Sprintf("%s: the pack typed the first turn it refused", label))
		}
	}
	return Pass(rppHandleProfile, RequirementRPPFirstTurnQueued,
		"Start types no first turn; it is queued once as the user's submit, due no earlier than ready_delay_ms past the start and bound to the launch the runtime reads, which a claim takes alone; sent fenced through the handle it is typed once, and a refusal types nothing and reaches the queue as its typed sentinel")
}

// typedFirstTurns counts rppFirstTurn in the input the golden typed into
// any box under stateDir.
func typedFirstTurns(stateDir string) int {
	n := 0
	typed, _ := filepath.Glob(filepath.Join(stateDir, "city*", "*", "typed"))
	for _, path := range typed {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line == rppFirstTurn {
				n++
			}
		}
	}
	return n
}
