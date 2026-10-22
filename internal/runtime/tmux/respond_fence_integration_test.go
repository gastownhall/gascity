//go:build integration

package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// promptScript is a pane that shows a Claude Code approval prompt and writes
// each key it is typed to file. After each key it shows the prompt again
// (an identical prompt raised right after the answer, as two parallel
// identical tool calls do) until it has taken rounds keys, then clears.
func promptScript(t *testing.T, file string, rounds int) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "prompt.sh")
	body := fmt.Sprintf(`#!/bin/sh
show() {
  printf '\033[H\033[2J'
  printf '● Read(file_path: /tmp/test.txt)\r\n\r\n Do you want to proceed?\r\n ❯ 1. Yes\r\n   2. Yes, and don'"'"'t ask again for: Read:*\r\n   3. No\r\n'
}
stty raw -echo
show
: > %[1]s
n=0
while [ "$n" -lt %[2]d ]; do
  dd bs=1 count=1 2>/dev/null >> %[1]s
  n=$((n+1))
  if [ "$n" -lt %[2]d ]; then show; fi
done
printf '\033[H\033[2J'
printf 'assistant ready\r\n'
exec sleep 300
`, file, rounds)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// startPrompt starts session name as f's incarnation showing promptScript's
// prompt, and returns its key file once the prompt shows.
func startPrompt(t *testing.T, p *Provider, name string, f runtime.InputFence, rounds int) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "typed")
	if err := p.Start(context.Background(), name, runtime.Config{Command: promptScript(t, file, rounds), WorkDir: dir, Env: fenceEnv(f)}); err != nil {
		t.Fatalf("Start %s: %v", name, err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	waitCaptureFile(t, file)
	eventually(t, "the approval prompt", func() bool {
		pending, err := p.Pending(name)
		return err == nil && pending != nil
	})
	return file
}

// A fenced answer reaches only the incarnation it names (DES-O1 §6.2, I6):
// under another incarnation's fence it sends nothing and misses; under its
// own it answers, and the prompt clears.
func TestRespondFencedRealTmuxAnswersOnlyItsIncarnation(t *testing.T) {
	withFastRespondVerify(t, 5*time.Second)
	p := fenceTestProvider(t, "rf")
	const name = "gc-respond-fenced"
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "token-r"}
	other := runtime.InputFence{Generation: 2, Epoch: 1, Token: "token-r2"}
	file := startPrompt(t, p, name, f, 1)
	pending, err := p.Pending(name)
	if err != nil || pending == nil {
		t.Fatalf("Pending = %+v, %v", pending, err)
	}
	answer := runtime.InteractionResponse{RequestID: pending.RequestID, Action: "approve"}
	var miss *runtime.FenceMismatchError
	if err := p.RespondFenced(context.Background(), name, other, answer); !errors.As(err, &miss) || miss.Partial {
		t.Fatalf("RespondFenced(other incarnation) = %v, want a fence miss before any key", err)
	}
	if got := typedSoFar(file); got != "" {
		t.Fatalf("a misfenced answer typed %q", got)
	}
	if err := p.RespondFenced(context.Background(), name, f, answer); err != nil {
		t.Fatalf("RespondFenced(own incarnation) = %v", err)
	}
	if got := waitTyped(t, file, containsFn("1")); got != "1" {
		t.Fatalf("pane received %q, want the one answer key", got)
	}
}

// Two identical prompts back to back share a request id (a content hash):
// tmux sees the second as the first not clearing, so Respond reports an
// error that is none of its nothing-sent refusals. The lane must treat it as
// an answer that may have landed (a respond is at most once), never resend
// it into the second prompt; and exactly one key reached the pane.
func TestRespondRealTmuxBackToBackIdenticalPrompts(t *testing.T) {
	withFastRespondVerify(t, time.Second)
	p := fenceTestProvider(t, "rb")
	const name = "gc-respond-twice"
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "token-b"}
	file := startPrompt(t, p, name, f, 2)
	pending, err := p.Pending(name)
	if err != nil || pending == nil {
		t.Fatalf("Pending = %+v, %v", pending, err)
	}
	err = p.RespondFenced(context.Background(), name, f, runtime.InteractionResponse{RequestID: pending.RequestID, Action: "approve"})
	if err == nil {
		t.Fatal("Respond into a prompt an identical one replaced reported success")
	}
	for _, refusal := range []error{
		runtime.ErrInteractionGone, runtime.ErrInteractionResponseInvalid, runtime.ErrInteractionActionUnavailable,
		runtime.ErrRuntimeUnavailable, runtime.ErrSessionNotFound, runtime.ErrFenceMismatch,
	} {
		if errors.Is(err, refusal) {
			t.Fatalf("Respond = %v, a nothing-sent refusal (%v) for an answer that was typed", err, refusal)
		}
	}
	if got := typedSoFar(file); got != "1" {
		t.Fatalf("pane received %q, want exactly one answer key", got)
	}
	// The second prompt shows the same request id.
	again, err := p.Pending(name)
	if err != nil || again == nil || again.RequestID != pending.RequestID {
		t.Fatalf("Pending after the answer = %+v, %v; want the identical prompt", again, err)
	}
}
