package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// requalifiedCity is the requalification shape of gastownhall/gascity#5957:
// a pre-adopt top-level named session `golden-hon` whose config now holds
// only contentbuild/lane.golden-hon, the entry `gc rig add --adopt`
// requalified it to.
func requalifiedCity() *config.City {
	return &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "golden-hon", Dir: "contentbuild", BindingName: "lane"}},
		NamedSessions: []config.NamedSession{{Template: "golden-hon", Dir: "contentbuild", BindingName: "lane"}},
	}
}

// requalifiedSpec is requalifiedCity's spec for the qualified identity,
// requiring bare as its BareIdentity.
func requalifiedSpec(t *testing.T, bare string) NamedSessionSpec {
	t.Helper()
	spec, ok := FindNamedSessionSpec(requalifiedCity(), "test-city", "contentbuild/lane.golden-hon")
	if !ok {
		t.Fatal("no spec for contentbuild/lane.golden-hon")
	}
	if spec.BareIdentity != bare {
		t.Fatalf("spec.BareIdentity = %q, want %q", spec.BareIdentity, bare)
	}
	return spec
}

// preAdoptRow is the live row the pre-adopt config minted: session_name,
// configured_named_identity and alias all `golden-hon`, started, with the
// stale baseline a config-drift compare would trip over.
func preAdoptRow(id string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Type:   BeadType,
		Status: "open",
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			NamedSessionMetadataKey:      "true",
			NamedSessionIdentityMetadata: "golden-hon",
			NamedSessionModeMetadata:     "always",
			"session_name":               "golden-hon",
			"alias":                      "golden-hon",
			"state":                      "active",
			"started_config_hash":        "stale",
			"continuity_eligible":        "true",
		},
	}
}

func TestFindCanonicalNamedSessionInfoResolvesThePreRequalificationBareRow(t *testing.T) {
	spec := requalifiedSpec(t, "golden-hon")
	row := preAdoptRow("ga-hon")
	info, ok := FindCanonicalNamedSessionInfo([]Info{infoFromPersistedBead(row)}, spec)
	if !ok || info.ID != "ga-hon" {
		t.Fatalf("FindCanonicalNamedSessionInfo = (%q, %v), want the pre-adopt bare row", info.ID, ok)
	}
	// An exact row still wins over the bare one.
	exact := preAdoptRow("ga-qualified")
	exact.Metadata = map[string]string{
		NamedSessionMetadataKey:      "true",
		NamedSessionIdentityMetadata: "contentbuild/lane.golden-hon",
		NamedSessionModeMetadata:     "always",
		"session_name":               "contentbuild--lane__golden-hon",
		"state":                      "active",
	}
	got, ok := FindCanonicalNamedSessionInfo([]Info{infoFromPersistedBead(row), infoFromPersistedBead(exact)}, spec)
	if !ok || got.ID != "ga-qualified" {
		t.Fatalf("FindCanonicalNamedSessionInfo = (%q, %v), want the exact row to beat the bare one", got.ID, ok)
	}
}

func TestFindCanonicalNamedSessionInfoBarePassIsUniqueAndEligibleOnly(t *testing.T) {
	spec := requalifiedSpec(t, "golden-hon")
	second := preAdoptRow("ga-hon-2")
	if _, ok := FindCanonicalNamedSessionInfo([]Info{infoFromPersistedBead(preAdoptRow("ga-hon")), infoFromPersistedBead(second)}, spec); ok {
		t.Fatal("two bare rows: want none canonical")
	}
	closed := preAdoptRow("ga-closed")
	closed.Status = "closed"
	if _, ok := FindCanonicalNamedSessionInfo([]Info{infoFromPersistedBead(closed)}, spec); ok {
		t.Fatal("closed bare row: want none canonical")
	}
	continuityOff := preAdoptRow("ga-continuity")
	continuityOff.Metadata["continuity_eligible"] = "false"
	if _, ok := FindCanonicalNamedSessionInfo([]Info{infoFromPersistedBead(continuityOff)}, spec); ok {
		t.Fatal("continuity-ineligible bare row: want none canonical")
	}
	// A bare row of a DIFFERENT named identity does not resolve.
	other := preAdoptRow("ga-other")
	other.Metadata[NamedSessionIdentityMetadata] = "some-other"
	if _, ok := FindCanonicalNamedSessionInfo([]Info{infoFromPersistedBead(other)}, spec); ok {
		t.Fatal("a bare row of another identity: want none canonical")
	}
}

func TestNamedSessionSpecBareIdentityGuards(t *testing.T) {
	// An ambiguous leaf (two bindings share golden-hon) leaves BareIdentity
	// empty on both entries, so the resolvers take no bare pass at all.
	ambiguous := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "golden-hon", Dir: "contentbuild", BindingName: "lane"},
			{Name: "golden-hon", Dir: "contentbuild", BindingName: "ops"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "golden-hon", Dir: "contentbuild", BindingName: "lane"},
			{Template: "golden-hon", Dir: "contentbuild", BindingName: "ops"},
		},
	}
	for _, identity := range []string{"contentbuild/lane.golden-hon", "contentbuild/ops.golden-hon"} {
		spec, ok := FindNamedSessionSpec(ambiguous, "test-city", identity)
		if !ok || spec.BareIdentity != "" {
			t.Fatalf("%s: spec = (%v, bare %q), want BareIdentity empty", identity, ok, spec.BareIdentity)
		}
	}
	// A spec looked up under the bare identity echoes it and carries no
	// BareIdentity: the exact pass already resolves rows of that identity.
	spec, ok := FindNamedSessionSpec(requalifiedCity(), "test-city", "golden-hon")
	if !ok || spec.Identity != "golden-hon" || spec.BareIdentity != "" {
		t.Fatalf("bare lookup: spec = (%v, identity %q, bare %q), want the identity echoed and no bare form", ok, spec.Identity, spec.BareIdentity)
	}
	// A top-level named session has no bare form either.
	top := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "mayor"}},
		NamedSessions: []config.NamedSession{{Template: "mayor"}},
	}
	spec, ok = FindNamedSessionSpec(top, "test-city", "mayor")
	if !ok || spec.BareIdentity != "" {
		t.Fatalf("top-level spec = (%v, bare %q), want no bare form", ok, spec.BareIdentity)
	}
}
