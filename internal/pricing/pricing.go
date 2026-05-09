// Package pricing defines the pricing seam used to estimate per-invocation
// LLM cost. It is the named policy seam introduced by issue #1255 (1d).
//
// Estimates are decision-support, not invoice reconciliation. Field names,
// CLI output, and dashboard headers should consistently label the result as
// an estimate.
//
// Layering (low → high precedence):
//
//  1. Defaults shipped with the package (DefaultPricings).
//  2. Pack-level overrides ([[pricing]] in pack.toml).
//  3. City-level overrides ([[pricing]] in city.toml).
//
// Lookups go (city → pack → default), returning the first match for a
// (provider, model) key.
package pricing

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// LastVerifiedLayout is the date format used in ModelPricing.LastVerified.
const LastVerifiedLayout = "2006-01-02"

// Tier defines per-token-type rates in USD per 1 million tokens.
//
// Token types are kept separate by design: Claude cache-read pricing is
// roughly 10× cheaper than prompt pricing, and cache-creation pricing is
// roughly 1.25× more expensive. Conflating them produces "badly wrong
// numbers" for any city using prompt caching, which is the common case.
type Tier struct {
	PromptUSDPer1M        float64 `toml:"prompt_usd_per_1m" json:"prompt_usd_per_1m"`
	CompletionUSDPer1M    float64 `toml:"completion_usd_per_1m" json:"completion_usd_per_1m"`
	CacheReadUSDPer1M     float64 `toml:"cache_read_usd_per_1m" json:"cache_read_usd_per_1m"`
	CacheCreationUSDPer1M float64 `toml:"cache_creation_usd_per_1m" json:"cache_creation_usd_per_1m"`
}

// IsZero reports whether t has no rates set.
func (t Tier) IsZero() bool {
	return t == Tier{}
}

// ModelPricing is a complete pricing entry for a (Provider, Model) pair.
//
// LastVerified is the date the rates were last confirmed against the
// provider's published pricing, in YYYY-MM-DD format. Stale entries can
// produce misleading cost estimates; consumers may emit warnings when
// LastVerified is older than a configured threshold.
type ModelPricing struct {
	// Provider is the LLM provider label (e.g. "claude", "codex", "gemini").
	Provider string `toml:"provider" json:"provider"`
	// Model is the provider-specific model identifier (e.g. "claude-opus-4-7").
	Model string `toml:"model" json:"model"`
	// Tier holds the per-token-type rates.
	Tier Tier `toml:"tier" json:"tier"`
	// LastVerified is the date these rates were confirmed (YYYY-MM-DD).
	LastVerified string `toml:"last_verified" json:"last_verified"`
	// Source is a runtime-only debug field naming the layer this entry
	// originated from ("default", "pack", "city"). Not parsed from TOML.
	Source string `toml:"-" json:"source,omitempty"`
}

// Usage is the token counts for a single invocation.
type Usage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens"`
}

// IsZero reports whether u has no token counts set.
func (u Usage) IsZero() bool {
	return u == Usage{}
}

// Estimate computes the USD cost of u given the rates in p.
// Returns 0 when p has no rates set; callers should consider treating a
// zero estimate from non-zero usage as missing pricing rather than free.
func (p ModelPricing) Estimate(u Usage) float64 {
	return float64(u.PromptTokens)*p.Tier.PromptUSDPer1M/1_000_000 +
		float64(u.CompletionTokens)*p.Tier.CompletionUSDPer1M/1_000_000 +
		float64(u.CacheReadTokens)*p.Tier.CacheReadUSDPer1M/1_000_000 +
		float64(u.CacheCreationTokens)*p.Tier.CacheCreationUSDPer1M/1_000_000
}

// IsZero reports whether p has no rates set.
func (p ModelPricing) IsZero() bool {
	return p.Tier.IsZero()
}

// IsStale reports whether p.LastVerified is empty, malformed, or older than
// threshold relative to now. A zero or negative threshold disables staleness
// checking and always returns false for well-formed dates.
func (p ModelPricing) IsStale(threshold time.Duration, now time.Time) bool {
	if strings.TrimSpace(p.LastVerified) == "" {
		return true
	}
	ts, err := time.Parse(LastVerifiedLayout, p.LastVerified)
	if err != nil {
		return true
	}
	if threshold <= 0 {
		return false
	}
	return now.Sub(ts) > threshold
}

// Key returns the canonical lookup key for (provider, model).
// Both components are normalized to lower case and trimmed.
func Key(provider, model string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + ":" +
		strings.ToLower(strings.TrimSpace(model))
}

// Validate checks that p has the minimum fields required for lookup.
func (p ModelPricing) Validate() error {
	if strings.TrimSpace(p.Provider) == "" {
		return fmt.Errorf("pricing entry missing provider")
	}
	if strings.TrimSpace(p.Model) == "" {
		return fmt.Errorf("pricing entry missing model (provider=%q)", p.Provider)
	}
	if p.Tier.PromptUSDPer1M < 0 ||
		p.Tier.CompletionUSDPer1M < 0 ||
		p.Tier.CacheReadUSDPer1M < 0 ||
		p.Tier.CacheCreationUSDPer1M < 0 {
		return fmt.Errorf("pricing entry %s/%s has negative rate", p.Provider, p.Model)
	}
	if strings.TrimSpace(p.LastVerified) != "" {
		if _, err := time.Parse(LastVerifiedLayout, p.LastVerified); err != nil {
			return fmt.Errorf("pricing entry %s/%s: invalid last_verified %q (want YYYY-MM-DD)",
				p.Provider, p.Model, p.LastVerified)
		}
	}
	return nil
}

// LayerName identifies one precedence layer in a Registry.
type LayerName string

const (
	// LayerDefault is the lowest-precedence layer; populated from
	// DefaultPricings at registry creation.
	LayerDefault LayerName = "default"
	// LayerPack is the pack-level override layer; populated from
	// pack.toml [[pricing]] entries during config compose.
	LayerPack LayerName = "pack"
	// LayerCity is the highest-precedence layer; populated from
	// city.toml [[pricing]] entries during config compose.
	LayerCity LayerName = "city"
)

// layerOrder lists layers in the order they are evaluated during Lookup
// (highest precedence first).
var layerOrder = []LayerName{LayerCity, LayerPack, LayerDefault}

// Registry holds ModelPricing entries across multiple precedence layers.
// Safe for concurrent use.
type Registry struct {
	mu     sync.RWMutex
	layers map[LayerName]map[string]ModelPricing
}

// New creates a Registry seeded with defaults at LayerDefault. Any malformed
// default entries are silently dropped — defaults are author-controlled and
// validated at package init.
func New(defaults []ModelPricing) *Registry {
	r := &Registry{layers: make(map[LayerName]map[string]ModelPricing)}
	r.SetLayer(LayerDefault, defaults)
	return r
}

// SetLayer replaces the contents of layer with entries. Entries with empty
// Provider or Model are skipped. Entries with negative rates are skipped.
func (r *Registry) SetLayer(layer LayerName, entries []ModelPricing) {
	r.mu.Lock()
	defer r.mu.Unlock()
	dst := make(map[string]ModelPricing, len(entries))
	for _, e := range entries {
		if e.Validate() != nil {
			continue
		}
		e.Source = string(layer)
		dst[Key(e.Provider, e.Model)] = e
	}
	r.layers[layer] = dst
}

// Lookup returns the highest-precedence ModelPricing for (provider, model)
// and true, or zero value and false if no layer has an entry.
func (r *Registry) Lookup(provider, model string) (ModelPricing, bool) {
	if strings.TrimSpace(provider) == "" || strings.TrimSpace(model) == "" {
		return ModelPricing{}, false
	}
	key := Key(provider, model)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, l := range layerOrder {
		if entries, ok := r.layers[l]; ok {
			if entry, ok := entries[key]; ok {
				return entry, true
			}
		}
	}
	return ModelPricing{}, false
}

// Estimate is a convenience wrapper that looks up pricing for (provider,
// model) and returns the cost estimate. Returns (0, false) if no entry
// matches; the bool distinguishes "no pricing data" from "zero usage".
func (r *Registry) Estimate(provider, model string, u Usage) (float64, bool) {
	p, ok := r.Lookup(provider, model)
	if !ok {
		return 0, false
	}
	return p.Estimate(u), true
}

// All returns every entry in the registry, flattened across layers, with
// higher-precedence entries shadowing lower ones. Returned slice is safe to
// modify by the caller; ordering is unspecified.
func (r *Registry) All() []ModelPricing {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]ModelPricing)
	for _, l := range layerOrder {
		entries, ok := r.layers[l]
		if !ok {
			continue
		}
		for k, v := range entries {
			if _, exists := seen[k]; !exists {
				seen[k] = v
			}
		}
	}
	out := make([]ModelPricing, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	return out
}

// Provider is an optional interface implemented by packages that ship
// default pricing data. The runtime can type-assert against this interface
// to discover provider-supplied pricing without coupling the core Provider
// contract to per-provider knowledge. Mirrors the optional-interface pattern
// used by IdleWaitProvider, DialogProvider in internal/runtime/runtime.go.
type Provider interface {
	// DefaultPricing returns the provider's published rates by model, with
	// LastVerified set to the date they were confirmed. The returned slice
	// is owned by the implementation; callers must not mutate.
	DefaultPricing() []ModelPricing
}
