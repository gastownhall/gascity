package session

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// sessionBeadFixture builds a persisted session bead with the given id, status,
// and metadata, carrying the canonical type and label so the read seam
// recognizes it.
func sessionBeadFixture(id, status string, meta map[string]string) beads.Bead {
	m := map[string]string{}
	for k, v := range meta {
		m[k] = v
	}
	return beads.Bead{
		ID:        id,
		Type:      BeadType,
		Status:    status,
		Title:     m["__title"],
		Labels:    []string{LabelSession},
		Metadata:  m,
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func seedSessionStore(t *testing.T, beadsIn ...beads.Bead) beads.SessionStore {
	t.Helper()
	// NewMemStoreFrom seeds beads verbatim, preserving the fixture IDs and
	// status (Create would rewrite both), which the projection tests depend on.
	mem := beads.NewMemStoreFrom(len(beadsIn), beadsIn, nil)
	return beads.SessionStore{Store: mem}
}

// TestInfoStoreGetSpeaksInfo asserts the domain store hands back a session.Info
// projected from the persisted bead, with bead serialization confined inside
// the store. The Info must match the persisted projection codec exactly.
func TestInfoStoreGetSpeaksInfo(t *testing.T) {
	b := sessionBeadFixture("s-get-1", "open", map[string]string{
		"__title":      "My Session",
		"template":     "polecat",
		"state":        "asleep",
		"alias":        "pc-1",
		"agent_name":   "polecat-7",
		"provider":     "claude",
		"command":      "claude --foo",
		"work_dir":     "/tmp/wd",
		"session_name": "s-get-1",
		"session_key":  "uuid-abc",
	})
	store := seedSessionStore(t, b)

	is := NewInfoStore(store)
	got, err := is.Get("s-get-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	want := InfoFromPersistedBead(b)
	if got != want {
		t.Fatalf("Get returned Info mismatch:\n got = %+v\nwant = %+v", got, want)
	}
	if got.ID != "s-get-1" || got.Title != "My Session" || got.Alias != "pc-1" {
		t.Fatalf("Get returned unexpected scalar fields: %+v", got)
	}
	if got.State != StateAsleep {
		t.Fatalf("Get state = %q, want %q", got.State, StateAsleep)
	}
}

// TestInfoStoreGetNotFound asserts a missing session id surfaces a not-found error.
func TestInfoStoreGetNotFound(t *testing.T) {
	store := seedSessionStore(t)
	is := NewInfoStore(store)
	if _, err := is.Get("missing"); err == nil {
		t.Fatal("Get(missing): want error, got nil")
	}
}

// TestInfoStoreListFiltersLikeCatalog asserts List applies the same state and
// template filtering as the existing ListFullFromBeads projection, returns only
// session.Info (no raw beads), and excludes closed beads by default.
func TestInfoStoreListFiltersLikeCatalog(t *testing.T) {
	open := sessionBeadFixture("s-open", "open", map[string]string{
		"template": "polecat", "state": "asleep",
	})
	active := sessionBeadFixture("s-active", "open", map[string]string{
		"template": "mayor", "state": "active",
	})
	closed := sessionBeadFixture("s-closed", "closed", map[string]string{
		"template": "polecat", "state": "asleep",
	})
	store := seedSessionStore(t, open, active, closed)
	is := NewInfoStore(store)

	// Default filter excludes closed.
	all, err := is.List("", "")
	if err != nil {
		t.Fatalf("List default: %v", err)
	}
	ids := infoIDs(all)
	if has(ids, "s-closed") {
		t.Fatalf("default List must exclude closed; got %v", ids)
	}
	if !has(ids, "s-open") || !has(ids, "s-active") {
		t.Fatalf("default List must include open sessions; got %v", ids)
	}

	// Template filter.
	pc, err := is.List("", "polecat")
	if err != nil {
		t.Fatalf("List template: %v", err)
	}
	pcIDs := infoIDs(pc)
	if !has(pcIDs, "s-open") || has(pcIDs, "s-active") {
		t.Fatalf("template filter mismatch; got %v", pcIDs)
	}

	// state=closed surfaces the closed bead.
	cl, err := is.List("closed", "")
	if err != nil {
		t.Fatalf("List closed: %v", err)
	}
	if !has(infoIDs(cl), "s-closed") {
		t.Fatalf("state=closed must include closed bead; got %v", infoIDs(cl))
	}
}

// TestInfoFromPersistedBeadProjectionDeterminism asserts the persisted
// projection is a deterministic, store-identity-independent function of the
// bead's plain fields: the same bead seeded into two distinct store instances
// round-trips to the same Info, and the direct codec agrees with the store
// projection.
//
// NOTE: both stores here are in-memory (MemStore), so this proves codec
// determinism, not true cross-backend (bd/sqlite/postgres) serialization
// invariance. The stronger invariance claim holds only because
// InfoFromPersistedBead reads exclusively backend-agnostic plain bead fields
// (ID, Title, Status, CreatedAt, Metadata); a real cross-backend assertion
// would need to seed those backends directly.
func TestInfoFromPersistedBeadProjectionDeterminism(t *testing.T) {
	meta := map[string]string{
		"__title": "Inv", "template": "polecat", "state": "asleep",
		"alias": "a", "agent_name": "n", "provider": "claude",
		"command": "c", "work_dir": "/wd", "session_name": "s-inv",
		"session_key": "k", "resume_flag": "--resume", "resume_style": "flag",
	}
	b := sessionBeadFixture("s-inv", "open", meta)

	storeA := seedSessionStore(t, b)
	storeB := seedSessionStore(t, b)

	infoA, err := NewInfoStore(storeA).Get("s-inv")
	if err != nil {
		t.Fatalf("Get A: %v", err)
	}
	infoB, err := NewInfoStore(storeB).Get("s-inv")
	if err != nil {
		t.Fatalf("Get B: %v", err)
	}
	if infoA != infoB {
		t.Fatalf("projection not deterministic across store instances:\n A = %+v\n B = %+v", infoA, infoB)
	}
	// And the direct codec matches the stored projection.
	if direct := InfoFromPersistedBead(b); direct != infoA {
		t.Fatalf("direct codec disagrees with store projection:\n codec = %+v\n store = %+v", direct, infoA)
	}
}

func infoIDs(in []Info) []string {
	out := make([]string, 0, len(in))
	for _, i := range in {
		out = append(out, i.ID)
	}
	return out
}

func has(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
