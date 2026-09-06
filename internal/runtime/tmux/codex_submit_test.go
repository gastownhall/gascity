package tmux

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// nudgeKeystrokesForFamily composes the keys a nudge actually puts into a pane,
// in NudgeSession's order: the optional pre-submit Escape (step 3) followed by
// the declared submit sequence (step 5). It exists in the test because the
// hazard is the COMPOSITION of two independent tables, which neither table can
// state on its own.
func nudgeKeystrokesForFamily(family string) []string {
	var keys []string
	if !providerEnvSkipsEscape(family) {
		keys = append(keys, "Escape")
	}
	return append(keys, nudgeSubmitKeySequenceForFamily(family)...)
}

// TestCodexNudgeSubmitsWithExactlyOneEscapeThenEnter is the #4706 fix and its
// own guard rail. codex buffers a send-keys burst as a paste, so a lone trailing
// Enter is swallowed as a composer newline and the agent's first turn never
// starts. Escape-then-Enter submits it — but a SECOND Escape would be
// backtrack/edit-previous, so the count matters as much as the keys.
func TestCodexNudgeSubmitsWithExactlyOneEscapeThenEnter(t *testing.T) {
	got := nudgeKeystrokesForFamily("codex")
	want := []string{"Escape", "Enter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codex nudge keystrokes = %v, want %v", got, want)
	}
	if got := nudgeSubmitKeySequenceForFamily("codex"); !reflect.DeepEqual(got, want) {
		t.Fatalf("codex submit sequence = %v, want %v declared as one retryable unit", got, want)
	}
	if !providerEnvSkipsEscape("codex") {
		t.Fatal("codex left providersSkippingEscapeBeforeEnter; combined with its submit entry that sends Escape twice, which codex binds to backtrack rather than submit")
	}
}

// Control: the claude path is byte-identical to what it has always been. If the
// codex entry had been added to a shared default, or the skip list had been
// edited instead of the table, this row would move too.
func TestClaudeNudgeKeystrokesAreUnchanged(t *testing.T) {
	if got, want := nudgeKeystrokesForFamily("claude"), []string{"Enter"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("claude nudge keystrokes = %v, want %v", got, want)
	}
	// And an unregistered family keeps the historical default-with-Escape shape.
	if got, want := nudgeKeystrokesForFamily("some-unregistered-family"), []string{"Escape", "Enter"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unregistered family keystrokes = %v, want %v", got, want)
	}
}

// TestCodexSubmitIsVerified pins the second half of the codex fix: its submit is
// CONFIRMED against the busy indicator paneContainsBusyIndicator already reads
// for it ("esc to interrupt"). Without eligibility the best-effort fallback
// reports success the moment keys reach tmux, so the queue acks and deletes an
// item whose paste may still be unsubmitted — losing the nudge outright instead
// of requeueing it under the attempt cap.
func TestCodexSubmitIsVerified(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		if !submitVerifyEligibleFamily(family) {
			t.Errorf("submitVerifyEligibleFamily(%q) = false, want true", family)
		}
	}
	// Control: a family whose busy indicator this package cannot read stays on
	// best-effort delivery, because reporting every submit as unconfirmed would
	// burn the queue's attempts re-pasting messages that already landed.
	for _, family := range []string{"grok", "kimi", "opencode", "", "some-unregistered-family"} {
		if submitVerifyEligibleFamily(family) {
			t.Errorf("submitVerifyEligibleFamily(%q) = true, want false", family)
		}
	}
}

// TestCodexBusyIndicatorIsReadable is the premise the eligibility above rests
// on: if codex's indicator string ever stops matching, verification would report
// every codex submit as unconfirmed and the queue would re-paste every nudge
// three times before dead-lettering it.
func TestCodexBusyIndicatorIsReadable(t *testing.T) {
	if !paneContainsBusyIndicator([]string{"  working  (esc to interrupt)"}) {
		t.Fatal("codex's busy indicator no longer matches; submit verification for codex would report every delivery unconfirmed")
	}
}

// TestNudgeSessionComposesEscapeThenSubmitSequence pins the PROVENANCE of the
// composition nudgeKeystrokesForFamily models.
//
// The two decisions live in NudgeSession's body — the pre-submit Escape at step
// 3, then the declared submit sequence at step 5 — and driving that body needs a
// live tmux pane (both decisions resolve through the pane's environment or a
// process sniff), so a unit test cannot execute it. What it CAN do is refuse to
// let the model drift from the code: if NudgeSession stops consulting either
// decision, or consults them in the other order, the helper above is describing
// something that no longer happens and this fails.
func TestNudgeSessionComposesEscapeThenSubmitSequence(t *testing.T) {
	body := nudgeSessionSource(t)
	escapeAt := strings.Index(body, "t.shouldSendEscapeBeforeEnter(target)")
	submitAt := strings.Index(body, "t.nudgeSubmitKeySequence(target)")
	switch {
	case escapeAt < 0:
		t.Fatal("NudgeSession no longer consults shouldSendEscapeBeforeEnter; nudgeKeystrokesForFamily models a step that is gone")
	case submitAt < 0:
		t.Fatal("NudgeSession no longer consults nudgeSubmitKeySequence; nudgeKeystrokesForFamily models a step that is gone")
	case submitAt < escapeAt:
		t.Fatal("NudgeSession now resolves the submit sequence BEFORE the pre-submit Escape decision; re-derive nudgeKeystrokesForFamily against the new order")
	}
}

// nudgeSessionSource returns the source text of NudgeSession's body.
func nudgeSessionSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("tmux.go")
	if err != nil {
		t.Fatalf("reading tmux.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func (t *Tmux) NudgeSession(")
	if start < 0 {
		t.Fatal("NudgeSession not found in tmux.go")
	}
	rest := body[start:]
	end := strings.Index(rest, "\nfunc ")
	if end < 0 {
		return rest
	}
	return rest[:end]
}
