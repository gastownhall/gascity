package storeref

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// seed creates a bead in store and returns its store-assigned ID. MemStore
// generates the ID on Create, so callers must probe with the returned value.
func seed(t *testing.T, store beads.Store) string {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: "seed", Status: "open"})
	if err != nil {
		t.Fatalf("seeding bead: %v", err)
	}
	return b.ID
}

func TestPrefixOwnerSingleCandidateCollapsesToIt(t *testing.T) {
	store := beads.NewMemStore()
	got, matched := PrefixOwner("ga-1", []Candidate{{Prefix: "ga", Store: store}}, ExactOrHyphen)
	if !matched || got != store {
		t.Fatalf("PrefixOwner = (%#v, %v), want the sole store, true", got, matched)
	}
}

func TestPrefixOwnerNoMatchReturnsFalse(t *testing.T) {
	store := beads.NewMemStore()
	got, matched := PrefixOwner("zz-1", []Candidate{{Prefix: "ga", Store: store}}, ExactOrHyphen)
	if matched || got != nil {
		t.Fatalf("PrefixOwner = (%#v, %v), want nil, false for unmatched prefix", got, matched)
	}
}

func TestPrefixOwnerHyphenNamespaceMatchesBothModes(t *testing.T) {
	store := beads.NewMemStore()
	cands := []Candidate{{Prefix: "ga", Store: store}}
	for _, mode := range []PrefixMatch{ExactOrHyphen, HyphenOnly} {
		if got, matched := PrefixOwner("ga-1", cands, mode); !matched || got != store {
			t.Fatalf("mode %d: PrefixOwner(ga-1) = (%#v, %v), want store, true", mode, got, matched)
		}
	}
}

// TestPrefixOwnerBareIDMatchDiverges pins the landmine: a bare ID equal to the
// prefix matches under ExactOrHyphen but not under HyphenOnly.
func TestPrefixOwnerBareIDMatchDiverges(t *testing.T) {
	store := beads.NewMemStore()
	cands := []Candidate{{Prefix: "ga", Store: store}}

	if got, matched := PrefixOwner("ga", cands, ExactOrHyphen); !matched || got != store {
		t.Fatalf("ExactOrHyphen: PrefixOwner(ga) = (%#v, %v), want store, true", got, matched)
	}
	if got, matched := PrefixOwner("ga", cands, HyphenOnly); matched || got != nil {
		t.Fatalf("HyphenOnly: PrefixOwner(ga) = (%#v, %v), want nil, false (bare id must not match)", got, matched)
	}
}

func TestPrefixOwnerLongestPrefixWins(t *testing.T) {
	city := beads.NewMemStore()
	rig := beads.NewMemStore()
	// "mc" (city) and "mc-alpha" (rig) both prefix "mc-alpha-123"; the longer
	// rig prefix must win regardless of candidate order.
	cands := []Candidate{
		{Prefix: "mc", Store: city},
		{Prefix: "mc-alpha", Store: rig},
	}
	if got, matched := PrefixOwner("mc-alpha-123", cands, ExactOrHyphen); !matched || got != rig {
		t.Fatalf("PrefixOwner = (%#v, %v), want longest-prefix rig store", got, matched)
	}

	reversed := []Candidate{
		{Prefix: "mc-alpha", Store: rig},
		{Prefix: "mc", Store: city},
	}
	if got, matched := PrefixOwner("mc-alpha-123", reversed, ExactOrHyphen); !matched || got != rig {
		t.Fatalf("PrefixOwner (reversed order) = (%#v, %v), want longest-prefix rig store", got, matched)
	}
}

func TestPrefixOwnerEqualLengthTieKeepsFirst(t *testing.T) {
	first := beads.NewMemStore()
	second := beads.NewMemStore()
	cands := []Candidate{
		{Prefix: "ga", Store: first},
		{Prefix: "ga", Store: second},
	}
	if got, _ := PrefixOwner("ga-1", cands, ExactOrHyphen); got != first {
		t.Fatalf("PrefixOwner = %#v, want first candidate on length tie", got)
	}
}

func TestPrefixOwnerSkipsEmptyPrefix(t *testing.T) {
	owner := beads.NewMemStore()
	cands := []Candidate{
		{Prefix: "", Store: beads.NewMemStore()}, // empty prefix never matches
		{Prefix: "ga", Store: owner},
	}
	if got, matched := PrefixOwner("ga-1", cands, ExactOrHyphen); !matched || got != owner {
		t.Fatalf("PrefixOwner = (%#v, %v), want store with valid prefix, true", got, matched)
	}
}

// TestPrefixOwnerNilStoreStillClaimsMatch pins the controller's semantic: a
// configured prefix that owns the id wins its slot even when its store is not
// loaded, so the caller can suppress an all-stores fallback. The longest
// matching prefix wins regardless, so a nil-store owner is reported as
// (nil, true) — not skipped in favor of a shorter prefix.
func TestPrefixOwnerNilStoreStillClaimsMatch(t *testing.T) {
	city := beads.NewMemStore()
	cands := []Candidate{
		{Prefix: "mc", Store: city},
		{Prefix: "mc-alpha", Store: nil}, // longer prefix owns, store not loaded
	}
	got, matched := PrefixOwner("mc-alpha-1", cands, ExactOrHyphen)
	if !matched {
		t.Fatalf("PrefixOwner matched = false, want true (owning prefix exists)")
	}
	if got != nil {
		t.Fatalf("PrefixOwner store = %#v, want nil (owning store not loaded)", got)
	}
}

func TestPrefixOwnerEmptyIDReturnsFalse(t *testing.T) {
	if got, matched := PrefixOwner("", []Candidate{{Prefix: "ga", Store: beads.NewMemStore()}}, ExactOrHyphen); matched || got != nil {
		t.Fatalf("PrefixOwner(\"\") = (%#v, %v), want nil, false", got, matched)
	}
}

func TestResolveSingleStoreReturnsBead(t *testing.T) {
	store := beads.NewMemStore()
	id := seed(t, store)
	got, err := Resolve(id, []beads.Store{store})
	if err != nil {
		t.Fatalf("Resolve error = %v, want nil", err)
	}
	if got.ID != id {
		t.Fatalf("Resolve bead = %q, want %q", got.ID, id)
	}
}

func TestResolveProbesUntilFirstHit(t *testing.T) {
	empty := beads.NewMemStore()
	owner := beads.NewMemStore()
	id := seed(t, owner)
	got, err := Resolve(id, []beads.Store{empty, owner})
	if err != nil {
		t.Fatalf("Resolve error = %v, want nil", err)
	}
	if got.ID != id {
		t.Fatalf("Resolve bead = %q, want %q", got.ID, id)
	}
}

func TestResolveReturnsErrNotFoundWhenAbsentEverywhere(t *testing.T) {
	a := beads.NewMemStore()
	b := beads.NewMemStore()
	_, err := Resolve("ga-1", []beads.Store{a, b})
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Resolve error = %v, want ErrNotFound", err)
	}
}

func TestResolveEmptyStoreListIsNotFound(t *testing.T) {
	_, err := Resolve("ga-1", nil)
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Resolve(nil) error = %v, want ErrNotFound", err)
	}
}

func TestResolveSkipsNilStores(t *testing.T) {
	owner := beads.NewMemStore()
	id := seed(t, owner)
	got, err := Resolve(id, []beads.Store{nil, owner, nil})
	if err != nil {
		t.Fatalf("Resolve error = %v, want nil", err)
	}
	if got.ID != id {
		t.Fatalf("Resolve bead = %q, want %q", got.ID, id)
	}
}

// errStore returns a fixed error from Get to exercise the non-not-found path.
type errStore struct {
	beads.Store
	err error
}

func (e errStore) Get(string) (beads.Bead, error) { return beads.Bead{}, e.err }

func TestResolvePropagatesNonNotFoundError(t *testing.T) {
	boom := errors.New("backend exploded")
	_, err := Resolve("ga-1", []beads.Store{errStore{err: boom}, beads.NewMemStore()})
	if !errors.Is(err, boom) {
		t.Fatalf("Resolve error = %v, want %v", err, boom)
	}
	if errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Resolve error must not be ErrNotFound when a probe hard-errors")
	}
}

func TestResolveTreatsWrappedNotFoundAsAbsent(t *testing.T) {
	wrapped := fmt.Errorf("getting bead: %w", beads.ErrNotFound)
	owner := beads.NewMemStore()
	id := seed(t, owner)
	got, err := Resolve(id, []beads.Store{errStore{err: wrapped}, owner})
	if err != nil {
		t.Fatalf("Resolve error = %v, want nil (wrapped not-found should be skipped)", err)
	}
	if got.ID != id {
		t.Fatalf("Resolve bead = %q, want %q", got.ID, id)
	}
}
