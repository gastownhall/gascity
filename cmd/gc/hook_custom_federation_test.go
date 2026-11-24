package main

import (
	"fmt"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
)

func TestCustomFederatedWorkQueryUsesOneStore(t *testing.T) {
	forEachTopologyWithRig(t, func(t *testing.T, e splitEnv) {
		for _, tc := range []struct {
			name, command string
			federated     bool
			one           bool
		}{
			{"declared_federated", "custom-reader --all-stores", true, true},
			{"legacy_custom", "custom-reader --all-stores", false, false},
			{"generated_query", "", true, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a := config.Agent{Scope: "city"}
				if _, err := toml.Decode(fmt.Sprintf("name = %q\nwork_query = %q\nwork_query_federated = %t\n", splitEnvPoolAgent, tc.command, tc.federated), &a); err != nil {
					t.Fatal(err)
				}
				env, err := hookQueryEnv(e.cityPath, e.cfg, &a)
				if err != nil {
					t.Fatal(err)
				}
				dir := agentCommandDir(e.cityPath, &a, e.cfg.Rigs)
				stores := hookWorkQueryStores(e.cityPath, e.cfg, &a, a.QualifiedName(), dir, mergeRuntimeEnv(nil, env), env, nil)
				want := 1 + len(e.cfg.Rigs)
				if tc.one {
					want = 1
				}
				if len(stores) != want {
					t.Fatalf("work-query legs = %d, want %d", len(stores), want)
				}
				if stores[0].dir != dir {
					t.Fatalf("primary query directory changed: %q != %q", stores[0].dir, dir)
				}
			})
		}
	})
}
