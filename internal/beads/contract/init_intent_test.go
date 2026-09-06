package contract

import "testing"

func TestResolveInitIntentPrecedenceForFreshScope(t *testing.T) {
	providerDefault := InitIntent{Transport: "proxied", Target: "local"}
	for _, tc := range []struct {
		name                     string
		cli, environment, policy InitIntent
		want                     InitIntent
		source                   string
	}{
		{"provider default", InitIntent{}, InitIntent{}, InitIntent{}, providerDefault, "provider-default"},
		{"policy", InitIntent{}, InitIntent{}, InitIntent{Transport: "direct", Target: "local"}, InitIntent{Transport: "direct", Target: "local"}, "city-config"},
		{"explicit policy environment", InitIntent{}, InitIntent{Transport: "direct", Target: "external"}, InitIntent{Transport: "direct", Target: "local"}, InitIntent{Transport: "direct", Target: "external"}, "environment"},
		{"cli", InitIntent{Transport: "proxied", Target: "local"}, InitIntent{Transport: "direct", Target: "external"}, InitIntent{}, InitIntent{Transport: "proxied", Target: "local"}, "cli"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveInitIntent(InitScopeState{}, tc.cli, tc.environment, tc.policy, providerDefault)
			if err != nil {
				t.Fatal(err)
			}
			if got.Intent != tc.want || got.Source != tc.source {
				t.Fatalf("got %+v, want %s %+v", got, tc.source, tc.want)
			}
		})
	}
}

func TestResolveInitIntentPersistedStateIsAuthoritative(t *testing.T) {
	persisted := InitScopeState{Initialized: true, Backend: "dolt", DoltMode: "proxied-server", Target: "local"}
	got, err := ResolveInitIntent(persisted, InitIntent{}, InitIntent{}, InitIntent{Transport: "proxied", Target: "local"}, InitIntent{Transport: "direct", Target: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Intent != (InitIntent{Transport: "proxied", Target: "local"}) || got.Source != "persisted" {
		t.Fatalf("got %+v", got)
	}
	if _, err := ResolveInitIntent(persisted, InitIntent{Transport: "direct", Target: "local"}, InitIntent{}, InitIntent{}, InitIntent{}); err == nil {
		t.Fatal("conflicting CLI selector succeeded")
	}
	if _, err := ResolveInitIntent(persisted, InitIntent{}, InitIntent{}, InitIntent{Transport: "direct", Target: "local"}, InitIntent{}); err == nil {
		t.Fatal("conflicting city policy succeeded")
	}
}

func TestResolveInitIntentPreservesInitializedNonDoltBackend(t *testing.T) {
	state := InitScopeState{Initialized: true, Backend: "doltlite", DoltMode: "embedded"}
	got, err := ResolveInitIntent(state, InitIntent{}, InitIntent{}, InitIntent{}, InitIntent{Transport: "proxied", Target: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.PreserveBackend || got.Source != "persisted-backend" {
		t.Fatalf("got %+v", got)
	}
	if _, err := ResolveInitIntent(state, InitIntent{Transport: "direct", Target: "local"}, InitIntent{}, InitIntent{}, InitIntent{}); err == nil {
		t.Fatal("selector changed initialized DoltLite backend")
	}
}

func TestResolveInitIntentRejectsPartialAndAmbientSelectors(t *testing.T) {
	for _, intent := range []InitIntent{{Transport: "proxied"}, {Target: "local"}, {Transport: "proxy", Target: "local"}, {Transport: "direct", Target: "remote"}} {
		if _, err := ResolveInitIntent(InitScopeState{}, intent, InitIntent{}, InitIntent{}, InitIntent{Transport: "proxied", Target: "local"}); err == nil {
			t.Fatalf("ResolveInitIntent(%+v) succeeded", intent)
		}
	}
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient.invalid")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "3307")
	// The resolver receives only a policy env intent; it does not inspect the
	// process environment, so ambient BEADS_DOLT_* cannot select topology.
	got, err := ResolveInitIntent(InitScopeState{}, InitIntent{}, InitIntent{}, InitIntent{}, InitIntent{Transport: "proxied", Target: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "provider-default" {
		t.Fatalf("got %+v, want provider default", got)
	}
}

func TestResolveInitIntentRejectsUnknownPersistedMode(t *testing.T) {
	_, err := ResolveInitIntent(
		InitScopeState{Initialized: true, Backend: "dolt", DoltMode: "mystery-mode", Target: "local"},
		InitIntent{}, InitIntent{}, InitIntent{}, InitIntent{Transport: "proxied", Target: "local"},
	)
	if err == nil {
		t.Fatal("unknown persisted dolt mode was accepted")
	}
}
