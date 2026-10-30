package api

import (
	"errors"
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
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", Provider: "wrapped"}},
		Providers: map[string]config.ProviderSpec{
			"wrapped": {
				DisplayName:       "Wrapped",
				Command:           "aimux",
				Args:              []string{"run", "gemini"},
				PathCheck:         "true", // use /usr/bin/true so LookPath succeeds in CI
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
// leaving the SessionStart hook as the sole carrier.
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
	if len(*calls) != 1 {
		t.Fatalf("State.ApplyStartupPrompt called %d times, want 1", len(*calls))
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
	if runtimeCfg.Hints.Nudge != startupPromptSentinelNudge {
		t.Errorf("Hints.Nudge = %q, want what State returned", runtimeCfg.Hints.Nudge)
	}
	if runtimeCfg.Hints.Env[startupPromptSentinelMarker] != "1" {
		t.Errorf("Hints.Env = %v, want the marker State returned", runtimeCfg.Hints.Env)
	}
}

// TestResolveWorkerSessionRuntimeSurfacesStartupPromptFailure pins that a
// delivery failure (for example an oversized prompt on a runtime with no
// fallback) fails the resolution instead of launching a session that silently
// lacks its prompt.
func TestResolveWorkerSessionRuntimeSurfacesStartupPromptFailure(t *testing.T) {
	errDelivery := errors.New("startup prompt cannot be delivered")
	fs, _ := startupPromptState(t)
	fs.startupPromptFn = func(session.Info, *config.ResolvedProvider, string, runtime.Config) (runtime.Config, error) {
		return runtime.Config{}, errDelivery
	}
	srv := New(fs)

	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(startupPromptSessionInfo(), "", nil)
	if !errors.Is(err, errDelivery) {
		t.Errorf("resolveWorkerSessionRuntimeWithMetadata error = %v, want errors.Is(%v)", err, errDelivery)
	}
	if runtimeCfg != nil {
		t.Errorf("resolveWorkerSessionRuntimeWithMetadata returned a runtime alongside the error: %+v", runtimeCfg)
	}
}
