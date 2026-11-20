package worker

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestFactoryThreadsLaunchFinalizerToTheManager: FactoryConfig.FinalizeLaunch
// is the finalizer of the manager behind every handle the factory builds, so a
// started create hands the runtime the config the finalizer returned.
func TestFactoryThreadsLaunchFinalizerToTheManager(t *testing.T) {
	var seen []sessionpkg.LaunchContext
	finalize := func(lc sessionpkg.LaunchContext) (runtime.Config, error) {
		seen = append(seen, lc)
		lc.Config.Nudge = "FACTORY FINALIZED"
		return lc.Config, nil
	}
	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{
		Store:          beads.NewMemStore(),
		Provider:       sp,
		CityPath:       t.TempDir(),
		FinalizeLaunch: finalize,
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	handle, err := factory.Session(SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("factory.Session: %v", err)
	}
	info, err := handle.Create(context.Background(), CreateModeStarted)
	if err != nil {
		t.Fatalf("Create(started): %v", err)
	}
	if len(seen) != 1 || seen[0].Kind != sessionpkg.LaunchCreate {
		t.Fatalf("finalizer saw %d launches (%+v), want one create", len(seen), seen)
	}
	got := sp.LastStartConfig(info.SessionName)
	if got == nil {
		t.Fatalf("no Start recorded: %#v", sp.Calls)
	}
	if got.Nudge != "FACTORY FINALIZED" {
		t.Fatalf("Start config Nudge = %q, want the finalizer's output", got.Nudge)
	}
}

// TestFactoryWithoutFinalizerLaunchesAsAssembled: a nil FinalizeLaunch leaves
// the factory's launches untouched.
func TestFactoryWithoutFinalizerLaunchesAsAssembled(t *testing.T) {
	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{Store: beads.NewMemStore(), Provider: sp, CityPath: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	handle, err := factory.Session(SessionSpec{Profile: ProfileClaudeTmuxCLI, Template: "probe", Title: "Probe", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatalf("factory.Session: %v", err)
	}
	info, err := handle.Create(context.Background(), CreateModeStarted)
	if err != nil {
		t.Fatalf("Create(started): %v", err)
	}
	if got := sp.LastStartConfig(info.SessionName); got == nil || got.Nudge != "" {
		t.Fatalf("Start config = %+v, want an untouched launch", got)
	}
}
