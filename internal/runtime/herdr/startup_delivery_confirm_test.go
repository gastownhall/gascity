package herdr

import (
	"context"
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
	if !strings.Contains(calls, "agent wait %5 --until working --until done --until blocked --timeout 60000") {
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
}
