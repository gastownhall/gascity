package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// ── Dolt config wiring tests (issue 011) ──────────────────────────────

func TestCityRuntimeProcessEnvStripsAmbientGCDolt(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")

	cityPath := t.TempDir()
	env := cityRuntimeProcessEnv(cityPath)
	for _, entry := range env {
		if strings.HasPrefix(entry, "GC_DOLT=") {
			t.Fatalf("cityRuntimeProcessEnv leaked ambient GC_DOLT control var: %q", entry)
		}
	}
}

func TestBdStoreForCityResolvesIDPrefixFromScopeConfig(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "Metro City"
prefix = "mc"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte("issue_prefix: hq\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := bdStoreForCity(cityDir, cityDir)
	if got := store.IDPrefix(); got != "hq" {
		t.Fatalf("IDPrefix() = %q, want hq", got)
	}
}

func TestBdStoreForRigResolvesIDPrefixFromScopeConfig(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte("issue_prefix: repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: "rigs/repo", Prefix: "ga"}}}

	store := bdStoreForRig(rigDir, cityDir, cfg)
	if got := store.IDPrefix(); got != "repo" {
		t.Fatalf("IDPrefix() = %q, want repo", got)
	}
}

func TestBdStoreForRigPrefersScopeConfigOverKnownPrefix(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte("issue_prefix: repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: "rigs/repo", Prefix: "ga"}}}

	store := bdStoreForRig(rigDir, cityDir, cfg, "stale")
	if got := store.IDPrefix(); got != "repo" {
		t.Fatalf("IDPrefix() = %q, want repo", got)
	}
}

func TestBdStoreForRigFallsBackToConfiguredEffectivePrefix(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "repo")
	if err := os.MkdirAll(rigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: "rigs/repo", Prefix: "ga"}}}

	store := bdStoreForRig(rigDir, cityDir, cfg)
	if got := store.IDPrefix(); got != "ga" {
		t.Fatalf("IDPrefix() = %q, want ga", got)
	}
}

func TestBdRuntimeEnvIncludesDoltHost(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "mini2.hippo-tilapia.ts.net")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT_USER", "agent")
	t.Setenv("GC_DOLT_PASSWORD", "s3cret")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	env := bdRuntimeEnv(cityPath)

	if got := env["GC_DOLT_HOST"]; got != "mini2.hippo-tilapia.ts.net" {
		t.Errorf("GC_DOLT_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Errorf("GC_DOLT_PORT = %q, want %q", got, "3307")
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "mini2.hippo-tilapia.ts.net" {
		t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3307" {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "3307")
	}
	if got := env["BEADS_DOLT_SERVER_USER"]; got != "agent" {
		t.Errorf("BEADS_DOLT_SERVER_USER = %q, want %q", got, "agent")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "s3cret" {
		t.Errorf("BEADS_DOLT_PASSWORD = %q, want %q", got, "s3cret")
	}
	if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
		t.Errorf("BEADS_DOLT_AUTO_START = %q, want %q", got, "0")
	}
}

func TestBdRuntimeEnvExternalHostSkipsLocalState(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "remote.example.com")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	env := bdRuntimeEnv(cityPath)

	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Errorf("GC_DOLT_PORT = %q, want %q (should use env, not local state)", got, "3307")
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3307" {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q (should mirror external env)", got, "3307")
	}
}

func TestManagedLocalDoltHostRecognizesIPv6LoopbackAndWildcard(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"", true},
		{"127.0.0.1", true},
		{"localhost", true},
		{"0.0.0.0", true},
		{"::1", true},
		{"::", true},
		{"db.example.com", false},
	} {
		t.Run(tc.host, func(t *testing.T) {
			if got := managedLocalDoltHost(tc.host); got != tc.want {
				t.Fatalf("managedLocalDoltHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func TestResolvedRuntimeCityDoltTargetIgnoresIPv6LocalEnvOverride(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_PORT", "3307")
	for _, host := range []string{"::1", "::"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("GC_DOLT_HOST", host)
			cityPath := t.TempDir()
			target, ok, err := resolvedRuntimeCityDoltTarget(cityPath, false)
			if err != nil {
				t.Fatalf("resolvedRuntimeCityDoltTarget() error = %v", err)
			}
			if ok {
				t.Fatalf("resolvedRuntimeCityDoltTarget() = %+v, want no external fallback for local host %q", target, host)
			}
		})
	}
}

func TestBdRuntimeEnvUsesCanonicalExternalUser(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: db.example.com
dolt.port: 3307
dolt.user: agent
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := bdRuntimeEnv(cityPath)
	if got := env["GC_DOLT_HOST"]; got != "db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", got, "db.example.com")
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, "3307")
	}
	if got := env["GC_DOLT_USER"]; got != "agent" {
		t.Fatalf("GC_DOLT_USER = %q, want %q", got, "agent")
	}
	if got := env["BEADS_DOLT_SERVER_USER"]; got != "agent" {
		t.Fatalf("BEADS_DOLT_SERVER_USER = %q, want %q", got, "agent")
	}
}

func TestBdRuntimeEnvDoesNotUseStalePortFileWithoutManagedRuntimeState(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")
	t.Setenv("GC_DOLT_USER", "")
	_ = os.Unsetenv("GC_DOLT_USER")
	t.Setenv("GC_DOLT_PASSWORD", "")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo

dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }() //nolint:errcheck // test cleanup
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "dolt-server.port"), []byte(strings.TrimPrefix(ln.Addr().String(), "127.0.0.1:")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env := bdRuntimeEnv(cityPath)
	if got := env["GC_DOLT_PORT"]; got != "" {
		t.Fatalf("GC_DOLT_PORT = %q, want empty without managed runtime state", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want empty without managed runtime state", got)
	}
}

func TestBdRuntimeEnvInvalidCanonicalConfigDoesNotFallbackToCompatRegistration(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")
	_ = os.Unsetenv("GC_DOLT_USER")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := bdRuntimeEnv(cityPath)
	if got := env["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("GC_DOLT_HOST = %q, want empty for invalid canonical config", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "" {
		t.Fatalf("GC_DOLT_PORT = %q, want empty for invalid canonical config", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want empty for invalid canonical config", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want empty for invalid canonical config", got)
	}
}

func TestCityRuntimeProcessEnvInvalidCanonicalConfigDoesNotFallbackToCompatRegistration(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")
	_ = os.Unsetenv("GC_DOLT_USER")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	entries := cityRuntimeProcessEnv(cityPath)
	got := map[string]string{}
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	if got["GC_DOLT_HOST"] != "" || got["GC_DOLT_PORT"] != "" {
		t.Fatalf("cityRuntimeProcessEnv leaked fallback target: %#v", got)
	}
	if got["BEADS_DOLT_SERVER_HOST"] != "" || got["BEADS_DOLT_SERVER_PORT"] != "" {
		t.Fatalf("cityRuntimeProcessEnv leaked fallback beads target: %#v", got)
	}
}

func TestBdRuntimeEnvInvalidCityExplicitOriginDoesNotFallbackToCompatRegistration(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")
	_ = os.Unsetenv("GC_DOLT_USER")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: invalid-db.example.com
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := bdRuntimeEnv(cityPath)
	if got := env["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("GC_DOLT_HOST = %q, want empty for invalid city origin", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "" {
		t.Fatalf("GC_DOLT_PORT = %q, want empty for invalid city origin", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want empty for invalid city origin", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want empty for invalid city origin", got)
	}
}

func TestBdRuntimeEnvInvalidManagedCityConfigDoesNotProjectTrackedEndpoint(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")
	_ = os.Unsetenv("GC_DOLT_USER")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: stale-db.example.com
dolt.port: 3307
dolt.user: stale-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := bdRuntimeEnv(cityPath)
	if got := env["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("GC_DOLT_HOST = %q, want empty for stale managed-city endpoint", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "" {
		t.Fatalf("GC_DOLT_PORT = %q, want empty for stale managed-city endpoint", got)
	}
	if got := env["GC_DOLT_USER"]; got != "" {
		t.Fatalf("GC_DOLT_USER = %q, want empty for stale managed-city endpoint", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want empty for stale managed-city endpoint", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want empty for stale managed-city endpoint", got)
	}
	if got := env["BEADS_DOLT_SERVER_USER"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_USER = %q, want empty for stale managed-city endpoint", got)
	}
}

func TestBdRuntimeEnvPrefersCanonicalExternalConfigOverCompatRegistration(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")
	t.Setenv("GC_DOLT_USER", "")
	_ = os.Unsetenv("GC_DOLT_USER")
	t.Setenv("GC_DOLT_PASSWORD", "")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := bdRuntimeEnv(cityPath)
	if got := env["GC_DOLT_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want canonical host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want canonical port", got)
	}
	if got := env["GC_DOLT_USER"]; got != "canonical-user" {
		t.Fatalf("GC_DOLT_USER = %q, want canonical user", got)
	}
	for _, key := range []string{"GC_DOLT_HOST", "BEADS_DOLT_SERVER_HOST"} {
		if strings.Contains(env[key], "compat-db.example.com") {
			t.Fatalf("%s should ignore compat host, env = %#v", key, env)
		}
	}
}

func TestBdRuntimeEnvIgnoresAmbientHostPortOverrideOverCanonicalConfig(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "override-db.example.com")
	t.Setenv("GC_DOLT_PORT", "5511")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo

gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := bdRuntimeEnv(cityPath)
	if got := env["GC_DOLT_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want canonical host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want canonical port", got)
	}
	if got := env["GC_DOLT_USER"]; got != "canonical-user" {
		t.Fatalf("GC_DOLT_USER = %q, want canonical user", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want canonical host", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3307" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want canonical port", got)
	}
}

func TestSessionDoltEnvFallsBackToCompatCityRegistrationWhenCityConfigLacksEndpointAuthority(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := sessionDoltEnv(cityPath, "", nil)
	if got := env["GC_DOLT_HOST"]; got != "compat-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want compat host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want compat port", got)
	}
}

func TestSessionDoltEnvInheritsCompatCityTargetWhenRigConfigLacksEndpointAuthority(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := sessionDoltEnv(cityPath, rigDir, []config.Rig{{Name: "repo", Path: rigDir}})
	if got := env["GC_DOLT_HOST"]; got != "compat-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want inherited compat host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want inherited compat port", got)
	}
}

func TestSessionDoltEnvFallsBackToCompatRigOverrideWhenRigConfigLacksEndpointAuthority(t *testing.T) {
	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := sessionDoltEnv(cityPath, rigDir, []config.Rig{{Name: "repo", Path: rigDir, DoltHost: "rig-db.example.com", DoltPort: "3308"}})
	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want explicit rig compat host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "3308" {
		t.Fatalf("GC_DOLT_PORT = %q, want explicit rig compat port", got)
	}
}

func TestSessionDoltEnvUsesCanonicalRigUser(t *testing.T) {
	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: rig-db.example.com
dolt.port: 3308
dolt.user: rig-user
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := sessionDoltEnv(cityPath, rigDir, []config.Rig{{Name: "repo", Path: rigDir}})
	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", got, "rig-db.example.com")
	}
	if got := env["GC_DOLT_PORT"]; got != "3308" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, "3308")
	}
	if got := env["GC_DOLT_USER"]; got != "rig-user" {
		t.Fatalf("GC_DOLT_USER = %q, want %q", got, "rig-user")
	}
	if got := env["BEADS_DOLT_SERVER_USER"]; got != "rig-user" {
		t.Fatalf("BEADS_DOLT_SERVER_USER = %q, want %q", got, "rig-user")
	}
}

func TestSessionDoltEnvPrefersCanonicalCityConfigOverCompatRegistration(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := sessionDoltEnv(cityPath, "", nil)
	if got := env["GC_DOLT_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want canonical host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want canonical port", got)
	}
	if got := env["GC_DOLT_USER"]; got != "canonical-user" {
		t.Fatalf("GC_DOLT_USER = %q, want canonical user", got)
	}
	for _, key := range []string{"GC_DOLT_HOST", "BEADS_DOLT_SERVER_HOST"} {
		if strings.Contains(env[key], "compat-db.example.com") {
			t.Fatalf("%s should ignore compat host, env = %#v", key, env)
		}
	}
}

func TestSessionDoltEnvIgnoresAmbientHostPortOverrideOverCanonicalConfig(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "override-db.example.com")
	t.Setenv("GC_DOLT_PORT", "5511")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo

gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := sessionDoltEnv(cityPath, "", nil)
	if got := env["GC_DOLT_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want canonical host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want canonical port", got)
	}
	if got := env["GC_DOLT_USER"]; got != "canonical-user" {
		t.Fatalf("GC_DOLT_USER = %q, want canonical user", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want canonical host", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3307" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want canonical port", got)
	}
}

func TestSessionDoltEnvPrefersInheritedCanonicalRigConfigOverCompatRigOverride(t *testing.T) {
	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	for _, dir := range []string{cityPath, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: stale-db.example.com
dolt.port: 5507
dolt.user: stale-user
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := sessionDoltEnv(cityPath, rigDir, []config.Rig{{Name: "repo", Path: rigDir, DoltHost: "compat-rig-db.example.com", DoltPort: "6608"}})
	if got := env["GC_DOLT_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want inherited canonical host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want inherited canonical port", got)
	}
	if got := env["GC_DOLT_USER"]; got != "canonical-user" {
		t.Fatalf("GC_DOLT_USER = %q, want inherited canonical user", got)
	}
	for _, forbidden := range []string{"compat-rig-db.example.com", "6608", "stale-db.example.com", "5507", "stale-user"} {
		for key, value := range env {
			if strings.Contains(value, forbidden) {
				t.Fatalf("%s should ignore non-canonical inherited value %q, env = %#v", key, forbidden, env)
			}
		}
	}
}

func TestCityRuntimeProcessEnvIncludesDoltHost(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "mini2.hippo-tilapia.ts.net")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT_USER", "agent")
	t.Setenv("GC_DOLT_PASSWORD", "s3cret")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	env := cityRuntimeProcessEnv(cityPath)

	var foundHost, foundPort, foundBeadsHost, foundBeadsPort, foundBeadsUser, foundBeadsPass bool
	for _, entry := range env {
		if strings.HasPrefix(entry, "GC_DOLT_HOST=") {
			foundHost = true
			if got := strings.TrimPrefix(entry, "GC_DOLT_HOST="); got != "mini2.hippo-tilapia.ts.net" {
				t.Errorf("GC_DOLT_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
			}
		}
		if strings.HasPrefix(entry, "GC_DOLT_PORT=") {
			foundPort = true
			if got := strings.TrimPrefix(entry, "GC_DOLT_PORT="); got != "3307" {
				t.Errorf("GC_DOLT_PORT = %q, want %q", got, "3307")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_HOST=") {
			foundBeadsHost = true
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_HOST="); got != "mini2.hippo-tilapia.ts.net" {
				t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_PORT=") {
			foundBeadsPort = true
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_PORT="); got != "3307" {
				t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "3307")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_USER=") {
			foundBeadsUser = true
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_USER="); got != "agent" {
				t.Errorf("BEADS_DOLT_SERVER_USER = %q, want %q", got, "agent")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_PASSWORD=") {
			foundBeadsPass = true
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_PASSWORD="); got != "s3cret" {
				t.Errorf("BEADS_DOLT_PASSWORD = %q, want %q", got, "s3cret")
			}
		}
	}
	if !foundHost {
		t.Error("GC_DOLT_HOST not found in cityRuntimeProcessEnv output")
	}
	if !foundPort {
		t.Error("GC_DOLT_PORT not found in cityRuntimeProcessEnv output")
	}
	if !foundBeadsHost {
		t.Error("BEADS_DOLT_SERVER_HOST not found in cityRuntimeProcessEnv output")
	}
	if !foundBeadsPort {
		t.Error("BEADS_DOLT_SERVER_PORT not found in cityRuntimeProcessEnv output")
	}
	if !foundBeadsUser {
		t.Error("BEADS_DOLT_SERVER_USER not found in cityRuntimeProcessEnv output")
	}
	if !foundBeadsPass {
		t.Error("BEADS_DOLT_PASSWORD not found in cityRuntimeProcessEnv output")
	}
}

func TestCityRuntimeProcessEnvIncludesCanonicalExternalHostForExecGcBeadsBd(t *testing.T) {
	t.Setenv("GC_BEADS", "exec:/tmp/gc-beads-bd")
	t.Setenv("GC_DOLT_HOST", "ambient.invalid")
	t.Setenv("GC_DOLT_PORT", "9999")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries := cityRuntimeProcessEnv(cityPath)
	got := map[string]string{}
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	want := map[string]string{
		"GC_DOLT_HOST":           "city-db.example.com",
		"GC_DOLT_PORT":           "3307",
		"GC_DOLT_USER":           "canonical-user",
		"GC_DOLT_PASSWORD":       "city-pass",
		"BEADS_DOLT_SERVER_HOST": "city-db.example.com",
		"BEADS_DOLT_SERVER_PORT": "3307",
		"BEADS_DOLT_SERVER_USER": "canonical-user",
		"BEADS_DOLT_PASSWORD":    "city-pass",
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Fatalf("%s = %q, want %q (env=%#v)", key, got[key], wantValue, got)
		}
	}
}

func TestBdRuntimeEnvFallsBackToCompatRegistrationWhenCityConfigLacksEndpointAuthority(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")
	t.Setenv("GC_DOLT_USER", "")
	_ = os.Unsetenv("GC_DOLT_USER")
	t.Setenv("GC_DOLT_PASSWORD", "")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := bdRuntimeEnv(cityPath)
	if got := env["GC_DOLT_HOST"]; got != "compat-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want compat host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want compat port", got)
	}
}

func TestCityRuntimeProcessEnvFallsBackToCompatRegistrationWhenCityConfigLacksEndpointAuthority(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")
	t.Setenv("GC_DOLT_USER", "")
	_ = os.Unsetenv("GC_DOLT_USER")
	t.Setenv("GC_DOLT_PASSWORD", "")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := cityRuntimeProcessEnv(cityPath)
	got := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	if got["GC_DOLT_HOST"] != "compat-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want compat host", got["GC_DOLT_HOST"])
	}
	if got["GC_DOLT_PORT"] != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want compat port", got["GC_DOLT_PORT"])
	}
}

func TestCityRuntimeProcessEnvPrefersCanonicalExternalConfigOverCompatRegistration(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")
	t.Setenv("GC_DOLT_USER", "")
	_ = os.Unsetenv("GC_DOLT_USER")
	t.Setenv("GC_DOLT_PASSWORD", "")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := cityRuntimeProcessEnv(cityPath)
	got := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	if got["GC_DOLT_HOST"] != "canonical-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want canonical host", got["GC_DOLT_HOST"])
	}
	if got["GC_DOLT_PORT"] != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want canonical port", got["GC_DOLT_PORT"])
	}
	if got["GC_DOLT_USER"] != "canonical-user" {
		t.Fatalf("GC_DOLT_USER = %q, want canonical user", got["GC_DOLT_USER"])
	}
	if got["BEADS_DOLT_SERVER_HOST"] != "canonical-db.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want canonical host", got["BEADS_DOLT_SERVER_HOST"])
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "3307" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want canonical port", got["BEADS_DOLT_SERVER_PORT"])
	}
	if got["BEADS_DOLT_SERVER_USER"] != "canonical-user" {
		t.Fatalf("BEADS_DOLT_SERVER_USER = %q, want canonical user", got["BEADS_DOLT_SERVER_USER"])
	}
}

func TestMergeRuntimeEnvIncludesDoltHost(t *testing.T) {
	parent := []string{
		"BEADS_DOLT_SERVER_HOST=old-beads-host",
		"BEADS_DOLT_SERVER_PORT=9999",
		"PATH=/usr/bin",
		"GC_DOLT_HOST=old-host",
	}
	overrides := map[string]string{
		"BEADS_DOLT_SERVER_HOST": "new-host.example.com",
		"BEADS_DOLT_SERVER_PORT": "3307",
		"GC_DOLT_HOST":           "new-host.example.com",
	}
	result := mergeRuntimeEnv(parent, overrides)

	var count, beadsCount, beadsPortCount int
	for _, entry := range result {
		if strings.HasPrefix(entry, "GC_DOLT_HOST=") {
			count++
			if got := strings.TrimPrefix(entry, "GC_DOLT_HOST="); got != "new-host.example.com" {
				t.Errorf("GC_DOLT_HOST = %q, want %q", got, "new-host.example.com")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_HOST=") {
			beadsCount++
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_HOST="); got != "new-host.example.com" {
				t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", got, "new-host.example.com")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_PORT=") {
			beadsPortCount++
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_PORT="); got != "3307" {
				t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "3307")
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 GC_DOLT_HOST entry, got %d", count)
	}
	if beadsCount != 1 {
		t.Errorf("expected exactly 1 BEADS_DOLT_SERVER_HOST entry, got %d", beadsCount)
	}
	if beadsPortCount != 1 {
		t.Errorf("expected exactly 1 BEADS_DOLT_SERVER_PORT entry, got %d", beadsPortCount)
	}
}

func TestBdRuntimeEnvLocalHostNoHostKey(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")

	cityPath := t.TempDir()
	env := bdRuntimeEnv(cityPath)

	if _, ok := env["GC_DOLT_HOST"]; ok {
		t.Error("GC_DOLT_HOST should not be present when not configured")
	}
	if _, ok := env["BEADS_DOLT_SERVER_HOST"]; ok {
		t.Error("BEADS_DOLT_SERVER_HOST should not be present when not configured")
	}
}

func TestOpenStoreAtForCityUsesScopeLocalFileStore(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "test-external")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "file")
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatal(err)
	}

	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	if _, err := rigStore.Create(beads.Bead{Title: "rig bead", Type: "task"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	cityStore, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	cityList, err := cityStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("city List after rig create: %v", err)
	}
	if len(cityList) != 0 {
		t.Fatalf("city store should stay empty after rig create, got %d bead(s)", len(cityList))
	}

	if _, err := cityStore.Create(beads.Bead{Title: "city bead", Type: "task"}); err != nil {
		t.Fatalf("city Create: %v", err)
	}
	rigList, err := rigStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("rig List after city create: %v", err)
	}
	if len(rigList) != 1 || rigList[0].Title != "rig bead" {
		t.Fatalf("rig store should still contain only its own bead, got %#v", rigList)
	}
}

func TestOpenStoreAtForCityUsesRigBdStoreUnderFileBackedCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	if _, ok := store.(*beads.BdStore); !ok {
		t.Fatalf("openStoreAtForCity(rig) returned %T, want *beads.BdStore", store)
	}
}

func TestOpenStoreAtForCityUsesRigFileStoreUnderBdBackedCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".gc", "beads.json"), []byte("{\"seq\":0,\"beads\":[]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	if _, ok := store.(*beads.FileStore); !ok {
		t.Fatalf("openStoreAtForCity(rig) returned %T, want *beads.FileStore", store)
	}
}

func TestOpenStoreAtForCityLegacyEmptyFileCityDoesNotFailOrCreateRigState(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "legacy-rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")

	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	rigList, err := rigStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("rig List: %v", err)
	}
	if len(rigList) != 0 {
		t.Fatalf("empty legacy file city should list zero beads, got %#v", rigList)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".gc")); !os.IsNotExist(err) {
		t.Fatalf("legacy empty rig open should not create rig .gc state, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".gc", "beads.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy empty city open should not create beads.json, stat err = %v", err)
	}
}

func TestOpenStoreAtForCityPreservesLegacySharedFileStoreWithoutCreatingRigState(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "legacy-rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")

	legacyCityStore, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatalf("openScopeLocalFileStore(city): %v", err)
	}
	if _, err := legacyCityStore.Create(beads.Bead{Title: "legacy city bead", Type: "task"}); err != nil {
		t.Fatalf("legacy city Create: %v", err)
	}

	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	rigList, err := rigStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("rig List: %v", err)
	}
	if len(rigList) != 1 || rigList[0].Title != "legacy city bead" {
		t.Fatalf("rig store should read legacy shared city data, got %#v", rigList)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".gc")); !os.IsNotExist(err) {
		t.Fatalf("legacy rig open should not create rig .gc state, stat err = %v", err)
	}
}

func TestMergeRuntimeEnvReplacesInheritedRuntimeKeys(t *testing.T) {
	env := mergeRuntimeEnv([]string{
		"BEADS_DIR=/rig/.beads",
		"BEADS_DOLT_SERVER_PORT=9999",
		"PATH=/bin",
		"GC_CITY_PATH=/wrong",
		"GC_DOLT_PORT=9999",
		"GC_PACK_STATE_DIR=/wrong/.gc/runtime/packs/dolt",
		"GC_RIG=demo",
		"GC_RIG_ROOT=/rig",
	}, map[string]string{
		"BEADS_DOLT_SERVER_PORT": "31364",
		"GC_CITY_PATH":           "/city",
		"GC_DOLT_PORT":           "31364",
	})

	got := make(map[string]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}

	if got["GC_CITY_PATH"] != "/city" {
		t.Fatalf("GC_CITY_PATH = %q, want %q", got["GC_CITY_PATH"], "/city")
	}
	if got["GC_DOLT_PORT"] != "31364" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got["GC_DOLT_PORT"], "31364")
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "31364" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got["BEADS_DOLT_SERVER_PORT"], "31364")
	}
	if _, ok := got["BEADS_DIR"]; ok {
		t.Fatalf("BEADS_DIR should be removed, env = %#v", got)
	}
	if _, ok := got["GC_PACK_STATE_DIR"]; ok {
		t.Fatalf("GC_PACK_STATE_DIR should be removed, env = %#v", got)
	}
	if _, ok := got["GC_RIG"]; ok {
		t.Fatalf("GC_RIG should be removed, env = %#v", got)
	}
	if _, ok := got["GC_RIG_ROOT"]; ok {
		t.Fatalf("GC_RIG_ROOT should be removed, env = %#v", got)
	}
}

func TestBdCommandRunnerForCityPinsCityStoreEnv(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("BEADS_DIR", "/rig/.beads")
	t.Setenv("GC_RIG", "demo-rig")
	t.Setenv("GC_RIG_ROOT", "/rig")

	runner := bdCommandRunnerForCity(cityDir)
	out, err := runner(cityDir, "sh", "-c", `printf '%s\n%s\n%s\n%s\n' "$GC_CITY_PATH" "$BEADS_DIR" "$GC_RIG" "$GC_RIG_ROOT"`)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(string(out), "\n")
	if len(lines) != 5 {
		t.Fatalf("lines = %q, want 5 lines including trailing newline", string(out))
	}
	lines = lines[:4]
	if len(lines) != 4 {
		t.Fatalf("lines = %q, want 4 lines", string(out))
	}
	if lines[0] != cityDir {
		t.Fatalf("GC_CITY_PATH = %q, want %q", lines[0], cityDir)
	}
	if lines[1] != filepath.Join(cityDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", lines[1], filepath.Join(cityDir, ".beads"))
	}
	if lines[2] != "" {
		t.Fatalf("GC_RIG = %q, want empty", lines[2])
	}
	if lines[3] != "" {
		t.Fatalf("GC_RIG_ROOT = %q, want empty", lines[3])
	}
}

func TestBdCommandRunnerForCityClearsAmbientDoltEnvWhenManagedRuntimeUnavailable(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "ambient.invalid")
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("GC_DOLT_USER", "ambient-user")
	t.Setenv("GC_DOLT_PASSWORD", "ambient-pass")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient.invalid")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_USER", "ambient-user")
	t.Setenv("BEADS_DOLT_PASSWORD", "ambient-pass")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := bdCommandRunnerForCity(cityDir)
	out, err := runner(cityDir, "sh", "-c", `printf '%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n' "$GC_DOLT_HOST" "$GC_DOLT_PORT" "$GC_DOLT_USER" "$GC_DOLT_PASSWORD" "$BEADS_DOLT_SERVER_HOST" "$BEADS_DOLT_SERVER_PORT" "$BEADS_DOLT_SERVER_USER" "$BEADS_DOLT_PASSWORD"`)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(string(out), "\n")
	if len(lines) != 9 {
		t.Fatalf("lines = %q, want 9 lines including trailing newline", string(out))
	}
	for i, name := range []string{
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_PASSWORD",
	} {
		if lines[i] != "" {
			t.Fatalf("%s = %q, want empty when managed runtime is unavailable", name, lines[i])
		}
	}
}

// This test exercises the shared bd opener path for a rig-scoped store.
// It verifies that the opener and runner pick up the rig's canonical
// Dolt target instead of falling back to the city-scoped opener.
func TestOpenStoreAtForCityUsesRigScopedDoltConfigWithoutProcessEnvSync(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.host: city-db.example.com
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(t.TempDir(), "my-rig")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: myrig
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.host: rig-db.example.com
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "bd-env.txt")
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
{
  printf 'GC_DOLT_HOST=%s\n' "${GC_DOLT_HOST:-}"
  printf 'GC_DOLT_PORT=%s\n' "${GC_DOLT_PORT:-}"
  printf 'BEADS_DOLT_SERVER_HOST=%s\n' "${BEADS_DOLT_SERVER_HOST:-}"
  printf 'BEADS_DOLT_SERVER_PORT=%s\n' "${BEADS_DOLT_SERVER_PORT:-}"
  printf 'BEADS_DIR=%s\n' "${BEADS_DIR:-}"
  printf 'GC_RIG_ROOT=%s\n' "${GC_RIG_ROOT:-}"
} > "${CAPTURE_PATH}"
exit 0
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-city.example.com")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")

	store, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	bdStore, ok := store.(*beads.BdStore)
	if !ok {
		t.Fatalf("openStoreAtForCity returned %T, want *beads.BdStore", store)
	}
	if err := bdStore.Init("myrig", "", ""); err != nil {
		t.Fatalf("bd store init: %v", err)
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			got[key] = value
		}
	}
	if got["GC_DOLT_HOST"] != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", got["GC_DOLT_HOST"], "rig-db.example.com")
	}
	if got["GC_DOLT_PORT"] != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got["GC_DOLT_PORT"], "3307")
	}
	if got["BEADS_DOLT_SERVER_HOST"] != "rig-db.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want %q", got["BEADS_DOLT_SERVER_HOST"], "rig-db.example.com")
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "3307" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got["BEADS_DOLT_SERVER_PORT"], "3307")
	}
	if got["BEADS_DIR"] != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got["BEADS_DIR"], filepath.Join(rigDir, ".beads"))
	}
	if got["GC_RIG_ROOT"] != rigDir {
		t.Fatalf("GC_RIG_ROOT = %q, want %q", got["GC_RIG_ROOT"], rigDir)
	}
}

func TestBdRuntimeEnvForRigUsesCanonicalManagedRigTarget(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }() //nolint:errcheck // test cleanup

	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: "2026-04-02T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "dolt-server.port"), []byte("31364"), 0o644); err != nil {
		t.Fatal(err)
	}

	env := bdRuntimeEnvForRig(cityDir, &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}, rigDir)
	wantPort := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	if got := env["GC_DOLT_PORT"]; got != wantPort {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, wantPort)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != wantPort {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, wantPort)
	}
	if got := env["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("GC_DOLT_HOST = %q, want empty for managed target", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want empty for managed target", got)
	}
}

func TestBdRuntimeEnvForRigFallsBackToManagedCityPort(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }() //nolint:errcheck // test cleanup

	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: "2026-04-02T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	env := bdRuntimeEnvForRig(cityDir, &config.City{}, rigDir)
	want := strings.TrimSpace(strings.TrimPrefix(ln.Addr().String(), "127.0.0.1:"))
	if got := env["GC_DOLT_PORT"]; got != want {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, want)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != want {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, want)
	}
	if got := env["BEADS_DIR"]; got != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got, filepath.Join(rigDir, ".beads"))
	}
}

func TestBdRuntimeEnvForRigInvalidCanonicalConfigDoesNotFallbackToCompatRegistration(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: demo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityDir, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityDir) })

	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.port: 3308
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := bdRuntimeEnvForRig(cityDir, &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}, rigDir)
	if got := env["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("GC_DOLT_HOST = %q, want empty for invalid canonical rig config", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "" {
		t.Fatalf("GC_DOLT_PORT = %q, want empty for invalid canonical rig config", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want empty for invalid canonical rig config", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want empty for invalid canonical rig config", got)
	}
	if got := env["BEADS_DIR"]; got != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got, filepath.Join(rigDir, ".beads"))
	}
}

func TestBdRuntimeEnvForRigInheritedManagedCityConfigDoesNotProjectTrackedEndpoint(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: "2026-04-02T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: stale-rig-db.example.com
dolt.port: 5507
dolt.user: stale-user
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := bdRuntimeEnvForRig(cityDir, &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}, rigDir)
	if got := env["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("GC_DOLT_HOST = %q, want empty for stale inherited managed-city endpoint", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "" {
		t.Fatalf("GC_DOLT_PORT = %q, want empty for stale inherited managed-city endpoint", got)
	}
	if got := env["GC_DOLT_USER"]; got != "" {
		t.Fatalf("GC_DOLT_USER = %q, want empty for stale inherited managed-city endpoint", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want empty for stale inherited managed-city endpoint", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want empty for stale inherited managed-city endpoint", got)
	}
	if got := env["BEADS_DOLT_SERVER_USER"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_USER = %q, want empty for stale inherited managed-city endpoint", got)
	}
	if got := env["BEADS_DIR"]; got != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got, filepath.Join(rigDir, ".beads"))
	}
}

func TestBdRuntimeEnvForRigInheritsCompatCityTargetWhenRigConfigLacksEndpointAuthority(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := bdRuntimeEnvForRig(cityPath, &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}, rigDir)
	if got := env["GC_DOLT_HOST"]; got != "compat-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want inherited compat host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want inherited compat port", got)
	}
}

func TestBdRuntimeEnvForRigInheritsResolvedCityTargetWhenAuthoritativeRigUsesInheritedCity(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "compat-db.example.com")
	t.Setenv("GC_DOLT_PORT", "4406")

	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := bdRuntimeEnvForRig(cityPath, &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}, rigDir)
	if got := env["GC_DOLT_HOST"]; got != "compat-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want inherited resolved city host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want inherited resolved city port", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "compat-db.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want inherited resolved city host", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "4406" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want inherited resolved city port", got)
	}
}

func TestBdRuntimeEnvForRigPrefersInheritedCanonicalRigConfigOverCompatRigOverride(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: canonical-db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: stale-db.example.com
dolt.port: 5507
dolt.user: stale-user
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := bdRuntimeEnvForRig(cityPath, &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir, DoltHost: "compat-rig-db.example.com", DoltPort: "6608"}}}, rigDir)
	if got := env["GC_DOLT_HOST"]; got != "canonical-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want inherited canonical host", got)
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want inherited canonical port", got)
	}
	if got := env["GC_DOLT_USER"]; got != "canonical-user" {
		t.Fatalf("GC_DOLT_USER = %q, want inherited canonical user", got)
	}
	// Check dolt-related keys directly for the forbidden values.
	// Scoping by key (rather than a substring scan across every value
	// including path-shaped ones like GC_RIG_ROOT) avoids false
	// positives when Go's t.TempDir random suffix happens to embed one
	// of the forbidden digit sequences — e.g. tempdir
	// ".../Test..2266660824/002/repo" contains "6608" and caused this
	// test to flake in CI.
	forbiddenByKey := map[string][]string{
		"GC_DOLT_HOST":           {"compat-rig-db.example.com", "stale-db.example.com"},
		"GC_DOLT_PORT":           {"6608", "5507"},
		"GC_DOLT_USER":           {"stale-user"},
		"BEADS_DOLT_SERVER_HOST": {"compat-rig-db.example.com", "stale-db.example.com"},
		"BEADS_DOLT_SERVER_PORT": {"6608", "5507"},
		"BEADS_DOLT_SERVER_USER": {"stale-user"},
	}
	for key, bad := range forbiddenByKey {
		value := env[key]
		for _, forbidden := range bad {
			if value == forbidden {
				t.Fatalf("%s = %q is a non-canonical inherited value; env = %#v", key, forbidden, env)
			}
		}
	}
}

func TestBdRuntimeEnvForRigPrefersExplicitRigDoltConfigOverManagedCity(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }() //nolint:errcheck // test cleanup

	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: "2026-04-02T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:     "repo",
			Path:     rigDir,
			DoltHost: "rig-db.example.com",
			DoltPort: "3307",
		}},
	}

	env := bdRuntimeEnvForRig(cityDir, cfg, rigDir)
	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", got, "rig-db.example.com")
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, "3307")
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want %q", got, "rig-db.example.com")
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3307" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "3307")
	}
	if got := env["BEADS_DIR"]; got != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got, filepath.Join(rigDir, ".beads"))
	}
	if got := env["GC_RIG"]; got != "repo" {
		t.Fatalf("GC_RIG = %q, want %q", got, "repo")
	}
	if got := env["GC_RIG_ROOT"]; got != rigDir {
		t.Fatalf("GC_RIG_ROOT = %q, want %q", got, rigDir)
	}
}

func TestBdRuntimeEnvAlwaysIncludesBeadsDoltServerPort(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	// No host/port configured — BEADS_DOLT_SERVER_PORT should still be
	// present (empty) as defense-in-depth against inherited env leakage.
	_ = os.Unsetenv("GC_DOLT_HOST")
	_ = os.Unsetenv("GC_DOLT_PORT")

	cityPath := t.TempDir()
	env := bdRuntimeEnv(cityPath)

	val, ok := env["BEADS_DOLT_SERVER_PORT"]
	if !ok {
		t.Fatal("BEADS_DOLT_SERVER_PORT must always be present in bdRuntimeEnv output")
	}
	if val != "" {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want empty (no port configured)", val)
	}
}

func TestDoltAutoStartSuppressedInAllEnvPaths(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()

	t.Run("bdRuntimeEnv", func(t *testing.T) {
		env := bdRuntimeEnv(cityPath)
		if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
			t.Errorf("BEADS_DOLT_AUTO_START = %q, want %q", got, "0")
		}
	})

	t.Run("bdRuntimeEnvForRig", func(t *testing.T) {
		rigDir := filepath.Join(t.TempDir(), "rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		env := bdRuntimeEnvForRig(cityPath, &config.City{}, rigDir)
		if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
			t.Errorf("BEADS_DOLT_AUTO_START = %q, want %q", got, "0")
		}
	})

	t.Run("sessionDoltEnv", func(t *testing.T) {
		env := sessionDoltEnv(cityPath, "", nil)
		if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
			t.Errorf("BEADS_DOLT_AUTO_START = %q, want %q", got, "0")
		}
	})
}

// ── cityForStoreDir boundary tests ──────────────────────────────────

func TestCityForStoreDirHonoursGCCity(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

	cityDir := filepath.Join(homeDir, "mycity")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"mine\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// GC_CITY points to the exact city root — should resolve via
	// validateCityPath without walk-up, even when the store dir is
	// outside bounded discovery range.
	t.Setenv("GC_CITY", cityDir)
	outsideDir := t.TempDir()
	got := cityForStoreDir(outsideDir)
	if canonicalTestPath(got) != canonicalTestPath(cityDir) {
		t.Errorf("cityForStoreDir(%q) = %q, want %q (from GC_CITY)", outsideDir, got, cityDir)
	}
}

func TestCityForStoreDirFallsBackToFindCity(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

	// Unset GC_CITY so cityForStoreDir falls back to findCity.
	t.Setenv("GC_CITY", "")

	cityDir := filepath.Join(homeDir, "projects", "alpha")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"alpha\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Store dir is inside the city — findCity should discover it.
	storeDir := filepath.Join(cityDir, "rigs", "repo")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	got := cityForStoreDir(storeDir)
	if canonicalTestPath(got) != canonicalTestPath(cityDir) {
		t.Errorf("cityForStoreDir(%q) = %q, want %q (from findCity)", storeDir, got, cityDir)
	}
}

func TestCityForStoreDirFallsBackToDirWhenNoCityFound(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("GC_CITY", "")

	// No city.toml anywhere — cityForStoreDir should return dir as fallback.
	noCity := filepath.Join(homeDir, "nocity", "deep")
	if err := os.MkdirAll(noCity, 0o755); err != nil {
		t.Fatal(err)
	}

	got := cityForStoreDir(noCity)
	if canonicalTestPath(got) != canonicalTestPath(noCity) {
		t.Errorf("cityForStoreDir(%q) = %q, want same dir as fallback", noCity, got)
	}
}

func TestBdRuntimeEnvUsesStoreLocalBeadsEnvPassword(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := bdRuntimeEnv(cityPath)
	if got := env["GC_DOLT_PASSWORD"]; got != "city-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "city-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "city-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "city-secret")
	}
}

func TestBdRuntimeEnvPrefersProcessPasswordOverStoreAndCredentials(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "override-secret")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	credentialsPath := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(credentialsPath, []byte("[db.example.com:3307]\npassword=credentials-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	env := bdRuntimeEnv(cityPath)
	if got := env["GC_DOLT_PASSWORD"]; got != "override-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "override-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "override-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "override-secret")
	}
	if got := env["BEADS_CREDENTIALS_FILE"]; got != credentialsPath {
		t.Fatalf("BEADS_CREDENTIALS_FILE = %q, want %q", got, credentialsPath)
	}
}

func TestBdRuntimeEnvUsesCredentialsFilePasswordWhenStoreSecretMissing(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	credentialsPath := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(credentialsPath, []byte("[db.example.com:3307]\npassword=credentials-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	env := bdRuntimeEnv(cityPath)
	if got := env["GC_DOLT_PASSWORD"]; got != "credentials-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "credentials-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "credentials-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "credentials-secret")
	}
	if got := env["BEADS_CREDENTIALS_FILE"]; got != credentialsPath {
		t.Fatalf("BEADS_CREDENTIALS_FILE = %q, want %q", got, credentialsPath)
	}
}

func TestSessionDoltEnvInheritedRigUsesCityStorePassword(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	for _, dir := range []string{cityPath, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=rig-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := sessionDoltEnv(cityPath, rigDir, []config.Rig{{Name: "repo", Path: rigDir}})
	if got := env["GC_DOLT_PASSWORD"]; got != "city-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "city-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "city-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "city-secret")
	}
}

func TestSessionDoltEnvExplicitRigUsesRigStorePassword(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	for _, dir := range []string{cityPath, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: city-db.example.com
dolt.port: 3307
dolt.user: city-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: explicit
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: rig-db.example.com
dolt.port: 3308
dolt.user: rig-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=rig-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := sessionDoltEnv(cityPath, rigDir, []config.Rig{{Name: "repo", Path: rigDir}})
	if got := env["GC_DOLT_PASSWORD"]; got != "rig-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "rig-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "rig-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "rig-secret")
	}
}

func TestCityRuntimeProcessEnvForwardsBeadsCredentialsFile(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: db.example.com
dolt.port: 3307
dolt.user: canonical-user
`), 0o644); err != nil {
		t.Fatal(err)
	}
	credentialsPath := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(credentialsPath, []byte("[db.example.com:3307]\npassword=credentials-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	entries := cityRuntimeProcessEnv(cityPath)
	got := map[string]string{}
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	if got["BEADS_CREDENTIALS_FILE"] != credentialsPath {
		t.Fatalf("BEADS_CREDENTIALS_FILE = %q, want %q", got["BEADS_CREDENTIALS_FILE"], credentialsPath)
	}
	if got["GC_DOLT_PASSWORD"] != "credentials-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got["GC_DOLT_PASSWORD"], "credentials-secret")
	}
	if got["BEADS_DOLT_PASSWORD"] != "credentials-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got["BEADS_DOLT_PASSWORD"], "credentials-secret")
	}
}

func TestSessionDoltEnvCompatCityFallbackUsesCityStorePassword(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cityDoltConfigs.Store(cityPath, config.DoltConfig{Host: "compat-db.example.com", Port: 4406})
	t.Cleanup(func() { cityDoltConfigs.Delete(cityPath) })

	env := sessionDoltEnv(cityPath, "", nil)
	if got := env["GC_DOLT_PASSWORD"]; got != "city-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "city-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "city-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "city-secret")
	}
}

func TestSessionDoltEnvCompatRigOverrideUsesRigStorePassword(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=rig-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := sessionDoltEnv(cityPath, rigDir, []config.Rig{{Name: "repo", Path: rigDir, DoltHost: "rig-db.example.com", DoltPort: "3308"}})
	if got := env["GC_DOLT_PASSWORD"]; got != "rig-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "rig-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "rig-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "rig-secret")
	}
}

func TestBdTransportRetryableErrorDoesNotTreatCommandTimeoutAsTransportFailure(t *testing.T) {
	env := map[string]string{"GC_DOLT_HOST": ""}
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	if bdTransportRetryableError(cityPath, cityPath, env, fmt.Errorf("timed out after 120s")) {
		t.Fatal("timed out after 120s should not be treated as transport-retryable")
	}
}

func TestBdTransportRetryableErrorUsesScopeProviderForMixedRig(t *testing.T) {
	cityPath := t.TempDir()
	_ = writeReachableManagedDoltState(t, cityPath)
	rigDir := filepath.Join(cityPath, "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"repo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GC_DOLT_HOST": "", "GC_DOLT_PORT": "3307"}

	if !bdTransportRetryableError(cityPath, rigDir, env, fmt.Errorf("server unreachable at 127.0.0.1:3307")) {
		t.Fatal("bd-backed rig under file-backed city should still be transport-retryable")
	}
}

func TestBdCommandRunnerWithManagedRetryRecoversAndRerunsWithFreshEnv(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	origRunner := beadsExecCommandRunnerWithEnv
	origRecover := recoverManagedBDCommand
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		recoverManagedBDCommand = origRecover
	})

	port := "3307"
	attempts := 0
	recoverCalls := 0
	seenPorts := make([]string, 0, 2)

	beadsExecCommandRunnerWithEnv = func(env map[string]string) beads.CommandRunner {
		copied := map[string]string{}
		for key, value := range env {
			copied[key] = value
		}
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			attempts++
			seenPorts = append(seenPorts, copied["GC_DOLT_PORT"])
			if attempts == 1 {
				return nil, fmt.Errorf("server unreachable at 127.0.0.1:%s", copied["GC_DOLT_PORT"])
			}
			return []byte("ok"), nil
		}
	}
	recoverManagedBDCommand = func(_ string) error {
		recoverCalls++
		port = "3308"
		return nil
	}

	runner := bdCommandRunnerWithManagedRetry(t.TempDir(), func(_ string) map[string]string {
		return map[string]string{
			"GC_DOLT_PORT": port,
		}
	})

	out, err := runner(t.TempDir(), "bd", "list", "--json")
	if err != nil {
		t.Fatalf("runner error = %v, want nil", err)
	}
	if string(out) != "ok" {
		t.Fatalf("runner output = %q, want %q", out, "ok")
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if recoverCalls != 1 {
		t.Fatalf("recoverCalls = %d, want 1", recoverCalls)
	}
	if len(seenPorts) != 2 {
		t.Fatalf("seenPorts = %v, want 2 attempts", seenPorts)
	}
	if seenPorts[0] != "3307" || seenPorts[1] != "3308" {
		t.Fatalf("seenPorts = %v, want [3307 3308]", seenPorts)
	}
}

func TestBdCommandRunnerWithManagedRetrySkipsRecoveryForLoopbackExternalEndpoint(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: 127.0.0.1
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}

	origRunner := beadsExecCommandRunnerWithEnv
	origRecover := recoverManagedBDCommand
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv = origRunner
		recoverManagedBDCommand = origRecover
	})

	attempts := 0
	recoverCalls := 0
	beadsExecCommandRunnerWithEnv = func(env map[string]string) beads.CommandRunner {
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			attempts++
			return nil, fmt.Errorf("server unreachable at 127.0.0.1:%s", env["GC_DOLT_PORT"])
		}
	}
	recoverManagedBDCommand = func(_ string) error {
		recoverCalls++
		return nil
	}

	runner := bdCommandRunnerWithManagedRetry(cityPath, func(_ string) map[string]string {
		return map[string]string{
			"GC_DOLT_HOST": "127.0.0.1",
			"GC_DOLT_PORT": "3307",
		}
	})

	_, err := runner(cityPath, "bd", "list", "--json")
	if err == nil || !strings.Contains(err.Error(), "server unreachable") {
		t.Fatalf("runner error = %v, want transport failure", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if recoverCalls != 0 {
		t.Fatalf("recoverCalls = %d, want 0", recoverCalls)
	}
}
