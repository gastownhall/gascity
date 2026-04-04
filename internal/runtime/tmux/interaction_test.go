package tmux

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
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
	session := "phase2-pending"
	fe := &fakeExecutor{out: approvalPromptPane()}
	provider := &Provider{
		tm: &Tmux{
			cfg:  Config{SocketName: "phase2-sock"},
			exec: fe,
		},
	}

	pending, err := provider.Pending(session)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending == nil {
		t.Fatal("expected pending interaction")
	}
	if pending.Kind != "approval" {
		t.Fatalf("Kind = %q, want approval", pending.Kind)
	}
	if pending.Metadata["tool_name"] != "Read" {
		t.Fatalf("tool_name = %q, want Read", pending.Metadata["tool_name"])
	}
	if pending.Metadata["source"] != "tmux" {
		t.Fatalf("source = %q, want tmux", pending.Metadata["source"])
	}
	if len(fe.calls) != 1 {
		t.Fatalf("tmux calls = %d, want 1", len(fe.calls))
	}
	want := []string{"-u", "-L", "phase2-sock", "capture-pane", "-p", "-t", session, "-S", "-40"}
	assertTMuxCall(t, fe.calls[0], want)
}

func TestPhase2ProviderRespondRejectsMismatchedRequest(t *testing.T) {
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
	if err == nil {
		t.Fatal("Respond should fail for mismatched request ID")
	}
	if !strings.Contains(err.Error(), "approval prompt changed") {
		t.Fatalf("Respond error = %v, want approval prompt changed", err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("tmux calls = %d, want 1", len(fe.calls))
	}
	if strings.Contains(strings.Join(fe.calls[0], " "), "send-keys") {
		t.Fatal("Respond sent keys despite mismatched request")
	}
}

func TestPhase2ProviderRespondApprovesAndClearsPrompt(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if len(fe.calls) != 3 {
		t.Fatalf("tmux calls = %d, want 3", len(fe.calls))
	}
	assertTMuxCall(t, fe.calls[0], []string{"-u", "-L", "phase2-sock", "capture-pane", "-p", "-t", session, "-S", "-40"})
	assertTMuxCall(t, fe.calls[1], []string{"-u", "-L", "phase2-sock", "send-keys", "-t", session, "-l", "1"})
	assertTMuxCall(t, fe.calls[2], []string{"-u", "-L", "phase2-sock", "capture-pane", "-p", "-t", session, "-S", "-40"})
}

func TestPhase2ProviderPendingDedupIsInstanceLocal(t *testing.T) {
	approval := &parsedApproval{ToolName: "Read", Input: "file_path: /tmp/test.txt"}
	tmA := &Tmux{}
	tmB := &Tmux{}

	if tmA.approvalDedup() == tmB.approvalDedup() {
		t.Fatal("Tmux instances unexpectedly share dedup state")
	}
	if !tmA.approvalDedup().isNew("phase2-local", approval) {
		t.Fatal("first approval in tmA should be new")
	}
	if !tmB.approvalDedup().isNew("phase2-local", approval) {
		t.Fatal("first approval in tmB should be new")
	}

	tmA.approvalDedup().clear("phase2-local")
	if !tmA.approvalDedup().isNew("phase2-local", approval) {
		t.Fatal("tmA clear should reset only tmA state")
	}
	if tmB.approvalDedup().isNew("phase2-local", approval) {
		t.Fatal("tmB dedup state should remain intact after tmA clear")
	}
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

func assertTMuxCall(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("tmux args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tmux args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
