package chartest_test

import (
	"regexp"
	"testing"

	"github.com/gastownhall/gascity/internal/chartest"
)

var (
	beadRule = chartest.Rule{Category: "BEAD", Pattern: regexp.MustCompile(`b-[a-z0-9]+`)}
	tsRule   = chartest.Rule{Category: "T", Pattern: regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`)}
)

func TestCanonicalize_SameTokenSamePlaceholder(t *testing.T) {
	c := chartest.NewCanonicalizer(beadRule)
	got := string(c.Canonicalize([]byte("root b-abc123 depends on b-abc123")))
	want := "root BEAD-1 depends on BEAD-1"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCanonicalize_DistinctTokensNumberedInOrder(t *testing.T) {
	c := chartest.NewCanonicalizer(beadRule)
	got := string(c.Canonicalize([]byte("b-zzz then b-aaa then b-zzz")))
	want := "BEAD-1 then BEAD-2 then BEAD-1"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCanonicalize_MultipleCategoriesNumberedPerCategory(t *testing.T) {
	c := chartest.NewCanonicalizer(beadRule, tsRule)
	in := "b-x1 at 2026-07-08T12:00:00Z, b-y2 at 2026-07-08T13:00:00Z, b-x1 again"
	got := string(c.Canonicalize([]byte(in)))
	want := "BEAD-1 at T-1, BEAD-2 at T-2, BEAD-1 again"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCanonicalize_NoMatchesUnchanged(t *testing.T) {
	c := chartest.NewCanonicalizer(beadRule, tsRule)
	in := "nothing volatile here"
	if got := string(c.Canonicalize([]byte(in))); got != in {
		t.Fatalf("got %q, want unchanged %q", got, in)
	}
}

func TestCanonicalizeStreams_CrossSurfaceIdentityAsserted(t *testing.T) {
	// The same real id in stdout and json must map to the SAME placeholder;
	// numbering follows the explicit stream order (stdout before json).
	c := chartest.NewCanonicalizer(beadRule)
	out := c.CanonicalizeStreams([]chartest.Stream{
		{Name: "stdout", Data: []byte("created b-new111 parent b-root22")},
		{Name: "json", Data: []byte(`{"id":"b-new111","parent":"b-root22"}`)},
	})
	if len(out) != 2 {
		t.Fatalf("got %d streams, want 2", len(out))
	}
	if got := string(out[0].Data); got != "created BEAD-1 parent BEAD-2" {
		t.Fatalf("stdout = %q", got)
	}
	if got := string(out[1].Data); got != `{"id":"BEAD-1","parent":"BEAD-2"}` {
		t.Fatalf("json = %q (cross-surface identity not preserved)", got)
	}
}

func TestCanonicalize_RowOrderBlindSpot(t *testing.T) {
	// Documents the KNOWN limitation: two rows differing ONLY by a volatile token
	// canonicalize identically regardless of emission order, so a byte-exact
	// golden cannot catch a reorder among them. Pinned so a future change that
	// accidentally makes such order observable is noticed and the doc caveat on
	// Canonicalizer updated.
	forward := chartest.NewCanonicalizer(beadRule)
	reverse := chartest.NewCanonicalizer(beadRule)
	got := string(forward.Canonicalize([]byte("b-aaa ready\nb-bbb ready")))
	rev := string(reverse.Canonicalize([]byte("b-bbb ready\nb-aaa ready")))
	if got != rev {
		t.Fatalf("expected order-blind canonicalization, got %q vs %q", got, rev)
	}
	if got != "BEAD-1 ready\nBEAD-2 ready" {
		t.Fatalf("canonicalized = %q, want %q", got, "BEAD-1 ready\nBEAD-2 ready")
	}
}

func TestCanonicalize_StableColumnMakesOrderObservable(t *testing.T) {
	// Proves the documented mitigation: seeding each row with a distinct STABLE
	// column makes a reorder change the canonicalized bytes, so a multi-row
	// golden that must protect order can.
	forward := chartest.NewCanonicalizer(beadRule)
	reverse := chartest.NewCanonicalizer(beadRule)
	got := string(forward.Canonicalize([]byte("b-aaa alpha\nb-bbb beta")))
	rev := string(reverse.Canonicalize([]byte("b-bbb beta\nb-aaa alpha")))
	if got == rev {
		t.Fatalf("distinct stable columns should make order observable, both = %q", got)
	}
}

func TestCanonicalize_LongerMatchWinsAtSamePosition(t *testing.T) {
	// Two rules that could both match at the same start: the longer match is
	// emitted, the overlapping shorter one skipped — deterministic, no garbling.
	short := chartest.Rule{Category: "S", Pattern: regexp.MustCompile(`ab`)}
	long := chartest.Rule{Category: "L", Pattern: regexp.MustCompile(`abcd`)}
	c := chartest.NewCanonicalizer(short, long)
	got := string(c.Canonicalize([]byte("abcd")))
	if got != "L-1" {
		t.Fatalf("got %q, want L-1 (longer match wins)", got)
	}
}
