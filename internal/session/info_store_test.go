package session

import (
	"errors"
	"reflect"
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

// TestStoreGetSpeaksInfo asserts the domain store hands back a session.Info
// projected from the persisted bead, with bead serialization confined inside
// the store. The Info must match the persisted projection codec exactly.
func TestStoreGetSpeaksInfo(t *testing.T) {
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

	is := NewStore(store)
	got, err := is.Get("s-get-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	want := infoFromPersistedBead(b)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Get returned Info mismatch:\n got = %+v\nwant = %+v", got, want)
	}
	if got.ID != "s-get-1" || got.Title != "My Session" || got.Alias != "pc-1" {
		t.Fatalf("Get returned unexpected scalar fields: %+v", got)
	}
	if got.State != StateAsleep {
		t.Fatalf("Get state = %q, want %q", got.State, StateAsleep)
	}
}

// TestStoreGetProjectsLastWokeAtFromLocalString proves Get reads
// Info.LastWokeAt from clone-local storage, not durable Bead.Metadata — the
// read-side half of the last_woke_at cutover (ga-igcny0.1.2.1 Phase B). The
// fixture carries no durable last_woke_at at all (the post-migration steady
// state); only a clone-local value is seeded, and the projection must still
// surface it as the current value.
func TestStoreGetProjectsLastWokeAtFromLocalString(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	if err := mem.SetLocalString("s-1", "last_woke_at", "2026-08-12T00:00:00Z"); err != nil {
		t.Fatalf("SetLocalString (seed): %v", err)
	}
	is := NewStore(beads.SessionStore{Store: mem})

	got, err := is.Get("s-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastWokeAt != "2026-08-12T00:00:00Z" {
		t.Errorf("Get LastWokeAt = %q, want %q (from clone-local storage; durable Metadata never carried the key)", got.LastWokeAt, "2026-08-12T00:00:00Z")
	}
}

// TestStoreGetFallsBackToDurableLastWokeAtWhenLocalUntouched pins the
// durable-fallback half of Store.projectWithLocalOverlay: a bead whose
// last_woke_at lives only in durable Bead.Metadata — because it predates the
// last_woke_at cutover (ga-igcny0.1.2.1 Phase B), or was only ever touched
// via session.Tx, which writes durable directly and never calls
// SetLocalString (see storeTx.ApplyPatch) — reads back its real durable
// value, not blank. GetLocalString returns ("", nil) both when a key was
// explicitly cleared and when it was never set (SetLocalString's
// empty-clears contract), so the overlay cannot tell "never touched" apart
// from "cleared" by itself; it relies on the write-side guarantee that an
// explicit clear also clears durable (splitLocalMetadataPatch /
// setMetadataValue in store.go), so an empty local read always means
// "durable is authoritative here," never "a real clear was silently lost."
// See TestGetReflectsApplyPatch (store_test.go) for the paired case — an
// explicit clear through Store.ApplyPatch — which pins that durable agrees
// with local ("") after a real clear, so this fallback does not resurrect a
// stale value there.
func TestStoreGetFallsBackToDurableLastWokeAtWhenLocalUntouched(t *testing.T) {
	b := sessionBeadFixture("s-durable-only", "open", map[string]string{
		"state":        "active",
		"last_woke_at": "2026-08-01T00:00:00Z",
	})
	store := seedSessionStore(t, b)
	is := NewStore(store)

	got, err := is.Get("s-durable-only")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastWokeAt != "2026-08-01T00:00:00Z" {
		t.Errorf("Get LastWokeAt = %q, want the durable value %q (clone-local was never written, so the overlay falls back to durable)", got.LastWokeAt, "2026-08-01T00:00:00Z")
	}
}

// panicsOnGetLocalStringStore is a beads.Store double that implements only Get
// and List by embedding the bare interface, leaving every other method —
// including GetLocalString — as the promoted-but-nil embedded value. This is
// the exact test-double shape used throughout the repo (e.g. cmd/gc's
// lookupPoolSessionNameCandidatesStore, internal/api's pathAliasFakeStore):
// a partial mock built by embedding beads.Store and overriding only the one
// or two methods the test exercises. Calling any unoverridden method panics
// with a nil pointer dereference.
type panicsOnGetLocalStringStore struct {
	beads.Store
	beads []beads.Bead
}

func (s panicsOnGetLocalStringStore) Get(id string) (beads.Bead, error) {
	for _, b := range s.beads {
		if b.ID == id {
			return b, nil
		}
	}
	return beads.Bead{}, beads.ErrNotFound
}

func (s panicsOnGetLocalStringStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return s.beads, nil
}

// TestStoreGetAndListSurviveGetLocalStringPanic proves Get and List fall back
// to the durable last_woke_at via safeGetLocalString instead of panicking
// when the underlying beads.Store is a partial double whose GetLocalString
// resolves to a promoted nil embedded interface method. This is the failure
// mode hit by TestLookupPoolSessionNames_DoesNotRecoverOwnedPoolSessionNameSlot
// (cmd/gc) and TestResolveLiveSessionByPathAlias_TiebreakerPrefersMostRecent
// (internal/api), reproduced directly against this package's own
// GetLocalString-calling overlay rather than through those callers.
func TestStoreGetAndListSurviveGetLocalStringPanic(t *testing.T) {
	b := sessionBeadFixture("s-panic", "open", map[string]string{
		"state":        "active",
		"last_woke_at": "2026-08-01T00:00:00Z",
	})
	double := panicsOnGetLocalStringStore{beads: []beads.Bead{b}}
	is := NewStore(beads.SessionStore{Store: double})

	got, err := is.Get("s-panic")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastWokeAt != "2026-08-01T00:00:00Z" {
		t.Errorf("Get LastWokeAt = %q, want durable fallback %q (not a panic)", got.LastWokeAt, "2026-08-01T00:00:00Z")
	}

	all, err := is.List("", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 || all[0].LastWokeAt != "2026-08-01T00:00:00Z" {
		t.Fatalf("List = %+v, want one Info with durable fallback LastWokeAt", all)
	}
}

// TestStoreGetNotFound asserts a missing session id surfaces a not-found error.
func TestStoreGetNotFound(t *testing.T) {
	store := seedSessionStore(t)
	is := NewStore(store)
	if _, err := is.Get("missing"); err == nil {
		t.Fatal("Get(missing): want error, got nil")
	}
}

// TestStoreGetNilInnerStore pins the WI-6 W4 nit-5 fix: a non-nil *Store wrapping
// a nil inner beads.Store must NOT panic in validatedBead — it returns the wrapped
// beads.ErrNotFound (the absence contract), mirroring ListAll's nil-inner-store
// guard. Get and GetPersistedResponse share validatedBead, so both are covered.
func TestStoreGetNilInnerStore(t *testing.T) {
	is := NewStore(beads.SessionStore{Store: nil})
	_, err := is.Get("gc-1")
	if err == nil {
		t.Fatal("Get on nil inner store: want error, got nil")
	}
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get on nil inner store: want errors.Is(beads.ErrNotFound), got %v", err)
	}
	if _, _, perr := is.GetPersistedResponse("gc-1"); !errors.Is(perr, beads.ErrNotFound) {
		t.Fatalf("GetPersistedResponse on nil inner store: want errors.Is(beads.ErrNotFound), got %v", perr)
	}
}

// TestStoreListFiltersLikeCatalog asserts List applies the same state and
// template filtering as the existing ListFullFromBeads projection, returns only
// session.Info (no raw beads), and excludes closed beads by default.
func TestStoreListFiltersLikeCatalog(t *testing.T) {
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
	is := NewStore(store)

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

	infoA, err := NewStore(storeA).Get("s-inv")
	if err != nil {
		t.Fatalf("Get A: %v", err)
	}
	infoB, err := NewStore(storeB).Get("s-inv")
	if err != nil {
		t.Fatalf("Get B: %v", err)
	}
	if !reflect.DeepEqual(infoA, infoB) {
		t.Fatalf("projection not deterministic across store instances:\n A = %+v\n B = %+v", infoA, infoB)
	}
	// And the direct codec matches the stored projection.
	if direct := infoFromPersistedBead(b); !reflect.DeepEqual(direct, infoA) {
		t.Fatalf("direct codec disagrees with store projection:\n codec = %+v\n store = %+v", direct, infoA)
	}
}

// TestInfoFromPersistedBeadProjectsContinuationAndSleepReason proves the
// additive Info.ContinuationEpoch / Info.SleepReason fields project verbatim
// from plain bead metadata, keeping the projection backend-invariant. These
// fields exist only for internal session reads (cmd_wait registration / retry /
// wait-hold clear); they are NOT emitted on the HTTP session-response wire.
func TestInfoFromPersistedBeadProjectsContinuationAndSleepReason(t *testing.T) {
	b := sessionBeadFixture("s-cont", "open", map[string]string{
		"session_name":       "polecat-1",
		"continuation_epoch": "9",
		"sleep_reason":       "wait-hold",
	})
	info := infoFromPersistedBead(b)
	if info.ContinuationEpoch != "9" {
		t.Errorf("ContinuationEpoch = %q, want %q", info.ContinuationEpoch, "9")
	}
	if info.SleepReason != "wait-hold" {
		t.Errorf("SleepReason = %q, want %q", info.SleepReason, "wait-hold")
	}
	// Unset markers project to empty (no error, no default).
	bare := sessionBeadFixture("s-bare", "open", map[string]string{"state": "active"})
	if got := infoFromPersistedBead(bare); got.ContinuationEpoch != "" || got.SleepReason != "" {
		t.Errorf("unset markers projected non-empty: epoch=%q reason=%q", got.ContinuationEpoch, got.SleepReason)
	}
}

func TestInfoFromPersistedBeadProjectsIdentityPoolNamedCluster(t *testing.T) {
	b := sessionBeadFixture("s-cluster", "open", map[string]string{
		"state":                      "active",
		NamedSessionIdentityMetadata: "worker#3",
		NamedSessionMetadataKey:      "true",
		NamedSessionModeMetadata:     "sticky",
		"common_name":                "worker",
		"pool_slot":                  "3",
		"pool_managed":               "true",
		"session_origin":             "ephemeral",
		"dependency_only":            "true",
		"manual_session":             "true",
	})
	info := infoFromPersistedBead(b)
	for _, c := range []struct{ name, got, want string }{
		{"ConfiguredNamedIdentity", info.ConfiguredNamedIdentity, "worker#3"},
		{"ConfiguredNamedMode", info.ConfiguredNamedMode, "sticky"},
		{"CommonName", info.CommonName, "worker"},
		{"PoolSlot", info.PoolSlot, "3"},
		{"SessionOrigin", info.SessionOrigin, "ephemeral"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if !info.ConfiguredNamedSession || !info.PoolManaged || !info.DependencyOnly || !info.ManualSession {
		t.Errorf("bool cluster: named=%v pool=%v dep=%v manual=%v, want all true",
			info.ConfiguredNamedSession, info.PoolManaged, info.DependencyOnly, info.ManualSession)
	}
	if len(info.Labels) != 1 || info.Labels[0] != LabelSession {
		t.Errorf("Labels = %v, want [%q]", info.Labels, LabelSession)
	}

	// Bare bead: the whole cluster projects to its zero value (no defaults).
	bare := infoFromPersistedBead(sessionBeadFixture("s-bare", "open", map[string]string{"state": "active"}))
	if bare.ConfiguredNamedSession || bare.PoolManaged || bare.DependencyOnly || bare.ManualSession ||
		bare.ConfiguredNamedIdentity != "" || bare.ConfiguredNamedMode != "" || bare.CommonName != "" ||
		bare.PoolSlot != "" || bare.SessionOrigin != "" {
		t.Errorf("unset cluster projected non-zero: %+v", bare)
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
