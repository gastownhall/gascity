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
// whose BareIdentity is the pre-adopt bare leaf golden-hon.
func requalifiedSpec(t *testing.T) NamedSessionSpec {
	t.Helper()
	spec, ok := FindNamedSessionSpec(requalifiedCity(), "test-city", "contentbuild/lane.golden-hon")
	if !ok {
		t.Fatal("no spec for contentbuild/lane.golden-hon")
	}
	if spec.BareIdentity != "golden-hon" {
		t.Fatalf("spec.BareIdentity = %q, want %q", spec.BareIdentity, "golden-hon")
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
	spec := requalifiedSpec(t)
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
	spec := requalifiedSpec(t)
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

// requalifiedExactRow is the row a config that was already qualified mints:
// qualified identity everywhere, the canonical runtime name underneath.
func requalifiedExactRow(id string) beads.Bead {
	b := preAdoptRow(id)
	b.Metadata = map[string]string{
		NamedSessionMetadataKey:      "true",
		NamedSessionIdentityMetadata: "contentbuild/lane.golden-hon",
		NamedSessionModeMetadata:     "always",
		"session_name":               "contentbuild--lane__golden-hon",
		"alias":                      "contentbuild/lane.golden-hon",
		"state":                      "active",
		"continuity_eligible":        "true",
	}
	return b
}

// seedBareRow stores a pre-adopt bare row in a real store.
func seedBareRow(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	row := preAdoptRow("ga-unassigned")
	b, err := store.Create(beads.Bead{
		Type:     row.Type,
		Labels:   row.Labels,
		Metadata: row.Metadata,
	})
	if err != nil {
		t.Fatalf("seeding the bare row: %v", err)
	}
	return b
}

// The bead resolver picks up the pre-requalification bare row the same way
// the Info resolver does: uniquely, open, continuity-eligible, exact first.
func TestFindCanonicalNamedSessionBeadResolvesThePreRequalificationBareRow(t *testing.T) {
	spec := requalifiedSpec(t)
	row := preAdoptRow("ga-hon")
	bead, ok := FindCanonicalNamedSessionBead([]beads.Bead{row}, spec)
	if !ok || bead.ID != "ga-hon" {
		t.Fatalf("FindCanonicalNamedSessionBead = (%q, %v), want the bare row", bead.ID, ok)
	}
	exact := requalifiedExactRow("ga-qualified")
	got, ok := FindCanonicalNamedSessionBead([]beads.Bead{row, exact}, spec)
	if !ok || got.ID != "ga-qualified" {
		t.Fatalf("FindCanonicalNamedSessionBead = (%q, %v), want the exact row to beat the bare one", got.ID, ok)
	}
	if _, ok := FindCanonicalNamedSessionBead([]beads.Bead{row, preAdoptRow("ga-hon-2")}, spec); ok {
		t.Fatal("two bare rows: want none canonical")
	}
	continuityOff := preAdoptRow("ga-continuity")
	continuityOff.Metadata["continuity_eligible"] = "false"
	if _, ok := FindCanonicalNamedSessionBead([]beads.Bead{continuityOff}, spec); ok {
		t.Fatal("continuity-ineligible bare row: want none canonical")
	}
	other := preAdoptRow("ga-other")
	other.Metadata[NamedSessionIdentityMetadata] = "some-other"
	if _, ok := FindCanonicalNamedSessionBead([]beads.Bead{other}, spec); ok {
		t.Fatal("a bare row of another identity: want none canonical")
	}
}

// The store-backed lookup path — what interactive session targeting uses —
// finds the bare row through a configured_named_identity query for the
// prior form, without mistaking two bare rows for a conflict.
func TestLookupConfiguredNamedSessionResolvesThePreRequalificationBareRow(t *testing.T) {
	store := beads.NewMemStore()
	row := seedBareRow(t, store)
	spec := requalifiedSpec(t)

	lookup, err := LookupConfiguredNamedSession(store, spec)
	if err != nil {
		t.Fatalf("LookupConfiguredNamedSession: %v", err)
	}
	if !lookup.HasCanonical || lookup.Canonical.ID != row.ID {
		t.Fatalf("lookup = (canonical %q, has %v, conflict %q, hasConf %v), want the bare row %q", lookup.Canonical.ID, lookup.HasCanonical, lookup.Conflict.ID, lookup.HasConflict, row.ID)
	}
	bead, ok, err := FindCanonicalConfiguredNamedSessionBead(store, spec)
	if err != nil {
		t.Fatalf("FindCanonicalConfiguredNamedSessionBead: %v", err)
	}
	if !ok || bead.ID != row.ID {
		t.Fatalf("FindCanonicalConfiguredNamedSessionBead = (%q, %v), want the bare row %q", bead.ID, ok, row.ID)
	}

	// Two bare rows are ambiguous, not canonical — and they are not a
	// foreign conflict either: no row claims the qualified identity.
	twoBare := beads.NewMemStore()
	seedBareRow(t, twoBare)
	seedBareRow(t, twoBare)
	lookup, err = LookupConfiguredNamedSession(twoBare, spec)
	if err != nil {
		t.Fatalf("LookupConfiguredNamedSession(twoBare): %v", err)
	}
	if lookup.HasCanonical || lookup.HasConflict {
		t.Fatalf("two bare rows: lookup = (canonical %q, has %v, conflict %q, hasConf %v), want none", lookup.Canonical.ID, lookup.HasCanonical, lookup.Conflict.ID, lookup.HasConflict)
	}
}

// The label-scoped candidate scan (the reconcilers' path) reaches the bare
// row for a qualified spec, and drops it when the config's leaf is
// ambiguous (BareIdentity empty).
func TestNamedSessionResolutionCandidatesAdmitThePreRequalificationBareRow(t *testing.T) {
	store := beads.NewMemStore()
	row := seedBareRow(t, store)
	spec := requalifiedSpec(t)
	got, err := NamedSessionResolutionCandidates(store, spec)
	if err != nil {
		t.Fatalf("NamedSessionResolutionCandidates: %v", err)
	}
	found := false
	for _, b := range got {
		if b.ID == row.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("the bare row %q is missing from the %d candidates for the qualified spec", row.ID, len(got))
	}

	// An ambiguous leaf disables the bare admission: the candidate scan runs
	// on each binding's own spec, and neither carries a BareIdentity.
	bare := seedBareRow(t, store)
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
		aspec, ok := FindNamedSessionSpec(ambiguous, "test-city", identity)
		if !ok || aspec.BareIdentity != "" {
			t.Fatalf("%s: spec = (%v, bare %q), want BareIdentity empty", identity, ok, aspec.BareIdentity)
		}
		candidates, err := NamedSessionResolutionCandidates(store, aspec)
		if err != nil {
			t.Fatalf("NamedSessionResolutionCandidates(%s): %v", identity, err)
		}
		for _, b := range candidates {
			if b.ID == bare.ID {
				t.Errorf("%s: the ambiguously-claimed bare row %q was admitted", identity, bare.ID)
			}
		}
	}
}
