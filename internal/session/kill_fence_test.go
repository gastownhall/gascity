package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestKillPendingMetadata(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-10 * time.Second).Format(time.RFC3339)
	for _, tc := range []struct {
		name                                     string
		state, stateReason, sleepReason, sleptAt string
		want                                     bool
	}{
		{name: "fresh fence", state: "asleep", stateReason: KillPendingReason, sleepReason: "killed", sleptAt: fresh, want: true},
		{name: "padded values", state: " asleep ", stateReason: " kill-pending ", sleepReason: " killed ", sleptAt: " " + fresh + " ", want: true},
		{name: "small forward skew", state: "asleep", stateReason: KillPendingReason, sleepReason: "killed", sleptAt: now.Add(30 * time.Second).Format(time.RFC3339), want: true},
		{name: "aged past grace", state: "asleep", stateReason: KillPendingReason, sleepReason: "killed", sleptAt: now.Add(-KillPendingGrace).Format(time.RFC3339), want: false},
		{name: "far future", state: "asleep", stateReason: KillPendingReason, sleepReason: "killed", sleptAt: now.Add(KillPendingGrace).Format(time.RFC3339), want: false},
		{name: "unparseable stamp", state: "asleep", stateReason: KillPendingReason, sleepReason: "killed", sleptAt: "soon", want: false},
		{name: "missing stamp", state: "asleep", stateReason: KillPendingReason, sleepReason: "killed", want: false},
		{name: "fence lifted", state: "asleep", sleepReason: "killed", sleptAt: fresh, want: false},
		{name: "row moved on", state: "active", stateReason: KillPendingReason, sleepReason: "killed", sleptAt: fresh, want: false},
		{name: "other sleep reason", state: "asleep", stateReason: KillPendingReason, sleepReason: "idle", sleptAt: fresh, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := KillPendingMetadata(tc.state, tc.stateReason, tc.sleepReason, tc.sleptAt, now); got != tc.want {
				t.Fatalf("KillPendingMetadata = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestKillPendingPatchIsSleepPatchPlusFence(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	patch := KillPendingPatch(now)
	for key, want := range SleepPatch(now, "killed") {
		if key == "state_reason" {
			continue
		}
		if got := patch[key]; got != want {
			t.Errorf("patch[%s] = %q, want SleepPatch's %q", key, got, want)
		}
	}
	if got := patch["state_reason"]; got != KillPendingReason {
		t.Errorf("patch[state_reason] = %q, want %q", got, KillPendingReason)
	}
	info := Info{}.ApplyPatch(patch)
	if !IsKillPendingInfo(info, now) {
		t.Fatalf("IsKillPendingInfo(folded patch) = false, want true: %#v", info)
	}
}

// TestManagerRefusesKillFencedSession pins that the direct Manager entry
// points (attach, send) neither flip a kill-fenced row back to active while
// its runtime is dying nor deliver into that runtime.
func TestManagerRefusesKillFencedSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Manager, string) error
	}{
		{name: "attach", call: func(m *Manager, id string) error {
			return m.Attach(context.Background(), id, "claude --resume", runtime.Config{})
		}},
		{name: "send", call: func(m *Manager, id string) error {
			return m.Send(context.Background(), id, "hello", "claude --resume", runtime.Config{WorkDir: "/tmp"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			mgr := NewManagerWithOptions(store, sp)
			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			if !sp.IsRunning(info.SessionName) {
				t.Fatal("test premise: runtime should be running")
			}
			if err := store.SetMetadataBatch(info.ID, KillPendingPatch(time.Now())); err != nil {
				t.Fatalf("writing kill fence: %v", err)
			}
			callsBefore := len(sp.SnapshotCalls())

			if err := tc.call(mgr, info.ID); !errors.Is(err, ErrSessionKillPending) {
				t.Fatalf("err = %v, want ErrSessionKillPending", err)
			}
			b, err := store.Get(info.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got := b.Metadata["state"]; got != string(StateAsleep) {
				t.Errorf("state = %q, want asleep: the fence must survive", got)
			}
			if got := b.Metadata["state_reason"]; got != KillPendingReason {
				t.Errorf("state_reason = %q, want %q", got, KillPendingReason)
			}
			for _, call := range sp.SnapshotCalls()[callsBefore:] {
				switch call.Method {
				case "Start", "Nudge", "Attach", "SendKeys":
					t.Errorf("fenced session reached runtime effect %s", call.Method)
				}
			}
		})
	}
}
