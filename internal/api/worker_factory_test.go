package api

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func TestResolveWorkerSessionRuntimePreservesStoredResolvedCommandAndBackfillsCurrentResumeSettings(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "resolved-worker"
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName:       "Resolved Worker",
		Command:           "/bin/echo",
		ReadyPromptPrefix: "resolved-ready>",
		ReadyDelayMs:      321,
		ResumeFlag:        "--resume-resolved",
		ResumeStyle:       "flag",
		ResumeCommand:     "resolved resume {{.SessionKey}}",
		SessionIDFlag:     "--session-id-resolved",
	}

	srv := New(fs)
	info := session.Info{
		ID:            "sess-1",
		Template:      "myrig/worker",
		Command:       "/bin/echo --composed",
		Provider:      "persisted-provider",
		WorkDir:       t.TempDir(),
		ResumeFlag:    "--resume-persisted",
		ResumeStyle:   "subcommand",
		ResumeCommand: "persisted resume {{.SessionKey}}",
	}

	runtimeCfg, err := srv.resolveWorkerSessionRuntime(info, "")
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntime: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntime() = nil")
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
	if got, want := runtimeCfg.Resume.ResumeFlag, "--resume-resolved"; got != want {
		t.Fatalf("Resume.ResumeFlag = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeStyle, "flag"; got != want {
		t.Fatalf("Resume.ResumeStyle = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeCommand, "resolved resume {{.SessionKey}}"; got != want {
		t.Fatalf("Resume.ResumeCommand = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.SessionIDFlag, "--session-id-resolved"; got != want {
		t.Fatalf("Resume.SessionIDFlag = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.ReadyPromptPrefix, "resolved-ready>"; got != want {
		t.Fatalf("Hints.ReadyPromptPrefix = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.ReadyDelayMs, 321; got != want {
		t.Fatalf("Hints.ReadyDelayMs = %d, want %d", got, want)
	}
}

func TestResolveWorkerSessionRuntimeUsesResolvedCommandWhenPersistedCommandIsStale(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "resolved-worker"
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName:       "Resolved Worker",
		Command:           "/bin/echo",
		ReadyPromptPrefix: "resolved-ready>",
		ReadyDelayMs:      321,
		ResumeFlag:        "--resume-resolved",
		ResumeStyle:       "flag",
		ResumeCommand:     "resolved resume {{.SessionKey}}",
		SessionIDFlag:     "--session-id-resolved",
	}

	srv := New(fs)
	info := session.Info{
		ID:            "sess-1",
		Template:      "myrig/worker",
		Command:       "legacy-agent --dangerously-skip-permissions",
		Provider:      "persisted-provider",
		WorkDir:       t.TempDir(),
		ResumeFlag:    "--resume-persisted",
		ResumeStyle:   "subcommand",
		ResumeCommand: "persisted resume {{.SessionKey}}",
	}

	runtimeCfg, err := srv.resolveWorkerSessionRuntime(info, "")
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntime: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntime() = nil")
	}
	if got, want := runtimeCfg.Command, "/bin/echo"; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Provider, info.Provider; got != want {
		t.Fatalf("Provider = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.WorkDir, info.WorkDir; got != want {
		t.Fatalf("WorkDir = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeFlag, "--resume-resolved"; got != want {
		t.Fatalf("Resume.ResumeFlag = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeStyle, "flag"; got != want {
		t.Fatalf("Resume.ResumeStyle = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.ResumeCommand, "resolved resume {{.SessionKey}}"; got != want {
		t.Fatalf("Resume.ResumeCommand = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Resume.SessionIDFlag, "--session-id-resolved"; got != want {
		t.Fatalf("Resume.SessionIDFlag = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.ReadyPromptPrefix, "resolved-ready>"; got != want {
		t.Fatalf("Hints.ReadyPromptPrefix = %q, want %q", got, want)
	}
	if got, want := runtimeCfg.Hints.ReadyDelayMs, 321; got != want {
		t.Fatalf("Hints.ReadyDelayMs = %d, want %d", got, want)
	}
}

func TestWorkerFactorySessionByIDUsesResolvedTemplateRuntime(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "resolved-worker"
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName:       "Resolved Worker",
		Command:           "/bin/echo",
		ReadyPromptPrefix: "resolved-ready>",
		ReadyDelayMs:      321,
		ResumeFlag:        "--resume-resolved",
		ResumeStyle:       "flag",
		SessionIDFlag:     "--session-id-resolved",
	}

	srv := New(fs)
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateBeadOnly(
		"myrig/worker",
		"Chat",
		"",
		t.TempDir(),
		"",
		"",
		nil,
		session.ProviderResume{SessionIDFlag: "--stale-session-id"},
	)
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}

	factory, err := srv.workerFactory(fs.cityBeadStore)
	if err != nil {
		t.Fatalf("workerFactory: %v", err)
	}
	handle, err := factory.SessionByID(info.ID)
	if err != nil {
		t.Fatalf("SessionByID(%q): %v", info.ID, err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := fs.sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil")
	}
	if got, want := start.Command, "/bin/echo --session-id-resolved "+info.SessionKey; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
	if got, want := start.ReadyPromptPrefix, "resolved-ready>"; got != want {
		t.Fatalf("ReadyPromptPrefix = %q, want %q", got, want)
	}
	if got, want := start.ReadyDelayMs, 321; got != want {
		t.Fatalf("ReadyDelayMs = %d, want %d", got, want)
	}
}

func TestWorkerFactorySessionByIDPreservesStoredResolvedCommand(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "resolved-worker"
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName:   "Resolved Worker",
		Command:       "/bin/echo",
		SessionIDFlag: "--session-id-resolved",
	}

	srv := New(fs)
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateBeadOnly(
		"myrig/worker",
		"Chat",
		"/bin/echo --composed",
		t.TempDir(),
		"resolved-worker",
		"",
		nil,
		session.ProviderResume{SessionIDFlag: "--stale-session-id"},
	)
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}

	factory, err := srv.workerFactory(fs.cityBeadStore)
	if err != nil {
		t.Fatalf("workerFactory: %v", err)
	}
	handle, err := factory.SessionByID(info.ID)
	if err != nil {
		t.Fatalf("SessionByID(%q): %v", info.ID, err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := fs.sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil")
	}
	if got, want := start.Command, "/bin/echo --composed --session-id-resolved "+info.SessionKey; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
}

func TestWorkerFactorySessionByIDUsesResolvedCommandAndResumeSettingsOnResume(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "resolved-worker"
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName:   "Resolved Worker",
		Command:       "/bin/echo",
		ResumeFlag:    "--resume-resolved",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id-resolved",
	}

	srv := New(fs)
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.Create(
		context.Background(),
		"myrig/worker",
		"Chat",
		"legacy-agent",
		t.TempDir(),
		"resolved-worker",
		nil,
		session.ProviderResume{
			ResumeFlag:    "--old-resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id-resolved",
		},
		runtime.Config{},
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	factory, err := srv.workerFactory(fs.cityBeadStore)
	if err != nil {
		t.Fatalf("workerFactory: %v", err)
	}
	handle, err := factory.SessionByID(info.ID)
	if err != nil {
		t.Fatalf("SessionByID(%q): %v", info.ID, err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := fs.sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil")
	}
	if got, want := start.Command, "/bin/echo --resume-resolved "+info.SessionKey; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
}

func TestWorkerFactoryHandleForTargetUsesResolvedTemplateRuntimeForSessionMeta(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "resolved-worker"
	fs.cfg.Providers["resolved-worker"] = config.ProviderSpec{
		DisplayName:       "Resolved Worker",
		Command:           "/bin/echo",
		ReadyPromptPrefix: "resolved-ready>",
		ReadyDelayMs:      321,
		ResumeFlag:        "--resume-resolved",
		ResumeStyle:       "flag",
		SessionIDFlag:     "--session-id-resolved",
	}

	srv := New(fs)
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateBeadOnly(
		"myrig/worker",
		"Chat",
		"",
		t.TempDir(),
		"",
		"",
		nil,
		session.ProviderResume{SessionIDFlag: "--stale-session-id"},
	)
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	if err := fs.sp.SetMeta("legacy-runtime-name", "GC_SESSION_ID", info.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	factory, err := srv.workerFactory(fs.cityBeadStore)
	if err != nil {
		t.Fatalf("workerFactory: %v", err)
	}
	handle, err := factory.HandleForTarget("legacy-runtime-name", nil)
	if err != nil {
		t.Fatalf("HandleForTarget: %v", err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := fs.sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil")
	}
	if got, want := start.Command, "/bin/echo --session-id-resolved "+info.SessionKey; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
	if got, want := start.ReadyPromptPrefix, "resolved-ready>"; got != want {
		t.Fatalf("ReadyPromptPrefix = %q, want %q", got, want)
	}
	if got, want := start.ReadyDelayMs, 321; got != want {
		t.Fatalf("ReadyDelayMs = %d, want %d", got, want)
	}
}

func TestNewResolvedWorkerSessionHandleStartsResolvedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	handle, err := srv.newResolvedWorkerSessionHandle(fs.cityBeadStore, worker.ResolvedSessionConfig{
		Alias:        "worker",
		ExplicitName: "worker-named",
		Template:     "myrig/worker",
		Title:        "Worker Named",
		Transport:    "acp",
		Metadata:     map[string]string{"session_origin": "named"},
		Runtime: worker.ResolvedRuntime{
			Command:    "/bin/echo",
			WorkDir:    t.TempDir(),
			Provider:   "resolved-worker",
			SessionEnv: map[string]string{"API_RESOLVED_ENV": "present"},
			Resume: session.ProviderResume{
				SessionIDFlag: "--session-id-resolved",
			},
			Hints: runtime.Config{
				ReadyPromptPrefix: "resolved-ready>",
				ReadyDelayMs:      321,
			},
		},
	})
	if err != nil {
		t.Fatalf("newResolvedWorkerSessionHandle: %v", err)
	}

	info, err := handle.Create(context.Background(), worker.CreateModeStarted)
	if err != nil {
		t.Fatalf("Create(started): %v", err)
	}

	start := fs.sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil")
	}
	if got, want := start.Command, "/bin/echo --session-id-resolved "+info.SessionKey; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
	if got, want := start.ReadyPromptPrefix, "resolved-ready>"; got != want {
		t.Fatalf("ReadyPromptPrefix = %q, want %q", got, want)
	}
	if got, want := start.ReadyDelayMs, 321; got != want {
		t.Fatalf("ReadyDelayMs = %d, want %d", got, want)
	}
	if got, want := start.Env["API_RESOLVED_ENV"], "present"; got != want {
		t.Fatalf("Env[API_RESOLVED_ENV] = %q, want %q", got, want)
	}
}

func TestNewResolvedWorkerSessionHandleDerivesProviderFromCommand(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	handle, err := srv.newResolvedWorkerSessionHandle(fs.cityBeadStore, worker.ResolvedSessionConfig{
		Alias:        "worker",
		ExplicitName: "worker-command-only",
		Template:     "myrig/worker",
		Title:        "Worker Command Only",
		Runtime: worker.ResolvedRuntime{
			Command: "/bin/echo --print",
			WorkDir: t.TempDir(),
		},
	})
	if err != nil {
		t.Fatalf("newResolvedWorkerSessionHandle: %v", err)
	}

	info, err := handle.Create(context.Background(), worker.CreateModeStarted)
	if err != nil {
		t.Fatalf("Create(started): %v", err)
	}

	start := fs.sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil")
	}
	if got, want := start.Command, "/bin/echo --print"; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}

	bead, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%q): %v", info.ID, err)
	}
	if got, want := bead.Metadata["provider"], "/bin/echo"; got != want {
		t.Fatalf("Metadata[provider] = %q, want %q", got, want)
	}
}

func TestWorkerFactoryRoutesWorkerOperationEventsToStateProvider(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	handle, err := srv.newResolvedWorkerSessionHandle(fs.cityBeadStore, worker.ResolvedSessionConfig{
		Alias:        "worker",
		ExplicitName: "worker-events",
		Template:     "myrig/worker",
		Title:        "Worker Events",
		Runtime: worker.ResolvedRuntime{
			Command:  "/bin/echo",
			WorkDir:  t.TempDir(),
			Provider: "resolved-worker",
			Resume: session.ProviderResume{
				SessionIDFlag: "--session-id",
			},
		},
	})
	if err != nil {
		t.Fatalf("newResolvedWorkerSessionHandle: %v", err)
	}

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	recorded := fs.eventProv.(*events.Fake).Events
	if len(recorded) == 0 {
		t.Fatal("worker start recorded no events")
	}
	last := recorded[len(recorded)-1]
	if got, want := last.Type, events.WorkerOperation; got != want {
		t.Fatalf("last event type = %q, want %q", got, want)
	}
}
