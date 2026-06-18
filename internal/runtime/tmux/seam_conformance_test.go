//go:build integration

package tmux

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestTmuxSeamConformance runs the FULL legacy Provider conformance suite against
// the tmux provider reconstructed from its seams via runtime.NewProviderFromSeams.
// Because the local tmux server is genuinely stateful, this gives the cut-over
// for the riskiest provider the same end-to-end validation subprocess got (the
// carrier providers' mocks aren't stateful enough for this). It exercises the
// seam path that production now uses for tmux.
func TestTmuxSeamConformance(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = "gc-seam-conform" // distinct server, isolated from TestTmuxConformance
	raw := NewProviderWithConfig(cfg)
	rt, tp := raw.Seams()
	p := runtime.NewProviderFromSeams(rt, tp)
	var counter int64

	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		id := atomic.AddInt64(&counter, 1)
		name := fmt.Sprintf("gc-test-seam-conform-%d", id)
		t.Cleanup(func() { _ = p.Stop(name) })
		return p, runtime.Config{
			Command: "sleep 300",
			WorkDir: t.TempDir(),
		}, name
	})
}
