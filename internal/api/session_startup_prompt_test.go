package api

import (
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// startupPromptState returns a fake state with one agent whose provider wraps
// another command, the shape the API resume resolver builds launch hints for.
func startupPromptState(t *testing.T) *fakeState {
	t.Helper()
	// The resolver LookPaths the provider's path_check; the test binary exists
	// on every OS, where "true" is not on a Windows PATH.
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", Provider: "wrapped"}},
		Providers: map[string]config.ProviderSpec{
			"wrapped": {
				DisplayName:       "Wrapped",
				Command:           "aimux",
				Args:              []string{"run", "gemini"},
				PathCheck:         exe,
				ReadyPromptPrefix: "> ",
			},
		},
	}
	return fs
}

func startupPromptSessionInfo() session.Info {
	return session.Info{
		ID:       "gc-1",
		Template: "worker",
		Command:  "aimux run gemini",
		Provider: "wrapped",
		WorkDir:  "/tmp/workdir",
	}
}
