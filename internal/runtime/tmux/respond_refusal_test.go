package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Every refusal of Respond that sends nothing names why with a runtime
// sentinel, so a caller can tell a refusal from an answer that may have
// landed: a prompt gone or changed is ErrInteractionGone, an action no prompt
// offers is ErrInteractionResponseInvalid, and a pane that could not be read,
// or a key that found no tmux server, is ErrRuntimeUnavailable.
func TestRespondRefusalsNameWhy(t *testing.T) {
	requestID := "tmux-" + approvalHash(&parsedApproval{ToolName: "Read", Input: "file_path: /tmp/test.txt"})
	cases := []struct {
		name     string
		exec     *fakeExecutor
		response runtime.InteractionResponse
		want     error
	}{
		{
			"prompt gone", &fakeExecutor{agentPane: true, outs: []string{"assistant ready"}},
			runtime.InteractionResponse{RequestID: requestID, Action: "approve"},
			runtime.ErrInteractionGone,
		},
		{
			"prompt changed", &fakeExecutor{agentPane: true, outs: []string{approvalPromptPane()}},
			runtime.InteractionResponse{RequestID: "tmux-other", Action: "approve"},
			runtime.ErrInteractionGone,
		},
		{
			"unknown action", &fakeExecutor{agentPane: true, outs: []string{approvalPromptPane()}},
			runtime.InteractionResponse{RequestID: requestID, Action: "answer"},
			runtime.ErrInteractionResponseInvalid,
		},
		{
			"capture failed", &fakeExecutor{agentPane: true, errs: []error{errors.New("server busy")}},
			runtime.InteractionResponse{RequestID: requestID, Action: "approve"},
			runtime.ErrRuntimeUnavailable,
		},
		{
			"pane lookup failed", &fakeExecutor{err: errors.New("server busy")},
			runtime.InteractionResponse{RequestID: requestID, Action: "approve"},
			runtime.ErrRuntimeUnavailable,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			provider := &Provider{tm: &Tmux{exec: c.exec}}
			err := provider.Respond("s", c.response)
			if !errors.Is(err, c.want) {
				t.Fatalf("Respond = %v, want %v", err, c.want)
			}
			if sent := sentKeys(c.exec.calls); len(sent) != 0 {
				t.Fatalf("a refusal sent %q", sent)
			}
		})
	}
	// An answer naming no request when no prompt shows stays a no-op.
	provider := &Provider{tm: &Tmux{exec: &fakeExecutor{agentPane: true, outs: []string{"assistant ready"}}}}
	if err := provider.Respond("s", runtime.InteractionResponse{Action: "approve"}); err != nil {
		t.Fatalf("Respond with no request and no prompt = %v", err)
	}
	// The pane shows the prompt, out of copy-mode, and the key finds the
	// server gone: no key reached the prompt.
	exec := &fakeExecutor{agentPane: true, outs: []string{approvalPromptPane(), "0"}, errs: []error{nil, nil, ErrNoServer}}
	err := (&Provider{tm: &Tmux{exec: exec}}).Respond("s", runtime.InteractionResponse{RequestID: requestID, Action: "approve"})
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !slices.Equal(sentKeys(exec.calls), []string{"1"}) {
		t.Fatalf("Respond = %v after keys %q, want ErrRuntimeUnavailable from the one key", err, sentKeys(exec.calls))
	}
}

// tmux fences answers as it fences input.
func TestProviderResponseFencingIsAtomic(t *testing.T) {
	if got := runtime.ResponseFencingFor(&Provider{}, "s"); got != runtime.InputFencingAtomic {
		t.Fatalf("ResponseFencingFor(tmux) = %s, want atomic", got)
	}
}

// A fenced answer types its key inside the agent pane's guard (if-shell -F),
// never as a bare send-keys: a respawn between the fence check and the key
// then sends nothing to the new incarnation (DES-O1 I6), and the miss is a
// *runtime.FenceMismatchError.
func TestRespondFencedTypesTheAnswerUnderThePaneGuard(t *testing.T) {
	interval := respondVerifyInterval
	respondVerifyInterval = time.Millisecond
	t.Cleanup(func() { respondVerifyInterval = interval })
	requestID := "tmux-" + approvalHash(&parsedApproval{ToolName: "Read", Input: "file_path: /tmp/test.txt"})
	answer := runtime.InteractionResponse{RequestID: requestID, Action: "approve"}
	// model shows the approval until an answer lands.
	model := func() *fenceModel {
		m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
		answered := false
		m.screen = func(*fenceModel) string {
			if answered {
				return "$ "
			}
			return approvalPromptPane()
		}
		m.onEvent = func(_ *fenceModel, ev string) { answered = answered || strings.HasPrefix(ev, "text:") }
		return m
	}
	t.Run("answered", func(t *testing.T) {
		m := model()
		if err := fenceModelProvider(m).RespondFenced(context.Background(), fenceModelSession, testFence, answer); err != nil {
			t.Fatalf("RespondFenced = %v", err)
		}
		bare, events, _ := m.snapshot()
		if len(bare) != 0 || !slices.Equal(events, []string{"text:1"}) || m.guards == 0 {
			t.Fatalf("bare %q, events %q, guards %d; want the answer digit under the guard", bare, events, m.guards)
		}
	})
	t.Run("respawned before the key", func(t *testing.T) {
		m := model()
		m.beforeGuard = func(m *fenceModel, _ int) { m.dead = true }
		err := fenceModelProvider(m).RespondFenced(context.Background(), fenceModelSession, testFence, answer)
		var miss *runtime.FenceMismatchError
		if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundDead {
			t.Fatalf("RespondFenced = %v, want a fence miss finding a dead pane", err)
		}
		if bare, events, _ := m.snapshot(); len(bare) != 0 || len(events) != 0 {
			t.Fatalf("bare %q, events %q; want nothing typed", bare, events)
		}
	})
	// A failure before the guarded paste, reading the fence or staging the
	// key, sends nothing and says so (ErrInputNotSent): it is no miss, and
	// no error after a key that may have landed.
	loadErr := errors.New("load-buffer: no space left on device")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		fault func(*fenceModel)
		cause error
	}{
		{"staging the key failed", context.Background(), func(m *fenceModel) { m.failLoad = map[int]error{1: loadErr} }, loadErr},
		{"reading the fence failed", canceled, func(*fenceModel) {}, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := model()
			tc.fault(m)
			err := fenceModelProvider(m).RespondFenced(tc.ctx, fenceModelSession, testFence, answer)
			var miss *runtime.FenceMismatchError
			if !errors.Is(err, runtime.ErrInputNotSent) || !errors.Is(err, tc.cause) || errors.As(err, &miss) {
				t.Fatalf("RespondFenced = %v, want ErrInputNotSent wrapping %v", err, tc.cause)
			}
			if bare, events, _ := m.snapshot(); len(bare) != 0 || len(events) != 0 || m.guards != 0 {
				t.Fatalf("bare %q, events %q, guards %d; want nothing typed", bare, events, m.guards)
			}
		})
	}
}
