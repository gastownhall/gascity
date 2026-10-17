package runtime_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// Kills a locality, idle-snapshot or turn-reset answer read off the composite
// instead of the session's routed leaf, and a backend taken to be local
// without saying so.
func TestTurnTraitsFollowTheRoute(t *testing.T) {
	local := runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()})
	remote := runtime.NewFakeProfile(runtime.ProfileK8s, runtime.FullFake{Fake: runtime.NewFake()})
	composite := hybrid.New(local, remote, func(name string) bool { return name == "remote" })
	cases := []struct {
		name      string
		p         runtime.Provider
		session   string
		local     bool
		snapshots bool
		resets    bool
	}{
		{"tmux leaf", local, "s", true, true, true},
		{"k8s leaf", remote, "s", false, false, false},
		{"hybrid routed local", composite, "local", true, true, true},
		{"hybrid routed remote", composite, "remote", false, false, false},
		{"subprocess leaf", runtime.NewFakeProfile(runtime.ProfileSubprocess, runtime.FullFake{Fake: runtime.NewFake()}), "s", true, false, false},
		{"herdr leaf", runtime.NewFakeProfile(runtime.ProfileHerdr, runtime.FullFake{Fake: runtime.NewFake()}), "s", true, false, false},
		{"undeclared backend", runtime.NewFake(), "s", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runtime.LocalTranscriptsFor(tc.p, tc.session); got != tc.local {
				t.Errorf("LocalTranscriptsFor = %v, want %v", got, tc.local)
			}
			if got := runtime.IdleSnapshotsFor(tc.p, tc.session); got != tc.snapshots {
				t.Errorf("IdleSnapshotsFor = %v, want %v", got, tc.snapshots)
			}
			if got := runtime.InterruptedTurnResetsFor(tc.p, tc.session); got != tc.resets {
				t.Errorf("InterruptedTurnResetsFor = %v, want %v", got, tc.resets)
			}
		})
	}
}
