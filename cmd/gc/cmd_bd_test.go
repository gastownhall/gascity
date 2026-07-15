package main

import (
	"bytes"
	"encoding/json"
	"errors"
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
	"github.com/gastownhall/gascity/internal/pgauth"
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

func TestExtractBdDirectoryFlag(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"short flag", []string{"create", "-C", "/tmp/packs", "--json"}, "/tmp/packs"},
		{"long flag space", []string{"create", "--directory", "/tmp/packs"}, "/tmp/packs"},
		{"long flag equals", []string{"create", "--directory=/tmp/packs"}, "/tmp/packs"},
		{"absent", []string{"create", "--json"}, ""},
		{"short flag at end no value", []string{"create", "-C"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractBdDirectoryFlag(tt.args); got != tt.want {
				t.Fatalf("extractBdDirectoryFlag(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestResolveBdScopeTarget(t *testing.T) {
	// Isolate cwd from any ambient `.beads/redirect` in the working tree
	// (e.g. when `make test` runs from a polecat/crew worktree, the worktree's
	// redirect resolves to a path outside the synthetic rig config below and
	// `rigFromRedirectedBeadsDir` rejects it). t.TempDir's ancestry is /tmp,
	// which is guaranteed to be free of city redirects.
	setCwd(t, t.TempDir())

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
		name         string
		rigName      string
		args         []string
		cityExplicit bool
		want         execStoreTarget
		wantError    string
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
		{
			// gastownhall/gascity#3410: an explicit --city must pin the city
			// store and not be silently downgraded to a rig store, even when a
			// rig-prefixed bead id is present in the args.
			name:         "explicit city pins city over bead-prefix",
			rigName:      "",
			args:         []string{"show", "projectwrenunity-0xk"},
			cityExplicit: true,
			want: execStoreTarget{
				ScopeRoot: cityDir,
				ScopeKind: "city",
				Prefix:    "ga",
			},
		},
		{
			name:         "explicit city pins city for list",
			rigName:      "",
			args:         []string{"list", "--status", "open"},
			cityExplicit: true,
			want: execStoreTarget{
				ScopeRoot: cityDir,
				ScopeKind: "city",
				Prefix:    "ga",
			},
		},
		{
			// An explicit --rig still wins over an explicit --city.
			name:         "explicit rig wins over explicit city",
			rigName:      "wren",
			args:         []string{"list"},
			cityExplicit: true,
			want: execStoreTarget{
				ScopeRoot: filepath.Join(cityDir, "rigs", "wren"),
				ScopeKind: "rig",
				Prefix:    "projectwrenunity",
				RigName:   "wren",
			},
		},
		{
			name:    "-C routes to matching rig",
			rigName: "",
			args:    []string{"create", "-C", filepath.Join(cityDir, "rigs", "wren"), "--json"},
			want: execStoreTarget{
				ScopeRoot: filepath.Join(cityDir, "rigs", "wren"),
				ScopeKind: "rig",
				Prefix:    "projectwrenunity",
				RigName:   "wren",
			},
		},
		{
			name:    "--directory routes to matching rig",
			rigName: "",
			args:    []string{"create", "--directory", filepath.Join(cityDir, "rigs", "wren"), "--json"},
			want: execStoreTarget{
				ScopeRoot: filepath.Join(cityDir, "rigs", "wren"),
				ScopeKind: "rig",
				Prefix:    "projectwrenunity",
				RigName:   "wren",
			},
		},
		{
			name:    "-C outside known rigs falls back to city",
			rigName: "",
			args:    []string{"create", "-C", "/tmp/unknown-dir", "--json"},
			want: execStoreTarget{
				ScopeRoot: cityDir,
				ScopeKind: "city",
				Prefix:    "ga",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveBdScopeTarget(cfgForTest(), cityDir, tt.rigName, tt.args, tt.cityExplicit)
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
	got, err := resolveBdScopeTarget(cfg, cityDir, "", []string{"list"}, false)
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

// TestResolveBdScopeTargetUsesGCRIGEnv covers the bug where GC_RIG env (set by
// the controller on every rig agent) was silently ignored, causing gc bd list to
// hit the city HQ database and return empty results instead of rig-scoped results.
// See gastownhall/gascity#gcy-6ul.
func TestResolveBdScopeTargetUsesGCRIGEnv(t *testing.T) {
	setCwd(t, t.TempDir())
	origProbe := bdBeadExists
	defer func() { bdBeadExists = origProbe }()
	bdBeadExists = func(_ string, _ execStoreTarget, _ string) bool { return false }

	cityDir := filepath.Join(t.TempDir(), "city")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gascity"},
		Rigs: []config.Rig{
			{Name: "chatehr", Path: filepath.Join("rigs", "chatehr"), Prefix: "ch"},
			{Name: "wren", Path: filepath.Join("rigs", "wren"), Prefix: "projectwrenunity"},
		},
	}

	t.Run("GC_RIG env routes to rig when no flag and no bead-id args", func(t *testing.T) {
		t.Setenv("GC_RIG", "chatehr")
		got, err := resolveBdScopeTarget(cfg, cityDir, "", []string{"list", "--assignee=chatehr/gastown.refinery", "--status=open"}, false)
		if err != nil {
			t.Fatalf("resolveBdScopeTarget() error = %v", err)
		}
		want := execStoreTarget{
			ScopeRoot: filepath.Join(cityDir, "rigs", "chatehr"),
			ScopeKind: "rig",
			Prefix:    "ch",
			RigName:   "chatehr",
		}
		if got != want {
			t.Fatalf("resolveBdScopeTarget() = %#v, want %#v", got, want)
		}
	})

	t.Run("explicit --rig flag overrides GC_RIG env", func(t *testing.T) {
		t.Setenv("GC_RIG", "chatehr")
		got, err := resolveBdScopeTarget(cfg, cityDir, "wren", []string{"list"}, false)
		if err != nil {
			t.Fatalf("resolveBdScopeTarget() error = %v", err)
		}
		want := execStoreTarget{
			ScopeRoot: filepath.Join(cityDir, "rigs", "wren"),
			ScopeKind: "rig",
			Prefix:    "projectwrenunity",
			RigName:   "wren",
		}
		if got != want {
			t.Fatalf("resolveBdScopeTarget() = %#v, want %#v", got, want)
		}
	})

	t.Run("bead-id prefix detection wins over GC_RIG env", func(t *testing.T) {
		t.Setenv("GC_RIG", "chatehr")
		// Restore bdBeadExists to return true for a wren bead
		origProbe2 := bdBeadExists
		defer func() { bdBeadExists = origProbe2 }()
		bdBeadExists = func(_ string, target execStoreTarget, beadID string) bool {
			return beadID == "projectwrenunity-0xk" && target.RigName == "wren"
		}
		got, err := resolveBdScopeTarget(cfg, cityDir, "", []string{"show", "projectwrenunity-0xk"}, false)
		if err != nil {
			t.Fatalf("resolveBdScopeTarget() error = %v", err)
		}
		want := execStoreTarget{
			ScopeRoot: filepath.Join(cityDir, "rigs", "wren"),
			ScopeKind: "rig",
			Prefix:    "projectwrenunity",
			RigName:   "wren",
		}
		if got != want {
			t.Fatalf("resolveBdScopeTarget() = %#v, want %#v", got, want)
		}
	})

	t.Run("unknown GC_RIG env falls through to city root", func(t *testing.T) {
		t.Setenv("GC_RIG", "nonexistent-rig")
		got, err := resolveBdScopeTarget(cfg, cityDir, "", []string{"list"}, false)
		if err != nil {
			t.Fatalf("resolveBdScopeTarget() error = %v", err)
		}
		// Must land on city root, not the (unknown) GC_RIG rig.
		if got.ScopeKind != "city" {
			t.Fatalf("resolveBdScopeTarget() ScopeKind = %q, want %q", got.ScopeKind, "city")
		}
		if got.ScopeRoot != cityDir {
			t.Fatalf("resolveBdScopeTarget() ScopeRoot = %q, want %q", got.ScopeRoot, cityDir)
		}
	})
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
	_, err := resolveBdScopeTarget(cfg, cityDir, "", []string{"list"}, false)
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
	envList, err := bdCommandEnv(cityDir, cfg, execStoreTarget{
		ScopeRoot: rigDir,
		ScopeKind: "rig",
		Prefix:    "repo",
		RigName:   "repo",
	})
	if err != nil {
		t.Fatalf("bdCommandEnv: %v", err)
	}
	env := listToMap(envList)
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

func TestBdCommandEnvSurfacesPostgresProjectionError(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
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
	rigDir := filepath.Join(cityDir, "rigs", "pg")
	writePGScopeFixture(t, rigDir, "")
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: pg
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "pg", Path: "rigs/pg", Prefix: "pg"}}}

	_, err := bdCommandEnv(cityDir, cfg, execStoreTarget{
		ScopeRoot: rigDir,
		ScopeKind: "rig",
		Prefix:    "pg",
		RigName:   "pg",
	})
	if err == nil {
		t.Fatal("bdCommandEnv err = nil, want postgres projection error")
	}
	if !errors.Is(err, pgauth.ErrNoPasswordResolvable) {
		t.Errorf("errors.Is(err, ErrNoPasswordResolvable) = false, want true; err=%v", err)
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
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
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
	disableManagedDoltRecoveryForTest(t)

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
	// See TestResolveBdScopeTarget for rationale: isolate cwd so any
	// `.beads/redirect` in the ambient working tree doesn't surface here.
	setCwd(t, cityDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeBuiltinImportsFixture(t, cityDir, "core", "bd")

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

func TestGcBdReapsStaleBdExportJSONLBeforeDirectCommand(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)

	origCityFlag := cityFlag
	origRigFlag := rigFlag
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	}()
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	beadsDir := filepath.Join(cityDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue_prefix: gc\ngc.endpoint_origin: managed_city\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(beadsDir, "issues.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(`{"_type":"issue","id":"gc-1"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
printf '[{"id":"gc-1","title":"ok"}]\n'
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY_PATH", cityDir)

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"show", "gc-1", "--json"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd() = %d, want 0; stdout=%q stderr=%q", got, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(jsonlPath); !os.IsNotExist(err) {
		t.Fatalf("issues.jsonl present after direct gc bd command; stat err = %v, want IsNotExist", err)
	}
}

func TestGcBdDoesNotAutoRouteHyphenatedFlagValue(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)

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
	// See TestResolveBdScopeTarget for rationale: isolate cwd so any
	// `.beads/redirect` in the ambient working tree doesn't surface here.
	setCwd(t, cityDir)

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

	// Scrub inherited beads env (notably GC_BEADS_SCOPE_ROOT from a
	// gc agent's outer city) so the explicit GC_BEADS override below
	// is honored by configuredBeadsProviderValue. Without this, a leaked
	// GC_BEADS_SCOPE_ROOT disqualifies the override and the provider
	// resolution falls back to city.toml peek (which has no [beads]
	// section here) → defaults to "bd" → rejection never fires.
	clearInheritedBeadsEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// See TestResolveBdScopeTarget for rationale: isolate cwd so any
	// `.beads/redirect` in the ambient working tree doesn't surface here.
	setCwd(t, cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_BEADS", "file")
	// Clear any inherited scope pin so the GC_BEADS override applies to
	// this test's city. When run from a polecat session, the ambient
	// GC_BEADS_SCOPE_ROOT points at the rig repo and would suppress the
	// override before the provider check could fire.
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

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
	// See TestResolveBdScopeTarget for rationale: isolate cwd so any
	// `.beads/redirect` in the ambient working tree doesn't surface here.
	setCwd(t, cityDir)
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

	got, err := resolveBdScopeTarget(cfg, cityDir, "", []string{"context", "--json"}, false)
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

	got, err := resolveBdScopeTarget(cfg, cityDir, "", []string{"show", "mc-city1"}, false)
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
	disableManagedDoltRecoveryForTest(t)

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
	disableManagedDoltRecoveryForTest(t)

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
	disableManagedDoltRecoveryForTest(t)

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

// silentFallbackFakeBdScript builds a fake `bd` shell script that emits the
// silent-fallback marker pair on stderr ("auto-importing ... into empty
// database") and exits 0 — the exact shape bd produces when it loses the
// managed Dolt server and falls back to opening the on-disk store. doBd
// should treat this as a hard failure regardless of bd's exit code.
const silentFallbackFakeBdScript = `#!/bin/sh
echo "auto-importing 220929 bytes from .beads/issues.jsonl into empty database... auto-imported 123 issues" >&2
echo "$@"
exit 0
`

// silentFallbackTestSetup writes a fake bd binary that emits the silent-
// fallback marker, prepends it to PATH, and configures a minimal city as a
// bd-backed scope (via GC_CITY_PATH) so doBd will dispatch through it.
func silentFallbackTestSetup(t *testing.T, fakeBdScript string) {
	t.Helper()

	origCityFlag := cityFlag
	origRigFlag := rigFlag
	t.Cleanup(func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	})
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	port := strconv.Itoa(writeReachableManagedDoltState(t, cityDir))

	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := "issue_prefix: demo\n" +
		"gc.endpoint_origin: city_canonical\n" +
		"gc.endpoint_status: verified\n" +
		"dolt.auto-start: false\n" +
		"dolt.host: 127.0.0.1\n" +
		"dolt.port: " + port + "\n"
	// writeReachableManagedDoltState already creates .beads, but don't rely
	// on that side-effect — make the directory explicit before writing.
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(fakeBdScript), 0o755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+origPath)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_DOLT_PORT", port)
}

// TestGcBdSurfacesSilentFallbackAsLoudError_UpdatePath pins the #2080 fix:
// when bd's update path silently falls back to the on-disk store, gc bd must
// convert that into a non-zero exit with an operator-facing message instead
// of letting the silent write loss reach the operator as success.
func TestGcBdSurfacesSilentFallbackAsLoudError_UpdatePath(t *testing.T) {
	silentFallbackTestSetup(t, silentFallbackFakeBdScript)

	var stdout, stderr bytes.Buffer
	got := doBd([]string{"update", "demo-abc", "--set-metadata", "k=v"}, &stdout, &stderr)
	if got != bdSilentFallbackExitCode {
		t.Fatalf("doBd(update) = %d, want %d (silent-fallback exit code); stderr=%q",
			got, bdSilentFallbackExitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "managed Dolt unreachable") {
		t.Fatalf("stderr missing loud-fail message; stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "auto-importing") {
		t.Fatalf("original bd stderr not passed through; stderr=%q", stderr.String())
	}
}

// TestGcBdSurfacesSilentFallbackAsLoudError_ClosePath pins the #2079 half of
// the bd-write-persistence quad: bd close goes through the same doBd
// handoff, so the silent-fallback detection must fire identically. Pre-fix,
// gc bd close would have exited 0 even when the close never persisted to
// JSONL (the behavior #2079 documents).
func TestGcBdSurfacesSilentFallbackAsLoudError_ClosePath(t *testing.T) {
	silentFallbackTestSetup(t, silentFallbackFakeBdScript)

	var stdout, stderr bytes.Buffer
	got := doBd([]string{"close", "demo-abc", "-r", "duplicate"}, &stdout, &stderr)
	if got != bdSilentFallbackExitCode {
		t.Fatalf("doBd(close) = %d, want %d (silent-fallback exit code); stderr=%q",
			got, bdSilentFallbackExitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "managed Dolt unreachable") {
		t.Fatalf("stderr missing loud-fail message; stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "auto-importing") {
		t.Fatalf("original bd stderr not passed through; stderr=%q", stderr.String())
	}
}

func TestGcBdSurfacesSilentFallbackAsLoudError_ReleaseIfCurrentPath(t *testing.T) {
	silentFallbackTestSetup(t, silentFallbackFakeBdScript)

	var stdout, stderr bytes.Buffer
	got := doBd([]string{"release-if-current", "demo-abc", "worker-1"}, &stdout, &stderr)
	if got != bdSilentFallbackExitCode {
		t.Fatalf("doBd(release-if-current) = %d, want %d (silent-fallback exit code); stderr=%q stdout=%q",
			got, bdSilentFallbackExitCode, stderr.String(), stdout.String())
	}
	if !strings.Contains(stderr.String(), "managed Dolt unreachable") {
		t.Fatalf("stderr missing loud-fail message; stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "auto-importing") {
		t.Fatalf("original bd stderr not passed through; stderr=%q", stderr.String())
	}
	if strings.Contains(stdout.String(), "released") {
		t.Fatalf("release-if-current reported success despite silent fallback; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// TestGcBdHappyPathExitsZeroWithoutFallbackMarker is the inverse: a clean
// bd run that produces no auto-import marker must NOT be converted into the
// loud-fail. This guards against false positives where bd's stderr happens
// to contain unrelated content.
func TestGcBdHappyPathExitsZeroWithoutFallbackMarker(t *testing.T) {
	// Fake bd that exits 0 with normal output and an unrelated stderr line.
	const happyPathFakeBdScript = `#!/bin/sh
echo "some normal bd output"
echo "some unrelated stderr line" >&2
exit 0
`
	silentFallbackTestSetup(t, happyPathFakeBdScript)

	var stdout, stderr bytes.Buffer
	got := doBd([]string{"list"}, &stdout, &stderr)
	if got != 0 {
		t.Fatalf("doBd(list) = %d, want 0; stderr=%q", got, stderr.String())
	}
	if strings.Contains(stderr.String(), "managed Dolt unreachable") {
		t.Fatalf("loud-fail message fired on a happy-path run; stderr=%q", stderr.String())
	}
}

// TestGcBdProcessExitCodeMatchesSilentFallbackContract pins the process-
// level exit code contract that the bdSilentFallbackExitCode = 4 doc
// comment promises operators and CI. PR #2327 review found the previous
// RunE used `return errExit` on any non-zero, which collapsed every code
// to 1 in commandExitCode — defeating the operator/CI signal the loud-
// fail was meant to provide. Plumbing doBd's numeric code through
// exitForCode ensures the process exit code matches what doBd computed.
func TestGcBdProcessExitCodeMatchesSilentFallbackContract(t *testing.T) {
	silentFallbackTestSetup(t, silentFallbackFakeBdScript)

	var stdout, stderr bytes.Buffer
	got := run([]string{"bd", "update", "demo-abc", "--set-metadata", "k=v"}, &stdout, &stderr)
	if got != bdSilentFallbackExitCode {
		t.Fatalf("run(bd update) = %d, want %d (silent-fallback exit code); stderr=%q",
			got, bdSilentFallbackExitCode, stderr.String())
	}
}

// TestGcBdProcessExitCodePreservesBdNonZero is the inverse case: when bd
// returns a non-zero code that isn't the silent-fallback case (e.g., bd
// itself rejected the command), gc bd must preserve bd's exit code rather
// than collapsing it to 1. exitForCode encodes ≥2 codes via
// commandExitError so commandExitCode reads them back faithfully.
func TestGcBdProcessExitCodePreservesBdNonZero(t *testing.T) {
	const bdRejectsScript = `#!/bin/sh
echo "bd: simulated usage error" >&2
exit 3
`
	silentFallbackTestSetup(t, bdRejectsScript)

	var stdout, stderr bytes.Buffer
	got := run([]string{"bd", "list"}, &stdout, &stderr)
	if got != 3 {
		t.Fatalf("run(bd list) = %d, want 3 (preserved bd exit code); stderr=%q",
			got, stderr.String())
	}
}

// TestBdOutputIndicatesSilentFallback covers the marker-detection helper
// directly with table-driven cases so the source-of-truth for what counts
// as "silent fallback" is unit-pinned.
func TestBdOutputIndicatesSilentFallback(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"empty", "", false},
		{"single marker only auto-importing", "auto-importing 100 bytes from foo", false},
		{"single marker only into-empty-database", "into empty database", false},
		{"both markers same line", "auto-importing 100 bytes into empty database", true},
		{"both markers reversed order", "into empty database <- auto-importing 100 bytes", true},
		{"both markers across newlines", "auto-importing 100 bytes\n  into empty database\n  done", true},
		{"case insensitive uppercase", "AUTO-IMPORTING INTO EMPTY DATABASE", true},
		{"case insensitive mixed", "Auto-Importing 200 bytes Into Empty Database", true},
		{"unrelated transport error", "dial tcp 127.0.0.1:3306: connect: connection refused", false},
		{"unrelated server-unreachable error", "server unreachable", false},
		{"both markers buried in long output", "starting bd\n... \nauto-importing 220929 bytes from .beads/issues.jsonl into empty database... \n... \nauto-imported 123 issues\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bdOutputIndicatesSilentFallback(tt.input); got != tt.want {
				t.Errorf("bdOutputIndicatesSilentFallback(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestHeadLimitedWriter pins the bounded-prefix behavior used to scan bd's
// stderr: writes past the limit are reported as fully consumed (so it is
// safe behind io.MultiWriter) but only the first limit bytes are retained.
func TestHeadLimitedWriter(t *testing.T) {
	t.Run("retains only the first limit bytes of an oversized write", func(t *testing.T) {
		w := &headLimitedWriter{limit: 5}
		n, err := w.Write([]byte("abcdefgh"))
		if err != nil || n != 8 {
			t.Fatalf("Write = (%d, %v), want (8, nil)", n, err)
		}
		if got := w.String(); got != "abcde" {
			t.Fatalf("String() = %q, want %q", got, "abcde")
		}
	})
	t.Run("accumulates across writes and stops at the limit", func(t *testing.T) {
		w := &headLimitedWriter{limit: 5}
		for _, chunk := range []string{"ab", "cdef", "ghi"} {
			if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
				t.Fatalf("Write(%q) = (%d, %v), want (%d, nil)", chunk, n, err, len(chunk))
			}
		}
		if got := w.String(); got != "abcde" {
			t.Fatalf("String() = %q, want %q", got, "abcde")
		}
	})
	t.Run("zero limit retains nothing but still consumes the write", func(t *testing.T) {
		w := &headLimitedWriter{limit: 0}
		n, err := w.Write([]byte("xyz"))
		if err != nil || n != 3 {
			t.Fatalf("Write = (%d, %v), want (3, nil)", n, err)
		}
		if got := w.String(); got != "" {
			t.Fatalf("String() = %q, want empty", got)
		}
	})
}

// TestGcBdHeartbeatRewritesToMetadataUpdate pins the gastownhall/gascity#1855
// worker-heartbeat write half: `gc bd heartbeat <id>` must forward to bd as
// `update <id> --set-metadata gc.last_heartbeat_at=<RFC3339 UTC>`. The exact
// key (with the _at suffix) is what the gas-city-dashboard will read
// (dashboard #324) to tell a live worker from a dead one, and the stamp must
// be valid RFC3339 in UTC even when the local clock is in another zone.
func TestGcBdHeartbeatRewritesToMetadataUpdate(t *testing.T) {
	origNow := bdHeartbeatNow
	t.Cleanup(func() { bdHeartbeatNow = origNow })
	// Pin the clock to a non-UTC zone to prove the rewrite normalizes to UTC.
	fixed := time.Date(2026, 5, 31, 12, 0, 0, 0, time.FixedZone("PST", -8*3600))
	bdHeartbeatNow = func() time.Time { return fixed }

	// The fake bd captures its forwarded args so the assertion can inspect them.
	capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
	silentFallbackTestSetup(t, "#!/bin/sh\nprintf '%s' \"$*\" > \"${CAPTURE_PATH}\"\n")
	t.Setenv("CAPTURE_PATH", capture)

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"heartbeat", "demo-abc"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd(heartbeat) = %d, want 0; stderr=%q", got, stderr.String())
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "update demo-abc --set-metadata " + heartbeatMetadataKey + "="
	gotArgs := string(data)
	stamp, ok := strings.CutPrefix(gotArgs, prefix)
	if !ok {
		t.Fatalf("forwarded args = %q, want prefix %q", gotArgs, prefix)
	}
	parsed, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("heartbeat stamp %q is not valid RFC3339: %v", stamp, err)
	}
	if _, offset := parsed.Zone(); offset != 0 {
		t.Fatalf("heartbeat stamp %q is not UTC (zone offset %d)", stamp, offset)
	}
	if want := fixed.UTC().Format(time.RFC3339); stamp != want {
		t.Fatalf("heartbeat stamp = %q, want %q", stamp, want)
	}
}

// TestRewriteBdHeartbeatArgs covers the arg-rewrite edge cases without the
// full city/bd harness: exactly one issue id is required, and non-heartbeat
// commands pass through untouched so the generic bd passthrough is intact.
func TestRewriteBdHeartbeatArgs(t *testing.T) {
	t.Run("rejects wrong arity, flag-as-id, or whitespace id", func(t *testing.T) {
		for _, args := range [][]string{
			{"heartbeat"},
			{"heartbeat", "demo-abc", "extra"},
			{"heartbeat", "--flag"},
			{"heartbeat", ""},
			{"heartbeat", "  "},        // all-whitespace
			{"heartbeat", " demo-abc"}, // leading space
			{"heartbeat", "demo-abc "}, // trailing space
			{"heartbeat", "demo abc"},  // internal space
		} {
			got, err := rewriteBdHeartbeatArgs(args)
			if err == nil {
				t.Fatalf("rewriteBdHeartbeatArgs(%q) = (%q, nil), want usage error", args, got)
			}
		}
	})
	t.Run("rewrites a clean id to a set-metadata update", func(t *testing.T) {
		out, err := rewriteBdHeartbeatArgs([]string{"heartbeat", "demo-abc"})
		if err != nil {
			t.Fatalf("rewriteBdHeartbeatArgs unexpected error: %v", err)
		}
		if len(out) != 4 || out[0] != "update" || out[1] != "demo-abc" || out[2] != "--set-metadata" {
			t.Fatalf("rewriteBdHeartbeatArgs = %q, want [update demo-abc --set-metadata ...]", out)
		}
	})
	t.Run("passes non-heartbeat args through unchanged", func(t *testing.T) {
		in := []string{"list", "-s", "open"}
		out, err := rewriteBdHeartbeatArgs(in)
		if err != nil {
			t.Fatalf("rewriteBdHeartbeatArgs(%q) unexpected error: %v", in, err)
		}
		if len(out) != len(in) || out[0] != "list" || out[2] != "open" {
			t.Fatalf("rewriteBdHeartbeatArgs(%q) = %q, want passthrough", in, out)
		}
	})
}

// TestBdMutationWriteID covers the compatibility shim (first-ID extraction).
func TestBdMutationWriteID(t *testing.T) {
	t.Run("extracts id from write subcommands", func(t *testing.T) {
		cases := []struct {
			args []string
			want string
		}{
			{[]string{"update", "gcy-dv7", "--title", "x"}, "gcy-dv7"},
			{[]string{"update", "--title", "x", "gcy-dv7"}, "gcy-dv7"},
			{[]string{"close", "gcy-dv7"}, "gcy-dv7"},
			{[]string{"close", "--reason", "done", "gcy-dv7"}, "gcy-dv7"},
			{[]string{"close", "--force", "--json", "gcy-dv7"}, "gcy-dv7"},
			{[]string{"reopen", "gcy-dv7"}, "gcy-dv7"},
			{[]string{"delete", "--force", "gcy-dv7"}, "gcy-dv7"},
			{[]string{"delete", "--force", "--json", "gcy-dv7"}, "gcy-dv7"},
			// double-dash separator
			{[]string{"update", "--", "gcy-dv7"}, "gcy-dv7"},
		}
		for _, tc := range cases {
			got, ok := bdMutationWriteID(tc.args)
			if !ok || got != tc.want {
				t.Errorf("bdMutationWriteID(%q) = (%q, %v), want (%q, true)", tc.args, got, ok, tc.want)
			}
		}
	})
	t.Run("returns false for read or unrecognized subcommands", func(t *testing.T) {
		for _, args := range [][]string{
			{"show", "gcy-dv7"},
			{"list", "-s", "open"},
			{"query", "gcy-dv7"},
			{},
			{"create", "new task"},
		} {
			if _, ok := bdMutationWriteID(args); ok {
				t.Errorf("bdMutationWriteID(%q) returned ok=true, want false", args)
			}
		}
	})
	// Regression: short ID "gcy-dv7" must NOT be confused for "gcy-wisp-dv78"
	// by the caller — the returned token is the exact string in args.
	t.Run("returns the exact supplied token (gcy-g4o regression)", func(t *testing.T) {
		got, ok := bdMutationWriteID([]string{"update", "gcy-dv7", "--status", "open"})
		if !ok || got != "gcy-dv7" {
			t.Errorf("bdMutationWriteID: got (%q, %v), want (\"gcy-dv7\", true)", got, ok)
		}
	})
}

// TestBdMutationWriteIDs covers the full scanner used by the pre-flight guard.
func TestBdMutationWriteIDs(t *testing.T) {
	type result struct {
		ids       []string
		ok        bool
		ambiguous bool
	}
	cases := []struct {
		name string
		args []string
		want result
	}{
		// --- Basic extraction ---
		{
			name: "single id, no flags",
			args: []string{"close", "gcy-dv7"},
			want: result{ids: []string{"gcy-dv7"}, ok: true},
		},
		{
			name: "id before flags",
			args: []string{"update", "gcy-dv7", "--title", "x"},
			want: result{ids: []string{"gcy-dv7"}, ok: true},
		},
		{
			name: "id after long value flag",
			args: []string{"update", "--title", "new title", "gcy-dv7"},
			want: result{ids: []string{"gcy-dv7"}, ok: true},
		},

		// --- Short flags (previously broken) ---
		{
			name: "short -s value flag before id",
			args: []string{"update", "-s", "closed", "gcy-dv7"},
			want: result{ids: []string{"gcy-dv7"}, ok: true},
		},
		{
			name: "short -a value flag before id",
			args: []string{"update", "-a", "alice", "gcy-dv7"},
			want: result{ids: []string{"gcy-dv7"}, ok: true},
		},
		{
			name: "short -t value flag before id",
			args: []string{"update", "-t", "task", "gcy-dv7"},
			want: result{ids: []string{"gcy-dv7"}, ok: true},
		},
		{
			name: "short -p value flag before id",
			args: []string{"update", "-p", "P2", "gcy-dv7"},
			want: result{ids: []string{"gcy-dv7"}, ok: true},
		},
		{
			name: "short -d value flag before id",
			args: []string{"update", "-d", "description text", "gcy-dv7"},
			want: result{ids: []string{"gcy-dv7"}, ok: true},
		},
		{
			name: "short -e value flag before id",
			args: []string{"update", "-e", "60", "gcy-dv7"},
			want: result{ids: []string{"gcy-dv7"}, ok: true},
		},
		{
			name: "short -r reason flag for close before id",
			args: []string{"close", "-r", "done", "gcy-dv7"},
			want: result{ids: []string{"gcy-dv7"}, ok: true},
		},
		{
			name: "short -C directory flag before id",
			args: []string{"close", "-C", "/some/path", "gcy-dv7"},
			want: result{ids: []string{"gcy-dv7"}, ok: true},
		},

		// --- --flag=value form ---
		{
			name: "--flag=value does not consume next token",
			args: []string{"update", "--status=closed", "gcy-dv7"},
			want: result{ids: []string{"gcy-dv7"}, ok: true},
		},
		{
			name: "--title=value with id after",
			args: []string{"update", "--title=new title", "gcy-dv7"},
			want: result{ids: []string{"gcy-dv7"}, ok: true},
		},

		// --- Message / notes flags whose values may contain bead tokens ---
		{
			name: "notes value contains bead token",
			args: []string{"update", "--notes", "see gcy-dv7 for context", "gcy-real"},
			want: result{ids: []string{"gcy-real"}, ok: true},
		},
		{
			name: "append-notes value contains bead token",
			args: []string{"update", "--append-notes", "related: gcy-dv7", "gcy-real"},
			want: result{ids: []string{"gcy-real"}, ok: true},
		},

		// --- Batch IDs ---
		{
			name: "batch close multiple ids",
			args: []string{"close", "id1", "id2", "id3"},
			want: result{ids: []string{"id1", "id2", "id3"}, ok: true},
		},
		{
			name: "batch delete multiple ids with --force",
			args: []string{"delete", "--force", "id1", "id2", "id3"},
			want: result{ids: []string{"id1", "id2", "id3"}, ok: true},
		},
		{
			name: "batch update with flags interspersed",
			args: []string{"update", "--status", "closed", "id1", "id2"},
			want: result{ids: []string{"id1", "id2"}, ok: true},
		},

		// --- Double-dash terminator ---
		{
			name: "double-dash: everything after is positional",
			args: []string{"update", "--", "gcy-dv7"},
			want: result{ids: []string{"gcy-dv7"}, ok: true},
		},
		{
			name: "double-dash: multiple ids after",
			args: []string{"close", "--force", "--", "id1", "id2"},
			want: result{ids: []string{"id1", "id2"}, ok: true},
		},

		// --- Fail-closed: ambiguous unknown flags ---
		// gcy-g4o demonstrated break: `close --session gcy-realbead gcy-dv7`
		// Previously the hand-rolled scanner lacked --session → returned
		// "gcy-realbead" as the ID, leaving gcy-dv7 unguarded. Now --session
		// is in the known set so it is correctly handled, but an *unknown*
		// value flag must trigger ambiguous.
		{
			name: "known --session flag handled correctly for close",
			args: []string{"close", "--session", "sess-id-abc", "gcy-dv7"},
			want: result{ids: []string{"gcy-dv7"}, ok: true},
		},
		{
			name: "unknown value flag triggers fail-closed",
			args: []string{"close", "--unknown-future-flag", "gcy-realbead", "gcy-dv7"},
			want: result{ok: true, ambiguous: true},
		},
		{
			name: "unknown short flag triggers fail-closed",
			args: []string{"update", "-z", "something", "gcy-dv7"},
			want: result{ok: true, ambiguous: true},
		},

		// --- Non-write subcommands ---
		{
			name: "show is not a write command",
			args: []string{"show", "gcy-dv7"},
			want: result{ok: false},
		},
		{
			name: "list is not a write command",
			args: []string{"list", "-s", "open"},
			want: result{ok: false},
		},
		{
			name: "empty args",
			args: []string{},
			want: result{ok: false},
		},

		// --- No IDs supplied (e.g. "last touched" fallback) ---
		{
			name: "update with no ids (last-touched fallback)",
			args: []string{"update", "--status", "closed"},
			want: result{ids: nil, ok: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ids, ok, ambiguous := bdMutationWriteIDs(tc.args)
			if ok != tc.want.ok {
				t.Errorf("ok = %v, want %v", ok, tc.want.ok)
			}
			if ambiguous != tc.want.ambiguous {
				t.Errorf("ambiguous = %v, want %v", ambiguous, tc.want.ambiguous)
			}
			if !bdTestSlicesEqual(ids, tc.want.ids) {
				t.Errorf("ids = %v, want %v", ids, tc.want.ids)
			}
		})
	}
}

func bdTestSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseBdReleaseIfCurrentArgs(t *testing.T) {
	id, assignee, ok, err := parseBdReleaseIfCurrentArgs([]string{"release-if-current", "gc-abc", "worker-1"})
	if err != nil {
		t.Fatalf("parseBdReleaseIfCurrentArgs unexpected error: %v", err)
	}
	if !ok || id != "gc-abc" || assignee != "worker-1" {
		t.Fatalf("parseBdReleaseIfCurrentArgs = (%q, %q, %v), want gc-abc worker-1 true", id, assignee, ok)
	}
	if _, _, ok, err := parseBdReleaseIfCurrentArgs([]string{"list"}); ok || err != nil {
		t.Fatalf("non-release command parsed as release: ok=%v err=%v", ok, err)
	}
	for _, args := range [][]string{
		{"release-if-current"},
		{"release-if-current", "gc-abc"},
		{"release-if-current", "gc-abc", "worker-1", "extra"},
		{"release-if-current", "", "worker-1"},
		{"release-if-current", "gc abc", "worker-1"},
		{"release-if-current", "gc-abc", "worker 1"},
	} {
		if _, _, ok, err := parseBdReleaseIfCurrentArgs(args); !ok || err == nil {
			t.Fatalf("parseBdReleaseIfCurrentArgs(%q) = ok=%v err=%v, want release usage error", args, ok, err)
		}
	}
}

func TestDoBdReleaseIfCurrentUpdatesOnlyMatchingAssignment(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	created, err := store.Create(beads.Bead{Title: "work", Assignee: "worker-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Update(created.ID, beads.UpdateOpts{Status: strPtr("in_progress")}); err != nil {
		t.Fatalf("Update status: %v", err)
	}

	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "gc"}
	var stdout, stderr bytes.Buffer
	if got := doBdReleaseIfCurrent(cityDir, target, created.ID, "worker-2", &stdout, &stderr); got != 0 {
		t.Fatalf("doBdReleaseIfCurrent wrong assignee = %d, want 0; stderr=%q", got, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "skipped" {
		t.Fatalf("wrong-assignee output = %q, want skipped", stdout.String())
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after skipped release: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-1" {
		t.Fatalf("skipped release mutated bead: %+v", got)
	}

	stdout.Reset()
	stderr.Reset()
	if got := doBdReleaseIfCurrent(cityDir, target, created.ID, "worker-1", &stdout, &stderr); got != 0 {
		t.Fatalf("doBdReleaseIfCurrent matching assignee = %d, want 0; stderr=%q", got, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "released" {
		t.Fatalf("matching-assignee output = %q, want released", stdout.String())
	}
	got, err = store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after release: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("released bead = %+v, want open and unassigned", got)
	}
}

func TestDoBdReleaseIfCurrentWorksForBdStoreFallback(t *testing.T) {
	clearInheritedBeadsEnv(t)
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
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
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
		t.Fatalf("write city.toml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir rig .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatalf("write rig metadata: %v", err)
	}
	fakeBin := t.TempDir()
	sqlLog := filepath.Join(fakeBin, "sql.log")
	fakeBD := "#!/bin/sh\n" +
		"if [ \"$1\" = \"sql\" ] && [ \"$2\" = \"--json\" ]; then\n" +
		"  printf '%s\\n' \"$3\" > " + strconv.Quote(sqlLog) + "\n" +
		"  printf '{\"rows_affected\":1,\"schema_version\":1}\\n'\n" +
		"  exit 0\n" +
		"fi\n" +
		"printf 'unexpected bd args:' >&2\n" +
		"printf ' %s' \"$@\" >&2\n" +
		"printf '\\n' >&2\n" +
		"exit 2\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "bd"), []byte(fakeBD), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	setCwd(t, cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_BEADS_FORCE_FALLBACK", "1")

	store, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	if _, ok := underlyingPolicyStoreForTest(store).(*beads.BdStore); !ok {
		t.Fatalf("openStoreAtForCity returned %T, want policy-wrapped *beads.BdStore", underlyingPolicyStoreForTest(store))
	}

	target := execStoreTarget{ScopeRoot: rigDir, ScopeKind: "rig", Prefix: "fe"}
	var stdout, stderr bytes.Buffer
	if got := doBdReleaseIfCurrent(cityDir, target, "fe-abc", "worker-1", &stdout, &stderr); got != 0 {
		t.Fatalf("doBdReleaseIfCurrent = %d, want 0; stderr=%q", got, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "released" {
		t.Fatalf("output = %q, want released", stdout.String())
	}
	query, err := os.ReadFile(sqlLog)
	if err != nil {
		t.Fatalf("read SQL log: %v", err)
	}
	wantQuery := "UPDATE issues SET status = 'open', assignee = '', updated_at = CURRENT_TIMESTAMP WHERE id = 'fe-abc' AND status = 'in_progress' AND assignee = 'worker-1'"
	if strings.TrimSpace(string(query)) != wantQuery {
		t.Fatalf("SQL query = %q, want %q", strings.TrimSpace(string(query)), wantQuery)
	}
}
