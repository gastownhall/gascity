package tmux

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func nativeQuestionHookPayload(event, tool string) string {
	p := projectedClaudeQuestion()
	value := map[string]any{
		"hook_event_name": event, "session_id": p.SessionID, "prompt_id": p.PromptID,
		"tool_use_id": tool, "tool_name": "AskUserQuestion", "tool_input": map[string]any{"questions": p.Questions},
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func TestClaudeQuestionHookProjectionIsInvocationAndIncarnationScoped(t *testing.T) {
	env := fenceEnv(testFence)
	env["TMUX"] = "/tmp/native-question,1234,0"
	env["TMUX_PANE"] = "%1"
	current := testFence.Digest()
	options := map[string]string{}
	var calls [][]string
	run := func(_ context.Context, args []string) (string, error) {
		calls = append(calls, slices.Clone(args))
		if !slices.Equal(args[:2], []string{"-S", "/tmp/native-question"}) {
			t.Fatalf("wrong server: %q", args)
		}
		args = args[2:]
		switch args[0] {
		case "display-message":
			return "1234", nil
		case "show-options":
			var lines []string
			for k, v := range options {
				lines = append(lines, k+" "+v)
			}
			return strings.Join(lines, "\n"), nil
		case "if-shell":
			if !strings.Contains(args[4], current+".#{pane_id}") {
				return runtime.TmuxFenceMissPrefix + "changed", nil
			}
			inner := shellquote.Split(args[5])
			if inner[0] != "set-option" || !slices.Contains(inner, "-p") || !slices.Contains(inner, "%1") {
				t.Fatalf("hook escaped its pane: %q", inner)
			}
			if slices.Contains(inner, "-u") {
				delete(options, inner[len(inner)-1])
			} else {
				options[inner[len(inner)-2]] = inner[len(inner)-1]
			}
			return "", nil
		}
		return "", fmt.Errorf("unexpected tmux call %q", args)
	}
	observe := func(event, tool string) error {
		return observeClaudeQuestionHook(context.Background(), strings.NewReader(nativeQuestionHookPayload(event, tool)), env, run)
	}
	for _, tool := range []string{"toolu_first", "toolu_second", "toolu_second"} {
		if err := observe("PreToolUse", tool); err != nil {
			t.Fatal(err)
		}
	}
	if len(options) != 2 {
		t.Fatalf("parallel/idempotent callbacks lost an invocation: %v", options)
	}
	for key, value := range options {
		data, err := base64.RawStdEncoding.DecodeString(value)
		if err != nil {
			t.Fatal(err)
		}
		var p claudeQuestionProjection
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatal(err)
		}
		if p.optionKey() != key || p.FenceDigest != current || p.Questions[0].Question != projectedClaudeQuestion().Questions[0].Question {
			t.Fatalf("native identity/input did not survive projection: %+v", p)
		}
	}
	if err := observe("PostToolUse", "toolu_first"); err != nil {
		t.Fatal(err)
	}
	if len(options) != 1 {
		t.Fatal("post callback cleared a different pending invocation")
	}
	for _, value := range options {
		data, _ := base64.RawStdEncoding.DecodeString(value)
		if !strings.Contains(string(data), "toolu_second") {
			t.Fatal("late callback deleted the newer invocation")
		}
	}
	current = runtime.InputFence{Generation: testFence.Generation + 1, Epoch: testFence.Epoch, Token: "replacement-token"}.Digest()
	if err := observe("PostToolUseFailure", "toolu_second"); err == nil {
		t.Fatal("stale callback reached a replacement incarnation")
	}
	if len(options) != 1 {
		t.Fatal("stale callback changed pane state")
	}
	for _, args := range calls {
		if strings.Contains(strings.Join(args, " "), testFence.Token) {
			t.Fatal("instance token leaked into tmux arguments")
		}
	}
}

func TestClaudeQuestionHookRejectsUnboundAndOversizedCallbacks(t *testing.T) {
	env := fenceEnv(testFence)
	env["TMUX"] = "/tmp/native-question,1234,0"
	env["TMUX_PANE"] = "%1"
	invoked := false
	run := func(context.Context, []string) (string, error) { invoked = true; return "", nil }
	for _, payload := range []string{
		"{}", strings.Repeat("x", claudeQuestionProjectionLimit+1),
		strings.ReplaceAll(nativeQuestionHookPayload("PreToolUse", "toolu_1"), "prompt-first", ""),
	} {
		if err := observeClaudeQuestionHook(context.Background(), strings.NewReader(payload), env, run); err == nil {
			t.Fatal("unbound native callback accepted")
		}
	}
	if invoked {
		t.Fatal("invalid callback touched tmux")
	}
}

type claudeProjectionExecutor struct {
	paneExecutor
	env            map[string]string
	fence, options string
}

func (e *claudeProjectionExecutor) execute(args []string) (string, error) {
	e.calls = append(e.calls, slices.Clone(args))
	switch {
	case slices.Contains(args, "show-environment"):
		var lines []string
		for key, value := range e.env {
			lines = append(lines, key+"="+value)
		}
		return strings.Join(lines, "\n"), nil
	case slices.Contains(args, fenceReadFormat):
		return e.fence, nil
	case slices.Contains(args, "show-options"):
		return e.options, nil
	}
	return e.paneExecutor.execute(args)
}

func (e *claudeProjectionExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

func TestClaudeQuestionProjectionReaderRefusesUnprovenIdentity(t *testing.T) {
	for _, scenario := range []string{"valid", "changed fence", "wrong native session", "invalid value", "wrong key", "oversized value"} {
		t.Run(scenario, func(t *testing.T) {
			env := fenceEnv(testFence)
			env["GC_PROVIDER_SESSION_ID"] = "native-session"
			p := projectedClaudeQuestion()
			p.FenceDigest = testFence.Digest()
			key := p.optionKey()
			data, _ := json.Marshal(p)
			encoded := base64.RawStdEncoding.EncodeToString(data)
			ex := &claudeProjectionExecutor{env: env, fence: testFence.Digest() + ".%1|%1|0"}
			switch scenario {
			case "changed fence":
				ex.fence = "wrong.%1|%1|0"
			case "wrong native session":
				env["GC_PROVIDER_SESSION_ID"] = "another"
			case "invalid value":
				encoded = "not-base64!"
			case "wrong key":
				key += "wrong"
			case "oversized value":
				encoded = strings.Repeat("x", claudeQuestionProjectionLimit+1)
			}
			ex.options = key + " " + encoded
			tm := &Tmux{cfg: DefaultConfig(), exec: ex}
			rows, err := tm.readClaudeQuestionProjections("=q:@1.%1")
			if scenario == "valid" {
				if err != nil || len(rows) != 1 || rows[0].ToolUseID != p.ToolUseID {
					t.Fatalf("read=%+v,%v", rows, err)
				}
			} else if err == nil {
				t.Fatalf("unproven projection accepted: %+v", rows)
			}
			requireNoKeys(t, ex.calls, false)
		})
	}
}
