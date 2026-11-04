package agentutil

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestRigRouteAdmitted(t *testing.T) {
	beta := &config.Rig{Name: "beta", Federation: config.RigFederationRig}
	cases := map[string]bool{
		"beta/crew.worker":             true,
		"beta--crew__worker":           true,
		"beta/core.control-dispatcher": true,
		"crew.worker":                  false,
		"alpha/crew.worker":            false,
		"Beta/crew.worker":             false,
		"beta/":                        false,
		"beta":                         false,
		"/crew":                        false,
		"beta//x":                      false,
		"":                             false,
		"beta/crew.worker-gcx-session-0123456789abcdef0123456789abcdef": false,
	}
	for v, want := range cases {
		if got := RigRouteAdmitted(beta, v); got != want {
			t.Errorf("RigRouteAdmitted(%q) = %v, want %v", v, got, want)
		}
	}
	if !RigRouteAdmitted(beta, "\tbeta/crew.worker\n") {
		t.Error("surrounding space must be trimmed")
	}
	if !RigRouteAdmitted(nil, "anything") {
		t.Error("a nil rig (unguarded) must admit everything")
	}
	deny := &config.Rig{Federation: config.RigFederationRig}
	if RigRouteAdmitted(deny, "beta/crew.worker") {
		t.Error("a nameless guarded rig must admit nothing")
	}
}

func TestGuardedRigLookups(t *testing.T) {
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "beta", Prefix: "bt", Federation: config.RigFederationRig},
		{Name: "alpha", Prefix: "al"},
		{Name: "gamma", Prefix: "zz", Federation: config.RigFederationRig},
		{Name: "delta", Prefix: "ZZ", Federation: config.RigFederationRig},
	}}
	if r := GuardedRigForStoreRef(cfg, "rig:beta"); r == nil || r.Name != "beta" {
		t.Fatalf("rig:beta = %+v", r)
	}
	for _, ref := range []string{"rig:alpha", "rig:Beta", "city:x", "", "rig:"} {
		if r := GuardedRigForStoreRef(cfg, ref); r != nil {
			t.Errorf("%q = %+v, want nil", ref, r)
		}
	}
	if r := GuardedRigForPrefix(cfg, "BT"); r == nil || r.Name != "beta" {
		t.Fatalf("prefix BT = %+v", r)
	}
	if r := GuardedRigForPrefix(cfg, "al"); r != nil {
		t.Fatalf("unguarded prefix = %+v", r)
	}
	r := GuardedRigForPrefix(cfg, "zz")
	if r == nil || r.Name != "" || RigRouteAdmitted(r, "gamma/x") {
		t.Fatalf("ambiguous prefix = %+v, want a deny-all rig", r)
	}
}

func TestRigRouteRefusalMessage(t *testing.T) {
	beta := &config.Rig{Name: "beta", Federation: config.RigFederationRig}
	err := RigRouteRefusal(beta, "", "mayor")
	if !errors.Is(err, ErrRigNamespaceRoute) || !errors.Is(err, config.ErrRigNamespace) {
		t.Fatalf("err = %v, want it to wrap ErrRigNamespaceRoute and config.ErrRigNamespace", err)
	}
	want := `refusing route into rig "beta": target "mayor" is outside its namespace "beta/" (federation = "rig"); route to one of the rig's own agents. --force does not override this.`
	if err.Error() != want {
		t.Fatalf("message = %q\nwant      %q", err.Error(), want)
	}
	if err := RigRouteRefusal(beta, "gc.routed_to", "  "); err != nil {
		t.Fatalf("empty value refused: %v", err)
	}
	if err := RigRouteRefusal(beta, "gc.routed_to", "crew"); err == nil || !strings.Contains(err.Error(), "(gc.routed_to)") {
		t.Fatalf("keyed refusal = %v", err)
	}
}
