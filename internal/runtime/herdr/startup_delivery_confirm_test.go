package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ── startup-delivery confirmation against the fake herdr (gas-90h) ───────────
//
// The stranded-startup outage: a freshly-spawned TUI can swallow the submit
// CR, leaving the first turn typed-but-unsubmitted while the un-waited
// `agent prompt` reports ok — the agent then idles forever with its
// instruction visible in the input box. These tests pin the confirmed
// delivery path: --wait confirmation flags on the startup prompt, an Enter
// recovery on agent_prompt_stalled, and a durable sidecar marker when even
// recovery cannot confirm the submit.

// A registered agent's startup turn must be delivered through
// `agent prompt --wait`, so herdr verifies the submission actually started a
// turn instead of reporting ok the instant the text is typed.
func TestStartupDeliveryPromptsWithConfirmation(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	want := "agent prompt %5 Run gc hook --claim --json now. --wait --until working --until done --until blocked --timeout 60000"
	if !strings.Contains(calls, want) {
		t.Fatalf("startup delivery did not request submission confirmation:\nwant %q in:\n%s", want, calls)
	}
	if strings.Contains(calls, "send-keys") {
		t.Fatalf("confirmed delivery must not fire a recovery Enter:\n%s", calls)
	}
	if v, _ := p.GetMeta("gastown__witness", metaStartupUnconfirmed); v != "" {
		t.Fatalf("confirmed delivery recorded an unconfirmed marker: %q", v)
	}
}

// agent_prompt_stalled on an agent herdr reports idle means the text was typed
// but the submit CR was swallowed. Recovery is a single explicit Enter — the
// text is already sitting in the input box, and idle is the one state where a
// bare Enter cannot do anything else — followed by a confirming wait. No
// re-prompt: retyping would double the text in the box.
func TestStartupDeliveryStallRecoversWithEnter(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_stalled")

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if get := strings.Index(calls, "agent get %5"); get < 0 || get > strings.Index(calls, "pane send-keys %5 Enter") {
		t.Fatalf("recovery Enter was not qualified by reading the agent state first:\n%s", calls)
	}
	if !strings.Contains(calls, "pane send-keys %5 Enter") {
		t.Fatalf("stalled submit was not recovered with an explicit Enter:\n%s", calls)
	}
	if !strings.Contains(calls, "agent wait %5 --until working --until done --until blocked --timeout "+strconv.Itoa(startupEnterRecoveryWaitMS)) {
		t.Fatalf("Enter recovery was not re-confirmed:\n%s", calls)
	}
	if n := strings.Count(calls, "agent prompt"); n != 1 {
		t.Fatalf("stall recovery must not re-prompt (typed text would double); prompts = %d:\n%s", n, calls)
	}
	if v, _ := p.GetMeta("gastown__witness", metaStartupUnconfirmed); v != "" {
		t.Fatalf("recovered delivery recorded an unconfirmed marker: %q", v)
	}
}

// When the stall recovery cannot confirm either, Start still succeeds (the
// session is live; failing Start would trigger a respawn storm) but the
// strand must be recorded durably on the sidecar so it is machine-visible
// and countable — stderr alone is discarded in daemon contexts.
func TestStartupDeliveryUnconfirmedRecordsSidecarMarker(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_stalled")
	setState(t, state, "wait_times_out")

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "pane send-keys %5 Enter") {
		t.Fatalf("unconfirmed delivery skipped the Enter recovery attempt:\n%s", calls)
	}
	v, err := p.GetMeta("gastown__witness", metaStartupUnconfirmed)
	if err != nil || v == "" {
		t.Fatalf("unconfirmed startup delivery left no sidecar marker (v=%q err=%v)", v, err)
	}
	if !strings.Contains(v, "agent_prompt_stalled") {
		t.Fatalf("marker does not carry the stall verdict: %q", v)
	}
}

// agent_prompt_stalled says "no state change observed", not "the box is
// idle". An agent already parked on a confirmation dialog when its first turn
// arrives is just as unchanging, and the readiness wait cannot rule that out
// (in production it reached idle 0 times in 20h). So the recovery is
// qualified by reading the state herdr actually reports: on anything but idle
// the Enter is withheld, because there it would answer the dialog rather than
// no-op on an empty input box.
func TestStartupDeliveryStallOnBlockedAgentWithholdsEnter(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_stalled")
	setState(t, state, "agent_blocked")

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if strings.Contains(calls, "send-keys") {
		t.Fatalf("stall recovery keystroked an agent sitting on a dialog:\n%s", calls)
	}
	v, err := p.GetMeta("gastown__witness", metaStartupUnconfirmed)
	if err != nil || v == "" {
		t.Fatalf("withheld recovery left no sidecar marker (v=%q err=%v)", v, err)
	}
	if !strings.Contains(v, "blocked") {
		t.Fatalf("marker does not say which state withheld the Enter: %q", v)
	}
}

// The other half of the branch: herdr reports `timeout`, not
// `agent_prompt_stalled`. Because the prompt bound exceeds herdr's fixed
// 5000ms observed-state-change window, that verdict is only reachable AFTER
// the state-change gate passed — the submit CR demonstrably landed and the
// agent simply never settled into a confirming state inside the bound. The
// input box is therefore not idle, and a bare Enter would not be the no-op
// the stall path relies on: it would go to whatever the agent is doing. So
// this half must never keystroke. Start still succeeds and the strand is
// recorded, because "submitted but unsettled" is not proof the turn is
// running.
func TestStartupDeliveryTimeoutNeverBlindEnters(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_times_out")

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if strings.Contains(calls, "send-keys") {
		t.Fatalf("timeout verdict fired a recovery Enter; herdr already observed the submit land:\n%s", calls)
	}
	if n := strings.Count(calls, "agent prompt"); n != 1 {
		t.Fatalf("timeout verdict must not re-prompt; prompts = %d:\n%s", n, calls)
	}
	v, err := p.GetMeta("gastown__witness", metaStartupUnconfirmed)
	if err != nil || v == "" {
		t.Fatalf("unsettled startup delivery left no sidecar marker (v=%q err=%v)", v, err)
	}
	if !strings.Contains(v, "timeout") {
		t.Fatalf("marker does not carry the timeout verdict: %q", v)
	}
}

// `blocked` is a confirming state, not a failure: an agent that answered its
// first turn by opening a confirmation dialog did submit. It must resolve the
// prompt wait — which requires the delivery to have asked for `--until
// blocked` — so it never reaches the recovery path where an Enter would
// answer the dialog instead of no-op on an empty box. The fake honors only
// the requested states, so dropping "blocked" from startupConfirmStates fails
// this test.
func TestStartupDeliveryBlockedConfirmsWithoutRecovery(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_blocked")

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if strings.Contains(calls, "send-keys") {
		t.Fatalf("a blocked first turn drew a recovery Enter, which would answer the dialog:\n%s", calls)
	}
	if v, _ := p.GetMeta("gastown__witness", metaStartupUnconfirmed); v != "" {
		t.Fatalf("a blocked (submitted) first turn recorded an unconfirmed marker: %q", v)
	}
}

// notReadyCfg is a first turn for a provider that declares the line it prints
// when it discards a submit because it is not ready for input yet, and the
// prompt its input box starts with, as the builtin Copilot provider does.
func notReadyCfg() runtime.Config {
	return runtime.Config{
		Command:           "copilot",
		Nudge:             "Run gc hook --claim --json now.",
		NotReadyMarkers:   []string{"policy is still loading"},
		ReadyPromptPrefix: fakeComposer,
	}
}

// A provider that is up but not yet taking turns (Copilot before its
// managed-account policy loads) consumes the submitted first turn, prints a
// not-ready line and stays idle. herdr reports agent_prompt_stalled exactly as
// for a swallowed CR, but the input box is empty, so the Enter recovery
// submits nothing and the agent never gets its instructions. A new
// not-ready marker on screen after the submit tells the two apart: the text is
// gone, so the turn is prompted again, and no Enter is sent.
func TestStartupDeliveryResubmitsTurnRejectedAsNotReady(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_not_ready_once")

	if err := p.Start(context.Background(), "gastown__mayor", notReadyCfg()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if n := strings.Count(calls, "agent prompt %5 "); n != 2 {
		t.Fatalf("a turn the agent rejected as not ready was not prompted again; prompts = %d:\n%s", n, calls)
	}
	if strings.Contains(calls, "send-keys") {
		t.Fatalf("a rejected turn drew an Enter on an empty input box:\n%s", calls)
	}
	if v, _ := p.GetMeta("gastown__mayor", metaStartupUnconfirmed); v != "" {
		t.Fatalf("a re-delivered first turn recorded an unconfirmed marker: %q", v)
	}
}

// The marker is counted, not merely found: a not-ready line already on screen
// before the submit (an earlier rejection) says nothing about this submit. If
// this one's CR was swallowed, its text still sits in the input box and a
// re-prompt would double it, so the stall takes the Enter recovery.
func TestStartupDeliveryStaleNotReadyMarkerKeepsEnterRecovery(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_stalled")
	if err := os.WriteFile(filepath.Join(state, "screen"), []byte(fakeNotReadyLine+"\n"+fakeIdleScreen), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := p.Start(context.Background(), "gastown__mayor", notReadyCfg()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if n := strings.Count(calls, "agent prompt %5 "); n != 1 {
		t.Fatalf("a not-ready line from before the submit triggered a re-prompt (typed text would double); prompts = %d:\n%s", n, calls)
	}
	if !strings.Contains(calls, "pane send-keys %5 Enter") {
		t.Fatalf("a swallowed CR was not recovered with an Enter:\n%s", calls)
	}
}

// A startup turn may itself quote a not-ready marker (a prompt telling the
// agent what to do when it sees one). If its CR is swallowed, the typed text
// stays in the input box on screen, so that marker's count rises because of
// the text itself, not a rejection. A marker the submitted text carries says
// nothing about the submit: the stall keeps its Enter recovery instead of
// typing the turn again into a box that still holds it.
func TestStartupDeliveryMarkerQuotedByTurnKeepsEnterRecovery(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_stalled_echo")

	cfg := notReadyCfg()
	cfg.Nudge = "If Copilot says policy is still loading, wait and retry. Run gc hook now."
	if err := p.Start(context.Background(), "gastown__mayor", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if n := strings.Count(calls, "agent prompt %5 "); n != 1 {
		t.Fatalf("a marker quoted by the turn itself triggered a re-prompt (typed text would double); prompts = %d:\n%s", n, calls)
	}
	if !strings.Contains(calls, "pane send-keys %5 Enter") {
		t.Fatalf("a swallowed CR was not recovered with an Enter:\n%s", calls)
	}
}

// A provider that declares no not-ready markers keeps the stall path it had:
// no pane reads, one prompt, the Enter recovery.
func TestStartupDeliveryWithoutNotReadyMarkersReadsNoPane(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_not_ready_once")

	cfg := notReadyCfg()
	cfg.NotReadyMarkers = nil
	if err := p.Start(context.Background(), "gastown__mayor", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if strings.Contains(calls, "pane read") {
		t.Fatalf("delivery read the pane for a provider with no not-ready markers:\n%s", calls)
	}
	if n := strings.Count(calls, "agent prompt %5 "); n != 1 {
		t.Fatalf("delivery re-prompted without a not-ready marker to justify it; prompts = %d:\n%s", n, calls)
	}
	if !strings.Contains(calls, "pane send-keys %5 Enter") {
		t.Fatalf("the stall lost its Enter recovery:\n%s", calls)
	}
}

// The re-prompt is a keystroke into the agent like the recovery Enter, and is
// qualified the same way: only an agent herdr reports idle gets one. An agent
// that went to a dialog after printing its not-ready line would take the
// typed turn as an answer.
func TestStartupDeliveryNotReadyOnBlockedAgentWithholdsReprompt(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_not_ready_once")
	setState(t, state, "agent_blocked")

	if err := p.Start(context.Background(), "gastown__mayor", notReadyCfg()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if n := strings.Count(calls, "agent prompt %5 "); n != 1 {
		t.Fatalf("re-prompted an agent herdr reports blocked; prompts = %d:\n%s", n, calls)
	}
	if strings.Contains(calls, "send-keys") {
		t.Fatalf("keystroked an agent herdr reports blocked:\n%s", calls)
	}
	if v, _ := p.GetMeta("gastown__mayor", metaStartupUnconfirmed); v == "" {
		t.Fatal("a withheld re-prompt left no sidecar marker")
	}
}

// An agent that stays not ready is prompted again until the not-ready budget
// runs out, never with an Enter (its box is empty after every rejection), and
// the strand is recorded with the marker that kept rejecting it, so the record
// says why. The budget is notReadyBudget from the first submit, or less when
// Start's ctx expires sooner: then re-prompting stops startupNotReadyReserve
// before the deadline, so Start returns in time. The reconciler records a
// Start that returns after its deadline as a failed start, which would turn a
// live session into a restart.
func TestStartupDeliveryNotReadyRetriesAreBounded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
		ctx    func() (context.Context, context.CancelFunc)
	}{
		{"no deadline: notReadyBudget", time.Second, func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		}},
		{"start deadline: reserve kept", startupNotReadyBudget, func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), startupNotReadyReserve+time.Second)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			p.c.notReadyBudget = tc.budget
			setState(t, state, "prompt_not_ready_always")
			ctx, cancel := tc.ctx()
			defer cancel()

			if err := p.Start(ctx, "gastown__mayor", notReadyCfg()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if ctx.Err() != nil {
				t.Fatalf("Start returned after its ctx ended (%v): re-prompting did not leave the reserve", ctx.Err())
			}
			calls := fakeCalls(t, state)
			if n := strings.Count(calls, "agent prompt %5 "); n < 2 {
				t.Fatalf("a turn rejected as not ready was not prompted again within the budget; prompts = %d:\n%s", n, calls)
			}
			if strings.Contains(calls, "send-keys") {
				t.Fatalf("a turn rejected as not ready drew an Enter on an empty input box:\n%s", calls)
			}
			v, err := p.GetMeta("gastown__mayor", metaStartupUnconfirmed)
			if err != nil || v == "" {
				t.Fatalf("a turn never accepted left no sidecar marker (v=%q err=%v)", v, err)
			}
			if !strings.Contains(v, "policy is still loading") {
				t.Fatalf("marker does not say the agent kept rejecting the turn as not ready: %q", v)
			}
		})
	}
}

// How long an agent stays not ready is the provider's business: Copilot
// loading its managed-account policy has taken 30-60 s and more, longer than
// a fixed handful of attempts covers. Re-prompting is bounded by time, not a
// count, so an agent that rejects the turn more often than six times still
// gets it.
func TestStartupDeliveryNotReadyBeyondSixRejectionsStillDelivers(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	if err := os.WriteFile(filepath.Join(state, "prompt_not_ready_times"), []byte("8"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := p.Start(context.Background(), "gastown__mayor", notReadyCfg()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if n := strings.Count(calls, "agent prompt %5 "); n != 9 {
		t.Fatalf("prompts = %d, want 9 (8 rejections, then the accepted turn):\n%s", n, calls)
	}
	if strings.Contains(calls, "send-keys") {
		t.Fatalf("a turn rejected as not ready drew an Enter on an empty input box:\n%s", calls)
	}
	if v, _ := p.GetMeta("gastown__mayor", metaStartupUnconfirmed); v != "" {
		t.Fatalf("a turn the agent finally took recorded an unconfirmed marker: %q", v)
	}
}

// herdr's idle verdict only says the agent is not working or blocked, so the
// first turn can be typed before the agent's TUI is up (a Copilot
// launched through a wrapper came up 17 s after its pane). That submit stalls
// with nothing on screen, so there is no rejection to see yet and the stall
// takes the Enter recovery. When the TUI comes up it reads the typed-ahead
// turn, and a recovery Enter submits it while the agent is still not ready: the
// agent consumes it and prints its not-ready line. From then on the box is
// empty and every further Enter submits nothing, so the recovery must notice
// the rejection after each Enter and prompt the turn again.
func TestStartupDeliveryResubmitsTurnRejectedDuringEnterRecovery(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	rejectingEnter := min(2, startupEnterRecoveryAttempts)
	if err := os.WriteFile(filepath.Join(state, "enter_rejects_not_ready"), []byte(strconv.Itoa(rejectingEnter)), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := p.Start(context.Background(), "gastown__mayor", notReadyCfg()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if n := strings.Count(calls, "agent prompt %5 "); n != 2 {
		t.Fatalf("a turn rejected as not ready during the Enter recovery was not prompted again; prompts = %d:\n%s", n, calls)
	}
	if n := strings.Count(calls, "pane send-keys %5 Enter"); n != rejectingEnter {
		t.Fatalf("Enters = %d, want %d: the recovery kept pressing Enter on the empty box after the rejection:\n%s", n, rejectingEnter, calls)
	}
	if v, _ := p.GetMeta("gastown__mayor", metaStartupUnconfirmed); v != "" {
		t.Fatalf("a re-delivered first turn recorded an unconfirmed marker: %q", v)
	}
}

// The recovery's rejection check counts against the screen from before the
// first submit, like the check at the stall: a not-ready line already there
// says nothing about this turn, whose text may still be in the box. A recovery
// that never confirms keeps its one prompt and records the strand.
func TestStartupDeliveryStaleNotReadyMarkerDoesNotRepromptDuringRecovery(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_stalled")
	setState(t, state, "wait_times_out")
	if err := os.WriteFile(filepath.Join(state, "screen"), []byte(fakeNotReadyLine+"\n"+fakeIdleScreen), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := p.Start(context.Background(), "gastown__mayor", notReadyCfg()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if n := strings.Count(calls, "agent prompt %5 "); n != 1 {
		t.Fatalf("a not-ready line from before the submit triggered a re-prompt during the recovery; prompts = %d:\n%s", n, calls)
	}
	if n := strings.Count(calls, "pane send-keys %5 Enter"); n != startupEnterRecoveryAttempts {
		t.Fatalf("Enters = %d, want all %d recovery attempts:\n%s", n, startupEnterRecoveryAttempts, calls)
	}
	if v, _ := p.GetMeta("gastown__mayor", metaStartupUnconfirmed); v == "" {
		t.Fatal("a recovery that never confirmed left no sidecar marker")
	}
}

// A new not-ready line is not a rejection while the input box still holds
// text. A turn typed before the agent's TUI was up is checked
// against a screen with nothing on it, so anything the TUI draws later counts
// as new: an old warning in a resumed conversation's history, for one. If the
// TUI lost the turn's CR, the turn is still in the box, and prompting it again
// would type it twice. The box is read off the live composer, the last line
// that starts with the provider's ready prompt. These cases draw the line above
// a box that still holds the turn: at the first stall (Enter 0), and after the
// first recovery Enter. Either way the stall keeps its Enter recovery, the turn
// is prompted once, and the strand is recorded.
func TestStartupDeliveryMarkerAboveHeldTurnIsNotARejection(t *testing.T) {
	for _, at := range []int{0, 1} {
		t.Run("at Enter "+strconv.Itoa(at), func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			if err := os.WriteFile(filepath.Join(state, "marker_above_held_turn"), []byte(strconv.Itoa(at)), 0o644); err != nil {
				t.Fatal(err)
			}

			if err := p.Start(context.Background(), "gastown__mayor", notReadyCfg()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			calls := fakeCalls(t, state)
			if n := strings.Count(calls, "agent prompt %5 "); n != 1 {
				t.Fatalf("a turn still in the input box was prompted again (it would be typed twice); prompts = %d:\n%s", n, calls)
			}
			if n := strings.Count(calls, "pane send-keys %5 Enter"); n != startupEnterRecoveryAttempts {
				t.Fatalf("Enters = %d, want all %d recovery attempts for the turn in the box:\n%s", n, startupEnterRecoveryAttempts, calls)
			}
			if v, _ := p.GetMeta("gastown__mayor", metaStartupUnconfirmed); v == "" {
				t.Fatal("a recovery that never confirmed left no sidecar marker")
			}
		})
	}
}

// composerHoldsText reads the input box off Copilot CLI screens as herdr's
// `pane read --source visible` returned them: the live composer is the last
// line that starts with the prompt, below the transcript's echoes of past
// turns, and an empty one is the bare prompt.
func TestComposerHoldsText(t *testing.T) {
	const rule = "───────────────────────────────────────────"
	for _, tc := range []struct {
		name           string
		screen, prompt string
		holds, seen    bool
	}{
		{"empty below a past turn's echo", " ❯ Startup check 3: reply with exactly TOKEN-3 and nothing else.      00:08\n\n ● TOKEN-3\n\n" + rule + "\n❯\n" + rule + "\n ● Working · esc interrupt\n", fakeComposer, false, true},
		{"holding the typed turn", " ! MCP server needs authentication.\n\n" + rule + "\r\n❯ Startup check 1: reply with exactly TOKEN-1 and nothing else.\r\n\r\n" + rule + "\r\n Interactive · Allow All\r\n", fakeComposer, true, true},
		{"holding text after an NBSP", "❯\u00a0Run gc hook now.\n", fakeComposer, true, true},
		{"no composer yet", "PS C:\\work> copilot\n", fakeComposer, false, false},
		{"no prompt declared", fakeIdleScreen, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if holds, seen := composerHoldsText(tc.screen, tc.prompt); holds != tc.holds || seen != tc.seen {
				t.Fatalf("composerHoldsText = (holds %v, observed %v), want (%v, %v)", holds, seen, tc.holds, tc.seen)
			}
		})
	}
}

// ── Start's deadline bounds the startup delivery ─────────────────────────────
//
// The reconciler records a Start that returns after its ctx's deadline as a
// failed start, even when the session is live. So every herdr wait the
// startup delivery starts must end startupDeadlineMargin before that
// deadline, and a keystroke is sent only when there is time to confirm it.

// startupConfirmTimeouts returns the --timeout of each confirming herdr wait in
// calls: the startup prompt's and the Enter recovery's.
func startupConfirmTimeouts(t *testing.T, calls string) []int {
	t.Helper()
	var timeouts []int
	for _, m := range regexp.MustCompile(`(?m)--until working --until done --until blocked --timeout (-?\d+)$`).FindAllStringSubmatch(calls, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatal(err)
		}
		timeouts = append(timeouts, n)
	}
	return timeouts
}

// The prompt's confirming wait and each Enter recovery wait are capped by the
// time left before Start's deadline, less startupDeadlineMargin, and never
// below herdr's own state-change window: a shorter --wait bound turns herdr's
// agent_prompt_stalled into a plain timeout.
func TestStartupDeliveryBoundsHerdrTimeoutsByStartDeadline(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_stalled")
	setState(t, state, "wait_times_out")
	budget := startupDeadlineMargin + herdrStateChangeWindowMS*time.Millisecond + 10*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(ctx, "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	timeouts := startupConfirmTimeouts(t, calls)
	if len(timeouts) < 2 {
		t.Fatalf("want the prompt's and the recovery's confirming waits, got %v:\n%s", timeouts, calls)
	}
	limit := int((budget - startupDeadlineMargin).Milliseconds())
	for _, ms := range timeouts {
		if ms > limit || ms < herdrStateChangeWindowMS {
			t.Fatalf("a confirming wait ran with --timeout %d, want %d..%d (Start's %s deadline less its %s margin, and herdr's state-change window):\n%s",
				ms, herdrStateChangeWindowMS, limit, budget, startupDeadlineMargin, calls)
		}
	}
}

// With too little of Start's ctx left for herdr to confirm a submit, the turn
// is still sent, without --wait, and recorded as unconfirmed, naming the
// deadline. A --wait prompt would outlive the ctx, and skipping the turn
// would leave the agent idle with no instructions.
func TestStartupDeliveryWithoutTimeToConfirmSendsTheTurnUnconfirmed(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), startupDeadlineMargin+herdrStateChangeWindowMS*time.Millisecond)
	defer cancel()

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(ctx, "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("Start returned after its ctx ended: %v", ctx.Err())
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "agent prompt %5 Run gc hook --claim --json now.\n") {
		t.Fatalf("the turn was not sent unconfirmed:\n%s", calls)
	}
	if strings.Contains(calls, "--wait") || strings.Contains(calls, "send-keys") {
		t.Fatalf("a delivery with no time to confirm still waited or keystroked:\n%s", calls)
	}
	v, err := p.GetMeta("gastown__witness", metaStartupUnconfirmed)
	if err != nil || !strings.Contains(v, "deadline") {
		t.Fatalf("the unconfirmed turn's record does not name Start's deadline (v=%q err=%v)", v, err)
	}
}

// The Enter recovery is bounded like the prompt: an Enter goes only where its
// confirming wait can run herdr's state-change window and still end
// startupDeadlineMargin before Start's deadline. A late re-prompt that stalls
// with nothing on screen takes this recovery; unbounded, its waits outlived the
// ctx (10 x 6 s on Windows, one 60 s wait elsewhere) and the reconciler recorded
// a failed start for a live session.
func TestStartupRecoveryWithholdsEnterWithoutTimeToConfirm(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	setState(t, state, "registered")
	ctx, cancel := context.WithTimeout(context.Background(), startupDeadlineMargin+herdrStateChangeWindowMS*time.Millisecond)
	defer cancel()

	stalled := errors.New("agent_prompt_stalled")
	marker, err := p.c.recoverStalledStartup(ctx, "%5", stalled, func() (string, error) { return "", nil })
	if err == nil || marker != "" {
		t.Fatalf("recovery = (%q, %v), want it unconfirmed", marker, err)
	}
	if calls := fakeCalls(t, state); strings.Contains(calls, "send-keys") {
		t.Fatalf("an Enter went out with no time left to confirm it:\n%s", calls)
	}
	if !strings.Contains(err.Error(), "deadline") || !errors.Is(err, stalled) {
		t.Fatalf("error %q does not carry the stall and name Start's deadline", err)
	}
}

// A herdr call still running when its ctx ends is killed, and a killed
// process's exit status ("exit status 1" on Windows) does not say why. The
// error names the ctx's end instead, so whatever records it (the unconfirmed
// startup marker, the reconciler) sees that the deadline cut the call short.
func TestHerdrCallCutShortByItsContextReportsTheContextError(t *testing.T) {
	for _, tc := range []struct {
		name string
		verb string
		call func(ctx context.Context, c *client) error
	}{
		{"run", "agent wait %5", func(ctx context.Context, c *client) error {
			_, err := c.run(ctx, "agent", "wait", "%5", "--until", "working", "--timeout", "6000")
			return err
		}},
		{"runRaw", "pane read %5", func(ctx context.Context, c *client) error {
			_, err := c.paneRead(ctx, "%5", "visible", 0)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			setState(t, state, "hang")
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			err := tc.call(ctx, p.c)
			if calls := fakeCalls(t, state); !strings.Contains(calls, tc.verb) {
				t.Fatalf("herdr never ran, so nothing was cut short:\n%s", calls)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want it to wrap %v", err, context.DeadlineExceeded)
			}
		})
	}
}

// Raw-exec sessions never register an agent, so there is no prompt machinery
// to confirm against: the legacy paste+settle+Enter path stays, best-effort
// by construction, and must not record an unconfirmed marker.
func TestStartupDeliveryUnregisteredPaneFallsBackToPaste(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)

	cfg := runtime.Config{Command: "python3 worker.py", Nudge: "Read your brief."}
	if err := p.Start(context.Background(), "gastown__worker", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "pane run %5 Read your brief.") {
		t.Fatalf("unregistered pane did not fall back to paste:\n%s", calls)
	}
	if !strings.Contains(calls, "pane send-keys %5 Enter") {
		t.Fatalf("paste fallback did not submit with Enter:\n%s", calls)
	}
	if v, _ := p.GetMeta("gastown__worker", metaStartupUnconfirmed); v != "" {
		t.Fatalf("best-effort raw delivery must not record an unconfirmed marker: %q", v)
	}
}

// A marker left by a prior life (controller crash: Stop never wiped the
// sidecar) must not outlive a life whose delivery confirms — a stale marker
// would false-positive the named-session delivery backstop built on it.
func TestStartupDeliveryConfirmedClearsStaleMarker(t *testing.T) {
	p, _ := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	if err := p.SetMeta("gastown__witness", metaStartupUnconfirmed, "stale prior-life record"); err != nil {
		t.Fatal(err)
	}

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if v, _ := p.GetMeta("gastown__witness", metaStartupUnconfirmed); v != "" {
		t.Fatalf("stale unconfirmed marker survived a confirmed delivery: %q", v)
	}
}

// The clear cannot hang off the delivery block, or it inherits that block's
// guard. Adoption is the first escape: Start attaches to a live holder and
// deliberately skips delivery, so a marker a crashed prior life left behind
// would survive into a session Start has just declared live and primed —
// exactly the false positive the delivery backstop reading this key would
// act on.
func TestStartupDeliveryClearsStaleMarkerOnAdopt(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "name_taken")
	if err := p.SetMeta("gastown__witness", metaStartupUnconfirmed, "stale prior-life record"); err != nil {
		t.Fatal(err)
	}

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if strings.Contains(calls, "agent prompt") {
		t.Fatalf("adopted a live holder but still delivered a startup turn:\n%s", calls)
	}
	if v, _ := p.GetMeta("gastown__witness", metaStartupUnconfirmed); v != "" {
		t.Fatalf("stale unconfirmed marker survived an adopting Start: %q", v)
	}
}

// The second escape: a session with no startup text at all. Nothing is
// delivered, so nothing in this life can justify a marker, and a prior life's
// must not be inherited.
func TestStartupDeliveryClearsStaleMarkerWithNoStartupText(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	if err := p.SetMeta("gastown__witness", metaStartupUnconfirmed, "stale prior-life record"); err != nil {
		t.Fatal(err)
	}

	if err := p.Start(context.Background(), "gastown__witness", runtime.Config{Command: "claude"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if strings.Contains(calls, "agent prompt") {
		t.Fatalf("delivered a startup turn for a session with no startup text:\n%s", calls)
	}
	if v, _ := p.GetMeta("gastown__witness", metaStartupUnconfirmed); v != "" {
		t.Fatalf("stale unconfirmed marker survived a Start with no startup text: %q", v)
	}
}

// waitForIdleOutcome makes the startup readiness guard legible: the live city
// measured 0 ok / 8 timeout / 2 error across 20h with every verdict discarded.
// The interface-facing WaitForIdle keeps its proceed-either-way contract; the
// outcome lets Start record WHY a strand happened.
func TestWaitForIdleOutcomeMapping(t *testing.T) {
	p, state := newFakeHerdrProvider(t)

	if got := p.waitForIdleOutcome(context.Background(), "gastown__witness", time.Second); got != idleWaitNoAgent {
		t.Fatalf("unregistered agent outcome = %q; want %q", got, idleWaitNoAgent)
	}
	setState(t, state, "registered")
	if got := p.waitForIdleOutcome(context.Background(), "gastown__witness", time.Second); got != idleWaitReached {
		t.Fatalf("idle agent outcome = %q; want %q", got, idleWaitReached)
	}
	setState(t, state, "wait_times_out")
	if got := p.waitForIdleOutcome(context.Background(), "gastown__witness", time.Second); got != idleWaitTimeout {
		t.Fatalf("timed-out wait outcome = %q; want %q", got, idleWaitTimeout)
	}
	if calls := fakeCalls(t, state); !strings.Contains(calls, "--until idle --until done --timeout") {
		t.Fatalf("idle wait must also accept a finished Claude turn (done):\n%s", calls)
	}
}
