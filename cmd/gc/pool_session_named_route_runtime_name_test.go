package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// namedRouteIdentity is the qualified identity of the fixture's named session.
// A binding makes it "seth.seth", which sanitizes to the distinct runtime name
// "seth__seth" — the maintainer-city shape where the two forms diverge. An
// unbound, separator-free identity would sanitize to itself and could not
// exercise the bug at all.
const (
	namedRouteIdentity = "seth.seth"
	namedRouteTemplate = "seth.seth"
)

// namedRouteRuntimeNameFixture builds a city with one bound on-demand named
// session whose runtime session name differs from its qualified identity.
func namedRouteRuntimeNameFixture(t *testing.T) *config.City {
	t.Helper()
	return &config.City{
		Agents: []config.Agent{{
			Name:              "seth",
			BindingName:       "seth",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
		}},
		NamedSessions: []config.NamedSession{{
			Template:    "seth",
			BindingName: "seth",
			Mode:        "on_demand",
		}},
		ResolvedWorkspaceName: "test-city",
	}
}

// namedRouteRuntimeName returns the fixture's tmux-safe runtime session name,
// asserting it actually differs from the qualified identity so none of the
// tests below can pass vacuously.
func namedRouteRuntimeName(t *testing.T, cfg *config.City) string {
	t.Helper()
	runtimeName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, namedRouteIdentity)
	if runtimeName == namedRouteIdentity {
		t.Fatalf("fixture is vacuous: runtime name %q equals the qualified identity, so the "+
			"runtime-form blindness cannot be observed", runtimeName)
	}
	return runtimeName
}

// A configured named session claims work under its tmux-safe RUNTIME name, not
// its qualified identity — that is what GC_SESSION_NAME carries into the hook
// (ga-e70d2). assigneePreservesNamedSessionRoute resolved the assignee with
// findNamedSessionSpec, which matches only the qualified identity, so the
// runtime form resolved to no spec at all and the named-route guard was inert
// for the one claim form that actually occurs.
//
// That matters when the session bead is gone: with the bead closed,
// openSessionOwnsWork and liveOpenSessionAssignmentExists both answer false, so
// this guard is the only thing keeping a configured named session's claim (and
// its continuation-group metadata) from being torn down and handed to a backup
// worker. A named session is reconstitutable from config, so its route must
// survive session-bead churn.
func TestAssigneePreservesNamedSessionRouteAcceptsRuntimeName(t *testing.T) {
	cfg := namedRouteRuntimeNameFixture(t)
	runtimeName := namedRouteRuntimeName(t, cfg)

	if !assigneePreservesNamedSessionRoute(cfg, t.TempDir(), namedRouteTemplate, runtimeName, "", false) {
		t.Fatalf("runtime-form assignee %q did not preserve the named route for identity %q — "+
			"the resolver only recognizes the qualified form, so a live named session's claim is "+
			"released and re-minted on a backup worker", runtimeName, namedRouteIdentity)
	}
}

// Control for the test above: the qualified form must keep working. If the fix
// had swapped one accepted form for the other rather than accepting both, this
// fails.
func TestAssigneePreservesNamedSessionRouteStillAcceptsQualifiedIdentity(t *testing.T) {
	cfg := namedRouteRuntimeNameFixture(t)
	if !assigneePreservesNamedSessionRoute(cfg, t.TempDir(), namedRouteTemplate, namedRouteIdentity, "", false) {
		t.Fatalf("qualified identity %q must still preserve the named route", namedRouteIdentity)
	}
}

// Second control: accepting the runtime form must not turn the resolver into a
// prefix or fuzzy match. Names that merely resemble the identity, and names no
// session owns, must still release.
//
// The V2 bare-leaf form ("seth") is deliberately absent from this list: it
// already resolves through config.FindNamedSession and is pre-existing accepted
// behavior, not something the runtime-form fix introduces.
func TestAssigneePreservesNamedSessionRouteRejectsNonClaimForms(t *testing.T) {
	cfg := namedRouteRuntimeNameFixture(t)
	for _, assignee := range []string{"seth.seth-extra", "seth__seth-extra", "", "gc-1", "seth.other"} {
		if assigneePreservesNamedSessionRoute(cfg, t.TempDir(), namedRouteTemplate, assignee, "", false) {
			t.Errorf("assignee %q must not preserve the named route: no configured named session "+
				"claims work under that name", assignee)
		}
	}
}

// The runtime form must also carry the store-ref federation check, not bypass
// it. A rig-scoped named agent whose work lives under a different store-ref is
// genuinely unreachable, so release stays correct for both assignee forms.
func TestAssigneePreservesNamedSessionRouteRuntimeNameHonorsStoreRef(t *testing.T) {
	cfg := namedRouteRuntimeNameFixture(t)
	runtimeName := namedRouteRuntimeName(t, cfg)
	if assigneePreservesNamedSessionRoute(cfg, t.TempDir(), namedRouteTemplate, runtimeName, "unreachable-rig", true) {
		t.Fatalf("runtime-form assignee %q must still fail the store-ref check for a store the "+
			"named agent cannot reach — the fix widens which assignee forms resolve, not which "+
			"stores federate", runtimeName)
	}
}
