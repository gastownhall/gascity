//go:build integration

package tmux

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestTmuxSeamsLifecycle proves the split Runtime/Transport contracts compose
// over one real tmux session. Full Provider behavior is covered once by
// TestTmuxConformance through NewSeamBackedWithConfig; repeating the full suite
// here would test the same provider and adapter path twice.
func TestTmuxSeamsLifecycle(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	raw := NewProviderWithConfig(cfg)
	rt, tp := raw.Seams()
	name := "gc-test-seam-lifecycle"
	t.Cleanup(func() { _ = rt.Teardown(context.Background(), name) })

	place, err := rt.Provision(context.Background(), name, runtime.ProvisionRequest{Config: runtime.Config{
		Command: "sleep 300",
		WorkDir: t.TempDir(),
	}})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if running, err := place.IsRunning(context.Background()); err != nil || !running {
		t.Fatalf("Place.IsRunning = %v, %v; want true, nil", running, err)
	}

	attachment, ok, err := tp.Open(context.Background(), place, name)
	if err != nil || !ok {
		t.Fatalf("Transport.Open = _, %v, %v; want attachment, true, nil", ok, err)
	}
	observation, err := attachment.Observe(context.Background(), nil)
	if err != nil {
		t.Fatalf("Attachment.Observe: %v", err)
	}
	if !observation.ProcessAlive {
		t.Fatal("Attachment.Observe ProcessAlive = false, want true")
	}

	if err := place.Teardown(context.Background()); err != nil {
		t.Fatalf("Place.Teardown: %v", err)
	}
	if _, found, err := rt.Open(context.Background(), name); err != nil || found {
		t.Fatalf("Runtime.Open after teardown = _, %v, %v; want _, false, nil", found, err)
	}
}
