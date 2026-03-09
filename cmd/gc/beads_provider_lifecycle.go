package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
)

// ── Consolidated lifecycle operations ────────────────────────────────────
//
// The bead store lifecycle has a strict ordering:
//
//   start → [init + hooks]* → (agents run) → health* → stop
//
// These high-level functions enforce that ordering so call sites don't
// need to know the sequence. Use these instead of calling the low-level
// functions (ensureBeadsProvider, initBeadsForDir, installBeadHooks)
// directly.
//
// Exec provider protocol operations:
//   ensure-ready  — legacy start (still called for backward compat)
//   start         — enhanced start with backoff/health tracking
//   init          — initialize beads in a directory
//   health        — check provider health
//   stop          — enhanced stop with graceful shutdown
//   shutdown      — legacy stop (still called for backward compat)

// startBeadsLifecycle runs the full bead store startup sequence:
// ensure-ready → init+hooks(city) → init+hooks(each rig) → regenerate routes.
// Called by gc start and controller config reload. Rigs must have absolute
// paths before calling (resolve relative paths first).
func startBeadsLifecycle(cityPath, cityName string, cfg *config.City, stderr io.Writer) error {
	if err := ensureBeadsProvider(cityPath); err != nil {
		return fmt.Errorf("bead store: %w", err)
	}
	beadsPrefix := config.DeriveBeadsPrefix(cityName)
	if err := initAndHookDir(cityPath, cityPath, beadsPrefix); err != nil {
		return fmt.Errorf("init city beads: %w", err)
	}
	for i := range cfg.Rigs {
		prefix := cfg.Rigs[i].EffectivePrefix()
		if err := initAndHookDir(cityPath, cfg.Rigs[i].Path, prefix); err != nil {
			return fmt.Errorf("init rig %q beads: %w", cfg.Rigs[i].Name, err)
		}
	}
	// Install agent hooks (Claude, Gemini, etc.) for city and all rigs.
	// Idempotent — safe to run on every start. Non-fatal but logged.
	if ih := cfg.Workspace.InstallAgentHooks; len(ih) > 0 {
		if err := hooks.Install(fsys.OSFS{}, cityPath, cityPath, ih); err != nil {
			fmt.Fprintf(stderr, "beads lifecycle: installing agent hooks for city: %v\n", err) //nolint:errcheck // best-effort stderr
		}
		for i := range cfg.Rigs {
			if err := hooks.Install(fsys.OSFS{}, cityPath, cfg.Rigs[i].Path, ih); err != nil {
				fmt.Fprintf(stderr, "beads lifecycle: installing agent hooks for rig %q: %v\n", cfg.Rigs[i].Name, err) //nolint:errcheck // best-effort stderr
			}
		}
	}
	// Regenerate routes for cross-rig routing.
	if len(cfg.Rigs) > 0 {
		allRigs := collectRigRoutes(cityPath, cfg)
		if err := writeAllRoutes(allRigs); err != nil {
			return fmt.Errorf("writing routes: %w", err)
		}
	}
	return nil
}

// initDirIfReady initializes beads for a single directory, ensuring the
// backing service is ready first. For the bd provider, this is a no-op
// (Dolt isn't running until gc start). Used by gc init and gc rig add.
//
// Returns (deferred bool, err). deferred=true means the bd provider
// skipped init — the caller should tell the user it's deferred to gc start.
func initDirIfReady(cityPath, dir, prefix string) (deferred bool, err error) {
	provider := beadsProvider(cityPath)
	if provider == "" {
		return true, nil
	}
	// For exec: providers, probe to check if the backing service is available.
	// If not available (exit 2 or error), defer initialization to gc start.
	if strings.HasPrefix(provider, "exec:") {
		script := strings.TrimPrefix(provider, "exec:")
		if !runProviderProbe(script, cityPath) {
			return true, nil // Not running — defer to gc start.
		}
	}
	if err := ensureBeadsProvider(cityPath); err != nil {
		return false, fmt.Errorf("bead store: %w", err)
	}
	if err := initAndHookDir(cityPath, dir, prefix); err != nil {
		return false, err
	}
	return false, nil
}

// initAndHookDir is the atomic unit of bead store initialization:
// init the directory, then install event hooks. The ordering matters
// because init (bd init) may recreate .beads/ and wipe existing hooks.
func initAndHookDir(cityPath, dir, prefix string) error {
	if err := initBeadsForDir(cityPath, dir, prefix); err != nil {
		return err
	}
	// Non-fatal: hooks are convenience (event forwarding), not critical.
	if err := installBeadHooks(dir); err != nil {
		return fmt.Errorf("install hooks at %s: %w", dir, err)
	}
	return nil
}

// resolveRigPaths resolves relative rig paths to absolute (relative to
// cityPath). Mutates cfg.Rigs in place. Called before any function that
// uses rig paths.
func resolveRigPaths(cityPath string, rigs []config.Rig) {
	for i := range rigs {
		if !filepath.IsAbs(rigs[i].Path) {
			rigs[i].Path = filepath.Join(cityPath, rigs[i].Path)
		}
	}
}

// ── Low-level provider operations ────────────────────────────────────────
//
// These are the building blocks. Prefer the consolidated functions above
// for new call sites. These remain exported for tests that need to verify
// individual operations.

// ensureBeadsProvider starts the bead store's backing service if needed.
// For exec providers, fires "start" (enhanced) then "ensure-ready" (legacy).
// Providers that don't implement "start" return exit 2 (no-op), and we
// fall through to "ensure-ready" for backward compatibility.
func ensureBeadsProvider(cityPath string) error {
	provider := beadsProvider(cityPath)
	if strings.HasPrefix(provider, "exec:") {
		script := strings.TrimPrefix(provider, "exec:")
		// Fire start first (enhanced lifecycle hook).
		_ = runProviderOp(script, cityPath, "start")
		// Always fire ensure-ready for backward compat.
		return runProviderOp(script, cityPath, "ensure-ready")
	}
	return nil // file: always available
}

// shutdownBeadsProvider stops the bead store's backing service.
// Called by gc stop after agents have been terminated.
// For exec providers, fires "stop" (enhanced) then "shutdown" (legacy).
func shutdownBeadsProvider(cityPath string) error {
	provider := beadsProvider(cityPath)
	if strings.HasPrefix(provider, "exec:") {
		script := strings.TrimPrefix(provider, "exec:")
		// Fire stop first (enhanced lifecycle hook).
		_ = runProviderOp(script, cityPath, "stop")
		// Always fire shutdown for backward compat.
		return runProviderOp(script, cityPath, "shutdown")
	}
	return nil
}

// initBeadsForDir initializes bead store infrastructure in a directory.
// Idempotent — skips if already initialized. Callers should use
// initAndHookDir instead to ensure hooks are installed afterward.
func initBeadsForDir(cityPath, dir, prefix string) error {
	provider := beadsProvider(cityPath)
	if strings.HasPrefix(provider, "exec:") {
		return runProviderOp(strings.TrimPrefix(provider, "exec:"), cityPath, "init", dir, prefix)
	}
	return nil
}

// healthBeadsProvider checks the bead store's backing service health.
// For exec providers, fires the "health" operation. For bd (dolt), runs
// a three-layer health check and attempts recovery on failure. For file
// provider, always healthy (no-op).
func healthBeadsProvider(cityPath string) error {
	provider := beadsProvider(cityPath)
	if strings.HasPrefix(provider, "exec:") {
		script := strings.TrimPrefix(provider, "exec:")
		if err := runProviderOp(script, cityPath, "health"); err != nil {
			if recErr := runProviderOp(script, cityPath, "recover"); recErr != nil {
				return fmt.Errorf("unhealthy (%w) and recovery failed: %w", err, recErr)
			}
		}
		return nil
	}
	return nil // file: always healthy
}

// runProviderProbe runs a "probe" operation against an exec beads script.
// Returns true if the backing service is available (exit 0), false if not
// available (exit 2) or on any error. Unlike runProviderOp, exit 2 means
// "not running" rather than "not needed."
func runProviderProbe(script, cityPath string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, script, "probe")
	cmd.WaitDelay = 2 * time.Second
	if cityPath != "" {
		cmd.Env = append(os.Environ(), "GC_CITY_PATH="+cityPath)
	}
	return cmd.Run() == nil
}

// runProviderOp runs a lifecycle operation against an exec beads script.
// Exit 2 = not needed (treated as success, no-op). Used for start,
// ensure-ready, init, health, recover, stop, and shutdown operations.
// cityPath is set as GC_CITY_PATH in the subprocess environment so scripts
// can locate the city root.
func runProviderOp(script, cityPath string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, script, args...)
	cmd.WaitDelay = 2 * time.Second
	if cityPath != "" {
		cmd.Env = append(os.Environ(), "GC_CITY_PATH="+cityPath)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			return nil // Not needed
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("exec beads %s: %s", args[0], msg)
	}
	return nil
}
