package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// TestClassifyParkedSession exercises the pure classifier used by `gc
// session doctor parked-sessions`. The classifier is the only piece with
// behavior worth testing in isolation; tmux capture/SendKeys plumbing is
// covered by the broader runtime/tmux suite.
//
// Each case names the rule it's pinning so regressions can be traced
// back to the matching bullet in the command's detection rule docstring
// (see classifyParkedSession in cmd_session_doctor.go).
func TestClassifyParkedSession(t *testing.T) {
	// paneWithTypedInput is the canonical "mayor parked after restart"
	// snapshot: prompt cursor with unsubmitted operator text, no
	// thinking indicator anywhere. Used as the baseline for the
	// positive-detection cases below.
	const paneWithTypedInput = `Last output line from previous turn.

> drain the backlog
❯ drain the backlog
`

	// paneIdlePrompt is a fresh prompt with nothing typed — the most
	// common false-positive shape. Must NOT classify as parked.
	const paneIdlePrompt = `Last output line from previous turn.

❯
`

	// paneWithThinking has typed input AND a thinking indicator: the
	// model is mid-turn and the queued input will be submitted on the
	// next turn boundary. Must NOT classify as parked.
	const paneWithThinking = `Last output line from previous turn.
Sautéing… (12s)
❯ drain the backlog
`

	cases := []struct {
		name       string
		before     string
		after      string
		state      session.State
		wantParked bool
		wantReason string
	}{
		{
			name:       "parked: typed input, no motion, no thinking, active",
			before:     paneWithTypedInput,
			after:      paneWithTypedInput,
			state:      session.StateActive,
			wantParked: true,
			wantReason: parkedSessionReasonInputPrompt,
		},
		{
			name:       "parked: typed input, no motion, no thinking, awake (alias for active)",
			before:     paneWithTypedInput,
			after:      paneWithTypedInput,
			state:      session.StateAwake,
			wantParked: true,
			wantReason: parkedSessionReasonInputPrompt,
		},
		{
			name:       "not parked: pane changed between captures (model is printing)",
			before:     paneWithTypedInput,
			after:      paneWithTypedInput + "more output\n",
			state:      session.StateActive,
			wantParked: false,
		},
		{
			name:       "not parked: empty prompt, no typed input",
			before:     paneIdlePrompt,
			after:      paneIdlePrompt,
			state:      session.StateActive,
			wantParked: false,
		},
		{
			name:       "not parked: thinking indicator visible (Sautéing…)",
			before:     paneWithThinking,
			after:      paneWithThinking,
			state:      session.StateActive,
			wantParked: false,
		},
		{
			name:       "not parked: state is suspended",
			before:     paneWithTypedInput,
			after:      paneWithTypedInput,
			state:      session.StateSuspended,
			wantParked: false,
		},
		{
			name:       "not parked: state is asleep",
			before:     paneWithTypedInput,
			after:      paneWithTypedInput,
			state:      session.StateAsleep,
			wantParked: false,
		},
		{
			name:       "not parked: state is closed",
			before:     paneWithTypedInput,
			after:      paneWithTypedInput,
			state:      session.StateClosed,
			wantParked: false,
		},
		{
			name:       "not parked: state is creating",
			before:     paneWithTypedInput,
			after:      paneWithTypedInput,
			state:      session.StateCreating,
			wantParked: false,
		},
		{
			name: "not parked: bare cursor only, no text after",
			before: `Some output.
❯
`,
			after: `Some output.
❯
`,
			state:      session.StateActive,
			wantParked: false,
		},
		{
			name: "parked: typed input with leading whitespace still counts",
			// The regex anchors on ^❯ + (one or more spaces) + non-space;
			// extra spaces between cursor and text are normal.
			before: `Output line.
❯   queued message text
`,
			after: `Output line.
❯   queued message text
`,
			state:      session.StateActive,
			wantParked: true,
			wantReason: parkedSessionReasonInputPrompt,
		},
		{
			name: "not parked: thinking indicator anywhere counts (Blanching… on earlier line)",
			before: `Blanching… (3s)
some intermediate output
❯ pending
`,
			after: `Blanching… (3s)
some intermediate output
❯ pending
`,
			state:      session.StateActive,
			wantParked: false,
		},
		{
			name: "not parked: Worked for marker (turn finished, idle prompt only)",
			before: `Worked for 4m 12s
❯
`,
			after: `Worked for 4m 12s
❯
`,
			state:      session.StateActive,
			wantParked: false,
		},
		{
			name: "parked: Worked for present but typed input also present -- thinking marker wins, not parked",
			// "Worked for" is in the indicator list because it marks a
			// model output state; even though there is typed input, we
			// treat the session as making progress when any indicator
			// is visible. This pins the precedence so future changes
			// don't silently flip it.
			before: `Worked for 4m 12s
❯ next prompt typed
`,
			after: `Worked for 4m 12s
❯ next prompt typed
`,
			state:      session.StateActive,
			wantParked: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotParked, gotReason := classifyParkedSession(tc.before, tc.after, tc.state)
			if gotParked != tc.wantParked {
				t.Fatalf("parked: got %v, want %v (reason=%q)", gotParked, tc.wantParked, gotReason)
			}
			if tc.wantParked && gotReason != tc.wantReason {
				t.Fatalf("reason: got %q, want %q", gotReason, tc.wantReason)
			}
			if !tc.wantParked && gotReason != "" {
				t.Fatalf("reason: got %q, want empty for not-parked case", gotReason)
			}
		})
	}
}

// TestContainsThinkingIndicator confirms every documented Claude Code
// thinking marker is recognized. If a marker is added to the codebase
// without updating thinkingIndicatorPatterns this test starts failing.
func TestContainsThinkingIndicator(t *testing.T) {
	markers := []string{
		"Blanching…",
		"Seasoning…",
		"Sautéing…",
		"Sautéed",
		"Forging…",
		"Ionizing…",
		"Baking…",
		"Baked",
		"Churned",
		"Channeling…",
		"Fiddle-faddling…",
		"Cooked",
		"Brewed",
		"Worked for",
		"Sautéed for",
	}
	for _, m := range markers {
		if !containsThinkingIndicator("prefix " + m + " suffix") {
			t.Errorf("expected marker %q to be recognized", m)
		}
	}
	if containsThinkingIndicator("nothing interesting here") {
		t.Error("expected no marker match on plain text")
	}
	if containsThinkingIndicator("") {
		t.Error("expected no marker match on empty string")
	}
}
