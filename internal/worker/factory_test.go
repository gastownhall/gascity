package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestFactorySessionAndCatalogShareWorkerBoundary(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	searchPaths := []string{"/tmp/worker-a", "/tmp/worker-b"}

	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: searchPaths,
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	handle, err := factory.Session(SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("factory.Session: %v", err)
	}
	if !reflect.DeepEqual(handle.adapter.SearchPaths, searchPaths) {
		t.Fatalf("handle adapter search paths = %#v, want %#v", handle.adapter.SearchPaths, searchPaths)
	}

	info, err := handle.Create(context.Background(), CreateModeDeferred)
	if err != nil {
		t.Fatalf("Create(deferred): %v", err)
	}

	catalog, err := factory.Catalog()
	if err != nil {
		t.Fatalf("factory.Catalog: %v", err)
	}
	got, err := catalog.Get(info.ID)
	if err != nil {
		t.Fatalf("catalog.Get(%q): %v", info.ID, err)
	}
	if got.ID != info.ID {
		t.Fatalf("catalog.Get(%q).ID = %q, want %q", info.ID, got.ID, info.ID)
	}
	if got.Template != "probe" {
		t.Fatalf("catalog.Get(%q).Template = %q, want probe", info.ID, got.Template)
	}
}

func TestFactoryAdapterUsesConfiguredSearchPaths(t *testing.T) {
	factory, err := NewFactory(FactoryConfig{
		Store:       beads.NewMemStore(),
		SearchPaths: []string{"/tmp/factory-search"},
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	adapter := factory.Adapter()
	if !reflect.DeepEqual(adapter.SearchPaths, []string{"/tmp/factory-search"}) {
		t.Fatalf("Adapter().SearchPaths = %#v, want %#v", adapter.SearchPaths, []string{"/tmp/factory-search"})
	}
}

func TestFactoryTranscriptMethodsUseConfiguredSearchPaths(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()
	slug := strings.ReplaceAll(workDir, "/", "-")
	slug = strings.ReplaceAll(slug, ".", "-")
	transcriptDir := filepath.Join(searchBase, slug)
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", transcriptDir, err)
	}
	transcriptPath := filepath.Join(transcriptDir, "session.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(
		"{\"uuid\":\"1\",\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"model\":\"claude-opus-4-5-20251101\",\"usage\":{\"input_tokens\":1000}},\"timestamp\":\"2025-01-01T00:00:00Z\"}\n",
	), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", transcriptPath, err)
	}

	factory, err := NewFactory(FactoryConfig{
		Store:       beads.NewMemStore(),
		SearchPaths: []string{searchBase},
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	gotPath := factory.DiscoverTranscript("claude", workDir, "")
	if gotPath != transcriptPath {
		t.Fatalf("DiscoverTranscript() = %q, want %q", gotPath, transcriptPath)
	}
	meta, err := factory.TailMeta(transcriptPath)
	if err != nil {
		t.Fatalf("TailMeta(%q): %v", transcriptPath, err)
	}
	if meta == nil || meta.Model != "claude-opus-4-5-20251101" {
		t.Fatalf("TailMeta(%q) = %#v, want model claude-opus-4-5-20251101", transcriptPath, meta)
	}
}

func TestFactorySessionByIDResolvesSessionRuntime(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManager(store, sp)

	info, err := manager.CreateBeadOnly(
		"worker",
		"Probe",
		"",
		t.TempDir(),
		"legacy-provider",
		"",
		nil,
		sessionpkg.ProviderResume{SessionIDFlag: "--stale-session-id"},
	)
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	if err := store.SetMetadata(info.ID, "real_world_app_session_kind", "provider"); err != nil {
		t.Fatalf("SetMetadata(real_world_app_session_kind): %v", err)
	}
	if err := store.SetMetadata(info.ID, "worker_profile", string(ProfileClaudeTmuxCLI)); err != nil {
		t.Fatalf("SetMetadata(worker_profile): %v", err)
	}

	var gotSessionKind string
	var gotProfile Profile
	factory, err := NewFactory(FactoryConfig{
		Store:    store,
		Provider: sp,
		ResolveSessionRuntime: func(_ sessionpkg.Info, sessionKind string, _ map[string]string) (*ResolvedRuntime, error) {
			gotSessionKind = sessionKind
			return &ResolvedRuntime{
				Command:  "/bin/echo",
				WorkDir:  t.TempDir(),
				Provider: "stub",
				Resume:   sessionpkg.ProviderResume{SessionIDFlag: "--session-id"},
				Hints: runtime.Config{
					ReadyPromptPrefix: "stub-ready>",
					ReadyDelayMs:      250,
				},
				SessionEnv: map[string]string{"STUB_ENV": "present"},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	handle, err := factory.SessionByID(info.ID)
	if err != nil {
		t.Fatalf("SessionByID(%q): %v", info.ID, err)
	}
	sessionHandle, ok := handle.(*SessionHandle)
	if !ok {
		t.Fatalf("SessionByID(%q) returned %T, want *SessionHandle", info.ID, handle)
	}
	gotProfile = sessionHandle.session.Profile
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if gotSessionKind != "provider" {
		t.Fatalf("sessionKind = %q, want provider", gotSessionKind)
	}
	if gotProfile != ProfileClaudeTmuxCLI {
		t.Fatalf("profile = %q, want %q", gotProfile, ProfileClaudeTmuxCLI)
	}
	start := sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatal("LastStartConfig() = nil")
	}
	wantArg := "--session-id " + info.SessionKey
	if got := start.Command; got != "/bin/echo "+wantArg {
		t.Fatalf("start command = %q, want %q", got, "/bin/echo "+wantArg)
	}
	if got := start.ReadyPromptPrefix; got != "stub-ready>" {
		t.Fatalf("ReadyPromptPrefix = %q, want stub-ready>", got)
	}
	if got := start.ReadyDelayMs; got != 250 {
		t.Fatalf("ReadyDelayMs = %d, want 250", got)
	}
	if got := start.Env["STUB_ENV"]; got != "present" {
		t.Fatalf("Env[STUB_ENV] = %q, want present", got)
	}
}

func TestFactoryTransportResolverReceivesProviderForLegacyProviderSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManager(store, sp)

	info, err := manager.CreateBeadOnly(
		"opencode",
		"Probe",
		"",
		t.TempDir(),
		"opencode",
		"",
		nil,
		sessionpkg.ProviderResume{},
	)
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	if err := store.SetMetadata(info.ID, "mc_session_kind", "provider"); err != nil {
		t.Fatalf("SetMetadata(mc_session_kind): %v", err)
	}

	var gotTemplate, gotProvider string
	factory, err := NewFactory(FactoryConfig{
		Store:    store,
		Provider: sp,
		ResolveTransport: func(template, provider string) string {
			gotTemplate = template
			gotProvider = provider
			if provider == "opencode" {
				return "acp"
			}
			return ""
		},
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	catalog, err := factory.Catalog()
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	got, err := catalog.Get(info.ID)
	if err != nil {
		t.Fatalf("catalog.Get(%q): %v", info.ID, err)
	}
	if gotTemplate != "opencode" {
		t.Fatalf("ResolveTransport template = %q, want %q", gotTemplate, "opencode")
	}
	if gotProvider != "opencode" {
		t.Fatalf("ResolveTransport provider = %q, want %q", gotProvider, "opencode")
	}
	if got.Transport != "acp" {
		t.Fatalf("catalog.Get(%q).Transport = %q, want %q", info.ID, got.Transport, "acp")
	}
}

func TestFactorySessionByIDPropagatesResolvedRuntimeError(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManager(store, sp)

	info, err := manager.CreateBeadOnly(
		"worker",
		"Probe",
		"",
		t.TempDir(),
		"legacy-provider",
		"",
		nil,
		sessionpkg.ProviderResume{SessionIDFlag: "--stale-session-id"},
	)
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}

	wantErr := errors.New("resolve runtime boom")
	factory, err := NewFactory(FactoryConfig{
		Store:    store,
		Provider: sp,
		ResolveSessionRuntime: func(sessionpkg.Info, string, map[string]string) (*ResolvedRuntime, error) {
			return nil, wantErr
		},
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	_, err = factory.SessionByID(info.ID)
	if !errors.Is(err, wantErr) {
		t.Fatalf("SessionByID(%q) error = %v, want %v", info.ID, err, wantErr)
	}
}

func TestFactorySessionByIDPreservesTemplateInWorkerOperationEvents(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManager(store, sp)
	recorder := events.NewFake()

	info, err := manager.CreateBeadOnly(
		"myrig/worker",
		"Probe",
		"",
		t.TempDir(),
		"stub",
		"",
		nil,
		sessionpkg.ProviderResume{SessionIDFlag: "--session-id"},
	)
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}

	factory, err := NewFactory(FactoryConfig{
		Store:    store,
		Provider: sp,
		Recorder: recorder,
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	handle, err := factory.SessionByID(info.ID)
	if err != nil {
		t.Fatalf("SessionByID(%q): %v", info.ID, err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	recorded := recorder.Events
	if len(recorded) == 0 {
		t.Fatal("no worker events recorded")
	}
	var payload operationEventPayload
	if err := json.Unmarshal(recorded[len(recorded)-1].Payload, &payload); err != nil {
		t.Fatalf("Unmarshal(payload): %v", err)
	}
	if got, want := payload.Template, info.Template; got != want {
		t.Fatalf("payload.Template = %q, want %q", got, want)
	}
}

func TestFactoryHandleForTargetResolvesRuntimeSessionMeta(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManager(store, sp)

	info, err := manager.Create(
		context.Background(),
		"worker",
		"Probe",
		"",
		t.TempDir(),
		"stub",
		nil,
		sessionpkg.ProviderResume{},
		runtime.Config{},
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sp.SetMeta("legacy-runtime-name", "GC_SESSION_ID", info.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	factory, err := NewFactory(FactoryConfig{
		Store:    store,
		Provider: sp,
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	handle, err := factory.HandleForTarget("legacy-runtime-name", nil)
	if err != nil {
		t.Fatalf("HandleForTarget: %v", err)
	}
	if err := handle.Kill(context.Background()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	last := sp.Calls[len(sp.Calls)-1]
	if last.Method != "Stop" || last.Name != info.SessionName {
		t.Fatalf("last runtime call = %#v, want Stop %q", last, info.SessionName)
	}
}

func TestFactoryHandleForTargetRuntimeFallbackPreservesRecorder(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	recorder := events.NewFake()
	if err := sp.Start(context.Background(), "legacy-runtime-name", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := sp.SetMeta("legacy-runtime-name", "GC_PROVIDER", "claude"); err != nil {
		t.Fatalf("SetMeta(GC_PROVIDER): %v", err)
	}

	factory, err := NewFactory(FactoryConfig{
		Store:    store,
		Provider: sp,
		Recorder: recorder,
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	handle, err := factory.HandleForTarget("legacy-runtime-name", nil)
	if err != nil {
		t.Fatalf("HandleForTarget: %v", err)
	}
	if err := handle.Interrupt(context.Background(), InterruptRequest{}); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	recorded := recorder.Events
	if len(recorded) == 0 {
		t.Fatal("no worker events recorded")
	}
	var payload operationEventPayload
	if err := json.Unmarshal(recorded[len(recorded)-1].Payload, &payload); err != nil {
		t.Fatalf("Unmarshal(payload): %v", err)
	}
	if got, want := payload.Operation, string(workerOperationInterrupt); got != want {
		t.Fatalf("payload.Operation = %q, want %q", got, want)
	}
	if got, want := payload.SessionName, "legacy-runtime-name"; got != want {
		t.Fatalf("payload.SessionName = %q, want %q", got, want)
	}
	if got, want := payload.Provider, "claude"; got != want {
		t.Fatalf("payload.Provider = %q, want %q", got, want)
	}
}

type failingGetStore struct {
	beads.Store
	err error
}

func (s failingGetStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, s.err
}

func TestFactoryHandleForTargetPropagatesSessionResolutionError(t *testing.T) {
	wantErr := errors.New("store boom")
	factory, err := NewFactory(FactoryConfig{
		Store: failingGetStore{
			Store: beads.NewMemStore(),
			err:   wantErr,
		},
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	_, err = factory.HandleForTarget("sess-1", nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandleForTarget error = %v, want %v", err, wantErr)
	}
}

func TestFactoryRuntimeHandleUsesConfiguredProviderAndRecorder(t *testing.T) {
	sp := runtime.NewFake()
	recorder := events.NewFake()
	processNames := []string{"claude"}

	factory, err := NewFactory(FactoryConfig{
		Provider: sp,
		Recorder: recorder,
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	handle, err := factory.RuntimeHandle("legacy-runtime-name", "claude", "tmux-cli", processNames)
	if err != nil {
		t.Fatalf("RuntimeHandle: %v", err)
	}
	runtimeHandle, ok := handle.(*RuntimeHandle)
	if !ok {
		t.Fatalf("RuntimeHandle() returned %T, want *RuntimeHandle", handle)
	}
	if runtimeHandle.provider != sp {
		t.Fatal("RuntimeHandle().provider did not reuse factory provider")
	}
	if got, want := runtimeHandle.providerName, "claude"; got != want {
		t.Fatalf("providerName = %q, want %q", got, want)
	}
	if got, want := runtimeHandle.transport, "tmux-cli"; got != want {
		t.Fatalf("transport = %q, want %q", got, want)
	}
	processNames[0] = "mutated"
	if got, want := runtimeHandle.processNames[0], "claude"; got != want {
		t.Fatalf("processNames[0] = %q, want %q", got, want)
	}

	if err := sp.Start(context.Background(), "legacy-runtime-name", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := handle.Interrupt(context.Background(), InterruptRequest{}); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	recorded := recorder.Events
	if len(recorded) == 0 {
		t.Fatal("no worker events recorded")
	}
	var payload operationEventPayload
	if err := json.Unmarshal(recorded[len(recorded)-1].Payload, &payload); err != nil {
		t.Fatalf("Unmarshal(payload): %v", err)
	}
	if got, want := payload.Operation, string(workerOperationInterrupt); got != want {
		t.Fatalf("payload.Operation = %q, want %q", got, want)
	}
	if got, want := payload.SessionName, "legacy-runtime-name"; got != want {
		t.Fatalf("payload.SessionName = %q, want %q", got, want)
	}
}
