package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// proxiedEnvTestCity builds a ready proxied city whose exec provider records
// every op it is asked to run, so a test can prove a projection invoked bd or
// did not.
func proxiedEnvTestCity(t *testing.T) (cityPath, logPath string) {
	t.Helper()
	cityPath = t.TempDir()
	logPath = filepath.Join(cityPath, "provider-ops")
	// The name matters: contract.ProviderUsesBDContract only recognizes an
	// exec provider called gc-beads-bd as one that speaks the bd contract.
	script := filepath.Join(cityPath, "gc-beads-bd.sh")
	body := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"" + logPath + "\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"t\"\n[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil { //nolint:gosec // fixture config
		t.Fatal(err)
	}
	writeScopeBeadsMetadata(t, cityPath, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`)
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	return cityPath, logPath
}

func providerOpsRun(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// Projecting the environment for a bd command is a question about a scope's
// topology, not a lifecycle operation. On a proxied scope the managed-Dolt
// recovery ladder can never find a port — there is none — so it fell through
// to the city-wide health fan-out and every single bd command paid one
// `bd ping` per provider-owned scope. bd owns proxied readiness (D3/R2): it
// starts the proxy on the next command, and a command that cannot reach it
// reports its own error.
func TestProxiedCityRuntimeEnvDoesNotRunTheProviderHealthFanOut(t *testing.T) {
	cityPath, logPath := proxiedEnvTestCity(t)

	env, err := bdRuntimeEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("bdRuntimeEnvWithError: %v", err)
	}
	if ops := providerOpsRun(t, logPath); len(ops) != 0 {
		t.Fatalf("projecting a proxied city's env invoked the provider: %v", ops)
	}
	if env["BEADS_DOLT_PROXIED_SERVER"] != "1" {
		t.Errorf("BEADS_DOLT_PROXIED_SERVER = %q, want 1", env["BEADS_DOLT_PROXIED_SERVER"])
	}
	if env["BEADS_BACKEND"] != "dolt" || env["GC_BEADS_BACKEND"] != "dolt" {
		t.Errorf("backend projection = %q/%q, want dolt", env["GC_BEADS_BACKEND"], env["BEADS_BACKEND"])
	}
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT"} {
		if value, projected := env[key]; projected && value != "" {
			t.Errorf("%s = %q, want no managed endpoint for a bd-owned proxy", key, value)
		}
	}
}

// The same holds for a proxied rig: its projection must not fall back to the
// city's managed-Dolt recovery either.
func TestProxiedRigRuntimeEnvDoesNotRunTheProviderHealthFanOut(t *testing.T) {
	cityPath, logPath := proxiedEnvTestCity(t)
	rig := filepath.Join(cityPath, "rigs", "r1")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"r1"}`)
	if err := os.MkdirAll(filepath.Join(rig, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}

	env, err := bdRuntimeEnvForRigWithError(cityPath, nil, rig)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError: %v", err)
	}
	if ops := providerOpsRun(t, logPath); len(ops) != 0 {
		t.Fatalf("projecting a proxied rig's env invoked the provider: %v", ops)
	}
	if env["BEADS_DOLT_PROXIED_SERVER"] != "1" {
		t.Errorf("BEADS_DOLT_PROXIED_SERVER = %q, want 1", env["BEADS_DOLT_PROXIED_SERVER"])
	}
	if got, want := env["BEADS_DIR"], filepath.Join(rig, ".beads"); got != want {
		t.Errorf("BEADS_DIR = %q, want %q", got, want)
	}
}

// The proxied projection carries no port, so it is the same answer on every
// call and is memoised for the life of the process. A city.toml rewrite is
// still honored: the stamp the cache is keyed on changes with it.
func TestProxiedCityRuntimeEnvIsMemoisedButFollowsCityConfigRewrites(t *testing.T) {
	cityPath, _ := proxiedEnvTestCity(t)
	t.Cleanup(func() { forgetProxiedScopeRuntimeEnv(cityPath) })

	before := proxiedScopeRuntimeEnvBuilds.Load()
	for range 5 {
		if _, err := bdRuntimeEnvWithError(cityPath); err != nil {
			t.Fatalf("bdRuntimeEnvWithError: %v", err)
		}
	}
	if built := proxiedScopeRuntimeEnvBuilds.Load() - before; built != 1 {
		t.Fatalf("proxied projection built %d times for 5 calls, want 1", built)
	}

	// The name matters: contract.ProviderUsesBDContract only recognizes an
	// exec provider called gc-beads-bd as one that speaks the bd contract.
	script := filepath.Join(cityPath, "gc-beads-bd.sh")
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"rewritten\"\n[beads]\nprovider = \"exec:"+script+"\"\n"),
		0o644); err != nil { //nolint:gosec // fixture config
		t.Fatal(err)
	}
	if _, err := bdRuntimeEnvWithError(cityPath); err != nil {
		t.Fatalf("bdRuntimeEnvWithError after rewrite: %v", err)
	}
	if built := proxiedScopeRuntimeEnvBuilds.Load() - before; built != 2 {
		t.Fatalf("a rewritten city.toml did not rebuild the projection (builds = %d)", built)
	}
}

// A rewrite that lands WHILE the projection is being built must not be
// swallowed. The builder reads city.toml first and stores the entry last, so
// stamping at the Store would record the new file identity against the old
// content and serve it — in a supervisor, for as long as nothing else rewrote
// the file. The stamp is therefore taken before the build, which makes a torn
// read store an already-stale stamp. Simulated by hooking the last projection
// step, which runs after every stamped input has been read.
func TestProxiedRuntimeEnvRebuildsAfterARewriteDuringTheBuild(t *testing.T) {
	cityPath, _ := proxiedEnvTestCity(t)
	t.Cleanup(func() { forgetProxiedScopeRuntimeEnv(cityPath) })

	rewriteCityConfig := func(name string) {
		t.Helper()
		script := filepath.Join(cityPath, "gc-beads-bd.sh")
		body := "[workspace]\nname = \"" + name + "\"\n[beads]\nprovider = \"exec:" + script + "\"\n"
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(body), 0o644); err != nil { //nolint:gosec // fixture config
			t.Fatal(err)
		}
	}

	prev := applyProxiedScopeRuntimeEnvFn
	t.Cleanup(func() { applyProxiedScopeRuntimeEnvFn = prev })
	torn := 0
	applyProxiedScopeRuntimeEnvFn = func(env map[string]string, scopeRoot string) error {
		if torn == 0 {
			torn++
			// city.toml has already been read by this point in the build.
			rewriteCityConfig("rewritten-mid-build")
		}
		return prev(env, scopeRoot)
	}

	before := proxiedScopeRuntimeEnvBuilds.Load()
	if _, err := bdRuntimeEnvWithError(cityPath); err != nil {
		t.Fatalf("bdRuntimeEnvWithError: %v", err)
	}
	if built := proxiedScopeRuntimeEnvBuilds.Load() - before; built != 1 {
		t.Fatalf("first call built the projection %d times, want 1", built)
	}
	if _, err := bdRuntimeEnvWithError(cityPath); err != nil {
		t.Fatalf("bdRuntimeEnvWithError after the torn build: %v", err)
	}
	if built := proxiedScopeRuntimeEnvBuilds.Load() - before; built != 2 {
		t.Fatal("a city.toml rewritten during the build was cached as already-observed")
	}
	// And the rebuild settles: nothing rewrote city.toml this time.
	if _, err := bdRuntimeEnvWithError(cityPath); err != nil {
		t.Fatalf("bdRuntimeEnvWithError after the rebuild: %v", err)
	}
	if built := proxiedScopeRuntimeEnvBuilds.Load() - before; built != 2 {
		t.Fatalf("the settled projection was rebuilt again (builds = %d)", built)
	}
}

// config.yaml decides the classification for a scope whose metadata predates
// dolt_mode, so a canonical config rewrite has to invalidate the projection
// like every other stamped input.
func TestProxiedRuntimeEnvStampCoversScopeConfigRewrites(t *testing.T) {
	cityPath, _ := proxiedEnvTestCity(t)
	inputs := proxiedScopeRuntimeEnvInputs(cityPath, cityPath)
	configPath := filepath.Join(cityPath, ".beads", "config.yaml")
	if !slices.Contains(inputs, configPath) {
		t.Fatalf("stamp inputs %v do not cover %q", inputs, configPath)
	}

	t.Cleanup(func() { forgetProxiedScopeRuntimeEnv(cityPath) })
	before := proxiedScopeRuntimeEnvBuilds.Load()
	if _, err := bdRuntimeEnvWithError(cityPath); err != nil {
		t.Fatalf("bdRuntimeEnvWithError: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("dolt:\n  mode: proxied-server\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bdRuntimeEnvWithError(cityPath); err != nil {
		t.Fatalf("bdRuntimeEnvWithError after the config rewrite: %v", err)
	}
	if built := proxiedScopeRuntimeEnvBuilds.Load() - before; built != 2 {
		t.Fatalf("a rewritten .beads/config.yaml did not rebuild the projection (builds = %d)", built)
	}
}

// `gc init` and `gc rig add` commit a scope's canonical binding in
// finalizeCanonicalBdScopeInit, which is the other in-process topology change
// — a city torn down and rebuilt at a path this process already projected is
// the case the stamp cannot see, because the rebuilt files can land with the
// same size and an unchanged directory entry.
func TestFinalizeCanonicalBdScopeInitDropsTheProjectionCache(t *testing.T) {
	cityPath, _ := proxiedEnvTestCity(t)
	t.Cleanup(func() { forgetProxiedScopeRuntimeEnv(cityPath) })

	rememberProxiedScopeRuntimeEnv(cityPath, cityPath, "pre-init-stamp",
		map[string]string{"GC_DOLT_PORT": "3306"})
	if _, cached := proxiedScopeRuntimeEnvCache.Load(proxiedScopeRuntimeEnvCacheKey(cityPath, cityPath)); !cached {
		t.Fatal("fixture did not seed a cache entry")
	}

	if err := finalizeCanonicalBdScopeInit(cityPath, cityPath, "", "hq"); err != nil {
		t.Fatalf("finalizeCanonicalBdScopeInit: %v", err)
	}
	if _, cached := proxiedScopeRuntimeEnvCache.Load(proxiedScopeRuntimeEnvCacheKey(cityPath, cityPath)); cached {
		t.Error("the city kept a projection cached across a canonical init")
	}
}
