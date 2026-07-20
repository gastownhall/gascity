package tmux

import (
	"errors"
	"testing"
)

// These tests cover the re-authored composer-wedge recovery ladder
// (submitWithRecovery). The ladder must never take a destructive action
// (C-u clear / re-paste / Escape) on an unobserved-busy alone: it acts only
// when the draft is VERIFIABLY still in the composer, treats a pane-capture
// error as state-unknown (bounce, no keys), and honors the claude family's
// no-Escape rule. All side effects are injected, so the decision logic is
// exercised without a live tmux server (noSleep is defined in
// nudge_submit_confirm_test.go).

// TestComposerContainsDraftScopesToComposerRegion locks the false-positive guard
// at the heart of point 4: a submitted message echoes in the transcript, so the
// draft check must be scoped to the composer region (after the last prompt
// glyph). A transcript echo above an empty composer is DELIVERED; the same text
// still sitting on the prompt line is a live draft.
func TestComposerContainsDraftScopesToComposerRegion(t *testing.T) {
	msg := "please review the PR and respond now"

	delivered := "You: please review the PR and respond now\n\n[assistant reply...]\n\n❯ "
	if composerContainsDraft(delivered, msg) {
		t.Fatal("transcript echo above an empty composer must NOT read as a live draft (fast-completion false positive)")
	}

	wedged := "[assistant reply...]\n\n❯ please review the PR and respond now"
	if !composerContainsDraft(wedged, msg) {
		t.Fatal("a message still sitting on the prompt line must read as a live draft (wedge)")
	}

	// Bordered composer with the draft on the prompt line inside the box.
	bordered := "transcript\n╭───────────────────────────────────────╮\n│ ❯ please review the PR and respond now │\n╰───────────────────────────────────────╯"
	if !composerContainsDraft(bordered, msg) {
		t.Fatal("a bordered composer draft must still be detected")
	}
}

// TestSubmitWithRecoveryFastCompletionDelivered covers point 5's fast-completion
// case: the busy window is missed (busy never observed), but the composer is
// empty — the submit landed and the turn completed fast. This MUST read as
// delivered with no destructive keys (no clear, no re-paste, no Escape).
func TestSubmitWithRecoveryFastCompletionDelivered(t *testing.T) {
	var escapes, clears, repastes int
	l := submitLadder{
		sendEnter:  func() error { return nil },
		sendEscape: func() error { escapes++; return nil },
		clearInput: func() error { clears++; return nil },
		repaste:    func() error { repastes++; return nil },
		wake:       func() {},
		busy:       func() (bool, error) { return false, nil }, // busy window missed
		// Composer is empty (just the prompt): submit landed, turn completed fast.
		capture: func() (string, error) { return "…assistant reply…\n\n❯ ", nil },
		message: "hello wedge please respond",
		sleep:   noSleep,
	}
	if err := submitWithRecovery(l); err != nil {
		t.Fatalf("err = %v, want nil (fast completion must read as delivered)", err)
	}
	if escapes != 0 || clears != 0 || repastes != 0 {
		t.Fatalf("destructive keys fired on a delivered fast-completion: escapes=%d clears=%d repastes=%d", escapes, clears, repastes)
	}
}

// TestSubmitWithRecoveryCaptureErrorUndeliverable covers point 2: a
// busy/composer observation that ERRORS is state-unknown. The ladder must NOT
// advance the destructive rungs on a capture error — it returns
// ErrNudgeUndeliverable so the caller bounces instead of gambling a clear/
// re-paste on a possibly-live turn.
func TestSubmitWithRecoveryCaptureErrorUndeliverable(t *testing.T) {
	var escapes, clears, repastes int
	l := submitLadder{
		sendEnter:  func() error { return nil },
		sendEscape: func() error { escapes++; return nil },
		clearInput: func() error { clears++; return nil },
		repaste:    func() error { repastes++; return nil },
		wake:       func() {},
		busy:       func() (bool, error) { return false, nil },
		capture:    func() (string, error) { return "", errors.New("capture-pane: no such pane") },
		message:    "hello wedge",
		sleep:      noSleep,
	}
	err := submitWithRecovery(l)
	if !errors.Is(err, ErrNudgeUndeliverable) {
		t.Fatalf("err = %v, want ErrNudgeUndeliverable (state-unknown)", err)
	}
	if escapes != 0 || clears != 0 || repastes != 0 {
		t.Fatalf("destructive keys fired despite unknown composer state: escapes=%d clears=%d repastes=%d", escapes, clears, repastes)
	}
}

// TestSubmitWithRecoveryFailedClearUndeliverableNoRepaste covers point 5's
// failed-clear case (and point 4's "verify cleared"): the draft is verifiably
// stuck, C-u is attempted, but a re-capture shows the draft STILL present — the
// clear failed. The ladder must return ErrNudgeUndeliverable and must NOT
// re-paste on top of the stuck draft.
func TestSubmitWithRecoveryFailedClearUndeliverableNoRepaste(t *testing.T) {
	var clears, repastes int
	stuck := "…reply…\n\n❯ hello wedge please respond now"
	l := submitLadder{
		sendEnter:  func() error { return nil },
		sendEscape: func() error { t.Fatal("Escape must not fire for a skip-escape (claude) provider"); return nil },
		skipEscape: true, // claude family
		clearInput: func() error { clears++; return nil },
		repaste:    func() error { repastes++; return nil },
		wake:       func() {},
		busy:       func() (bool, error) { return false, nil },
		// Draft is ALWAYS present, even after C-u: the clear failed.
		capture: func() (string, error) { return stuck, nil },
		message: "hello wedge please respond now",
		sleep:   noSleep,
	}
	err := submitWithRecovery(l)
	if !errors.Is(err, ErrNudgeUndeliverable) {
		t.Fatalf("err = %v, want ErrNudgeUndeliverable (failed clear)", err)
	}
	if clears != 1 {
		t.Fatalf("clears = %d, want 1 (C-u attempted exactly once)", clears)
	}
	if repastes != 0 {
		t.Fatalf("repastes = %d, want 0 (must NOT re-paste on top of a stuck draft)", repastes)
	}
}

// TestSubmitWithRecoveryClaudeSkipsEscape covers point 3 / point 5's "claude
// gets no Escape": a wedged claude composer is recovered by the verified
// C-u clear + clean re-paste path, and the Escape rung is never used (Escape is
// a semantic interrupt for the claude family).
func TestSubmitWithRecoveryClaudeSkipsEscape(t *testing.T) {
	var escapes, clears, repastes int
	cleared := false
	l := submitLadder{
		sendEnter:  func() error { return nil },
		sendEscape: func() error { escapes++; return nil },
		skipEscape: true, // claude family
		clearInput: func() error { clears++; cleared = true; return nil },
		repaste:    func() error { repastes++; return nil },
		wake:       func() {},
		// The composer accepts a submit only after a clean re-paste.
		busy: func() (bool, error) { return repastes > 0, nil },
		capture: func() (string, error) {
			if cleared {
				return "…reply…\n\n❯ ", nil // C-u emptied the composer
			}
			return "…reply…\n\n❯ hello wedge please respond now", nil // draft stuck
		},
		message: "hello wedge please respond now",
		sleep:   noSleep,
	}
	if err := submitWithRecovery(l); err != nil {
		t.Fatalf("err = %v, want nil (claude wedge recovers via C-u + re-paste)", err)
	}
	if escapes != 0 {
		t.Fatalf("escapes = %d, want 0 (claude must never receive an Escape)", escapes)
	}
	if clears != 1 || repastes != 1 {
		t.Fatalf("recovery path wrong: clears=%d repastes=%d, want 1/1", clears, repastes)
	}
}

// TestSubmitWithRecoveryNonClaudeUsesEscapeRung is the complement to the claude
// case: a NON-skip-escape provider (e.g. grok) does get the Escape rung, and an
// Escape that unsticks the composer recovers without ever reaching the
// destructive C-u/re-paste rung.
func TestSubmitWithRecoveryNonClaudeUsesEscapeRung(t *testing.T) {
	var escapes, clears, repastes int
	escaped := false
	l := submitLadder{
		sendEnter:  func() error { return nil },
		sendEscape: func() error { escapes++; escaped = true; return nil },
		skipEscape: false, // non-claude: Escape is a mode-reset key
		clearInput: func() error { clears++; return nil },
		repaste:    func() error { repastes++; return nil },
		wake:       func() {},
		busy:       func() (bool, error) { return escaped, nil }, // Escape unsticks; next Enter submits
		capture:    func() (string, error) { return "❯ hello wedge please respond now", nil },
		message:    "hello wedge please respond now",
		sleep:      noSleep,
	}
	if err := submitWithRecovery(l); err != nil {
		t.Fatalf("err = %v, want nil (Escape rung should recover a non-claude wedge)", err)
	}
	if escapes == 0 {
		t.Fatal("Escape rung never fired for a non-skip-escape provider")
	}
	if clears != 0 || repastes != 0 {
		t.Fatalf("destructive C-u/re-paste fired though Escape already recovered: clears=%d repastes=%d", clears, repastes)
	}
}

// TestSubmitWithRecoveryRung1ConfirmNoEscalation proves the common path is
// untouched: when the first verified re-Enter confirms the submit, no draft
// inspection or destructive rung fires.
func TestSubmitWithRecoveryRung1ConfirmNoEscalation(t *testing.T) {
	var enters, escapes, clears, repastes, captures int
	l := submitLadder{
		sendEnter:  func() error { enters++; return nil },
		sendEscape: func() error { escapes++; return nil },
		clearInput: func() error { clears++; return nil },
		repaste:    func() error { repastes++; return nil },
		wake:       func() {},
		busy:       func() (bool, error) { return enters >= 1, nil }, // first Enter submits
		capture:    func() (string, error) { captures++; return "❯ ", nil },
		message:    "hello",
		sleep:      noSleep,
	}
	if err := submitWithRecovery(l); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if enters != 1 {
		t.Fatalf("enters = %d, want 1 (rung 1 confirmed)", enters)
	}
	if escapes != 0 || clears != 0 || repastes != 0 || captures != 0 {
		t.Fatalf("escalation fired on an already-submitted turn: escapes=%d clears=%d repastes=%d captures=%d", escapes, clears, repastes, captures)
	}
}
