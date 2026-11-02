package session

import (
	"context"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// idleSnapshotFake is a Fake whose one-look idle snapshot answers idle and
// err (runtime.IdleSnapshotProvider).
type idleSnapshotFake struct {
	*runtime.Fake
	idle bool
	err  error
}

func (f *idleSnapshotFake) SnapshotIdle(string) (bool, error) { return f.idle, f.err }

// Both wait-idle nudges take a pre-read first (runtime.IdleBoundaryNow): a
// session that reads busy, or reads indeterminate while its runtime reports a
// pending interaction (an open dialog), is reported undelivered at once, for
// its caller to queue, without entering the idle wait and without typing. A
// pre-read at the boundary, or one that cannot tell, waits for idle and
// delivers as before.
func TestChatWaitIdleBusySnapshotQueuesWithoutWaiting(t *testing.T) {
	indeterminate := fmt.Errorf("scan: %w", runtime.ErrIdleIndeterminate)
	for _, tc := range []struct {
		name      string
		idle      bool
		err       error
		pending   *runtime.PendingInteraction
		delivered bool
	}{
		{name: "busy"},
		{name: "pending", err: indeterminate, pending: &runtime.PendingInteraction{RequestID: "tmux-1", Kind: "approval", Prompt: "Allow Bash?"}},
		{name: "at the boundary", idle: true, delivered: true},
		{name: "indeterminate", err: indeterminate, delivered: true},
	} {
		for path, deliver := range map[string]func(*Manager, Info, runtime.Config) (bool, error){
			"TryWaitIdleNudge": func(m *Manager, i Info, h runtime.Config) (bool, error) {
				return m.TryWaitIdleNudge(context.Background(), testActor(m, ActorBackground), i.ID, "mail", "check deploy status", BuildResumeCommand(i), h)
			},
			"TryWaitIdleNudgeLiveOnly": func(m *Manager, i Info, h runtime.Config) (bool, error) {
				return m.TryWaitIdleNudgeLiveOnly(context.Background(), i.ID, "mail", "check deploy status", h.ProcessNames)
			},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				fake := runtime.NewFake()
				sp := &idleSnapshotFake{Fake: fake, idle: tc.idle, err: tc.err}
				mgr := NewManagerWithOptions(beads.NewMemStore(), sp, WithCityPath(t.TempDir()))
				info, err := mgr.CreateSession(context.Background(), CreateOptions{
					Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude",
					ExtraMeta: map[string]string{"session_origin": "manual"},
				})
				if err != nil {
					t.Fatalf("CreateSession: %v", err)
				}
				fake.WaitForIdleErrors[info.SessionName] = nil
				fake.SetPendingInteraction(info.SessionName, tc.pending)
				before := len(fake.SnapshotCalls())

				delivered, err := deliver(mgr, info, runtime.Config{WorkDir: info.WorkDir})
				if err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				if delivered != tc.delivered {
					t.Fatalf("%s delivered = %t, want %t", path, delivered, tc.delivered)
				}
				var waited, typed bool
				for _, call := range fake.SnapshotCalls()[before:] {
					if call.Name != info.SessionName {
						continue
					}
					switch call.Method {
					case "WaitForIdle":
						waited = true
					case "Nudge", "NudgeNow":
						typed = true
					}
				}
				if waited != tc.delivered {
					t.Fatalf("waited for idle = %t, want %t", waited, tc.delivered)
				}
				if typed != tc.delivered {
					t.Fatalf("typed = %t, want %t", typed, tc.delivered)
				}
			})
		}
	}
}
