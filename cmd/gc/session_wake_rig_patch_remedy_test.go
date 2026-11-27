package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// A named session whose agent is suspended by a local [[rigs.patches]] entry
// (the agent itself comes from a rig-imported pack) is refused by wake with a
// `gc agent resume` remedy. Running that remedy on the direct-config fallback
// must succeed and clear the suspension the wake predicate reads (#8708).
func TestSessionWakeRefusalRemedyResumesRigPatchSuspendedAgent(t *testing.T) {
	cityDir := t.TempDir()
	packDir := filepath.Join(cityDir, "packs", "base")
	rigDir := filepath.Join(cityDir, "proj")
	for _, d := range []string{packDir, rigDir, filepath.Join(cityDir, ".gc")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(packDir, "pack.toml"), `[pack]
name = "base"
schema = 2

[[agent]]
name = "worker"
scope = "rig"
provider = "claude"
`)
	tomlPath := filepath.Join(cityDir, "city.toml")
	writeFile(tomlPath, `[workspace]
name = "test-city"

[providers.claude]
base = "builtin:claude"

[[rigs]]
name = "proj"

[rigs.imports.base]
source = "./packs/base"

# Held while the rig is being migrated.
[[rigs.patches]]
agent = "worker"
suspended = true
`)

	writeFile(filepath.Join(cityDir, ".gc", "site.toml"), "[[rig]]\nname = \"proj\"\npath = "+strconv.Quote(rigDir)+"\n")

	refusal := func() string {
		t.Helper()
		cfg, err := loadCityConfigFS(fsys.OSFS{}, tomlPath, io.Discard)
		if err != nil {
			t.Fatalf("loading city config: %v", err)
		}
		cs := &controllerState{cityPath: cityDir, cfg: cfg, cityBeadStore: beads.NewMemStore(), sp: runtime.NewFake()}
		why, _ := cs.WakeStartRefusal(sessionpkg.Info{
			Template:               "proj/base.worker",
			SessionNameMetadata:    "worker",
			ConfiguredNamedSession: true,
		})
		return why
	}

	why := refusal()
	const remedyPrefix = "run `gc agent resume "
	i := strings.Index(why, remedyPrefix)
	if i < 0 {
		t.Fatalf("wake refusal = %q, want an agent resume remedy", why)
	}
	target := strings.TrimSuffix(why[i+len(remedyPrefix):], "`")

	var stdout, stderr bytes.Buffer
	if code := doAgentResume(fsys.OSFS{}, cityDir, target, &stdout, &stderr); code != 0 {
		t.Fatalf("remedy `gc agent resume %s` exit = %d; stderr: %s", target, code, stderr.String())
	}
	if want := "Resumed agent '" + target + "'\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if why := refusal(); why != "" {
		t.Fatalf("wake still refused after the remedy: %q", why)
	}
}
