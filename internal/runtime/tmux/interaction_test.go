package tmux

import (
	"errors"
	"fmt"
	"os"
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
			approvalPromptPane(),
			"",
			`assistant ready`,
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
	want := []string{"-u", "-L", "phase2-sock", "capture-pane", "-p", "-t", session, "-S", "-40"}
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
	if len(calls) != 3 {
		return workertest.Fail(profile, workertest.RequirementInteractionRespond,
			fmt.Sprintf("tmux calls = %d, want 3", len(calls))).WithEvidence(evidence)
	}
	wantCalls := [][]string{
		{"-u", "-L", "phase2-sock", "capture-pane", "-p", "-t", session, "-S", "-40"},
		{"-u", "-L", "phase2-sock", "send-keys", "-t", session, "-l", "1"},
		{"-u", "-L", "phase2-sock", "capture-pane", "-p", "-t", session, "-S", "-40"},
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
	case string(workertest.ProfileGeminiTmuxCLI):
		return workertest.ProfileGeminiTmuxCLI
	default:
		return workertest.ProfileClaudeTmuxCLI
	}
}
