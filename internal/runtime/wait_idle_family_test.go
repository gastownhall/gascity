package runtime_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// WaitIdleLiveFamily is the family half of the gate every wait-idle delivery
// path asks: the families whose idle boundary this build can wait on, and
// only those. The tmux leaf pins its busy-trust policy against this list.
func TestWaitIdleLiveFamily(t *testing.T) {
	for _, family := range runtime.WaitIdleLiveFamilies {
		if !runtime.WaitIdleLiveFamily(family) {
			t.Errorf("WaitIdleLiveFamily(%q) = false, want true", family)
		}
	}
	// Controls: families a wait-idle nudge still cannot wait on. Codex and
	// Gemini run turns the scan can read, but their wait-idle delivery is not
	// wired: the gates refused them before this list existed and still do.
	for _, family := range []string{"codex", "gemini", "kimi", "opencode", "", "some-unregistered-family"} {
		if runtime.WaitIdleLiveFamily(family) {
			t.Errorf("WaitIdleLiveFamily(%q) = true, want false", family)
		}
	}
}

// WaitIdleLiveOn is the whole gate. claude's busy indicator is what every
// pane scan reads, so a claude session is live on every runtime that waits.
// grok's turn shows only in its own footer, so a grok session is live only
// where the leaf serving it says its idle wait reads grok
// (runtime.IdleWaitFamilyProvider): a tmux leaf does, an exec leaf, whose wait
// classifies captures without the family, and a runtime that says nothing do
// not. A composite is judged by the leaf it routes the session to, through
// every hop, and a family outside WaitIdleLiveFamilies is live on none.
func TestWaitIdleLiveOnAsksTheServingLeafForFamilyReadFamilies(t *testing.T) {
	full := func() runtime.FullFake { return runtime.FullFake{Fake: runtime.NewFake()} }
	tmuxLeaf := runtime.NewFakeProfile(runtime.ProfileTmux, full())
	execLeaf := runtime.NewFakeProfile(runtime.ProfileExec, full())
	silent := runtime.NewFake()
	// auto (default leg: hybrid of a tmux local leaf and an exec remote one,
	// ACP leg: acp): "local" ends on the tmux leaf, "remote" on the exec one.
	composite := auto.New(
		hybrid.New(tmuxLeaf, execLeaf, func(name string) bool { return name == "remote" }),
		runtime.NewFakeProfile(runtime.ProfileACP, full()),
	)
	for _, tc := range []struct {
		name     string
		provider runtime.Provider
		session  string
		family   string
		want     bool
	}{
		{name: "grok on tmux", provider: tmuxLeaf, session: "s", family: "grok", want: true},
		{name: "grok on exec", provider: execLeaf, session: "s", family: "grok"},
		{name: "grok on a runtime that says nothing", provider: silent, session: "s", family: "grok"},
		{name: "grok routed to tmux", provider: composite, session: "local", family: "grok", want: true},
		{name: "grok routed to exec", provider: composite, session: "remote", family: "grok"},
		{name: "claude on tmux", provider: tmuxLeaf, session: "s", family: "claude", want: true},
		{name: "claude on exec", provider: execLeaf, session: "s", family: "claude", want: true},
		{name: "claude on a runtime that says nothing", provider: silent, session: "s", family: "claude", want: true},
		{name: "claude routed to exec", provider: composite, session: "remote", family: "claude", want: true},
		{name: "codex on tmux, which reads its turn", provider: tmuxLeaf, session: "s", family: "codex"},
		{name: "gemini on tmux", provider: tmuxLeaf, session: "s", family: "gemini"},
		{name: "no family", provider: tmuxLeaf, session: "s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runtime.WaitIdleLiveOn(tc.provider, tc.session, tc.family); got != tc.want {
				t.Errorf("WaitIdleLiveOn(%T, %q, %q) = %t, want %t", tc.provider, tc.session, tc.family, got, tc.want)
			}
		})
	}
}
