package workertest

// ProfileID is the canonical worker profile identifier.
type ProfileID string

// revive:disable:exported
const ( //nolint:revive // exported profile IDs are documented by the enclosing type.
	// Profile* identify the canonical worker profiles used by conformance tests.
	ProfileClaudeTmuxCLI   ProfileID = "claude/tmux-cli"
	ProfileCodexTmuxCLI    ProfileID = "codex/tmux-cli"
	ProfileGeminiTmuxCLI   ProfileID = "gemini/tmux-cli"
	ProfileOpenCodeTmuxCLI ProfileID = "opencode/tmux-cli"
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

// Profile identifies the worker profile and its phase-1 fixture bundle.
type Profile struct {
	ID           ProfileID
	Provider     string
	WorkDir      string
	Fixtures     ProfileFixtureSet
	Continuation ContinuationOracle
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
		},
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
	}
}
