package config

import (
	"testing"

	"github.com/BurntSushi/toml"
)

// TestProviderSpec_RateLimitPatterns_Parsed verifies that a provider section
// with rate_limit_patterns parses the string slice correctly from TOML.
func TestProviderSpec_RateLimitPatterns_Parsed(t *testing.T) {
	input := `
[providers.claude]
command = "claude"
rate_limit_patterns = ["rate limit exceeded", "429 Too Many Requests"]
`
	var cfg City
	if _, err := toml.Decode(input, &cfg); err != nil {
		t.Fatalf("toml.Decode: %v", err)
	}
	spec, ok := cfg.Providers["claude"]
	if !ok {
		t.Fatal("provider 'claude' not found in parsed config")
	}
	if len(spec.RateLimitPatterns) != 2 {
		t.Fatalf("len(RateLimitPatterns) = %d, want 2", len(spec.RateLimitPatterns))
	}
	if spec.RateLimitPatterns[0] != "rate limit exceeded" {
		t.Errorf("RateLimitPatterns[0] = %q, want %q", spec.RateLimitPatterns[0], "rate limit exceeded")
	}
	if spec.RateLimitPatterns[1] != "429 Too Many Requests" {
		t.Errorf("RateLimitPatterns[1] = %q, want %q", spec.RateLimitPatterns[1], "429 Too Many Requests")
	}
}

// TestResolveProvider_CarriesPatterns verifies that RateLimitPatterns from
// the ProviderSpec are carried through to the ResolvedProvider after resolution.
func TestResolveProvider_CarriesPatterns(t *testing.T) {
	agent := &Agent{Name: "worker", Provider: "myai"}
	cityProviders := map[string]ProviderSpec{
		"myai": {
			Command:           "myai",
			PromptMode:        "arg",
			RateLimitPatterns: []string{"rate limit", "quota exceeded"},
		},
	}
	rp, err := ResolveProvider(agent, nil, cityProviders, lookPathOnly("myai"))
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if len(rp.RateLimitPatterns) != 2 {
		t.Fatalf("len(RateLimitPatterns) = %d, want 2", len(rp.RateLimitPatterns))
	}
	if rp.RateLimitPatterns[0] != "rate limit" {
		t.Errorf("RateLimitPatterns[0] = %q, want %q", rp.RateLimitPatterns[0], "rate limit")
	}
	if rp.RateLimitPatterns[1] != "quota exceeded" {
		t.Errorf("RateLimitPatterns[1] = %q, want %q", rp.RateLimitPatterns[1], "quota exceeded")
	}
}

// TestProviderSpec_EmptyPatterns verifies that an explicit empty array
// rate_limit_patterns = [] parses as an empty (non-nil) slice.
func TestProviderSpec_EmptyPatterns(t *testing.T) {
	input := `
[providers.claude]
command = "claude"
rate_limit_patterns = []
`
	var cfg City
	if _, err := toml.Decode(input, &cfg); err != nil {
		t.Fatalf("toml.Decode: %v", err)
	}
	spec := cfg.Providers["claude"]
	if spec.RateLimitPatterns == nil {
		t.Fatal("RateLimitPatterns is nil, want non-nil empty slice")
	}
	if len(spec.RateLimitPatterns) != 0 {
		t.Errorf("len(RateLimitPatterns) = %d, want 0", len(spec.RateLimitPatterns))
	}
}

// TestProviderSpec_NoPatterns verifies backward compatibility: when the
// rate_limit_patterns field is absent from TOML, the field is nil/empty.
func TestProviderSpec_NoPatterns(t *testing.T) {
	input := `
[providers.claude]
command = "claude"
`
	var cfg City
	if _, err := toml.Decode(input, &cfg); err != nil {
		t.Fatalf("toml.Decode: %v", err)
	}
	spec := cfg.Providers["claude"]
	if len(spec.RateLimitPatterns) != 0 {
		t.Errorf("len(RateLimitPatterns) = %d, want 0 (absent field should be zero-value)", len(spec.RateLimitPatterns))
	}
}
