package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// An agent's own env BEADS_DIR replaces the generated default: the rig's
// .beads for a rig-scoped agent, the explicit empty value for a city-scoped
// one. It expands like the rest of the agent's env, an explicit empty value
// stays explicit (so tmux still unsets an inherited BEADS_DIR), and GC_RIG,
// GC_RIG_ROOT and GC_BEADS_SCOPE_ROOT stay generated.
func TestResolveTemplateAgentEnvBeadsDirOverridesGeneratedDefault(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	rigRoot := filepath.Join(cityPath, "demo")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		rigs:       []config.Rig{{Name: "demo", Path: rigRoot}},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	const sentinel = "/nonexistent/no-raw-bd/.beads"
	t.Setenv("GC_TEST_NO_RAW_BD_ROOT", "/nonexistent/expanded")
	for _, tc := range []struct {
		name string
		dir  string // the agent's rig; empty for a city-scoped agent
		env  map[string]string
		want string
	}{
		{name: "rig default", dir: "demo", want: filepath.Join(rigRoot, ".beads")},
		{name: "rig agent env", dir: "demo", env: map[string]string{"BEADS_DIR": sentinel}, want: sentinel},
		{name: "rig agent env expands", dir: "demo", env: map[string]string{"BEADS_DIR": "$GC_TEST_NO_RAW_BD_ROOT/.beads"}, want: "/nonexistent/expanded/.beads"},
		{name: "rig agent env explicit empty", dir: "demo", env: map[string]string{"BEADS_DIR": ""}, want: ""},
		{name: "city default", want: ""},
		{name: "city agent env", env: map[string]string{"BEADS_DIR": sentinel}, want: sentinel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &config.Agent{Name: "worker", Dir: tc.dir, Env: tc.env}
			tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
			if err != nil {
				t.Fatalf("resolveTemplate: %v", err)
			}
			if got, ok := tp.Env["BEADS_DIR"]; !ok || got != tc.want {
				t.Errorf("BEADS_DIR = %q (present %t), want %q", got, ok, tc.want)
			}
			wantRig, wantRigRoot, wantScopeRoot := "", "", cityPath
			if tc.dir != "" {
				wantRig, wantRigRoot, wantScopeRoot = tc.dir, rigRoot, rigRoot
			}
			for _, key := range []struct{ name, want string }{
				{"GC_RIG", wantRig},
				{"GC_RIG_ROOT", wantRigRoot},
				{"GC_BEADS_SCOPE_ROOT", wantScopeRoot},
			} {
				if got := tp.Env[key.name]; got != key.want {
					t.Errorf("%s = %q, want %q", key.name, got, key.want)
				}
			}
		})
	}
}
