package main

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
)

// programModeRecorder is a session leaf that records the program-status
// switches published to it.
type programModeRecorder struct {
	*runtime.Fake
	modes []runtime.ProgramStatusMode
}

func (r *programModeRecorder) SetProgramStatusMode(mode runtime.ProgramStatusMode) {
	r.modes = append(r.modes, mode)
}

// Kills a reload that keeps the session provider (its selection unchanged)
// but leaves its leaf reading under the switch it was built with: every
// accepted config publishes its [session] program_status switch to the
// leaves of the provider it publishes, in every direction.
func TestPublishRuntimeConfigPublishesProgramStatusSwitch(t *testing.T) {
	leaf := &programModeRecorder{Fake: runtime.NewFake()}
	sp := auto.New(leaf, runtime.NewFake())
	cr := &CityRuntime{sp: sp, dops: newDrainOps(sp)}
	for i, set := range []string{"shadow", "on", "off", "on", "shadow", ""} {
		cfg := &config.City{Session: config.SessionConfig{ProgramStatus: set}}
		if !cr.publishRuntimeConfig(cfg, sp, cr.dops, "rev-"+set) {
			t.Fatalf("publication %d (%q) rejected", i, set)
		}
	}
	want := []runtime.ProgramStatusMode{
		runtime.ProgramStatusShadow, runtime.ProgramStatusOn, runtime.ProgramStatusOff,
		runtime.ProgramStatusOn, runtime.ProgramStatusShadow, runtime.ProgramStatusOff,
	}
	if !slices.Equal(leaf.modes, want) {
		t.Fatalf("the reused leaf received %q, want %q", leaf.modes, want)
	}
}

// Kills a publish that stops at a composite or skips a nested leaf: every
// leaf that takes the switch receives it, through auto wrapping hybrid, and
// a leaf without it is passed by.
func TestPublishProgramStatusModeReachesEveryLeaf(t *testing.T) {
	local, remote, acp := &programModeRecorder{Fake: runtime.NewFake()}, &programModeRecorder{Fake: runtime.NewFake()}, &programModeRecorder{Fake: runtime.NewFake()}
	composite := auto.New(hybrid.New(local, remote, func(name string) bool { return name == "remote" }), acp)
	publishProgramStatusMode(&config.City{Session: config.SessionConfig{ProgramStatus: "shadow"}}, composite)
	publishProgramStatusMode(nil, composite)
	want := []runtime.ProgramStatusMode{runtime.ProgramStatusShadow, runtime.ProgramStatusOff}
	for name, leaf := range map[string]*programModeRecorder{"local": local, "remote": remote, "acp": acp} {
		if !slices.Equal(leaf.modes, want) {
			t.Errorf("%s leaf received %q, want %q", name, leaf.modes, want)
		}
	}
	publishProgramStatusModeTo(nil, runtime.ProgramStatusOn)
	publishProgramStatusModeTo(runtime.NewFake(), runtime.ProgramStatusOn)
}

// The production leaf takes the switch: the tmux provider is a
// programStatusModeSetter (the seam-backed one the controller builds embeds
// it; tmux TestSeamBackedTakesTheProgramStatusSwitch).
func TestTmuxProviderTakesTheProgramStatusSwitch(t *testing.T) {
	var p runtime.Provider = (*sessiontmux.Provider)(nil)
	if _, ok := p.(programStatusModeSetter); !ok {
		t.Fatal("the tmux provider does not take the published program-status switch")
	}
}
