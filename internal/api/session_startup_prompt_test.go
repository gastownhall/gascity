package api

import (
	"errors"
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	startupPromptSentinelNudge  = "SENTINEL STARTUP PROMPT"
	startupPromptSentinelMarker = "GC_STARTUP_PROMPT_DELIVERED"
)

// startupPromptCall is what the API handed State for one resume launch.
type startupPromptCall struct {
	info      session.Info
	resolved  *config.ResolvedProvider
	transport string
	hints     runtime.Config
}

// startupPromptState returns a fake state with one agent and a hook that records
// each ApplyStartupPrompt call and answers with hints carrying a sentinel
// payload, so a test can see the API return exactly what State produced.
func startupPromptState(t *testing.T) (*fakeState, *[]startupPromptCall) {
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
	var calls []startupPromptCall
	fs.startupPromptFn = func(info session.Info, resolved *config.ResolvedProvider, transport string, hints runtime.Config) (runtime.Config, error) {
		calls = append(calls, startupPromptCall{info: info, resolved: resolved, transport: transport, hints: hints})
		hints.Nudge = startupPromptSentinelNudge
		hints.Env = map[string]string{startupPromptSentinelMarker: "1"}
		return hints, nil
	}
	return fs, &calls
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

// TestResolveWorkerSessionRuntimeAsksStateToDeliverTheStartupPrompt pins ga-4k4zfk
// for the resolver the API worker factory uses (worker_factory.go
// ResolveSessionRuntime), the only production source of the API's resume launch
// hints: they must come from State, which owns prompt rendering and the delivery
// policy, so a cold start carries the prompt and the delivered marker instead of
// leaving the SessionStart hook as the sole carrier. The resolver builds a handle
// for every single-session request, reads included, so it leaves the rendering
// to the launch (LaunchHints) rather than paying for it per request.
func TestResolveWorkerSessionRuntimeAsksStateToDeliverTheStartupPrompt(t *testing.T) {
	fs, calls := startupPromptState(t)
	srv := New(fs)

	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(startupPromptSessionInfo(), "", nil)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntimeWithMetadata: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntimeWithMetadata() = nil")
	}
	if len(*calls) != 0 {
		t.Fatalf("resolving the runtime called State.ApplyStartupPrompt %d times, want 0 until a launch", len(*calls))
	}
	if runtimeCfg.Hints.Nudge != "" {
		t.Errorf("resolved Hints.Nudge = %q, want the prompt left to the launch", runtimeCfg.Hints.Nudge)
	}
	if runtimeCfg.LaunchHints == nil {
		t.Fatal("resolved runtime has no LaunchHints: a launch would carry no startup prompt")
	}
	launch, err := runtimeCfg.LaunchHints(runtimeCfg.Hints)
	if err != nil {
		t.Fatalf("LaunchHints: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("State.ApplyStartupPrompt called %d times for one launch, want 1", len(*calls))
	}
	call := (*calls)[0]
	if call.info.Template != "worker" {
		t.Errorf("State saw template %q, want worker", call.info.Template)
	}
	if call.resolved == nil || call.resolved.Name != "wrapped" {
		t.Errorf("State saw resolved provider %+v, want the wrapped provider", call.resolved)
	}
	if call.hints.WorkDir != "/tmp/workdir" || call.hints.ReadyPromptPrefix != "> " {
		t.Errorf("State must receive the fully built hints, got WorkDir=%q ReadyPromptPrefix=%q", call.hints.WorkDir, call.hints.ReadyPromptPrefix)
	}
	if launch.Nudge != startupPromptSentinelNudge {
		t.Errorf("launch Nudge = %q, want what State returned", launch.Nudge)
	}
	if launch.Env[startupPromptSentinelMarker] != "1" {
		t.Errorf("launch Env = %v, want the marker State returned", launch.Env)
	}
}

// TestResolveWorkerSessionRuntimeSurfacesStartupPromptFailure pins that a
// delivery failure (for example an oversized prompt on a runtime with no
// fallback) fails the launch instead of launching a session that silently
// lacks its prompt.
func TestResolveWorkerSessionRuntimeSurfacesStartupPromptFailure(t *testing.T) {
	errDelivery := errors.New("startup prompt cannot be delivered")
	fs, _ := startupPromptState(t)
	fs.startupPromptFn = func(session.Info, *config.ResolvedProvider, string, runtime.Config) (runtime.Config, error) {
		return runtime.Config{}, errDelivery
	}
	srv := New(fs)

	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(startupPromptSessionInfo(), "", nil)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntimeWithMetadata: %v", err)
	}
	if runtimeCfg == nil || runtimeCfg.LaunchHints == nil {
		t.Fatalf("resolved runtime %+v has no LaunchHints", runtimeCfg)
	}
	if _, err := runtimeCfg.LaunchHints(runtimeCfg.Hints); !errors.Is(err, errDelivery) {
		t.Errorf("LaunchHints error = %v, want errors.Is(%v)", err, errDelivery)
	}
}
