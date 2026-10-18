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
// offers is ErrInteractionResponseInvalid, and a pane that could not be read
// is ErrRuntimeUnavailable.
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
}
