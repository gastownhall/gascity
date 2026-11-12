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

// A rig-scoped agent's own env may name BEADS_DIR, so a pack can point its
// sessions' raw bd at no store while gc bd, which pins the store it routes to,
// keeps writing the rig's ledger. Without it, the generated rig default wins.
func TestResolveTemplateAgentEnvBeadsDirOverridesRigDefault(t *testing.T) {
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
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"rig default", nil, filepath.Join(rigRoot, ".beads")},
		{"agent env", map[string]string{"BEADS_DIR": sentinel}, sentinel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &config.Agent{Name: "worker", Dir: "demo", Env: tc.env}
			tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
			if err != nil {
				t.Fatalf("resolveTemplate: %v", err)
			}
			if got := tp.Env["BEADS_DIR"]; got != tc.want {
				t.Fatalf("BEADS_DIR = %q, want %q", got, tc.want)
			}
			if got := tp.Env["GC_BEADS_SCOPE_ROOT"]; got != rigRoot {
				t.Fatalf("GC_BEADS_SCOPE_ROOT = %q, want the rig root %q", got, rigRoot)
			}
			if got := tp.Env["GC_RIG_ROOT"]; got != rigRoot {
				t.Fatalf("GC_RIG_ROOT = %q, want the rig root %q", got, rigRoot)
			}
		})
	}
}
