package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Opt-in proxied-server overlay (gastownhall/gascity #1978).
//
// gascity normally runs bd in direct ServerMode and re-asserts
// dolt_mode="server" on every reconcile (ensureCanonicalScopeMetadata). When a
// city opts in with [beads] proxied=true AND the resolved bd supports it, this
// overlay re-flips each scope to dolt_mode="proxied-server" and writes
// proxied_server_client_info.json with an external block pointing at the managed
// dolt server, so bd routes through the connection-pooling db-proxy. When the
// city does not opt in (or bd lacks support) every path here is a no-op or a
// clean revert, so existing cities and standard-bd deployments are unaffected.

const (
	proxiedServerClientInfoFile = "proxied_server_client_info.json"
	doltModeProxiedServer       = "proxied-server"
)

// proxiedServerClientInfo mirrors the subset of beads'
// configfile.ProxiedServerClientInfo that gascity writes. gascity cannot import
// beads' internal/configfile, so this small mirror serializes the same JSON
// schema (.beads/proxied_server_client_info.json) that bd reads to learn which
// external dolt sql-server the proxy should front.
type proxiedServerClientInfo struct {
	External *proxiedServerExternal `json:"external,omitempty"`
}

type proxiedServerExternal struct {
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	User string `json:"user,omitempty"`
}

// bdSupportsProxiedServer reports whether the resolved bd binary supports
// `bd init --proxied-server` (external). Probed once and memoized: a city that
// opts into proxied mode but is paired with a standard bd must fall back to
// server mode rather than break init. Override the probe in tests via
// bdProxiedProbeOverride.
var (
	bdProxiedProbeOnce     sync.Once
	bdProxiedProbeOK       bool
	bdProxiedProbeOverride *bool
)

func bdSupportsProxiedServer() bool {
	if bdProxiedProbeOverride != nil {
		return *bdProxiedProbeOverride
	}
	bdProxiedProbeOnce.Do(func() {
		bin := strings.TrimSpace(os.Getenv("BD_BIN"))
		if bin == "" {
			bin = "bd"
		}
		out, err := exec.Command(bin, "init", "--help").CombinedOutput() //nolint:gosec // bin is the operator-configured bd
		if err != nil {
			bdProxiedProbeOK = false
			return
		}
		bdProxiedProbeOK = strings.Contains(string(out), "proxied-server-external")
	})
	return bdProxiedProbeOK
}

// proxiedServerScopeActive reports the EFFECTIVE proxied state: the city opted
// in AND the resolved bd supports it. False keeps direct ServerMode.
func proxiedServerScopeActive(cfg *config.City) bool {
	return cfg != nil && cfg.Beads.ProxiedEnabled() && bdSupportsProxiedServer()
}

// applyProxiedPoolEnv injects the connection-pool env into a bd invocation's
// environment when the city opts into proxied mode and bd supports it. The keys
// are read by gc-beads-bd.sh (GC_BEADS_PROXIED / GC_BEADS_PROXY_POOL_SIZE) and
// by Go-launched bd directly (BEADS_PROXY_POOL_SIZE). No-op when not opted in,
// so server, doltlite, and postgres scopes are unaffected.
func applyProxiedPoolEnv(env map[string]string, cityPath string) {
	if env == nil {
		return
	}
	active, n, idle := resolveProxiedGate(cityPath)
	if !active {
		return
	}
	env["GC_BEADS_PROXIED"] = "1"
	env["GC_BEADS_PROXY_POOL_SIZE"] = n
	env["BEADS_PROXY_POOL_SIZE"] = n
	// Keep proxies warm across sparse controller probes (kills respawn churn).
	env["GC_BEADS_PROXY_IDLE_TIMEOUT"] = idle
	env["BEADS_PROXY_IDLE_TIMEOUT"] = idle
}

// proxiedGateCache memoizes the [beads] proxied gate per city, keyed by
// city.toml's mtime. applyProxiedPoolEnv runs on every bd-command env build (hot
// under active agents); resolving it through the full loadCityConfig forced a
// pack-DAG expansion per invocation. The gate fields (proxied / proxy_pool_size
// / proxy_idle_timeout) are operator-set in city.toml, so a cheap TOML parse —
// cached until city.toml changes — suffices and skips the per-command expansion.
var proxiedGateCache sync.Map // cityPath -> proxiedGateEntry

type proxiedGateEntry struct {
	mtime    int64
	active   bool
	poolSize string
	idle     string
}

// resolveProxiedGate reports whether the city opts into proxied mode (and bd
// supports it), plus the pool size and idle timeout, parsing only city.toml's
// [beads] section and memoizing by city.toml mtime.
func resolveProxiedGate(cityPath string) (active bool, poolSize, idle string) {
	tomlPath := filepath.Join(cityPath, "city.toml")
	fi, err := os.Stat(tomlPath)
	if err != nil {
		return false, "", ""
	}
	mt := fi.ModTime().UnixNano()
	if v, ok := proxiedGateCache.Load(cityPath); ok {
		if e := v.(proxiedGateEntry); e.mtime == mt {
			return e.active, e.poolSize, e.idle
		}
	}
	entry := proxiedGateEntry{mtime: mt}
	if data, readErr := os.ReadFile(tomlPath); readErr == nil {
		if cfg, parseErr := config.Parse(data); parseErr == nil && proxiedServerScopeActive(cfg) {
			entry.active = true
			entry.poolSize = strconv.Itoa(cfg.Beads.ProxyPoolSizeOrDefault())
			entry.idle = cfg.Beads.ProxyIdleTimeoutOrDefault()
		}
	}
	proxiedGateCache.Store(cityPath, entry)
	return entry.active, entry.poolSize, entry.idle
}

// applyProxiedServerScopeOverlay reconciles a scope's proxied-server state.
// active=true flips canonical metadata.json DoltMode to "proxied-server" and
// writes proxied_server_client_info.json pointing at the scope's resolved dolt
// target; active=false removes any stale client_info (metadata is already
// "server" from ensureCanonicalScopeMetadata). Idempotent — safe on every
// reconcile and for the on→off revert.
//
//nolint:unparam // fs is threaded for testability/parity with the contract helpers; production callers pass fsys.OSFS{}.
func applyProxiedServerScopeOverlay(fs fsys.FS, cityRoot, scopeRoot string, active bool) error {
	beadsDir := filepath.Join(scopeRoot, ".beads")
	clientInfoPath := filepath.Join(beadsDir, proxiedServerClientInfoFile)

	if !active {
		if err := fs.Remove(clientInfoPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing stale %s: %w", clientInfoPath, err)
		}
		return nil
	}

	target, err := contract.ResolveDoltConnectionTarget(fs, cityRoot, scopeRoot)
	if err != nil {
		return fmt.Errorf("resolving dolt target for proxied overlay %s: %w", scopeRoot, err)
	}

	metaPath := filepath.Join(beadsDir, "metadata.json")
	state, ok, err := contract.LoadMetadataState(fs, metaPath)
	if err != nil {
		return fmt.Errorf("loading metadata for proxied overlay %s: %w", scopeRoot, err)
	}
	if !ok {
		// ensureCanonicalScopeMetadata writes metadata first; nothing to flip yet.
		return nil
	}
	if state.DoltMode != doltModeProxiedServer {
		state.DoltMode = doltModeProxiedServer
		if _, err := contract.EnsureCanonicalMetadata(fs, metaPath, state); err != nil {
			return fmt.Errorf("writing proxied-server metadata %s: %w", scopeRoot, err)
		}
	}

	port, _ := strconv.Atoi(strings.TrimSpace(target.Port))
	info := proxiedServerClientInfo{External: &proxiedServerExternal{
		Host: strings.TrimSpace(target.Host),
		Port: port,
		User: strings.TrimSpace(target.User),
	}}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := ensureBeadsDir(fs, beadsDir); err != nil {
		return err
	}
	if err := fsys.WriteFileAtomic(fs, clientInfoPath, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", clientInfoPath, err)
	}
	return nil
}
