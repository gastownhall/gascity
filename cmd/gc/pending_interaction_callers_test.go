package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// These tests pin how callers answer a session waiting on a pending
// interaction (a permission prompt or a question, which typed text would
// answer): wait-idle and queued delivery queue behind it, only an explicit
// immediate delivery is refused, and a bounded retry never spends an
// attempt on the refusal.

// pendingCallerApproval is the permission prompt these tests raise.
var pendingCallerApproval = &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "Allow Bash?"}

// pendingRefusals are the two forms of the runtime's refusal to type into a
// pending interaction: the tmux provider's and the ACP provider's.
var pendingRefusals = map[string]error{
	"tmux": runtime.ErrPendingInteraction,
	"acp":  runtime.ErrNudgeRefusedPendingInteraction,
}

// pendingAtDeliveryProvider is a runtime whose session shows no prompt to
// any look a caller takes, but has raised one by the time the nudge is
// typed: while pending is set its Nudge refuses with refusal, typing
// nothing.
type pendingAtDeliveryProvider struct {
	*runtime.Fake
	refusal error
	pending bool
	refused int
}

func (p *pendingAtDeliveryProvider) Nudge(name string, content []runtime.ContentBlock) error {
	if p.pending {
		p.refused++
		return fmt.Errorf("%w: a permission prompt is open in %q", p.refusal, name)
	}
	return p.Fake.Nudge(name, content)
}

// TestBackstopRefundsAttemptOnPendingRefusal (row A7): the backstop's look
// saw no prompt and reserved the attempt, then the runtime refused to type
// into a prompt that opened before delivery. Nothing was typed, so the
// reservation is refunded as for a busy deferral: the slot is neither
// drained early nor denied its nudges, and the next quiet tick with no
// prompt delivers attempt 1.
func TestBackstopRefundsAttemptOnPendingRefusal(t *testing.T) {
	for name, refusal := range pendingRefusals {
		t.Run(name, func(t *testing.T) {
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			now := base.Add(idleClaimNudgeGrace + time.Second)
			s := staleSnapshotClaimSession(base)
			s.Metadata["instance_token"] = "tok-1"
			s.Metadata[idleClaimNudgeTokenKey] = "tok-1"
			store := beads.NewMemStoreFrom(0, []beads.Bead{s, {ID: "work-a", Status: "open"}}, nil)
			sp := &pendingAtDeliveryProvider{Fake: quietClaimFake(t, now), refusal: refusal, pending: true}
			drains := &drainRecorder{}
			snapshot := []beads.Bead{{ID: "work-a", Status: "open"}}
			var out bytes.Buffer

			ticks := idleClaimNudgeMaxAttempts + 2
			for tick := 0; tick < ticks; tick++ {
				at := now.Add(time.Duration(tick) * (idleClaimNudgeBackoff + time.Second))
				sp.SetActivity("session-a", at.Add(-time.Hour))
				nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{mustGetTestBead(t, store, s.ID)}, snapshot, nil, at, liveClaimDrain(store, drains.request), &out)
			}
			if sp.refused != ticks || sp.CountCalls("Nudge", "session-a") != 0 {
				t.Fatalf("refused %d delivered %d, want all %d ticks refused and nothing delivered (log %q)", sp.refused, sp.CountCalls("Nudge", "session-a"), ticks, out.String())
			}
			got := mustGetTestBead(t, store, s.ID)
			if got.Metadata[idleClaimNudgeCountKey] != "0" || got.Metadata[idleClaimNudgeAtKey] != base.Format(time.RFC3339) {
				t.Fatalf("marker count %q at %q, want the reservation refunded to 0 at %s", got.Metadata[idleClaimNudgeCountKey], got.Metadata[idleClaimNudgeAtKey], base.Format(time.RFC3339))
			}
			if len(drains.ids) != 0 || got.Metadata[idleClaimNudgeStalledKey] != "" {
				t.Fatalf("drains %v latch %q, want no drain for a slot whose nudges were only refused", drains.ids, got.Metadata[idleClaimNudgeStalledKey])
			}
			if !strings.Contains(out.String(), "pending interaction") {
				t.Fatalf("log %q, want the refusal logged", out.String())
			}

			sp.pending = false
			later := now.Add(time.Duration(ticks) * (idleClaimNudgeBackoff + time.Second))
			sp.SetActivity("session-a", later.Add(-time.Hour))
			nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{mustGetTestBead(t, store, s.ID)}, snapshot, nil, later, liveClaimDrain(store, drains.request), &out)
			if got := mustGetTestBead(t, store, s.ID).Metadata[idleClaimNudgeCountKey]; sp.CountCalls("Nudge", "session-a") != 1 || got != "1" {
				t.Fatalf("delivered %d count %q, want attempt 1 delivered once the prompt is answered", sp.CountCalls("Nudge", "session-a"), got)
			}
		})
	}
}

// TestDrainReminderRestoresMarkerOnPendingRefusal (row A8): a reminder the
// runtime refused to type into a prompt that opened after the hold's look
// spends no attempt: the reservation is restored and the row held as
// pending, as for a busy deferral, and the next pass delivers reminder 1.
func TestDrainReminderRestoresMarkerOnPendingRefusal(t *testing.T) {
	for name, refusal := range pendingRefusals {
		t.Run(name, func(t *testing.T) {
			e := newDrainReminderEnv(t)
			sp := &pendingAtDeliveryProvider{Fake: e.sp, refusal: refusal, pending: true}

			if got := e.remindWith(sp); got != drainReminderHeld {
				t.Fatalf("outcome = %v, want held for a refused delivery", got)
			}
			if sp.refused != 1 {
				t.Fatalf("refused = %d, want 1", sp.refused)
			}
			if got := e.meta(drainReminderCountKey); got != "0" {
				t.Errorf("%s = %q, want 0: a refusal spends no attempt", drainReminderCountKey, got)
			}
			if got := e.meta(drainReminderFailedKey); got != "0" {
				t.Errorf("%s = %q, want 0: a refusal is not an undeliverable attempt", drainReminderFailedKey, got)
			}
			if got := e.meta(drainReminderHoldKey); got != drainReminderHoldPending {
				t.Errorf("%s = %q, want %q", drainReminderHoldKey, got, drainReminderHoldPending)
			}

			sp.pending = false
			if got := e.remindWith(sp); got != drainReminderDelivered {
				t.Fatalf("outcome = %v, want reminder 1 delivered once the prompt is answered", got)
			}
			if got := e.meta(drainReminderCountKey); got != "1" {
				t.Errorf("%s = %q, want 1", drainReminderCountKey, got)
			}
		})
	}
}

// TestWarmBindSkipsWaitBehindPending (row A10): a warm slot waiting on a
// pending interaction is not waited on for 30 s nor typed into; the tick is
// skipped, and a later tick nudges it once the prompt is answered.
func TestWarmBindSkipsWaitBehindPending(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "slot-1", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.WaitForIdleErrors["slot-1"] = nil
	sp.SetPendingInteraction("slot-1", pendingCallerApproval)

	if nudgeWarmBindSlot(context.Background(), sp, "slot-1", "work-a", "claim work-a") {
		t.Fatal("nudgeWarmBindSlot = true, want the tick skipped behind the pending prompt")
	}
	if got := sp.CountCalls("WaitForIdle", "slot-1"); got != 0 {
		t.Fatalf("WaitForIdle calls = %d, want none behind the pending prompt", got)
	}
	if got := sp.CountCalls("Nudge", "slot-1"); got != 0 {
		t.Fatalf("Nudge calls = %d, want nothing typed into the prompt", got)
	}

	sp.SetPendingInteraction("slot-1", nil)
	if !nudgeWarmBindSlot(context.Background(), sp, "slot-1", "work-a", "claim work-a") {
		t.Fatal("nudgeWarmBindSlot = false, want the slot nudged once the prompt is answered")
	}
}

// pendingNudgeSessionTarget starts a claude session over a file-backed
// store, raises a permission prompt in it, and returns its nudge target.
func pendingNudgeSessionTarget(t *testing.T) (nudgeTarget, beads.Store, *runtime.Fake, string) {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude", Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), testOperatorIn(t, dir), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitForIdleErrors[info.SessionName] = nil
	fake.SetPendingInteraction(info.SessionName, pendingCallerApproval)
	prev := startNudgePoller
	startNudgePoller = func(string, string, string) error { return nil }
	t.Cleanup(func() { startNudgePoller = prev })
	return nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker"},
		resolved:    &config.ResolvedProvider{Name: "claude"},
		sessionID:   info.ID,
		sessionName: info.SessionName,
	}, store, fake, dir
}

// assertQueuedWithoutWait checks that nothing waited for idle or was typed
// into target's session, and that one nudge is queued for it.
func assertQueuedWithoutWait(t *testing.T, target nudgeTarget, fake *runtime.Fake, dir string) {
	t.Helper()
	for _, method := range []string{"WaitForIdle", "Nudge", "NudgeNow"} {
		if got := fake.CountCalls(method, target.sessionName); got != 0 {
			t.Fatalf("%s calls = %d, want none behind the pending prompt", method, got)
		}
	}
	pending, _, _, err := listQueuedNudgesForTarget(dir, target, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("listing queued nudges: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("queued %d nudges, want the nudge queued behind the prompt", len(pending))
	}
}

// TestSlingNudgeEnqueuesWithoutWaitBehindPending (row A11): gc sling's
// courtesy nudge to a session waiting on a pending interaction is queued at
// once, with no 30 s wait for an idle boundary the prompt never gives.
func TestSlingNudgeEnqueuesWithoutWaitBehindPending(t *testing.T) {
	target, store, fake, dir := pendingNudgeSessionTarget(t)
	var stdout, stderr bytes.Buffer
	deliverSlingNudge(target, fake, store, store, dir, "", &stdout, &stderr)
	assertQueuedWithoutWait(t, target, fake, dir)
	if !strings.Contains(stdout.String(), "Queued nudge for") {
		t.Fatalf("stdout %q, want the nudge reported queued; stderr %q", stdout.String(), stderr.String())
	}
}

// TestMailNotifyEnqueuesWithoutWaitBehindPending (row A12): a mail notice to
// a session waiting on a pending interaction is queued at once, with no wait
// for an idle boundary.
func TestMailNotifyEnqueuesWithoutWaitBehindPending(t *testing.T) {
	target, store, fake, dir := pendingNudgeSessionTarget(t)
	queued, err := deliverMailNotifyLegacy(target, store, fake, mailNotifyText("human"), "")
	if err != nil || !queued {
		t.Fatalf("deliverMailNotifyLegacy = %t, %v; want queued", queued, err)
	}
	assertQueuedWithoutWait(t, target, fake, dir)
}

// TestLaneImmediateNudgeRefusedBehindPending (row A4): under lane authority
// an explicit immediate nudge to a session waiting on a pending interaction
// is refused at the front door, nothing written, as the legacy path refuses
// it; wait-idle and queued delivery are still accepted for after the
// interaction is answered.
func TestLaneImmediateNudgeRefusedBehindPending(t *testing.T) {
	t.Run("immediate is refused", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.sp.SetPendingInteraction(c.name, pendingCallerApproval)
		code, stdout, stderr := c.nudge(nudgeDeliveryImmediate, "", true, cliLaneTestMessage)
		if code != 1 {
			t.Fatalf("nudge = %d, want refused; stdout %q stderr %q", code, stdout, stderr)
		}
		got := cliOpJSON(t, []byte(stdout))
		detail, _ := got["error"].(map[string]any)
		if detail["code"] != cliOpCodePendingInteraction {
			t.Fatalf("JSON %v, want error code %s", got, cliOpCodePendingInteraction)
		}
		if !strings.Contains(stderr, "waiting on a permission prompt") || !strings.Contains(stderr, "--delivery=wait-idle") {
			t.Fatalf("stderr %q, want the explanatory refusal", stderr)
		}
		if ops := c.ops(t); len(ops) != 0 {
			t.Fatalf("op rows %+v, want nothing written", ops)
		}
		if n := c.queuedItems(t); n != 0 || len(c.rings) != 0 {
			t.Fatalf("queued %d, rings %v; want nothing queued or rung", n, c.rings)
		}
	})
	t.Run("a question is refused with its own answer", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.sp.SetPendingInteraction(c.name, &runtime.PendingInteraction{RequestID: "q-1", Kind: "question", Prompt: "Which branch?"})
		code, _, stderr := c.nudge(nudgeDeliveryImmediate, "", false, cliLaneTestMessage)
		if code != 1 || !strings.Contains(stderr, "waiting on a question") {
			t.Fatalf("nudge = %d, stderr %q; want the question refusal", code, stderr)
		}
	})
	for _, mode := range []nudgeDeliveryMode{nudgeDeliveryWaitIdle, nudgeDeliveryQueue} {
		t.Run(string(mode)+" queues", func(t *testing.T) {
			c := newCLILaneCity(t)
			c.sp.SetPendingInteraction(c.name, pendingCallerApproval)
			code, stdout, stderr := c.nudge(mode, "", false, cliLaneTestMessage)
			if code != 0 || !strings.HasPrefix(stdout, "Queued nudge for ") {
				t.Fatalf("nudge = %d, stdout %q; stderr %q", code, stdout, stderr)
			}
			if op := c.only(t); op.Payload != cliLaneTestMessage {
				t.Fatalf("op %+v, want the nudge's op", op)
			}
		})
	}
	t.Run("a key that names an op replays it", func(t *testing.T) {
		c := newCLILaneCity(t)
		if code, _, stderr := c.nudge(nudgeDeliveryImmediate, "k-replay", false, cliLaneTestMessage); code != 0 {
			t.Fatalf("first nudge = %d; stderr %q", code, stderr)
		}
		c.sp.SetPendingInteraction(c.name, pendingCallerApproval)
		code, _, stderr := c.nudge(nudgeDeliveryImmediate, "k-replay", false, cliLaneTestMessage)
		if code != 0 || !strings.Contains(stderr, "replayed op") {
			t.Fatalf("rerun = %d, stderr %q; want the op replayed, not refused", code, stderr)
		}
	})
	t.Run("a look that fails refuses", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.sp.PendingErrors = map[string]error{c.name: errors.New("capture failed")}
		code, _, stderr := c.nudge(nudgeDeliveryImmediate, "", false, cliLaneTestMessage)
		if code != 1 || !strings.Contains(stderr, "capture failed") {
			t.Fatalf("nudge = %d, stderr %q; want the failed look refused", code, stderr)
		}
		if ops := c.ops(t); len(ops) != 0 {
			t.Fatalf("op rows %+v, want nothing written", ops)
		}
	})
}
