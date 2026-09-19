package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

type suspendExitObserver struct {
	*runtime.Fake
	beforeStop func()
	stopErr    error
}

func (p *suspendExitObserver) Stop(name string) error {
	if p.beforeStop != nil {
		p.beforeStop()
	}
	if p.stopErr != nil {
		return p.stopErr
	}
	return p.Fake.Stop(name)
}

func TestSuspendPreservesConversationAcrossExitClassification(t *testing.T) {
	for _, elapsed := range []time.Duration{time.Second, time.Minute} {
		t.Run(elapsed.String(), func(t *testing.T) {
			store := beads.NewMemStore()
			sp := &suspendExitObserver{Fake: runtime.NewFake()}
			mgr := NewManagerWithOptions(store, sp)
			now := time.Now().UTC()
			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetMetadataBatch(info.ID, map[string]string{"session_key": "keep-provider-conversation", "last_woke_at": now.Add(-elapsed).Format(time.RFC3339)}); err != nil {
				t.Fatal(err)
			}
			check := func() {
				b, err := store.Get(info.ID)
				if err != nil {
					t.Fatal(err)
				}
				outcome := DecideSessionExit(ExitFacts{LastWokeAt: b.Metadata["last_woke_at"], Now: now, StabilityThreshold: 10 * time.Second, ProductivityThreshold: 5 * time.Minute})
				if outcome != ExitNone {
					t.Errorf("intentional suspend classified as %v", outcome)
				}
				if b.Metadata["session_key"] != "keep-provider-conversation" {
					t.Error("suspend changed provider conversation")
				}
			}
			sp.beforeStop = check
			if err := mgr.Suspend(info.ID); err != nil {
				t.Fatal(err)
			}
			check()
		})
	}
}

func TestSuspendRestoresExitTrackingWhenLiveStopFails(t *testing.T) {
	store := beads.NewMemStore()
	sp := &suspendExitObserver{Fake: runtime.NewFake()}
	mgr := NewManagerWithOptions(store, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	woke := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if err := store.SetMetadata(info.ID, "last_woke_at", woke); err != nil {
		t.Fatal(err)
	}
	sp.stopErr = errors.New("cannot stop live process")
	if err := mgr.Suspend(info.ID); !errors.Is(err, sp.stopErr) {
		t.Fatalf("Suspend error = %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata["last_woke_at"] != woke {
		t.Errorf("failed stop lost wake stamp: %q", b.Metadata["last_woke_at"])
	}
	if State(b.Metadata["state"]) != StateActive {
		t.Errorf("failed stop state = %q", b.Metadata["state"])
	}
}

func TestSuspendDoesNotStopWhenExitTrackingCannotBeSaved(t *testing.T) {
	store := beads.NewMemStore()
	sp := &suspendExitObserver{Fake: runtime.NewFake()}
	mgr := NewManagerWithOptions(store, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(info.ID, "last_woke_at", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	mgr.store = failMetadataKeyStore{MemStore: store, key: "last_woke_at"}
	stopped := false
	sp.beforeStop = func() { stopped = true }
	if err := mgr.Suspend(info.ID); err == nil {
		t.Fatal("expected persistence error")
	}
	if stopped {
		t.Error("stopped runtime without saving deliberate-stop intent")
	}
	if !sp.IsRunning(info.SessionName) {
		t.Error("persistence failure stopped runtime")
	}
}
