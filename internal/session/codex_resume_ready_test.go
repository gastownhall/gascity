package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSubmitResumedCodexWaitsForCurrentRuntime(t *testing.T) {
	for _, waitErr := range []error{nil, context.DeadlineExceeded, context.Canceled, runtime.ErrInteractionUnsupported} {
		name := "ready"
		if waitErr != nil {
			name = waitErr.Error()
		}
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			mgr := NewManagerWithOptions(store, sp)
			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatal(err)
			}
			// The previous process was idle and its startup dialogs verified.
			// A stopped runtime must not inherit readiness from that process.
			if err := store.SetMetadata(info.ID, startupDialogVerifiedKey, "true"); err != nil {
				t.Fatal(err)
			}
			if err := sp.Stop(info.SessionName); err != nil {
				t.Fatal(err)
			}
			sp.WaitForIdleErrors[info.SessionName] = waitErr
			started, ready := make(chan struct{}), make(chan struct{})
			sp.WaitForIdleStarted[info.SessionName] = started
			sp.WaitForIdleGates[info.SessionName] = ready
			done := make(chan error, 1)
			go func() {
				_, err := mgr.Submit(context.Background(), info.ID, "complete input", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
				done <- err
			}()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("submit finished before checking resumed runtime readiness: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("readiness wait never started")
			}
			for _, call := range sp.SnapshotCalls() {
				if call.Method == "NudgeNow" || call.Method == "Nudge" {
					t.Fatalf("input sent before readiness: %#v", call)
				}
			}
			close(ready)
			select {
			case err := <-done:
				if !errors.Is(err, waitErr) {
					t.Fatalf("submit error = %v, want %v", err, waitErr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("submit did not finish")
			}
			nudges := 0
			for _, call := range sp.SnapshotCalls() {
				if call.Method == "NudgeNow" || call.Method == "Nudge" {
					nudges++
					if call.Message != "complete input" {
						t.Fatalf("wrong input: %q", call.Message)
					}
				}
			}
			want := 0
			if waitErr == nil {
				want = 1
			}
			if nudges != want {
				t.Fatalf("nudges = %d, want %d", nudges, want)
			}
		})
	}
}

func TestSubmitResumedCodexWithInferredACPDoesNotWaitForTerminal(t *testing.T) {
	for key, value := range map[string]string{
		MCPIdentityMetadataKey:        "test-mcp-identity",
		MCPServersSnapshotMetadataKey: `[{"name":"filesystem","transport":"stdio","command":"/bin/mcp"}]`,
	} {
		t.Run(key, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			mgr := NewManagerWithOptions(store, sp)
			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatal(err)
			}
			// Older ACP sessions may have MCP identity/snapshot evidence without
			// an explicit transport field. Resume already infers ACP from it.
			if err := store.SetMetadata(info.ID, "transport", ""); err != nil {
				t.Fatal(err)
			}
			if err := store.SetMetadata(info.ID, key, value); err != nil {
				t.Fatal(err)
			}
			if err := sp.Stop(info.SessionName); err != nil {
				t.Fatal(err)
			}
			_, err = mgr.Submit(context.Background(), info.ID, "ACP input", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault)
			if err != nil {
				t.Fatalf("inferred ACP resume: %v", err)
			}
			nudges := 0
			for _, call := range sp.SnapshotCalls() {
				if call.Method == "WaitForIdle" {
					t.Fatal("ACP session incorrectly required terminal readiness")
				}
				if (call.Method == "Nudge" || call.Method == "NudgeNow") && call.Message == "ACP input" {
					nudges++
				}
			}
			if nudges != 1 {
				t.Fatalf("delivered nudges = %d, want 1", nudges)
			}
		})
	}
}
