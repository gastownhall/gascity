package storeref

// The relic census: does this binding still hold a bead that only a probe can
// find?
//
// A class migration copies rows across with their ORIGINAL ids
// (beads.ForeignIDCreator), so a converged city's binding holds a population of
// work-shaped ids no reader can locate from the id alone. That population is
// the whole reason ClassBinding.HasLegacyResidents exists, and it is a
// LIVE-STATE question, not a config or manifest one: the migration manifest
// records what was copied, not what is still open, and relics close over the
// following weeks. A recorded number would be stale the day after it was
// written — "no status files — query live state".
//
// So the answer is one read of the binding, taken once per process, and it is
// deliberately allowed to be wrong in exactly one direction. Reporting relics
// that have since closed keeps a probe one process longer. Reporting clean when
// the read failed retires the probe over beads that are still there.
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
// across under their original ids.
//
// Closed beads are excluded because the retirement blocker is a bead that can
// still be READ BY ID; counting a city's whole history would pin the probe
// forever. Both tiers are read, because a wisp carried across is as unfindable
// as an issue.
//
// The ids come back sorted so an operator report and a boot verdict describe
// the same binding the same way twice.
func OpenLegacyResidents(store beads.Store, prefixes []string) ([]string, error) {
	if store == nil {
		return nil, fmt.Errorf("relic census: no binding store to read")
	}
	rows, err := store.List(beads.ListQuery{TierMode: beads.FederatedReadTier, AllowScan: true})
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

// HasOpenLegacyResidents is the boot-time verdict form of the census: the value
// ClassBinding.HasLegacyResidents is allowed to be set from.
//
// A census that failed answers TRUE. An unread binding has said nothing about
// its residents, and "nothing" is not the claim that retires a probe — the
// refused city, whose store answers every read with the standing storage
// refusal, takes this branch too.
func HasOpenLegacyResidents(b ClassBinding) bool {
	relics, err := OpenLegacyResidents(b.Leg.Store, b.Prefixes)
	if err != nil {
		return true
	}
	return len(relics) > 0
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
