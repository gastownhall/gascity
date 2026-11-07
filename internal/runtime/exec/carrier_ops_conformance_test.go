package exec

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

func TestOpsCarrierIdleWaitContract(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			fixture := func(screen string) func(*testing.T) (runtime.Provider, string) {
				return func(t *testing.T) (runtime.Provider, string) {
					pk := newOpsPack(t, opsIdleCaps)
					pk.put(t, "screen", screen)
					t.Cleanup(func() { pk.noTmux(t) })
					return ctor(pk.script), "s"
				}
			}
			runtimetest.RunIdleWaitContractTests(t, runtimetest.IdleWaitFixtures{Busy: fixture(busyClaudePane), Unobserved: fixture("no prompt"), Timeout: 200 * time.Millisecond})
		})
	}
}

func TestExecV2ConformanceNonTmux(t *testing.T) {
	state := t.TempDir()
	script := filepath.Join(t.TempDir(), "golden")
	if err := os.WriteFile(script, []byte(reference.Script(state, reference.NonTmuxBodies())), 0o755); err != nil {
		t.Fatal(err)
	}
	sp := NewSeamBackedForCity(script, "ops-conformance", "/ops-conformance")
	if !runtime.FreshReadable(sp) || !runtime.ListRunningAttested(sp) {
		t.Fatal("non-tmux golden not v2-ready")
	}
	n := 0
	runtimetest.RunProviderTests(t, func(ct *testing.T) (runtime.Provider, runtime.Config, string) {
		n++
		return sp, runtime.Config{WorkDir: ct.TempDir(), Command: "sleep 300", ProcessNames: []string{"sleep"}, AcceptStartupDialogs: new(bool)}, fmt.Sprintf("ops-v2-%06d", n)
	})
	if _, err := os.Stat(filepath.Join(state, "tmux-rejected")); !os.IsNotExist(err) {
		t.Error("production composition sent tmux to ops golden")
	}
}
