package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestExtractRigFlag(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantRig  string
		wantArgs []string
	}{
		{
			name:     "no rig flag",
			args:     []string{"list", "--limit", "5"},
			wantRig:  "",
			wantArgs: []string{"list", "--limit", "5"},
		},
		{
			name:     "rig flag with space",
			args:     []string{"--rig", "myproject", "list"},
			wantRig:  "myproject",
			wantArgs: []string{"list"},
		},
		{
			name:     "rig flag with equals",
			args:     []string{"--rig=myproject", "list"},
			wantRig:  "myproject",
			wantArgs: []string{"list"},
		},
		{
			name:     "rig flag in middle",
			args:     []string{"show", "--rig", "myproject", "BL-42"},
			wantRig:  "myproject",
			wantArgs: []string{"show", "BL-42"},
		},
		{
			name:     "empty args",
			args:     nil,
			wantRig:  "",
			wantArgs: nil,
		},
		{
			name:     "rig flag at end missing value",
			args:     []string{"list", "--rig"},
			wantRig:  "",
			wantArgs: []string{"list", "--rig"},
		},
	}

	origRigFlag := rigFlag
	defer func() { rigFlag = origRigFlag }()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rigFlag = ""
			gotRig, gotArgs := extractRigFlag(tt.args)
			if gotRig != tt.wantRig {
				t.Errorf("rig = %q, want %q", gotRig, tt.wantRig)
			}
			if len(gotArgs) != len(tt.wantArgs) {
				t.Fatalf("args len = %d, want %d; got %v", len(gotArgs), len(tt.wantArgs), gotArgs)
			}
			for i := range gotArgs {
				if gotArgs[i] != tt.wantArgs[i] {
					t.Errorf("args[%d] = %q, want %q", i, gotArgs[i], tt.wantArgs[i])
				}
			}
		})
	}
}

func TestExtractRigFlagFallsBackToGlobal(t *testing.T) {
	origRigFlag := rigFlag
	defer func() { rigFlag = origRigFlag }()

	rigFlag = "from-global"
	gotRig, gotArgs := extractRigFlag([]string{"list"})
	if gotRig != "from-global" {
		t.Errorf("rig = %q, want %q", gotRig, "from-global")
	}
	if len(gotArgs) != 1 || gotArgs[0] != "list" {
		t.Errorf("args = %v, want [list]", gotArgs)
	}
}

func TestExtractBdScopeFlags(t *testing.T) {
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	}()

	cityFlag = ""
	rigFlag = ""
	gotCity, gotRig, gotArgs := extractBdScopeFlags([]string{"--city=/tmp/city", "--rig", "repo", "context", "--json"})
	if gotCity != "/tmp/city" {
		t.Fatalf("city = %q, want %q", gotCity, "/tmp/city")
	}
	if gotRig != "repo" {
		t.Fatalf("rig = %q, want %q", gotRig, "repo")
	}
	wantArgs := []string{"context", "--json"}
	if len(gotArgs) != len(wantArgs) {
		t.Fatalf("args len = %d, want %d; got %v", len(gotArgs), len(wantArgs), gotArgs)
	}
	for i := range wantArgs {
		if gotArgs[i] != wantArgs[i] {
			t.Fatalf("args[%d] = %q, want %q", i, gotArgs[i], wantArgs[i])
		}
	}

	cityFlag = "/flag-city"
	rigFlag = "flag-rig"
	gotCity, gotRig, gotArgs = extractBdScopeFlags([]string{"list"})
	if gotCity != "/flag-city" {
		t.Fatalf("fallback city = %q, want %q", gotCity, "/flag-city")
	}
	if gotRig != "flag-rig" {
		t.Fatalf("fallback rig = %q, want %q", gotRig, "flag-rig")
	}
	if len(gotArgs) != 1 || gotArgs[0] != "list" {
		t.Fatalf("fallback args = %v, want [list]", gotArgs)
	}
}

func TestResolveBdScopeTarget(t *testing.T) {
	origProbe := bdBeadExists
	defer func() { bdBeadExists = origProbe }()
	bdBeadExists = func(_ string, _ execStoreTarget, beadID string) bool {
		return beadID == "projectwrenunity-0xk" || beadID == "projectwrenunity-abc"
	}
	cityDir := filepath.Join(t.TempDir(), "city")
	cfgForTest := func() *config.City {
		return &config.City{
			Workspace: config.Workspace{Name: "gascity"},
			Rigs: []config.Rig{
				{Name: "wren", Path: filepath.Join("rigs", "wren"), Prefix: "projectwrenunity"},
				{Name: "gascity", Path: filepath.Join("rigs", "gascity")},
			},
		}
	}

	tests := []struct {
		name      string
		rigName   string
		args      []string
		want      execStoreTarget
		wantError string
	}{
		{
			name:    "explicit rig name",
			rigName: "wren",
			args:    []string{"list"},
			want: execStoreTarget{
				ScopeRoot: filepath.Join(cityDir, "rigs", "wren"),
				ScopeKind: "rig",
				Prefix:    "projectwrenunity",
				RigName:   "wren",
			},
		},
		{
			name:    "explicit rig name case insensitive",
			rigName: "Wren",
			args:    []string{"list"},
			want: execStoreTarget{
				ScopeRoot: filepath.Join(cityDir, "rigs", "wren"),
				ScopeKind: "rig",
				Prefix:    "projectwrenunity",
				RigName:   "wren",
			},
		},
		{
			name:    "auto-detect from bead prefix",
			rigName: "",
			args:    []string{"show", "projectwrenunity-0xk"},
			want: execStoreTarget{
				ScopeRoot: filepath.Join(cityDir, "rigs", "wren"),
				ScopeKind: "rig",
				Prefix:    "projectwrenunity",
				RigName:   "wren",
			},
		},
		{
			name:    "no rig falls back to city",
			rigName: "",
			args:    []string{"list"},
			want: execStoreTarget{
				ScopeRoot: cityDir,
				ScopeKind: "city",
				Prefix:    "ga",
			},
		},
		{
			name:      "unknown explicit rig errors",
			rigName:   "nonexistent",
			args:      []string{"show", "projectwrenunity-abc"},
			wantError: `rig "nonexistent" not found`,
		},
		{
			name:    "skips flags during auto-detect",
			rigName: "",
			args:    []string{"list", "--status", "open"},
			want: execStoreTarget{
				ScopeRoot: cityDir,
				ScopeKind: "city",
				Prefix:    "ga",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveBdScopeTarget(cfgForTest(), cityDir, tt.rigName, tt.args)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("resolveBdScopeTarget() error = %v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveBdScopeTarget() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveBdScopeTarget() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestResolveBdScopeTargetUsesRedirectedWorktreeRig(t *testing.T) {
	cityDir := t.TempDir()
	worktreeDir := filepath.Join(cityDir, ".gc", "worktrees", "frontend", "polecats", "polecat-1")
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(filepath.Join(worktreeDir, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(worktree .beads): %v", err)
	}
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rigDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, ".beads", "redirect"), []byte(filepath.Join(rigDir, ".beads")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(redirect): %v", err)
	}
	setCwd(t, worktreeDir)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gascity"},
		Rigs:      []config.Rig{{Name: "frontend", Path: filepath.Join("rigs", "frontend"), Prefix: "fr"}},
	}
	got, err := resolveBdScopeTarget(cfg, cityDir, "", []string{"list"})
	if err != nil {
		t.Fatalf("resolveBdScopeTarget() error = %v", err)
	}
	want := execStoreTarget{
		ScopeRoot: rigDir,
		ScopeKind: "rig",
		Prefix:    "fr",
		RigName:   "frontend",
	}
	if got != want {
		t.Fatalf("resolveBdScopeTarget() = %#v, want %#v", got, want)
	}
}

func TestResolveBdScopeTargetErrorsOnForeignRedirect(t *testing.T) {
	cityDir := t.TempDir()
	worktreeDir := filepath.Join(cityDir, ".gc", "worktrees", "frontend", "polecats", "polecat-1")
	foreignDir := filepath.Join(t.TempDir(), "foreign")
	if err := os.MkdirAll(filepath.Join(worktreeDir, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(worktree .beads): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(foreignDir, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(foreign .beads): %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, ".beads", "redirect"), []byte(filepath.Join(foreignDir, ".beads")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(redirect): %v", err)
	}
	setCwd(t, worktreeDir)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gascity"},
		Rigs:      []config.Rig{{Name: "frontend", Path: filepath.Join("rigs", "frontend"), Prefix: "fr"}},
	}
	_, err := resolveBdScopeTarget(cfg, cityDir, "", []string{"list"})
	if err == nil || !strings.Contains(err.Error(), "points outside declared city rigs") {
		t.Fatalf("resolveBdScopeTarget() error = %v, want foreign redirect error", err)
	}
}

func TestBdCommandEnvUsesCanonicalRigTarget(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("BEADS_ACTOR")

	cityDir := t.TempDir()
	wantPort := strconv.Itoa(writeReachableManagedDoltState(t, cityDir))
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
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}
	env := listToMap(bdCommandEnv(cityDir, cfg, execStoreTarget{
		ScopeRoot: rigDir,
		ScopeKind: "rig",
		Prefix:    "repo",
		RigName:   "repo",
	}))
	if got := env["GC_DOLT_PORT"]; got != wantPort {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, wantPort)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != wantPort {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, wantPort)
	}
	if got := env["BEADS_DIR"]; got != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got, filepath.Join(rigDir, ".beads"))
	}
	if got := env["GC_RIG"]; got != "repo" {
		t.Fatalf("GC_RIG = %q, want %q", got, "repo")
	}
	if got := env["GC_STORE_ROOT"]; got != rigDir {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got, rigDir)
	}
	if got := env["GC_STORE_SCOPE"]; got != "rig" {
		t.Fatalf("GC_STORE_SCOPE = %q, want %q", got, "rig")
	}
	if got := env["GC_BEADS_PREFIX"]; got != "repo" {
		t.Fatalf("GC_BEADS_PREFIX = %q, want %q", got, "repo")
	}
	if _, present := env["BEADS_ACTOR"]; present {
		t.Fatalf("BEADS_ACTOR = %q, want absent for direct gc bd env without explicit actor", env["BEADS_ACTOR"])
	}
}

func TestBdCommandRunnerForCityDoesNotDefaultBeadsActorWhenUnset(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	_ = os.Unsetenv("BEADS_ACTOR")

	origRunner := beadsExecCommandRunnerWithEnv
	t.Cleanup(func() { beadsExecCommandRunnerWithEnv = origRunner })

	var captured map[string]string
	beadsExecCommandRunnerWithEnv = func(env map[string]string) beads.CommandRunner {
		captured = map[string]string{}
		for key, value := range env {
			captured[key] = value
		}
		return func(_ string, _ string, _ ...string) ([]byte, error) {
			return []byte("ok"), nil
		}
	}

	cityPath := t.TempDir()
	runner := bdCommandRunnerForCity(cityPath)
	if _, err := runner(cityPath, "bd", "list", "--json"); err != nil {
		t.Fatalf("bd runner error = %v, want nil", err)
	}

	if _, present := captured["BEADS_ACTOR"]; present {
		t.Fatalf("BEADS_ACTOR = %q, want absent for normal bd runner without explicit actor", captured["BEADS_ACTOR"])
	}
}

func TestGcBdUsesProjectionNotAmbientEnv(t *testing.T) {
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	origProbe := bdBeadExists
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
		bdBeadExists = origProbe
	}()
	bdBeadExists = func(_ string, _ execStoreTarget, beadID string) bool {
		return beadID == "repo-abc"
	}
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	wantPort := strconv.Itoa(writeReachableManagedDoltState(t, cityDir))
	rigDir := filepath.Join(cityDir, "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[[rigs]]
name = "repo"
path = "repo"
prefix = "repo"
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

	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "gc-bd-env.txt")
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
{
  printf 'pwd=%s\n' "$PWD"
  printf 'args=%s\n' "$*"
  printf 'GC_STORE_ROOT=%s\n' "${GC_STORE_ROOT:-}"
  printf 'GC_STORE_SCOPE=%s\n' "${GC_STORE_SCOPE:-}"
  printf 'GC_BEADS_PREFIX=%s\n' "${GC_BEADS_PREFIX:-}"
  printf 'GC_DOLT_HOST=%s\n' "${GC_DOLT_HOST:-}"
  printf 'GC_DOLT_PORT=%s\n' "${GC_DOLT_PORT:-}"
  printf 'BEADS_DOLT_SERVER_HOST=%s\n' "${BEADS_DOLT_SERVER_HOST:-}"
  printf 'BEADS_DOLT_SERVER_PORT=%s\n' "${BEADS_DOLT_SERVER_PORT:-}"
  printf 'BEADS_DIR=%s\n' "${BEADS_DIR:-}"
  printf 'GC_RIG=%s\n' "${GC_RIG:-}"
  printf 'GC_RIG_ROOT=%s\n' "${GC_RIG_ROOT:-}"
} > "${CAPTURE_PATH}"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+origPath)
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_DOLT_HOST", "ambient-dolt.example.com")
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient-beads.example.com")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	t.Setenv("BEADS_DIR", "/ambient/.beads")
	t.Setenv("GC_STORE_ROOT", "/ambient/store")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"show", "repo-abc"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd() = %d, want 0; stderr=%q", got, stderr.String())
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
	if !samePath(got["pwd"], rigDir) {
		t.Fatalf("pwd = %q, want %q", got["pwd"], rigDir)
	}
	if got["args"] != "show repo-abc" {
		t.Fatalf("args = %q, want %q", got["args"], "show repo-abc")
	}
	if !samePath(got["GC_STORE_ROOT"], rigDir) {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got["GC_STORE_ROOT"], rigDir)
	}
	if got["GC_STORE_SCOPE"] != "rig" {
		t.Fatalf("GC_STORE_SCOPE = %q, want %q", got["GC_STORE_SCOPE"], "rig")
	}
	if got["GC_BEADS_PREFIX"] != "repo" {
		t.Fatalf("GC_BEADS_PREFIX = %q, want %q", got["GC_BEADS_PREFIX"], "repo")
	}
	if got["GC_DOLT_HOST"] != "" {
		t.Fatalf("GC_DOLT_HOST = %q, want empty for managed target", got["GC_DOLT_HOST"])
	}
	if got["GC_DOLT_PORT"] != wantPort {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got["GC_DOLT_PORT"], wantPort)
	}
	if got["BEADS_DOLT_SERVER_HOST"] != "" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want empty for managed target", got["BEADS_DOLT_SERVER_HOST"])
	}
	if got["BEADS_DOLT_SERVER_PORT"] != wantPort {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got["BEADS_DOLT_SERVER_PORT"], wantPort)
	}
	if !samePath(got["BEADS_DIR"], filepath.Join(rigDir, ".beads")) {
		t.Fatalf("BEADS_DIR = %q, want %q", got["BEADS_DIR"], filepath.Join(rigDir, ".beads"))
	}
	if got["GC_RIG"] != "repo" {
		t.Fatalf("GC_RIG = %q, want %q", got["GC_RIG"], "repo")
	}
	if !samePath(got["GC_RIG_ROOT"], rigDir) {
		t.Fatalf("GC_RIG_ROOT = %q, want %q", got["GC_RIG_ROOT"], rigDir)
	}
}

func TestGcBdSuppressesBdAutoExportInChildEnv(t *testing.T) {
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	}()
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
if [ "${BD_EXPORT_AUTO:-}" != "false" ]; then
  echo "BD_EXPORT_AUTO=${BD_EXPORT_AUTO:-}" >&2
  exit 73
fi
case "${1:-}" in
  show)
    printf '[{"id":"gc-1","title":"ok"}]\n'
    ;;
  update)
    printf '{"id":"gc-1","status":"in_progress"}\n'
    ;;
  *)
    exit 2
    ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("BD_EXPORT_AUTO", "true")

	for _, args := range [][]string{
		{"show", "gc-1", "--json"},
		{"update", "gc-1", "--claim", "--json"},
	} {
		var stdout, stderr bytes.Buffer
		if got := doBd(args, &stdout, &stderr); got != 0 {
			t.Fatalf("doBd(%v) = %d, want 0; stdout=%q stderr=%q", args, got, stdout.String(), stderr.String())
		}
		if strings.TrimSpace(stdout.String()) == "" {
			t.Fatalf("doBd(%v) produced empty stdout", args)
		}
		if stderr.String() != "" {
			t.Fatalf("doBd(%v) stderr = %q, want empty", args, stderr.String())
		}
	}
}

func TestGcBdDoesNotAutoRouteHyphenatedFlagValue(t *testing.T) {
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	origProbe := bdBeadExists
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
		bdBeadExists = origProbe
	}()
	cityFlag = ""
	rigFlag = ""
	bdBeadExists = func(string, execStoreTarget, string) bool { return false }

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "repo")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[[rigs]]
name = "repo"
path = "repo"
prefix = "repo"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "gc-bd-city-env.txt")
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
{
  printf 'pwd=%s\n' "$PWD"
  printf 'args=%s\n' "$*"
  printf 'GC_STORE_ROOT=%s\n' "${GC_STORE_ROOT:-}"
  printf 'GC_STORE_SCOPE=%s\n' "${GC_STORE_SCOPE:-}"
  printf 'GC_BEADS_PREFIX=%s\n' "${GC_BEADS_PREFIX:-}"
  printf 'BEADS_DIR=%s\n' "${BEADS_DIR:-}"
} > "${CAPTURE_PATH}"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+origPath)
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("GC_CITY_PATH", cityDir)

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"list", "--label", "repo-open"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd() = %d, want 0; stderr=%q", got, stderr.String())
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
	if !samePath(got["pwd"], cityDir) {
		t.Fatalf("pwd = %q, want %q", got["pwd"], cityDir)
	}
	if got["args"] != "list --label repo-open" {
		t.Fatalf("args = %q, want %q", got["args"], "list --label repo-open")
	}
	if !samePath(got["GC_STORE_ROOT"], cityDir) {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got["GC_STORE_ROOT"], cityDir)
	}
	if got["GC_STORE_SCOPE"] != "city" {
		t.Fatalf("GC_STORE_SCOPE = %q, want %q", got["GC_STORE_SCOPE"], "city")
	}
	if got["GC_BEADS_PREFIX"] != "de" {
		t.Fatalf("GC_BEADS_PREFIX = %q, want %q", got["GC_BEADS_PREFIX"], "de")
	}
	if !samePath(got["BEADS_DIR"], filepath.Join(cityDir, ".beads")) {
		t.Fatalf("BEADS_DIR = %q, want %q", got["BEADS_DIR"], filepath.Join(cityDir, ".beads"))
	}
}

func TestGcBdRejectsGCBeadsFileOverride(t *testing.T) {
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	}()
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"list"}, &stdout, &stderr); got == 0 {
		t.Fatalf("doBd() = %d, want non-zero", got)
	}
	if !strings.Contains(stderr.String(), "only supported for bd-backed beads providers") {
		t.Fatalf("stderr = %q, want provider error", stderr.String())
	}
}

func TestGcBdRejectsNonBdProvider(t *testing.T) {
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	}()
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY_PATH", cityDir)

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"list"}, &stdout, &stderr); got == 0 {
		t.Fatalf("doBd() = %d, want non-zero", got)
	}
	if !strings.Contains(stderr.String(), "only supported for bd-backed beads providers") {
		t.Fatalf("stderr = %q, want provider error", stderr.String())
	}
}

// TestGcBdRejectsStaleFileMarkerWithDiagnosticHint asserts the error when
// a scope has a stale .gc/beads.json (file-store marker) but no
// .beads/metadata.json (bd-store marker): gc rejects with a hint that
// names the offending marker and suggests the fix. Regression for the
// post-#899 behavior change where stale migration artifacts silently
// reclassified rigs as file-backed with no diagnostic.
func TestGcBdRejectsStaleFileMarkerWithDiagnosticHint(t *testing.T) {
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	}()
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "legacy-rig")
	if err := os.MkdirAll(filepath.Join(rigDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"

[[rigs]]
name = "legacy-rig"
path = "legacy-rig"
prefix = "lg"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".gc", "beads.json"), []byte(`{"seq":1,"beads":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY_PATH", cityDir)

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"--rig", "legacy-rig", "list"}, &stdout, &stderr); got == 0 {
		t.Fatalf("doBd() = %d, want non-zero", got)
	}
	out := stderr.String()
	if !strings.Contains(out, `resolved "file"`) {
		t.Fatalf("stderr = %q, want named provider in error", out)
	}
	if !strings.Contains(out, ".gc/beads.json") {
		t.Fatalf("stderr = %q, want named marker in hint", out)
	}
	if !strings.Contains(out, ".beads/metadata.json") {
		t.Fatalf("stderr = %q, want named fix in hint", out)
	}
}

func TestGcBdAllowsRigPassthroughForBdBackedRigUnderFileCity(t *testing.T) {
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	}()
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
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

	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "gc-bd-mixed-provider.txt")
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
{
  printf 'pwd=%s\n' "$PWD"
  printf 'args=%s\n' "$*"
  printf 'BEADS_DIR=%s\n' "${BEADS_DIR:-}"
} > "${CAPTURE_PATH}"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("GC_CITY_PATH", cityDir)

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"--rig", "frontend", "list"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd() = %d, want 0; stderr=%q", got, stderr.String())
	}
	if strings.Contains(stderr.String(), "only supported for bd-backed beads providers") {
		t.Fatalf("stderr = %q, want rig passthrough instead of provider gate", stderr.String())
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
	if !samePath(got["pwd"], rigDir) {
		t.Fatalf("pwd = %q, want %q", got["pwd"], rigDir)
	}
	if got["args"] != "list" {
		t.Fatalf("args = %q, want %q", got["args"], "list")
	}
	if !samePath(got["BEADS_DIR"], filepath.Join(rigDir, ".beads")) {
		t.Fatalf("BEADS_DIR = %q, want %q", got["BEADS_DIR"], filepath.Join(rigDir, ".beads"))
	}
}

func runRawBDFromDir(t *testing.T, bdPath, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bdPath, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("raw bd %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func parseCreatedBeadID(t *testing.T, out string) string {
	t.Helper()
	idx := strings.Index(out, "{")
	if idx < 0 {
		t.Fatalf("create output missing JSON: %s", out)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out[idx:]), &created); err != nil {
		t.Fatalf("parse create JSON: %v\n%s", err, out)
	}
	if created.ID == "" {
		t.Fatalf("create output missing id: %s", out)
	}
	return created.ID
}

func TestGcBdRigListRecoversAfterManagedHardKillPortRebind(t *testing.T) {
	cityPath, rigPath := setupManagedBdWaitTestCity(t)
	bdPath := waitTestRealBDPath(t)
	rawDir := filepath.Join(rigPath, "nested-rebind")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rawDir): %v", err)
	}
	rawID := parseCreatedBeadID(t, runRawBDFromDir(t, bdPath, rawDir, "create", "--json", "rig rebind bead", "-t", "task"))

	before, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile(before): %v", err)
	}
	if before.PID <= 0 || before.Port <= 0 {
		t.Fatalf("unexpected managed runtime before fault: %+v", before)
	}
	if err := syscall.Kill(before.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("Kill(%d): %v", before.PID, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for pidAlive(before.PID) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}

	occupyManagedDoltPort(t, before.Port)

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"--city", cityPath, "--rig", "frontend", "list", "--json", "--all", "--limit=0"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc bd rig list = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), rawID) {
		t.Fatalf("gc bd rig list output missing bead %q:\n%s", rawID, stdout.String())
	}

	var after doltRuntimeState
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		state, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
		if err == nil && state.Running && state.Port > 0 && state.Port != before.Port && state.PID > 0 && pidAlive(state.PID) {
			after = state
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if after.Port == 0 {
		after, err = readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
		if err != nil {
			t.Fatalf("readDoltRuntimeStateFile(after): %v", err)
		}
		t.Fatalf("managed Dolt did not rebind after hard kill; before=%+v after=%+v", before, after)
	}
	rawList := runRawBDFromDir(t, bdPath, rawDir, "list", "--json", "--all", "--limit=0")
	if !strings.Contains(rawList, rawID) {
		t.Fatalf("raw bd rig list output missing bead %q after rebind:\n%s", rawID, rawList)
	}
	rawShow := runRawBDFromDir(t, bdPath, rawDir, "show", "--json", rawID)
	if !strings.Contains(rawShow, rawID) {
		t.Fatalf("raw bd rig show output missing bead %q after rebind:\n%s", rawID, rawShow)
	}
}

func TestManagedBdRigProviderStoreRecoversAfterHardKillPortRebind(t *testing.T) {
	cityPath, rigPath := setupManagedBdWaitTestCity(t)
	bdPath := waitTestRealBDPath(t)
	rawDir := filepath.Join(rigPath, "provider-rebind")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rawDir): %v", err)
	}

	rawID := parseCreatedBeadID(t, runRawBDFromDir(t, bdPath, rawDir, "create", "--json", "provider rebind bead", "-t", "task"))
	providerStore, err := openStoreAtForCity(rigPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	if got, err := providerStore.Get(rawID); err != nil {
		t.Fatalf("providerStore.Get(rawID) before rebind: %v", err)
	} else if got.ID != rawID {
		t.Fatalf("providerStore.Get(rawID).ID = %q, want %q", got.ID, rawID)
	}

	before, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile(before): %v", err)
	}
	if before.PID <= 0 || before.Port <= 0 {
		t.Fatalf("unexpected managed runtime before fault: %+v", before)
	}
	if err := syscall.Kill(before.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("Kill(%d): %v", before.PID, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for pidAlive(before.PID) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}

	occupyManagedDoltPort(t, before.Port)

	t.Setenv("GC_DOLT_PORT", "9999")
	if got, err := providerStore.Get(rawID); err != nil {
		t.Fatalf("providerStore.Get(rawID) after rebind: %v", err)
	} else if got.ID != rawID {
		t.Fatalf("providerStore.Get(rawID) after rebind ID = %q, want %q", got.ID, rawID)
	}

	rebound, err := providerStore.Create(beads.Bead{Title: "provider rebind bead after recovery", Type: "task"})
	if err != nil {
		t.Fatalf("providerStore.Create after rebind: %v", err)
	}
	if got := beadPrefix(nil, rebound.ID); got != "fe" {
		t.Fatalf("provider rebind bead prefix = %q, want %q", got, "fe")
	}

	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		after, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
		if err == nil && after.Running && after.Port > 0 && after.Port != before.Port && after.PID > 0 && pidAlive(after.PID) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	after, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile(after): %v", err)
	}
	t.Fatalf("managed Dolt did not rebind for provider store; before=%+v after=%+v", before, after)
}

func TestManagedBdRigStoreConsistentAcrossRawBdGcBdAndProviderStore(t *testing.T) {
	cityPath, rigPath := setupManagedBdWaitTestCity(t)
	bdPath := waitTestRealBDPath(t)
	rawDir := filepath.Join(rigPath, "nested")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rawDir): %v", err)
	}

	rawID := parseCreatedBeadID(t, runRawBDFromDir(t, bdPath, rawDir, "create", "--json", "raw mixed bead", "-t", "task"))
	providerStore, err := openStoreAtForCity(rigPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	if got, err := providerStore.Get(rawID); err != nil {
		t.Fatalf("providerStore.Get(rawID): %v", err)
	} else if got.ID != rawID {
		t.Fatalf("providerStore.Get(rawID).ID = %q, want %q", got.ID, rawID)
	}

	t.Setenv("GC_DOLT_PORT", "9999")
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"--city", cityPath, "--rig", "frontend", "show", rawID}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc bd show rawID = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), rawID) {
		t.Fatalf("gc bd show output missing raw id %q:\n%s", rawID, stdout.String())
	}

	providerBead, err := providerStore.Create(beads.Bead{Title: "provider mixed bead", Type: "task"})
	if err != nil {
		t.Fatalf("providerStore.Create: %v", err)
	}
	if got := beadPrefix(nil, providerBead.ID); got != "fe" {
		t.Fatalf("provider rig bead prefix = %q, want %q", got, "fe")
	}
	rawShow := runRawBDFromDir(t, bdPath, rawDir, "show", "--json", providerBead.ID)
	if !strings.Contains(rawShow, providerBead.ID) {
		t.Fatalf("raw bd show missing provider-created bead %q:\n%s", providerBead.ID, rawShow)
	}
	stdout.Reset()
	stderr.Reset()
	if code := doBd([]string{"--city", cityPath, "--rig", "frontend", "show", providerBead.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc bd show provider bead = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), providerBead.ID) {
		t.Fatalf("gc bd show output missing provider bead %q:\n%s", providerBead.ID, stdout.String())
	}
}

func TestManagedExecBdRigStoreConsistentAcrossRawBdAndProviderStore(t *testing.T) {
	cityPath, rigPath := setupManagedBdWaitTestCity(t)
	bdPath := waitTestRealBDPath(t)
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(cityPath))
	rawDir := filepath.Join(rigPath, "nested-exec")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rawDir): %v", err)
	}

	providerStore, err := openStoreAtForCity(rigPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	providerBead, err := providerStore.Create(beads.Bead{Title: "provider exec bead", Type: "task"})
	if err != nil {
		t.Fatalf("providerStore.Create: %v", err)
	}
	if rawShow := runRawBDFromDir(t, bdPath, rawDir, "show", "--json", providerBead.ID); !strings.Contains(rawShow, providerBead.ID) {
		t.Fatalf("raw bd show missing provider-created bead %q:\n%s", providerBead.ID, rawShow)
	}
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"--city", cityPath, "--rig", "frontend", "show", providerBead.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc bd show provider bead = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), providerBead.ID) {
		t.Fatalf("gc bd show output missing provider bead %q:\n%s", providerBead.ID, stdout.String())
	}

	rawID := parseCreatedBeadID(t, runRawBDFromDir(t, bdPath, rawDir, "create", "--json", "raw exec bead", "-t", "task"))
	if got, err := providerStore.Get(rawID); err != nil {
		t.Fatalf("providerStore.Get(rawID): %v", err)
	} else if got.ID != rawID {
		t.Fatalf("providerStore.Get(rawID).ID = %q, want %q", got.ID, rawID)
	}
}

func TestManagedBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore(t *testing.T) {
	cityPath, rigPath := setupManagedBdWaitTestCity(t)
	bdPath := waitTestRealBDPath(t)
	worktreeDir := filepath.Join(cityPath, ".gc", "worktrees", "frontend", "polecats", "polecat-1")
	if err := os.MkdirAll(filepath.Join(worktreeDir, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(worktree .beads): %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, ".beads", "redirect"), []byte(filepath.Join(rigPath, ".beads")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(redirect): %v", err)
	}

	providerStore, err := openStoreAtForCity(rigPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}

	rawID := parseCreatedBeadID(t, runRawBDFromDir(t, bdPath, worktreeDir, "create", "--json", "raw worktree bead", "-t", "task"))
	if got, err := providerStore.Get(rawID); err != nil {
		t.Fatalf("providerStore.Get(rawID): %v", err)
	} else if got.ID != rawID {
		t.Fatalf("providerStore.Get(rawID).ID = %q, want %q", got.ID, rawID)
	}

	setCwd(t, worktreeDir)
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_DOLT_PORT", "9999")
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"show", rawID}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc bd show rawID from worktree = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), rawID) {
		t.Fatalf("gc bd show output missing raw id %q from worktree:\n%s", rawID, stdout.String())
	}

	providerBead, err := providerStore.Create(beads.Bead{Title: "provider worktree bead", Type: "task"})
	if err != nil {
		t.Fatalf("providerStore.Create: %v", err)
	}
	if rawShow := runRawBDFromDir(t, bdPath, worktreeDir, "show", "--json", providerBead.ID); !strings.Contains(rawShow, providerBead.ID) {
		t.Fatalf("raw bd show missing provider-created bead %q from worktree:\n%s", providerBead.ID, rawShow)
	}
	stdout.Reset()
	stderr.Reset()
	if code := doBd([]string{"show", providerBead.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc bd show provider bead from worktree = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), providerBead.ID) {
		t.Fatalf("gc bd show output missing provider bead %q from worktree:\n%s", providerBead.ID, stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doBd([]string{"create", "--json", "gc worktree bead", "-t", "task"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc bd create from worktree = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	gcID := parseCreatedBeadID(t, stdout.String())
	if got, err := providerStore.Get(gcID); err != nil {
		t.Fatalf("providerStore.Get(gcID): %v", err)
	} else if got.ID != gcID {
		t.Fatalf("providerStore.Get(gcID).ID = %q, want %q", got.ID, gcID)
	}
	if rawShow := runRawBDFromDir(t, bdPath, worktreeDir, "show", "--json", gcID); !strings.Contains(rawShow, gcID) {
		t.Fatalf("raw bd show missing gc-created bead %q from worktree:\n%s", gcID, rawShow)
	}
}

func TestManagedBdCityStoreConsistentAcrossRawBdGcBdAndProviderStore(t *testing.T) {
	cityPath, _ := setupManagedBdWaitTestCity(t)
	bdPath := waitTestRealBDPath(t)
	rawDir := filepath.Join(cityPath, "nested")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rawDir): %v", err)
	}

	rawID := parseCreatedBeadID(t, runRawBDFromDir(t, bdPath, rawDir, "create", "--json", "raw city bead", "-t", "task"))
	if got := beadPrefix(nil, rawID); got != "gc" {
		t.Fatalf("raw city bead prefix = %q, want %q", got, "gc")
	}
	providerStore, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	if got, err := providerStore.Get(rawID); err != nil {
		t.Fatalf("providerStore.Get(rawID): %v", err)
	} else if got.ID != rawID {
		t.Fatalf("providerStore.Get(rawID).ID = %q, want %q", got.ID, rawID)
	}

	t.Setenv("GC_DOLT_PORT", "9999")
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"--city", cityPath, "show", rawID}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc bd show rawID = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), rawID) {
		t.Fatalf("gc bd show output missing raw id %q:\n%s", rawID, stdout.String())
	}

	providerBead, err := providerStore.Create(beads.Bead{Title: "provider city bead", Type: "task"})
	if err != nil {
		t.Fatalf("providerStore.Create: %v", err)
	}
	if got := beadPrefix(nil, providerBead.ID); got != "gc" {
		t.Fatalf("provider city bead prefix = %q, want %q", got, "gc")
	}
	rawShow := runRawBDFromDir(t, bdPath, rawDir, "show", "--json", providerBead.ID)
	if !strings.Contains(rawShow, providerBead.ID) {
		t.Fatalf("raw bd show missing provider-created bead %q:\n%s", providerBead.ID, rawShow)
	}
	stdout.Reset()
	stderr.Reset()
	if code := doBd([]string{"--city", cityPath, "show", providerBead.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc bd show provider bead = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), providerBead.ID) {
		t.Fatalf("gc bd show output missing provider bead %q:\n%s", providerBead.ID, stdout.String())
	}
}

func TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix(t *testing.T) {
	cityPath, _ := setupFreshManagedBdWaitTestCity(t)
	bdPath := waitTestRealBDPath(t)

	cmd := exec.Command("dolt", "sql", "-q", "show tables")
	cmd.Dir = filepath.Join(cityPath, ".beads", "dolt", "hq")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dolt sql show tables in hq: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "config") {
		t.Fatalf("hq database missing bead schema tables:\n%s", out)
	}

	rawDir := filepath.Join(cityPath, "fresh-nested")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rawDir): %v", err)
	}
	rawID := parseCreatedBeadID(t, runRawBDFromDir(t, bdPath, rawDir, "create", "--json", "fresh city bead", "-t", "task"))
	if got := beadPrefix(nil, rawID); got != "gc" {
		t.Fatalf("raw city bead prefix = %q, want %q", got, "gc")
	}
	providerStore, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	providerBead, err := providerStore.Create(beads.Bead{Title: "fresh provider city bead", Type: "task"})
	if err != nil {
		t.Fatalf("providerStore.Create: %v", err)
	}
	if got := beadPrefix(nil, providerBead.ID); got != "gc" {
		t.Fatalf("provider city bead prefix = %q, want %q", got, "gc")
	}
}

func TestInheritedExternalExecBdRigStoreConsistentAcrossRawBdAndProviderStore(t *testing.T) {
	cityPath, rigPath := setupManagedBdWaitTestCity(t)
	bdPath := waitTestRealBDPath(t)
	statePath := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile(dolt-state.json): %v", err)
	}
	var state struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("json.Unmarshal(dolt-state.json): %v", err)
	}
	port := strconv.Itoa(state.Port)
	cityCfg := strings.Join([]string{
		"issue_prefix: gc",
		"gc.endpoint_origin: city_canonical",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"dolt.host: 127.0.0.1",
		"dolt.port: " + port,
		"",
	}, "\n")
	rigCfg := strings.Join([]string{
		"issue_prefix: fe",
		"gc.endpoint_origin: inherited_city",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"dolt.host: 127.0.0.1",
		"dolt.port: " + port,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cityCfg), 0o644); err != nil {
		t.Fatalf("WriteFile(city config): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(rigCfg), 0o644); err != nil {
		t.Fatalf("WriteFile(rig config): %v", err)
	}
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(cityPath))
	t.Setenv("GC_DOLT_HOST", "bad.example.invalid")
	t.Setenv("GC_DOLT_PORT", "9999")
	rawDir := filepath.Join(rigPath, "nested-exec-external")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rawDir): %v", err)
	}

	providerStore, err := openStoreAtForCity(rigPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	providerBead, err := providerStore.Create(beads.Bead{Title: "provider exec external bead", Type: "task"})
	if err != nil {
		t.Fatalf("providerStore.Create: %v", err)
	}
	if rawShow := runRawBDFromDir(t, bdPath, rawDir, "show", "--json", providerBead.ID); !strings.Contains(rawShow, providerBead.ID) {
		t.Fatalf("raw bd show missing provider-created bead %q:\n%s", providerBead.ID, rawShow)
	}

	rawID := parseCreatedBeadID(t, runRawBDFromDir(t, bdPath, rawDir, "create", "--json", "raw exec external bead", "-t", "task"))
	if got, err := providerStore.Get(rawID); err != nil {
		t.Fatalf("providerStore.Get(rawID): %v", err)
	} else if got.ID != rawID {
		t.Fatalf("providerStore.Get(rawID).ID = %q, want %q", got.ID, rawID)
	}
}

func TestInheritedExternalBdRigStoreConsistentAcrossRawBdGcBdAndProviderStore(t *testing.T) {
	cityPath, rigPath := setupManagedBdWaitTestCity(t)
	bdPath := waitTestRealBDPath(t)
	statePath := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile(dolt-state.json): %v", err)
	}
	var state struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("json.Unmarshal(dolt-state.json): %v", err)
	}
	if state.Port == 0 {
		t.Fatalf("dolt runtime port = 0 in %s", statePath)
	}
	port := strconv.Itoa(state.Port)
	cityCfg := strings.Join([]string{
		"issue_prefix: gc",
		"gc.endpoint_origin: city_canonical",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"dolt.host: 127.0.0.1",
		"dolt.port: " + port,
		"",
	}, "\n")
	rigCfg := strings.Join([]string{
		"issue_prefix: fe",
		"gc.endpoint_origin: inherited_city",
		"gc.endpoint_status: verified",
		"dolt.auto-start: false",
		"dolt.host: 127.0.0.1",
		"dolt.port: " + port,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cityCfg), 0o644); err != nil {
		t.Fatalf("WriteFile(city config): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(rigCfg), 0o644); err != nil {
		t.Fatalf("WriteFile(rig config): %v", err)
	}
	rawDir := filepath.Join(rigPath, "nested")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rawDir): %v", err)
	}

	rawID := parseCreatedBeadID(t, runRawBDFromDir(t, bdPath, rawDir, "create", "--json", "raw inherited external bead", "-t", "task"))
	providerStore, err := openStoreAtForCity(rigPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	if got, err := providerStore.Get(rawID); err != nil {
		t.Fatalf("providerStore.Get(rawID): %v", err)
	} else if got.ID != rawID {
		t.Fatalf("providerStore.Get(rawID).ID = %q, want %q", got.ID, rawID)
	}

	t.Setenv("GC_DOLT_HOST", "bad.example.invalid")
	t.Setenv("GC_DOLT_PORT", "9999")
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"--city", cityPath, "--rig", "frontend", "show", rawID}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc bd show rawID = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), rawID) {
		t.Fatalf("gc bd show output missing raw id %q:\n%s", rawID, stdout.String())
	}

	providerBead, err := providerStore.Create(beads.Bead{Title: "provider inherited external bead", Type: "task"})
	if err != nil {
		t.Fatalf("providerStore.Create: %v", err)
	}
	rawShow := runRawBDFromDir(t, bdPath, rawDir, "show", "--json", providerBead.ID)
	if !strings.Contains(rawShow, providerBead.ID) {
		t.Fatalf("raw bd show missing provider-created bead %q:\n%s", providerBead.ID, rawShow)
	}
	stdout.Reset()
	stderr.Reset()
	if code := doBd([]string{"--city", cityPath, "--rig", "frontend", "show", providerBead.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc bd show provider bead = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), providerBead.ID) {
		t.Fatalf("gc bd show output missing provider bead %q:\n%s", providerBead.ID, stdout.String())
	}
}

func listToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

func TestResolveBdScopeTargetUsesEnclosingRig(t *testing.T) {
	origProbe := bdBeadExists
	defer func() { bdBeadExists = origProbe }()
	bdBeadExists = func(string, execStoreTarget, string) bool { return false }

	cityDir := filepath.Join(t.TempDir(), "city")
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs:      []config.Rig{{Name: "frontend", Path: "frontend", Prefix: "fr"}},
	}
	setCwd(t, filepath.Join(rigDir, "nested"))

	got, err := resolveBdScopeTarget(cfg, cityDir, "", []string{"context", "--json"})
	if err != nil {
		t.Fatalf("resolveBdScopeTarget() error = %v", err)
	}
	want := execStoreTarget{
		ScopeRoot: rigDir,
		ScopeKind: "rig",
		Prefix:    "fr",
		RigName:   "frontend",
	}
	if got != want {
		t.Fatalf("resolveBdScopeTarget() = %#v, want %#v", got, want)
	}
}

func TestResolveBdScopeTargetRoutesExistingCityBeadFromRigCwd(t *testing.T) {
	origProbe := bdBeadExists
	defer func() { bdBeadExists = origProbe }()
	bdBeadExists = func(_ string, target execStoreTarget, beadID string) bool {
		return target.ScopeKind == "city" && beadID == "mc-city1"
	}

	cityDir := filepath.Join(t.TempDir(), "city")
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "maintainer-city", Prefix: "mc"},
		Rigs:      []config.Rig{{Name: "frontend", Path: "frontend", Prefix: "fr"}},
	}
	setCwd(t, filepath.Join(rigDir, "nested"))

	got, err := resolveBdScopeTarget(cfg, cityDir, "", []string{"show", "mc-city1"})
	if err != nil {
		t.Fatalf("resolveBdScopeTarget() error = %v", err)
	}
	want := execStoreTarget{
		ScopeRoot: cityDir,
		ScopeKind: "city",
		Prefix:    "mc",
	}
	if got != want {
		t.Fatalf("resolveBdScopeTarget() = %#v, want %#v", got, want)
	}
}

func TestGcBdRespectsRawCityFlag(t *testing.T) {
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	origProbe := bdBeadExists
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
		bdBeadExists = origProbe
	}()
	bdBeadExists = func(string, execStoreTarget, string) bool { return false }
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	setCwd(t, t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "gc-bd-city.txt")
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
{
  printf 'pwd=%s\n' "$PWD"
  printf 'args=%s\n' "$*"
  printf 'GC_STORE_ROOT=%s\n' "${GC_STORE_ROOT:-}"
  printf 'GC_STORE_SCOPE=%s\n' "${GC_STORE_SCOPE:-}"
} > "${CAPTURE_PATH}"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+origPath)
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("GC_CITY_PATH", "")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"--city", cityDir, "context", "--json"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd() = %d, want 0; stderr=%q", got, stderr.String())
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
	if !samePath(got["pwd"], cityDir) {
		t.Fatalf("pwd = %q, want %q", got["pwd"], cityDir)
	}
	if got["args"] != "context --json" {
		t.Fatalf("args = %q, want %q", got["args"], "context --json")
	}
	if !samePath(got["GC_STORE_ROOT"], cityDir) {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got["GC_STORE_ROOT"], cityDir)
	}
	if got["GC_STORE_SCOPE"] != "city" {
		t.Fatalf("GC_STORE_SCOPE = %q, want %q", got["GC_STORE_SCOPE"], "city")
	}
}

func TestGcBdUsesEnclosingRigWhenNoFlag(t *testing.T) {
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	origProbe := bdBeadExists
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
		bdBeadExists = origProbe
	}()
	bdBeadExists = func(string, execStoreTarget, string) bool { return false }
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fr"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	setCwd(t, rigDir)

	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "gc-bd-rig.txt")
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
{
  printf 'pwd=%s\n' "$PWD"
  printf 'args=%s\n' "$*"
  printf 'GC_STORE_ROOT=%s\n' "${GC_STORE_ROOT:-}"
  printf 'GC_STORE_SCOPE=%s\n' "${GC_STORE_SCOPE:-}"
  printf 'GC_RIG=%s\n' "${GC_RIG:-}"
} > "${CAPTURE_PATH}"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+origPath)
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("GC_CITY_PATH", "")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"context", "--json"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd() = %d, want 0; stderr=%q", got, stderr.String())
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
	if !samePath(got["pwd"], rigDir) {
		t.Fatalf("pwd = %q, want %q", got["pwd"], rigDir)
	}
	if got["args"] != "context --json" {
		t.Fatalf("args = %q, want %q", got["args"], "context --json")
	}
	if !samePath(got["GC_STORE_ROOT"], rigDir) {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got["GC_STORE_ROOT"], rigDir)
	}
	if got["GC_STORE_SCOPE"] != "rig" {
		t.Fatalf("GC_STORE_SCOPE = %q, want %q", got["GC_STORE_SCOPE"], "rig")
	}
	if got["GC_RIG"] != "frontend" {
		t.Fatalf("GC_RIG = %q, want %q", got["GC_RIG"], "frontend")
	}
}

func TestGcBdWarnsOnExternalOverrideDrift(t *testing.T) {
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	}()
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[[rigs]]
name = "repo"
path = "repo"
prefix = "repo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: explicit
gc.endpoint_status: unverified
dolt.auto-start: false
dolt.host: 127.0.0.1
dolt.port: 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	capture := filepath.Join(t.TempDir(), "gc-bd-external-env.txt")
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
{
  printf 'GC_DOLT_HOST=%s\n' "${GC_DOLT_HOST:-}"
  printf 'GC_DOLT_PORT=%s\n' "${GC_DOLT_PORT:-}"
} > "${CAPTURE_PATH}"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+origPath)
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_DOLT_PORT", "9999")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"--city", cityDir, "--rig", "repo", "show", "repo-abc"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd() = %d, want 0; stderr=%q", got, stderr.String())
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	got := listToMap(strings.Split(strings.TrimSpace(string(data)), "\n"))
	if got["GC_DOLT_PORT"] != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want canonical 3307", got["GC_DOLT_PORT"])
	}
	if !strings.Contains(stderr.String(), "warning: ignoring ambient Dolt host/port override for external target") {
		t.Fatalf("stderr = %q, want ignored-override warning", stderr.String())
	}
	if !strings.Contains(stderr.String(), "GC_DOLT_PORT=9999 (canonical 3307)") {
		t.Fatalf("stderr = %q, want canonical drift detail", stderr.String())
	}
}
