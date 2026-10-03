package dolttest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGlobalConfigDisablesNetworkChecks pins what the shared dolt config
// carries. Without versioncheck.disabled, `dolt version` makes a synchronous
// HTTPS call for the latest release and waits up to 30 s for it; the pinned bd
// probes `dolt version` at init with a fixed 10 s timeout and kills it, so a
// slow network fails an otherwise correct test. Without metrics.disabled, dolt
// status and sql send metrics from every fresh HOME. dolt takes no environment
// variable for either, so the config file is the only switch.
func TestGlobalConfigDisablesNetworkChecks(t *testing.T) {
	var cfg map[string]string
	if err := json.Unmarshal([]byte(GlobalConfigJSON), &cfg); err != nil {
		t.Fatalf("GlobalConfigJSON is not a JSON object of strings: %v\n%s", err, GlobalConfigJSON)
	}
	for key, want := range map[string]string{
		"versioncheck.disabled": "true",
		"metrics.disabled":      "true",
	} {
		if got := cfg[key]; got != want {
			t.Errorf("GlobalConfigJSON[%q] = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"user.name", "user.email"} {
		if cfg[key] == "" {
			t.Errorf("GlobalConfigJSON[%q] is empty; dolt refuses to commit without an author identity", key)
		}
	}
}

func TestWriteGlobalConfigSeedsDoltRoot(t *testing.T) {
	root := t.TempDir()
	// A HOME is often seeded by one helper and re-seeded by another, so a second
	// write over the first must succeed and leave the same content.
	for range 2 {
		if err := WriteGlobalConfig(root); err != nil {
			t.Fatalf("WriteGlobalConfig(%q): %v", root, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(root, ".dolt", globalConfigFile))
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}
	if string(got) != GlobalConfigJSON {
		t.Fatalf("seeded config = %s, want %s", got, GlobalConfigJSON)
	}
}

func TestWriteGlobalConfigNamesThePathItCouldNotWrite(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := WriteGlobalConfig(blocker)
	if err == nil {
		t.Fatal("WriteGlobalConfig under a regular file succeeded, want an error")
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
