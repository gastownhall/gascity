package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestResolveTemplatePrependsGCBinDirToPATH(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", "/opt/homebrew/bin"+sep+"/usr/bin")

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{Name: "runner"}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	gcBin := tp.Env["GC_BIN"]
	if gcBin == "" {
		t.Fatal("GC_BIN is empty")
	}
	wantDir := filepath.Dir(gcBin)
	parts := strings.Split(tp.Env["PATH"], sep)
	if len(parts) == 0 || parts[0] != wantDir {
		t.Fatalf("PATH first entry = %q, want gc bin dir %q (PATH=%q)", parts[0], wantDir, tp.Env["PATH"])
	}
	count := 0
	for _, part := range parts {
		if part == wantDir {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("gc bin dir %q should appear exactly once, found %d in PATH=%q", wantDir, count, tp.Env["PATH"])
	}
}

func TestResolveTemplatePrependsGCBinDirToConfiguredAgentPATH(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", "/opt/homebrew/bin"+sep+"/usr/bin")

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	configuredPATH := "/custom/tools" + sep + "/usr/local/bin"
	agent := &config.Agent{
		Name: "runner",
		Env:  map[string]string{"PATH": configuredPATH},
	}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	gcBin := tp.Env["GC_BIN"]
	if gcBin == "" {
		t.Fatal("GC_BIN is empty")
	}
	wantDir := filepath.Dir(gcBin)
	parts := strings.Split(tp.Env["PATH"], sep)
	wantPrefix := []string{wantDir, "/custom/tools", "/usr/local/bin"}
	if len(parts) < len(wantPrefix) {
		t.Fatalf("PATH=%q has fewer entries than expected prefix %v", tp.Env["PATH"], wantPrefix)
	}
	for i, want := range wantPrefix {
		if parts[i] != want {
			t.Fatalf("PATH entry %d = %q, want %q (PATH=%q)", i, parts[i], want, tp.Env["PATH"])
		}
	}
}

func TestResolveTemplateUsesTrustedRuntimeRootForControlTraceDefault(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	customRuntimeDir := filepath.Join(t.TempDir(), "runtime-root")
	t.Setenv("GC_CITY_PATH", cityPath)
	t.Setenv("GC_CITY_RUNTIME_DIR", customRuntimeDir)

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{Name: "runner"}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	if got := tp.Env["GC_CITY_RUNTIME_DIR"]; got != customRuntimeDir {
		t.Fatalf("GC_CITY_RUNTIME_DIR = %q, want %q", got, customRuntimeDir)
	}
	wantTraceDefault := filepath.Join(customRuntimeDir, "control-dispatcher-trace.log")
	if got := tp.Env["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]; got != wantTraceDefault {
		t.Fatalf("GC_CONTROL_DISPATCHER_TRACE_DEFAULT = %q, want %q", got, wantTraceDefault)
	}
}

// TestResolveTemplateInjectsPerDispatcherTraceDefault asserts that
// resolveTemplate produces a per-dispatcher GC_CONTROL_DISPATCHER_TRACE_DEFAULT
// in agentEnv for control-dispatcher agents (closes #1650). The override
// goes in agentEnv (last in mergeEnv) so it deterministically wins over
// the uniform city-level default seeded by cityRuntimeEnvMapForCity.
func TestResolveTemplateInjectsPerDispatcherTraceDefault(t *testing.T) {
	cases := []struct {
		name          string
		dir           string
		qualifiedName string
		wantFilename  string
	}{
		{
			name:          "city dispatcher",
			dir:           "",
			qualifiedName: config.ControlDispatcherAgentName,
			wantFilename:  "control-dispatcher-trace.log",
		},
		{
			name:          "rig dispatcher uses double-dash filename",
			dir:           "app",
			qualifiedName: "app/control-dispatcher",
			wantFilename:  "app--control-dispatcher-trace.log",
		},
		{
			name:          "non-dispatcher agent untouched",
			dir:           "",
			qualifiedName: "polecat",
			wantFilename:  "control-dispatcher-trace.log", // city-uniform default preserved
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			writeTemplateResolveCityConfig(t, cityPath, "file")
			t.Setenv("GC_CITY_PATH", cityPath)
			t.Setenv("GC_CITY_RUNTIME_DIR", "")

			params := &agentBuildParams{
				cityName:   "city",
				cityPath:   cityPath,
				workspace:  &config.Workspace{Provider: "test"},
				providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
				lookPath:   func(string) (string, error) { return "/bin/echo", nil },
				fs:         fsys.OSFS{},
				beaconTime: time.Unix(0, 0),
				beadNames:  make(map[string]string),
				stderr:     io.Discard,
			}

			agentName := config.ControlDispatcherAgentName
			if tc.qualifiedName == "polecat" {
				agentName = "polecat"
			}
			agent := &config.Agent{Name: agentName, Dir: tc.dir}
			tp, err := resolveTemplate(params, agent, tc.qualifiedName, nil)
			if err != nil {
				t.Fatalf("resolveTemplate: %v", err)
			}

			wantPath := filepath.Join(cityPath, ".gc", "runtime", tc.wantFilename)
			if got := tp.Env["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]; got != wantPath {
				t.Fatalf("GC_CONTROL_DISPATCHER_TRACE_DEFAULT = %q, want %q", got, wantPath)
			}
		})
	}
}
