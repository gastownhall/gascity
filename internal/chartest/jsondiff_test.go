package chartest_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/chartest"
)

func TestJSONShapeDiff_IdenticalMatches(t *testing.T) {
	if d := chartest.JSONShapeDiff([]byte(`{"a":1,"b":[1,2,3]}`), []byte(`{"a":1,"b":[1,2,3]}`), nil); d != "" {
		t.Fatalf("identical should match, got diff: %s", d)
	}
}

func TestJSONShapeDiff_ArrayOrderInsensitive(t *testing.T) {
	// The pilot's finding: gc list --json order is non-deterministic. List order
	// is not a behavioral contract → arrays compare as multisets.
	want := []byte(`{"convoys":[{"id":"BEAD-1"},{"id":"BEAD-2"}]}`)
	got := []byte(`{"convoys":[{"id":"BEAD-2"},{"id":"BEAD-1"}]}`)
	if d := chartest.JSONShapeDiff(want, got, nil); d != "" {
		t.Fatalf("reordered array should match as a multiset, got diff: %s", d)
	}
}

func TestJSONShapeDiff_ArrayContentDifferenceCaught(t *testing.T) {
	want := []byte(`[{"id":"BEAD-1"},{"id":"BEAD-2"}]`)
	got := []byte(`[{"id":"BEAD-1"},{"id":"BEAD-3"}]`)
	if d := chartest.JSONShapeDiff(want, got, nil); d == "" {
		t.Fatal("a genuinely different array element must be caught, even order-insensitively")
	}
}

func TestJSONShapeDiff_ArrayLengthDifferenceCaught(t *testing.T) {
	if d := chartest.JSONShapeDiff([]byte(`[1,2,3]`), []byte(`[1,2]`), nil); d == "" {
		t.Fatal("array length change must be caught")
	}
}

func TestJSONShapeDiff_ExtraKeyRejectedUnlessAllowed(t *testing.T) {
	want := []byte(`{"id":"BEAD-1"}`)
	got := []byte(`{"id":"BEAD-1","molecule_id":"BEAD-9"}`)
	if d := chartest.JSONShapeDiff(want, got, nil); d == "" {
		t.Fatal("an undeclared extra key must be a diff")
	}
	if d := chartest.JSONShapeDiff(want, got, []string{"molecule_id"}); d != "" {
		t.Fatalf("a declared-additive key must be allowed, got diff: %s", d)
	}
}

func TestJSONShapeDiff_MissingKeyCaught(t *testing.T) {
	// Additive allowlist covers EXTRA keys in got, never MISSING ones.
	want := []byte(`{"id":"BEAD-1","title":"x"}`)
	got := []byte(`{"id":"BEAD-1"}`)
	if d := chartest.JSONShapeDiff(want, got, []string{"title"}); d == "" {
		t.Fatal("a key present in want but missing in got must be a diff")
	}
}

func TestJSONShapeDiff_ValueMismatchCaught(t *testing.T) {
	if d := chartest.JSONShapeDiff([]byte(`{"n":1}`), []byte(`{"n":2}`), nil); d == "" {
		t.Fatal("a scalar value change must be caught")
	}
}

func TestJSONShapeDiff_AdditiveAppliesAtAnyDepth(t *testing.T) {
	want := []byte(`{"outer":{"id":"BEAD-1"}}`)
	got := []byte(`{"outer":{"id":"BEAD-1","extra":true}}`)
	if d := chartest.JSONShapeDiff(want, got, []string{"extra"}); d != "" {
		t.Fatalf("additive key nested in an object must be allowed, got: %s", d)
	}
}

func TestJSONShapeDiff_InvalidJSONReported(t *testing.T) {
	if d := chartest.JSONShapeDiff([]byte(`{`), []byte(`{}`), nil); d == "" {
		t.Fatal("invalid want JSON must be reported, not silently matched")
	}
}
