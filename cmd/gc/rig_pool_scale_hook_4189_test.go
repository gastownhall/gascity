package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestRigScopedPoolDefaultsCoverFieldScenario_4189 is a combined regression
// guard for #4189 (the moneymachine field report): a rig-scoped pool agent
// with NEITHER an explicit scale_check NOR an explicit work_query must still
// (a) report non-zero controller demand and (b) have `gc hook` surface the
// same ready, unassigned, routed bead — both sourced from the agent's own
// rig store, with no local workaround. The two halves were previously
// covered separately (scale-from-zero demand tests; hook rig-store env
// wiring tests) but never asserted together against one shared bead/target,
// which is the exact combination the field report needed verified.
func TestRigScopedPoolDefaultsCoverFieldScenario_4189(t *testing.T) {
	t.Run("demand", func(t *testing.T) {
		cfg, cityStore, rigStores, qualified := newNoScaleCheckRigPoolCity(t)

		if _, err := rigStores["rig-A"].Create(beads.Bead{
			ID:       "bead-4189",
			Status:   "open",
			Type:     "task",
			Metadata: map[string]string{"gc.routed_to": qualified},
		}); err != nil {
			t.Fatal(err)
		}

		result := buildDesiredStateWithSessionBeads(
			"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
			cityStore, rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
		)

		if got := result.ScaleCheckCounts[qualified]; got != 1 {
			t.Errorf("controller demand = %d, want 1 (default scale_check must read the rig store for a no-scale_check rig pool agent)", got)
		}
	})

	t.Run("hook", func(t *testing.T) {
		clearGCEnv(t)
		disableManagedDoltRecoveryForTest(t)
		t.Setenv("GC_TMUX_SESSION", "rig-a-executor-4189")
		cityDir := t.TempDir()
		rigDir := filepath.Join(cityDir, "rig-A-repo")
		fakeBin := t.TempDir()

		if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		cityToml := fmt.Sprintf(`[workspace]
name = "test-city"

[[rigs]]
name = "rig-A"
path = %q

[[agent]]
name = "executor"
dir = "rig-A"

[agent.pool]
min = 0
max = 5
`, rigDir)
		if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
			t.Fatal(err)
		}

		// Fake bd: any invocation whose args contain the canonical
		// routed/unassigned predicate for rig-A/executor returns one ready
		// bead; every other invocation (assigned-tier probes, ephemeral
		// probes, the legacy run_target migration tier) returns an empty
		// array, exactly like a real store with no other work.
		fakeBD := filepath.Join(fakeBin, "bd")
		script := "#!/bin/sh\n" +
			"for a in \"$@\"; do\n" +
			"  case \"$a\" in\n" +
			"    *'gc.routed_to=rig-A/executor'*)\n" +
			"      printf '[{\"id\":\"bead-4189\",\"status\":\"open\",\"type\":\"task\",\"metadata\":{\"gc.routed_to\":\"rig-A/executor\"}}]'\n" +
			"      exit 0\n" +
			"      ;;\n" +
			"  esac\n" +
			"done\n" +
			"printf '[]'\n"
		if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}

		origPath := os.Getenv("PATH")
		t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+origPath)
		t.Setenv("GC_CITY", cityDir)
		t.Setenv("GC_AGENT", "rig-A/executor")

		var stdout, stderr bytes.Buffer
		code := cmdHook(nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("cmdHook() = %d, want 0 (default work_query must surface the routed rig-store bead); stderr=%s stdout=%s", code, stderr.String(), stdout.String())
		}
		if !strings.Contains(stdout.String(), "bead-4189") {
			t.Fatalf("stdout = %q, want it to surface bead-4189", stdout.String())
		}
	})
}
