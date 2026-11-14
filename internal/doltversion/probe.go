package doltversion

import (
	"fmt"
	"os"
	"path/filepath"
)

// probeGlobalConfig keeps dolt off the network for a version probe.
// versioncheck.disabled stops the latest-release lookup that every
// `dolt version` otherwise makes as a synchronous HTTPS fetch with no timeout
// of its own; metrics.disabled stops the usage-metrics call.
const probeGlobalConfig = `{"versioncheck.disabled":"true","metrics.disabled":"true"}`

// IsolatedProbeEnv returns the environment entries for running `dolt version`
// without the user's dolt global config, and a cleanup that removes the
// throwaway config directory behind them. DOLT_ROOT_PATH points at a fresh
// directory whose global config disables the release check, and
// DOLT_DISABLE_EVENT_FLUSH stops dolt forking a detached `dolt send-metrics`
// that would outlive the probe. Append the entries to os.Environ(); later
// entries win.
//
// When the directory cannot be made the error is returned with a no-op
// cleanup, and the caller decides whether to probe unisolated.
func IsolatedProbeEnv() (env []string, cleanup func(), err error) {
	root, err := os.MkdirTemp("", "gc-dolt-probe-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("creating throwaway dolt root: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(root) }
	dir := filepath.Join(root, ".dolt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("creating dolt config dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "config_global.json")
	if err := os.WriteFile(path, []byte(probeGlobalConfig), 0o600); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("writing dolt probe config %s: %w", path, err)
	}
	return []string{"DOLT_ROOT_PATH=" + root, "DOLT_DISABLE_EVENT_FLUSH=1"}, cleanup, nil
}
