package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// modelSwitchModalPane is the Codex/GPT mid-session "approaching rate limits"
// modal as it renders in a pane. Enter confirms the highlighted *Switch* option
// — which is exactly why raw nudge text plus Enter must never reach it.
const modelSwitchModalPane = "Approaching rate limits\n" +
	"Switch to gpt-5.4-mini for lower credit usage?\n" +
	"  1. Switch to gpt-5.4-mini\n" +
	"  2. Keep current model\n" +
	"Press enter to confirm or esc to go back"

// fencePaneExecutor answers the nudge path's tmux calls by rule and models the
// two state changes the fence can make: a copy-mode cancel leaves the pane, and
// a modal dismissal advances the pane content. Rules beat a fixed output queue
// here because the number of calls is exactly what these tests are asserting on.
type fencePaneExecutor struct {
	session   string
	attached  string   // #{session_attached} reported by the census and the target probe
	inMode    string   // #{pane_in_mode}, cleared by a bound `-X cancel`
	dead      string   // #{pane_dead}; "1" is a remain-on-exit corpse
	panes     []string // capture-pane answers; a dismissal advances to the next, the last repeats
	fenced    bool     // every guarded effect reports the fence marker
	effectErr error    // every guarded effect fails with this error instead
	calls     [][]string
	paneAt    int
}

func (e *fencePaneExecutor) pane() string {
	if len(e.panes) == 0 {
		return "ready >"
	}
	if e.paneAt >= len(e.panes) {
		return e.panes[len(e.panes)-1]
	}
	return e.panes[e.paneAt]
}

func (e *fencePaneExecutor) execute(args []string) (string, error) {
	e.calls = append(e.calls, append([]string(nil), args...))
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "if-shell"):
		if e.effectErr != nil {
			return "", e.effectErr
		}
		if e.fenced {
			return boundInputFenceMarker, nil
		}
		if strings.Contains(joined, "-X cancel") {
			e.inMode = "0"
		}
		if strings.Contains(joined, " Down ") {
			e.paneAt++
		}
		return boundInputDeliveredMarker, nil
	case strings.Contains(joined, "capture-pane"):
		return e.pane(), nil
	case strings.Contains(joined, "show-environment") && strings.Contains(joined, "GC_PROVIDER"):
		return "GC_PROVIDER=codex", nil
	case strings.Contains(joined, "list-panes") && strings.Contains(joined, "session_attached"):
		return strings.Join([]string{"$7", e.session, "@3", "%9", e.attached, e.inMode, "0"}, "\t"), nil
	case strings.Contains(joined, "list-panes"):
		return "%9\tsh\t123", nil
	case strings.Contains(joined, "display-message") && strings.Contains(joined, "session_id"):
		dead := e.dead
		if dead == "" {
			dead = "0"
		}
		return strings.Join([]string{"$7", e.session, "@3", "%9", e.attached, e.inMode, dead}, "\t"), nil
	case strings.Contains(joined, "list-windows"):
		return "123", nil
	default:
		return "", nil
	}
}

func (e *fencePaneExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

func (e *fencePaneExecutor) provider(cfg Config) *Provider {
	tm := NewTmuxWithConfig(cfg)
	tm.exec = e
	return &Provider{tm: tm}
}

// unguardedInput reports the first call that carries raw input outside a
// server-queued if-shell guard.
func unguardedInput(calls [][]string) []string {
	for _, call := range calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "if-shell") {
			continue
		}
		if strings.Contains(joined, "send-keys") || strings.Contains(joined, "paste-buffer") {
			return call
		}
	}
	return nil
}

// A nudge that lands on a visible model-switch modal types its text into the
// modal and submits it — Enter confirms the highlighted "Switch to <cheaper
// model>" option, so the nudge silently downgrades the agent's model (or wedges
// the session on the dialog). tmux cannot see a TUI modal, so neither the pane
// census nor the server-queued predicate can fence it: the modal has to be
// cleared before any payload reaches the pane, with input that is certified
// exactly like the payload's. This is the #3916 / ga-3syh production signature.
func TestNudgeDismissesModelSwitchModalBeforeDeliveringPayload(t *testing.T) {
	fe := &fencePaneExecutor{
		session:  "worker",
		attached: "0",
		inMode:   "0",
		panes:    []string{modelSwitchModalPane, "ready >"},
	}
	p := fe.provider(Config{})

	if err := p.NudgeNow("worker", runtime.TextContent("claim-your-work")); err != nil {
		t.Fatalf("NudgeNow against a model-switch modal: %v", err)
	}

	dismiss := callIndexWithTokens(fe.calls, "if-shell", "-F")
	deliver := -1
	for i, call := range fe.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "paste-buffer") {
			deliver = i
			break
		}
		if strings.Contains(joined, " Down ") && strings.Contains(joined, "if-shell") {
			dismiss = i
		}
	}
	if dismiss < 0 || deliver < 0 || dismiss >= deliver {
		t.Fatalf("dismissal idx=%d payload idx=%d: the nudge typed into a visible modal; calls=%#v", dismiss, deliver, fe.calls)
	}
	if call := unguardedInput(fe.calls); call != nil {
		t.Fatalf("modal dismissal escaped the guard: %#v", call)
	}
}

// Nudging a remain-on-exit corpse is a no-op, not a failure. Signaling is
// best-effort — a pane with no process cannot lose the text — and tmux refuses
// `paste-buffer` on a dead pane ("target pane has exited") where the `send-keys`
// this path replaced exited 0. Interrupt leaves exactly such a corpse behind, so
// treating it as an error makes an interrupt-then-nudge sequence fail.
func TestNudgeOnDeadPaneIsANoOp(t *testing.T) {
	fe := &fencePaneExecutor{session: "worker", attached: "0", inMode: "0", dead: "1", panes: []string{"ready >"}}
	p := fe.provider(Config{})

	if err := p.NudgeNow("worker", runtime.TextContent("claim-your-work")); err != nil {
		t.Fatalf("NudgeNow on a dead pane = %v, want nil", err)
	}
	for _, call := range fe.calls {
		if joined := strings.Join(call, " "); strings.Contains(joined, "paste-buffer") || strings.Contains(joined, "load-buffer") {
			t.Fatalf("nudge staged input for a dead pane: %#v", call)
		}
	}
}

// Control for the dismissal: an ordinary working pane must never receive the
// Down/Enter dismissal keys, or every nudge would inject stray keystrokes.
func TestNudgeOnOrdinaryPaneSendsNoDismissalKeys(t *testing.T) {
	fe := &fencePaneExecutor{session: "worker", attached: "0", inMode: "0", panes: []string{"ready >"}}
	p := fe.provider(Config{})

	if err := p.NudgeNow("worker", runtime.TextContent("claim-your-work")); err != nil {
		t.Fatalf("NudgeNow: %v", err)
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), " Down ") {
			t.Fatalf("ordinary pane received a modal dismissal: %#v", fe.calls)
		}
	}
	if callIndexWithTokens(fe.calls, "if-shell") < 0 {
		t.Fatalf("payload was never delivered: %#v", fe.calls)
	}
}

// If the pane cannot be certified for the dismissal, the modal is still up and
// the pane is still unsafe, so the nudge must park rather than type into it.
func TestNudgeRefusesWhenTheModalDismissalCannotBeCertified(t *testing.T) {
	fe := &fencePaneExecutor{session: "worker", attached: "0", inMode: "0", panes: []string{modelSwitchModalPane}, fenced: true}
	p := fe.provider(Config{})

	err := p.NudgeNow("worker", runtime.TextContent("claim-your-work"))
	if !errors.Is(err, runtime.ErrInputFenced) {
		t.Fatalf("NudgeNow with a stuck modal = %v, want ErrInputFenced", err)
	}
	if !strings.Contains(err.Error(), "model-switch modal") {
		t.Fatalf("fence error = %v, want a named model-switch reason", err)
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "paste-buffer") {
			t.Fatalf("payload reached a pane still showing the modal: %#v", fe.calls)
		}
	}
}

// The wait-idle timeout branch is where the dismissal used to live (it was
// dropped as unguarded input). Nudge must still clear the modal there — the
// modal is precisely what keeps the pane from going idle — and must still do it
// without a single raw send-keys.
func TestNudgeIdleTimeoutClearsModalWithGuardedInputOnly(t *testing.T) {
	fe := &fencePaneExecutor{
		session:  "worker",
		attached: "0",
		inMode:   "0",
		panes:    []string{modelSwitchModalPane, "ready >"},
	}
	p := fe.provider(Config{NudgeIdleTimeout: time.Nanosecond})

	if err := p.Nudge("worker", runtime.TextContent("wake")); err != nil {
		t.Fatalf("Nudge after idle timeout: %v", err)
	}
	if call := unguardedInput(fe.calls); call != nil {
		t.Fatalf("idle-timeout path emitted unguarded input: %#v", call)
	}
	dismissed := false
	for _, call := range fe.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "paste-buffer") {
			break
		}
		if strings.Contains(joined, " Down ") {
			dismissed = true
		}
	}
	if !dismissed {
		t.Fatalf("idle-timeout nudge typed into a visible modal: %#v", fe.calls)
	}
}

// A remediation is input too, so on the token-fenced path it must be held to the
// same session incarnation as the payload: a pane can be respawned in place,
// keeping its tmux IDs while carrying a different agent.
func TestNudgeFencedHoldsTheModalDismissalToTheExpectedInstance(t *testing.T) {
	fe := &fencePaneExecutor{
		session:  "worker",
		attached: "0",
		inMode:   "0",
		panes:    []string{modelSwitchModalPane, "ready >"},
	}
	p := fe.provider(Config{})

	if err := p.NudgeFenced("worker", "expected-token", runtime.TextContent("claim-your-work")); err != nil {
		t.Fatalf("NudgeFenced against a model-switch modal: %v", err)
	}
	for _, call := range fe.calls {
		joined := strings.Join(call, " ")
		if !strings.Contains(joined, " Down ") {
			continue
		}
		if !strings.Contains(joined, "#{==:#{E:GC_INSTANCE_TOKEN},expected-token}") {
			t.Fatalf("modal dismissal = %q, want the expected instance token in its guard", joined)
		}
		return
	}
	t.Fatalf("modal was never dismissed: %#v", fe.calls)
}

// ga-c4w: the WheelUpPane binding parks a pane in copy mode, and the park
// survives the human detaching. Refusing until someone exits copy mode leaves
// the agent permanently deaf with nobody watching, so the nudge path force-exits
// copy mode on its own certified pane and then delivers — the behavior the
// pre-fence NudgeSession path had via cancelCopyModeIfParked.
func TestNudgeCancelsParkedCopyModeThenDelivers(t *testing.T) {
	fe := &fencePaneExecutor{session: "worker", attached: "0", inMode: "1", panes: []string{"ready >"}}
	p := fe.provider(Config{})

	if err := p.NudgeNow("worker", runtime.TextContent("claim-your-work")); err != nil {
		t.Fatalf("NudgeNow on a copy-mode-parked pane: %v", err)
	}
	cancel, deliver := -1, -1
	for i, call := range fe.calls {
		joined := strings.Join(call, " ")
		if cancel < 0 && strings.Contains(joined, "-X cancel") {
			cancel = i
		}
		if deliver < 0 && strings.Contains(joined, "paste-buffer") {
			deliver = i
		}
	}
	if cancel < 0 || deliver < 0 || cancel >= deliver {
		t.Fatalf("copy-mode cancel idx=%d payload idx=%d, want a cancel before delivery; calls=%#v", cancel, deliver, fe.calls)
	}
	if call := unguardedInput(fe.calls); call != nil {
		t.Fatalf("copy-mode cancel escaped the guard: %#v", call)
	}
}

// Control for the cancel: an unparked pane must not be sent `-X cancel`.
func TestNudgeOnUnparkedPaneIssuesNoCopyModeCancel(t *testing.T) {
	fe := &fencePaneExecutor{session: "worker", attached: "0", inMode: "0", panes: []string{"ready >"}}
	p := fe.provider(Config{})

	if err := p.NudgeNow("worker", runtime.TextContent("claim-your-work")); err != nil {
		t.Fatalf("NudgeNow: %v", err)
	}
	if idx := callIndexWithTokens(fe.calls, "-X", "cancel"); idx >= 0 {
		t.Fatalf("unparked pane got a spurious copy-mode cancel at idx %d: %#v", idx, fe.calls)
	}
}

// A cancel that does not take (a human holding the pane in a mode we cannot
// clear) still has to fence, with a reason that names copy mode.
func TestNudgeRefusesWhenCopyModeSurvivesTheCancel(t *testing.T) {
	fe := &fencePaneExecutor{session: "worker", attached: "0", inMode: "1", panes: []string{"ready >"}, fenced: true}
	p := fe.provider(Config{})

	err := p.NudgeNow("worker", runtime.TextContent("claim-your-work"))
	if !errors.Is(err, runtime.ErrInputFenced) {
		t.Fatalf("NudgeNow = %v, want ErrInputFenced", err)
	}
	if !strings.Contains(err.Error(), "copy mode") {
		t.Fatalf("fence error = %v, want a named copy-mode reason", err)
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "paste-buffer") {
			t.Fatalf("payload reached a pane still in copy mode: %#v", fe.calls)
		}
	}
}

// An attached client is visibility, not an input hazard: tmux routes
// paste-buffer and send-keys to the pane we name regardless of who is watching.
// Refusing while attached means `gc attach` silently stops the controller from
// talking to the session you are attached to — observation changing behavior,
// with no recovery until the human detaches.
func TestNudgeDeliversWhileAClientIsAttached(t *testing.T) {
	fe := &fencePaneExecutor{session: "worker", attached: "1", inMode: "0", panes: []string{"ready >"}}
	p := fe.provider(Config{})

	if err := p.NudgeNow("worker", runtime.TextContent("claim-your-work")); err != nil {
		t.Fatalf("NudgeNow while attached = %v, want delivery", err)
	}
	effect := strings.Join(fe.calls[len(fe.calls)-1], " ")
	if !strings.Contains(effect, "paste-buffer") {
		t.Fatalf("final effect = %q, want the payload delivered", effect)
	}
	if strings.Contains(effect, "session_attached") {
		t.Fatalf("effect condition = %q, want no attachment fence", effect)
	}
	// resize-window on an attached window flips it to manual sizing, so the
	// SIGWINCH wake must not run when a client is already servicing the pane.
	if strings.Contains(effect, "resize-window") {
		t.Fatalf("effect = %q, want no window resize while a client is attached", effect)
	}
}

// Control for the attached path: a detached pane still gets the SIGWINCH wake,
// which is what makes a never-observed TUI consume the paste.
func TestNudgeWakesTheWindowWhileDetached(t *testing.T) {
	fe := &fencePaneExecutor{session: "worker", attached: "0", inMode: "0", panes: []string{"ready >"}}
	p := fe.provider(Config{})

	if err := p.NudgeNow("worker", runtime.TextContent("claim-your-work")); err != nil {
		t.Fatalf("NudgeNow: %v", err)
	}
	effect := strings.Join(fe.calls[len(fe.calls)-1], " ")
	if !strings.Contains(effect, "resize-window -t @3 -D 1") || !strings.Contains(effect, "resize-window -t @3 -U 1") {
		t.Fatalf("detached effect = %q, want the SIGWINCH wake around the settle", effect)
	}
}
