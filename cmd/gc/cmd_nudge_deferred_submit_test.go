package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// deferredSubmitHarness is a started Codex session whose runtime defers the
// first submit (runtime.ErrSessionBusy), so the user's message is queued, and
// the poller's view of it once its pane is idle again.
type deferredSubmitHarness struct {
	dir    string
	store  beads.NudgesStore
	fake   *runtime.Fake
	info   session.Info
	target nudgeTarget
	obs    worker.LiveObservation
}

func newDeferredSubmitHarness(t *testing.T, prompt string) deferredSubmitHarness {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex", Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), testOperatorIn(t, dir), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The pane reads busy when the submit arrives: the runtime defers it.
	fake.NudgeErrors = map[string]error{info.SessionName: fmt.Errorf("%w: test pane busy", runtime.ErrSessionBusy)}
	outcome, err := mgr.Submit(context.Background(), testOperatorIn(t, dir), info.ID, prompt, "", runtime.Config{WorkDir: dir}, session.SubmitIntentDefault)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !outcome.Queued {
		t.Fatalf("Submit outcome = %+v, want queued (the runtime deferred it)", outcome)
	}

	// The pane goes idle again.
	fake.NudgeErrors = nil
	idleSince := time.Now().Add(-10 * time.Second)
	fake.Activity = map[string]time.Time{info.SessionName: idleSince}
	return deferredSubmitHarness{
		dir:    dir,
		store:  store,
		fake:   fake,
		info:   info,
		target: nudgeTarget{cityPath: dir, agent: config.Agent{Name: "worker"}, sessionID: info.ID, resolved: &config.ResolvedProvider{Name: "codex"}, sessionName: info.SessionName},
		obs:    worker.LiveObservation{Running: true, Alive: true, LastActivity: &idleSince},
	}
}

// deliverPass runs one poller pass and returns the texts it typed.
func (h deferredSubmitHarness) deliverPass(t *testing.T) []string {
	t.Helper()
	before := len(h.fake.Calls)
	delivered, err := tryDeliverQueuedNudgesByPoller(h.target, h.store, h.store, h.fake, 3*time.Second, h.obs)
	if err != nil || !delivered {
		t.Fatalf("tryDeliverQueuedNudgesByPoller = %v, %v; want a delivery", delivered, err)
	}
	var got []string
	for _, call := range h.fake.Calls[before:] {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			got = append(got, call.Message)
		}
	}
	return got
}

// A user's submit the runtime deferred (BB live E2E, codex-cli 0.162.1:
// {"queued": true}) is still the user's prompt. The queue's poller types it
// as the user would have, verbatim: wrapped as a deferred <system-reminder>
// ("Handle them after this turn") the agent takes it as a note, and GC's
// structured history holds no user message with the submitted text.
func TestDeferredSubmitIsDeliveredAsTheUserPrompt(t *testing.T) {
	const prompt = "User request:\nList the files in this directory."
	h := newDeferredSubmitHarness(t, prompt)

	got := h.deliverPass(t)
	if len(got) != 1 || got[0] != prompt {
		t.Fatalf("the deferred submit reached the agent as %q, want the user's prompt verbatim %q", got, prompt)
	}
}

// A deferred submit goes out alone: a reminder queued after it waits for a
// later pass and keeps its reminder wrapping, and neither text is folded into
// the other.
func TestDeferredSubmitGoesOutAloneAheadOfALaterReminder(t *testing.T) {
	const prompt = "Summarize the README."
	h := newDeferredSubmitHarness(t, prompt)
	if err := enqueueQueuedNudge(h.dir, newQueuedNudge("worker", "review the deploy logs", time.Now())); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	first := h.deliverPass(t)
	if len(first) != 1 || first[0] != prompt {
		t.Fatalf("first pass typed %q, want only the user's prompt %q", first, prompt)
	}
	second := h.deliverPass(t)
	if len(second) != 1 || !strings.Contains(second[0], "<system-reminder>") ||
		!strings.Contains(second[0], "review the deploy logs") || strings.Contains(second[0], prompt) {
		t.Fatalf("second pass typed %q, want the reminder alone, wrapped", second)
	}
	pending, inFlight, dead, err := listQueuedNudgesForTarget(h.dir, h.target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending)+len(inFlight)+len(dead) != 0 {
		t.Fatalf("queue after both passes = %d pending, %d in flight, %d dead; want empty", len(pending), len(inFlight), len(dead))
	}
}

// The UserPromptSubmit hook's drain injects queued reminders into the turn a
// prompt starts. A deferred submit is not a reminder: injected there it would
// reach the agent as hook context, never as the user's prompt. The drain
// leaves it queued for the poller, which types it.
func TestHookDrainLeavesADeferredSubmitForThePoller(t *testing.T) {
	f := newHookBudgetFixture(t)
	f.queueNudge(t)
	submit := newQueuedNudgeWithOptions(hookBudgetIdentity, "List the files in this directory.", nudgequeue.SourceSubmit, time.Now().Add(-time.Minute), queuedNudgeOptions{})
	if err := enqueueQueuedNudge(f.cityDir, submit); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	resetHookBudgetProcessState(t)
	setHookProcessStart(t, time.Now().Add(-time.Minute))
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(time.Now().Add(time.Hour)))

	var stdout, stderr bytes.Buffer
	if code := nudgeDrainInject(t, f, &stdout, &stderr); code != 0 {
		t.Fatalf("drain exit %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "review queued work") {
		t.Fatalf("drain output %q, want the queued reminder injected", stdout.String())
	}
	if strings.Contains(stdout.String(), "List the files") {
		t.Fatalf("drain output %q injected the deferred submit as a reminder", stdout.String())
	}
	pending, inFlight, dead, err := listQueuedNudges(f.cityDir, hookBudgetIdentity, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != submit.ID || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("queue after drain = pending %v, %d in flight, %d dead; want only the deferred submit pending", pending, len(inFlight), len(dead))
	}
}

// A queued delivery the runtime defers as busy is released without spending
// an attempt, so nothing else records it; the poller says so each time.
func TestBusyDeferredQueuedDeliveryIsReported(t *testing.T) {
	h := newDeferredSubmitHarness(t, "Summarize the README.")
	var warnings bytes.Buffer
	prev := nudgeWarningWriter
	nudgeWarningWriter = &warnings
	t.Cleanup(func() { nudgeWarningWriter = prev })
	h.fake.NudgeErrors = map[string]error{h.info.SessionName: fmt.Errorf("%w: test pane busy", runtime.ErrSessionBusy)}

	delivered, err := tryDeliverQueuedNudgesByPoller(h.target, h.store, h.store, h.fake, 3*time.Second, h.obs)
	if err != nil || delivered {
		t.Fatalf("tryDeliverQueuedNudgesByPoller = %v, %v; want a busy deferral, not a delivery", delivered, err)
	}
	if got := warnings.String(); !strings.Contains(got, "deferred, retry at") || !strings.Contains(got, "session is busy") {
		t.Fatalf("warnings = %q, want the busy deferral reported", got)
	}
}
