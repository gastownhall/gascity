package session

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The identifier-resolution methods on Store are equivalence-tested against the
// package-level functions they carry, not against hand-written expectations.
// That is deliberate: their whole job is to make the SAME resolution reachable
// through the typed front door for consumers that hold no bead store, so any
// difference between the two forms is the defect. A test that asserted its own
// notion of the right answer would keep passing while the two drifted apart.

func resolveFixtureStore(t *testing.T) (*Store, beads.Store) {
	t.Helper()
	seed := []beads.Bead{
		sessionBeadFixture("s-open-1", "open", map[string]string{
			"session_name": "mayor",
			"alias":        "demo/mayor",
			"template":     "mayor",
		}),
		sessionBeadFixture("s-closed-1", "closed", map[string]string{
			"session_name": "retired",
			"alias":        "demo/retired",
			"template":     "worker",
		}),
		sessionBeadFixture("s-open-2", "open", map[string]string{
			"session_name": "worker-a",
			"alias":        "rig/worker-a",
			"template":     "worker",
		}),
	}
	mem := beads.NewMemStoreFrom(len(seed), seed, nil)
	return NewStore(beads.SessionStore{Store: mem}), mem
}

// TestStoreResolveMethodsMatchTheirPackageFunctions is the equivalence oracle
// for the front door's identifier resolvers. Each method must answer exactly
// what the package function answers for the store behind the door — same id,
// same error — across the direct-id, live-identifier, closed-fallback and
// absent lanes. The closed lane is what separates ResolveID from
// ResolveIDAllowClosed, so a method wired to the wrong variant fails here.
func TestStoreResolveMethodsMatchTheirPackageFunctions(t *testing.T) {
	front, store := resolveFixtureStore(t)

	identifiers := []string{"s-open-1", "mayor", "demo/mayor", "retired", "demo/retired", "worker-a", "absent", ""}
	for _, identifier := range identifiers {
		t.Run("exact-id/"+identifier, func(t *testing.T) {
			gotID, gotErr := front.ResolveIDByExactID(identifier)
			wantID, wantErr := ResolveSessionIDByExactID(store, identifier)
			assertSameResolution(t, gotID, gotErr, wantID, wantErr)
		})
		t.Run("live/"+identifier, func(t *testing.T) {
			gotID, gotErr := front.ResolveID(identifier)
			wantID, wantErr := ResolveSessionID(store, identifier)
			assertSameResolution(t, gotID, gotErr, wantID, wantErr)
		})
		t.Run("allow-closed/"+identifier, func(t *testing.T) {
			gotID, gotErr := front.ResolveIDAllowClosed(identifier)
			wantID, wantErr := ResolveSessionIDAllowClosed(store, identifier)
			assertSameResolution(t, gotID, gotErr, wantID, wantErr)
		})
	}

	// The closed session is the discriminator between the two live variants:
	// pin it directly so an implementation that routed both to the same
	// package function cannot pass on equivalence alone.
	if _, err := front.ResolveID("retired"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("ResolveID(closed session) = %v, want ErrSessionNotFound; the live resolver must not see closed beads", err)
	}
	if id, err := front.ResolveIDAllowClosed("retired"); err != nil || id != "s-closed-1" {
		t.Fatalf("ResolveIDAllowClosed(closed session) = (%q, %v), want (s-closed-1, nil)", id, err)
	}
}

func assertSameResolution(t *testing.T, gotID string, gotErr error, wantID string, wantErr error) {
	t.Helper()
	if gotID != wantID {
		t.Fatalf("front door resolved %q, package function resolved %q", gotID, wantID)
	}
	switch {
	case gotErr == nil && wantErr == nil:
	case gotErr == nil || wantErr == nil:
		t.Fatalf("front door error = %v, package function error = %v", gotErr, wantErr)
	case gotErr.Error() != wantErr.Error():
		t.Fatalf("front door error = %q, package function error = %q", gotErr, wantErr)
	}
}

// TestStoreLookupConfiguredNamedProjectsIdsWithoutBeads pins the bead-free
// projection the closed contract needs. ConfiguredNamedSessionLookup carries
// raw beads.Bead values, which must not cross the front door, and every cmd/gc
// consumer of that lookup reads only the two ids and the two flags. The method
// must report exactly what the raw lookup reports, projected.
func TestStoreLookupConfiguredNamedProjectsIdsWithoutBeads(t *testing.T) {
	spec := NamedSessionSpec{Identity: "demo/mayor", SessionName: "mayor"}
	seed := []beads.Bead{
		sessionBeadFixture("s-named-1", "open", map[string]string{
			"session_name":               "mayor",
			"alias":                      "demo/mayor",
			NamedSessionIdentityMetadata: "demo/mayor",
			"template":                   "mayor",
		}),
	}
	mem := beads.NewMemStoreFrom(len(seed), seed, nil)
	front := NewStore(beads.SessionStore{Store: mem})

	match, err := front.LookupConfiguredNamed(spec)
	if err != nil {
		t.Fatalf("LookupConfiguredNamed: %v", err)
	}
	lookup, err := LookupConfiguredNamedSession(mem, spec)
	if err != nil {
		t.Fatalf("LookupConfiguredNamedSession: %v", err)
	}
	if match.HasCanonical != lookup.HasCanonical || match.Canonical != lookup.Canonical.ID {
		t.Fatalf("canonical projection = (%v, %q), want (%v, %q)", match.HasCanonical, match.Canonical, lookup.HasCanonical, lookup.Canonical.ID)
	}
	if match.HasConflict != lookup.HasConflict || match.Conflict != lookup.Conflict.ID {
		t.Fatalf("conflict projection = (%v, %q), want (%v, %q)", match.HasConflict, match.Conflict, lookup.HasConflict, lookup.Conflict.ID)
	}
	if !match.HasCanonical || match.Canonical != "s-named-1" {
		t.Fatalf("LookupConfiguredNamed = %+v, want the canonical named-session bead", match)
	}
}

// TestStoreLookupConfiguredNamedReportsAConflict pins the other half of the
// projection: a live bead that claims the configured session_name without being
// the configured named session is a CONFLICT, and the resolver's error path
// depends on both the flag and the id.
func TestStoreLookupConfiguredNamedReportsAConflict(t *testing.T) {
	spec := NamedSessionSpec{Identity: "demo/mayor", SessionName: "mayor"}
	seed := []beads.Bead{
		sessionBeadFixture("s-squatter", "open", map[string]string{
			"session_name": "mayor",
			"alias":        "someone/else",
			"template":     "worker",
			"agent_name":   "someone-else",
		}),
	}
	mem := beads.NewMemStoreFrom(len(seed), seed, nil)
	front := NewStore(beads.SessionStore{Store: mem})

	match, err := front.LookupConfiguredNamed(spec)
	if err != nil {
		t.Fatalf("LookupConfiguredNamed: %v", err)
	}
	lookup, err := LookupConfiguredNamedSession(mem, spec)
	if err != nil {
		t.Fatalf("LookupConfiguredNamedSession: %v", err)
	}
	if !lookup.HasConflict {
		t.Fatal("fixture no longer produces a conflict; the projection assertion below would be vacuous")
	}
	if match.HasConflict != lookup.HasConflict || match.Conflict != lookup.Conflict.ID {
		t.Fatalf("conflict projection = (%v, %q), want (%v, %q)", match.HasConflict, match.Conflict, lookup.HasConflict, lookup.Conflict.ID)
	}
	if match.HasCanonical {
		t.Fatalf("LookupConfiguredNamed = %+v, want no canonical bead", match)
	}
}

// TestStoreResolveMethodsTolerateAnUnbackedDoor pins the nil-inner-store
// behavior the rest of the read surface already has (listAllBeads,
// validatedBead): a door over no store answers, it does not panic. The
// resolution methods route through the package functions' own nil-store
// contract, so this test is what proves they inherited it rather than
// dereferencing.
func TestStoreResolveMethodsTolerateAnUnbackedDoor(t *testing.T) {
	for name, front := range map[string]*Store{
		"nil receiver":    nil,
		"nil inner store": NewStore(beads.SessionStore{}),
		"nil typed store": NewStore(beads.SessionStore{Store: nil}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := front.ResolveIDByExactID("anything"); err == nil {
				t.Fatal("ResolveIDByExactID over an unbacked door returned no error")
			}
			if _, err := front.ResolveID("anything"); err == nil {
				t.Fatal("ResolveID over an unbacked door returned no error")
			}
			if _, err := front.ResolveIDAllowClosed("anything"); err == nil {
				t.Fatal("ResolveIDAllowClosed over an unbacked door returned no error")
			}
			match, err := front.LookupConfiguredNamed(NamedSessionSpec{Identity: "demo/mayor"})
			if err != nil {
				t.Fatalf("LookupConfiguredNamed over an unbacked door: %v", err)
			}
			if match.HasCanonical || match.HasConflict {
				t.Fatalf("LookupConfiguredNamed over an unbacked door = %+v, want an empty match", match)
			}
		})
	}
}
