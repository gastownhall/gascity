package api

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// recordingLaunchFinalizer records each launch it sees and stamps the config so
// a test can tell the runtime was started with the finalizer's output.
func recordingLaunchFinalizer(seen *[]session.LaunchContext) session.LaunchFinalizer {
	return func(lc session.LaunchContext) (runtime.Config, error) {
		*seen = append(*seen, lc)
		lc.Config.Nudge = "API FINALIZED"
		return lc.Config, nil
	}
}

// TestWorkerFactoryInstallsStateLaunchFinalizer: a session the API creates
// through its worker factory is launched through the finalizer State supplies,
// so the startup prompt reaches an API-created session (ga-4k4zfk).
func TestWorkerFactoryInstallsStateLaunchFinalizer(t *testing.T) {
	var seen []session.LaunchContext
	fs := newSessionFakeState(t)
	fs.launchFinalizer = recordingLaunchFinalizer(&seen)
	srv := New(fs)

	factory, err := srv.workerFactory(beadstest.Sessions(fs.cityBeadStore))
	if err != nil {
		t.Fatalf("workerFactory: %v", err)
	}
	handle, err := factory.Session(worker.SessionSpec{
		Profile: worker.ProfileClaudeTmuxCLI, Template: "probe", Title: "Probe",
		Command: "claude", WorkDir: t.TempDir(), Provider: "claude",
	})
	if err != nil {
		t.Fatalf("factory.Session: %v", err)
	}
	info, err := handle.Create(context.Background(), worker.CreateModeStarted)
	if err != nil {
		t.Fatalf("Create(started): %v", err)
	}
	if len(seen) != 1 || seen[0].Kind != session.LaunchCreate {
		t.Fatalf("finalizer saw %d launches (%+v), want one create", len(seen), seen)
	}
	got := fs.sp.LastStartConfig(info.SessionName)
	if got == nil || got.Nudge != "API FINALIZED" {
		t.Fatalf("Start config = %+v, want the finalizer's output", got)
	}
}

// TestSessionManagerInstallsStateLaunchFinalizer: the API's direct session
// manager (the path handlers use) installs the same finalizer.
func TestSessionManagerInstallsStateLaunchFinalizer(t *testing.T) {
	var seen []session.LaunchContext
	fs := newSessionFakeState(t)
	fs.launchFinalizer = recordingLaunchFinalizer(&seen)
	srv := New(fs)

	mgr := srv.sessionManager(beadstest.Sessions(fs.cityBeadStore))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "probe", Title: "Probe", Command: "claude", WorkDir: t.TempDir(), Provider: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(seen) != 1 || seen[0].Kind != session.LaunchCreate {
		t.Fatalf("finalizer saw %d launches (%+v), want one create", len(seen), seen)
	}
	got := fs.sp.LastStartConfig(info.SessionName)
	if got == nil || got.Nudge != "API FINALIZED" {
		t.Fatalf("Start config = %+v, want the finalizer's output", got)
	}
}

// TestResolveWorkerSessionRuntimeRendersNoPrompt: the resolver builds launch
// hints only. Prompt delivery belongs to the launch seam, so resolving a
// runtime never renders a prompt (the same hints are built for launches that
// never start a runtime).
func TestResolveWorkerSessionRuntimeRendersNoPrompt(t *testing.T) {
	fs, calls := startupPromptState(t)
	srv := New(fs)

	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(startupPromptSessionInfo(), "", nil)
	if err != nil || runtimeCfg == nil {
		t.Fatalf("resolveWorkerSessionRuntimeWithMetadata = %v, %v", runtimeCfg, err)
	}
	if len(*calls) != 0 {
		t.Errorf("resolver asked State to render the startup prompt %d times, want 0", len(*calls))
	}
	if runtimeCfg.Hints.Nudge != "" || runtimeCfg.Hints.PromptSuffix != "" {
		t.Errorf("resolver hints carry a prompt: Nudge=%q PromptSuffix=%q", runtimeCfg.Hints.Nudge, runtimeCfg.Hints.PromptSuffix)
	}
	if _, ok := runtimeCfg.Hints.Env[startupPromptSentinelMarker]; ok {
		t.Errorf("resolver hints set the delivered marker: %v", runtimeCfg.Hints.Env)
	}
}
