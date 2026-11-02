package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// An open AskUserQuestion dialog reaches the controller as a pending
// interaction of kind "question": the operator answers it in the terminal, and
// nothing typed at the pane may land on it. Every typed-nudge path here must
// hold off while one is open, and none may spend a delivery attempt on it.

// questionInteraction is what the tmux runtime reports for an open dialog.
func questionInteraction() *runtime.PendingInteraction {
	return &runtime.PendingInteraction{
		RequestID: "tmux-question-0123456789abcdef",
		Kind:      "question",
		Prompt:    "be-tx30v: How do you want to proceed?",
	}
}

// questionPendingFake is approvalPendingFake with an open question dialog.
func questionPendingFake(t *testing.T) (*runtime.Fake, nudgeTarget) {
	t.Helper()
	fake, target := approvalPendingFake(t)
	fake.SetPendingInteraction(target.sessionName, questionInteraction())
	return fake, target
}

// The incident path (gm-55kp2u): a queued "Work slung. Check your hook." for a
// session parked on an AskUserQuestion. The poller passed quiescence (pane
// static for 45 minutes), claimed the nudge, and SendLiveOnly typed it plus
// Enter, selecting option 1. The nudge must stay queued, with no attempt
// charged, and wait the shared 15s release before the next pass.
func TestPollerHoldsQueuedNudgeWhileQuestionInteractionPending(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_HOME", t.TempDir())
	dir := t.TempDir()
	if err := enqueueQueuedNudge(dir, newQueuedNudge("deployer", "Work slung. Check your hook.", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "deployer", Title: "Deployer", Command: "claude", WorkDir: dir, Provider: "claude", Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), testOperatorIn(t, dir), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	idleSince := time.Now().Add(-45 * time.Minute) // question drawn, then nothing
	fake.Activity = map[string]time.Time{info.SessionName: idleSince}
	fake.SetPendingInteraction(info.SessionName, questionInteraction())
	// A running session's handle is runtime-only, so the provider is what
	// refuses the typed text; runtime.Fake does not, so it stands in for the
	// tmux refusal (the tmux tests prove that provider refuses a question).
	fake.NudgeErrors = map[string]error{info.SessionName: fmt.Errorf("%w: %s is waiting on a question dialog", runtime.ErrPendingInteraction, info.SessionName)}
	fake.Calls = nil

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "deployer"},
		sessionID:   info.ID,
		resolved:    &config.ResolvedProvider{Name: "claude"},
		sessionName: info.SessionName,
	}
	obs := worker.LiveObservation{Running: true, Alive: true, LastActivity: &idleSince}
	before := time.Now()
	delivered, err := tryDeliverQueuedNudgesByPoller(target, store, store, fake, 3*time.Second, obs)
	if err != nil {
		t.Fatalf("tryDeliverQueuedNudgesByPoller: %v", err)
	}

	if delivered {
		t.Error("delivered = true, want false while the question is pending")
	}
	pending, inFlight, dead, err := listQueuedNudges(dir, "deployer", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 1/0/0: queued, not acked, not dead-lettered", len(pending), len(inFlight), len(dead))
	}
	if pending[0].Attempts != 0 {
		t.Errorf("Attempts = %d, want 0: holding for an open question is not a failed delivery", pending[0].Attempts)
	}
	// The dialog stays up until someone answers it; the next pass must not
	// claim the item again at once.
	if earliest := before.Add(defaultQueuedNudgeRetryDelay - time.Second); pending[0].DeliverAfter.Before(earliest) {
		t.Errorf("DeliverAfter = %v, want it released for at least %s (not before %v)", pending[0].DeliverAfter, defaultQueuedNudgeRetryDelay, earliest)
	}
}

// A wait-idle nudge queues behind an open question and says why, in words that
// fit a question: it is not a permission prompt.
func TestDeliverSessionNudgeWaitIdleQueuesBehindPendingQuestion(t *testing.T) {
	fake, target := questionPendingFake(t)
	prev := startNudgePoller
	startNudgePoller = func(string, string, string) error { return nil }
	t.Cleanup(func() { startNudgePoller = prev })

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithProvider(target, fake, nudgeDeliveryWaitIdle, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithProvider = %d, want 0; stderr: %s", code, stderr.String())
	}
	for _, call := range fake.Calls {
		switch call.Method {
		case "Nudge", "NudgeNow", "SendKeys":
			t.Fatalf("session received %s %q while a question was pending", call.Method, call.Message)
		}
	}
	out := stdout.String()
	if !strings.Contains(out, "Queued nudge for worker") || !strings.Contains(out, "question") {
		t.Fatalf("stdout = %q, want a queued confirmation that names the question", out)
	}
	if strings.Contains(out, "permission prompt") {
		t.Fatalf("stdout = %q calls a question dialog a permission prompt", out)
	}
	pending, _, _, err := listQueuedNudges(target.cityPath, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("queued nudges = %d, want 1", len(pending))
	}
}

// An immediate nudge cannot wait, so it refuses. The explanation says the
// session is waiting on a question answered in its terminal, and does not send
// the operator to approve or deny something that has no such verbs.
func TestDeliverSessionNudgeImmediateRefusesPendingQuestion(t *testing.T) {
	fake, target := questionPendingFake(t)

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithProvider(target, fake, nudgeDeliveryImmediate, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("deliverSessionNudgeWithProvider = 0, want non-zero; stdout: %s", stdout.String())
	}
	for _, call := range fake.Calls {
		switch call.Method {
		case "Nudge", "NudgeNow", "SendKeys":
			t.Fatalf("session received %s %q while a question was pending", call.Method, call.Message)
		}
	}
	text := stderr.String()
	for _, want := range []string{"question", "tmux-question-0123456789abcdef", "gc session attach", "--delivery=queue"} {
		if !strings.Contains(text, want) {
			t.Errorf("stderr = %q, want it to mention %q", text, want)
		}
	}
	for _, banned := range []string{"permission prompt", "pprove", "respond"} {
		if strings.Contains(text, banned) {
			t.Errorf("stderr = %q mentions %q, which does not apply to a question", text, banned)
		}
	}
}

// The claim backstop reserves an attempt before it types. Pacing an attempt
// against a pane that cannot take one burns the bounded budget and ends in the
// drain, so an open question holds the backstop BEFORE the reserve, under its
// own breadcrumb, and the budget is intact once the dialog is answered.
func TestExecutionBackstopHoldsBeforeReserveWhileQuestionPending(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	f.tick(t) // first sighting: grace clock starts

	f.sp.SetPendingInteraction(f.sessName, questionInteraction())
	f.sp.Calls = nil
	for i := 0; i < idleClaimNudgeMaxAttempts+2; i++ {
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}
	if got := f.nudgesDelivered(); len(got) != 0 {
		t.Fatalf("nudges delivered into an open question = %q, want none", got)
	}
	if got := f.sessionMeta(t, executionClaimNudgeCountKey); got != "" && got != "0" {
		t.Fatalf("attempt count = %q, want none reserved while the question is open", got)
	}
	if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, "pending_interaction ") {
		t.Fatalf("hold breadcrumb = %q, want the pending_interaction reason", got)
	}
	if n := f.stalledEvents(); n != 0 || len(f.drained) != 0 {
		t.Fatalf("stalled events = %d, drained = %v; holding for a question must not escalate", n, f.drained)
	}

	// Answered: the full budget is still there and the breadcrumb clears.
	f.sp.SetPendingInteraction(f.sessName, nil)
	f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimNudgeCountKey); got != "1" {
		t.Fatalf("attempt count after the dialog was answered = %q, want 1", got)
	}
	if got := f.sessionMeta(t, executionClaimHoldKey); got != "" {
		t.Fatalf("hold breadcrumb after the dialog was answered = %q, want cleared", got)
	}
}

// A drain reminder spends one of a few attempts, and its marker is written
// ahead of the nudge. With a question open the reminder is held before the
// marker, records why once, and writes nothing on later ticks.
func TestDrainReminderHoldsWhileQuestionPending(t *testing.T) {
	e := newDrainReminderEnv(t)
	e.sp.SetPendingInteraction(e.name, questionInteraction())

	if got := e.remind(); got != drainReminderHeld {
		t.Fatalf("outcome = %v, want held", got)
	}
	if n := len(e.nudges()); n != 0 {
		t.Errorf("nudge count = %d, want 0", n)
	}
	if got := e.meta(drainReminderHoldKey); got != "pending_interaction" {
		t.Errorf("%s = %q, want pending_interaction", drainReminderHoldKey, got)
	}
	if got := e.meta(drainReminderCountKey); got != "" {
		t.Errorf("%s = %q, want unset: the hold comes before the marker write", drainReminderCountKey, got)
	}
	if e.out.Len() == 0 {
		t.Error("the hold transition left no log line")
	}

	// A standing hold writes nothing and logs nothing.
	logged := e.out.Len()
	before := e.beadSnapshot()
	for i := 0; i < 3; i++ {
		if got := e.remind(); got != drainReminderHeld {
			t.Fatalf("tick %d outcome = %v, want held", i+2, got)
		}
	}
	e.assertBeadUnchanged(before)
	if e.out.Len() != logged {
		t.Errorf("a standing hold logged %q, want no output per tick", e.out.String()[logged:])
	}

	// Answered: the reminder goes out.
	e.sp.SetPendingInteraction(e.name, nil)
	if got := e.remind(); got != drainReminderDelivered {
		t.Fatalf("outcome after the dialog was answered = %v, want delivered", got)
	}
	if n := len(e.nudges()); n != 1 {
		t.Errorf("nudge count after the dialog was answered = %d, want 1", n)
	}
}
