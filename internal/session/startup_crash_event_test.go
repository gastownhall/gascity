package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

const startupCrashPaneSnippet = "Trust this folder? [y/n]"

func startupDeathErr(name string) error {
	return fmt.Errorf("%w: session %q; last pane output:\n%s", runtime.ErrSessionDiedDuringStartup, name, startupCrashPaneSnippet)
}

type paneStartupDeathProvider struct {
	*runtime.Fake
	armed bool
}

func (p *paneStartupDeathProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.armed {
		p.armed = false
		return startupDeathErr(name)
	}
	return p.Fake.Start(ctx, name, cfg)
}

func assertSessionCrashedEvent(t *testing.T, rec *events.Fake, sessionID, sessionName string) events.Event {
	t.Helper()
	var crashed []events.Event
	for _, e := range rec.Events {
		if e.Type == events.SessionCrashed {
			crashed = append(crashed, e)
		}
	}
	if len(crashed) != 1 {
		t.Fatalf("SessionCrashed events = %d, want 1 (total events %d)", len(crashed), len(rec.Events))
	}
	got := crashed[0]
	if got.Subject != sessionName {
		t.Errorf("Subject = %q, want session name %q", got.Subject, sessionName)
	}
	if sessionID == "" {
		if got.SessionID == "" {
			t.Error("SessionID is empty")
		}
	} else if got.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", got.SessionID, sessionID)
	}
	if !strings.Contains(got.Message, startupCrashPaneSnippet) {
		t.Errorf("Message %q does not contain pane snippet %q", got.Message, startupCrashPaneSnippet)
	}
	var payload struct {
		SessionID string `json:"session_id"`
		Template  string `json:"template"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	wantID := sessionID
	if wantID == "" {
		wantID = got.SessionID
	}
	if payload.SessionID != wantID {
		t.Errorf("payload.session_id = %q, want %q", payload.SessionID, wantID)
	}
	if payload.Reason != string(SleepReasonStartupCrash) {
		t.Errorf("payload.reason = %q, want %q", payload.Reason, SleepReasonStartupCrash)
	}
	return got
}

func TestCreateSessionEmitsSessionCrashedOnStartupDeath(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	sp.StartErrors["sky"] = startupDeathErr("sky")
	rec := events.NewFake()
	mgr := NewManagerWithOptions(store, sp, WithEventRecorder(rec))

	_, err := mgr.CreateSession(context.Background(), CreateOptions{
		ExplicitName: "sky",
		Template:     "helper",
		Title:        "first",
		Command:      "claude",
		WorkDir:      "/tmp",
		Provider:     "claude",
	})
	if err == nil {
		t.Fatal("expected startup death")
	}
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("error = %v, want ErrSessionDiedDuringStartup", err)
	}
	got := assertSessionCrashedEvent(t, rec, "", "sky")
	var payload struct {
		Template string `json:"template"`
	}
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Template != "helper" {
		t.Errorf("payload.template = %q, want helper", payload.Template)
	}
}

func TestCreateSessionStartupDeathDoesNotEmitWithoutRecorder(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	sp.StartErrors["sky"] = startupDeathErr("sky")
	mgr := NewManagerWithOptions(store, sp)

	_, err := mgr.CreateSession(context.Background(), CreateOptions{
		ExplicitName: "sky",
		Template:     "helper",
		Command:      "claude",
		WorkDir:      "/tmp",
		Provider:     "claude",
	})
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("error = %v, want ErrSessionDiedDuringStartup", err)
	}
}

func TestEnsureRunningEmitsSessionCrashedWhenStartupDeathIsTerminal(t *testing.T) {
	store := beads.NewMemStore()
	sp := &paneStartupDeathProvider{Fake: runtime.NewFake()}
	rec := events.NewFake()
	mgr := NewManagerWithOptions(store, sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter), WithEventRecorder(rec))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "worker",
		Command:  "claude --dangerously",
		WorkDir:  "/tmp",
		Provider: "claude",
		Resume: ProviderResume{
			ResumeFlag:    "--resume",
			SessionIDFlag: "--session-id",
		},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := store.SetMetadata(info.ID, "session_key", ""); err != nil {
		t.Fatalf("clearing session_key: %v", err)
	}

	sp.armed = true
	err = mgr.Send(context.Background(), info.ID, "hello", "claude --dangerously", runtime.Config{WorkDir: "/tmp"})
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("Send error = %v, want ErrSessionDiedDuringStartup", err)
	}
	assertSessionCrashedEvent(t, rec, info.ID, info.SessionName)

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := b.Metadata["sleep_reason"]; got != string(SleepReasonStartupCrash) {
		t.Errorf("sleep_reason = %q, want %q", got, SleepReasonStartupCrash)
	}
}

func TestStartRuntimeOnlyEmitsSessionCrashedWhenStartupDeathIsTerminal(t *testing.T) {
	store := beads.NewMemStore()
	sp := &paneStartupDeathProvider{Fake: runtime.NewFake()}
	rec := events.NewFake()
	mgr := NewManagerWithOptions(store, sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter), WithEventRecorder(rec))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "worker",
		Command:  "claude --dangerously",
		WorkDir:  "/tmp",
		Provider: "claude",
		Resume: ProviderResume{
			ResumeFlag:    "--resume",
			SessionIDFlag: "--session-id",
		},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := store.SetMetadata(info.ID, "session_key", ""); err != nil {
		t.Fatalf("clearing session_key: %v", err)
	}

	sp.armed = true
	err = mgr.StartRuntimeOnly(context.Background(), info.ID, "claude --dangerously", runtime.Config{WorkDir: "/tmp"})
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("StartRuntimeOnly error = %v, want ErrSessionDiedDuringStartup", err)
	}
	assertSessionCrashedEvent(t, rec, info.ID, info.SessionName)
}

func TestEnsureRunningDoesNotEmitSessionCrashedWhenStaleKeyRetrySucceeds(t *testing.T) {
	mgr, sp, info, sessionKey := newSuspendedResumableSession(t)
	rec := events.NewFake()
	mgr.rec = rec

	resumeCmd := "claude --dangerously --resume " + sessionKey
	sp.armed = true
	if err := mgr.Send(context.Background(), info.ID, "hello", resumeCmd, runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("Send should recover: %v", err)
	}
	for _, e := range rec.Events {
		if e.Type == events.SessionCrashed {
			t.Fatalf("recovered stale-key death must not emit SessionCrashed, got %+v", e)
		}
	}
}
