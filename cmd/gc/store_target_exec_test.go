package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func writeExecStoreCityConfig(t *testing.T, cityDir, cityName, cityPrefix string, rigs []config.Rig) {
	t.Helper()

	content := fmt.Sprintf("[workspace]\nname = %q\n", cityName)
	if cityPrefix != "" {
		content += fmt.Sprintf("prefix = %q\n", cityPrefix)
	}
	for _, rig := range rigs {
		content += "\n[[rigs]]\n"
		content += fmt.Sprintf("name = %q\npath = %q\n", rig.Name, rig.Path)
		if rig.Prefix != "" {
			content += fmt.Sprintf("prefix = %q\n", rig.Prefix)
		}
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeNamedExecCaptureScript(t *testing.T, captureDir, name string) string {
	t.Helper()
	path := filepath.Join(captureDir, name)
	script := fmt.Sprintf(`#!/bin/sh
set -eu
op="$1"
shift
capture_dir=%q
case "$op" in
  create)
    name=city
    if [ "${GC_STORE_SCOPE:-}" = "rig" ]; then
      name="${GC_RIG:-rig}"
    fi
    out="$capture_dir/$name.env"
    printf 'GC_STORE_ROOT=%%s
GC_STORE_SCOPE=%%s
GC_BEADS_PREFIX=%%s
GC_CITY=%%s
GC_CITY_PATH=%%s
GC_RIG=%%s
GC_RIG_ROOT=%%s
GC_PROVIDER=%%s
BEADS_DIR=%%s
GC_DOLT_HOST=%%s
GC_DOLT_PORT=%%s
'       "${GC_STORE_ROOT:-}" "${GC_STORE_SCOPE:-}" "${GC_BEADS_PREFIX:-}" "${GC_CITY:-}" "${GC_CITY_PATH:-}" "${GC_RIG:-}" "${GC_RIG_ROOT:-}" "${GC_PROVIDER:-}" "${BEADS_DIR:-}" "${GC_DOLT_HOST:-}" "${GC_DOLT_PORT:-}" > "$out"
    cat >/dev/null
    echo '{"id":"EX-1","title":"captured","status":"open","type":"task","created_at":"2026-02-27T10:00:00Z"}'
    ;;
  *)
    exit 2
    ;;
esac
`, captureDir)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeExecCaptureScript(t *testing.T, captureDir string) string {
	t.Helper()
	return writeNamedExecCaptureScript(t, captureDir, "exec-provider.sh")
}

func readExecCaptureEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed capture line %q", line)
		}
		out[key] = value
	}
	return out
}

func envSliceValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func TestProviderUsesBdStoreContract(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{provider: "", want: true},
		{provider: "bd", want: true},
		{provider: "file", want: false},
		{provider: "exec:/tmp/gc-beads-bd", want: true},
		{provider: "exec:/tmp/gc-beads-k8s", want: false},
		{provider: "exec:/tmp/custom", want: false},
	}
	for _, tt := range tests {
		if got := providerUsesBdStoreContract(tt.provider); got != tt.want {
			t.Fatalf("providerUsesBdStoreContract(%q) = %v, want %v", tt.provider, got, tt.want)
		}
	}
}

func TestGcExecLifecycleInitProcessEnvDoesNotProjectCanonicalFilesOwnedFlagForGcBeadsBd(t *testing.T) {
	cityDir := t.TempDir()
	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "gc"}
	env, err := gcExecLifecycleInitProcessEnv(cityDir, target, "exec:/tmp/gc-beads-bd")
	if err != nil {
		t.Fatalf("gcExecLifecycleInitProcessEnv(gc-beads-bd): %v", err)
	}
	if got := envSliceValue(env, "GC_CANONICAL_FILES_OWNED"); got != "" {
		t.Fatalf("GC_CANONICAL_FILES_OWNED = %q, want empty", got)
	}
	if got := envSliceValue(env, "GC_STORE_ROOT"); got != cityDir {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got, cityDir)
	}
	if got := envSliceValue(env, "GC_STORE_SCOPE"); got != "city" {
		t.Fatalf("GC_STORE_SCOPE = %q, want city", got)
	}
}

func TestGcExecLifecycleInitProcessEnvDoesNotLeakAmbientBEADS_DIRForGcBeadsK8s(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecStoreCityConfig(t, cityDir, "metro-city", "ct", []config.Rig{{
		Name:   "frontend",
		Path:   "rigs/frontend",
		Prefix: "fe",
	}})

	t.Setenv("BEADS_DIR", "/tmp/ambient-beads")
	target := execStoreTarget{
		ScopeRoot: rigDir,
		ScopeKind: "rig",
		Prefix:    "fe",
		RigName:   "frontend",
	}
	env, err := gcExecLifecycleInitProcessEnv(cityDir, target, "exec:/tmp/gc-beads-k8s")
	if err != nil {
		t.Fatalf("gcExecLifecycleInitProcessEnv(gc-beads-k8s): %v", err)
	}
	if got := envSliceValue(env, "BEADS_DIR"); got != "" {
		t.Fatalf("BEADS_DIR leaked as %q", got)
	}
	if got := envSliceValue(env, "GC_STORE_ROOT"); got != rigDir {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got, rigDir)
	}
	if got := envSliceValue(env, "GC_RIG"); got != "frontend" {
		t.Fatalf("GC_RIG = %q, want frontend", got)
	}
}

func TestGcExecStoreEnvProjectsGCBinForGcBeadsBd(t *testing.T) {
	cityDir := t.TempDir()
	oldResolve := resolveProviderLifecycleGCBinary
	resolveProviderLifecycleGCBinary = func() string { return "/opt/gc/bin/gc" }
	t.Cleanup(func() { resolveProviderLifecycleGCBinary = oldResolve })

	env := gcExecStoreEnv(cityDir, execStoreTarget{
		ScopeRoot: cityDir,
		ScopeKind: "city",
		Prefix:    "gc",
	}, "exec:/tmp/gc-beads-bd")

	if got := env["GC_BIN"]; got != "/opt/gc/bin/gc" {
		t.Fatalf("GC_BIN = %q, want %q", got, "/opt/gc/bin/gc")
	}
}

func TestGcExecStoreEnvDoesNotProjectGCBinForUnrelatedExecProvider(t *testing.T) {
	cityDir := t.TempDir()
	oldResolve := resolveProviderLifecycleGCBinary
	resolveProviderLifecycleGCBinary = func() string { return "/opt/gc/bin/gc" }
	t.Cleanup(func() { resolveProviderLifecycleGCBinary = oldResolve })

	env := gcExecStoreEnv(cityDir, execStoreTarget{
		ScopeRoot: cityDir,
		ScopeKind: "city",
		Prefix:    "gc",
	}, "exec:/tmp/custom-provider")

	if got := env["GC_BIN"]; got != "" {
		t.Fatalf("GC_BIN = %q, want empty for unrelated exec provider", got)
	}
}

func TestResolveConfiguredExecStoreTargetCity(t *testing.T) {
	cityDir := t.TempDir()
	writeExecStoreCityConfig(t, cityDir, "prefix-city", "ct", nil)

	target, err := resolveConfiguredExecStoreTarget(cityDir, cityDir)
	if err != nil {
		t.Fatalf("resolveConfiguredExecStoreTarget(city): %v", err)
	}
	if target.ScopeRoot != cityDir {
		t.Fatalf("ScopeRoot = %q, want %q", target.ScopeRoot, cityDir)
	}
	if target.ScopeKind != "city" {
		t.Fatalf("ScopeKind = %q, want city", target.ScopeKind)
	}
	if target.Prefix != "ct" {
		t.Fatalf("Prefix = %q, want ct", target.Prefix)
	}
	if target.RigName != "" {
		t.Fatalf("RigName = %q, want empty", target.RigName)
	}
}

func TestResolveConfiguredExecStoreTargetRig(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "rig-a")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecStoreCityConfig(t, cityDir, "prefix-city", "ct", []config.Rig{{
		Name:   "rig-a",
		Path:   "rigs/rig-a",
		Prefix: "ra",
	}})

	target, err := resolveConfiguredExecStoreTarget(cityDir, rigDir)
	if err != nil {
		t.Fatalf("resolveConfiguredExecStoreTarget(rig): %v", err)
	}
	if target.ScopeRoot != rigDir {
		t.Fatalf("ScopeRoot = %q, want %q", target.ScopeRoot, rigDir)
	}
	if target.ScopeKind != "rig" {
		t.Fatalf("ScopeKind = %q, want rig", target.ScopeKind)
	}
	if target.Prefix != "ra" {
		t.Fatalf("Prefix = %q, want ra", target.Prefix)
	}
	if target.RigName != "rig-a" {
		t.Fatalf("RigName = %q, want rig-a", target.RigName)
	}
}

func TestGcExecStoreEnvProjectsCityAndRigTargets(t *testing.T) {
	cityDir := t.TempDir()
	cityTarget := execStoreTarget{
		ScopeRoot: cityDir,
		ScopeKind: "city",
		Prefix:    "ct",
	}
	cityEnv := gcExecStoreEnv(cityDir, cityTarget, "exec:/tmp/spy")
	if got := cityEnv["GC_PROVIDER"]; got != "exec:/tmp/spy" {
		t.Fatalf("GC_PROVIDER = %q, want exec:/tmp/spy", got)
	}
	if got := cityEnv["GC_STORE_ROOT"]; got != cityDir {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got, cityDir)
	}
	if got := cityEnv["GC_STORE_SCOPE"]; got != "city" {
		t.Fatalf("GC_STORE_SCOPE = %q, want city", got)
	}
	if got := cityEnv["GC_BEADS_PREFIX"]; got != "ct" {
		t.Fatalf("GC_BEADS_PREFIX = %q, want ct", got)
	}
	if got := cityEnv["GC_RIG"]; got != "" {
		t.Fatalf("GC_RIG = %q, want empty", got)
	}
	if got := cityEnv["GC_RIG_ROOT"]; got != "" {
		t.Fatalf("GC_RIG_ROOT = %q, want empty", got)
	}
	if got := cityEnv["BEADS_DIR"]; got != "" {
		t.Fatalf("BEADS_DIR = %q, want empty", got)
	}
	if got := cityEnv["BEADS_CREDENTIALS_FILE"]; got != "" {
		t.Fatalf("BEADS_CREDENTIALS_FILE = %q, want empty", got)
	}

	rigDir := filepath.Join(cityDir, "rigs", "rig-a")
	rigTarget := execStoreTarget{
		ScopeRoot: rigDir,
		ScopeKind: "rig",
		Prefix:    "ra",
		RigName:   "rig-a",
	}
	rigEnv := gcExecStoreEnv(cityDir, rigTarget, "exec:/tmp/spy")
	if got := rigEnv["GC_STORE_ROOT"]; got != rigDir {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got, rigDir)
	}
	if got := rigEnv["GC_STORE_SCOPE"]; got != "rig" {
		t.Fatalf("GC_STORE_SCOPE = %q, want rig", got)
	}
	if got := rigEnv["GC_BEADS_PREFIX"]; got != "ra" {
		t.Fatalf("GC_BEADS_PREFIX = %q, want ra", got)
	}
	if got := rigEnv["GC_RIG"]; got != "rig-a" {
		t.Fatalf("GC_RIG = %q, want rig-a", got)
	}
	if got := rigEnv["GC_RIG_ROOT"]; got != rigDir {
		t.Fatalf("GC_RIG_ROOT = %q, want %q", got, rigDir)
	}
	if got := rigEnv["BEADS_DIR"]; got != "" {
		t.Fatalf("BEADS_DIR = %q, want empty", got)
	}
	if got := rigEnv["BEADS_CREDENTIALS_FILE"]; got != "" {
		t.Fatalf("BEADS_CREDENTIALS_FILE = %q, want empty", got)
	}
	if got := rigEnv["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("GC_DOLT_HOST = %q, want empty", got)
	}
	if got := rigEnv["GC_DOLT_PORT"]; got != "" {
		t.Fatalf("GC_DOLT_PORT = %q, want empty", got)
	}
}

func TestOpenStoreAtForCityExecProjectsConfiguredTargets(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecStoreCityConfig(t, cityDir, "metro-city", "ct", []config.Rig{{
		Name:   "frontend",
		Path:   "rigs/frontend",
		Prefix: "fe",
	}})
	captureDir := t.TempDir()
	script := writeExecCaptureScript(t, captureDir)
	provider := "exec:" + script

	t.Setenv("GC_BEADS", provider)
	t.Setenv("BEADS_DIR", "/tmp/ambient-beads")
	t.Setenv("GC_DOLT_HOST", "ambient-dolt")
	t.Setenv("GC_STORE_ROOT", "/tmp/ambient-store")

	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	if _, err := cityStore.Create(beads.Bead{Title: "city"}); err != nil {
		t.Fatalf("city Create: %v", err)
	}

	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	if _, err := rigStore.Create(beads.Bead{Title: "rig"}); err != nil {
		t.Fatalf("rig Create: %v", err)
	}

	cityEnv := readExecCaptureEnv(t, filepath.Join(captureDir, "city.env"))
	if got := cityEnv["GC_STORE_ROOT"]; got != cityDir {
		t.Fatalf("city GC_STORE_ROOT = %q, want %q", got, cityDir)
	}
	if got := cityEnv["GC_STORE_SCOPE"]; got != "city" {
		t.Fatalf("city GC_STORE_SCOPE = %q, want city", got)
	}
	if got := cityEnv["GC_BEADS_PREFIX"]; got != "ct" {
		t.Fatalf("city GC_BEADS_PREFIX = %q, want ct", got)
	}
	if got := cityEnv["GC_PROVIDER"]; got != provider {
		t.Fatalf("city GC_PROVIDER = %q, want %q", got, provider)
	}
	if got := cityEnv["GC_CITY_PATH"]; got != cityDir {
		t.Fatalf("city GC_CITY_PATH = %q, want %q", got, cityDir)
	}
	if got := cityEnv["GC_RIG"]; got != "" {
		t.Fatalf("city GC_RIG = %q, want empty", got)
	}
	if got := cityEnv["GC_RIG_ROOT"]; got != "" {
		t.Fatalf("city GC_RIG_ROOT = %q, want empty", got)
	}
	if got := cityEnv["BEADS_DIR"]; got != "" {
		t.Fatalf("city BEADS_DIR leaked as %q", got)
	}
	if got := cityEnv["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("city GC_DOLT_HOST leaked as %q", got)
	}

	rigEnv := readExecCaptureEnv(t, filepath.Join(captureDir, "frontend.env"))
	if got := rigEnv["GC_STORE_ROOT"]; got != rigDir {
		t.Fatalf("rig GC_STORE_ROOT = %q, want %q", got, rigDir)
	}
	if got := rigEnv["GC_STORE_SCOPE"]; got != "rig" {
		t.Fatalf("rig GC_STORE_SCOPE = %q, want rig", got)
	}
	if got := rigEnv["GC_BEADS_PREFIX"]; got != "fe" {
		t.Fatalf("rig GC_BEADS_PREFIX = %q, want fe", got)
	}
	if got := rigEnv["GC_RIG"]; got != "frontend" {
		t.Fatalf("rig GC_RIG = %q, want frontend", got)
	}
	if got := rigEnv["GC_RIG_ROOT"]; got != rigDir {
		t.Fatalf("rig GC_RIG_ROOT = %q, want %q", got, rigDir)
	}
	if got := rigEnv["GC_PROVIDER"]; got != provider {
		t.Fatalf("rig GC_PROVIDER = %q, want %q", got, provider)
	}
	if got := rigEnv["BEADS_DIR"]; got != "" {
		t.Fatalf("rig BEADS_DIR leaked as %q", got)
	}
	if got := rigEnv["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("rig GC_DOLT_HOST leaked as %q", got)
	}
}

func TestOpenStoreAtForCityExecBeadsBdProjectsScopedExternalDoltEnv(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecStoreCityConfig(t, cityDir, "metro-city", "ct", []config.Rig{{
		Name:   "frontend",
		Path:   "rigs/frontend",
		Prefix: "fe",
	}})
	cityCfg := strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: city_canonical",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"dolt.host: db.example.internal",
		"dolt.port: 3317",
		"",
	}, "\n")
	rigCfg := strings.Join([]string{
		"issue_prefix: fe",
		"gc.endpoint_origin: inherited_city",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"dolt.host: db.example.internal",
		"dolt.port: 3317",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(cityCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(rigCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	captureDir := t.TempDir()
	script := writeNamedExecCaptureScript(t, captureDir, "gc-beads-bd")
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_DOLT_HOST", "ambient-dolt")
	t.Setenv("GC_DOLT_PORT", "9999")

	store, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	if _, err := store.Create(beads.Bead{Title: "rig"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rigEnv := readExecCaptureEnv(t, filepath.Join(captureDir, "frontend.env"))
	if got := rigEnv["GC_STORE_ROOT"]; got != rigDir {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got, rigDir)
	}
	if got := rigEnv["GC_DOLT_HOST"]; got != "db.example.internal" {
		t.Fatalf("GC_DOLT_HOST = %q, want db.example.internal", got)
	}
	if got := rigEnv["GC_DOLT_PORT"]; got != "3317" {
		t.Fatalf("GC_DOLT_PORT = %q, want 3317", got)
	}
	if got := rigEnv["BEADS_DIR"]; got != "" {
		t.Fatalf("BEADS_DIR leaked as %q", got)
	}
}

func TestControllerStateOpenRigStoreExecProjectsRigTarget(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	captureDir := t.TempDir()
	script := writeExecCaptureScript(t, captureDir)
	provider := "exec:" + script

	t.Setenv("BEADS_DIR", "/tmp/ambient-beads")
	t.Setenv("GC_DOLT_HOST", "ambient-dolt")

	cs := &controllerState{cityPath: cityDir}
	store := cs.openRigStore(provider, "frontend", rigDir, "fe", nil)
	if _, err := store.Create(beads.Bead{Title: "rig"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rigEnv := readExecCaptureEnv(t, filepath.Join(captureDir, "frontend.env"))
	if got := rigEnv["GC_STORE_ROOT"]; got != rigDir {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got, rigDir)
	}
	if got := rigEnv["GC_STORE_SCOPE"]; got != "rig" {
		t.Fatalf("GC_STORE_SCOPE = %q, want rig", got)
	}
	if got := rigEnv["GC_BEADS_PREFIX"]; got != "fe" {
		t.Fatalf("GC_BEADS_PREFIX = %q, want fe", got)
	}
	if got := rigEnv["GC_RIG"]; got != "frontend" {
		t.Fatalf("GC_RIG = %q, want frontend", got)
	}
	if got := rigEnv["GC_RIG_ROOT"]; got != rigDir {
		t.Fatalf("GC_RIG_ROOT = %q, want %q", got, rigDir)
	}
	if got := rigEnv["GC_PROVIDER"]; got != provider {
		t.Fatalf("GC_PROVIDER = %q, want %q", got, provider)
	}
	if got := rigEnv["BEADS_DIR"]; got != "" {
		t.Fatalf("BEADS_DIR leaked as %q", got)
	}
	if got := rigEnv["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("GC_DOLT_HOST leaked as %q", got)
	}
}

func TestControllerStateOpenRigStoreExecBdProjectsRigDoltEnv(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	captureDir := t.TempDir()
	script := writeNamedExecCaptureScript(t, captureDir, "gc-beads-bd.sh")
	provider := "exec:" + script

	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:     "frontend",
			Path:     rigDir,
			Prefix:   "fe",
			DoltHost: "rig-db.example.com",
			DoltPort: "3308",
		}},
	}

	t.Setenv("GC_DOLT_HOST", "ambient-dolt")
	t.Setenv("GC_DOLT_PORT", "9911")

	cs := &controllerState{cityPath: cityDir, cfg: cfg}
	store := cs.openRigStore(provider, "frontend", rigDir, "fe", cfg)
	if _, err := store.Create(beads.Bead{Title: "rig"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rigEnv := readExecCaptureEnv(t, filepath.Join(captureDir, "frontend.env"))
	if got := rigEnv["GC_DOLT_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want rig-db.example.com", got)
	}
	if got := rigEnv["GC_DOLT_PORT"]; got != "3308" {
		t.Fatalf("GC_DOLT_PORT = %q, want 3308", got)
	}
}

func TestOpenStoreAtForCityExecUsesUniversalStoreTargetEnv(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecStoreCityConfig(t, cityDir, "metro-city", "ct", []config.Rig{{
		Name:   "frontend",
		Path:   "rigs/frontend",
		Prefix: "fe",
	}})
	captureDir := t.TempDir()
	script := writeExecCaptureScript(t, captureDir)
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("BEADS_DIR", "/tmp/ambient-beads")
	t.Setenv("GC_DOLT_HOST", "ambient-dolt")

	store, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	if _, err := store.Create(beads.Bead{Title: "rig"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rigEnv := readExecCaptureEnv(t, filepath.Join(captureDir, "frontend.env"))
	if got := rigEnv["GC_STORE_ROOT"]; got != rigDir {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got, rigDir)
	}
	if got := rigEnv["GC_STORE_SCOPE"]; got != "rig" {
		t.Fatalf("GC_STORE_SCOPE = %q, want rig", got)
	}
	if got := rigEnv["GC_BEADS_PREFIX"]; got != "fe" {
		t.Fatalf("GC_BEADS_PREFIX = %q, want fe", got)
	}
	if got := rigEnv["GC_RIG"]; got != "frontend" {
		t.Fatalf("GC_RIG = %q, want frontend", got)
	}
	if got := rigEnv["BEADS_DIR"]; got != "" {
		t.Fatalf("BEADS_DIR leaked as %q", got)
	}
	if got := rigEnv["GC_DOLT_HOST"]; got != "" {
		t.Fatalf("GC_DOLT_HOST leaked as %q", got)
	}
}
