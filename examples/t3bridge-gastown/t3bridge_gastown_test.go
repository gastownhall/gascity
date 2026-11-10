package t3bridge_gastown_test

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

func exampleDir() string {
	if root := bazeltest.OverrideRoot(); root != "" {
		return filepath.Join(root, "examples", "t3bridge-gastown")
	}
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Dir(filename)
}

// primeBundledPackCache hydrates a hermetic repo cache with the bundled
// builtin pack content at the pinned commit so the example's packs.lock
// resolves offline.
func primeBundledPackCache(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	commit := strings.TrimPrefix(config.BundledPackImportVersion, "sha:")
	coreSource, ok := builtinpacks.Source("core")
	if !ok {
		t.Fatal("bundled core pack not registered")
	}
	cachePath, err := packman.RepoCachePath(coreSource, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := builtinpacks.MaterializeSyntheticRepo(cachePath, builtinpacks.Repository, commit); err != nil {
		t.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
}

func TestT3BridgeGastownExampleParses(t *testing.T) {
	primeBundledPackCache(t)
	dir := exampleDir()
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Workspace.Name; got != "t3bridge-gastown" {
		t.Fatalf("workspace.name = %q, want t3bridge-gastown", got)
	}
	if got := cfg.Session.Provider; got != "t3bridge" {
		t.Fatalf("session.provider = %q, want t3bridge", got)
	}
	if _, ok := cfg.Imports["t3demo"]; !ok {
		t.Fatalf("missing t3demo import")
	}
	for _, want := range []string{"polecat", "witness", "refinery"} {
		if !slices.ContainsFunc(cfg.Agents, func(a config.Agent) bool { return a.Name == want }) {
			t.Fatalf("missing imported agent %q; agents=%v", want, agentNames(cfg.Agents))
		}
	}
	for _, want := range []string{"example/t3demo.witness", "example/t3demo.refinery"} {
		if !slices.ContainsFunc(cfg.NamedSessions, func(s config.NamedSession) bool { return s.QualifiedName() == want }) {
			t.Fatalf("missing named session %q; sessions=%v", want, namedSessionNames(cfg.NamedSessions))
		}
	}
}

// TestT3BridgeGastownPolecatsClaimWithHookClaim pins the polecat prompt and
// nudge text to `gc hook --claim`. Plain `gc hook` only lists routed work, so
// a polecat told that it claims can work a bead it never claimed, with no
// assignee and no lease.
func TestT3BridgeGastownPolecatsClaimWithHookClaim(t *testing.T) {
	dir := exampleDir()
	// flatten joins wrapped lines and folds case, so a phrase is found
	// wherever the prose breaks it or a sentence starts with it.
	flatten := func(text string) string { return strings.ToLower(strings.Join(strings.Fields(text), " ")) }
	// The earlier wordings, each of which sent the polecat to plain `gc hook`
	// for its work.
	banned := []string{
		"run `gc hook` to claim",
		"run 'gc hook';",
		"running `gc hook` if",
		"run gc hook,",
	}
	for _, pack := range []string{"t3demo", "gastown"} {
		for _, name := range []string{"prompt.template.md", "agent.toml"} {
			path := filepath.Join(dir, "packs", pack, "agents", "polecat", name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			flat := flatten(string(data))
			if !strings.Contains(flat, "gc hook --claim") {
				t.Errorf("%s/%s: want the claim step to be `gc hook --claim`", pack, name)
			}
			for _, phrase := range banned {
				if strings.Contains(flat, phrase) {
					t.Errorf("%s/%s: describes plain gc hook as the claim step (%q)", pack, name, phrase)
				}
			}
			if name == "prompt.template.md" && !strings.Contains(flat, "a failed claim means another agent holds that bead: do not work it") {
				t.Errorf("%s/%s: missing the failed-claim rule", pack, name)
			}
		}
	}
}

func agentNames(agents []config.Agent) []string {
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		names = append(names, a.Name)
	}
	return names
}

func namedSessionNames(sessions []config.NamedSession) []string {
	names := make([]string, 0, len(sessions))
	for _, s := range sessions {
		names = append(names, s.QualifiedName())
	}
	return names
}
