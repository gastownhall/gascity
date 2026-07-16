package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/rollout"
)

// rolloutProviderState is a State that also implements RolloutFlagsProvider,
// exercising the composition-root path where the controller has already
// boot-latched its Flags.
type rolloutProviderState struct {
	*fakeState
	flags rollout.Flags
}

func (r rolloutProviderState) RolloutFlags() rollout.Flags { return r.flags }

var _ RolloutFlagsProvider = rolloutProviderState{}

// TestServerBootFlagsFromProvider proves newServer prefers a State's already
// latched Flags (the controller's boot value) over re-resolving.
func TestServerBootFlagsFromProvider(t *testing.T) {
	want := rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Require))
	st := rolloutProviderState{fakeState: newFakeState(t), flags: want}
	// A plain fakeState config would resolve to off; the provider must win.
	st.cfg.Beads.ConditionalWrites = "off"

	s := newServer(st, false)
	if got := s.bootFlags.BeadsConditionalWrites(); got != rollout.Require {
		t.Errorf("server bootFlags via provider = %q, want require (provider must win over config)", got)
	}
}

// TestServerBootFlagsFallbackFromConfig proves that a State which does not
// implement RolloutFlagsProvider falls back to resolving from its Config.
func TestServerBootFlagsFallbackFromConfig(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Beads.ConditionalWrites = "require"

	s := newServer(fs, false)
	if got := s.bootFlags.BeadsConditionalWrites(); got != rollout.Require {
		t.Errorf("server bootFlags fallback from Config = %q, want require", got)
	}
}
