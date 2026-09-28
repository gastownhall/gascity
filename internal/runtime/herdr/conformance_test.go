package herdr

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestHerdrConformance runs the full runtime.Provider conformance suite against
// the herdr provider backed by a real herdr binary. Each session gets its own
// isolated herdr session-server so the contract's session-scoped assertions
// (ListRunning, orphan detection, …) don't observe sibling sessions. Opt-in
// live tier: see requireLiveHerdr.
func TestHerdrConformance(t *testing.T) {
	requireLiveHerdr(t)

	var counter int64
	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		n := atomic.AddInt64(&counter, 1)
		p := New(fmt.Sprintf("gctest-conf-%d", n), t.TempDir(), t.TempDir(), 0, 0)
		t.Cleanup(func() { _ = p.TeardownServer() })
		return p, runtime.Config{WorkDir: t.TempDir()}, fmt.Sprintf("conf-%d", n)
	})
}
