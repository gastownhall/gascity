package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
)

// builtinRuntimeNames is the selection-name contract documented in
// internal/runtime/REQUIREMENTS.md (RUNTIME-SEL-002). Removing a name is
// a breaking change to city configs; update the ledger row with this list.
var builtinRuntimeNames = []string{
	"fake", "fail", "subprocess", "acp", "t3bridge", "cloudflare", "k8s", "hybrid", "tmux",
}

func TestRuntimeRegistryRegistersAllBuiltinNames(t *testing.T) {
	r := buildRuntimeRegistry()
	for _, name := range builtinRuntimeNames {
		if !r.Has(name) {
			t.Errorf("builtin runtime %q not registered", name)
		}
	}
}

func TestNewSessionProviderForCityByName_UnknownNameFallsBackToTmux(t *testing.T) {
	sp, err := newSessionProviderForCityByName(nil, "definitely-not-a-runtime", config.SessionConfig{}, "city", t.TempDir())
	if err != nil {
		t.Fatalf("newSessionProviderForCityByName(unknown): %v", err)
	}
	if _, ok := sp.(*sessiontmux.Provider); !ok {
		t.Fatalf("provider type = %T, want *tmux.Provider (documented fallback)", sp)
	}
}

func TestNewSessionProviderForCityByName_ExecPrefixUsesExecProvider(t *testing.T) {
	sp, err := newSessionProviderForCityByName(nil, "exec:/usr/local/bin/gc-session-screen", config.SessionConfig{}, "city", t.TempDir())
	if err != nil {
		t.Fatalf("newSessionProviderForCityByName(exec:...): %v", err)
	}
	if _, ok := sp.(*sessionexec.Provider); !ok {
		t.Fatalf("provider type = %T, want *exec.Provider", sp)
	}
}

func TestNewSessionProviderForCityByName_TmuxExactNameIsTmuxProvider(t *testing.T) {
	// "tmux" is registered as an exact builtin (not only the fallback) so
	// a pack-declared runtime can never silently shadow the default.
	sp, err := newSessionProviderForCityByName(nil, "tmux", config.SessionConfig{}, "city", t.TempDir())
	if err != nil {
		t.Fatalf("newSessionProviderForCityByName(tmux): %v", err)
	}
	if _, ok := sp.(*sessiontmux.Provider); !ok {
		t.Fatalf("provider type = %T, want *tmux.Provider", sp)
	}
}

func TestRuntimeRegistryForCity_PackRuntimeResolvesToDeclaredExecutable(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ops.log")
	script := writeRPPScript(t, fmt.Sprintf(`echo "$1" >> %q
case "$1" in is-running) echo false ;; *) exit 2 ;; esac
`, marker))

	cfg := &config.City{Runtimes: map[string]config.DiscoveredRuntime{
		"cloudpack": {Name: "cloudpack", Command: script, PackName: "cloud", PackDir: filepath.Dir(script)},
	}}
	reg, err := runtimeRegistryForCity(cfg)
	if err != nil {
		t.Fatalf("runtimeRegistryForCity: %v", err)
	}
	sp, err := reg.New("cloudpack", config.SessionConfig{}, "city", t.TempDir())
	if err != nil {
		t.Fatalf("New(cloudpack): %v", err)
	}
	if _, ok := sp.(*sessionexec.Provider); !ok {
		t.Fatalf("provider type = %T, want *exec.Provider (the RPP proxy)", sp)
	}
	sp.IsRunning("conformance-probe")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("declared executable was never invoked: %v", err)
	}
	if !strings.Contains(string(data), "is-running") {
		t.Errorf("ops log = %q, want is-running op routed to the declared command", data)
	}
}

func TestRuntimeRegistryForCity_BuiltinCollisionErrors(t *testing.T) {
	for _, name := range []string{"subprocess", "tmux"} {
		cfg := &config.City{Runtimes: map[string]config.DiscoveredRuntime{
			name: {Name: name, Command: "/bin/true", PackName: "badpack"},
		}}
		_, err := runtimeRegistryForCity(cfg)
		if err == nil {
			t.Errorf("pack runtime shadowing builtin %q must error", name)
			continue
		}
		for _, want := range []string{name, "badpack"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("collision error %q should mention %q", err, want)
			}
		}
	}
}

func TestRuntimeRegistryForCity_NoPackRuntimesSharesBuiltinRegistry(t *testing.T) {
	reg, err := runtimeRegistryForCity(nil)
	if err != nil {
		t.Fatalf("runtimeRegistryForCity(nil): %v", err)
	}
	if reg != runtimeRegistry {
		t.Error("nil config should reuse the builtin registry, not clone")
	}
	reg, err = runtimeRegistryForCity(&config.City{})
	if err != nil {
		t.Fatalf("runtimeRegistryForCity(empty): %v", err)
	}
	if reg != runtimeRegistry {
		t.Error("config without runtimes should reuse the builtin registry")
	}
}

func TestRuntimeRegistryForCity_DoesNotMutateGlobalRegistry(t *testing.T) {
	cfg := &config.City{Runtimes: map[string]config.DiscoveredRuntime{
		"isolated-rt": {Name: "isolated-rt", Command: "/bin/true", PackName: "p"},
	}}
	if _, err := runtimeRegistryForCity(cfg); err != nil {
		t.Fatalf("runtimeRegistryForCity: %v", err)
	}
	if runtimeRegistry.Has("isolated-rt") {
		t.Error("pack runtime registration must never leak into the process-global registry")
	}
}

func writeRuntimeCityFixture(t *testing.T, runtimeName string) string {
	t.Helper()
	cityDir := t.TempDir()
	packDir := filepath.Join(cityDir, "packs", "rtpack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	packToml := fmt.Sprintf(`
[pack]
name = "rtpack"
schema = 1

[runtimes.%s]
command = "scripts/provider.sh"
`, runtimeName)
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatal(err)
	}
	cityToml := `
[workspace]
name = "demo"

[imports.rtpack]
source = "packs/rtpack"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityDir
}

func TestLoadCityConfig_PackRuntimeBuiltinCollisionFailsLoad(t *testing.T) {
	cityDir := writeRuntimeCityFixture(t, "subprocess")
	_, err := loadCityConfig(cityDir, io.Discard)
	if err == nil {
		t.Fatal("a pack runtime shadowing a builtin must fail city composition, not first session construction")
	}
	for _, want := range []string{"subprocess", "rtpack"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("load error %q should mention %q", err, want)
		}
	}
}

func TestLoadCityConfig_PackRuntimeComposesIntoCity(t *testing.T) {
	cityDir := writeRuntimeCityFixture(t, "cloudpack")
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	rt, ok := cfg.Runtimes["cloudpack"]
	if !ok {
		t.Fatalf("pack runtime missing from composed config; got %v", cfg.Runtimes)
	}
	if want := filepath.Join(cityDir, "packs", "rtpack", "scripts", "provider.sh"); rt.Command != want {
		t.Errorf("command = %q, want %q", rt.Command, want)
	}
}

func TestTryReloadConfig_PackRuntimeBuiltinCollisionFails(t *testing.T) {
	// The controller's hot-reload loader must enforce the same
	// pack-runtime registration validation as every other city config
	// loader (RUNTIME-SEL-011). Without it, `gc reload` adopts a config
	// that every subsequent CLI invocation rejects at loadCityConfig —
	// the city stays live but unmanageable.
	cityDir := writeRuntimeCityFixture(t, "subprocess")
	_, err := tryReloadConfig(filepath.Join(cityDir, "city.toml"), "demo", cityDir)
	if err == nil {
		t.Fatal("reload of a config whose pack runtime shadows a builtin must fail like loadCityConfig does")
	}
	for _, want := range []string{"subprocess", "rtpack"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("reload error %q should mention %q", err, want)
		}
	}
}

func TestTryReloadConfig_PackRuntimeComposes(t *testing.T) {
	cityDir := writeRuntimeCityFixture(t, "cloudpack")
	result, err := tryReloadConfig(filepath.Join(cityDir, "city.toml"), "demo", cityDir)
	if err != nil {
		t.Fatalf("tryReloadConfig: %v", err)
	}
	if _, ok := result.Cfg.Runtimes["cloudpack"]; !ok {
		t.Fatalf("reloaded config missing pack runtime; got %v", result.Cfg.Runtimes)
	}
}

func TestPackRuntimeDeclarationChanged(t *testing.T) {
	withRT := func(rt config.DiscoveredRuntime) *config.City {
		return &config.City{Runtimes: map[string]config.DiscoveredRuntime{"rt": rt}}
	}
	declared := config.DiscoveredRuntime{Name: "rt", Command: "/p/a.sh", PackName: "alpha"}
	cases := []struct {
		name           string
		oldCfg, newCfg *config.City
		want           bool
	}{
		{"nil configs", nil, nil, false},
		{"not declared in either", &config.City{}, &config.City{}, false},
		{"builtin name untouched by pack runtimes", &config.City{}, &config.City{}, false},
		{"declaration added", &config.City{}, withRT(declared), true},
		{"declaration removed", withRT(declared), &config.City{}, true},
		{"command changed", withRT(declared), withRT(config.DiscoveredRuntime{Name: "rt", Command: "/p/b.sh", PackName: "alpha"}), true},
		{"protocol changed", withRT(declared), withRT(config.DiscoveredRuntime{Name: "rt", Command: "/p/a.sh", PackName: "alpha", Protocol: 1}), true},
		{"identical declaration", withRT(declared), withRT(declared), false},
		{"attribution-only change keeps provider", withRT(declared), withRT(config.DiscoveredRuntime{Name: "rt", Command: "/p/a.sh", PackName: "renamed"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := packRuntimeDeclarationChanged(tc.oldCfg, tc.newCfg, "rt"); got != tc.want {
				t.Errorf("packRuntimeDeclarationChanged = %v, want %v", got, tc.want)
			}
		})
	}
}
