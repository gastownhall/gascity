package tmux

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func approvalHookPayload(event, session, prompt, tool, command string) string {
	data, _ := json.Marshal(map[string]any{
		"hook_event_name": event, "session_id": session, "prompt_id": prompt,
		"tool_use_id": tool, "tool_name": "Bash", "tool_input": map[string]any{"command": command, "description": "Write qualification marker to grant-once file"},
	})
	return string(data)
}

func TestClaudeApprovalHookNativeCallbacksAreBoundedAndInvocationScoped(t *testing.T) {
	_, ex, _, session, prompt, command := approvalHookTmux(t)
	env := ex.env
	env["TMUX"] = "/tmp/owned-native-approval,1234,0"
	env["TMUX_PANE"] = "%1"
	run := func(ctx context.Context, args []string) (string, error) {
		if args[0] != "-S" || args[1] != "/tmp/owned-native-approval" {
			t.Fatal("callback escaped its exact server")
		}
		if args[2] == "display-message" && args[len(args)-1] == "#{pid}" {
			return "1234", nil
		}
		return ex.executeCtx(ctx, args[2:])
	}
	observe := func(event, tool string) error {
		return observeClaudeApprovalHook(context.Background(), strings.NewReader(approvalHookPayload(event, session, prompt, tool, command)), env, run)
	}
	for _, id := range []string{"toolu_first", "toolu_second", "toolu_second"} {
		if err := observe("PreToolUse", id); err != nil {
			t.Fatal(err)
		}
	}
	if len(ex.options) != 2 {
		t.Fatal("concurrent/idempotent observations lost native invocation identity")
	}
	firstKey, _ := approvalHookTestReceipt(session, prompt, "toolu_first", command, false)
	secondKey, _ := approvalHookTestReceipt(session, prompt, "toolu_second", command, false)
	if err := observe("PostToolUse", "toolu_first"); err != nil {
		t.Fatal(err)
	}
	if ex.options[firstKey] != "" || ex.options[secondKey] == "" {
		t.Fatal("completion cleared another invocation")
	}
	second := ex.options[secondKey]
	data, _ := base64.RawStdEncoding.DecodeString(second)
	var p claudeApprovalProjection
	_ = json.Unmarshal(data, &p)
	p.Consumed = true
	encoded, err := p.encode()
	if err != nil {
		t.Fatal(err)
	}
	ex.options[secondKey] = encoded
	if err := observe("PreToolUse", "toolu_second"); err == nil || ex.options[secondKey] != encoded {
		t.Fatal("duplicate callback resurrected consumed authority")
	}
	if err := observe("PostToolUseFailure", "toolu_second"); err != nil || len(ex.options) != 0 {
		t.Fatal("completion failed to retire only its consumed invocation")
	}
	// A late completion with no own observation does not clear a newer call.
	if err := observe("PreToolUse", "toolu_new"); err != nil {
		t.Fatal(err)
	}
	if err := observe("PostToolUse", "toolu_first"); err != nil || len(ex.options) != 1 {
		t.Fatal("late completion changed another call")
	}
	if ex.inputAttempts != 0 {
		t.Fatal("native observation answered an approval")
	}
	for i := 1; i < 32; i++ {
		if err := observe("PreToolUse", fmt.Sprintf("toolu_bounded_%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := observe("PreToolUse", "toolu_over_limit"); err == nil || len(ex.options) != 32 {
		t.Fatal("outstanding callback bound was not enforced")
	}
}

func TestClaudeApprovalHookRejectsInvalidForeignOversizedAndChangedCallbacks(t *testing.T) {
	_, ex, _, session, prompt, command := approvalHookTmux(t)
	env := ex.env
	env["TMUX"] = "/tmp/owned-native-approval,1234,0"
	env["TMUX_PANE"] = "%1"
	called := false
	run := func(context.Context, []string) (string, error) { called = true; return "", nil }
	for _, payload := range []string{
		"{}", strings.Repeat("x", claudeQuestionProjectionLimit+1),
		approvalHookPayload("PermissionRequest", session, prompt, "", command),
		approvalHookPayload("PreToolUse", "foreign", prompt, "toolu_one", command),
		approvalHookPayload("PreToolUse", session, "", "toolu_one", command),
		approvalHookPayload("PreToolUse", session, prompt, "", command),
	} {
		if err := observeClaudeApprovalHook(context.Background(), strings.NewReader(payload), env, run); err == nil {
			t.Fatal("unbound callback accepted")
		}
	}
	if called {
		t.Fatal("invalid callback touched tmux")
	}
	key, value := approvalHookTestReceipt(session, prompt, "toolu_one", command, false)
	ex.options[key] = value
	run = func(ctx context.Context, args []string) (string, error) {
		if args[2] == "display-message" {
			return "1234", nil
		}
		return ex.executeCtx(ctx, args[2:])
	}
	if err := observeClaudeApprovalHook(context.Background(), strings.NewReader(approvalHookPayload("PreToolUse", session, prompt, "toolu_one", command+" ")), env, run); err == nil || ex.options[key] != value {
		t.Fatal("same-ID command conflict changed its observation")
	}
	ex.drift = true
	if err := observeClaudeApprovalHook(context.Background(), strings.NewReader(approvalHookPayload("PreToolUse", session, prompt, "toolu_new", command)), env, run); err == nil || len(ex.options) != 1 {
		t.Fatal("replacement incarnation accepted callback")
	}

	// Stop's read-only scan must preserve the input operation's fence taxonomy
	// at either bookend, before it can consume any observation or send a key.
	var environment []string
	for key, value := range fenceEnv(testFence) {
		environment = append(environment, key+"="+value)
	}
	environment = append(environment, "GC_PROVIDER_SESSION_ID=native-approval-session")
	current := testFence.Digest() + ".%1|%1|0"
	newer := runtime.InputFence{Generation: 3, Epoch: 1, Token: "newer"}
	for _, bookend := range []int{1, 3} {
		for _, fault := range []struct{ name, output, found string }{
			{"absent", "||", runtime.FenceFoundAbsent},
			{"dead", testFence.Digest() + ".%1|%1|1", runtime.FenceFoundDead},
			{"new incarnation", newer.Digest() + ".%1|%1|0", newer.Digest()},
			{"unproven", "|%1|0", runtime.FenceFoundUnproven},
		} {
			t.Run(fmt.Sprintf("Stop bookend %d/%s", bookend, fault.name), func(t *testing.T) {
				fe := &fakeExecutor{outs: []string{strings.Join(environment, "\n"), current, "", current}}
				fe.outs[bookend] = fault.output
				tm := &Tmux{exec: fe}
				guard := newPaneGuard("q", testFence.Digest(), false)
				guard.pane = "%1"
				err := tm.consumeClaudeApprovalsForStop(withPaneGuard(context.Background(), guard), "=q:@1.%1")
				var miss *runtime.FenceMismatchError
				if !errors.As(err, &miss) || miss.Found != fault.found || miss.Partial || errors.Is(err, runtime.ErrInputNotSent) {
					t.Fatalf("Stop scan = %v, want non-partial %s fence miss", err, fault.found)
				}
				for _, call := range fe.calls {
					if slices.Contains(call, "if-shell") || slices.Contains(call, "send-keys") || slices.Contains(call, "set-option") {
						t.Fatalf("failed Stop scan mutated the pane: %q", call)
					}
				}
			})
		}
	}
	for _, step := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprintf("Stop preserves read error %d", step), func(t *testing.T) {
			readErr := errors.New("constructed native approval read failure")
			fe := &fakeExecutor{outs: []string{strings.Join(environment, "\n"), current, "", current}, errs: make([]error, 4)}
			fe.errs[step] = readErr
			guard := newPaneGuard("q", testFence.Digest(), false)
			guard.pane = "%1"
			err := (&Tmux{exec: fe}).consumeClaudeApprovalsForStop(withPaneGuard(context.Background(), guard), "=q:@1.%1")
			if !errors.Is(err, readErr) {
				t.Fatalf("Stop scan = %v, want the original read failure", err)
			}
			if len(fe.calls) != step+1 {
				t.Fatalf("failed Stop scan continued: %q", fe.calls)
			}
		})
	}
}
