package t3bridge_gastown_test

import (
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func exampleDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Dir(filename)
}

func TestT3BridgeGastownExampleParses(t *testing.T) {
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
