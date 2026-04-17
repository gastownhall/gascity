//go:build acceptance_a

package acceptance_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

type namedSessionListEntry struct {
	Template    string `json:"Template"`
	SessionName string `json:"SessionName"`
}

func TestImportedNamedSessionsUseSafeRuntimeNames(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	rigDir := filepath.Join(c.Dir, "repo")
	mustWriteTestFile(t, filepath.Join(c.Dir, "pack.toml"), `
[pack]
name = "import-regression"
schema = 2

[imports.gs]
source = "./assets/sidecar"
`)
	mustWriteTestFile(t, filepath.Join(c.Dir, "city.toml"), `
[workspace]
name = "import-regression"
start_command = "sleep 300"

[[rigs]]
name = "repo"
path = "./repo"

[rigs.imports.gs]
source = "./assets/sidecar"
`)
	mustWriteTestFile(t, filepath.Join(c.Dir, "assets", "sidecar", "pack.toml"), `
[pack]
name = "sidecar"
schema = 2

[[named_session]]
template = "captain"
scope = "city"
mode = "always"

[[named_session]]
template = "watcher"
scope = "rig"
mode = "always"
`)
	mustWriteTestFile(t, filepath.Join(c.Dir, "assets", "sidecar", "agents", "captain", "agent.toml"), "scope = \"city\"\n")
	mustWriteTestFile(t, filepath.Join(c.Dir, "assets", "sidecar", "agents", "captain", "prompt.md"), "You are the imported captain.\n")
	mustWriteTestFile(t, filepath.Join(c.Dir, "assets", "sidecar", "agents", "watcher", "agent.toml"), "scope = \"rig\"\n")
	mustWriteTestFile(t, filepath.Join(c.Dir, "assets", "sidecar", "agents", "watcher", "prompt.md"), "You are the imported watcher.\n")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}

	if out, err := c.GC("unregister", c.Dir); err != nil {
		t.Fatalf("gc unregister: %v\n%s", err, out)
	}

	c.StartForeground()

	var sessions []namedSessionListEntry
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.GC("session", "list", "--json")
		if err == nil {
			if unmarshalErr := json.Unmarshal([]byte(out), &sessions); unmarshalErr == nil {
				if hasNamedSession(sessions, "gs.captain", "gs__captain") &&
					hasNamedSession(sessions, "repo/gs.watcher", "repo--gs__watcher") {
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	out, err := c.GC("session", "list", "--json")
	if err != nil {
		t.Fatalf("gc session list --json: %v\n%s", err, out)
	}
	t.Fatalf("imported named sessions never reached safe runtime names:\n%s", out)
}

func hasNamedSession(sessions []namedSessionListEntry, template, sessionName string) bool {
	for _, s := range sessions {
		if s.Template == template && s.SessionName == sessionName {
			return true
		}
	}
	return false
}

func mustWriteTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
