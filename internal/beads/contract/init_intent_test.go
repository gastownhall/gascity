package contract

import "testing"

func TestResolveInitIntentPrecedence(t *testing.T) {
	def := InitIntent{Transport: "proxied", Target: "local"}
	cases := []struct {
		name          string
		cli, env, cfg InitIntent
		want          InitIntent
		source        string
	}{
		{"provider default", InitIntent{}, InitIntent{}, InitIntent{}, def, "provider-default"},
		{"city config", InitIntent{}, InitIntent{}, InitIntent{Transport: "direct", Target: "local"}, InitIntent{Transport: "direct", Target: "local"}, "city-config"},
		{"environment outranks config", InitIntent{}, InitIntent{Transport: "direct", Target: "external"}, InitIntent{Transport: "direct", Target: "local"}, InitIntent{Transport: "direct", Target: "external"}, "environment"},
		{"cli outranks environment", InitIntent{Transport: "proxied", Target: "local"}, InitIntent{Transport: "direct", Target: "external"}, InitIntent{}, InitIntent{Transport: "proxied", Target: "local"}, "cli"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveInitIntent(InitScopeState{}, tc.cli, tc.env, tc.cfg, def)
			if err != nil {
				t.Fatal(err)
			}
			if got.Intent != tc.want || got.Source != tc.source {
				t.Fatalf("got %+v, want intent=%+v source=%s", got, tc.want, tc.source)
			}
		})
	}
}

func TestResolveInitIntentRejectsPartialAndUnsupported(t *testing.T) {
	for _, tc := range []InitIntent{{Transport: "proxied"}, {Target: "local"}, {Transport: "proxy", Target: "local"}, {Transport: "direct", Target: "remote"}} {
		if _, err := ResolveInitIntent(InitScopeState{}, tc, InitIntent{}, InitIntent{}, InitIntent{Transport: "proxied", Target: "local"}); err == nil {
			t.Fatalf("ResolveInitIntent(%+v) succeeded, want error", tc)
		}
	}
}

func TestResolveInitIntentPersistedStateWinsAndConflicts(t *testing.T) {
	persisted := InitScopeState{Initialized: true, Backend: "dolt", DoltMode: "proxied-server", Target: "local"}
	got, err := ResolveInitIntent(persisted, InitIntent{}, InitIntent{}, InitIntent{Transport: "proxied", Target: "local"}, InitIntent{Transport: "direct", Target: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Intent != (InitIntent{Transport: "proxied", Target: "local"}) || got.Source != "persisted" {
		t.Fatalf("got %+v", got)
	}
	if _, err := ResolveInitIntent(persisted, InitIntent{Transport: "direct", Target: "local"}, InitIntent{}, InitIntent{}, InitIntent{}); err == nil {
		t.Fatal("conflicting persisted intent succeeded")
	}
}

func TestResolveInitIntentPreservesPersistedDirectExternal(t *testing.T) {
	persisted := InitScopeState{Initialized: true, Backend: "dolt", DoltMode: "server", Target: "external"}
	got, err := ResolveInitIntent(persisted, InitIntent{}, InitIntent{}, InitIntent{Transport: "direct", Target: "external"}, InitIntent{Transport: "proxied", Target: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Intent != (InitIntent{Transport: "direct", Target: "external"}) || got.Source != "persisted" {
		t.Fatalf("got %+v, want persisted direct/external", got)
	}
}

func TestResolveInitIntentTreatsMissingPersistedDoltModeAsDirectLocal(t *testing.T) {
	persisted := InitScopeState{Initialized: true, Backend: "dolt", Target: "local"}
	got, err := ResolveInitIntent(persisted, InitIntent{}, InitIntent{}, InitIntent{}, InitIntent{Transport: "proxied", Target: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Intent != (InitIntent{Transport: "direct", Target: "local"}) || got.Source != "persisted" {
		t.Fatalf("got %+v, want persisted direct/local", got)
	}
}

func TestResolveInitIntentRejectsUnknownPersistedDoltMode(t *testing.T) {
	for _, mode := range []string{"embedded", "mystery", "DIRECT"} {
		t.Run(mode, func(t *testing.T) {
			_, err := ResolveInitIntent(InitScopeState{Initialized: true, Backend: "dolt", DoltMode: mode, Target: "local"}, InitIntent{}, InitIntent{}, InitIntent{}, InitIntent{Transport: "proxied", Target: "local"})
			if err == nil {
				t.Fatalf("ResolveInitIntent accepted unknown persisted dolt mode %q", mode)
			}
		})
	}
}

func TestResolveInitIntentDoesNotReadAmbientEnvironment(t *testing.T) {
	// The resolver accepts an explicit policy environment value only. An
	// ambient BEADS_DOLT_* variable is intentionally invisible here, so it
	// cannot select transport or target for a fresh scope.
	got, err := ResolveInitIntent(InitScopeState{}, InitIntent{}, InitIntent{}, InitIntent{}, InitIntent{Transport: "proxied", Target: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "provider-default" || got.Intent.Transport != "proxied" {
		t.Fatalf("got %+v, want provider default", got)
	}
}

func TestResolveInitIntentPreservesNonDoltBackend(t *testing.T) {
	got, err := ResolveInitIntent(InitScopeState{Initialized: true, Backend: "doltlite", DoltMode: "embedded"}, InitIntent{}, InitIntent{}, InitIntent{}, InitIntent{Transport: "proxied", Target: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.PreserveBackend || got.Intent != (InitIntent{}) {
		t.Fatalf("got %+v, want preserve", got)
	}
	if _, err := ResolveInitIntent(InitScopeState{Initialized: true, Backend: "doltlite"}, InitIntent{Transport: "direct", Target: "local"}, InitIntent{}, InitIntent{}, InitIntent{}); err == nil {
		t.Fatal("selector changed initialized DoltLite backend")
	}
}
