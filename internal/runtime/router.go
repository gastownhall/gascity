package runtime

// Router is implemented by composite providers (auto, hybrid) that serve each
// session from one of several backends.
type Router interface {
	RouteFor(name string) Route
}

// Route names the backend serving a session: one of the composite's
// [BackendsProvider.Backends], so its label is the one the per-backend listing
// uses. Its Provider is never nil.
type Route struct {
	Backend
	// Known is false when the composite cannot vouch for the route, for
	// example an auto provider whose route table was not yet seeded from
	// the session beads.
	Known bool
}

// ResolveBackend follows nested Routers (auto may wrap hybrid) to the leaf
// backend serving name. labels lists each hop's route label, outermost first;
// a provider that is not a Router is its own leaf with no labels. known is
// false if any hop is not Known.
func ResolveBackend(sp Provider, name string) (leaf Provider, labels []string, known bool) {
	known = true
	for {
		router, ok := sp.(Router)
		if !ok {
			return sp, labels, known
		}
		route := router.RouteFor(name)
		labels = append(labels, route.Label)
		known = known && route.Known
		sp = route.Provider
	}
}

// CapabilitiesFor returns the Capabilities of the leaf backend serving name,
// not the composite's intersection. ok mirrors ResolveBackend's known.
func CapabilitiesFor(sp Provider, name string) (caps ProviderCapabilities, ok bool) {
	leaf, _, known := ResolveBackend(sp, name)
	return leaf.Capabilities(), known
}

// SleepCapabilityFromCapabilities derives the idle sleep capability of a
// provider that does not report one itself: Full needs both activity and
// attachment reports, TimedOnly needs activity, and anything less is Disabled.
func SleepCapabilityFromCapabilities(caps ProviderCapabilities) SessionSleepCapability {
	switch {
	case caps.CanReportActivity && caps.CanReportAttachment:
		return SessionSleepCapabilityFull
	case caps.CanReportActivity:
		return SessionSleepCapabilityTimedOnly
	default:
		return SessionSleepCapabilityDisabled
	}
}
