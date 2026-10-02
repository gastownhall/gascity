package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker/workertest"
)

func TestParseApprovalPrompt_BashCommand(t *testing.T) {
	pane := `● Bash(bd list --assignee=$GC_AGENT --status=in_progress 2>&1)
  ⎿  Running…

────────────────────────────────────────────────────────────────────────────────
 Bash command

   bd list --assignee=$GC_AGENT --status=in_progress 2>&1
   Check for in-progress work (crash recovery)

 This command requires approval

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don't ask again for: bd list:*
   3. No

 Esc to cancel · Tab to amend · ctrl+e to explain`

	a := parseApprovalPrompt(pane)
	if a == nil {
		t.Fatal("expected approval prompt, got nil")
	}
	if a.ToolName != "Bash" {
		t.Errorf("expected ToolName=Bash, got %q", a.ToolName)
	}
	if a.Input == "" {
		t.Error("expected non-empty Input")
	}
}

func TestParseApprovalPrompt_EditCommand(t *testing.T) {
	pane := `● Edit(file_path: /tmp/test.go)
  old_string: "foo"
  new_string: "bar"

 Approve edits?

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don't ask again for edits
   3. No`

	a := parseApprovalPrompt(pane)
	if a == nil {
		t.Fatal("expected approval prompt, got nil")
	}
	if a.ToolName != "Edit" {
		t.Errorf("expected ToolName=Edit, got %q", a.ToolName)
	}
}

func TestParseApprovalPrompt_NoPrompt(t *testing.T) {
	pane := `Just some regular output
$ echo hello
hello`

	a := parseApprovalPrompt(pane)
	if a != nil {
		t.Errorf("expected nil, got %+v", a)
	}
}

func TestParseApprovalPrompt_NoToolHeader_ReturnsNil(t *testing.T) {
	// Conversational text containing "requires approval" but no tool header.
	// Must NOT produce a false positive.
	pane := `Sure, I can explain how Claude's permission system works.

When a tool call is made, Claude checks if "This command requires approval"
based on the current permission mode. The user then sees a prompt.`

	a := parseApprovalPrompt(pane)
	if a != nil {
		t.Errorf("expected nil for conversational text, got %+v", a)
	}
}

func TestParseApprovalPrompt_WriteCommand(t *testing.T) {
	pane := `● Write(file_path: /tmp/new.txt)

 This command requires approval

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don't ask again for: Write:*
   3. No`

	a := parseApprovalPrompt(pane)
	if a == nil {
		t.Fatal("expected approval prompt, got nil")
	}
	if a.ToolName != "Write" {
		t.Errorf("expected ToolName=Write, got %q", a.ToolName)
	}
}

func TestParseApprovalPrompt_NestedParens(t *testing.T) {
	pane := `● Bash(echo "foo(bar)")

 This command requires approval

 Do you want to proceed?
 ❯ 1. Yes
   3. No`

	a := parseApprovalPrompt(pane)
	if a == nil {
		t.Fatal("expected approval prompt, got nil")
	}
	if a.ToolName != "Bash" {
		t.Errorf("expected ToolName=Bash, got %q", a.ToolName)
	}
	// Greedy match should capture full args including nested parens.
	if !strings.Contains(a.Input, "foo(bar)") {
		t.Errorf("expected input to contain nested parens, got %q", a.Input)
	}
}

func TestParseApprovalPrompt_MultipleToolHeaders_BindsToNearest(t *testing.T) {
	// Two tool blocks in pane output — approval is for the second one.
	pane := `● Read(file_path: /tmp/old.txt)
  ⎿  file contents here

● Bash(rm -rf /tmp/old.txt)

 This command requires approval

 Do you want to proceed?
 ❯ 1. Yes
   3. No`

	a := parseApprovalPrompt(pane)
	if a == nil {
		t.Fatal("expected approval prompt, got nil")
	}
	if a.ToolName != "Bash" {
		t.Errorf("expected ToolName=Bash (nearest to approval), got %q", a.ToolName)
	}
}

func TestApprovalDedup(t *testing.T) {
	d := &approvalDedup{lastHash: make(map[string]string)}

	a := &parsedApproval{ToolName: "Bash", Input: "ls"}
	if !d.isNew("s1", a) {
		t.Error("first call should be new")
	}
	if d.isNew("s1", a) {
		t.Error("second call with same content should not be new")
	}

	b := &parsedApproval{ToolName: "Bash", Input: "pwd"}
	if !d.isNew("s1", b) {
		t.Error("different content should be new")
	}

	d.clear("s1")
	if !d.isNew("s1", a) {
		t.Error("after clear, should be new again")
	}
}

func TestPhase2ProviderPendingInteractionSeam(t *testing.T) {
	reporter := workertest.NewSuiteReporter(t, "phase2-tmux-pending", map[string]string{
		"tier":      "worker-core",
		"phase":     "phase2",
		"component": "tmux",
	})
	session := "phase2-pending"
	fe := &fakeExecutor{out: approvalPromptPane()}
	provider := &Provider{
		tm: &Tmux{
			cfg:  Config{SocketName: "phase2-sock"},
			exec: fe,
		},
	}

	pending, err := provider.Pending(session)
	reporter.Require(t, pendingInteractionSeamResult(session, pending, err, fe.calls))
}

func TestProviderPendingMapsTmuxSessionNotFoundToRuntimeSentinel(t *testing.T) {
	provider := &Provider{
		tm: &Tmux{
			exec: &fakeExecutor{err: ErrSessionNotFound},
		},
	}

	pending, err := provider.Pending("missing")
	if pending != nil {
		t.Fatalf("Pending = %#v, want nil", pending)
	}
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Pending error = %v, want runtime.ErrSessionNotFound", err)
	}
}

// A tmux server that cannot be reached is a failed observation, not an empty
// pane: Pending must not answer "nothing pending", and its message must not
// read as gone to runtime.IsSessionGone. A server that answered with no
// sessions does prove the pane is gone.
func TestProviderPendingNoServerMapping(t *testing.T) {
	cases := []struct {
		name            string
		err             error
		wantUnavailable bool
		wantNotFound    bool
	}{
		{name: "no_server", err: ErrNoServer, wantUnavailable: true},
		{name: "no_current_target", err: ErrNoCurrentTarget, wantNotFound: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &Provider{tm: &Tmux{exec: &fakeExecutor{err: tc.err}}}
			pending, err := provider.Pending("worker")
			if pending != nil || err == nil {
				t.Fatalf("Pending = (%#v, %v), want (nil, error)", pending, err)
			}
			if got := errors.Is(err, runtime.ErrRuntimeUnavailable); got != tc.wantUnavailable {
				t.Fatalf("errors.Is(%v, ErrRuntimeUnavailable) = %v, want %v", err, got, tc.wantUnavailable)
			}
			if got := errors.Is(err, runtime.ErrSessionNotFound); got != tc.wantNotFound {
				t.Fatalf("errors.Is(%v, ErrSessionNotFound) = %v, want %v", err, got, tc.wantNotFound)
			}
			if got := runtime.IsSessionGone(err); got != tc.wantNotFound {
				t.Fatalf("IsSessionGone(%v) = %v, want %v", err, got, tc.wantNotFound)
			}
		})
	}
}

func TestProviderRespondMapsTmuxSessionNotFoundToRuntimeSentinel(t *testing.T) {
	provider := &Provider{
		tm: &Tmux{
			exec: &fakeExecutor{err: ErrSessionNotFound},
		},
	}

	err := provider.Respond("missing", runtime.InteractionResponse{Action: "approve"})
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Respond error = %v, want runtime.ErrSessionNotFound", err)
	}
}

func TestPhase2ProviderRespondRejectsMismatchedRequest(t *testing.T) {
	reporter := workertest.NewSuiteReporter(t, "phase2-tmux-reject", map[string]string{
		"tier":      "worker-core",
		"phase":     "phase2",
		"component": "tmux",
	})
	session := "phase2-reject"
	fe := &fakeExecutor{out: approvalPromptPane()}
	provider := &Provider{
		tm: &Tmux{
			exec: fe,
		},
	}

	err := provider.Respond(session, runtime.InteractionResponse{
		RequestID: "tmux-wrong",
		Action:    "approve",
	})
	reporter.Require(t, rejectInteractionSeamResult(session, err, fe.calls))
}

func TestPhase2ProviderRespondApprovesAndClearsPrompt(t *testing.T) {
	reporter := workertest.NewSuiteReporter(t, "phase2-tmux-respond", map[string]string{
		"tier":      "worker-core",
		"phase":     "phase2",
		"component": "tmux",
	})
	session := "phase2-approve"
	fe := &fakeExecutor{
		outs: []string{
			approvalPromptPane(), // pre-verify capture: prompt present
			"0",                  // #{pane_in_mode} probe -> not parked (no cancel)
			"",                   // send-keys -l result (ignored)
			`assistant ready`,    // poll capture: prompt cleared
		},
	}
	provider := &Provider{
		tm: &Tmux{
			cfg:  Config{SocketName: "phase2-sock"},
			exec: fe,
		},
	}

	requestID := "tmux-" + approvalHash(&parsedApproval{
		ToolName: "Read",
		Input:    "file_path: /tmp/test.txt",
	})
	err := provider.Respond(session, runtime.InteractionResponse{
		RequestID: requestID,
		Action:    "approve",
	})
	reporter.Require(t, respondInteractionSeamResult(session, err, fe.calls))
}

func TestPhase2ProviderPendingDedupIsInstanceLocal(t *testing.T) {
	reporter := workertest.NewSuiteReporter(t, "phase2-tmux-dedup", map[string]string{
		"tier":      "worker-core",
		"phase":     "phase2",
		"component": "tmux",
	})
	approval := &parsedApproval{ToolName: "Read", Input: "file_path: /tmp/test.txt"}
	tmA := &Tmux{}
	tmB := &Tmux{}

	reporter.Require(t, interactionInstanceLocalDedupResult(approval, tmA, tmB))
}

func TestExtractToolInput_NoParens(t *testing.T) {
	pane := `● Bash
   bd list --assignee=$GC_AGENT --status=in_progress 2>&1
   Check for in-progress work (crash recovery)`

	input := extractToolInput(pane, "Bash")
	if input == "" {
		t.Error("expected non-empty input")
	}
	if !strings.Contains(input, "bd list") {
		t.Errorf("expected input to contain 'bd list', got %q", input)
	}
}

func TestExtractToolInput_SkipsUIDecoration(t *testing.T) {
	pane := `● Bash
  ⎿  Running…
   actual command here`

	input := extractToolInput(pane, "Bash")
	if strings.Contains(input, "Running") {
		t.Errorf("should skip UI decoration, got %q", input)
	}
	if !strings.Contains(input, "actual command") {
		t.Errorf("should capture actual content, got %q", input)
	}
}

func TestExtractToolInput_LastOccurrence(t *testing.T) {
	// Two tool headers — should extract from the LAST one.
	pane := `● Bash
   first command
● Bash
   second command`

	input := extractToolInput(pane, "Bash")
	if !strings.Contains(input, "second") {
		t.Errorf("should extract from last header, got %q", input)
	}
}

func approvalPromptPane() string {
	return `● Read(file_path: /tmp/test.txt)

 This command requires approval

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don't ask again for: Read:*
   3. No`
}

func pendingInteractionSeamResult(session string, pending *runtime.PendingInteraction, err error, calls [][]string) workertest.Result {
	profile := phase2ReportProfile()
	evidence := map[string]string{
		"session":         session,
		"tmux_call_count": fmt.Sprintf("%d", len(calls)),
	}
	if err != nil {
		evidence["error"] = err.Error()
		return workertest.Fail(profile, workertest.RequirementInteractionPending, fmt.Sprintf("Pending: %v", err)).WithEvidence(evidence)
	}
	if pending == nil {
		return workertest.Fail(profile, workertest.RequirementInteractionPending, "expected pending interaction").WithEvidence(evidence)
	}
	evidence["kind"] = pending.Kind
	evidence["tool_name"] = pending.Metadata["tool_name"]
	evidence["source"] = pending.Metadata["source"]
	if pending.Kind != "approval" {
		return workertest.Fail(profile, workertest.RequirementInteractionPending,
			fmt.Sprintf("Kind = %q, want approval", pending.Kind)).WithEvidence(evidence)
	}
	if pending.Metadata["tool_name"] != "Read" {
		return workertest.Fail(profile, workertest.RequirementInteractionPending,
			fmt.Sprintf("tool_name = %q, want Read", pending.Metadata["tool_name"])).WithEvidence(evidence)
	}
	if pending.Metadata["source"] != "tmux" {
		return workertest.Fail(profile, workertest.RequirementInteractionPending,
			fmt.Sprintf("source = %q, want tmux", pending.Metadata["source"])).WithEvidence(evidence)
	}
	if len(calls) != 1 {
		return workertest.Fail(profile, workertest.RequirementInteractionPending,
			fmt.Sprintf("tmux calls = %d, want 1", len(calls))).WithEvidence(evidence)
	}
	want := []string{"-u", "-L", "phase2-sock", "capture-pane", "-p", "-t", "=" + session + ":", "-S", "-40"}
	if err := matchTMuxCall(calls[0], want); err != nil {
		evidence["tmux_call"] = strings.Join(calls[0], " ")
		return workertest.Fail(profile, workertest.RequirementInteractionPending, err.Error()).WithEvidence(evidence)
	}
	evidence["tmux_call"] = strings.Join(calls[0], " ")
	return workertest.Pass(profile, workertest.RequirementInteractionPending, "tmux provider exposed the pending approval interaction").WithEvidence(evidence)
}

func rejectInteractionSeamResult(session string, err error, calls [][]string) workertest.Result {
	profile := phase2ReportProfile()
	evidence := map[string]string{
		"session":         session,
		"tmux_call_count": fmt.Sprintf("%d", len(calls)),
	}
	if err == nil {
		return workertest.Fail(profile, workertest.RequirementInteractionReject, "Respond should fail for mismatched request ID").WithEvidence(evidence)
	}
	evidence["error"] = err.Error()
	if !strings.Contains(err.Error(), "approval prompt changed") {
		return workertest.Fail(profile, workertest.RequirementInteractionReject,
			fmt.Sprintf("Respond error = %v, want approval prompt changed", err)).WithEvidence(evidence)
	}
	if len(calls) != 1 {
		return workertest.Fail(profile, workertest.RequirementInteractionReject,
			fmt.Sprintf("tmux calls = %d, want 1", len(calls))).WithEvidence(evidence)
	}
	call := strings.Join(calls[0], " ")
	evidence["tmux_call"] = call
	if strings.Contains(call, "send-keys") {
		return workertest.Fail(profile, workertest.RequirementInteractionReject,
			"Respond sent keys despite mismatched request").WithEvidence(evidence)
	}
	return workertest.Pass(profile, workertest.RequirementInteractionReject, "tmux provider rejected the mismatched approval without sending input").WithEvidence(evidence)
}

func respondInteractionSeamResult(session string, err error, calls [][]string) workertest.Result {
	profile := phase2ReportProfile()
	evidence := map[string]string{
		"session":         session,
		"tmux_call_count": fmt.Sprintf("%d", len(calls)),
	}
	if err != nil {
		evidence["error"] = err.Error()
		return workertest.Fail(profile, workertest.RequirementInteractionRespond, fmt.Sprintf("Respond: %v", err)).WithEvidence(evidence)
	}
	if len(calls) != 4 {
		return workertest.Fail(profile, workertest.RequirementInteractionRespond,
			fmt.Sprintf("tmux calls = %d, want 4", len(calls))).WithEvidence(evidence)
	}
	// The #{pane_in_mode} probe is the ga-c4w major #2 copy-mode guard: Respond
	// checks whether the pane is parked in copy-mode before delivering the
	// keystroke. Here the probe reports not-parked ("0"), so no -X cancel is
	// issued and delivery is otherwise unchanged.
	wantCalls := [][]string{
		{"-u", "-L", "phase2-sock", "capture-pane", "-p", "-t", "=" + session + ":", "-S", "-40"},
		{"-u", "-L", "phase2-sock", "display-message", "-t", "=" + session + ":", "-p", "#{pane_in_mode}"},
		{"-u", "-L", "phase2-sock", "send-keys", "-t", "=" + session + ":", "-l", "1"},
		{"-u", "-L", "phase2-sock", "capture-pane", "-p", "-t", "=" + session + ":", "-S", "-40"},
	}
	for i, want := range wantCalls {
		if callErr := matchTMuxCall(calls[i], want); callErr != nil {
			evidence["tmux_call_index"] = fmt.Sprintf("%d", i)
			evidence["tmux_call"] = strings.Join(calls[i], " ")
			return workertest.Fail(profile, workertest.RequirementInteractionRespond, callErr.Error()).WithEvidence(evidence)
		}
	}
	evidence["tmux_calls"] = strings.Join([]string{
		strings.Join(calls[0], " "),
		strings.Join(calls[1], " "),
		strings.Join(calls[2], " "),
		strings.Join(calls[3], " "),
	}, " | ")
	return workertest.Pass(profile, workertest.RequirementInteractionRespond, "tmux provider approved the interaction and cleared the prompt").WithEvidence(evidence)
}

func interactionInstanceLocalDedupResult(approval *parsedApproval, tmA, tmB *Tmux) workertest.Result {
	profile := phase2ReportProfile()
	evidence := map[string]string{
		"approval_hash": approvalHash(approval),
	}
	if tmA.approvalDedup() == tmB.approvalDedup() {
		return workertest.Fail(profile, workertest.RequirementInteractionInstanceLocalDedup,
			"Tmux instances unexpectedly share dedup state").WithEvidence(evidence)
	}
	if !tmA.approvalDedup().isNew("phase2-local", approval) {
		return workertest.Fail(profile, workertest.RequirementInteractionInstanceLocalDedup,
			"first approval in tmA should be new").WithEvidence(evidence)
	}
	if !tmB.approvalDedup().isNew("phase2-local", approval) {
		return workertest.Fail(profile, workertest.RequirementInteractionInstanceLocalDedup,
			"first approval in tmB should be new").WithEvidence(evidence)
	}
	tmA.approvalDedup().clear("phase2-local")
	if !tmA.approvalDedup().isNew("phase2-local", approval) {
		return workertest.Fail(profile, workertest.RequirementInteractionInstanceLocalDedup,
			"tmA clear should reset only tmA state").WithEvidence(evidence)
	}
	if tmB.approvalDedup().isNew("phase2-local", approval) {
		return workertest.Fail(profile, workertest.RequirementInteractionInstanceLocalDedup,
			"tmB dedup state should remain intact after tmA clear").WithEvidence(evidence)
	}
	return workertest.Pass(profile, workertest.RequirementInteractionInstanceLocalDedup, "tmux approval dedup state is isolated per provider instance").WithEvidence(evidence)
}

func matchTMuxCall(got, want []string) error {
	if len(got) != len(want) {
		return fmt.Errorf("tmux args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("tmux args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	return nil
}

func phase2ReportProfile() workertest.ProfileID {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("PROFILE"))) {
	case string(workertest.ProfileCodexTmuxCLI):
		return workertest.ProfileCodexTmuxCLI
	case string(workertest.ProfileCursorTmuxCLI):
		return workertest.ProfileCursorTmuxCLI
	case string(workertest.ProfileGeminiTmuxCLI):
		return workertest.ProfileGeminiTmuxCLI
	default:
		return workertest.ProfileClaudeTmuxCLI
	}
}

// ---------------------------------------------------------------------------
// Claude Code 2.1.284 fixtures (#2892)
//
// The files under testdata/claude-approval are real `tmux capture-pane -p`
// captures of the claude CLI (Claude Code v2.1.284, Haiku and Sonnet) sitting
// on permission prompts in a scratch session. Unlike the hand-written legacy
// panes above, today's layout:
//   - renders the tool line with an animated ⏺ that blinks to a space, or as
//     prose ("Running date command…"), so no "● Tool(" header is reliable;
//   - omits "This command requires approval" in the default (manual) mode;
//   - wraps long option labels onto continuation lines at 80 columns;
//   - can offer four options, where "3" is "Yes, and switch to auto mode".
// ---------------------------------------------------------------------------

func readApprovalFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "claude-approval", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return string(data)
}

func TestParseApprovalPrompt_ClaudeCodeFixtures(t *testing.T) {
	cases := []struct {
		fixture   string
		tool      string
		input     string
		wantLabel []string
	}{
		{
			fixture: "bash-manual-80x24.txt",
			tool:    "Bash",
			input:   "date > bash-ran.txt",
			wantLabel: []string{
				"Yes",
				"Yes, and always allow access to /private/tmp/claude/aprcap.QPjbuv from this project",
				"No",
			},
		},
		{
			fixture: "bash-manual-160x50.txt",
			tool:    "Bash",
			input:   "date > bash-ran.txt",
			wantLabel: []string{
				"Yes",
				"Yes, and always allow access to /private/tmp/claude/aprcap.QPjbuv from this project",
				"No",
			},
		},
		{
			fixture:   "bash-acceptedits-80x24.txt",
			tool:      "Bash",
			input:     `python3 -c "print(6*7)"`,
			wantLabel: []string{"Yes", "Yes, and don’t ask again for: python3 *", "No"},
		},
		{
			fixture: "bash-automode-option.txt",
			tool:    "Bash",
			input:   `python3 -c "print(6*7)"`,
			wantLabel: []string{
				"Yes",
				"Yes, and don’t ask again for: python3 *",
				"Yes, and switch to auto mode · auto mode handles these prompts for you",
				"No",
			},
		},
		{
			fixture: "edit-80x24.txt",
			tool:    "Edit",
			input:   "notes.txt",
			wantLabel: []string{
				"Yes",
				"Yes, and switch to accept edits (auto-approve file edits and common file commands) for this session (shift+tab)",
				"No",
			},
		},
		{
			fixture: "edit-160x50.txt",
			tool:    "Edit",
			input:   "notes.txt",
			wantLabel: []string{
				"Yes",
				"Yes, and switch to accept edits (auto-approve file edits and common file commands) for this session (shift+tab)",
				"No",
			},
		},
		{
			// Claude Code 2.1.287 moved the command into a ╌-delimited box
			// below Claude's one-line description.
			fixture: "bash-acceptedits-2.1.287-80x24.txt",
			tool:    "Bash",
			input:   `python3 -c "print(6*7)"`,
			wantLabel: []string{
				"Yes",
				"Yes, and don’t ask again for: python3 *",
				"No",
			},
		},
		{
			fixture: "write-80x24.txt",
			tool:    "Write",
			input:   "hello.txt",
			wantLabel: []string{
				"Yes",
				"Yes, and switch to accept edits (auto-approve file edits and common file commands) for this session (shift+tab)",
				"No",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			a := parseApprovalPrompt(readApprovalFixture(t, tc.fixture))
			if a == nil {
				t.Fatal("expected an approval prompt, got nil")
			}
			if a.ToolName != tc.tool {
				t.Errorf("ToolName = %q, want %q", a.ToolName, tc.tool)
			}
			if !strings.Contains(a.Input, tc.input) {
				t.Errorf("Input = %q, want it to contain %q", a.Input, tc.input)
			}
			if got := approvalOptionLabels(a); strings.Join(got, "\n") != strings.Join(tc.wantLabel, "\n") {
				t.Errorf("option labels = %q, want %q", got, tc.wantLabel)
			}
		})
	}
}

func TestParseApprovalPrompt_RequestIDStableAcrossPaneWidths(t *testing.T) {
	for _, pair := range [][2]string{
		{"bash-manual-80x24.txt", "bash-manual-160x50.txt"},
		{"edit-80x24.txt", "edit-160x50.txt"},
		{"bash-acceptedits-2.1.287-80x24.txt", "bash-acceptedits-2.1.287-100x30.txt"},
	} {
		narrow := parseApprovalPrompt(readApprovalFixture(t, pair[0]))
		wide := parseApprovalPrompt(readApprovalFixture(t, pair[1]))
		if narrow == nil || wide == nil {
			t.Fatalf("%v: expected both captures to parse, got %v / %v", pair, narrow, wide)
		}
		if approvalHash(narrow) != approvalHash(wide) {
			t.Errorf("%v: request IDs differ across widths: %+v vs %+v", pair, narrow, wide)
		}
	}
}

func TestParseApprovalPrompt_IdleClaudeCodePanes(t *testing.T) {
	for _, fixture := range []string{"idle-after-deny-80x24.txt", "idle-after-approve.txt"} {
		if a := parseApprovalPrompt(readApprovalFixture(t, fixture)); a != nil {
			t.Errorf("%s: expected no approval prompt, got %+v", fixture, a)
		}
	}
}

func TestParseApprovalPrompt_MenuMustBeLive(t *testing.T) {
	// A menu that has scrolled up above the composer is history, not a live
	// prompt: text typed now goes to the composer.
	pane := readApprovalFixture(t, "bash-manual-80x24.txt") + `
  Ran 1 shell command

────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────
  ⏸ manual mode on · ? for shortcuts · ← for agents`
	if a := parseApprovalPrompt(pane); a != nil {
		t.Fatalf("expected no live approval prompt, got %+v", a)
	}
}

func TestParseApprovalPrompt_IgnoresAPIKeyDialog(t *testing.T) {
	pane := ` Detected a custom API key in your environment

 ANTHROPIC_API_KEY: sk-ant-...XXXX

 Do you want to use this API key?

 ❯ 1. Yes
   2. No (recommended)`
	if a := parseApprovalPrompt(pane); a != nil {
		t.Fatalf("expected the API-key startup dialog not to parse as an approval, got %+v", a)
	}
}

func TestApprovalOptionKey_ChoosesByLabel(t *testing.T) {
	cases := []struct {
		fixture string
		action  string
		want    string
	}{
		{"bash-manual-80x24.txt", "approve", "1"},
		{"bash-manual-80x24.txt", "approve_always", "2"},
		{"bash-manual-80x24.txt", "deny", "3"},
		{"bash-acceptedits-80x24.txt", "approve_always", "2"},
		{"bash-acceptedits-80x24.txt", "deny", "3"},
		// "3" is "Yes, and switch to auto mode" here: deny must be "4".
		{"bash-automode-option.txt", "approve", "1"},
		{"bash-automode-option.txt", "approve_always", "2"},
		{"bash-automode-option.txt", "deny", "4"},
		{"edit-80x24.txt", "approve_accept_edits", "2"},
		{"edit-80x24.txt", "deny", "3"},
		{"write-80x24.txt", "approve", "1"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture+"/"+tc.action, func(t *testing.T) {
			a := parseApprovalPrompt(readApprovalFixture(t, tc.fixture))
			if a == nil {
				t.Fatal("expected an approval prompt, got nil")
			}
			got, err := approvalOptionKey(a, tc.action)
			if err != nil {
				t.Fatalf("approvalOptionKey(%q): %v", tc.action, err)
			}
			if got != tc.want {
				t.Fatalf("approvalOptionKey(%q) = %q, want %q (options %q)", tc.action, got, tc.want, approvalOptionLabels(a))
			}
		})
	}
}

func TestApprovalOptionKey_FailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		pane   string
		action string
	}{
		{
			name:   "no reject option",
			action: "deny",
			pane: ` Bash command

   rm -rf build

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and switch to auto mode`,
		},
		{
			name:   "two plain yes options",
			action: "approve",
			pane: ` Bash command

   rm -rf build

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes
   3. No`,
		},
		{
			name:   "accept edits not offered on a bash prompt",
			action: "approve_accept_edits",
			pane:   readApprovalFixture(t, "bash-automode-option.txt"),
		},
		{
			name:   "unknown action",
			action: "allow",
			pane:   readApprovalFixture(t, "bash-manual-80x24.txt"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := parseApprovalPrompt(tc.pane)
			if a == nil {
				t.Fatal("expected an approval prompt, got nil")
			}
			if key, err := approvalOptionKey(a, tc.action); err == nil {
				t.Fatalf("approvalOptionKey(%q) = %q, want an error (options %q)", tc.action, key, approvalOptionLabels(a))
			}
		})
	}
}

func TestRespond_DenySendsTheNoOptionNotThree(t *testing.T) {
	session := "automode-deny"
	fe := &fakeExecutor{
		// pre-verify capture, #{pane_in_mode} probe (not parked), send-keys,
		// then the verify capture showing the prompt cleared.
		outs: []string{
			readApprovalFixture(t, "bash-automode-option.txt"),
			"0",
			"",
			readApprovalFixture(t, "idle-after-deny-80x24.txt"),
		},
	}
	provider := &Provider{tm: &Tmux{exec: fe}}
	if err := provider.Respond(session, runtime.InteractionResponse{Action: "deny"}); err != nil {
		t.Fatalf("Respond(deny): %v", err)
	}
	var sent []string
	for _, call := range fe.calls {
		if len(call) > 0 && containsArg(call, "send-keys") {
			sent = append(sent, call[len(call)-1])
		}
	}
	if len(sent) != 1 || sent[0] != "4" {
		t.Fatalf("send-keys payloads = %q, want exactly [\"4\"] (the \"No\" option)", sent)
	}
}

func TestRespond_UnresolvableMenuSendsNothing(t *testing.T) {
	session := "no-reject"
	pane := ` Bash command

   rm -rf build

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and switch to auto mode`
	fe := &fakeExecutor{out: pane}
	provider := &Provider{tm: &Tmux{exec: fe}}
	err := provider.Respond(session, runtime.InteractionResponse{Action: "deny"})
	if err == nil {
		t.Fatal("Respond(deny) succeeded on a menu with no reject option, want an error")
	}
	if !errors.Is(err, runtime.ErrInteractionActionUnavailable) {
		t.Fatalf("Respond(deny) error = %v, want runtime.ErrInteractionActionUnavailable", err)
	}
	for _, call := range fe.calls {
		if containsArg(call, "send-keys") {
			t.Fatalf("Respond sent keys despite an unresolvable menu: %v", call)
		}
	}
}

func TestPending_ReportsParsedOptionLabels(t *testing.T) {
	fe := &fakeExecutor{out: readApprovalFixture(t, "bash-automode-option.txt")}
	provider := &Provider{tm: &Tmux{exec: fe}}
	pending, err := provider.Pending("s")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending == nil {
		t.Fatal("Pending = nil, want the approval prompt")
	}
	if len(pending.Options) != 4 || pending.Options[3] != "No" {
		t.Fatalf("Options = %q, want the four labels from the pane", pending.Options)
	}
	if pending.Metadata["tool_name"] != "Bash" {
		t.Fatalf("tool_name = %q, want Bash", pending.Metadata["tool_name"])
	}
}

func TestNudgeNow_RefusesToTypeIntoApprovalPrompt(t *testing.T) {
	fe := &fakeExecutor{out: readApprovalFixture(t, "bash-manual-80x24.txt")}
	provider := &Provider{tm: &Tmux{exec: fe}}
	err := provider.NudgeNow("s", runtime.TextContent("hello?"))
	if !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("NudgeNow error = %v, want runtime.ErrPendingInteraction", err)
	}
	for _, call := range fe.calls {
		if containsArg(call, "send-keys") {
			t.Fatalf("NudgeNow sent keys into a pending approval prompt: %v", call)
		}
	}
}

func TestSnapshotPaneIdle_ApprovalPromptIsNotIdle(t *testing.T) {
	// "❯ 1. Yes" matches the ready-prompt prefix, so without an explicit
	// approval check a permission prompt reads as an idle composer and
	// wait-idle delivery types into it.
	fe := &fakeExecutor{out: readApprovalFixture(t, "bash-manual-80x24.txt")}
	tm := &Tmux{exec: fe}
	idle, err := tm.snapshotPaneIdleWithPrefix("s", DefaultReadyPromptPrefix)
	if err != nil {
		t.Fatalf("snapshotPaneIdleWithPrefix: %v", err)
	}
	if idle {
		t.Fatal("pane showing a permission prompt reported idle")
	}

	fe.out = readApprovalFixture(t, "idle-after-deny-80x24.txt")
	idle, err = tm.snapshotPaneIdleWithPrefix("s", DefaultReadyPromptPrefix)
	if err != nil {
		t.Fatalf("snapshotPaneIdleWithPrefix: %v", err)
	}
	if !idle {
		t.Fatal("idle composer pane reported busy")
	}
}

func containsArg(call []string, want string) bool {
	for _, arg := range call {
		if arg == want {
			return true
		}
	}
	return false
}

// TestParseApprovalPrompt_BashInputLeadsWithCommand pins the contract clients
// rely on: a Bash prompt's Input starts with the command itself, and Claude's
// model-written description (if any) follows it. A client that shows or speaks
// only the first line must never present the description as the command.
func TestParseApprovalPrompt_BashInputLeadsWithCommand(t *testing.T) {
	for _, tc := range []struct {
		fixture, command, description string
	}{
		{"bash-manual-80x24.txt", "date > bash-ran.txt", "Run date command and write output to bash-ran.txt"},
		{"bash-acceptedits-2.1.287-80x24.txt", `python3 -c "print(6*7)"`, "Run Python command to calculate 6*7"},
		{"bash-acceptedits-2.1.287-100x30.txt", `python3 -c "print(6*7)"`, "Run Python command to calculate 6*7"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			a := parseApprovalPrompt(readApprovalFixture(t, tc.fixture))
			if a == nil {
				t.Fatal("expected an approval prompt, got nil")
			}
			lines := strings.Split(a.Input, "\n")
			if lines[0] != tc.command {
				t.Errorf("first line of Input = %q, want the command %q (Input = %q)", lines[0], tc.command, a.Input)
			}
			if !strings.Contains(a.Input, tc.description) {
				t.Errorf("Input = %q, want it to keep the description %q after the command", a.Input, tc.description)
			}
			if got, want := approvalPromptText(a), "Bash: "+tc.command; !strings.HasPrefix(got, want) {
				t.Errorf("prompt = %q, want it to start with %q", got, want)
			}
		})
	}
}
