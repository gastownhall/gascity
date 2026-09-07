package tmux

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestLaunchStopsAtUnrecognizedWorkspaceTrust(t *testing.T) {
	for _, pass := range []int{1, 2} {
		t.Run(fmt.Sprintf("pass%d", pass), func(t *testing.T) {
			ops := &fakeStartOps{hasSessionResult: true}
			observed := 0
			ops.acceptStartupDialogsHook = func() {
				observed++
				if observed == pass {
					ops.acceptStartupDialogsErr = fmt.Errorf("workspace trust dialog: %w", runtime.ErrUnrecognizedWorkspaceTrust)
				}
			}
			cfg := runtime.Config{Command: "claude", ProcessNames: []string{"claude"}, ReadyPromptPrefix: "❯", Nudge: "startup work"}
			err := doStartSession(context.Background(), ops, "owned-test", cfg, DefaultConfig().SetupTimeout)
			if !errors.Is(err, runtime.ErrUnrecognizedWorkspaceTrust) {
				t.Fatalf("error=%v, want unsafe trust error", err)
			}
			readyCalls := 0
			for _, call := range ops.calls {
				if call.method == "sendKeys" || call.method == "runSetupCommand" {
					t.Fatalf("launch continued after unsafe trust menu: %+v", call)
				}
				if call.method == "waitForReady" {
					readyCalls++
				}
			}
			if readyCalls != pass-1 || observed != pass {
				t.Fatalf("ready calls=%d dialog calls=%d, want %d and %d", readyCalls, observed, pass-1, pass)
			}
		})
	}
}

func TestLaunchKeepsOtherDialogErrorsBestEffort(t *testing.T) {
	ops := &fakeStartOps{hasSessionResult: true, acceptStartupDialogsErr: errors.New("temporary capture failure")}
	cfg := runtime.Config{Command: "claude", ProcessNames: []string{"claude"}, ReadyPromptPrefix: "❯", Nudge: "startup work"}
	if err := doStartSession(context.Background(), ops, "owned-test", cfg, DefaultConfig().SetupTimeout); err != nil {
		t.Fatal(err)
	}
	sent := false
	for _, call := range ops.calls {
		if call.method == "sendKeys" {
			sent = true
		}
	}
	if !sent {
		t.Fatal("unrelated best-effort dialog error suppressed startup input")
	}
}
