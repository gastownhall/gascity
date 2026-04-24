package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type failingSessionLookupStore struct {
	beads.Store
	err error
}

func (s *failingSessionLookupStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, s.err
}

func (s *failingSessionLookupStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, s.err
}

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

func TestResolvedWorkerRuntimeWithConfigUsesProviderLaunchCommand(t *testing.T) {
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcDir, "settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	claude := config.BuiltinProviders()["claude"]
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "claude",
		}},
		Providers: map[string]config.ProviderSpec{
			"claude": claude,
		},
	}

	resolved := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "worker",
		WorkDir:  cityDir,
	}, "")
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if !strings.Contains(resolved.Command, "--dangerously-skip-permissions") {
		t.Fatalf("Command = %q, want unrestricted default", resolved.Command)
	}
	if !strings.Contains(resolved.Command, "--effort max") {
		t.Fatalf("Command = %q, want effort max default", resolved.Command)
	}
	if !strings.Contains(resolved.Command, "--settings") {
		t.Fatalf("Command = %q, want settings arg", resolved.Command)
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

func TestWorkerObserveSessionTargetWithConfigFallsBackToRunningRuntimeHandle(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{Command: "echo"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
		},
	}

	target := cliSessionName("/home/user/city", cfg.Workspace.Name, "mayor", cfg.Workspace.SessionTemplate)
	obs, err := workerObserveSessionTargetWithConfig("/home/user/city", nil, sp, cfg, target)
	if err != nil {
		t.Fatalf("workerObserveSessionTargetWithConfig: %v", err)
	}
	if !obs.Running {
		t.Fatalf("obs.Running = false, want true for %q", target)
	}
}

func TestWorkerObserveSessionTargetWithConfigIgnoresStoreLookupFailuresForRuntimeFallback(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{Command: "echo"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
		},
	}

	target := cliSessionName("/home/user/city", cfg.Workspace.Name, "mayor", cfg.Workspace.SessionTemplate)
	store := &failingSessionLookupStore{err: fmt.Errorf("store lookup failed")}
	obs, err := workerObserveSessionTargetWithConfig("/home/user/city", store, sp, cfg, target)
	if err != nil {
		t.Fatalf("workerObserveSessionTargetWithConfig: %v", err)
	}
	if !obs.Running {
		t.Fatalf("obs.Running = false, want true for %q when runtime session is live", target)
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

func TestResolvedWorkerSessionConfigWithConfigFallsBackToResolvedProviderNameForCommand(t *testing.T) {
	cfg, err := resolvedWorkerSessionConfigWithConfig(
		"",
		"",
		"/tmp/work",
		"worker",
		"",
		"worker",
		"Worker",
		"",
		&config.ResolvedProvider{
			Name: "custom-provider",
		},
		map[string]string{"session_origin": "test"},
	)
	if err != nil {
		t.Fatalf("resolvedWorkerSessionConfigWithConfig: %v", err)
	}
	if got, want := cfg.Runtime.Command, "custom-provider"; got != want {
		t.Fatalf("Runtime.Command = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.Provider, "custom-provider"; got != want {
		t.Fatalf("Runtime.Provider = %q, want %q", got, want)
	}
}

func TestResolvedWorkerSessionConfigWithConfigFallsBackToProviderArgForCommand(t *testing.T) {
	cfg, err := resolvedWorkerSessionConfigWithConfig(
		"",
		"legacy-provider",
		"/tmp/work",
		"worker",
		"",
		"worker",
		"Worker",
		"",
		&config.ResolvedProvider{},
		nil,
	)
	if err != nil {
		t.Fatalf("resolvedWorkerSessionConfigWithConfig: %v", err)
	}
	if got, want := cfg.Runtime.Command, "legacy-provider"; got != want {
		t.Fatalf("Runtime.Command = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.Provider, "legacy-provider"; got != want {
		t.Fatalf("Runtime.Provider = %q, want %q", got, want)
	}
}

func TestResolvedWorkerRuntimeWithConfigFallsBackToCityPathAndSyncsHintsWorkDir(t *testing.T) {
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
ready_prompt_prefix = "stub-ready>"
ready_delay_ms = 250
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	runtimeCfg := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "worker",
	}, "")
	if runtimeCfg == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if got, want := runtimeCfg.WorkDir, cityDir; got != want {
		t.Fatalf("WorkDir = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.WorkDir, cityDir; got != want {
		t.Fatalf("Hints.WorkDir = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Provider, "stub"; got != want {
		t.Fatalf("Provider = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Command, "/bin/echo"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}

func TestWorkerSessionRuntimeResolverWithConfigFallsBackToProviderNameWhenResolvedCommandMissing(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "stub",
		}},
		Providers: map[string]config.ProviderSpec{
			"stub": {},
		},
	}

	resolver := workerSessionRuntimeResolverWithConfig(t.TempDir(), cfg)
	if resolver == nil {
		t.Fatal("workerSessionRuntimeResolverWithConfig() = nil")
	}

	runtimeCfg, err := resolver(session.Info{Template: "worker"}, "")
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolver() = nil")
	}
	if got, want := runtimeCfg.Command, "stub"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Provider, "stub"; got != want {
		t.Fatalf("Provider = %q, want %q", got, want)
	}
}

func TestWorkerSessionRuntimeResolverWithConfigFallsBackToPersistedRuntimeOnIncompleteResolvedConfig(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "stub",
		}},
		Providers: map[string]config.ProviderSpec{
			"stub": {
				ReadyPromptPrefix: "resolved-ready>",
				ReadyDelayMs:      321,
			},
		},
	}

	resolver := workerSessionRuntimeResolverWithConfig(t.TempDir(), cfg)
	if resolver == nil {
		t.Fatal("workerSessionRuntimeResolverWithConfig() = nil")
	}

	info := session.Info{
		Template:      "worker",
		Command:       "persisted-worker --dangerously-skip-permissions",
		Provider:      "persisted-provider",
		WorkDir:       "/tmp/persisted-workdir",
		ResumeFlag:    "--resume-persisted",
		ResumeStyle:   "subcommand",
		ResumeCommand: "persisted resume {{.SessionKey}}",
	}

	runtimeCfg, err := resolver(info, "")
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolver() = nil")
	}
	if got, want := runtimeCfg.Command, info.Command; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Provider, info.Provider; got != want {
		t.Fatalf("Provider = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.WorkDir, info.WorkDir; got != want {
		t.Fatalf("WorkDir = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeFlag, info.ResumeFlag; got != want {
		t.Fatalf("Resume.ResumeFlag = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeStyle, info.ResumeStyle; got != want {
		t.Fatalf("Resume.ResumeStyle = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeCommand, info.ResumeCommand; got != want {
		t.Fatalf("Resume.ResumeCommand = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.WorkDir, info.WorkDir; got != want {
		t.Fatalf("Hints.WorkDir = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.ReadyPromptPrefix, "resolved-ready>"; got != want {
		t.Fatalf("Hints.ReadyPromptPrefix = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.ReadyDelayMs, 321; got != want {
		t.Fatalf("Hints.ReadyDelayMs = %d, want %d", got, want)
	}
}

func TestWorkerSessionRuntimeResolverWithConfigFallsBackToPersistedProviderWhenCommandMissing(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "resolved-provider",
		}},
		Providers: map[string]config.ProviderSpec{
			"resolved-provider": {
				ReadyPromptPrefix: "resolved-ready>",
			},
		},
	}

	resolver := workerSessionRuntimeResolverWithConfig(t.TempDir(), cfg)
	if resolver == nil {
		t.Fatal("workerSessionRuntimeResolverWithConfig() = nil")
	}

	info := session.Info{
		Template: "worker",
		Provider: "persisted-provider",
	}

	runtimeCfg, err := resolver(info, "")
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolver() = nil")
	}
	if got, want := runtimeCfg.Command, info.Provider; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Provider, info.Provider; got != want {
		t.Fatalf("Provider = %q, want %q", got, want)
	}
}
