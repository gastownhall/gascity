package storeref

// The relic census: does this binding still hold a bead that only a probe can
// find?
//
// A class migration copies rows across with their ORIGINAL ids
// (beads.ForeignIDCreator), so a converged city's binding holds a population of
// work-shaped ids no reader can locate from the id alone. That population is
// the whole reason ClassBinding.HasLegacyResidents exists, and it is a
// LIVE-STATE question, not a config or manifest one. The migration manifest
// records an ACT, not the binding's contents: it is absent on a city migrated
// by another build, incomplete on one restored from a backup, and silent about
// anything written to the binding by other means. Trusting it would retire a
// probe over beads that are sitting right there — "no status files — query
// live state".
//
// So the answer is one read of the binding, taken once per process, and it is
// deliberately allowed to be wrong in exactly one direction: reporting clean
// when the read failed would retire the probe over beads that are still there,
// so a failed read answers "relics".
//
// The verdict counts every resident, open or closed. Closing a relic does not
// make it unreachable — it is still shown, reopened, claimed and written by id,
// and `gc storage migrate` never deletes the work store's pre-migration copy —
// so a rule that retired the probe when the last relic CLOSED would send that
// id back to a frozen copy that reads OPEN forever (ga-qdt5y.19). Because
// nothing ever deletes a relic, the widened verdict is MONOTONE: a binding that
// has ever held a resident holds one for good. Staleness can therefore only
// keep a probe alive, never retire one, which is what makes reading it once per
// process sound rather than merely cheap.
//
// The once-per-process part rests on one premise: no relic ARRIVES while the
// process runs. That holds because the only thing that creates one is `gc
// storage migrate`, which refuses to run against a city with a live controller
// (cmd/gc/infra_class_migrate.go, guarded by
// TestEnsureInfraClassMigratedRefusesWhileAnotherControllerIsLive). If that
// refusal is ever relaxed, this verdict has to be recomputed on migrate rather
// than taken once at boot.

import (
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
)

// OpenLegacyResidents returns the ids of the OPEN beads store holds whose
// namespace none of prefixes claims — the relics a class migration carried
// across that an operator has not yet drained.
//
// This is the DRAIN REPORT's count, and open-only is what a drain report wants:
// it is the number an operator watches fall as the carried-across work closes.
// It is deliberately NOT the retirement condition — see LegacyResidents, which
// counts the closed ones too because they stay readable by id. A city can drain
// this count to zero and still, correctly, keep its residence probe forever.
//
// The ids come back sorted so two reports describe the same binding the same
// way twice.
func OpenLegacyResidents(store beads.Store, prefixes []string) ([]string, error) {
	return legacyResidents(store, prefixes, false)
}

// LegacyResidents returns the ids of EVERY bead store holds whose namespace
// none of prefixes claims, closed ones included — the population that can only
// be found by probing this binding.
//
// Both tiers are read, because a wisp carried across is as unfindable as an
// issue.
func LegacyResidents(store beads.Store, prefixes []string) ([]string, error) {
	return legacyResidents(store, prefixes, true)
}

func legacyResidents(store beads.Store, prefixes []string, includeClosed bool) ([]string, error) {
	if store == nil {
		return nil, fmt.Errorf("relic census: no binding store to read")
	}
	rows, err := store.List(beads.ListQuery{TierMode: beads.FederatedReadTier, AllowScan: true, IncludeClosed: includeClosed})
	if err != nil {
		return nil, fmt.Errorf("relic census: listing the binding: %w", err)
	}
	var relics []string
	for _, b := range rows {
		if idInAnyNamespace(b.ID, prefixes) {
			continue
		}
		relics = append(relics, b.ID)
	}
	sort.Strings(relics)
	return relics, nil
}

// HasLegacyResidents is the boot-time verdict form of the census: the value
// ClassBinding.HasLegacyResidents is allowed to be set from.
//
// A census that failed answers TRUE. An unread binding has said nothing about
// its residents, and "nothing" is not the claim that retires a probe — the
// refused city, whose store answers every read with the standing storage
// refusal, takes this branch too.
func HasLegacyResidents(b ClassBinding) bool {
	has, err := legacyResidentVerdict(b.Leg.Store, b.Prefixes)
	if err != nil {
		return true
	}
	return has
}

// legacyResidentVerdict answers the verdict from the store itself when the store
// can answer it, and by listing the store when it cannot.
//
// The verdict is a yes-or-no question over the binding's whole history, and the
// listing form pays for the history twice: once to hydrate every row, once to
// throw all but the id away. A store that can answer it in one statement is
// asked directly, and it is asked instead of the scan rather than as well as it
// — running both would make the fast path a pure cost.
//
// The fallback is not a legacy path. A binding served by the native Dolt engine
// reaches its rows through the beads library and holds no SQL of its own, so it
// cannot implement the capability and lists exactly as before.
//
// Discovery goes through beads.NamespaceCensusFor rather than a bare assertion
// on beads.NamespaceCensus, because the binding this runs against is not always
// a bare engine: cmd/gc's emitting class store is held to every engine method
// by TestEmittingClassStoreKeepsEveryEngineCapability, so it carries
// HasResidentOutside whatever it wraps, and a bare assertion would take that
// method's word for a native-Dolt-served binding and lose the scan the binding
// depends on.
//
// The drain report (OpenLegacyResidents) deliberately does not come through
// here: it needs the ids themselves, and a verdict cannot name them.
func legacyResidentVerdict(store beads.Store, prefixes []string) (bool, error) {
	if store == nil {
		return false, fmt.Errorf("relic census: no binding store to read")
	}
	if census, ok := beads.NamespaceCensusFor(store); ok {
		has, err := census.HasResidentOutside(prefixes)
		if err != nil {
			// Not a reason to fall back to the scan: a store whose own read
			// failed will not serve a listing of itself either, so the fallback
			// would only pay the cost twice on the way to the same answer. The
			// caller reads an error as "keep the probe", which is the honest
			// verdict for a binding that said nothing.
			return false, fmt.Errorf("relic census: asking the binding for a resident outside its namespaces: %w", err)
		}
		return has, nil
	}
	relics, err := LegacyResidents(store, prefixes)
	if err != nil {
		return false, err
	}
	return len(relics) > 0, nil
}

// idInAnyNamespace reports whether any prefix claims id's namespace. It is the
// one rule the census and ClassBinding.coversID both read ids by.
func idInAnyNamespace(id string, prefixes []string) bool {
	for _, p := range prefixes {
		if IDInNamespace(id, p) {
			return true
		}
	}
	return false
}
