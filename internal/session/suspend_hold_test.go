package session

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func suspendHoldFixture(t *testing.T) (*Manager, *beads.MemStore, *runtime.Fake, Info) {
	t.Helper()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp, WithCityPath(t.TempDir()), WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", Provider: "claude", WorkDir: t.TempDir(), ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(info.ID, "session_key", "retained-conversation"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatal(err)
	}
	held, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	until, err := time.Parse(time.RFC3339, held.HeldUntil)
	if err != nil || !until.After(time.Now().Add(99*365*24*time.Hour)) || held.SleepIntent != "user-hold" {
		t.Fatalf("suspend did not persist durable hold: held=%q intent=%q", held.HeldUntil, held.SleepIntent)
	}
	return mgr, store, sp, info
}

func TestExplicitResumeReleasesSuspensionHold(t *testing.T) {
	for _, action := range []string{"start", "attach", "send", "submit", "interrupt", "follow-up"} {
		t.Run(action, func(t *testing.T) {
			mgr, _, sp, info := suspendHoldFixture(t)
			ctx := context.Background()
			cmd := "claude --resume retained-conversation"
			var err error
			switch action {
			case "start":
				err = mgr.Start(ctx, info.ID, cmd, runtime.Config{})
			case "attach":
				err = mgr.Attach(ctx, info.ID, cmd, runtime.Config{})
			case "send":
				err = mgr.Send(ctx, info.ID, "request", cmd, runtime.Config{})
			default:
				intent := SubmitIntentDefault
				if action == "interrupt" {
					intent = SubmitIntentInterruptNow
				}
				if action == "follow-up" {
					intent = SubmitIntentFollowUp
				}
				_, err = mgr.Submit(ctx, info.ID, "request", cmd, runtime.Config{}, intent)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := mgr.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !sp.IsRunning(info.SessionName) || got.SessionKey != "retained-conversation" || got.HeldUntil != "" || got.SleepIntent != "" || got.SleepReason != "" {
				t.Fatalf("successful explicit resume retained suspend blockers: key=%q held=%q intent=%q reason=%q", got.SessionKey, got.HeldUntil, got.SleepIntent, got.SleepReason)
			}
		})
	}
}

func TestUnsuccessfulResumePreservesSuspensionHold(t *testing.T) {
	for _, action := range []string{"start-failure", "resume-required", "live-only", "runtime-only"} {
		t.Run(action, func(t *testing.T) {
			mgr, store, sp, info := suspendHoldFixture(t)
			before, err := store.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			cmd := "claude --resume retained-conversation"
			switch action {
			case "start-failure":
				sp.StartErrors[info.SessionName] = errors.New("injected start failure")
				err = mgr.Start(ctx, info.ID, cmd, runtime.Config{})
				if err == nil {
					t.Fatal("missing start failure")
				}
			case "resume-required":
				err = mgr.Start(ctx, info.ID, "", runtime.Config{})
				if !errors.Is(err, ErrResumeRequired) {
					t.Fatalf("want resume required: %v", err)
				}
			case "live-only":
				var sent bool
				sent, err = mgr.SendLiveOnly(ctx, info.ID, "request")
				if sent || err != nil {
					t.Fatalf("live-only unexpectedly sent: %v %v", sent, err)
				}
			case "runtime-only":
				if err = mgr.StartRuntimeOnly(ctx, info.ID, cmd, runtime.Config{}); err != nil {
					t.Fatal(err)
				}
			}
			after, err := store.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"held_until", "sleep_intent", "sleep_reason", "session_key"} {
				if !reflect.DeepEqual(before.Metadata[key], after.Metadata[key]) {
					t.Fatalf("%s changed %s", action, key)
				}
			}
		})
	}
}

func TestExplicitStartPreservesHeartbeatHold(t *testing.T) {
	mgr, store, _, info := suspendHoldFixture(t)
	// A heartbeat has a timed hold without the explicit suspension intent.
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := store.SetMetadataBatch(info.ID, map[string]string{"held_until": until, "sleep_intent": "", "sleep_reason": "", "state": "asleep"}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(context.Background(), info.ID, "claude --resume retained-conversation", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeldUntil != until {
		t.Fatal("explicit start cleared unrelated heartbeat hold")
	}
}

func TestRepeatedDurableSuspendIsIdempotent(t *testing.T) {
	mgr, store, sp, info := suspendHoldFixture(t)
	before, err := store.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	stops := sp.CountCalls("Stop", info.SessionName)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatal(err)
	}
	after, err := store.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Metadata, after.Metadata) || sp.CountCalls("Stop", info.SessionName) != stops {
		t.Fatal("repeated durable suspend mutated state/runtime")
	}
}
