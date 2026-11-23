package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestCrossStoreDemandLegsCarryOwnClaimView: every city leg a rig pool's
// default probe reads, on the generic path and the named-backing path, carries
// that pool's own store as its claim view; no own leg, city-scoped pool leg,
// city-aliased rig pool leg or store-scoped control-dispatcher leg does, and
// neither does a class-binding leg, whose rows no work ledger holds.
func TestCrossStoreDemandLegsCarryOwnClaimView(t *testing.T) {
	cityPath := t.TempDir()
	rigPaths := map[string]string{}
	for _, name := range []string{"gascity", "aliased"} {
		p := filepath.Join(cityPath, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		rigPaths[name] = p
	}
	maxSess, minSess := 3, 0
	pool := func(name, dir string) config.Agent {
		return config.Agent{Name: name, Dir: dir, StartCommand: "true", MaxActiveSessions: &maxSess, MinActiveSessions: &minSess}
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Rigs:      []config.Rig{{Name: "gascity", Path: rigPaths["gascity"]}, {Name: "aliased", Path: rigPaths["aliased"]}},
		Agents: []config.Agent{
			pool("worker", "gascity"),
			pool("named", "gascity"),
			pool("cityworker", ""),
			pool("aliasworker", "aliased"),
			pool(config.ControlDispatcherAgentName, "gascity"),
		},
		NamedSessions: []config.NamedSession{{Template: "named", Dir: "gascity", Mode: "on_demand"}},
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{"gascity": rigStore, "aliased": cityStore}

	targets := buildDemandTargets("gc", cityPath, cfg, cityStore, rigStores, nil, nil, controllerQueryRuntimeEnv, io.Discard)

	crossStore := map[string]int{}
	check := func(list string, all []defaultScaleCheckTarget) {
		for _, target := range all {
			switch {
			case (target.template == "gascity/worker" || target.template == "gascity/named") && target.storeKey == "city":
				if target.claimView != rigStore || target.claimViewKey != "rig:gascity" {
					t.Errorf("%s: %s city leg claim view = (%p, %q), want the rig store (%p, %q)", list, target.template, target.claimView, target.claimViewKey, rigStore, "rig:gascity")
				}
				crossStore[list+" "+target.template]++
			case target.claimView != nil || target.claimViewKey != "":
				t.Errorf("%s: %s leg %q carries claim view (%p, %q); only a rig pool's cross-store legs do", list, target.template, target.storeKey, target.claimView, target.claimViewKey)
			}
		}
	}
	check("defaultScaleTargets", targets.defaultScaleTargets)
	check("defaultNamedScaleTargets", targets.defaultNamedScaleTargets)
	for _, want := range []string{"defaultScaleTargets gascity/worker", "defaultScaleTargets gascity/named", "defaultNamedScaleTargets gascity/named"} {
		if crossStore[want] == 0 {
			t.Errorf("no city leg in %s; the fixture must exercise the cross-store probe (got %v)", want, crossStore)
		}
	}

	own := defaultScaleCheckTarget{template: "gascity/worker", store: rigStore, storeKey: "rig:gascity"}
	binding := crossStoreDemandLegs([]defaultScaleCheckTarget{{template: "gascity/worker", store: cityStore, storeKey: "city", classBinding: true}}, own)
	if binding[0].claimView != nil || binding[0].claimViewKey != "" {
		t.Errorf("class-binding leg carries claim view (%p, %q); its rows are resolved in the binding", binding[0].claimView, binding[0].claimViewKey)
	}
}
