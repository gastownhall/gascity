package workertest

// ProfileID is the canonical worker profile identifier.
type ProfileID string

// revive:disable:exported
const ( //nolint:revive // exported profile IDs are documented by the enclosing type.
	// Profile* identify the canonical worker profiles used by conformance tests.
	ProfileClaudeTmuxCLI      ProfileID = "claude/tmux-cli"
	ProfileCodexTmuxCLI       ProfileID = "codex/tmux-cli"
	ProfileGeminiTmuxCLI      ProfileID = "gemini/tmux-cli"
	ProfileKimiTmuxCLI        ProfileID = "kimi/tmux-cli"
	ProfileOpenCodeTmuxCLI    ProfileID = "opencode/tmux-cli"
	ProfileMimoCodeTmuxCLI    ProfileID = "mimocode/tmux-cli"
	ProfilePiTmuxCLI          ProfileID = "pi/tmux-cli"
	ProfileAntigravityTmuxCLI ProfileID = "antigravity/tmux-cli"
)

// revive:enable:exported

// ProfileFixtureSet describes the provider-native fixture layouts for a profile.
type ProfileFixtureSet struct {
	FreshRoot        string
	ContinuationRoot string
	ResetRoot        string
}

// ContinuationOracle defines the restart-sensitive recall proof for a profile.
type ContinuationOracle struct {
	AnchorText             string
	RecallPromptContains   string
	RecallResponseContains string
	ResetResponseContains  string
}

// UsageExpectation is the per-invocation token-usage contract a profile's
// fresh fixture must satisfy. Supported mirrors whether the worker registers
// an invocation-usage extractor for the profile's family
// (worker.InvocationUsageFamily); the token fields are the aggregate the
// matching extractor must yield across the fresh fixture's usage-bearing
// invocations. Unsupported families leave Supported false and the totals zero.
type UsageExpectation struct {
	Supported       bool
	Invocations     int
	InputTokens     int
	OutputTokens    int
	CacheReadTokens int
	// Model is the model identifier every usage-bearing invocation must carry
	// (the pricing lookup key). Empty for unsupported families.
	Model string
	// DefaultCostPriced reports whether pricing.DefaultPricings() has a rate
	// for (family, Model) — true for families with shipped default rates
	// (claude), false for families priced only by operator config
	// (codex).
	DefaultCostPriced bool
}

// Profile identifies the worker profile and its phase-1 fixture bundle.
type Profile struct {
	ID           ProfileID
	Provider     string
	WorkDir      string
	Fixtures     ProfileFixtureSet
	Continuation ContinuationOracle
	Usage        UsageExpectation
}

// Phase1Profiles returns the canonical phase-1 worker-core profiles.
func Phase1Profiles() []Profile {
	return []Profile{
		{
			ID:       ProfileClaudeTmuxCLI,
			Provider: "claude/tmux-cli",
			WorkDir:  "/tmp/gascity/phase1/claude",
			Fixtures: ProfileFixtureSet{
				FreshRoot:        "testdata/fixtures/claude/fresh",
				ContinuationRoot: "testdata/fixtures/claude/continuation",
				ResetRoot:        "testdata/fixtures/claude/reset",
			},
			Continuation: ContinuationOracle{
				AnchorText:             "Phase 1 covers transcript normalization and continuation semantics.",
				RecallPromptContains:   "Repeat the exact phase-1 summary from earlier before answering.",
				RecallResponseContains: "Phase 1 covers transcript normalization and continuation semantics.",
				ResetResponseContains:  "I cannot repeat the earlier summary because this is a fresh session.",
			},
			Usage: UsageExpectation{
				Supported:         true,
				Invocations:       1,
				InputTokens:       100,
				OutputTokens:      40,
				CacheReadTokens:   10,
				Model:             "claude-sonnet-4-6",
				DefaultCostPriced: true,
			},
		},
		// codex and gemini below are covered by the Phase 1 transcript/
		// continuation requirements and by the WC-STRUCT-* structured family
		// (see structured_conformance_test.go).
		{
			ID:       ProfileCodexTmuxCLI,
			Provider: "codex/tmux-cli",
			WorkDir:  "/tmp/gascity/phase1/codex",
			Fixtures: ProfileFixtureSet{
				FreshRoot:        "testdata/fixtures/codex/fresh",
				ContinuationRoot: "testdata/fixtures/codex/continuation",
				ResetRoot:        "testdata/fixtures/codex/reset",
			},
			Continuation: ContinuationOracle{
				AnchorText:             "The adapter reads provider transcripts into a canonical history.",
				RecallPromptContains:   "Repeat the exact adapter summary from earlier before answering.",
				RecallResponseContains: "The adapter reads provider transcripts into a canonical history.",
				ResetResponseContains:  "I cannot repeat the earlier adapter summary because this session started fresh.",
			},
			Usage: UsageExpectation{
				Supported:         true,
				Invocations:       1,
				InputTokens:       100,
				OutputTokens:      40,
				CacheReadTokens:   10,
				Model:             "gpt-5-codex",
				DefaultCostPriced: false,
			},
		},
		{
			ID:       ProfileGeminiTmuxCLI,
			Provider: "gemini/tmux-cli",
			WorkDir:  "/tmp/gascity/phase1/gemini",
			Fixtures: ProfileFixtureSet{
				FreshRoot:        "testdata/fixtures/gemini/fresh/tmp-root",
				ContinuationRoot: "testdata/fixtures/gemini/continuation/tmp-root",
				ResetRoot:        "testdata/fixtures/gemini/reset/tmp-root",
			},
			Continuation: ContinuationOracle{
				AnchorText:             "The fixture models normalized transcript history.",
				RecallPromptContains:   "Repeat the exact fixture summary from earlier before answering.",
				RecallResponseContains: "The fixture models normalized transcript history.",
				ResetResponseContains:  "I cannot repeat the earlier fixture summary because this chat is fresh.",
			},
			// gemini is deprecated and its invocation-usage extractor was
			// dropped (PR #3485): no Usage expectation, so WC-TX-USAGE-001 /
			// WC-USAGE-COST-001 report it Unsupported (out of scope), matching
			// worker.InvocationUsageFamily.
		},
		{
			ID:       ProfileKimiTmuxCLI,
			Provider: "kimi/tmux-cli",
			WorkDir:  "/tmp/gascity/phase1/kimi",
			Fixtures: ProfileFixtureSet{
				FreshRoot:        "testdata/fixtures/kimi/fresh",
				ContinuationRoot: "testdata/fixtures/kimi/continuation",
				ResetRoot:        "testdata/fixtures/kimi/reset",
			},
			Continuation: ContinuationOracle{
				AnchorText:             "Kimi phase 1 validates the native context JSONL transcript contract.",
				RecallPromptContains:   "Repeat the exact Kimi phase-1 summary from earlier before answering.",
				RecallResponseContains: "Kimi phase 1 validates the native context JSONL transcript contract.",
				ResetResponseContains:  "I cannot repeat the earlier Kimi summary because this session started fresh.",
			},
		},
		{
			ID:       ProfileOpenCodeTmuxCLI,
			Provider: "opencode/tmux-cli",
			WorkDir:  "/tmp/gascity/phase1/opencode",
			Fixtures: ProfileFixtureSet{
				FreshRoot:        "testdata/fixtures/opencode/fresh",
				ContinuationRoot: "testdata/fixtures/opencode/continuation",
				ResetRoot:        "testdata/fixtures/opencode/reset",
			},
			Continuation: ContinuationOracle{
				AnchorText:             "OpenCode phase 1 validates the tmux CLI transcript contract.",
				RecallPromptContains:   "Repeat the exact OpenCode phase-1 summary from earlier before answering.",
				RecallResponseContains: "OpenCode phase 1 validates the tmux CLI transcript contract.",
				ResetResponseContains:  "I cannot repeat the earlier OpenCode summary because this session started fresh.",
			},
		},
		{
			ID:       ProfileMimoCodeTmuxCLI,
			Provider: "mimocode/tmux-cli",
			WorkDir:  "/tmp/gascity/phase1/mimocode",
			Fixtures: ProfileFixtureSet{
				FreshRoot:        "testdata/fixtures/mimocode/fresh",
				ContinuationRoot: "testdata/fixtures/mimocode/continuation",
				ResetRoot:        "testdata/fixtures/mimocode/reset",
			},
			Continuation: ContinuationOracle{
				AnchorText:             "MiMo Code phase 1 validates the tmux CLI transcript contract.",
				RecallPromptContains:   "Repeat the exact MiMo Code phase-1 summary from earlier before answering.",
				RecallResponseContains: "MiMo Code phase 1 validates the tmux CLI transcript contract.",
				ResetResponseContains:  "I cannot repeat the earlier MiMo Code summary because this session started fresh.",
			},
		},
		{
			ID:       ProfilePiTmuxCLI,
			Provider: "pi/tmux-cli",
			WorkDir:  "/tmp/gascity/phase1/pi",
			Fixtures: ProfileFixtureSet{
				FreshRoot:        "testdata/fixtures/pi/fresh",
				ContinuationRoot: "testdata/fixtures/pi/continuation",
				ResetRoot:        "testdata/fixtures/pi/reset",
			},
			Continuation: ContinuationOracle{
				AnchorText:             "Pi phase 1 validates the tmux CLI transcript contract.",
				RecallPromptContains:   "Repeat the exact Pi phase-1 summary from earlier before answering.",
				RecallResponseContains: "Pi phase 1 validates the tmux CLI transcript contract.",
				ResetResponseContains:  "I cannot repeat the earlier Pi summary because this session started fresh.",
			},
		},
		{
			ID:       ProfileAntigravityTmuxCLI,
			Provider: "antigravity/tmux-cli",
			WorkDir:  "/tmp/gascity/phase1/antigravity",
			Fixtures: ProfileFixtureSet{
				FreshRoot:        "testdata/fixtures/antigravity/fresh/brain",
				ContinuationRoot: "testdata/fixtures/antigravity/continuation/brain",
				ResetRoot:        "testdata/fixtures/antigravity/reset/brain",
			},
			Continuation: ContinuationOracle{
				AnchorText:             "Antigravity phase 1 validates the agy trajectory transcript contract.",
				RecallPromptContains:   "Repeat the exact Antigravity phase-1 summary from earlier before answering.",
				RecallResponseContains: "Antigravity phase 1 validates the agy trajectory transcript contract.",
				ResetResponseContains:  "I cannot repeat the earlier Antigravity summary because this session started fresh.",
			},
		},
	}
}
