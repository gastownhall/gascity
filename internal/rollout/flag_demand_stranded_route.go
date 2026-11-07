package rollout

import "github.com/gastownhall/gascity/internal/config"

// KeyDemandStrandedRoutePolicy is the exported registry Key for the stranded
// routed-demand policy gate, so composition-root code (cmd/gc, internal/api)
// can reference the gate without re-hardcoding the dotted string or matching
// it back out of the registry by a coincidental axis.
// keyDemandStrandedRoutePolicy is the package-internal spelling used
// throughout the resolver and registry.
const KeyDemandStrandedRoutePolicy = "demand.stranded_route_policy"

const keyDemandStrandedRoutePolicy = KeyDemandStrandedRoutePolicy

// envDemandStrandedRoutePolicy is the single source of truth for this gate's
// env override name: the registry Spec.EnvOverride, the resolver, and the
// testenv.LeakVectorVars membership test all reference it, so the three can
// never drift into a silent break-glass no-op.
const envDemandStrandedRoutePolicy = "GC_DEMAND_STRANDED_ROUTE_POLICY"

// StrandedRoutedDemand returns the resolved demand.stranded_route_policy mode.
func (f Flags) StrandedRoutedDemand() Mode {
	return f.strandedRoutedDemand.value
}

// WithStrandedRoutedDemand overrides demand.stranded_route_policy on a
// ForTest Flags value.
func WithStrandedRoutedDemand(m Mode) ForTestOption {
	return func(b *flagsBuilder) {
		b.flags.strandedRoutedDemand = resolved[Mode]{value: m, origin: OriginConfig}
	}
}

// readDemandStrandedRoutePolicy returns the raw config spelling for the gate
// and whether the merged config set it (empty string = unset, since the field
// is omitempty).
func readDemandStrandedRoutePolicy(cfg *config.City) (raw string, defined bool) {
	raw = cfg.Demand.StrandedRoutePolicy
	return raw, raw != ""
}
