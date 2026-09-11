package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// bd's proxy root layout, as written by `bd init --proxied-server`:
// <root>/{config.yaml,proxy.pid,...}, where root defaults to
// <scope>/.beads/dolt but follows BEADS_PROXIED_SERVER_ROOT_PATH or the
// sidecar's root_path when either is set. The sql-server child is launched
// with --config pointing at that config.yaml, and proxy.pid is the proxy's own
// liveness record.
const (
	bdProxyConfigFileName = "config.yaml"
	bdProxyPIDFileName    = "proxy.pid"
	bdProxyRecordKind     = "db-proxy"
)

// bdProxyPIDAlive is the liveness probe for a proxy.pid record. Injectable so
// reaper tests can drive both branches from a fabricated process table.
var bdProxyPIDAlive = pidutil.Alive

// bdProxyPIDRecord is the subset of bd's proxy.pid JSON gc needs to prove
// ownership. The full record also carries upstream_id, birth, root_id and
// control_port; gc reads none of those.
type bdProxyPIDRecord struct {
	PID  int    `json:"pid"`
	Port int    `json:"port"`
	Kind string `json:"kind"`
}

// bdOwnedProxyDoltConfig reports whether configPath is the sql-server config
// of a bd proxy root whose proxy.pid names a live db-proxy process, and
// returns that proxy's PID. A live proxy.pid is proof that bd — not gc — owns
// the sql-server child, so the reaper must never kill it. This holds under
// test temp directories too: a real-bd lifecycle test running in t.TempDir()
// produces exactly this shape, and a concurrent cleanup used to reap it
// through the test-config-path allowlist.
//
// Ownership is proven by the live proxy.pid record beside the config, not by
// where the root sits: bd resolves the root from
// BEADS_PROXIED_SERVER_ROOT_PATH, then the sidecar's root_path, and only then
// the default <scope>/.beads/dolt, so a directory-name check would unprotect
// every scope with an overridden root. Uncovered: a config_path override that
// puts config.yaml somewhere other than the proxy root — bd allows it, and gc
// then sees no proxy.pid sibling and falls through to the ordinary rules.
func bdOwnedProxyDoltConfig(configPath string) (int, bool) {
	if configPath == "" || filepath.Base(configPath) != bdProxyConfigFileName {
		return 0, false
	}
	root := filepath.Dir(filepath.Clean(configPath))
	data, err := os.ReadFile(filepath.Join(root, bdProxyPIDFileName))
	if err != nil {
		return 0, false
	}
	var record bdProxyPIDRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return 0, false
	}
	if record.Kind != bdProxyRecordKind || record.PID <= 0 {
		return 0, false
	}
	if !bdProxyPIDAlive(record.PID) {
		return 0, false
	}
	return record.PID, true
}

// looksLikeBdDBProxyChild reports whether argv is bd's own proxy supervisor.
// Process discovery only enumerates `dolt sql-server` today, so this is a
// standing guard rather than a live path: if discovery ever widens, the
// supervisor must not become a reap candidate.
func looksLikeBdDBProxyChild(argv []string) bool {
	return len(argv) >= 2 && filepath.Base(argv[0]) == "bd" && argv[1] == "db-proxy-child"
}
