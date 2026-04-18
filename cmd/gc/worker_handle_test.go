package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func TestWorkerHandleForSessionWithConfigUsesResolvedProviderOnFirstStart(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"
resume_flag = "--resume"
resume_style = "flag"
session_id_flag = "--session-id"
ready_prompt_prefix = "stub-ready>"
ready_delay_ms = 250

[providers.stub.env]
STUB_ENV = "present"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig(cityDir, store, sp, cfg)
	info, err := mgr.CreateBeadOnly("worker", "Probe", "", t.TempDir(), "stub", "", nil, session.ProviderResume{
		SessionIDFlag: "--old-session-id",
	})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	if strings.TrimSpace(info.SessionKey) == "" {
		t.Fatal("SessionKey is empty")
	}

	handle, err := workerHandleForSessionWithConfig(cityDir, store, sp, cfg, info.ID)
	if err != nil {
		t.Fatalf("workerHandleForSessionWithConfig: %v", err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("handle.Start: %v", err)
	}

	start := sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatalf("LastStartConfig(%q) = nil", info.SessionName)
	}
	wantArg := "--session-id " + info.SessionKey
	if !strings.Contains(start.Command, "/bin/echo") || !strings.Contains(start.Command, wantArg) {
		t.Fatalf("start command = %q, want /bin/echo with %q", start.Command, wantArg)
	}
	if strings.Contains(start.Command, "--old-session-id") {
		t.Fatalf("start command = %q, still used stale session id flag", start.Command)
	}
	if start.ReadyPromptPrefix != "stub-ready>" {
		t.Fatalf("ReadyPromptPrefix = %q, want stub-ready>", start.ReadyPromptPrefix)
	}
	if start.ReadyDelayMs != 250 {
		t.Fatalf("ReadyDelayMs = %d, want 250", start.ReadyDelayMs)
	}
	if start.Env["STUB_ENV"] != "present" {
		t.Fatalf("Env[STUB_ENV] = %q, want present", start.Env["STUB_ENV"])
	}
}

func TestWorkerHandleForSessionWithConfigUsesResolvedProviderOnResume(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"
resume_flag = "--resume"
resume_style = "flag"
session_id_flag = "--session-id"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig(cityDir, store, sp, cfg)
	info, err := mgr.Create(
		context.Background(),
		"worker",
		"Probe",
		"legacy-agent",
		t.TempDir(),
		"stub",
		nil,
		session.ProviderResume{
			ResumeFlag:    "--old-resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
		runtime.Config{},
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	handle, err := workerHandleForSessionWithConfig(cityDir, store, sp, cfg, info.ID)
	if err != nil {
		t.Fatalf("workerHandleForSessionWithConfig: %v", err)
	}

	sp.Calls = nil
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("handle.Start: %v", err)
	}

	start := sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatalf("LastStartConfig(%q) = nil", info.SessionName)
	}
	wantArg := "--resume " + info.SessionKey
	if !strings.Contains(start.Command, "/bin/echo") || !strings.Contains(start.Command, wantArg) {
		t.Fatalf("start command = %q, want /bin/echo with %q", start.Command, wantArg)
	}
	if strings.Contains(start.Command, "--old-resume") {
		t.Fatalf("start command = %q, still used stale resume flag", start.Command)
	}
}

func TestWorkerHandleForSessionTargetWithConfigResolvesSessionName(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"
resume_flag = "--resume"
resume_style = "flag"
session_id_flag = "--session-id"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig(cityDir, store, sp, cfg)
	info, err := mgr.Create(
		context.Background(),
		"worker",
		"Probe",
		"",
		t.TempDir(),
		"stub",
		nil,
		session.ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id"},
		runtime.Config{},
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	handle, err := workerHandleForSessionTargetWithConfig(cityDir, store, sp, cfg, info.SessionName)
	if err != nil {
		t.Fatalf("workerHandleForSessionTargetWithConfig: %v", err)
	}
	if err := handle.Kill(context.Background()); err != nil {
		t.Fatalf("handle.Kill: %v", err)
	}
	if stop := sp.Calls[len(sp.Calls)-1]; stop.Method != "Stop" || stop.Name != info.SessionName {
		t.Fatalf("last runtime call = %#v, want Stop %q", stop, info.SessionName)
	}
}

func TestWorkerKillSessionTargetWithConfigResolvesRuntimeSessionMeta(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig(cityDir, store, sp, cfg)
	info, err := mgr.Create(context.Background(), "worker", "Probe", "stub", t.TempDir(), "stub", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := workerKillSessionTargetWithConfig(cityDir, store, sp, cfg, info.SessionName); err != nil {
		t.Fatalf("workerKillSessionTargetWithConfig: %v", err)
	}
	last := sp.Calls[len(sp.Calls)-1]
	if last.Method != "Stop" || last.Name != info.SessionName {
		t.Fatalf("last runtime call = %#v, want Stop %q", last, info.SessionName)
	}
}

func TestWorkerDeliveryIntentForSubmitIntent(t *testing.T) {
	tests := []struct {
		name   string
		intent session.SubmitIntent
		want   worker.DeliveryIntent
	}{
		{name: "default", intent: session.SubmitIntentDefault, want: worker.DeliveryIntentDefault},
		{name: "follow up", intent: session.SubmitIntentFollowUp, want: worker.DeliveryIntentFollowUp},
		{name: "interrupt now", intent: session.SubmitIntentInterruptNow, want: worker.DeliveryIntentInterruptNow},
		{name: "empty defaults", intent: "", want: worker.DeliveryIntentDefault},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := workerDeliveryIntentForSubmitIntent(tt.intent); got != tt.want {
				t.Fatalf("workerDeliveryIntentForSubmitIntent(%q) = %q, want %q", tt.intent, got, tt.want)
			}
		})
	}
}

func TestWorkerNudgeDeliveryForMode(t *testing.T) {
	tests := []struct {
		name string
		mode nudgeDeliveryMode
		want worker.NudgeDelivery
		ok   bool
	}{
		{name: "immediate", mode: nudgeDeliveryImmediate, want: worker.NudgeDeliveryImmediate, ok: true},
		{name: "wait idle", mode: nudgeDeliveryWaitIdle, want: worker.NudgeDeliveryWaitIdle, ok: true},
		{name: "queue", mode: nudgeDeliveryQueue, want: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := workerNudgeDeliveryForMode(tt.mode)
			if ok != tt.ok {
				t.Fatalf("workerNudgeDeliveryForMode(%q) ok = %v, want %v", tt.mode, ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("workerNudgeDeliveryForMode(%q) = %q, want %q", tt.mode, got, tt.want)
			}
		})
	}
}
