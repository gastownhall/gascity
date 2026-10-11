package runtimetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

type startFailProvider struct {
	runtime.Provider
	err error
}

func (p startFailProvider) Start(_ context.Context, _ string, _ runtime.Config) error {
	return p.err
}

type cleanupProvider struct {
	runtime.Provider
	startErr error
	stopErr  error
	started  []string
	stopped  []string
}

func (p *cleanupProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.started = append(p.started, name)
	if p.startErr != nil {
		return p.startErr
	}
	return p.Provider.Start(ctx, name, cfg)
}

func (p *cleanupProvider) Stop(name string) error {
	p.stopped = append(p.stopped, name)
	if p.stopErr != nil {
		return p.stopErr
	}
	return p.Provider.Stop(name)
}

type duplicateSuccessProvider struct {
	*cleanupProvider
	startCalls int
}

func (p *duplicateSuccessProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.startCalls++
	if p.startCalls > 1 {
		return nil
	}
	return p.cleanupProvider.Start(ctx, name, cfg)
}

type cleanupRecorder struct {
	cleanups []func()
	errors   []string
}

func (*cleanupRecorder) Helper() {}

func (r *cleanupRecorder) Cleanup(cleanup func()) {
	r.cleanups = append(r.cleanups, cleanup)
}

func (r *cleanupRecorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func TestStartOrSkipRegistersCleanup(t *testing.T) {
	const name = "cleanup-owned-session"
	provider := &cleanupProvider{Provider: runtime.NewFake()}

	if ok := t.Run("owner", func(t *testing.T) {
		startOrSkip(t, Options{}, provider, name, runtime.Config{}, "Start")
		if !provider.IsRunning(name) {
			t.Fatalf("IsRunning(%q) = false after Start, want true", name)
		}
	}); !ok {
		t.Fatal("cleanup owner subtest failed")
	}

	if len(provider.stopped) != 1 || provider.stopped[0] != name {
		t.Fatalf("Stop calls = %v, want [%s]", provider.stopped, name)
	}
	if provider.IsRunning(name) {
		t.Fatalf("IsRunning(%q) = true after owner cleanup", name)
	}
}

func TestStartWithCleanupRegistersEverySuccessfulStart(t *testing.T) {
	const name = "duplicate-success-session"
	provider := &duplicateSuccessProvider{
		cleanupProvider: &cleanupProvider{Provider: runtime.NewFake()},
	}

	if ok := t.Run("owner", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			if err := startWithCleanup(t, provider, name, runtime.Config{}); err != nil {
				t.Fatalf("Start call %d: %v", i+1, err)
			}
		}
	}); !ok {
		t.Fatal("cleanup owner subtest failed")
	}

	if len(provider.stopped) != 2 || provider.stopped[0] != name || provider.stopped[1] != name {
		t.Fatalf("Stop calls = %v, want [%s %s]", provider.stopped, name, name)
	}
}

func TestRegisterStopCleanupReportsFailure(t *testing.T) {
	const name = "cleanup-error-session"
	stopErr := errors.New("injected stop failure")
	provider := &cleanupProvider{Provider: runtime.NewFake(), stopErr: stopErr}
	recorder := &cleanupRecorder{}

	registerStopCleanup(recorder, provider, name)
	if len(recorder.cleanups) != 1 {
		t.Fatalf("registered cleanups = %d, want 1", len(recorder.cleanups))
	}
	recorder.cleanups[0]()

	if len(recorder.errors) != 1 {
		t.Fatalf("reported errors = %v, want one", recorder.errors)
	}
	if !strings.Contains(recorder.errors[0], name) || !strings.Contains(recorder.errors[0], stopErr.Error()) {
		t.Fatalf("reported error = %q, want session name and stop error", recorder.errors[0])
	}
}

func TestStartOrSkipDoesNotStopSessionAfterClassifiedStartFailure(t *testing.T) {
	const name = "preexisting-session"
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("seed pre-existing session: %v", err)
	}
	t.Cleanup(func() { _ = fake.Stop(name) })
	provider := &cleanupProvider{Provider: fake, startErr: runtime.ErrSessionExists}

	t.Run("classified failure", func(t *testing.T) {
		startOrSkip(t, Options{
			SkipStartError: func(err error) (string, bool) {
				return "session already exists", errors.Is(err, runtime.ErrSessionExists)
			},
		}, provider, name, runtime.Config{}, "Start")
		t.Fatal("startOrSkip returned after classified Start failure")
	})

	if len(provider.stopped) != 0 {
		t.Fatalf("Stop calls = %v, want none for an unowned name", provider.stopped)
	}
	if len(provider.started) != 1 || provider.started[0] != name {
		t.Fatalf("Start calls = %v, want [%s]", provider.started, name)
	}
	if !provider.IsRunning(name) {
		t.Fatalf("pre-existing session %q was stopped after failed Start", name)
	}
}

func TestRunLifecycleTestsDoesNotStopUnownedConcurrentStart(t *testing.T) {
	const preexisting = "preexisting-concurrent-session"
	providers := make(map[string]*runtime.Fake)
	factoryCalls := make(map[string]int)
	var targetProvider *runtime.Fake
	var counter int64

	if ok := t.Run("suite", func(t *testing.T) {
		RunLifecycleTestsWithOptions(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
			testName := t.Name()
			provider := providers[testName]
			if provider == nil {
				provider = runtime.NewFake()
				providers[testName] = provider
				if strings.HasSuffix(testName, "/Start_ConcurrentDistinctSessions") {
					if err := provider.Start(context.Background(), preexisting, runtime.Config{}); err != nil {
						t.Fatalf("seed pre-existing session: %v", err)
					}
					targetProvider = provider
				}
			}

			factoryCalls[testName]++
			name := fmt.Sprintf("conformance-session-%d", atomic.AddInt64(&counter, 1))
			if strings.HasSuffix(testName, "/Start_ConcurrentDistinctSessions") && factoryCalls[testName] == 2 {
				name = preexisting
			}
			return provider, runtime.Config{}, name
		}, Options{
			SkipStartError: func(err error) (string, bool) {
				return "session already exists", errors.Is(err, runtime.ErrSessionExists)
			},
		})
	}); !ok {
		t.Fatal("lifecycle conformance subtest failed")
	}

	if targetProvider == nil {
		t.Fatal("concurrent-start provider was not exercised")
	}
	t.Cleanup(func() { _ = targetProvider.Stop(preexisting) })
	running, err := targetProvider.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(running) != 1 || running[0] != preexisting {
		t.Fatalf("running sessions after cleanup = %v, want [%s]", running, preexisting)
	}
}

func TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors(t *testing.T) {
	startErr := errors.New("environmental start failure")
	provider := startFailProvider{Provider: runtime.NewFake(), err: startErr}
	var counter int64

	RunProviderTestsWithOptions(t, func(_ *testing.T) (runtime.Provider, runtime.Config, string) {
		id := atomic.AddInt64(&counter, 1)
		return provider, runtime.Config{}, fmt.Sprintf("skip-start-%d", id)
	}, Options{
		SkipStartError: func(err error) (string, bool) {
			if errors.Is(err, startErr) {
				return "provider environment unavailable", true
			}
			return "", false
		},
	})
}
