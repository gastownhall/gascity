package storeref

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// classFixture is the shape every case below routes over: a relocated class
// store, the work store it moved away from, and a rig work store that may or may
// not shadow the class namespace.
type classFixture struct {
	class beads.Store
	work  beads.Store
	rig   beads.Store
}

func newClassFixture() classFixture {
	return classFixture{
		class: beads.NewMemStore(),
		work:  beads.NewMemStore(),
		rig:   beads.NewMemStore(),
	}
}

func storeNames(f classFixture, stores []beads.Store) []string {
	names := make([]string, 0, len(stores))
	for _, s := range stores {
		switch s {
		case f.class:
			names = append(names, "class")
		case f.work:
			names = append(names, "work")
		case f.rig:
			names = append(names, "rig")
		default:
			names = append(names, "unknown")
		}
	}
	return names
}

func assertCandidates(t *testing.T, f classFixture, got []beads.Store, want ...string) {
	t.Helper()
	names := storeNames(f, got)
	if len(names) != len(want) {
		t.Fatalf("candidates = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", names, want)
		}
	}
}

// TestClassCandidatesNotRelocated pins the identity gate: the resolver is keyed
// on whether the class store IS the work store, never on a marker file or a
// migration flag. A default city gets nil, which is what keeps its legacy by-id
// resolution byte-identical.
func TestClassCandidatesNotRelocated(t *testing.T) {
	f := newClassFixture()
	for _, tt := range []struct {
		name    string
		routing ClassRouting
	}{
		{"class store is the work store", ClassRouting{Prefix: "gcg", Class: f.work, Work: f.work}},
		{"no class store at all", ClassRouting{Prefix: "gcg", Class: nil, Work: f.work}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassCandidates("gcg-1", tt.routing); got != nil {
				t.Fatalf("ClassCandidates(gcg-1) = %v, want nil — the class is not relocated", storeNames(f, got))
			}
		})
	}
}

// TestClassCandidatesOutsideNamespace pins that the arm only fires for ids in
// the class namespace, and only under the exact-or-hyphen rule: a bare "gcg" is
// in, a longer word starting with the same letters is not.
func TestClassCandidatesOutsideNamespace(t *testing.T) {
	f := newClassFixture()
	routing := ClassRouting{Prefix: "gcg", Class: f.class, Work: f.work}
	for _, id := range []string{"gc-1", "gcgx-1", "gcgraph-9", "", "  "} {
		if got := ClassCandidates(id, routing); got != nil {
			t.Errorf("ClassCandidates(%q) = %v, want nil — %q is outside the gcg namespace", id, storeNames(f, got), id)
		}
	}
	for _, id := range []string{"gcg", "gcg-1", "gcg-wisp-0042", " gcg-1 "} {
		if got := ClassCandidates(id, routing); len(got) == 0 {
			t.Errorf("ClassCandidates(%q) = nil, want the class candidate list", id)
		}
	}
}

// TestClassCandidatesRelocated pins the base list a relocated class produces:
// the class store first (it is the sole minter of the namespace), then the work
// store as the fallback leg for an in-namespace id the class store does not
// hold. This pair is also the whole of the pre-seam internal/api list, and it
// stays the HEAD of every list this resolver builds.
func TestClassCandidatesRelocated(t *testing.T) {
	f := newClassFixture()
	got := ClassCandidates("gcg-1", ClassRouting{Prefix: "gcg", Class: f.class, Work: f.work})
	assertCandidates(t, f, got, "class", "work")
}

// TestClassCandidatesMigratedLegacyIDIsOutOfNamespace pins the limitation the
// doc now states instead of the coverage it used to claim.
//
// `gc storage migrate` preserves ids, so a bead it relocated keeps its HQ/rig-era
// prefix. IDInNamespace gates before the candidate list is built, so such an id
// gets NIL here however thoroughly the class is relocated — the candidate shape
// cannot rescue an id it never sees. Covering it needs a residence probe that
// asks the class store about every id (cmd/gc/cmd_bd_by_id.go does; the
// internal/api by-id path does not).
//
// If this ever starts returning candidates, the resolver has grown a residence
// arm and the "NOT covered" section of ClassCandidates' doc is stale.
func TestClassCandidatesMigratedLegacyIDIsOutOfNamespace(t *testing.T) {
	f := newClassFixture()
	routing := ClassRouting{
		Prefix:  "gcg",
		Class:   f.class,
		Work:    f.work,
		Shadows: []PrefixedStore{{Prefix: "mc", Store: f.work}},
	}
	// The id a migrated graph bead carries: minted by the work store under the
	// HQ prefix, then copied into the class store with the id preserved.
	if got := ClassCandidates("mc-123", routing); got != nil {
		t.Fatalf("ClassCandidates(mc-123) = %v, want nil — a relocated bead that kept its work-era id is OUTSIDE this resolver's namespace and is not covered by it", storeNames(f, got))
	}
}

// TestClassCandidatesKeepsPreSeamHead pins the property that makes the added
// legs safe: the pre-seam [class, work] list is a strict PREFIX of whatever this
// resolver returns, whatever shadows are configured. Every probe the old list
// made still happens in the same order, so a shadow store cannot change an
// answer the old list served, and cannot fail ahead of the store that holds the
// bead and turn a served read into an error.
func TestClassCandidatesKeepsPreSeamHead(t *testing.T) {
	f := newClassFixture()
	for _, tt := range []struct {
		name    string
		id      string
		shadows []PrefixedStore
	}{
		{"no shadows", "gcg-1", nil},
		{"reserved-prefix rig", "gcg-1", []PrefixedStore{{Prefix: "gcg", Store: f.rig}}},
		{"longer-prefix rig", "gcg-alpha-1", []PrefixedStore{{Prefix: "gcg-alpha", Store: f.rig}}},
		{"HQ shadows too", "gcg-alpha-1", []PrefixedStore{{Prefix: "gcg", Store: f.work}, {Prefix: "gcg-alpha", Store: f.rig}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassCandidates(tt.id, ClassRouting{Prefix: "gcg", Class: f.class, Work: f.work, Shadows: tt.shadows})
			names := storeNames(f, got)
			if len(names) < 2 || names[0] != "class" || names[1] != "work" {
				t.Fatalf("candidates = %v, want the pre-seam [class work] head first", names)
			}
		})
	}
}

// TestClassCandidatesNoWorkStore pins the class-only list: with no work store to
// fall back to, the class store is the whole answer rather than a nil entry the
// caller has to skip.
func TestClassCandidatesNoWorkStore(t *testing.T) {
	f := newClassFixture()
	got := ClassCandidates("gcg-1", ClassRouting{Prefix: "gcg", Class: f.class})
	assertCandidates(t, f, got, "class")
}

// TestClassCandidatesKeepsShadowingWorkStore is carried minor (a) from PR
// #5128's council: a rig configured with the reserved class prefix is
// warned-and-ALLOWED (config.ReservedPrefixWarnings; config.ValidateRigs does
// not reject it), so its beads are real and must stay reachable by id once the
// class relocates. Dropping it from the candidate set made them unreachable.
func TestClassCandidatesKeepsShadowingWorkStore(t *testing.T) {
	f := newClassFixture()
	got := ClassCandidates("gcg-1", ClassRouting{
		Prefix: "gcg",
		Class:  f.class,
		Work:   f.work,
		Shadows: []PrefixedStore{
			{Prefix: "mc", Store: f.work},
			{Prefix: "gcg", Store: f.rig},
		},
	})
	assertCandidates(t, f, got, "class", "work", "rig")
}

// TestClassCandidatesKeepsLongerPrefixWorkStore is carried minor (b): an id
// under a LONGER configured prefix that starts with the reserved one is inside
// the class namespace by the exact-or-hyphen rule, so the arm fires — and used
// to silently drop the rig store that actually declares the longer prefix.
//
// The longer prefix sorts FIRST among the shadows because it is the more
// specific declared owner — but the whole shadow tail sits behind the pre-seam
// [class, work] head, so the added leg can only extend reachability and never
// re-answer an id the old list already resolved.
func TestClassCandidatesKeepsLongerPrefixWorkStore(t *testing.T) {
	f := newClassFixture()
	got := ClassCandidates("gcg-alpha-1", ClassRouting{
		Prefix: "gcg",
		Class:  f.class,
		Work:   f.work,
		Shadows: []PrefixedStore{
			{Prefix: "gcg", Store: f.work},
			{Prefix: "gcg-alpha", Store: f.rig},
		},
	})
	assertCandidates(t, f, got, "class", "work", "rig")
}

// TestClassCandidatesDedupesAndSkipsNils pins that a store named twice (the HQ
// prefix shadowing the class namespace makes the work store its own shadow)
// appears once, and that an unloaded store never occupies a slot.
func TestClassCandidatesDedupesAndSkipsNils(t *testing.T) {
	f := newClassFixture()
	got := ClassCandidates("gcg-1", ClassRouting{
		Prefix: "gcg",
		Class:  f.class,
		Work:   f.work,
		Shadows: []PrefixedStore{
			{Prefix: "gcg", Store: f.work},
			{Prefix: "gcg", Store: nil},
			{Prefix: "", Store: f.rig},
		},
	})
	assertCandidates(t, f, got, "class", "work")
}

// TestClassCandidatesIgnoresNonCoveringShadows pins that a shadow whose prefix
// does not cover the id contributes nothing — the list is not "every work
// store", it is the stores that could actually hold this id.
func TestClassCandidatesIgnoresNonCoveringShadows(t *testing.T) {
	f := newClassFixture()
	got := ClassCandidates("gcg-1", ClassRouting{
		Prefix: "gcg",
		Class:  f.class,
		Work:   f.work,
		Shadows: []PrefixedStore{
			{Prefix: "mc", Store: f.work},
			{Prefix: "mr", Store: f.rig},
		},
	})
	assertCandidates(t, f, got, "class", "work")
}

// TestIDInNamespace pins the exact-or-hyphen rule and, on the same ids, the
// deliberate divergence from PrefixOwner's separator-only rule. Collapsing the
// two would let a bare id capture a store that never mints it.
func TestIDInNamespace(t *testing.T) {
	for _, tt := range []struct {
		id, prefix string
		want       bool
	}{
		{"gcg", "gcg", true},
		{"gcg-1", "gcg", true},
		{"gcg-wisp-0042", "gcg", true},
		{"gcgx-1", "gcg", false},
		{"gc-1", "gcg", false},
		{"gcg-1", "", false},
		{"gcg-1", "  ", false},
		{"gcg-1", " gcg ", true},
	} {
		if got := IDInNamespace(tt.id, tt.prefix); got != tt.want {
			t.Errorf("IDInNamespace(%q, %q) = %v, want %v", tt.id, tt.prefix, got, tt.want)
		}
	}
	// The bare-id divergence, stated against PrefixOwner directly.
	bare := newPrefixed("gcg")
	if owner := PrefixOwner("gcg", []beads.Store{bare}); owner != nil {
		t.Fatal("PrefixOwner(\"gcg\") claimed a store; the separator rule must reject the bare form, and IDInNamespace must stay the only accessor that admits it")
	}
	if !IDInNamespace("gcg", "gcg") {
		t.Fatal("IDInNamespace(\"gcg\", \"gcg\") = false; the configured-prefix rule must admit the bare form")
	}
}
