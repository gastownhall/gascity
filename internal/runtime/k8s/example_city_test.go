package k8s

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func k8sContribDir(t *testing.T) string {
	t.Helper()
	if root := bazeltest.OverrideRoot(); root != "" {
		return filepath.Join(root, "contrib", "k8s")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contrib", "k8s"))
}

// copyExampleCity lays the shipped example out the way its header documents:
// example-city.toml becomes <city>/city.toml, with pack.toml and agents/
// copied beside it.
func copyExampleCity(t *testing.T) string {
	t.Helper()
	src := k8sContribDir(t)
	city := t.TempDir()
	copyFile := func(from, to string) {
		t.Helper()
		data, err := os.ReadFile(from)
		if err != nil {
			t.Fatalf("read %s: %v", from, err)
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(to), err)
		}
		if err := os.WriteFile(to, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", to, err)
		}
	}
	copyFile(filepath.Join(src, "example-city.toml"), filepath.Join(city, "city.toml"))
	copyFile(filepath.Join(src, "pack.toml"), filepath.Join(city, "pack.toml"))
	err := filepath.WalkDir(filepath.Join(src, "agents"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		copyFile(path, filepath.Join(city, rel))
		return nil
	})
	if err != nil {
		t.Fatalf("copy agents/: %v", err)
	}
	return city
}

func loadExampleCity(t *testing.T, city string) (*config.City, *config.Provenance) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(city, "city.toml"))
	if err != nil {
		t.Fatalf("config.LoadWithIncludes: %v", err)
	}
	return cfg, prov
}

// TestExampleCityResolvesRunnableAgentsAndProvider covers the new-user path:
// a city copied from the shipped example must resolve at least one agent, and
// every provider it references must exist in the composed catalog.
func TestExampleCityResolvesRunnableAgentsAndProvider(t *testing.T) {
	cfg, _ := loadExampleCity(t, copyExampleCity(t))

	var names []string
	for _, a := range cfg.Agents {
		if a.Implicit {
			continue
		}
		names = append(names, a.Name)
	}
	for _, want := range []string{"coder", "mayor"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("example city agents = %v, want %q among them", names, want)
		}
	}
	if err := config.ValidateAgents(cfg.Agents); err != nil {
		t.Errorf("ValidateAgents: %v", err)
	}
	if missing := config.MissingProviderReferences(cfg); len(missing) != 0 {
		t.Errorf("example city references providers missing from the catalog: %+v", missing)
	}
	if _, ok := cfg.Providers[cfg.Workspace.Provider]; !ok {
		t.Errorf("workspace.provider %q is not in the provider catalog", cfg.Workspace.Provider)
	}
	for _, a := range cfg.Agents {
		if a.Name == "coder" || a.Name == "mayor" {
			if a.PromptTemplate == "" {
				t.Errorf("agent %q has no prompt_template", a.Name)
			} else if _, err := os.Stat(a.PromptTemplate); err != nil {
				t.Errorf("agent %q prompt_template %q does not resolve: %v", a.Name, a.PromptTemplate, err)
			}
		}
	}
}

// TestExampleCityHasNoUnsupportedKeys fails on any key the loader would
// silently drop: workspace.isolation and install_hooks are not config keys;
// install_agent_hooks is the supported spelling.
func TestExampleCityHasNoUnsupportedKeys(t *testing.T) {
	city := copyExampleCity(t)
	_, prov := loadExampleCity(t, city)
	for _, w := range prov.Warnings {
		if strings.Contains(w, "unknown field") {
			t.Errorf("example city load warning: %s", w)
		}
	}

	for _, rel := range []string{"city.toml", "agents/coder/agent.toml", "agents/mayor/agent.toml"} {
		var raw map[string]any
		if _, err := toml.DecodeFile(filepath.Join(city, rel), &raw); err != nil {
			t.Fatalf("decode %s: %v", rel, err)
		}
		if _, bad := raw["install_hooks"]; bad {
			t.Errorf("%s uses unsupported key install_hooks; use install_agent_hooks", rel)
		}
		if ws, ok := raw["workspace"].(map[string]any); ok {
			if _, bad := ws["isolation"]; bad {
				t.Errorf("%s sets unsupported workspace.isolation", rel)
			}
			if _, bad := ws["install_hooks"]; bad {
				t.Errorf("%s sets unsupported workspace.install_hooks", rel)
			}
		}
	}

	cfg, _ := loadExampleCity(t, city)
	for _, a := range cfg.Agents {
		if a.Name != "coder" && a.Name != "mayor" {
			continue
		}
		if len(a.InstallAgentHooks) != 1 || a.InstallAgentHooks[0] != "claude" {
			t.Errorf("agent %q install_agent_hooks = %v, want [claude]", a.Name, a.InstallAgentHooks)
		}
	}
}
