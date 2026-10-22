package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	sessionhybrid "github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// profiles are every fake capability profile.
func profiles() []runtime.Profile {
	var out []runtime.Profile
	for p := runtime.ProfileTmux; p <= runtime.ProfileT3Bridge; p++ {
		out = append(out, p)
	}
	return out
}

// freshProfile reports a backend whose liveness read is fresh and reports
// errors (freshReadable): tmux's fresh read, acp's and subprocess's by
// construction.
func freshProfile(p runtime.Profile) bool {
	return p == runtime.ProfileTmux || p == runtime.ProfileACP || p == runtime.ProfileSubprocess
}

// Kills a builtin runtime with no fake profile, or a profile no builtin
// builds: every backend the registry builds by name or prefix, but the
// fakes and the hybrid composite, has the profile of its name.
func TestRuntimeRegistryBuiltinsHaveFakeProfiles(t *testing.T) {
	var got, want []string
	for _, n := range runtimeRegistry.Names() {
		if n != "fake" && n != "fail" && n != "hybrid" {
			got = append(got, n)
		}
	}
	for _, prefix := range runtimeRegistry.Prefixes() {
		got = append(got, strings.TrimSuffix(prefix, ":"))
	}
	for _, p := range profiles() {
		want = append(want, p.String())
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("registry builtins %v, fake profiles %v", got, want)
	}
}

// matrixCase is one backend the matrix runs: a provider over a fake the
// row's runtime runs on, whether its reads are fresh, and a fresh copy for
// the heal.
type matrixCase struct {
	name  string
	fresh bool
	build func() (runtime.Provider, *runtime.Fake)
}

// matrixCases are every profile, and the composites production builds over
// them: auto (tmux, with acp; maintainer-city runs it) and hybrid (tmux over
// k8s).
func matrixCases() []matrixCase {
	var out []matrixCase
	for _, p := range profiles() {
		out = append(out, matrixCase{p.String(), freshProfile(p), func() (runtime.Provider, *runtime.Fake) { return fakeProfile(p) }})
	}
	out = append(out,
		matrixCase{"auto(tmux, acp)", true, func() (runtime.Provider, *runtime.Fake) {
			tmux, f := fakeProfile(runtime.ProfileTmux)
			acp, _ := fakeProfile(runtime.ProfileACP)
			a := auto.New(tmux, acp)
			a.SeedRoutes(nil)
			return a, f
		}},
		matrixCase{"hybrid(tmux, k8s)", true, func() (runtime.Provider, *runtime.Fake) {
			tmux, f := fakeProfile(runtime.ProfileTmux)
			k8s, _ := fakeProfile(runtime.ProfileK8s)
			return sessionhybrid.New(tmux, k8s, func(string) bool { return false }), f
		}},
	)
	return out
}

// Kills a transaction read, fence or heal that takes a path production never
// takes on some backend: over every profile and the auto and hybrid
// composites over them, the runtime read classes a
// running and an absent runtime by what the backend can read, the fence
// stops an own detached runtime only on a backend that reads it fresh, and
// the fresh heal of a gone runtime lands only there.
func TestRuntimeProfilesMatrix(t *testing.T) {
	for _, c := range matrixCases() {
		t.Run(c.name, func(t *testing.T) {
			fresh := c.fresh
			sp, f := c.build()
			startRuntime(t, f, "rt_a", ours("a"))
			read := func(name string) runtimeClass {
				rt, _ := readRuntime(context.Background(), sp, nil, name, gatherNow, func() time.Time { return gatherNow })
				return rt.Class
			}
			wantRunning, wantAbsent := rtUnsupported, rtUnsupported
			if fresh {
				wantRunning, wantAbsent = rtAlive, rtAbsent
			}
			if got := read("rt_a"); got != wantRunning {
				t.Errorf("running: class %d, want %d", got, wantRunning)
			}
			if got := read("rt_gone"); got != wantAbsent {
				t.Errorf("absent: class %d, want %d", got, wantAbsent)
			}

			v, confirmed := fenceRun(t, sp, fenceRequest{Row: fenceRow("a", "tok-a"), Legs: legsFull}, fenceOpts{stop: true})
			switch {
			case fresh && (!v.Proceed || !confirmed || f.IsRunning("rt_a")):
				t.Errorf("fence: %+v confirmed=%t, want an own detached runtime stopped and confirmed", v, confirmed)
			case !fresh && (v.Proceed || v.Reason != fenceLivenessUnknown || f.CountCalls("Stop", "rt_a") != 0):
				t.Errorf("fence: %+v, want liveness_unknown and nothing stopped", v)
			}

			heal, _ := c.build()
			c := newHealCase(t, livenessGone, desireNone, "state", "creating")
			_, s := c.run(t, heal, nil)
			switch {
			case fresh && s.Outcome != settledLanded:
				t.Errorf("heal of a gone runtime: settlement %+v, want landed", s)
			case !fresh && (s.Outcome != settledRefused || s.Cause != causeLivenessUnsupported):
				t.Errorf("heal of a gone runtime: settlement %+v, want refused %q", s, causeLivenessUnsupported)
			}
		})
	}
}
