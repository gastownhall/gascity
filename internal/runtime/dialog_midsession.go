package runtime

import (
	"context"
	"strings"
)

// midSessionDialog describes a dialog the runtime can dismiss without
// operator involvement while a session is already running. The match
// function inspects captured pane content; keys are sent in order with
// bypassDialogConfirmDelay between them.
//
// tailAnchor is a substring unique to the dialog's bottom-most rendered line
// (its footer or last option). A dialog is treated as active only when this
// anchor appears on the last non-blank line of the capture, i.e. the dialog
// is the bottom-most rendered block. That rejects a fully rendered but stale
// dialog left above a later idle prompt or newer agent output, which would
// otherwise satisfy the contains-based match and inject dismissal keys into a
// live prompt.
type midSessionDialog struct {
	name       string
	match      func(string) bool
	keys       []string
	tailAnchor string
}

// midSessionDialogs lists the dialogs known to appear during a running
// session (as opposed to the startup set in AcceptStartupDialogs). Only one
// is on screen at a time, so first match wins.
//
// The Codex/GPT "switch to a cheaper model?" modal is a fourth mid-session
// dialog handled separately by the tmux provider's
// DismissModelSwitchModalIfPresent (it needs an arrow-key selection move, not
// a single confirm key). The two mid-session dismissers are intentionally
// distinct; keep their matcher strictness and best-effort error policy aligned
// when either changes.
var midSessionDialogs = []midSessionDialog{
	{
		// "Resume previous conversation?" after a long session hits the
		// token ceiling. Unlike the startup path (which arrows Down to
		// "Resume full session as-is" to preserve in-flight context),
		// mid-session this fires at peak context, where resuming as-is
		// immediately re-hits the ceiling - confirm the pre-selected default
		// "Resume from summary" (the highlighted option), which triggers
		// /compact and recovers the session at low context. Enter accepts the
		// highlighted default, so this does not depend on an unverified numeric
		// shortcut for a dialog that renders no digit labels.
		name:       "claude-resume",
		match:      containsClaudeResumeDialog,
		keys:       []string{"Enter"},
		tailAnchor: "Enter to confirm",
	},
	{
		// Periodic "How is Claude doing this session?" feedback prompt.
		// "0" skips it.
		name:       "claude-session-feedback",
		match:      containsClaudeSessionFeedbackDialog,
		keys:       []string{"0", "Enter"},
		tailAnchor: "0: Dismiss",
	},
	{
		// Provider session/usage-limit chooser ("You've hit your session
		// limit ... 1. Stop and wait 2. Upgrade"). It does NOT auto-dismiss
		// when the limit window resets, so an unattended session freezes on
		// it indefinitely. Option 1, "Stop and wait", resumes the session
		// when the window resets.
		name:       "claude-session-limit",
		match:      containsClaudeSessionLimitDialog,
		keys:       []string{"1", "Enter"},
		tailAnchor: "Upgrade",
	},
}

// DismissMidSessionDialogs handles dialogs that appear during a running
// session and would otherwise absorb the next input - a nudge delivered
// while one is on screen lands in the dialog instead of the prompt and is
// lost. Callers invoke it best-effort immediately before sending text.
//
// Unlike the startup acceptors this does a single peek and never polls:
// the nudge path must not stall behind a session that is mid-turn (no
// prompt, no dialog). It reports whether a dialog was dismissed so callers
// can allow the UI a beat to settle before delivering their text.
//
// peek must return the pane's current visible screen (not deep scrollback):
// a live blocking dialog occupies the visible footer, and excluding
// scrollback is the first line of defense against matching an already-
// dismissed dialog. On top of that, a dialog fires only when it is the
// active bottom-most rendered block — its tailAnchor is on the last non-blank
// line of the capture. Together these reject a fully rendered but stale
// dialog left above a later idle prompt, which the contains-based matchers
// alone would treat as live and inject dismissal keys into the real prompt.
func DismissMidSessionDialogs(
	ctx context.Context,
	peek func() (string, error),
	sendKeys func(keys ...string) error,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	content, err := peek()
	if err != nil {
		return false, err
	}
	tail := lastNonBlankLine(content)
	for _, dialog := range midSessionDialogs {
		if dialog.match(content) && strings.Contains(tail, dialog.tailAnchor) {
			if err := sendDialogKeys(ctx, sendKeys, dialog.keys, bypassDialogConfirmDelay); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// lastNonBlankLine returns the final line of content that is not blank after
// trailing whitespace-only lines are ignored. tmux capture-pane pads the
// visible screen with blank rows to the pane bottom, so the active bottom
// block is the last line with real content, not the literal last line. It
// returns "" when content is empty or entirely blank.
func lastNonBlankLine(content string) string {
	lines := strings.Split(content, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// containsClaudeSessionFeedbackDialog reports whether the pane is actively
// showing the periodic "How is Claude doing this session?" feedback dialog. It
// requires the question AND the "0: Dismiss" option line so that ordinary
// scrollback merely quoting the question cannot false-match and receive a
// spurious "0" keystroke when checked mid-session against a working pane. The
// capture is scrollback-inclusive, so a single-phrase match here is hazardous.
func containsClaudeSessionFeedbackDialog(content string) bool {
	return strings.Contains(content, "How is Claude doing this session?") &&
		strings.Contains(content, "0: Dismiss")
}

// containsClaudeSessionLimitDialog reports whether the pane is actively showing
// the provider session-limit chooser ("You've hit your session limit ... 1.
// Stop and wait 2. Upgrade"). It requires the heading AND both option lines so
// that ordinary scrollback merely mentioning the limit cannot false-match and
// receive a spurious "1" keystroke when checked mid-session against a working
// pane. The bare heading is deliberately insufficient for the same reason the
// model-switch matcher requires both anchors (see ContainsModelSwitchModal).
//
// It deliberately does NOT key on "/rate-limit-options": that string marks the
// provider rate-limit / quarantine screen, which is owned by the session
// reconciler (ContainsProviderRateLimitScreen / checkRateLimitStability).
// Injecting "1" there is the wrong remedy and can perturb the very screen the
// reconciler re-reads each tick to decide quarantine vs. heal.
func containsClaudeSessionLimitDialog(content string) bool {
	return strings.Contains(content, "hit your session limit") &&
		strings.Contains(content, "Stop and wait") &&
		strings.Contains(content, "Upgrade")
}
