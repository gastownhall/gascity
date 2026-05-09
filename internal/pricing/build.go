package pricing

// BuildRegistry composes a Registry from the standard precedence order:
//
//	defaults -> packPricings -> cityPricings
//
// (low to high precedence). Each input slice is set onto its layer; lookups
// flow highest precedence first. Returned Registry is safe for concurrent
// use.
//
// Pass nil for any layer to skip it (defaults still come from
// DefaultPricings()). For tests that want a clean registry with no defaults,
// call New(nil) directly.
func BuildRegistry(packPricings, cityPricings []ModelPricing) *Registry {
	r := New(DefaultPricings())
	if len(packPricings) > 0 {
		r.SetLayer(LayerPack, packPricings)
	}
	if len(cityPricings) > 0 {
		r.SetLayer(LayerCity, cityPricings)
	}
	return r
}

// CollectFromProviders type-asserts each input value against Provider
// and returns the union of their DefaultPricing() entries. Used by callers
// that want to seed a Registry from a list of provider plugins without
// hardcoding which ones implement the interface.
//
// Inputs that don't implement Provider are silently skipped — that's
// the whole point of the optional-interface pattern.
func CollectFromProviders(providers ...any) []ModelPricing {
	var out []ModelPricing
	for _, p := range providers {
		pp, ok := p.(Provider)
		if !ok {
			continue
		}
		out = append(out, pp.DefaultPricing()...)
	}
	return out
}
