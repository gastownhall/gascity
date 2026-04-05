package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
)

// cityDoltConfigs stores per-city Dolt configuration keyed by cityPath.
// Registered by startBeadsLifecycle so env builders and isExternalDolt can
// read city-scoped config without relying on process-global env vars (which
// break supervisor multi-tenancy where multiple cities share one process).
var cityDoltConfigs sync.Map // cityPath → config.DoltConfig

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
//   start         — start the backing service
//   init          — initialize beads in a directory
//   health        — check provider health
//   stop          — stop the backing service

// startBeadsLifecycle runs the full bead store startup sequence:
// start → init+hooks(city) → init+hooks(each rig) → regenerate routes.
// Called by gc start and controller config reload. Rigs must have absolute
// paths before calling (resolve relative paths first).
func startBeadsLifecycle(cityPath, _ string, cfg *config.City, stderr io.Writer) error {
	// Register per-city dolt config so env builders and isExternalDolt can
	// read it without process-global env vars. This is the single
	// registration point — supervisor, standalone, and reload all flow
	// through here. Always write (or clear) to handle config reload:
	// removing [dolt] after a reload must not leave stale entries.
	if cfg.Dolt.Host != "" || cfg.Dolt.Port != 0 {
		cityDoltConfigs.Store(cityPath, cfg.Dolt)
	} else {
		cityDoltConfigs.Delete(cityPath)
	}
	// Skip local Dolt startup when an external host is configured AND
	// the provider is the built-in bd backend. Custom exec providers
	// are unaffected — their start operation runs regardless of Dolt config.
	skipLocalDolt := isExternalDolt(cityPath) && rawBeadsProvider(cityPath) == "bd"
	if !skipLocalDolt {
		if err := ensureBeadsProvider(cityPath); err != nil {
			return fmt.Errorf("bead store: %w", err)
		}
	}
	// Propagate the actual dolt port to the process environment so
	// passthroughEnv() includes it for all agent sessions.
	readDoltPort(cityPath)
	beadsPrefix := config.EffectiveHQPrefix(cfg)
	if err := initAndHookDir(cityPath, cityPath, beadsPrefix); err != nil {
		return fmt.Errorf("init city beads: %w", err)
	}
	for i := range cfg.Rigs {
		prefix := cfg.Rigs[i].EffectivePrefix()
		if err := initAndHookDir(cityPath, cfg.Rigs[i].Path, prefix); err != nil {
			return fmt.Errorf("init rig %q beads: %w", cfg.Rigs[i].Name, err)
		}
	}
	syncConfiguredDoltPortFiles(cityPath, cfg.Rigs)
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
	if rawBeadsProvider(cityPath) == "bd" {
		if os.Getenv("GC_DOLT") == "skip" {
			seedDeferredManagedBeads(dir, prefix)
			return true, nil
		}
		if err := ensureBeadsProvider(cityPath); err != nil {
			return false, fmt.Errorf("bead store: %w", err)
		}
		if err := initAndHookDir(cityPath, dir, prefix); err != nil {
			return false, err
		}
		return false, nil
	}

	provider := beadsProvider(cityPath)
	if provider == "" {
		seedDeferredManagedBeads(dir, prefix)
		return true, nil
	}
	// For exec: providers, probe to check if the backing service is available.
	// If not available (exit 2 or error), defer initialization to gc start.
	if strings.HasPrefix(provider, "exec:") {
		script := strings.TrimPrefix(provider, "exec:")
		if !runProviderProbe(script, cityPath) {
			if rawBeadsProvider(cityPath) == "bd" {
				seedDeferredManagedBeads(dir, prefix)
			}
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

func seedDeferredManagedBeads(dir, prefix string) {
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(prefix) == "" {
		return
	}
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		return
	}
	ensureDeferredManagedConfig(filepath.Join(beadsDir, "config.yaml"), prefix)
	ensureDeferredManagedMetadata(filepath.Join(beadsDir, "metadata.json"), prefix)
}

func ensureDeferredManagedConfig(path, prefix string) {
	const autoStartLine = "dolt.auto-start: false"

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return
		}
		contents := strings.Join([]string{
			"issue_prefix: " + prefix,
			"issue-prefix: " + prefix,
			autoStartLine,
			"",
		}, "\n")
		_ = os.WriteFile(path, []byte(contents), 0o644)
		return
	}

	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines)+3)
	var sawIssuePrefix, sawIssuePrefixDash, sawAutoStart bool
	changed := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "issue_prefix:"):
			sawIssuePrefix = true
			if trimmed != "issue_prefix: "+prefix {
				line = "issue_prefix: " + prefix
				changed = true
			}
		case strings.HasPrefix(trimmed, "issue-prefix:"):
			sawIssuePrefixDash = true
			if trimmed != "issue-prefix: "+prefix {
				line = "issue-prefix: " + prefix
				changed = true
			}
		case strings.HasPrefix(trimmed, "dolt.auto-start:"):
			sawAutoStart = true
			if trimmed != autoStartLine {
				line = autoStartLine
				changed = true
			}
		}
		out = append(out, line)
	}

	if !sawIssuePrefix {
		out = append(out, "issue_prefix: "+prefix)
		changed = true
	}
	if !sawIssuePrefixDash {
		out = append(out, "issue-prefix: "+prefix)
		changed = true
	}
	if !sawAutoStart {
		out = append(out, autoStartLine)
		changed = true
	}

	if !changed {
		return
	}
	if len(out) == 0 || strings.TrimSpace(out[len(out)-1]) != "" {
		out = append(out, "")
	}
	_ = os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644)
}

func ensureDeferredManagedMetadata(path, prefix string) {
	defaults := map[string]any{
		"backend":       "dolt",
		"database":      "dolt",
		"dolt_database": prefix,
		"dolt_mode":     "server",
	}

	var meta map[string]any
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if json.Unmarshal(data, &meta) != nil {
			meta = map[string]any{}
		}
	case os.IsNotExist(err):
		meta = map[string]any{}
	default:
		return
	}

	changed := false
	for key, value := range defaults {
		if existing, ok := meta[key]; !ok || strings.TrimSpace(fmt.Sprint(existing)) == "" {
			meta[key] = value
			changed = true
		}
	}
	if !changed && err == nil {
		return
	}

	out, marshalErr := json.MarshalIndent(meta, "", "  ")
	if marshalErr != nil {
		return
	}
	out = append(out, '\n')
	_ = os.WriteFile(path, out, 0o644)
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
// For exec providers, fires "start". For file providers, always available.
func ensureBeadsProvider(cityPath string) error {
	provider := beadsProvider(cityPath)
	if strings.HasPrefix(provider, "exec:") {
		script := strings.TrimPrefix(provider, "exec:")
		return runProviderOp(script, cityPath, "start")
	}
	return nil
}

// shutdownBeadsProvider stops the bead store's backing service.
// Called by gc stop after agents have been terminated.
// For exec providers, fires "stop". For file providers, always available.
func shutdownBeadsProvider(cityPath string) error {
	provider := beadsProvider(cityPath)
	if strings.HasPrefix(provider, "exec:") {
		script := strings.TrimPrefix(provider, "exec:")
		return runProviderOp(script, cityPath, "stop")
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

// isExternalDolt returns true when the city uses an explicitly configured
// (user-managed) Dolt server rather than the managed local one.
//
// Checks per-city config first (registered by startBeadsLifecycle), then
// falls back to env vars for non-controller paths. With config, any
// explicit host or port means "user-managed" regardless of whether the
// host resolves to localhost. Without config, the env-var fallback
// excludes localhost addresses for backwards compatibility.
func isExternalDolt(cityPath string) bool {
	// Per-city config: explicit host or port means user-managed.
	if v, ok := cityDoltConfigs.Load(cityPath); ok {
		dc := v.(config.DoltConfig)
		if dc.Host != "" || dc.Port != 0 {
			return true
		}
	}
	// Env-only fallback: non-empty, non-local host.
	host := os.Getenv("GC_DOLT_HOST")
	return host != "" && host != "localhost" && host != "127.0.0.1" && host != "0.0.0.0"
}

// doltHostForCity returns the effective Dolt host for a city.
// User env vars take precedence over per-city config.
func doltHostForCity(cityPath string) string {
	if h := os.Getenv("GC_DOLT_HOST"); h != "" {
		return h
	}
	if v, ok := cityDoltConfigs.Load(cityPath); ok {
		return v.(config.DoltConfig).Host
	}
	return ""
}

// doltPortForCity returns the effective Dolt port for a city.
// User env vars take precedence over per-city config.
func doltPortForCity(cityPath string) string {
	if p := os.Getenv("GC_DOLT_PORT"); p != "" {
		return p
	}
	if v, ok := cityDoltConfigs.Load(cityPath); ok {
		dc := v.(config.DoltConfig)
		if dc.Port != 0 {
			return strconv.Itoa(dc.Port)
		}
	}
	return ""
}

// readDoltPort reads the dolt server port from the port file and sets
// GC_DOLT_PORT in the process environment. This ensures passthroughEnv()
// propagates the ephemeral port to all agent sessions.
// No-op if GC_DOLT_PORT is already set.
//
// Guard: in test binaries, if GC_DOLT_PORT is not explicitly set and
// GC_DOLT != "skip", this is a no-op to avoid probing the host for a
// running dolt server.
//
// When an external Dolt host is configured, the port from config (or env)
// is preserved — local state files are not consulted.
func readDoltPort(cityPath string) {
	if isTestBinary() && os.Getenv("GC_DOLT_PORT") == "" && os.Getenv("GC_DOLT") != "skip" {
		return // During tests, never probe the host for a running dolt server.
	}
	// External host: port comes from config or user env.
	// Don't overwrite with local state files.
	if isExternalDolt(cityPath) {
		if host := doltHostForCity(cityPath); host != "" {
			_ = os.Setenv("BEADS_DOLT_HOST", host)
		} else {
			_ = os.Unsetenv("BEADS_DOLT_HOST")
		}
		if port := doltPortForCity(cityPath); port != "" {
			_ = os.Setenv("GC_DOLT_PORT", port)
			_ = os.Setenv("BEADS_DOLT_PORT", port)
		} else {
			_ = os.Unsetenv("GC_DOLT_PORT")
			_ = os.Unsetenv("BEADS_DOLT_PORT")
		}
		return
	}
	if port := currentDoltPort(cityPath); port != "" {
		_ = os.Setenv("GC_DOLT_PORT", port)
		_ = os.Setenv("BEADS_DOLT_PORT", port)
		_ = os.Unsetenv("BEADS_DOLT_HOST")
		return
	}
	// When auto-start is disabled, propagate the stale port so bd subprocess
	// calls fail fast with "connection refused" rather than auto-starting an
	// embedded dolt on a random port and overwriting the shared port file.
	if doltAutoStartDisabled(cityPath) {
		if stalePort := staleDoltPort(cityPath); stalePort != "" {
			_ = os.Setenv("GC_DOLT_PORT", stalePort)
			_ = os.Setenv("BEADS_DOLT_PORT", stalePort)
			_ = os.Unsetenv("BEADS_DOLT_HOST")
			return
		}
	}
	_ = os.Unsetenv("GC_DOLT_PORT")
	_ = os.Unsetenv("BEADS_DOLT_PORT")
	_ = os.Unsetenv("BEADS_DOLT_HOST")
}

type doltRuntimeState struct {
	Running   bool   `json:"running"`
	PID       int    `json:"pid"`
	Port      int    `json:"port"`
	DataDir   string `json:"data_dir"`
	StartedAt string `json:"started_at"`
}

// doltAutoStartDisabled returns true when the .beads/config.yaml in the
// given directory contains "dolt.auto-start: false". When true, the system
// must never auto-start a dolt server or overwrite the port file.
func doltAutoStartDisabled(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, ".beads", "config.yaml"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "dolt.auto-start:") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "dolt.auto-start:"))
			return val == "false"
		}
	}
	return false
}

// staleDoltPort reads the dolt-server.port file without checking
// reachability. Returns the port string or "" if no file exists.
func staleDoltPort(cityPath string) string {
	portFile := filepath.Join(cityPath, ".beads", "dolt-server.port")
	data, err := os.ReadFile(portFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// currentDoltPort returns the controller-managed Dolt port for the city.
// Prefer the runtime state file under .gc/runtime because .beads/dolt-server.port
// may be stale or missing in rig directories after restarts. Falls back to the
// legacy city root port file for compatibility.
//
// When dolt.auto-start is disabled, stale port files are preserved to prevent
// bd from auto-starting an embedded server on a random port (which overwrites
// the shared port file and corrupts the managed server setup).
func currentDoltPort(cityPath string) string {
	if port := currentManagedDoltPort(cityPath); port != "" {
		writeDoltPortFile(cityPath, port)
		return port
	}
	noAutoStart := doltAutoStartDisabled(cityPath)
	if hasManagedDoltState(cityPath) {
		if !noAutoStart {
			removeDoltPortFile(cityPath)
		}
		return ""
	}

	portFile := filepath.Join(cityPath, ".beads", "dolt-server.port")
	if data, err := os.ReadFile(portFile); err == nil {
		port := strings.TrimSpace(string(data))
		if port != "" && doltPortReachable(port) {
			return port
		}
		// When auto-start is disabled, preserve the port file so bd
		// doesn't fall back to spawning an embedded dolt server.
		if !noAutoStart {
			_ = os.Remove(portFile)
		}
	}
	return ""
}

func hasManagedDoltState(cityPath string) bool {
	statePaths, err := filepath.Glob(filepath.Join(cityPath, ".gc", "runtime", "packs", "*", "dolt-state.json"))
	return err == nil && len(statePaths) > 0
}

func currentManagedDoltPort(cityPath string) string {
	statePaths, err := filepath.Glob(filepath.Join(cityPath, ".gc", "runtime", "packs", "*", "dolt-state.json"))
	if err != nil {
		return ""
	}
	type candidate struct {
		path  string
		state doltRuntimeState
	}
	var candidates []candidate
	for _, statePath := range statePaths {
		data, err := os.ReadFile(statePath)
		if err != nil {
			continue
		}
		var state doltRuntimeState
		if json.Unmarshal(data, &state) != nil {
			continue
		}
		if !validDoltRuntimeState(state, cityPath) {
			continue
		}
		candidates = append(candidates, candidate{path: statePath, state: state})
	}
	if len(candidates) == 0 {
		return ""
	}
	best := candidates[0]
	bestTime := parseDoltStartedAt(best.state.StartedAt)
	for _, cand := range candidates[1:] {
		candTime := parseDoltStartedAt(cand.state.StartedAt)
		if candTime.After(bestTime) || (candTime.Equal(bestTime) && strings.Contains(cand.path, "/packs/dolt/")) {
			best = cand
			bestTime = candTime
		}
	}
	return strconv.Itoa(best.state.Port)
}

func parseDoltStartedAt(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return t
}

func validDoltRuntimeState(state doltRuntimeState, cityPath string) bool {
	if !state.Running || state.Port <= 0 || state.PID <= 0 {
		return false
	}
	expectedDataDir := filepath.Join(cityPath, ".beads", "dolt")
	if state.DataDir != "" && state.DataDir != expectedDataDir {
		return false
	}
	if !pidAlive(state.PID) {
		return false
	}
	if !doltPortReachable(strconv.Itoa(state.Port)) {
		return false
	}
	return true
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func doltPortReachable(port string) bool {
	if strings.TrimSpace(port) == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func writeDoltPortFile(dir, port string) {
	if dir == "" || port == "" {
		return
	}
	portFile := filepath.Join(dir, ".beads", "dolt-server.port")
	if data, err := os.ReadFile(portFile); err == nil && strings.TrimSpace(string(data)) == port {
		return
	}
	if err := os.MkdirAll(filepath.Dir(portFile), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(portFile, []byte(port+"\n"), 0o644)
}

func removeDoltPortFile(dir string) {
	if dir == "" {
		return
	}
	_ = os.Remove(filepath.Join(dir, ".beads", "dolt-server.port"))
}

func syncConfiguredDoltPortFiles(cityPath string, rigs []config.Rig) {
	port := currentDoltPort(cityPath)
	normalizeManagedDoltConfig(cityPath)
	if port != "" {
		writeDoltPortFile(cityPath, port)
	} else {
		removeDoltPortFile(cityPath)
	}
	for i := range rigs {
		// Skip rigs with their own Dolt server — don't overwrite their port.
		if rigs[i].DoltHost != "" || rigs[i].DoltPort != "" {
			continue
		}
		normalizeManagedDoltConfig(rigs[i].Path)
		if port != "" {
			writeDoltPortFile(rigs[i].Path, port)
		} else {
			removeDoltPortFile(rigs[i].Path)
		}
	}
}

func normalizeManagedDoltConfig(dir string) {
	configPath := filepath.Join(dir, ".beads", "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		return
	}
	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines)+1)
	sawAutoStart := false
	changed := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out = append(out, line)
			continue
		}
		key, _, ok := strings.Cut(trimmed, ":")
		if !ok {
			out = append(out, line)
			continue
		}
		key = strings.TrimSpace(key)
		switch key {
		case "dolt.port", "dolt_port", "dolt_server_port":
			changed = true
			continue
		case "dolt.auto-start":
			sawAutoStart = true
			if trimmed != "dolt.auto-start: false" {
				changed = true
			}
			out = append(out, "dolt.auto-start: false")
		default:
			out = append(out, line)
		}
	}
	if !sawAutoStart {
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "")
		}
		out = append(out, "dolt.auto-start: false")
		changed = true
	}
	if !changed {
		return
	}
	_ = os.WriteFile(configPath, []byte(strings.Join(out, "\n")), 0o644)
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
		cmd.Env = cityRuntimeProcessEnv(cityPath)
	}
	return cmd.Run() == nil
}

// providerOpTimeout returns the context timeout for a given lifecycle
// operation. The "start" and "recover" operations get a longer timeout
// because dolt server startup can take 30+ seconds for large data dirs.
// All other operations use 30s.
func providerOpTimeout(op string) time.Duration {
	switch op {
	case "start", "recover":
		return 120 * time.Second
	default:
		return 30 * time.Second
	}
}

// runProviderOp runs a lifecycle operation against an exec beads script.
// Exit 2 = not needed (treated as success, no-op). Used for start,
// init, health, recover, and stop operations.
// cityPath is set as GC_CITY_PATH in the subprocess environment so scripts
// can locate the city root.
func runProviderOp(script, cityPath string, args ...string) error {
	op := ""
	if len(args) > 0 {
		op = args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerOpTimeout(op))
	defer cancel()

	cmd := exec.CommandContext(ctx, script, args...)
	cmd.WaitDelay = 2 * time.Second
	if cityPath != "" {
		cmd.Env = cityRuntimeProcessEnv(cityPath)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			return nil // Not needed
		}
		// Detect missing script or missing dolt binary.
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("exec beads %s: provider script not found (%s); run \"gc doctor\" for diagnostics", args[0], script)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("exec beads %s: %s", args[0], msg)
	}
	return nil
}
