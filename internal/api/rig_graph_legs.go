package api

// rigGraphRelicReporter is implemented by states that know the S4 relic-free
// predicate: whether a rig's own store is proven to hold no graph row any more
// because its graph rows were imported into the city binding or it never held
// any. A state that does not implement it keeps every rig leg, which is the
// pessimistic default: retiring a leg is the claim that needs evidence.
type rigGraphRelicReporter interface {
	RigGraphRelicFree(rig string) bool
}

// rigGraphLegRetired reports whether graph readers may skip rig's own store.
// It is true only on a split city whose state proves the rig relic-free; a
// single-store city never retires a rig leg, because there the rig store is
// where the rig's graph rows live. The workflow legs read the same bit from
// the epoch (storeref.Workflow, Leg.RelicFree); the order reads ask it here
// until T7f gives them legs.
func rigGraphLegRetired(state State, rig string) bool {
	reporter, ok := state.(rigGraphRelicReporter)
	if !ok || rig == "" {
		return false
	}
	return reporter.RigGraphRelicFree(rig)
}
