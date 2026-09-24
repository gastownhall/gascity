package providerledger

import (
	"sort"
	"testing"
	"time"
)

// runtimeWaiverExpiries collects the shipped runtime waivers keyed by the
// constructor whose gap they cover.
func runtimeWaiverExpiries(t *testing.T) map[string][]time.Time {
	t.Helper()
	byConstructor := map[string][]time.Time{}
	for _, entry := range Catalog() {
		for _, claim := range entry.Claims {
			if claim.Disposition != DispositionWaived || claim.Waiver == nil {
				continue
			}
			key := renderSymbolRef(claim.Constructor)
			byConstructor[key] = append(byConstructor[key], claim.Waiver.Expires)
		}
	}
	if len(byConstructor) == 0 {
		t.Fatal("Catalog() has no waived runtime claims; this test would pass vacuously")
	}
	return byConstructor
}

// TestRuntimeWaiverExpiriesDivergeByGap checks that the runtime waivers expire
// one gap at a time.
//
// Eight waivers sharing one date is eight waivers that lapse together, which is
// the 2026-08-26 incident: a single calendar day turns every Go-touching push in
// the fleet red at once. Giving each gap its own date turns that cliff into a
// queue, so the owner gets one failure to answer at a time and everyone else
// keeps working.
//
// The unit is the gap, not the row. Two waivers cover
// internal/runtime/t3bridge.NewSeamBacked — the exact: and legacy prefix: routes
// both select it — and one contract closes both, so they deliberately share a
// date. Splitting them would claim two pieces of work where there is one.
func TestRuntimeWaiverExpiriesDivergeByGap(t *testing.T) {
	byConstructor := runtimeWaiverExpiries(t)

	for constructor, expiries := range byConstructor {
		first := expiries[0]
		for _, expiry := range expiries[1:] {
			if !expiry.Equal(first) {
				t.Errorf("%s: waivers on one constructor expire %s and %s; one gap closes with one contract, so it takes one date",
					constructor, first.Format("2006-01-02"), expiry.Format("2006-01-02"))
			}
		}
	}

	owners := map[string]string{}
	for constructor, expiries := range byConstructor {
		day := expiries[0].Format("2006-01-02")
		if other, ok := owners[day]; ok {
			t.Errorf("%s and %s both expire %s; separate gaps need separate dates or they lapse as one cliff", other, constructor, day)
			continue
		}
		owners[day] = constructor
	}
}

// TestRuntimeWaiverExpiriesAreWholeDaysInOrder checks the staggered dates are
// clean whole UTC days and that the ledger actually spreads them out.
//
// A waiver dated to the middle of a day expires at an hour nobody can read off
// the rendered YYYY-MM-DD table, and a stagger that fits inside one week is a
// cliff with extra steps.
func TestRuntimeWaiverExpiriesAreWholeDaysInOrder(t *testing.T) {
	byConstructor := runtimeWaiverExpiries(t)

	var days []time.Time
	for constructor, expiries := range byConstructor {
		expiry := expiries[0]
		if !expiry.Equal(expiry.Truncate(24 * time.Hour)) {
			t.Errorf("%s: waiver expires %s, want a whole UTC day", constructor, expiry.Format(time.RFC3339))
		}
		days = append(days, expiry)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })

	const minGap = 5 * 24 * time.Hour
	for i := 1; i < len(days); i++ {
		if gap := days[i].Sub(days[i-1]); gap < minGap {
			t.Errorf("waivers expire %s and %s, %v apart; want at least %v so the owner answers one at a time",
				days[i-1].Format("2006-01-02"), days[i].Format("2006-01-02"), gap, minGap)
		}
	}
}
