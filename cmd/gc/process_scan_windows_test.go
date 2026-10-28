//go:build windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/registry"
)

type scanScopeRecorder struct {
	runtime.Provider
	executables []string
}

func (p *scanScopeRecorder) SetScanExecutables(names []string) {
	p.executables = names
}

func TestConfigureProcessTableScan(t *testing.T) {
	actual, err := newSessionProviderForCityByName(&config.City{}, "subprocess", config.SessionConfig{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := actual.(interface{ SetScanExecutables([]string) }); !ok {
		t.Fatal("production subprocess constructor lost executable-scope forwarding")
	}
	p := &scanScopeRecorder{}
	oldRegistry := runtimeRegistry
	runtimeRegistry = registry.New()
	t.Cleanup(func() { runtimeRegistry = oldRegistry })
	if err := runtimeRegistry.Register("subprocess", func(string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return p, nil
	}); err != nil {
		t.Fatal(err)
	}
	resolve := func(cfg *config.City) {
		t.Helper()
		if _, err := resolveWorkerSpec(cfg, runtime.WorkerSpec{Runtime: "subprocess"}, config.SessionConfig{}, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	resolve(nil)
	if p.executables != nil {
		t.Fatal("city-less provider must retain strict scanning")
	}
	cfg := &config.City{
		Workspace: config.Workspace{StartCommand: "workspace-agent --run"},
		Providers: map[string]config.ProviderSpec{
			"custom": {
				Command: `& 'C:\tools\custom-agent.exe' --run`, ACPCommand: "acp-agent --stdio",
				PathCheck: "real-agent", ProcessNames: []string{"custom-host"},
			},
		},
		Agents: []config.Agent{{
			StartCommand:  `& 'C:\agent tools\runner.exe' --run`,
			ResumeCommand: "resume-agent --continue", ProcessNames: []string{"child-host"},
		}},
	}
	resolve(cfg)
	for _, name := range []string{
		"gc", "bd", "cmd", "pwsh", "powershell", "workspace-agent",
		`C:\tools\custom-agent.exe`, "custom-host", `C:\agent tools\runner.exe`,
		"resume-agent", "child-host", "acp-agent", "real-agent",
	} {
		if !slices.Contains(p.executables, name) {
			t.Errorf("scan scope omitted %q: %v", name, p.executables)
		}
	}
	if slices.Contains(p.executables, "") {
		t.Error("scan scope retained an empty executable")
	}
}

func TestCityRuntimeExecutablesRefusesDynamicCommands(t *testing.T) {
	for _, command := range []string{
		"& $env:AGENT_EXE --run",
		"known-agent; & $env:AGENT_EXE",
		"Invoke-Expression 'custom-agent'",
		"Start-Process custom-agent",
		"known-agent & child-agent",
		`known-agent; { & 'C:\x\child-agent.exe' }.Invoke()`,
		"known-agent @{ run = (child-agent) }",
		`ii 'C:\x\child-agent.exe'`,
		`Invoke-Item 'C:\x\child-agent.exe'`,
	} {
		t.Run(command, func(t *testing.T) {
			for _, cfg := range []*config.City{
				{Workspace: config.Workspace{StartCommand: command}},
				{Agents: []config.Agent{{StartCommand: command}}},
				{Providers: map[string]config.ProviderSpec{"custom": {Command: command}}},
				{ResolvedProviders: map[string]config.ResolvedProvider{"custom": {ACPCommand: command}}},
			} {
				if names := cityRuntimeExecutables(cfg); len(names) != 0 {
					t.Fatalf("unresolved command retained partial scope: %v", names)
				}
			}
		})
	}
}

func TestCityRuntimeExecutablesReportsUnresolvedCommand(t *testing.T) {
	t.Cleanup(unresolvedScanCommands.Clear)
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	cfg := &config.City{Providers: map[string]config.ProviderSpec{
		"unresolved-provider": {Command: "& $env:UNRESOLVED_TEST_AGENT"},
	}}
	for range 2 {
		cityRuntimeExecutables(cfg)
	}
	if strings.Count(output.String(), `providers["unresolved-provider"].command`) != 1 {
		t.Fatalf("expected one diagnostic identifying the unresolved command field: %s", output.String())
	}
	if strings.Contains(output.String(), "UNRESOLVED_TEST_AGENT") {
		t.Fatal("diagnostic must identify the field without exposing command contents")
	}
}

func TestCityRuntimeReloadRefreshesScanScope(t *testing.T) {
	clearInheritedBeadsEnv(t)
	t.Setenv("GC_SESSION", "")
	dir := shortSocketTempDir(t, "gc-scan-")
	tomlPath := filepath.Join(dir, "city.toml")
	writeConfig := func(command, provider string) {
		t.Helper()
		text := fmt.Sprintf("[workspace]\nname = \"test\"\nstart_command = %q\n[beads]\nprovider = \"file\"\n[session]\nprovider = %q\n", command, provider)
		if err := os.WriteFile(tomlPath, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("before-agent", "fake")
	initial, err := tryReloadConfig(tomlPath, "test", dir)
	if err != nil {
		t.Fatal(err)
	}
	p := &scanScopeRecorder{Provider: runtime.NewFake()}
	oldRegistry := runtimeRegistry
	runtimeRegistry = registry.New()
	t.Cleanup(func() { runtimeRegistry = oldRegistry })
	if err := runtimeRegistry.Register("replacement", func(string, config.SessionConfig, string, string) (runtime.Provider, error) {
		p = &scanScopeRecorder{Provider: runtime.NewFake()}
		return p, nil
	}); err != nil {
		t.Fatal(err)
	}
	sp := sessionauto.New(p, runtime.NewFake())
	configureProcessTableScan(sp, initial.Cfg)
	var stdout, stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath: dir, cityName: "test", configName: "test", tomlPath: tomlPath,
		configRev: initial.Revision, cfg: initial.Cfg, sp: sp, dops: newDrainOps(sp),
		rec: events.Discard, stdout: &stdout, stderr: &stderr, logPrefix: "gc test",
	}
	lastProviderName := "fake"
	for _, step := range []struct {
		command, provider string
		strict            bool
	}{
		{"after-agent", "fake", false},
		{"replacement-agent", "replacement", false},
		{"& $env:UNKNOWN_AGENT", "replacement", true},
		{"restored-agent", "replacement", true},
	} {
		writeConfig(step.command, step.provider)
		reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, dir, nil, reloadSourceManual)
		if reply.Outcome != reloadOutcomeApplied {
			t.Fatalf("reload failed: %+v; stderr=%s", reply, stderr.String())
		}
		if step.provider == "fake" && cr.sp != sp {
			t.Fatal("same-name reload replaced the wrapped provider")
		}
		if step.provider == "replacement" && cr.sp == sp {
			t.Fatal("changed runtime selection did not replace the provider")
		}
		if step.strict {
			if len(p.executables) != 0 {
				t.Errorf("reload %q lost strict scope: %v", step.command, p.executables)
			}
			continue
		}
		for _, name := range []string{"before-agent", "after-agent", step.command} {
			if !slices.Contains(p.executables, name) {
				t.Errorf("reload %q lost executable %q: %v", step.command, name, p.executables)
			}
		}
	}
}
