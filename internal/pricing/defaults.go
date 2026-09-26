package pricing

// DefaultPricings returns the package-shipped default pricing entries.
//
// These are best-effort published rates as of LastVerified; they are
// decision-support only and operators are expected to override stale or
// inaccurate entries via [[pricing]] in city.toml or pack.toml.
//
// The non-goal stated in #1255 ("a hard-coded Go pricing table") refers to
// the model where rates can only be updated by shipping a new release.
// These defaults exist as a bootstrap so cost estimates work out of the box;
// users override via config without waiting on a release.
//
// Returned slice is freshly allocated; callers may mutate.
func DefaultPricings() []ModelPricing {
	out := make([]ModelPricing, len(claudeDefaults))
	copy(out, claudeDefaults)
	return out
}

// claudeDefaults captures Anthropic's published Claude API rates.
//
// LastVerified is set conservatively; consumers should warn when entries
// exceed a configured staleness threshold. Cache-creation rates use the
// 5-minute (1.25× prompt) tier since that's the controller-default cache
// behavior in agent loops.
//
// See: https://www.anthropic.com/pricing
var claudeDefaults = []ModelPricing{
	// Claude 3 Opus (legacy).
	{
		Provider:     "claude",
		Model:        "claude-3-opus-20240229",
		LastVerified: "2026-04-25",
		Tier: Tier{
			PromptUSDPer1M:        15.00,
			CompletionUSDPer1M:    75.00,
			CacheReadUSDPer1M:     1.50,
			CacheCreationUSDPer1M: 18.75,
		},
	},
	// Claude 3.5 Sonnet (legacy, still common).
	{
		Provider:     "claude",
		Model:        "claude-3-5-sonnet-20241022",
		LastVerified: "2026-04-25",
		Tier: Tier{
			PromptUSDPer1M:        3.00,
			CompletionUSDPer1M:    15.00,
			CacheReadUSDPer1M:     0.30,
			CacheCreationUSDPer1M: 3.75,
		},
	},
	// Claude 3.5 Haiku.
	{
		Provider:     "claude",
		Model:        "claude-3-5-haiku-20241022",
		LastVerified: "2026-04-25",
		Tier: Tier{
			PromptUSDPer1M:        0.80,
			CompletionUSDPer1M:    4.00,
			CacheReadUSDPer1M:     0.08,
			CacheCreationUSDPer1M: 1.00,
		},
	},
	// Claude 4 Opus.
	{
		Provider:     "claude",
		Model:        "claude-opus-4",
		LastVerified: "2026-04-25",
		Tier: Tier{
			PromptUSDPer1M:        15.00,
			CompletionUSDPer1M:    75.00,
			CacheReadUSDPer1M:     1.50,
			CacheCreationUSDPer1M: 18.75,
		},
	},
	// Claude 4.6 Sonnet.
	{
		Provider:     "claude",
		Model:        "claude-sonnet-4-6",
		LastVerified: "2026-04-25",
		Tier: Tier{
			PromptUSDPer1M:        3.00,
			CompletionUSDPer1M:    15.00,
			CacheReadUSDPer1M:     0.30,
			CacheCreationUSDPer1M: 3.75,
		},
	},
	// Claude 4.7 Opus.
	{
		Provider:     "claude",
		Model:        "claude-opus-4-7",
		LastVerified: "2026-05-09",
		Tier: Tier{
			PromptUSDPer1M:        5.00,
			CompletionUSDPer1M:    25.00,
			CacheReadUSDPer1M:     0.50,
			CacheCreationUSDPer1M: 6.25,
		},
	},
	// Claude 4.8 Opus. Regular usage pricing is unchanged from Opus 4.7.
	{
		Provider:     "claude",
		Model:        "claude-opus-4-8",
		LastVerified: "2026-05-28",
		Tier: Tier{
			PromptUSDPer1M:        5.00,
			CompletionUSDPer1M:    25.00,
			CacheReadUSDPer1M:     0.50,
			CacheCreationUSDPer1M: 6.25,
		},
	},
	// Claude 4.5 Haiku.
	{
		Provider:     "claude",
		Model:        "claude-haiku-4-5-20251001",
		LastVerified: "2026-05-09",
		Tier: Tier{
			PromptUSDPer1M:        1.00,
			CompletionUSDPer1M:    5.00,
			CacheReadUSDPer1M:     0.10,
			CacheCreationUSDPer1M: 1.25,
		},
	},
	// Claude 5 Opus. Regular usage pricing is unchanged from Opus 4.7/4.8.
	{
		Provider:     "claude",
		Model:        "claude-opus-5",
		LastVerified: "2026-09-15",
		Tier: Tier{
			PromptUSDPer1M:        5.00,
			CompletionUSDPer1M:    25.00,
			CacheReadUSDPer1M:     0.50,
			CacheCreationUSDPer1M: 6.25,
		},
	},
	// Claude 5 Sonnet.
	{
		Provider:     "claude",
		Model:        "claude-sonnet-5",
		LastVerified: "2026-09-15",
		Tier: Tier{
			PromptUSDPer1M:        2.00,
			CompletionUSDPer1M:    10.00,
			CacheReadUSDPer1M:     0.20,
			CacheCreationUSDPer1M: 2.50,
		},
	},
	// Claude Fable 5.
	{
		Provider:     "claude",
		Model:        "claude-fable-5",
		LastVerified: "2026-09-15",
		Tier: Tier{
			PromptUSDPer1M:        10.00,
			CompletionUSDPer1M:    50.00,
			CacheReadUSDPer1M:     1.00,
			CacheCreationUSDPer1M: 12.50,
		},
	},
	// Claude Fable 5.1. Same prompt/completion tier as Fable 5, but cache
	// reads are published at $0.25/MTok rather than the 0.1x prompt rate the
	// rest of the family uses — see the ratio note in defaults_test.go.
	{
		Provider:     "claude",
		Model:        "claude-fable-5-1",
		LastVerified: "2026-09-15",
		Tier: Tier{
			PromptUSDPer1M:        10.00,
			CompletionUSDPer1M:    50.00,
			CacheReadUSDPer1M:     0.25,
			CacheCreationUSDPer1M: 12.50,
		},
	},
}
