package workselectors_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func exampleDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Dir(filename)
}

func TestCityTomlValidates(t *testing.T) {
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(exampleDir(), "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if err := config.ValidateAgents(cfg.Agents); err != nil {
		t.Fatalf("ValidateAgents: %v", err)
	}
}

func TestTriagerSelectorsMatch(t *testing.T) {
	agents, err := config.DiscoverPackAgents(
		fsys.OSFS{},
		filepath.Join(exampleDir(), "packs", "work-selectors"),
		"work-selectors",
		nil,
	)
	if err != nil {
		t.Fatalf("DiscoverPackAgents: %v", err)
	}
	var names []string
	var triager *config.Agent
	for i := range agents {
		names = append(names, agents[i].QualifiedName())
		if agents[i].Name == "triager" || agents[i].QualifiedName() == "work-selectors.triager" {
			triager = &agents[i]
			break
		}
	}
	if triager == nil {
		t.Fatalf("triager agent not found; agents: %s", strings.Join(names, ", "))
	}
	if triager.ScaleCheckQuery.IsZero() {
		t.Fatal("triager scale_check_query is zero")
	}
	if triager.WorkSelector.IsZero() {
		t.Fatal("triager work_selector is zero")
	}
	if !triager.ScaleCheckQuery.Equivalent(triager.WorkSelector) {
		t.Fatalf("scale_check_query = %+v, want equivalent to work_selector %+v", triager.ScaleCheckQuery, triager.WorkSelector)
	}
}
