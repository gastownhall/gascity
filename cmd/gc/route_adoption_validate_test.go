package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// unroutedTargetValidationConfig is a city with a default-query pool, a
// custom-sling_query agent, an imported (binding "gc") agent, a city-scoped
// agent that reads every store, a "hello-world" rig with a rig-scoped pool
// and an "other" rig with an agent of its own.
func unroutedTargetValidationConfig(t *testing.T) *config.City {
	t.Helper()
	return &config.City{
		Workspace: config.Workspace{Name: "city", Prefix: "ct"},
		Rigs: []config.Rig{
			{Name: "hello-world", Path: t.TempDir(), Prefix: "hw"},
			{Name: "other", Path: t.TempDir(), Prefix: "ot"},
		},
		Agents: []config.Agent{
			{Name: adoptionTestCityPool, MinActiveSessions: ptr(0), MaxActiveSessions: ptr(3)},
			{Name: "custom-router", SlingQuery: "my-router {}"},
			{Name: "scribe", BindingName: "gc"},
			{Name: "roamer", Scope: "city"},
			{Name: "researcher", Dir: "hello-world", MinActiveSessions: ptr(0), MaxActiveSessions: ptr(3)},
			{Name: "analyst", Dir: "other"},
		},
	}
}

// TestValidateUnroutedWorkTargets is AC-5's validator: a target must resolve
// to a configured agent the way sling resolves it, use built-in routing, and
// name an agent whose pool reads the scope's bead store, as sling's
// cross-store guard requires; the error names the scope,
// unrouted_work_target and the value.
func TestValidateUnroutedWorkTargets(t *testing.T) {
	cases := []struct {
		name      string
		workspace string
		rig       string
		wantErr   []string
	}{
		{name: "no targets"},
		{name: "valid city target", workspace: adoptionTestCityPool},
		{name: "valid rig target", rig: "hello-world/researcher"},
		{name: "bare rig agent name", rig: "researcher"},
		{name: "bare rig pool-instance name", rig: "researcher-2"},
		{name: "imported agent by its name alone", workspace: "scribe"},
		{name: "city-scoped agent serves the city", workspace: "roamer"},
		{name: "city-scoped agent serves a rig", rig: "roamer"},
		{name: "city target names no agent", workspace: "nobody", wantErr: []string{"workspace", "unrouted_work_target", `"nobody"`}},
		{name: "city target has custom sling_query", workspace: "custom-router", wantErr: []string{"workspace", "unrouted_work_target", `"custom-router"`, "sling_query"}},
		{name: "rig target names no agent", rig: "hello-world/ghost", wantErr: []string{`rig "hello-world"`, "unrouted_work_target", `"hello-world/ghost"`}},
		{name: "city target is confined to a rig", workspace: "hello-world/researcher", wantErr: []string{"workspace", "unrouted_work_target", `"hello-world/researcher"`, "reads rig:hello-world", "city:city"}},
		{name: "rig target is confined to another rig", rig: "other/analyst", wantErr: []string{`rig "hello-world"`, "unrouted_work_target", `"other/analyst"`, "reads rig:other", "rig:hello-world"}},
		{name: "rig target reads only the city store", rig: adoptionTestCityPool, wantErr: []string{`rig "hello-world"`, "unrouted_work_target", `"ccc-researcher"`, "reads city:city", "rig:hello-world"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := unroutedTargetValidationConfig(t)
			cfg.Workspace.UnroutedWorkTarget = tc.workspace
			cfg.Rigs[0].UnroutedWorkTarget = tc.rig
			err := validateUnroutedWorkTargets(cfg, t.TempDir())
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("validateUnroutedWorkTargets() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validateUnroutedWorkTargets() = nil, want an error")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %s", err, want)
				}
			}
		})
	}
}

const invalidUnroutedTargetCityTOML = `[workspace]
name = "test-city"
unrouted_work_target = "nobody"

[beads]
provider = "file"

[session]
provider = "fake"
`

// TestStartRefusesInvalidUnroutedWorkTarget is AC-5's start path: gc start
// exits non-zero on a target that names no agent.
func TestStartRefusesInvalidUnroutedWorkTarget(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityPath, invalidUnroutedTargetCityTOML)
	oldDry := dryRunMode
	t.Cleanup(func() { dryRunMode = oldDry })
	dryRunMode = true

	var stdout, stderr bytes.Buffer
	if code := doStartStandalone([]string{cityPath}, false, &stdout, &stderr); code == 0 {
		t.Fatalf("doStartStandalone exit = 0, want non-zero\nstderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `unrouted_work_target "nobody"`) {
		t.Fatalf("stderr = %q, want the unrouted_work_target refusal", stderr.String())
	}
}

// TestConfigShowValidateReportsInvalidUnroutedWorkTarget is AC-5's
// `gc config show --validate` path.
func TestConfigShowValidateReportsInvalidUnroutedWorkTarget(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, dir, invalidUnroutedTargetCityTOML)
	oldCity := cityFlag
	t.Cleanup(func() { cityFlag = oldCity })
	cityFlag = dir

	var stdout, stderr bytes.Buffer
	if code := doConfigShow(true, false, false, &stdout, &stderr); code == 0 {
		t.Fatalf("doConfigShow(validate) = 0, want non-zero\nstderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `unrouted_work_target "nobody"`) {
		t.Fatalf("stderr = %q, want the unrouted_work_target error", stderr.String())
	}
}

// TestReloadRefusesInvalidUnroutedWorkTarget is AC-5's reload path: the
// controller refuses the reload and keeps the previous config in effect.
func TestReloadRefusesInvalidUnroutedWorkTarget(t *testing.T) {
	oldStrict := strictMode
	strictMode = false
	t.Cleanup(func() { strictMode = oldStrict })

	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		LogPrefix: "gc reload",
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	oldCfg := cr.cfg
	if err := os.WriteFile(tomlPath, []byte(invalidUnroutedTargetCityTOML), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeFailed)
	}
	if !strings.Contains(reply.Error, `unrouted_work_target "nobody"`) {
		t.Fatalf("reply.Error = %q, want the unrouted_work_target refusal", reply.Error)
	}
	if cr.cfg != oldCfg {
		t.Fatal("cfg changed after a refused reload")
	}
}
