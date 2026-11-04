package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

// orderEnvLookup returns the value of key in an order exec env and whether
// it is present.
func orderEnvLookup(env []string, key string) (string, bool) {
	value, present := "", false
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			value, present = v, true
		}
	}
	return value, present
}

// TestOrderExecEnvExportsGuardedRigs pins GC_GUARDED_RIGS, which the core
// orders read to skip `gc rig list` in a city with no guarded rig: it is
// always set from the loaded config, empty when no rig has federation =
// "rig", the guarded names (comma-separated, in declaration order) otherwise,
// and neither a dispatch var nor an inherited value can change it. Without a
// config it is left unset, which the orders read as "may guard".
func TestOrderExecEnvExportsGuardedRigs(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv(orders.GuardedRigsEnv, "inherited")

	cityDir := t.TempDir()
	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "ct"}
	a := orders.Order{Name: "nudge-on-route", Trigger: "event", Exec: "true"}

	for _, tc := range []struct {
		name string
		rigs []config.Rig
		vars map[string]string
		want string
	}{
		{"no rigs", nil, nil, ""},
		{"no guarded rig", []config.Rig{{Name: "alpha"}, {Name: "beta", Federation: "city"}}, nil, ""},
		{"guarded rigs", []config.Rig{{Name: "alpha"}, {Name: "beta", Federation: "rig"}, {Name: "gamma", Federation: " Rig "}}, nil, "beta,gamma"},
		{"dispatch var cannot clear it", []config.Rig{{Name: "beta", Federation: "rig"}}, map[string]string{orders.GuardedRigsEnv: ""}, "beta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, err := orderExecEnvWithError(cityDir, &config.City{Rigs: tc.rigs}, target, a, tc.vars)
			if err != nil {
				t.Fatalf("orderExecEnvWithError: %v", err)
			}
			got, present := orderEnvLookup(env, orders.GuardedRigsEnv)
			if !present || got != tc.want {
				t.Fatalf("%s = %q (present %v), want %q set", orders.GuardedRigsEnv, got, present, tc.want)
			}
			if merged, _ := orderEnvLookup(mergeOrderExecEnv([]string{orders.GuardedRigsEnv + "=inherited"}, env), orders.GuardedRigsEnv); merged != tc.want {
				t.Fatalf("after merging the controller env, %s = %q, want %q", orders.GuardedRigsEnv, merged, tc.want)
			}
		})
	}

	env, err := orderExecEnvWithError(cityDir, nil, target, a, nil)
	if err != nil {
		t.Fatalf("orderExecEnvWithError(nil config): %v", err)
	}
	if got, present := orderEnvLookup(mergeOrderExecEnv([]string{orders.GuardedRigsEnv + "="}, env), orders.GuardedRigsEnv); present {
		t.Fatalf("%s = %q without a config, want unset (an inherited value dropped)", orders.GuardedRigsEnv, got)
	}

	reserved := orders.Order{Name: "o", Trigger: "manual", Exec: "true", Env: map[string]string{orders.GuardedRigsEnv: ""}}
	if err := orders.ValidateExecEnvOverrides(reserved); err == nil {
		t.Fatalf("[order.env] may override %s, want it reserved", orders.GuardedRigsEnv)
	}
}
