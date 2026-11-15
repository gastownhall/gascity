package testutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoltGlobalConfigDisablesNetworkChecks pins what the shared dolt config
// carries. Without versioncheck.disabled, `dolt version` makes a synchronous
// HTTPS call for the latest release, and that call has no timeout of its own
// (30 s behind a black-holed proxy, 60 s behind one that accepts and never
// answers, unbounded after a handshake); the pinned bd probes `dolt version` at
// init with a fixed 10 s timeout and kills it, so a slow network fails an
// otherwise correct test. Without metrics.disabled, dolt status and sql send
// metrics from every fresh HOME.
func TestDoltGlobalConfigDisablesNetworkChecks(t *testing.T) {
	var cfg map[string]string
	if err := json.Unmarshal([]byte(DoltGlobalConfig), &cfg); err != nil {
		t.Fatalf("DoltGlobalConfig is not a JSON object of strings: %v\n%s", err, DoltGlobalConfig)
	}
	for key, want := range map[string]string{
		"versioncheck.disabled": "true",
		"metrics.disabled":      "true",
	} {
		if got := cfg[key]; got != want {
			t.Errorf("DoltGlobalConfig[%q] = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"user.name", "user.email"} {
		if cfg[key] == "" {
			t.Errorf("DoltGlobalConfig[%q] is empty; dolt refuses to commit without an author identity", key)
		}
	}
}

func TestSeedDoltGlobalConfigSeedsDoltRoot(t *testing.T) {
	root := t.TempDir()
	// A HOME is often seeded by one helper and re-seeded by another, so a second
	// write over the first must succeed and leave the same content.
	for range 2 {
		if err := SeedDoltGlobalConfig(root); err != nil {
			t.Fatalf("SeedDoltGlobalConfig(%q): %v", root, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(root, ".dolt", "config_global.json"))
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}
	if string(got) != DoltGlobalConfig {
		t.Fatalf("seeded config = %s, want %s", got, DoltGlobalConfig)
	}
}

func TestSeedDoltGlobalConfigNamesThePathItCouldNotWrite(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	writeTestFile(t, blocker, "")

	err := SeedDoltGlobalConfig(blocker)
	if err == nil {
		t.Fatal("SeedDoltGlobalConfig under a regular file succeeded, want an error")
	}
	if !strings.Contains(err.Error(), blocker) {
		t.Errorf("error %q does not name the failing path %q", err, blocker)
	}
}

func TestUnroutableHTTPSProxyEnvBlackholesEveryRequest(t *testing.T) {
	env := map[string]string{}
	for _, kv := range UnroutableHTTPSProxyEnv() {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("env entry %q has no '='", kv)
		}
		env[key] = value
	}
	for _, key := range []string{"HTTPS_PROXY", "https_proxy"} {
		if env[key] != UnroutableHTTPSProxy {
			t.Errorf("%s = %q, want %q", key, env[key], UnroutableHTTPSProxy)
		}
	}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		if value, ok := env[key]; !ok || value != "" {
			t.Errorf("%s = %q (set=%v), want it set and empty so no bypass list reroutes a request", key, value, ok)
		}
	}
}
