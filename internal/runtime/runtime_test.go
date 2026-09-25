package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type contextNudgeFake struct{ *Fake }

func (p *contextNudgeFake) NudgeContext(ctx context.Context, _ string, _ []ContentBlock) error {
	<-ctx.Done()
	return ctx.Err()
}

type blockingLegacyNudgeFake struct {
	*Fake
	unblock <-chan struct{}
	started chan<- struct{}
	calls   atomic.Int32
}

func (p *blockingLegacyNudgeFake) Nudge(string, []ContentBlock) error {
	p.calls.Add(1)
	if p.started != nil {
		p.started <- struct{}{}
	}
	<-p.unblock
	return nil
}

func TestNudgeContextBoundsLegacyProvider(t *testing.T) {
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := NudgeContext(ctx, &blockingLegacyNudgeFake{Fake: NewFake(), unblock: unblock}, "worker", TextContent("wake"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("NudgeContext error = %v, want context deadline exceeded", err)
	}
	if !errors.Is(err, ErrNudgeOutcomeUnknown) {
		t.Fatalf("NudgeContext error = %v, want ErrNudgeOutcomeUnknown for a legacy mutation still in flight", err)
	}
}

func TestNudgeContextBoundsBlockedLegacyProviderConcurrency(t *testing.T) {
	unblock := make(chan struct{})
	started := make(chan struct{}, maxConcurrentLegacyCallsPerProvider+1)
	provider := &blockingLegacyNudgeFake{Fake: NewFake(), unblock: unblock, started: started}
	var calls sync.WaitGroup
	for range maxConcurrentLegacyCallsPerProvider {
		calls.Add(1)
		go func() {
			defer calls.Done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_ = NudgeContext(ctx, provider, "worker", TextContent("wake"))
		}()
	}
	for range maxConcurrentLegacyCallsPerProvider {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("legacy nudge did not occupy its concurrency slot")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := NudgeContext(ctx, provider, "worker", TextContent("wake"))
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("NudgeContext over capacity error = %v, want context deadline exceeded", err)
	}
	if errors.Is(err, ErrNudgeOutcomeUnknown) {
		t.Fatalf("NudgeContext over capacity error = %v, call never started so outcome is known", err)
	}
	select {
	case <-started:
		t.Fatal("legacy nudge exceeded the per-provider concurrency bound")
	default:
	}
	if got := provider.calls.Load(); got != maxConcurrentLegacyCallsPerProvider {
		t.Fatalf("legacy provider calls = %d, want %d", got, maxConcurrentLegacyCallsPerProvider)
	}

	close(unblock)
	calls.Wait()
}

func TestNudgeContextCancelsContextAwareProvider(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := NudgeContext(ctx, &contextNudgeFake{Fake: NewFake()}, "worker", TextContent("wake"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("NudgeContext error = %v, want context canceled", err)
	}
}

func TestSyncWorkDirEnvSetsGCDir(t *testing.T) {
	cfg := SyncWorkDirEnv(Config{WorkDir: "/tmp/work"})
	if got := cfg.Env["GC_DIR"]; got != "/tmp/work" {
		t.Fatalf("GC_DIR = %q, want %q", got, "/tmp/work")
	}
}

func TestSyncWorkDirEnvCopiesEnvBeforeMutation(t *testing.T) {
	original := map[string]string{"GC_DIR": "/stale", "GC_AGENT": "worker"}
	cfg := SyncWorkDirEnv(Config{
		WorkDir: "/tmp/work",
		Env:     original,
	})
	if got := cfg.Env["GC_DIR"]; got != "/tmp/work" {
		t.Fatalf("GC_DIR = %q, want %q", got, "/tmp/work")
	}
	if got := original["GC_DIR"]; got != "/stale" {
		t.Fatalf("original GC_DIR mutated to %q", got)
	}
	if got := cfg.Env["GC_AGENT"]; got != "worker" {
		t.Fatalf("GC_AGENT = %q, want %q", got, "worker")
	}
}

func TestHasManagedStartupHints(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{name: "none", cfg: Config{}, want: false},
		{name: "ready prompt", cfg: Config{ReadyPromptPrefix: "> "}, want: true},
		{name: "ready delay", cfg: Config{ReadyDelayMs: 100}, want: true},
		{name: "process names", cfg: Config{ProcessNames: []string{"claude"}}, want: true},
		{name: "permission warning", cfg: Config{EmitsPermissionWarning: true}, want: true},
		{name: "startup dialog override", cfg: Config{AcceptStartupDialogs: boolPtr(false)}, want: true},
		{name: "nudge", cfg: Config{Nudge: "Check your hook."}, want: true},
		{name: "pre start", cfg: Config{PreStart: []string{"echo pre"}}, want: true},
		{name: "session setup", cfg: Config{SessionSetup: []string{"echo setup"}}, want: true},
		{name: "session setup script", cfg: Config{SessionSetupScript: "setup.sh"}, want: true},
		{name: "session live", cfg: Config{SessionLive: []string{"echo live"}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasManagedStartupHints(tt.cfg); got != tt.want {
				t.Fatalf("HasManagedStartupHints() = %v, want %v", got, tt.want)
			}
		})
	}
}

func boolPtr(v bool) *bool {
	return &v
}
