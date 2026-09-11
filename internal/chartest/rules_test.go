package chartest_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/chartest"
)

func TestDefaultRules_CanonicalizesMintedNotStable(t *testing.T) {
	c := chartest.NewCanonicalizer(chartest.DefaultRules()...)

	// Volatile minted tokens ARE canonicalized.
	if got := string(c.Canonicalize([]byte("root gc-1 dep gc-42 again gc-1"))); got != "root BEAD-1 dep BEAD-2 again BEAD-1" {
		t.Errorf("minted bead ids: got %q", got)
	}
	ts := string(c.Canonicalize([]byte("a 2026-07-08T12:00:00Z b 2026-07-08T05:00:00.5-07:00")))
	if ts != "a T-1 b T-2" {
		t.Errorf("timestamps (Z + offset + frac): got %q", ts)
	}
}

func TestDefaultRules_ZeroTimeIsDistinctFromRealTime(t *testing.T) {
	// A real timestamp and the "unset" zero sentinel must NOT collapse to the
	// same placeholder — else a real->zero (dropped-field) regression hides.
	c := chartest.NewCanonicalizer(chartest.DefaultRules()...)
	got := string(c.Canonicalize([]byte(`created=2026-07-08T12:00:00Z updated=0001-01-01T00:00:00Z`)))
	if got != "created=T-1 updated=TZERO-1" {
		t.Errorf("zero-time not distinguished: got %q", got)
	}
}

func TestDefaultRules_LeavesStableIdentifiersAlone(t *testing.T) {
	c := chartest.NewCanonicalizer(chartest.DefaultRules()...)
	for _, stable := range []string{
		"gc",              // binary name
		"gc-hosted",       // stable component name
		"gc-runtime",      // stable component name
		"mol-adopt-pr-v2", // formula name
		"gcg-run-root",    // stable graph anchor
		`"schema_version":"1"`,
		"Total: 1",
		"gc.idem.request_id", // metadata key (stable)
	} {
		if got := string(c.Canonicalize([]byte(stable))); got != stable {
			t.Errorf("stable %q was wrongly canonicalized to %q", stable, got)
		}
	}
}
