package config

import (
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pricing"
)

func TestMergePricingByKey_EmptyBase(t *testing.T) {
	override := []pricing.ModelPricing{
		{Provider: "claude", Model: "opus", Tier: pricing.Tier{PromptUSDPer1M: 10}},
	}
	got := mergePricingByKey(nil, override)
	if len(got) != 1 || got[0].Tier.PromptUSDPer1M != 10 {
		t.Fatalf("got %+v, want copy of override", got)
	}
	got[0].Tier.PromptUSDPer1M = 99
	if override[0].Tier.PromptUSDPer1M == 99 {
		t.Fatal("merge result must not alias override slice")
	}
}

func TestMergePricingByKey_OverrideWins(t *testing.T) {
	base := []pricing.ModelPricing{
		{Provider: "claude", Model: "opus", Tier: pricing.Tier{PromptUSDPer1M: 15}},
		{Provider: "claude", Model: "sonnet", Tier: pricing.Tier{PromptUSDPer1M: 3}},
	}
	override := []pricing.ModelPricing{
		{Provider: "claude", Model: "opus", Tier: pricing.Tier{PromptUSDPer1M: 12}},
	}
	got := mergePricingByKey(base, override)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	for _, p := range got {
		if p.Model == "opus" && p.Tier.PromptUSDPer1M != 12 {
			t.Errorf("opus override lost: got %v want 12", p.Tier.PromptUSDPer1M)
		}
		if p.Model == "sonnet" && p.Tier.PromptUSDPer1M != 3 {
			t.Errorf("sonnet base dropped: got %v want 3", p.Tier.PromptUSDPer1M)
		}
	}
}

func TestMergePricingByKey_OverrideAddsNew(t *testing.T) {
	base := []pricing.ModelPricing{
		{Provider: "claude", Model: "opus", Tier: pricing.Tier{PromptUSDPer1M: 15}},
	}
	override := []pricing.ModelPricing{
		{Provider: "claude", Model: "sonnet", Tier: pricing.Tier{PromptUSDPer1M: 3}},
	}
	got := mergePricingByKey(base, override)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Model != "opus" {
		t.Errorf("base entry should come first: got order %+v", got)
	}
	if got[1].Model != "sonnet" {
		t.Errorf("override-only entry should come after base: got order %+v", got)
	}
}

func TestMergePricingByKey_DeduplicatesOverrideByLastKey(t *testing.T) {
	override := []pricing.ModelPricing{
		{Provider: "claude", Model: "opus", Tier: pricing.Tier{PromptUSDPer1M: 1}},
		{Provider: "claude", Model: "sonnet", Tier: pricing.Tier{PromptUSDPer1M: 3}},
		{Provider: "CLAUDE", Model: "OPUS", Tier: pricing.Tier{PromptUSDPer1M: 2}},
	}
	got := mergePricingByKey(nil, override)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 after duplicate override collapse: %+v", len(got), got)
	}
	if got[0].Model != "sonnet" {
		t.Fatalf("first surviving entry = %q, want sonnet: %+v", got[0].Model, got)
	}
	if got[1].Tier.PromptUSDPer1M != 2 {
		t.Fatalf("last opus override should win, got %+v", got[1])
	}
}

func TestMergePricingByKey_KeyIsCaseInsensitive(t *testing.T) {
	base := []pricing.ModelPricing{
		{Provider: "claude", Model: "opus", Tier: pricing.Tier{PromptUSDPer1M: 15}},
	}
	override := []pricing.ModelPricing{
		{Provider: "CLAUDE", Model: "OPUS", Tier: pricing.Tier{PromptUSDPer1M: 10}},
	}
	got := mergePricingByKey(base, override)
	if len(got) != 1 {
		t.Fatalf("case-mismatched override should still merge: got %d entries", len(got))
	}
	if got[0].Tier.PromptUSDPer1M != 10 {
		t.Errorf("override should win: got %v", got[0].Tier.PromptUSDPer1M)
	}
}

func TestLoadWithIncludes_PreservesPackAndCityPricingLayers(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[[pricing]]
provider = "claude"
model = "claude-opus-4-7"
[pricing.tier]
prompt_usd_per_1m = 10.0
`)
	fs.Files["/city/pack.toml"] = []byte(`
[pack]
name = "test"
schema = 2

[[pricing]]
provider = "claude"
model = "claude-opus-4-7"
[pricing.tier]
prompt_usd_per_1m = 15.0

[[pricing]]
provider = "claude"
model = "claude-sonnet-4-6"
[pricing.tier]
prompt_usd_per_1m = 3.0
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.PackPricing) != 2 {
		t.Fatalf("PackPricing = %+v, want two pack-layer entries", cfg.PackPricing)
	}
	if len(cfg.CityPricing) != 1 {
		t.Fatalf("CityPricing = %+v, want one city-layer entry", cfg.CityPricing)
	}

	r := pricing.BuildRegistry(cfg.PackPricing, cfg.CityPricing)
	got, ok := r.Lookup("claude", "claude-opus-4-7")
	if !ok {
		t.Fatal("opus pricing missing from registry")
	}
	if got.Tier.PromptUSDPer1M != 10.0 {
		t.Errorf("registry city override lost: %v", got.Tier.PromptUSDPer1M)
	}
	if got.Source != string(pricing.LayerCity) {
		t.Errorf("registry source = %q, want %q", got.Source, pricing.LayerCity)
	}
}

func TestParseCityWithPricing(t *testing.T) {
	in := `
[workspace]
name = "test"

[[pricing]]
provider = "claude"
model = "claude-opus-4-7"
last_verified = "2026-04-25"
[pricing.tier]
prompt_usd_per_1m = 14.0
completion_usd_per_1m = 70.0
cache_read_usd_per_1m = 1.4
cache_creation_usd_per_1m = 17.5

[[pricing]]
provider = "claude"
model = "claude-sonnet-4-6"
[pricing.tier]
prompt_usd_per_1m = 2.5
completion_usd_per_1m = 12.5
`
	cfg, _, _, err := parseWithMeta([]byte(in), "/tmp/city.toml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.Pricing) != 2 {
		t.Fatalf("expected 2 pricing entries, got %d", len(cfg.Pricing))
	}
	first := cfg.Pricing[0]
	if first.Provider != "claude" || first.Model != "claude-opus-4-7" {
		t.Errorf("first entry = %+v", first)
	}
	if first.Tier.PromptUSDPer1M != 14.0 {
		t.Errorf("first PromptUSDPer1M = %v want 14.0", first.Tier.PromptUSDPer1M)
	}
	if first.LastVerified != "2026-04-25" {
		t.Errorf("first LastVerified = %q", first.LastVerified)
	}
}

func TestLoadWithIncludes_CityOverridesPackPricing(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[[pricing]]
provider = "claude"
model = "claude-opus-4-7"
[pricing.tier]
prompt_usd_per_1m = 10.0
`)
	fs.Files["/city/pack.toml"] = []byte(`
[pack]
name = "test"
schema = 2

[[pricing]]
provider = "claude"
model = "claude-opus-4-7"
[pricing.tier]
prompt_usd_per_1m = 15.0

[[pricing]]
provider = "claude"
model = "claude-sonnet-4-6"
[pricing.tier]
prompt_usd_per_1m = 3.0
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.Pricing) != 2 {
		t.Fatalf("expected 2 pricing entries (city wins on opus, pack adds sonnet); got %d: %+v",
			len(cfg.Pricing), cfg.Pricing)
	}
	for _, p := range cfg.Pricing {
		switch p.Model {
		case "claude-opus-4-7":
			if p.Tier.PromptUSDPer1M != 10.0 {
				t.Errorf("city override lost: %v", p.Tier.PromptUSDPer1M)
			}
		case "claude-sonnet-4-6":
			if p.Tier.PromptUSDPer1M != 3.0 {
				t.Errorf("pack-only entry lost: %v", p.Tier.PromptUSDPer1M)
			}
		default:
			t.Errorf("unexpected entry %+v", p)
		}
	}
}

func TestParsePackWithPricing(t *testing.T) {
	in := `
[pack]
name = "test"
schema = 2

[[pricing]]
provider = "claude"
model = "claude-opus-4-7"
[pricing.tier]
prompt_usd_per_1m = 16.0
`
	cfg, _, err := parsePackConfigWithMeta([]byte(in), "/tmp/pack.toml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.Pricing) != 1 {
		t.Fatalf("expected 1 pricing entry, got %d", len(cfg.Pricing))
	}
	if cfg.Pricing[0].Tier.PromptUSDPer1M != 16.0 {
		t.Errorf("got %v want 16.0", cfg.Pricing[0].Tier.PromptUSDPer1M)
	}
}
