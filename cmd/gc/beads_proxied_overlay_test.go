package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

// setProxiedProbe overrides the memoized bd capability probe for the duration
// of a test, so tests never shell out to a real bd binary.
func setProxiedProbe(t *testing.T, supported bool) {
	t.Helper()
	prev := bdProxiedProbeOverride
	bdProxiedProbeOverride = &supported
	t.Cleanup(func() { bdProxiedProbeOverride = prev })
}

// seedCanonicalDoltScope writes a city-canonical .beads scope (config.yaml +
// metadata.json with dolt_mode=server) pointing at an external dolt target,
// mirroring what ensureCanonicalScopeMetadata produces.
func seedCanonicalDoltScope(t *testing.T, root, host, port, user string) {
	t.Helper()
	beadsDir := filepath.Join(root, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := strings.Join([]string{
		"issue_prefix: hq",
		"gc.endpoint_origin: city_canonical",
		"gc.endpoint_status: verified",
		"dolt.host: " + host,
		"dolt.port: " + port,
		"dolt.user: " + user,
		"dolt.auto-start: false",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(beadsDir, "metadata.json"), contract.MetadataState{
		Database: "beads",
		Backend:  "dolt",
		DoltMode: "server",
	}); err != nil {
		t.Fatalf("EnsureCanonicalMetadata: %v", err)
	}
}

func readDoltMode(t *testing.T, root string) string {
	t.Helper()
	state, ok, err := contract.LoadMetadataState(fsys.OSFS{}, filepath.Join(root, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("LoadMetadataState: %v", err)
	}
	if !ok {
		t.Fatal("metadata.json missing")
	}
	return state.DoltMode
}

func TestApplyProxiedServerScopeOverlayFlipsAndWritesClientInfo(t *testing.T) {
	root := t.TempDir()
	seedCanonicalDoltScope(t, root, "127.0.0.1", "42188", "root")
	clientInfo := filepath.Join(root, ".beads", proxiedServerClientInfoFile)

	if err := applyProxiedServerScopeOverlay(fsys.OSFS{}, root, root, true); err != nil {
		t.Fatalf("overlay active: %v", err)
	}
	if got := readDoltMode(t, root); got != doltModeProxiedServer {
		t.Fatalf("dolt_mode = %q, want %q", got, doltModeProxiedServer)
	}
	data, err := os.ReadFile(clientInfo)
	if err != nil {
		t.Fatalf("client_info not written: %v", err)
	}
	for _, want := range []string{`"127.0.0.1"`, "42188", `"root"`, `"external"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("client_info %s missing %q", data, want)
		}
	}
}

func TestApplyProxiedServerScopeOverlayInactiveRemovesClientInfo(t *testing.T) {
	root := t.TempDir()
	seedCanonicalDoltScope(t, root, "127.0.0.1", "42188", "root")
	clientInfo := filepath.Join(root, ".beads", proxiedServerClientInfoFile)

	// Flip on, then revert: client_info must be gone (clean rollback).
	if err := applyProxiedServerScopeOverlay(fsys.OSFS{}, root, root, true); err != nil {
		t.Fatalf("overlay active: %v", err)
	}
	if err := applyProxiedServerScopeOverlay(fsys.OSFS{}, root, root, false); err != nil {
		t.Fatalf("overlay inactive: %v", err)
	}
	if _, err := os.Stat(clientInfo); !os.IsNotExist(err) {
		t.Fatalf("client_info still present after revert (err=%v)", err)
	}
	// Inactive overlay must be a no-op even with no client_info present.
	if err := applyProxiedServerScopeOverlay(fsys.OSFS{}, root, root, false); err != nil {
		t.Fatalf("overlay inactive idempotent: %v", err)
	}
}

func writeProxiedCityTOML(t *testing.T, body string) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

func TestApplyProxiedPoolEnvInjectsOnlyWhenGateOnAndBDCapable(t *testing.T) {
	const tomlOn = "[workspace]\nname = \"t\"\n\n[beads]\nproxied = true\nproxy_pool_size = 6\n"
	const tomlOff = "[workspace]\nname = \"t\"\n\n[beads]\nproxied = false\n"

	t.Run("gate on + bd capable -> injected", func(t *testing.T) {
		setProxiedProbe(t, true)
		cityPath := writeProxiedCityTOML(t, tomlOn)
		env := map[string]string{}
		applyProxiedPoolEnv(env, cityPath)
		if env["GC_BEADS_PROXIED"] != "1" || env["GC_BEADS_PROXY_POOL_SIZE"] != "6" || env["BEADS_PROXY_POOL_SIZE"] != "6" {
			t.Fatalf("env = %v, want proxied=1 pool=6", env)
		}
		// Idle timeout defaults to keep proxies warm across sparse probes.
		if env["BEADS_PROXY_IDLE_TIMEOUT"] != "10m" || env["GC_BEADS_PROXY_IDLE_TIMEOUT"] != "10m" {
			t.Fatalf("env = %v, want idle_timeout=10m", env)
		}
	})

	t.Run("gate on + bd incapable -> fallback, no env", func(t *testing.T) {
		setProxiedProbe(t, false)
		cityPath := writeProxiedCityTOML(t, tomlOn)
		env := map[string]string{}
		applyProxiedPoolEnv(env, cityPath)
		if _, ok := env["GC_BEADS_PROXIED"]; ok {
			t.Fatalf("env injected despite incapable bd: %v", env)
		}
	})

	t.Run("gate off -> no env (byte-identical legacy)", func(t *testing.T) {
		setProxiedProbe(t, true)
		cityPath := writeProxiedCityTOML(t, tomlOff)
		env := map[string]string{}
		applyProxiedPoolEnv(env, cityPath)
		if len(env) != 0 {
			t.Fatalf("env injected with gate off: %v", env)
		}
	})
}

func TestBeadsProxiedCapabilityCheck(t *testing.T) {
	loadCfg := func(t *testing.T, body string) *config.City {
		t.Helper()
		cityPath := writeProxiedCityTOML(t, body)
		cfg, err := loadCityConfig(cityPath, os.Stderr)
		if err != nil {
			t.Fatalf("loadCityConfig: %v", err)
		}
		return cfg
	}

	t.Run("gate off -> OK", func(t *testing.T) {
		setProxiedProbe(t, false)
		cfg := loadCfg(t, "[workspace]\nname = \"t\"\n")
		r := newBeadsProxiedCapabilityCheck(cfg).Run(nil)
		if r.Status != doctor.StatusOK {
			t.Fatalf("status = %v, want OK", r.Status)
		}
	})

	t.Run("gate on + capable -> OK", func(t *testing.T) {
		setProxiedProbe(t, true)
		cfg := loadCfg(t, "[workspace]\nname = \"t\"\n\n[beads]\nproxied = true\n")
		r := newBeadsProxiedCapabilityCheck(cfg).Run(nil)
		if r.Status != doctor.StatusOK {
			t.Fatalf("status = %v, want OK", r.Status)
		}
	})

	t.Run("gate on + incapable -> advisory error", func(t *testing.T) {
		setProxiedProbe(t, false)
		cfg := loadCfg(t, "[workspace]\nname = \"t\"\n\n[beads]\nproxied = true\n")
		r := newBeadsProxiedCapabilityCheck(cfg).Run(nil)
		if r.Status != doctor.StatusError || r.Severity != doctor.SeverityAdvisory {
			t.Fatalf("status=%v severity=%v, want Error/Advisory", r.Status, r.Severity)
		}
		if r.FixHint == "" {
			t.Fatal("missing FixHint")
		}
	})
}
