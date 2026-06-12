package runtime

import (
	"context"
	"strings"
)

// midSessionDialog describes a dialog the runtime can dismiss without
// operator involvement while a session is already running. The match
// function inspects captured pane content; keys are sent in order with
// bypassDialogConfirmDelay between them.
type midSessionDialog struct {
	name  string
	match func(string) bool
	keys  []string
}

// midSessionDialogs lists the dialogs known to appear during a running
// session (as opposed to the startup set in AcceptStartupDialogs). Only one
// is on screen at a time, so first match wins.
var midSessionDialogs = []midSessionDialog{
	{
		// "Resume previous conversation?" after a long session hits the
		// token ceiling. Unlike the startup path (which resumes the full
		// session as-is to preserve in-flight context), mid-session this
		// fires at peak context, where resuming as-is immediately re-hits
		// the ceiling - select option 1, "Resume from summary", which
		// triggers /compact and recovers the session at low context.
		name:  "claude-resume",
		match: containsClaudeResumeDialog,
		keys:  []string{"1", "Enter"},
	},
	{
		// Periodic "How is Claude doing this session?" feedback prompt.
		// "0" skips it.
		name:  "claude-session-feedback",
		match: containsClaudeSessionFeedbackDialog,
		keys:  []string{"0", "Enter"},
	},
	{
		// Provider session/usage-limit chooser ("You've hit your session
		// limit ... 1. Stop and wait 2. Upgrade"). It does NOT auto-dismiss
		// when the limit window resets, so an unattended session freezes on
		// it indefinitely. Option 1, "Stop and wait", resumes the session
		// when the window resets.
		name:  "claude-session-limit",
		match: containsClaudeSessionLimitDialog,
		keys:  []string{"1", "Enter"},
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
func DismissMidSessionDialogs(
	ctx context.Context,
	peek func(lines int) (string, error),
	sendKeys func(keys ...string) error,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	content, err := peek(startupDialogPeekLines)
	if err != nil {
		return false, err
	}
	for _, dialog := range midSessionDialogs {
		if dialog.match(content) {
			if err := sendDialogKeys(ctx, sendKeys, dialog.keys, bypassDialogConfirmDelay); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// containsClaudeSessionFeedbackDialog reports whether the pane is actively
// showing the periodic "How is Claude doing this session?" feedback dialog.
func containsClaudeSessionFeedbackDialog(content string) bool {
	return strings.Contains(content, "How is Claude doing this session?")
}

// containsClaudeSessionLimitDialog reports whether the pane shows the
// provider session-limit chooser. Both anchors are distinctive UI strings;
// the bare "rate limit" forms are deliberately not used here because
// ordinary scrollback that merely talks about rate limits must not read as
// a dialog.
func containsClaudeSessionLimitDialog(content string) bool {
	return strings.Contains(content, "hit your session limit") ||
		strings.Contains(content, "/rate-limit-options")
}
