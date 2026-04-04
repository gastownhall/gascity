package workertest

// ProfileID is the canonical worker profile identifier.
type ProfileID string

const (
	ProfileClaudeTmuxCLI ProfileID = "claude/tmux-cli"
	ProfileCodexTmuxCLI  ProfileID = "codex/tmux-cli"
	ProfileGeminiTmuxCLI ProfileID = "gemini/tmux-cli"
)

// ProfileFixtureSet describes the provider-native fixture layouts for a profile.
type ProfileFixtureSet struct {
	FreshRoot        string
	ContinuationRoot string
	ResetRoot        string
}

// Profile identifies the worker profile and its phase-1 fixture bundle.
type Profile struct {
	ID       ProfileID
	Provider string
	WorkDir  string
	Fixtures ProfileFixtureSet
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
		},
	}
}
