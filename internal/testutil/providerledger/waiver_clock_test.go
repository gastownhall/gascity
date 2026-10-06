package providerledger

import (
	"strings"
	"testing"
	"time"
)

func lapsedEntry(expires time.Time) Entry {
	return Entry{
		ID:           "runtime.builtin.example",
		Roles:        []Role{RoleProductionProvider},
		Port:         PortRuntimeProvider,
		Constructors: []SymbolRef{repoSymbol("internal/runtime/example", "NewSeamBacked")},
		Catalog:      runtimeCatalogRef("exact:example"),
		Claims: []ContractClaim{{
			Constructor: repoSymbol("internal/runtime/example", "NewSeamBacked"),
			Contract:    ContractRuntimeProvider,
			Disposition: DispositionWaived,
			Waiver: &Waiver{
				Owner:   "ga-example",
				Expires: expires,
				Reason:  "the production composition has no full shared runtime contract",
			},
		}},
	}
}

// TestValidateIsDateFree pins the split that keeps this package cacheable:
// Validate judges structure only, so its verdict on a given ledger cannot
// change when the calendar does. A long-lapsed waiver and one parked far out
// are both structurally sound; whether their dates are acceptable today is
// internal/testpolicy/waiverexpiry's question, never this package's.
func TestValidateIsDateFree(t *testing.T) {
	for _, expires := range []time.Time{
		time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC),
	} {
		if err := Validate([]Entry{lapsedEntry(expires)}); err != nil {
			t.Fatalf("Validate(waiver dated %s) = %v, want no date verdict from the structural check",
				expires.Format("2006-01-02"), err)
		}
	}
}

// TestValidateKeepsStructuralWaiverProblemsFatal is the other half of the
// split: a missing owner, reason, or date is a defect somebody committed, and
// it fails whoever committed it.
func TestValidateKeepsStructuralWaiverProblemsFatal(t *testing.T) {
	expires := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func(*Waiver)
		want   string
	}{
		{"missing owner", func(w *Waiver) { w.Owner = "" }, "waiver owner is required"},
		{"missing reason", func(w *Waiver) { w.Reason = "" }, "waiver reason is required"},
		{"missing expiry", func(w *Waiver) { w.Expires = time.Time{} }, "waiver expiry is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := lapsedEntry(expires)
			tc.mutate(entry.Claims[0].Waiver)
			if err := Validate([]Entry{entry}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

// TestExpiriesHandsEveryWaiverToTheClock checks the data the date check reads:
// one expiry per dated waiver, naming the claim and its owner, carrying the
// bounded horizon so a parked date is still caught.
func TestExpiriesHandsEveryWaiverToTheClock(t *testing.T) {
	expires := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	got := Expiries([]Entry{lapsedEntry(expires)})
	if len(got) != 1 {
		t.Fatalf("Expiries() = %+v, want exactly one", got)
	}
	if got[0].Owner != "ga-example" || !got[0].Expires.Equal(expires) || got[0].Horizon != maxWaiverHorizon {
		t.Fatalf("Expiries()[0] = %+v, want owner ga-example, expiry %s, horizon %s", got[0], expires.Format("2006-01-02"), maxWaiverHorizon)
	}
	if !strings.Contains(got[0].Label, `entry "runtime.builtin.example"`) ||
		!strings.Contains(got[0].Label, "internal/runtime/example.NewSeamBacked") {
		t.Fatalf("Expiries()[0].Label = %q, want the entry and constructor named", got[0].Label)
	}
}

// TestExpiriesSkipsStructurallyBrokenWaivers keeps one authoring mistake from
// becoming two findings: Validate already reports a waiver with no owner or no
// date, so the clock must not see it too.
func TestExpiriesSkipsStructurallyBrokenWaivers(t *testing.T) {
	expires := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	unowned := lapsedEntry(expires)
	unowned.Claims[0].Waiver.Owner = ""
	undated := lapsedEntry(time.Time{})
	proved := lapsedEntry(expires)
	proved.Claims[0].Waiver = nil
	if got := Expiries([]Entry{unowned, undated, proved}); len(got) != 0 {
		t.Fatalf("Expiries() = %+v, want none", got)
	}
}

// TestCatalogWaiversAllReachTheClock guards against a shipped waiver slipping
// past the date check: every waived claim in the catalog yields one expiry.
func TestCatalogWaiversAllReachTheClock(t *testing.T) {
	entries := Catalog()
	waived := 0
	for _, entry := range entries {
		for _, claim := range entry.Claims {
			if claim.Waiver != nil {
				waived++
			}
		}
	}
	if got := len(Expiries(entries)); got != waived {
		t.Fatalf("Expiries(Catalog()) = %d, want one per waived claim (%d)", got, waived)
	}
}
