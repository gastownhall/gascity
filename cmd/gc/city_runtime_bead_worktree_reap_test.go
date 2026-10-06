package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// newReapTickRuntime builds a minimal standalone CityRuntime wired to the given
// city path, rig store, and daemon config, suitable for exercising the
// worktree-reap phase of a single tick.
func newReapTickRuntime(cityPath string, cfg *config.City, rigStore beads.Store, stderr io.Writer) *CityRuntime {
	return &CityRuntime{
		cityPath:            cityPath,
		cityName:            "test",
		cfg:                 cfg,
		sp:                  runtime.NewFake(),
		standaloneCityStore: beads.NewMemStore(),
		standaloneRigStores: map[string]beads.Store{"mrig": rigStore},
		wg:                  fixedWispGC{},
		rec:                 events.Discard,
		logPrefix:           "test",
		stdout:              io.Discard,
		stderr:              stderr,
		buildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	}
}

func runReapTick(t *testing.T, cr *CityRuntime) {
	t.Helper()
	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "test")
}

// TestCityRuntimeTick_SkipsClosedBeadWorktreeReapWhenDisabled verifies that the
// runtime tick does not invoke the worktree reaper when both the reap and
// dry-run daemon flags are false: the nested worktree survives untouched.
func TestCityRuntimeTick_SkipsClosedBeadWorktreeReapWhenDisabled(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-abc123")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)

	cfg := reapTestConfig(rigRoot)
	disabled := false
	cfg.Daemon = config.DaemonConfig{AutoReapClosedBeadWorktrees: &disabled}

	var stderr bytes.Buffer
	runReapTick(t, newReapTickRuntime(cityPath, cfg, store, &stderr))

	if _, err := os.Stat(wt); os.IsNotExist(err) {
		t.Error("worktree was removed, want it to survive when auto_reap_closed_bead_worktrees is false")
	}
	if s := stderr.String(); strings.Contains(s, "reapClosedBeadWorktrees") {
		t.Errorf("stderr = %q, want no reaper output when disabled", s)
	}
}

// TestCityRuntimeTick_ReapsClosedBeadWorktreeWhenEnabled verifies the tick
// invokes the reaper for real when AutoReapClosedBeadWorktrees is true: an
// idle, clean, closed-bead nested worktree is removed.
func TestCityRuntimeTick_ReapsClosedBeadWorktreeWhenEnabled(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-abc123")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)

	cfg := reapTestConfig(rigRoot)
	enabled := true
	cfg.Daemon = config.DaemonConfig{AutoReapClosedBeadWorktrees: &enabled}
	injectLiveness(t, liveWorktreeState{scanned: true}) // idle: nothing live

	var stderr bytes.Buffer
	runReapTick(t, newReapTickRuntime(cityPath, cfg, store, &stderr))

	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree %s survived, want it reaped when enabled (stat err=%v)\nstderr:\n%s", wt, err, stderr.String())
	}
}

// TestCityRuntimeTick_DryRunReapDeletesNothing verifies that enabling only the
// dry-run flag runs the reaper in report-only mode: the would-reap worktree is
// logged but remains on disk.
func TestCityRuntimeTick_DryRunReapDeletesNothing(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-abc123")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)

	cfg := reapTestConfig(rigRoot)
	dryRun := true
	cfg.Daemon = config.DaemonConfig{AutoReapClosedBeadWorktreesDryRun: &dryRun}
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	runReapTick(t, newReapTickRuntime(cityPath, cfg, store, &stderr))

	if _, err := os.Stat(wt); err != nil {
		t.Errorf("dry-run tick removed or broke worktree %s: %v", wt, err)
	}
	if !strings.Contains(stderr.String(), "dry-run: would reap") {
		t.Errorf("stderr = %q, want a dry-run would-reap line", stderr.String())
	}
}
