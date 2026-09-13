package config

import (
	"testing"

	"github.com/gastownhall/gascity/internal/pricing"
)

func TestPricingRegistryNilConfig(t *testing.T) {
	var cfg *City
	if got := cfg.PricingRegistry(); got != nil {
		t.Fatalf("(*City)(nil).PricingRegistry() = %v, want nil", got)
	}
}

func TestPricingRegistryCityLayerWins(t *testing.T) {
	cfg := &City{
		CityPricing: []pricing.ModelPricing{{
			Provider:     "claude",
			Model:        "claude-opus-4-7",
			LastVerified: "2026-06-01",
			Tier:         pricing.Tier{PromptUSDPer1M: 1.23},
		}},
	}
	registry := cfg.PricingRegistry()
	if registry == nil {
		t.Fatal("PricingRegistry = nil, want registry")
	}
	entry, ok := registry.Lookup("claude", "claude-opus-4-7")
	if !ok {
		t.Fatal("Lookup(claude, claude-opus-4-7) = false, want city override")
	}
	if entry.Tier.PromptUSDPer1M != 1.23 {
		t.Fatalf("PromptUSDPer1M = %v, want city-layer 1.23", entry.Tier.PromptUSDPer1M)
	}
	if entry.Source != string(pricing.LayerCity) {
		t.Fatalf("Source = %q, want %q", entry.Source, pricing.LayerCity)
	}
}

func TestPricingRegistryNonComposedPricingFallsBackToCityLayer(t *testing.T) {
	// Non-composed loads fill cfg.Pricing only; PackPricing and CityPricing
	// stay empty, so Pricing stands in as the city layer.
	cfg := &City{
		Pricing: []pricing.ModelPricing{{
			Provider:     "claude",
			Model:        "claude-opus-4-7",
			LastVerified: "2026-06-01",
			Tier:         pricing.Tier{PromptUSDPer1M: 4.56},
		}},
	}
	registry := cfg.PricingRegistry()
	if registry == nil {
		t.Fatal("PricingRegistry = nil, want registry")
	}
	entry, ok := registry.Lookup("claude", "claude-opus-4-7")
	if !ok {
		t.Fatal("Lookup(claude, claude-opus-4-7) = false, want entry")
	}
	if entry.Tier.PromptUSDPer1M != 4.56 {
		t.Fatalf("PromptUSDPer1M = %v, want non-composed city-layer 4.56", entry.Tier.PromptUSDPer1M)
	}
	// Defaults still answer for models the city does not override.
	if _, ok := registry.Lookup("claude", "claude-opus-4-8"); !ok {
		t.Fatal("Lookup(claude, claude-opus-4-8) = false, want shipped default")
	}
}

func TestPricingRegistryComposedPackOnlyStaysInPackLayer(t *testing.T) {
	// Composed loads where pack.toml defines [pricing] but city.toml does
	// not: compose fills PackPricing and merges the entries into Pricing,
	// leaving CityPricing empty. The pack entries must NOT be promoted into
	// the city layer (their Source stays "pack").
	pack := []pricing.ModelPricing{{
		Provider:     "claude",
		Model:        "claude-opus-4-7",
		LastVerified: "2026-06-01",
		Tier:         pricing.Tier{PromptUSDPer1M: 7.89},
	}}
	cfg := &City{
		Pricing:     append([]pricing.ModelPricing(nil), pack...),
		PackPricing: pack,
	}
	registry := cfg.PricingRegistry()
	if registry == nil {
		t.Fatal("PricingRegistry = nil, want registry")
	}
	entry, ok := registry.Lookup("claude", "claude-opus-4-7")
	if !ok {
		t.Fatal("Lookup(claude, claude-opus-4-7) = false, want pack entry")
	}
	if entry.Tier.PromptUSDPer1M != 7.89 {
		t.Fatalf("PromptUSDPer1M = %v, want pack-layer 7.89", entry.Tier.PromptUSDPer1M)
	}
	if entry.Source != string(pricing.LayerPack) {
		t.Fatalf("Source = %q, want %q (pack entries must not be mislabeled as city overrides)", entry.Source, pricing.LayerPack)
	}
}
