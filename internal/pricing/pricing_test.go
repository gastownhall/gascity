package pricing

import (
	"strings"
	"testing"
	"time"
)

func TestTierIsZero(t *testing.T) {
	if !(Tier{}).IsZero() {
		t.Fatal("zero Tier should report IsZero")
	}
	if (Tier{PromptUSDPer1M: 1}).IsZero() {
		t.Fatal("non-zero Tier should not report IsZero")
	}
}

func TestEstimate(t *testing.T) {
	p := ModelPricing{
		Provider: "claude",
		Model:    "claude-opus-4-7",
		Tier: Tier{
			PromptUSDPer1M:        15,
			CompletionUSDPer1M:    75,
			CacheReadUSDPer1M:     1.5,
			CacheCreationUSDPer1M: 18.75,
		},
	}
	tests := []struct {
		name string
		u    Usage
		want float64
	}{
		{"zero usage", Usage{}, 0},
		{
			"prompt only",
			Usage{PromptTokens: 1_000_000},
			15,
		},
		{
			"completion only",
			Usage{CompletionTokens: 1_000_000},
			75,
		},
		{
			"cache read only — distinct rate matters",
			Usage{CacheReadTokens: 1_000_000},
			1.5,
		},
		{
			"cache creation only",
			Usage{CacheCreationTokens: 1_000_000},
			18.75,
		},
		{
			"mixed usage",
			Usage{
				PromptTokens:        100_000,
				CompletionTokens:    50_000,
				CacheReadTokens:     200_000,
				CacheCreationTokens: 10_000,
			},
			// 0.1*15 + 0.05*75 + 0.2*1.5 + 0.01*18.75
			// = 1.5 + 3.75 + 0.3 + 0.1875 = 5.7375
			5.7375,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Estimate(tc.u)
			if !approxEqual(got, tc.want) {
				t.Errorf("Estimate(%+v) = %v, want %v", tc.u, got, tc.want)
			}
		})
	}
}

// TestEstimateCacheReadVsPromptDistinction guards the design intent behind
// keeping cache-read and prompt rates separate: the same prompt-token volume
// served from cache should produce a materially smaller estimate.
func TestEstimateCacheReadVsPromptDistinction(t *testing.T) {
	p := ModelPricing{
		Tier: Tier{
			PromptUSDPer1M:    3,    // Sonnet-ish
			CacheReadUSDPer1M: 0.30, // 10× cheaper, matching Claude pricing structure
		},
	}
	cold := p.Estimate(Usage{PromptTokens: 1_000_000})
	cached := p.Estimate(Usage{CacheReadTokens: 1_000_000})
	if cold == 0 || cached == 0 {
		t.Fatal("rates must produce non-zero estimates")
	}
	if !(cold > cached*5) {
		t.Errorf("cache-read estimate (%v) is not materially cheaper than prompt (%v)", cached, cold)
	}
}

func TestPricingIsZero(t *testing.T) {
	if !(ModelPricing{}).IsZero() {
		t.Fatal("zero ModelPricing should report IsZero")
	}
	withRates := ModelPricing{Tier: Tier{PromptUSDPer1M: 1}}
	if withRates.IsZero() {
		t.Fatal("ModelPricing with rates should not report IsZero")
	}
}

func TestIsStale(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		verified  string
		threshold time.Duration
		want      bool
	}{
		{"empty is stale", "", 90 * 24 * time.Hour, true},
		{"malformed is stale", "yesterday", 90 * 24 * time.Hour, true},
		{"recent is fresh", "2026-04-01", 90 * 24 * time.Hour, false},
		{"expired is stale", "2025-01-01", 90 * 24 * time.Hour, true},
		{"zero threshold disables check for valid date", "2020-01-01", 0, false},
		{"negative threshold disables check", "2020-01-01", -1 * time.Second, false},
		{"empty still stale even with zero threshold", "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ModelPricing{LastVerified: tc.verified}
			if got := p.IsStale(tc.threshold, now); got != tc.want {
				t.Errorf("IsStale(verified=%q, threshold=%v) = %v, want %v",
					tc.verified, tc.threshold, got, tc.want)
			}
		})
	}
}

func TestKey(t *testing.T) {
	cases := []struct {
		provider, model, want string
	}{
		{"claude", "claude-opus-4-7", "claude:claude-opus-4-7"},
		{"  CLAUDE  ", "  Claude-Opus-4-7  ", "claude:claude-opus-4-7"},
		{"", "model", ":model"},
	}
	for _, tc := range cases {
		if got := Key(tc.provider, tc.model); got != tc.want {
			t.Errorf("Key(%q, %q) = %q, want %q", tc.provider, tc.model, got, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		p       ModelPricing
		wantErr string
	}{
		{
			name: "valid",
			p: ModelPricing{
				Provider:     "claude",
				Model:        "claude-opus-4-7",
				LastVerified: "2026-01-15",
				Tier:         Tier{PromptUSDPer1M: 15},
			},
		},
		{
			name:    "missing provider",
			p:       ModelPricing{Model: "x"},
			wantErr: "missing provider",
		},
		{
			name:    "missing model",
			p:       ModelPricing{Provider: "claude"},
			wantErr: "missing model",
		},
		{
			name: "negative rate",
			p: ModelPricing{
				Provider: "claude",
				Model:    "x",
				Tier:     Tier{PromptUSDPer1M: -1},
			},
			wantErr: "negative rate",
		},
		{
			name: "malformed last_verified",
			p: ModelPricing{
				Provider:     "claude",
				Model:        "x",
				LastVerified: "April 25",
			},
			wantErr: "invalid last_verified",
		},
		{
			name: "no last_verified is fine",
			p: ModelPricing{
				Provider: "claude",
				Model:    "x",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestRegistryLookupDefaultLayer(t *testing.T) {
	defaults := []ModelPricing{
		{Provider: "claude", Model: "opus", Tier: Tier{PromptUSDPer1M: 15}},
		{Provider: "claude", Model: "sonnet", Tier: Tier{PromptUSDPer1M: 3}},
	}
	r := New(defaults)
	got, ok := r.Lookup("claude", "opus")
	if !ok {
		t.Fatal("expected to find opus in defaults")
	}
	if got.Tier.PromptUSDPer1M != 15 {
		t.Errorf("got rate %v, want 15", got.Tier.PromptUSDPer1M)
	}
	if got.Source != string(LayerDefault) {
		t.Errorf("got source %q, want %q", got.Source, LayerDefault)
	}
}

func TestRegistryLookupNormalization(t *testing.T) {
	defaults := []ModelPricing{
		{Provider: "claude", Model: "opus", Tier: Tier{PromptUSDPer1M: 15}},
	}
	r := New(defaults)
	if _, ok := r.Lookup("CLAUDE", "  OPUS  "); !ok {
		t.Fatal("Lookup should be case-insensitive and trim whitespace")
	}
}

func TestRegistryLookupMissing(t *testing.T) {
	r := New(nil)
	if _, ok := r.Lookup("claude", "ghost"); ok {
		t.Error("expected missing entry to return ok=false")
	}
	if _, ok := r.Lookup("", ""); ok {
		t.Error("empty provider/model must return ok=false")
	}
}

func TestRegistryLayerPrecedence(t *testing.T) {
	defaults := []ModelPricing{
		{Provider: "claude", Model: "opus", Tier: Tier{PromptUSDPer1M: 15}},
	}
	r := New(defaults)
	r.SetLayer(LayerPack, []ModelPricing{
		{Provider: "claude", Model: "opus", Tier: Tier{PromptUSDPer1M: 12}},
	})
	r.SetLayer(LayerCity, []ModelPricing{
		{Provider: "claude", Model: "opus", Tier: Tier{PromptUSDPer1M: 10}},
	})
	got, ok := r.Lookup("claude", "opus")
	if !ok {
		t.Fatal("expected to find opus")
	}
	if got.Tier.PromptUSDPer1M != 10 {
		t.Errorf("city should win: got %v, want 10", got.Tier.PromptUSDPer1M)
	}
	if got.Source != string(LayerCity) {
		t.Errorf("got source %q, want %q", got.Source, LayerCity)
	}
}

func TestRegistryLayerPackOverridesDefault(t *testing.T) {
	defaults := []ModelPricing{
		{Provider: "claude", Model: "opus", Tier: Tier{PromptUSDPer1M: 15}},
	}
	r := New(defaults)
	r.SetLayer(LayerPack, []ModelPricing{
		{Provider: "claude", Model: "opus", Tier: Tier{PromptUSDPer1M: 12}},
	})
	got, _ := r.Lookup("claude", "opus")
	if got.Tier.PromptUSDPer1M != 12 {
		t.Errorf("pack should win over default: got %v, want 12", got.Tier.PromptUSDPer1M)
	}
}

func TestRegistrySetLayerSkipsInvalid(t *testing.T) {
	r := New(nil)
	r.SetLayer(LayerCity, []ModelPricing{
		{Provider: "", Model: "x"},      // missing provider
		{Provider: "claude", Model: ""}, // missing model
		{Provider: "claude", Model: "opus", Tier: Tier{PromptUSDPer1M: -1}},
		{Provider: "claude", Model: "good", Tier: Tier{PromptUSDPer1M: 1}},
	})
	if _, ok := r.Lookup("claude", ""); ok {
		t.Error("invalid entry with empty model must not be registered")
	}
	if _, ok := r.Lookup("claude", "opus"); ok {
		t.Error("entry with negative rate must not be registered")
	}
	if _, ok := r.Lookup("claude", "good"); !ok {
		t.Error("valid entry should be registered")
	}
}

func TestRegistryEstimate(t *testing.T) {
	defaults := []ModelPricing{
		{Provider: "claude", Model: "opus", Tier: Tier{PromptUSDPer1M: 10}},
	}
	r := New(defaults)
	cost, ok := r.Estimate("claude", "opus", Usage{PromptTokens: 500_000})
	if !ok {
		t.Fatal("expected estimate ok")
	}
	if !approxEqual(cost, 5.0) {
		t.Errorf("got cost %v, want 5.0", cost)
	}
	if _, ok := r.Estimate("claude", "ghost", Usage{PromptTokens: 1}); ok {
		t.Error("missing pricing should return ok=false")
	}
}

func TestRegistryAll(t *testing.T) {
	r := New([]ModelPricing{
		{Provider: "claude", Model: "opus", Tier: Tier{PromptUSDPer1M: 15}},
		{Provider: "claude", Model: "sonnet", Tier: Tier{PromptUSDPer1M: 3}},
	})
	r.SetLayer(LayerCity, []ModelPricing{
		{Provider: "claude", Model: "opus", Tier: Tier{PromptUSDPer1M: 10}},
	})
	all := r.All()
	if len(all) != 2 {
		t.Fatalf("All() returned %d entries, want 2", len(all))
	}
	for _, p := range all {
		if p.Model == "opus" && p.Tier.PromptUSDPer1M != 10 {
			t.Errorf("All() should reflect higher-precedence rate for opus: got %v",
				p.Tier.PromptUSDPer1M)
		}
		if p.Model == "sonnet" && p.Tier.PromptUSDPer1M != 3 {
			t.Errorf("All() should keep default-only entry for sonnet: got %v",
				p.Tier.PromptUSDPer1M)
		}
	}
}

func TestRegistryConcurrentReadWrite(_ *testing.T) {
	r := New([]ModelPricing{
		{Provider: "claude", Model: "opus", Tier: Tier{PromptUSDPer1M: 15}},
	})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			r.SetLayer(LayerCity, []ModelPricing{
				{Provider: "claude", Model: "opus", Tier: Tier{PromptUSDPer1M: float64(i)}},
			})
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		_, _ = r.Lookup("claude", "opus")
	}
	<-done
}

// fakePricingProvider is a minimal Provider implementation used by
// the interface-conformance test below.
type fakePricingProvider struct{}

func (fakePricingProvider) DefaultPricing() []ModelPricing {
	return []ModelPricing{
		{Provider: "fake", Model: "m", Tier: Tier{PromptUSDPer1M: 1}},
	}
}

func TestProviderInterface(t *testing.T) {
	var p Provider = fakePricingProvider{}
	got := p.DefaultPricing()
	if len(got) != 1 || got[0].Model != "m" {
		t.Fatalf("DefaultPricing() = %+v, want one entry for fake/m", got)
	}
}

func approxEqual(a, b float64) bool {
	const epsilon = 1e-9
	if a-b > epsilon {
		return false
	}
	if b-a > epsilon {
		return false
	}
	return true
}
