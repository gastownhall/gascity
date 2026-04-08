package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

type registeredRigFixture struct {
	cityPath string
	rigDir   string
	workDir  string
	rigName  string
}

func setupRegisteredRigFixture(t *testing.T, insideCity, suspended bool) registeredRigFixture {
	t.Helper()
	configureIsolatedRuntimeEnv(t)
	resetFlags(t)

	cityPath := setupCity(t, "demo-city")
	rigName := "frontend"

	var rigDir string
	var rigPath string
	if insideCity {
		rigDir = filepath.Join(cityPath, "rigs", rigName)
		rigPath = filepath.Join("rigs", rigName)
	} else {
		rigDir = filepath.Join(t.TempDir(), rigName)
		rigPath = rigDir
	}
	workDir := filepath.Join(rigDir, "src", "deep")
	if err := os.MkdirAll(filepath.Join(rigDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	toml := fmt.Sprintf(`[workspace]
name = "demo-city"

[session]
provider = "fake"

[beads]
provider = "file"

[[agent]]
name = "mayor"

[[rigs]]
name = %q
path = %q
`, rigName, rigPath)
	if suspended {
		toml += "suspended = true\n"
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := registryAt(t, os.Getenv("GC_HOME"))
	if err := reg.RegisterRig(rigDir, rigName, cityPath); err != nil {
		t.Fatal(err)
	}

	return registeredRigFixture{
		cityPath: cityPath,
		rigDir:   rigDir,
		workDir:  workDir,
		rigName:  rigName,
	}
}

func TestRigAnywhere_ResolveCommandContext(t *testing.T) {
	cases := []struct {
		name       string
		insideCity bool
		useArg     bool
	}{
		{name: "cwd_inside_city", insideCity: true},
		{name: "cwd_outside_city", insideCity: false},
		{name: "arg_inside_city", insideCity: true, useArg: true},
		{name: "arg_outside_city", insideCity: false, useArg: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := setupRegisteredRigFixture(t, tc.insideCity, false)

			args := []string(nil)
			if tc.useArg {
				setCwd(t, t.TempDir())
				args = []string{fx.workDir}
			} else {
				setCwd(t, fx.workDir)
			}

			ctx, err := resolveCommandContext(args)
			if err != nil {
				t.Fatalf("resolveCommandContext() error: %v", err)
			}
			if ctx.CityPath != fx.cityPath {
				t.Fatalf("CityPath = %q, want %q", ctx.CityPath, fx.cityPath)
			}
			if ctx.RigName != fx.rigName {
				t.Fatalf("RigName = %q, want %q", ctx.RigName, fx.rigName)
			}
		})
	}
}

func TestResolveCommandContextPathValidatesExactCityRootAtHomeBoundary(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	resetFlags(t)

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	cityPath := homeDir
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"home-city\"\n\n[[agent]]\nname = \"mayor\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setCwd(t, t.TempDir())

	ctx, err := resolveCommandContext([]string{cityPath})
	if err != nil {
		t.Fatalf("resolveCommandContext() error: %v", err)
	}
	if ctx.CityPath != cityPath {
		t.Fatalf("CityPath = %q, want %q", ctx.CityPath, cityPath)
	}
	if ctx.RigName != "" {
		t.Fatalf("RigName = %q, want empty", ctx.RigName)
	}
}

func TestRigAnywhere_RequireBootstrappedCityFromRigDir(t *testing.T) {
	for _, insideCity := range []bool{true, false} {
		name := "outside_city"
		if insideCity {
			name = "inside_city"
		}
		t.Run(name, func(t *testing.T) {
			fx := setupRegisteredRigFixture(t, insideCity, false)

			got, err := requireBootstrappedCity(fx.workDir)
			if err != nil {
				t.Fatalf("requireBootstrappedCity(%q): %v", fx.workDir, err)
			}
			if got != fx.cityPath {
				t.Fatalf("requireBootstrappedCity(%q) = %q, want %q", fx.workDir, got, fx.cityPath)
			}
		})
	}
}

func TestRigAnywhere_CmdStopFromRigDir(t *testing.T) {
	for _, insideCity := range []bool{true, false} {
		name := "outside_city"
		if insideCity {
			name = "inside_city"
		}
		t.Run(name, func(t *testing.T) {
			fx := setupRegisteredRigFixture(t, insideCity, false)
			setCwd(t, fx.workDir)

			var stdout, stderr bytes.Buffer
			code := cmdStop(nil, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "City stopped.") {
				t.Fatalf("stdout = %q, want City stopped.", stdout.String())
			}
			if strings.Contains(stderr.String(), "city.toml") {
				t.Fatalf("stderr = %q, should not contain city.toml resolution failures", stderr.String())
			}
		})
	}
}

func TestRigAnywhere_CmdRigSuspendFromRigDir(t *testing.T) {
	for _, insideCity := range []bool{true, false} {
		name := "outside_city"
		if insideCity {
			name = "inside_city"
		}
		t.Run(name, func(t *testing.T) {
			fx := setupRegisteredRigFixture(t, insideCity, false)
			setCwd(t, fx.workDir)

			var stdout, stderr bytes.Buffer
			code := cmdRigSuspend(nil, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("cmdRigSuspend() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "Suspended rig '"+fx.rigName+"'") {
				t.Fatalf("stdout = %q, want rig suspend confirmation", stdout.String())
			}

			cfg, err := config.Load(fsys.OSFS{}, filepath.Join(fx.cityPath, "city.toml"))
			if err != nil {
				t.Fatalf("load city config: %v", err)
			}
			if len(cfg.Rigs) != 1 || !cfg.Rigs[0].Suspended {
				t.Fatalf("rig suspended = %v, want true", cfg.Rigs)
			}
		})
	}
}

func TestRigAnywhere_CmdRigStatusFromRigDir(t *testing.T) {
	for _, insideCity := range []bool{true, false} {
		name := "outside_city"
		if insideCity {
			name = "inside_city"
		}
		t.Run(name, func(t *testing.T) {
			fx := setupRegisteredRigFixture(t, insideCity, false)
			setCwd(t, fx.workDir)

			var stdout, stderr bytes.Buffer
			code := cmdRigStatus(nil, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("cmdRigStatus() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), fx.rigName+":") {
				t.Fatalf("stdout = %q, want rig name header", stdout.String())
			}
		})
	}
}

func TestRigAnywhere_CmdRigRestartFromRigDir(t *testing.T) {
	for _, insideCity := range []bool{true, false} {
		name := "outside_city"
		if insideCity {
			name = "inside_city"
		}
		t.Run(name, func(t *testing.T) {
			fx := setupRegisteredRigFixture(t, insideCity, false)
			setCwd(t, fx.workDir)

			var stdout, stderr bytes.Buffer
			code := cmdRigRestart(nil, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("cmdRigRestart() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "Restarted") {
				t.Fatalf("stdout = %q, want restart confirmation", stdout.String())
			}
		})
	}
}

func TestRigAnywhere_CmdRigResumeFromRigDir(t *testing.T) {
	for _, insideCity := range []bool{true, false} {
		name := "outside_city"
		if insideCity {
			name = "inside_city"
		}
		t.Run(name, func(t *testing.T) {
			fx := setupRegisteredRigFixture(t, insideCity, true)
			setCwd(t, fx.workDir)

			var stdout, stderr bytes.Buffer
			code := cmdRigResume(nil, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("cmdRigResume() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "Resumed rig '"+fx.rigName+"'") {
				t.Fatalf("stdout = %q, want rig resume confirmation", stdout.String())
			}

			cfg, err := config.Load(fsys.OSFS{}, filepath.Join(fx.cityPath, "city.toml"))
			if err != nil {
				t.Fatalf("load city config: %v", err)
			}
			if len(cfg.Rigs) != 1 || cfg.Rigs[0].Suspended {
				t.Fatalf("rig suspended = %v, want false", cfg.Rigs)
			}
		})
	}
}
