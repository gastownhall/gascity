// Package sessiontest provides real-store test doubles for building
// session.Info fixtures the way production does: seed a bead into a
// memstore-backed session front door and read the typed Info back through it.
//
// It exists so black-box tests in cmd/gc, internal/api, and internal/worker can
// stop hand-crafting beads.Bead literals and cracking them with the raw session
// projection codec — the codec belongs at the store edge, not in test setup.
// Reading a fixture back through session.Store.Get runs that exact codec
// internally, so the projection is byte-identical to the raw-bead form while the
// test never touches a raw *beads.Bead.
//
// internal/session's OWN white-box tests must NOT import this package: it
// imports session, so importing it back would create an import cycle. Those
// tests keep their in-package seedSessionStore / sessionBeadFixture helpers.
//
// Fidelity note (why Store/SeedBead seed VERBATIM, not via Create): a memstore's
// Create rewrites the bead — it forces a gc-N id, status "open", and
// CreatedAt=now (session.CreateSessionInfo inherits that, so it cannot express a
// pinned id, Status="closed", a custom CreatedAt, or extra labels). Verbatim
// fidelity therefore lives at construction, through beads.NewMemStoreFrom. Use
// Info for store-create fixtures where the test reads the store-assigned id
// back; use SeedBead / Store(seed…) for fixtures that pin any of those fields.
package sessiontest

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// Store returns a memstore-backed session front door together with the raw
// MemStore behind it. Every bead in seed is inserted VERBATIM at construction
// (via beads.NewMemStoreFrom), preserving its ID, Status, CreatedAt, Labels, and
// Metadata — the fields a store.Create would rewrite. Read fixtures back through
// the returned *session.Store (Get/List run the production codec); drive any
// additional store-assigned writes through the returned *beads.MemStore.
func Store(t testing.TB, seed ...beads.Bead) (*session.Store, *beads.MemStore) {
	t.Helper()
	mem := beads.NewMemStoreFrom(len(seed), seed, nil)
	return session.NewStore(beads.SessionStore{Store: mem}), mem
}

// Info creates a session through the front door and returns the projected
// session.Info of the just-created bead — the store-create fixture path,
// mirroring how production creates a session and reads its Info back.
//
// A memstore assigns the id (MemStore.Create does NOT honor spec.ID) and forces
// CreatedAt=now, so use Info only when the test reads the RETURNED Info.ID
// rather than asserting a specific id. For a pinned id, Status="closed", custom
// labels, or a pinned CreatedAt, use SeedBead instead — CreateSpec cannot
// express those.
//
// Field gotcha: spec.AgentName drives the "agent:<name>" selection LABEL only.
// The projected Info.AgentName comes from spec.Metadata["agent_name"], so a
// fixture that reads Info.AgentName must also set it in Metadata.
func Info(t testing.TB, s *session.Store, spec session.CreateSpec) session.Info {
	t.Helper()
	info, err := s.CreateSessionInfo(spec)
	if err != nil {
		t.Fatalf("sessiontest.Info: CreateSessionInfo(%+v): %v", spec, err)
	}
	return info
}

// SeedBead seeds b VERBATIM into a throwaway front-door store and returns the
// front-door Get projection — the same session.Info production reads for a
// persisted bead of that exact shape, with the codec confined to the store edge.
// Unlike Info (store-create), SeedBead preserves b's ID, Status (e.g. "closed"),
// CreatedAt, Labels, and Metadata.
//
// b MUST be a session-shaped bead with a non-empty ID: the front door narrows
// via session.IsSessionBeadOrRepairable and rejects an empty id, so a
// deliberately degraded / non-session / empty-id corpus would be filtered out —
// keep those fixtures on a struct literal (or the raw codec in internal/session).
func SeedBead(t testing.TB, b beads.Bead) session.Info {
	t.Helper()
	s, _ := Store(t, b)
	info, err := s.Get(b.ID)
	if err != nil {
		t.Fatalf("sessiontest.SeedBead: Get(%q) after verbatim seed: %v", b.ID, err)
	}
	return info
}

// InfoFromMeta is the one-liner for a standalone, metadata-only fixture with no
// store under test: it wraps meta in a minimal session-shaped bead and returns
// the front-door projection. The synthetic id is meta["session_name"] when set,
// else "s-fixture" — so InfoFromMeta is for fixtures that assert on projected
// metadata fields, NOT on Info.ID. When the id matters, build the bead and call
// SeedBead directly.
func InfoFromMeta(t testing.TB, meta map[string]string) session.Info {
	t.Helper()
	id := meta["session_name"]
	if id == "" {
		id = "s-fixture"
	}
	return SeedBead(t, beads.Bead{
		ID:       id,
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: meta,
	})
}
