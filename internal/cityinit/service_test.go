package cityinit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

type recordingLifecycleEvents struct {
	ensureErr     error
	createdErr    error
	unregisterErr error
	ensured       []string
	created       []struct {
		path string
		name string
	}
	unregistered []RegisteredCity
}

func (r *recordingLifecycleEvents) EnsureCityLog(cityPath string) error {
	r.ensured = append(r.ensured, cityPath)
	return r.ensureErr
}

func (r *recordingLifecycleEvents) CityCreated(cityPath, name string) error {
	r.created = append(r.created, struct {
		path string
		name string
	}{path: cityPath, name: name})
	return r.createdErr
}

func (r *recordingLifecycleEvents) CityUnregisterRequested(city RegisteredCity) error {
	r.unregistered = append(r.unregistered, city)
	return r.unregisterErr
}

type mockRegistry struct {
	registerFn   func(ctx context.Context, dir, nameOverride string) error
	findFn       func(ctx context.Context, name string) (RegisteredCity, error)
	unregisterFn func(ctx context.Context, city RegisteredCity) error
}

func (m *mockRegistry) Register(ctx context.Context, dir, nameOverride string) error {
	if m.registerFn != nil {
		return m.registerFn(ctx, dir, nameOverride)
	}
	return nil
}

func (m *mockRegistry) Find(ctx context.Context, name string) (RegisteredCity, error) {
	if m.findFn != nil {
		return m.findFn(ctx, name)
	}
	return RegisteredCity{}, ErrNotRegistered
}

func (m *mockRegistry) Unregister(ctx context.Context, city RegisteredCity) error {
	if m.unregisterFn != nil {
		return m.unregisterFn(ctx, city)
	}
	return nil
}

type mockReloader struct {
	reloadFn           func() error
	reloadAfterUnregFn func() error
}

func (m *mockReloader) Reload() error {
	if m.reloadFn != nil {
		return m.reloadFn()
	}
	return nil
}

func (m *mockReloader) ReloadAfterUnregister() error {
	if m.reloadAfterUnregFn != nil {
		return m.reloadAfterUnregFn()
	}
	return nil
}

type mockInitializer struct {
	scaffoldFn func(ctx context.Context, req InitRequest) error
	finalizeFn func(ctx context.Context, req InitRequest) error
}

func (m *mockInitializer) Scaffold(ctx context.Context, req InitRequest) error {
	if m.scaffoldFn != nil {
		return m.scaffoldFn(ctx, req)
	}
	return nil
}

func (m *mockInitializer) Finalize(ctx context.Context, req InitRequest) error {
	if m.finalizeFn != nil {
		return m.finalizeFn(ctx, req)
	}
	return nil
}

func mustNewService(t *testing.T, deps ServiceDeps) *Service {
	t.Helper()
	svc, err := NewService(deps)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestServiceValidateInitRequest(t *testing.T) {
	absDir := filepath.Join(t.TempDir(), "city")
	svc := mustNewService(t, ServiceDeps{FS: fsys.OSScaffoldFS{}})

	tests := []struct {
		name    string
		req     InitRequest
		wantErr error
	}{
		{
			name:    "missing dir",
			req:     InitRequest{Provider: "codex"},
			wantErr: ErrInvalidDirectory,
		},
		{
			name:    "relative dir",
			req:     InitRequest{Dir: "relative", Provider: "codex"},
			wantErr: ErrInvalidDirectory,
		},
		{
			name:    "provider or start command required",
			req:     InitRequest{Dir: absDir},
			wantErr: ErrInvalidProvider,
		},
		{
			name:    "provider and start command conflict",
			req:     InitRequest{Dir: absDir, Provider: "codex", StartCommand: "custom-agent"},
			wantErr: ErrInvalidProvider,
		},
		{
			name:    "unknown provider",
			req:     InitRequest{Dir: absDir, Provider: "not-a-provider"},
			wantErr: ErrInvalidProvider,
		},
		{
			name:    "unknown bootstrap",
			req:     InitRequest{Dir: absDir, Provider: "codex", BootstrapProfile: "moon-base"},
			wantErr: ErrInvalidBootstrapProfile,
		},
		{
			name: "valid provider",
			req:  InitRequest{Dir: absDir, Provider: "codex"},
		},
		{
			name: "valid custom command",
			req:  InitRequest{Dir: absDir, StartCommand: "custom-agent"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := svc.ValidateInitRequest(tt.req)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateInitRequest error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestServiceValidateInitRequestUsesInternalProviderValidation(t *testing.T) {
	absDir := filepath.Join(t.TempDir(), "city")
	svc := mustNewService(t, ServiceDeps{FS: fsys.OSScaffoldFS{}})

	if err := svc.ValidateInitRequest(InitRequest{
		Dir:              absDir,
		Provider:         "codex",
		BootstrapProfile: "kubernetes",
	}); err != nil {
		t.Fatalf("ValidateInitRequest valid provider/profile error = %v, want nil", err)
	}

	err := svc.ValidateInitRequest(InitRequest{Dir: absDir, Provider: "not-a-provider"})
	if !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("ValidateInitRequest unknown provider error = %v, want ErrInvalidProvider", err)
	}

	err = svc.ValidateInitRequest(InitRequest{Dir: absDir, Provider: "codex", BootstrapProfile: "moon-base"})
	if !errors.Is(err, ErrInvalidBootstrapProfile) {
		t.Fatalf("ValidateInitRequest unknown bootstrap error = %v, want ErrInvalidBootstrapProfile", err)
	}
}

func TestServiceInitScaffoldsAndFinalizes(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "init-city")
	var calls []string
	svc := mustNewService(t, ServiceDeps{
		FS: fsys.OSScaffoldFS{},
		Initializer: &mockInitializer{
			scaffoldFn: func(_ context.Context, req InitRequest) error {
				calls = append(calls, "do-init:"+req.Dir+":"+req.Provider)
				if err := os.MkdirAll(filepath.Join(req.Dir, citylayout.RuntimeRoot), 0o755); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(req.Dir, citylayout.CityConfigFile), []byte("[workspace]\nname = \"init-city\"\n"), 0o644)
			},
			finalizeFn: func(_ context.Context, req InitRequest) error {
				calls = append(calls, "finalize:"+req.Dir)
				return nil
			},
		},
	})

	result, err := svc.Init(context.Background(), InitRequest{
		Dir:      cityPath,
		Provider: "codex",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if result.CityName != "init-city" || result.CityPath != cityPath || result.ProviderUsed != "codex" {
		t.Fatalf("Init result = %+v", result)
	}
	wantCalls := []string{"do-init:" + cityPath + ":codex", "finalize:" + cityPath}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", calls, wantCalls)
	}
}

func TestServiceInitRequiresInitializerBeforeSideEffects(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "init-city")
	svc := mustNewService(t, ServiceDeps{
		FS: fsys.OSScaffoldFS{},
	})

	_, err := svc.Init(context.Background(), InitRequest{
		Dir:      cityPath,
		Provider: "codex",
	})
	if !errors.Is(err, ErrNotWired) {
		t.Fatalf("Init error = %v, want ErrNotWired", err)
	}
	if _, statErr := os.Stat(cityPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("city path after unwired Init = %v, want removed/not created", statErr)
	}
}

func TestServiceScaffoldRegistersAndEmitsCreated(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "api-city")
	var registered bool
	var reloaded bool
	lifecycleEvents := &recordingLifecycleEvents{}
	svc := mustNewService(t, ServiceDeps{
		FS: fsys.OSScaffoldFS{},
		Initializer: &mockInitializer{scaffoldFn: func(_ context.Context, req InitRequest) error {
			if err := os.MkdirAll(filepath.Join(req.Dir, citylayout.RuntimeRoot), 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(req.Dir, citylayout.CityConfigFile), []byte("[workspace]\nname = \"api-city\"\n"), 0o644)
		}},
		Registry: &mockRegistry{registerFn: func(_ context.Context, dir, nameOverride string) error {
			if dir != cityPath || nameOverride != "" {
				t.Fatalf("Register(%q, %q), want (%q, \"\")", dir, nameOverride, cityPath)
			}
			registered = true
			return nil
		}},
		Reloader:        &mockReloader{reloadFn: func() error { reloaded = true; return nil }},
		LifecycleEvents: lifecycleEvents,
	})

	result, err := svc.Scaffold(context.Background(), InitRequest{
		Dir:      cityPath,
		Provider: "codex",
	})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if result.CityName != "api-city" || result.CityPath != cityPath || result.ProviderUsed != "codex" {
		t.Fatalf("Scaffold result = %+v", result)
	}
	if !registered {
		t.Fatal("RegisterCity was not called")
	}
	if !reloaded {
		t.Fatal("ReloadSupervisor was not called")
	}
	if !reflect.DeepEqual(lifecycleEvents.ensured, []string{cityPath}) {
		t.Fatalf("ensured event logs = %v, want [%s]", lifecycleEvents.ensured, cityPath)
	}
	if len(lifecycleEvents.created) != 1 || lifecycleEvents.created[0].name != "api-city" || lifecycleEvents.created[0].path != cityPath {
		t.Fatalf("created lifecycle events = %+v, want api-city/%s", lifecycleEvents.created, cityPath)
	}
}

func TestServiceScaffoldReturnsPostRegisterErrorWithResultWhenCityCreatedFails(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "api-city")
	lifecycleErr := errors.New("event log unavailable")
	var registered bool
	svc := mustNewService(t, ServiceDeps{
		FS: fsys.OSScaffoldFS{},
		Initializer: &mockInitializer{scaffoldFn: func(_ context.Context, req InitRequest) error {
			if err := os.MkdirAll(filepath.Join(req.Dir, citylayout.RuntimeRoot), 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(req.Dir, citylayout.CityConfigFile), []byte("[workspace]\nname = \"api-city\"\n"), 0o644)
		}},
		Registry: &mockRegistry{registerFn: func(context.Context, string, string) error {
			registered = true
			return nil
		}},
		LifecycleEvents: &recordingLifecycleEvents{createdErr: lifecycleErr},
	})

	result, err := svc.Scaffold(context.Background(), InitRequest{
		Dir:      cityPath,
		Provider: "codex",
	})
	if !errors.Is(err, ErrPostRegisterFailure) {
		t.Fatalf("Scaffold error = %v, want ErrPostRegisterFailure", err)
	}
	if !errors.Is(err, lifecycleErr) {
		t.Fatalf("Scaffold error = %v, want wrapped lifecycle error %v", err, lifecycleErr)
	}
	if result == nil {
		t.Fatal("Scaffold result = nil, want committed city result")
	}
	if result.CityName != "api-city" || result.CityPath != cityPath {
		t.Fatalf("Scaffold result = %+v, want api-city/%s", result, cityPath)
	}
	if !registered {
		t.Fatal("Register was not called before post-register error")
	}
}

func TestServiceScaffoldUsesInternalScaffoldDetection(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "api-city")
	if err := EnsureCityScaffoldFS(fsys.OSFS{}, cityPath); err != nil {
		t.Fatalf("EnsureCityScaffoldFS: %v", err)
	}
	svc := mustNewService(t, ServiceDeps{
		FS: fsys.OSScaffoldFS{},
		Initializer: &mockInitializer{scaffoldFn: func(context.Context, InitRequest) error {
			t.Fatal("Scaffold should not run for an already scaffolded city")
			return nil
		}},
		Registry: &mockRegistry{registerFn: func(context.Context, string, string) error {
			t.Fatal("Register should not run for an already scaffolded city")
			return nil
		}},
		LifecycleEvents: &recordingLifecycleEvents{},
	})

	_, err := svc.Scaffold(context.Background(), InitRequest{
		Dir:      cityPath,
		Provider: "codex",
	})
	if !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("Scaffold error = %v, want ErrAlreadyInitialized", err)
	}
}

func TestServiceScaffoldRequiresRegisterBeforeSideEffects(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "api-city")
	scaffoldCalled := false
	svc := mustNewService(t, ServiceDeps{
		FS: fsys.OSScaffoldFS{},
		Initializer: &mockInitializer{scaffoldFn: func(_ context.Context, req InitRequest) error {
			scaffoldCalled = true
			return os.MkdirAll(filepath.Join(req.Dir, citylayout.RuntimeRoot), 0o755)
		}},
	})

	_, err := svc.Scaffold(context.Background(), InitRequest{
		Dir:      cityPath,
		Provider: "codex",
	})
	if !errors.Is(err, ErrNotWired) {
		t.Fatalf("Scaffold error = %v, want ErrNotWired", err)
	}
	if scaffoldCalled {
		t.Fatal("Initializer.Scaffold was called before Registry was wired")
	}
	if _, statErr := os.Stat(cityPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("city path after unwired Scaffold = %v, want removed/not created", statErr)
	}
}

func TestServiceScaffoldFailsBeforeRegisterWhenEventLogCannotBeCreated(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "api-city")
	var registered bool
	eventErr := errors.New("event log unavailable")
	svc := mustNewService(t, ServiceDeps{
		FS: fsys.OSScaffoldFS{},
		Initializer: &mockInitializer{scaffoldFn: func(_ context.Context, req InitRequest) error {
			if err := os.MkdirAll(filepath.Join(req.Dir, citylayout.RuntimeRoot), 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(req.Dir, citylayout.CityConfigFile), []byte("[workspace]\nname = \"api-city\"\n"), 0o644)
		}},
		Registry: &mockRegistry{registerFn: func(context.Context, string, string) error {
			registered = true
			return nil
		}},
		LifecycleEvents: &recordingLifecycleEvents{ensureErr: eventErr},
	})

	_, err := svc.Scaffold(context.Background(), InitRequest{
		Dir:      cityPath,
		Provider: "codex",
	})
	if !errors.Is(err, eventErr) {
		t.Fatalf("Scaffold error = %v, want %v", err, eventErr)
	}
	if registered {
		t.Fatal("RegisterCity was called after event log creation failed")
	}
	if _, statErr := os.Stat(cityPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("city path after failed scaffold = %v, want removed", statErr)
	}
}

func TestServiceScaffoldRollbackUsesInternalManagedPaths(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "api-city")
	keepPath := filepath.Join(cityPath, "keep.txt")
	customAgentPath := filepath.Join(cityPath, "agents", "custom.txt")
	generatedAgentPath := filepath.Join(cityPath, "agents", "generated.txt")
	if err := os.MkdirAll(filepath.Dir(customAgentPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keepPath, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(customAgentPath, []byte("custom"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := mustNewService(t, ServiceDeps{
		FS: fsys.OSScaffoldFS{},
		Initializer: &mockInitializer{scaffoldFn: func(_ context.Context, req InitRequest) error {
			if err := os.WriteFile(generatedAgentPath, []byte("generated"), 0o644); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Join(req.Dir, citylayout.RuntimeRoot), 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(req.Dir, citylayout.CityConfigFile), []byte("[workspace]\nname = \"api-city\"\n"), 0o644)
		}},
		Registry: &mockRegistry{registerFn: func(context.Context, string, string) error {
			return errors.New("registry unavailable")
		}},
		LifecycleEvents: &recordingLifecycleEvents{},
	})

	_, err := svc.Scaffold(context.Background(), InitRequest{
		Dir:      cityPath,
		Provider: "codex",
	})
	if err == nil {
		t.Fatal("Scaffold error = nil, want registration failure")
	}
	if _, statErr := os.Stat(generatedAgentPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("generated managed file stat = %v, want removed", statErr)
	}
	if data, readErr := os.ReadFile(customAgentPath); readErr != nil || string(data) != "custom" {
		t.Fatalf("custom agent file = %q/%v, want preserved", string(data), readErr)
	}
	if data, readErr := os.ReadFile(keepPath); readErr != nil || string(data) != "keep" {
		t.Fatalf("keep file = %q/%v, want preserved", string(data), readErr)
	}
}
