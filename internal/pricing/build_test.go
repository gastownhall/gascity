package pricing

import (
	"testing"
)

func TestBuildRegistry_DefaultsOnly(t *testing.T) {
	r := BuildRegistry(nil, nil)
	if _, ok := r.Lookup("claude", "claude-opus-4-7"); !ok {
		t.Fatal("expected default Claude pricing in registry")
	}
}

func TestBuildRegistry_CityWinsOverDefaults(t *testing.T) {
	r := BuildRegistry(nil, []ModelPricing{
		{Provider: "claude", Model: "claude-opus-4-7", Tier: Tier{PromptUSDPer1M: 1.0}},
	})
	got, _ := r.Lookup("claude", "claude-opus-4-7")
	if got.Tier.PromptUSDPer1M != 1.0 {
		t.Errorf("city override lost: got %v want 1.0", got.Tier.PromptUSDPer1M)
	}
	if got.Source != string(LayerCity) {
		t.Errorf("source = %q, want %q", got.Source, LayerCity)
	}
}

func TestBuildRegistry_LayerOrder(t *testing.T) {
	pack := []ModelPricing{
		{Provider: "claude", Model: "x", Tier: Tier{PromptUSDPer1M: 5}},
	}
	city := []ModelPricing{
		{Provider: "claude", Model: "x", Tier: Tier{PromptUSDPer1M: 10}},
	}
	r := BuildRegistry(pack, city)
	got, _ := r.Lookup("claude", "x")
	if got.Tier.PromptUSDPer1M != 10 {
		t.Errorf("city should win: got %v", got.Tier.PromptUSDPer1M)
	}
}

func TestBuildRegistry_PackOnlyAddsModel(t *testing.T) {
	pack := []ModelPricing{
		{Provider: "novel", Model: "m", Tier: Tier{PromptUSDPer1M: 7}},
	}
	r := BuildRegistry(pack, nil)
	got, ok := r.Lookup("novel", "m")
	if !ok {
		t.Fatal("pack-only entry missing")
	}
	if got.Source != string(LayerPack) {
		t.Errorf("source = %q, want %q", got.Source, LayerPack)
	}
}

type stubProvider struct {
	rates []ModelPricing
}

func (s stubProvider) DefaultPricing() []ModelPricing { return s.rates }

func TestCollectFromProviders(t *testing.T) {
	a := stubProvider{rates: []ModelPricing{
		{Provider: "a", Model: "m", Tier: Tier{PromptUSDPer1M: 1}},
	}}
	b := stubProvider{rates: []ModelPricing{
		{Provider: "b", Model: "m", Tier: Tier{PromptUSDPer1M: 2}},
	}}
	notProvider := struct{ Foo string }{Foo: "skip me"}

	got := CollectFromProviders(a, notProvider, b)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 (non-provider must be skipped)", len(got))
	}
	providers := map[string]bool{}
	for _, p := range got {
		providers[p.Provider] = true
	}
	if !providers["a"] || !providers["b"] {
		t.Errorf("missing entries: %+v", providers)
	}
}

func TestCollectFromProviders_NoneImplement(t *testing.T) {
	got := CollectFromProviders(struct{}{}, "string", 42)
	if got != nil {
		t.Errorf("CollectFromProviders with no providers = %+v, want nil", got)
	}
}
