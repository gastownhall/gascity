package runtime_test

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

type bootstrapScriptProfile struct{ runtime.FullFake }

func (p bootstrapScriptProfile) ObserveBootstrapInput(_ context.Context, _ string, req runtime.BootstrapInputRequest) (*runtime.BootstrapInputProof, error) {
	return &runtime.BootstrapInputProof{Fence: req.Fence, Token: "scripted-exact-credit"}, nil
}

func TestBootstrapProfileOnlyTmuxMatchesNativeObserverTrait(t *testing.T) {
	for profile := runtime.ProfileTmux; profile <= runtime.ProfileT3Bridge; profile++ {
		backend := runtime.FullFake{Fake: runtime.NewFake()}
		p := runtime.NewFakeProfile(profile, backend)
		observer, ok := p.(runtime.BootstrapInputObserver)
		if ok != (profile == runtime.ProfileTmux) {
			t.Fatalf("profile %v exposes wrong first-input capability", profile)
		}
		if ok {
			proof, err := observer.ObserveBootstrapInput(t.Context(), "session", runtime.BootstrapInputRequest{Family: "codex"})
			if err != nil || proof != nil {
				t.Fatal("default full fake invented a bootstrap credit")
			}
		}
	}
	backend := bootstrapScriptProfile{FullFake: runtime.FullFake{Fake: runtime.NewFake()}}
	p := runtime.NewFakeProfile(runtime.ProfileTmux, backend)
	fence := runtime.InputFence{Generation: 1, Epoch: 1, Token: "instance"}
	proof, err := runtime.ObserveBootstrapInput(t.Context(), p, "session", runtime.BootstrapInputRequest{Family: "codex", Fence: fence})
	if err != nil || proof == nil || proof.Token != "scripted-exact-credit" || proof.Fence != fence {
		t.Fatal("tmux profile dropped its deliberately scripted proof")
	}
}
