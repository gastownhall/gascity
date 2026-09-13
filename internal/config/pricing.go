package config

import "github.com/gastownhall/gascity/internal/pricing"

// PricingRegistry composes the invocation-cost pricing registry from the
// config's pricing layers (shipped defaults -> pack -> city). Composed loads
// fill PackPricing and CityPricing from their respective TOML layers
// (compose.go), so each layer keeps its own precedence and Source label.
// Non-composed loads fill only Pricing, so it stands in as the city layer —
// but only when PackPricing is also empty, otherwise a composed pack-only
// [pricing] table would be promoted into the city layer. Returns nil for a
// nil config; callers treat that as "use shipped defaults".
func (c *City) PricingRegistry() *pricing.Registry {
	if c == nil {
		return nil
	}
	city := c.CityPricing
	if len(city) == 0 && len(c.PackPricing) == 0 {
		city = c.Pricing
	}
	return pricing.BuildRegistry(c.PackPricing, city)
}
