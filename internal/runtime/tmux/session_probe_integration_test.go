//go:build integration

package tmux

import (
	"context"
	"testing"
)

// TestProbeSessionAnswersFromARealServer probes a real server hosting
// session "keep": only the exact name is alive, whatever TMPDIR says, and a
// name tmux's target lookup would match by prefix or pattern is dead.
func TestProbeSessionAnswersFromARealServer(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	name := privateSocketName("ps")
	realServerSocketPath(t, name)
	for _, env := range []struct {
		name   string
		set    bool
		tmpdir string
	}{
		{name: "TMPDIR unset"},
		{name: "TMPDIR elsewhere", set: true, tmpdir: shortResolvedDir(t)},
	} {
		t.Run(env.name, func(t *testing.T) {
			if env.set {
				t.Setenv("TMPDIR", env.tmpdir)
			} else {
				unsetEnvForTest(t, "TMPDIR")
			}
			for _, tt := range []struct {
				session string
				want    SessionPresence
			}{
				{session: "keep", want: PresenceAlive},
				{session: "kee", want: PresenceDead},
				{session: "k*", want: PresenceDead},
				{session: "keeper", want: PresenceDead},
			} {
				got, err := ProbeSession(context.Background(), name, tt.session)
				if got != tt.want || err != nil {
					t.Errorf("ProbeSession(%s, %q) = %v, %v; want %v", name, tt.session, got, err, tt.want)
				}
			}
		})
	}
}

// TestProbeSessionAcrossTmuxTmpdirsIsUnobservable starts a server while
// TMUX_TMPDIR names one directory and probes it while TMUX_TMPDIR names
// another: the prober cannot see that server, so the session is
// unobservable, never dead.
func TestProbeSessionAcrossTmuxTmpdirsIsUnobservable(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	name := privateSocketName("ps")
	t.Setenv("TMUX_TMPDIR", shortResolvedDir(t))
	served := realServerSocketPath(t, name)
	t.Setenv("TMUX_TMPDIR", shortResolvedDir(t))
	if SocketPath(name) == served {
		t.Fatalf("SocketPath(%s) = %s under both TMUX_TMPDIRs", name, served)
	}
	got, err := ProbeSession(context.Background(), name, "keep")
	if got != PresenceUnobservable || err == nil {
		t.Fatalf("ProbeSession(%s, keep) = %v, %v; want unobservable with an error", name, got, err)
	}
}
