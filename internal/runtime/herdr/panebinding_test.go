package herdr

import (
	"errors"
	"testing"
	"time"
)

// ── resolveBinding: two-tier name→pane resolution + running verdict ──────────
//
// herdr ≥0.7.4 clears an agent's *name* when its pane occupant changes, so
// name-keyed lookups can go dark on a live agent. resolveBinding keeps the
// name lookup as the fast path and falls back to the pane binding Start
// persisted in the sidecar, probed live before it is trusted (pane ids
// recycle). The running verdict is mode-aware: a registered agent
// (bindModeAgent) whose pane sits at a bare shell prompt past the launch
// grace has *exited* and is REAPED (pane closed, binding cleared) — under
// tmux the pane would have died with the process; a raw shell session
// (bindModeShell) is running as long as its pane exists, because
// `exec /bin/sh -c …` panes die with the command.

// resolveOpsRec records the side effects resolveBinding performed.
type resolveOpsRec struct {
	cleared bool
	reaped  string
}

func opsForRec(t *testing.T, agentHit bool, agentErr error, bound, mode string, probe paneProbe, probeErr error, rec *resolveOpsRec) paneLookupOps {
	t.Helper()
	return paneLookupOps{
		getAgent: func() (agentInfo, bool, error) {
			if agentErr != nil {
				return agentInfo{}, false, agentErr
			}
			if agentHit {
				return agentInfo{Name: "mayor", PaneID: "%5"}, true, nil
			}
			return agentInfo{}, false, nil
		},
		boundPane:    func() string { return bound },
		boundMode:    func() string { return mode },
		boundAge:     func() time.Duration { return time.Hour }, // long past any launch window
		probePane:    func(string) (paneProbe, error) { return probe, probeErr },
		reapPane:     func(paneID string) { rec.reaped = paneID },
		clearBinding: func() { rec.cleared = true },
	}
}

func opsFor(t *testing.T, agentHit bool, agentErr error, bound, mode string, probe paneProbe, probeErr error, cleared *bool) paneLookupOps {
	t.Helper()
	rec := &resolveOpsRec{}
	ops := opsForRec(t, agentHit, agentErr, bound, mode, probe, probeErr, rec)
	if cleared != nil {
		ops.clearBinding = func() { *cleared = true }
	}
	return ops
}

func TestResolveBindingNameHitWinsAndRuns(t *testing.T) {
	cleared := false
	ops := opsFor(t, true, nil, "", "", paneProbe{}, nil, &cleared)
	ops.boundPane = func() string { t.Fatal("bound pane must not be consulted on a name hit"); return "" }
	ops.probePane = func(string) (paneProbe, error) { t.Fatal("no probe on a name hit"); return paneProbe{}, nil }
	pane, running, err := resolveBinding(ops)
	if err != nil || pane != "%5" || !running {
		t.Fatalf("resolveBinding = %q, %v, %v; want %%5, true, nil", pane, running, err)
	}
	if cleared {
		t.Error("binding cleared on a name hit")
	}
}

// The 0.7.4 storm case: name cleared, bound pane busy running the agent.
func TestResolveBindingBusyPaneRunsRegardlessOfMode(t *testing.T) {
	for _, mode := range []string{bindModeAgent, bindModeShell, ""} {
		pane, running, err := resolveBinding(opsFor(t, false, nil, "%5", mode, paneProbe{Exists: true, Busy: true}, nil, nil))
		if err != nil || pane != "%5" || !running {
			t.Fatalf("mode %q: resolveBinding = %q, %v, %v; want %%5, true, nil", mode, pane, running, err)
		}
	}
}

// A registered agent's pane back at its bare shell prompt past the launch
// grace means the agent EXITED: under tmux the pane would have died with the
// process, so reap it — close the pane, clear the binding, resolve absent.
// Without this, every completed ephemeral wisp (unique tab label, no future
// Start to recycle it, no Stop because the session reads not-running) leaks
// one shell pane forever — the herdr echo of the witness sleep leak.
func TestResolveBindingReapsExitedAgentPane(t *testing.T) {
	rec := &resolveOpsRec{}
	pane, running, err := resolveBinding(opsForRec(t, false, nil, "%5", bindModeAgent, paneProbe{Exists: true, Busy: false}, nil, rec))
	if err != nil || pane != "" || running {
		t.Fatalf("resolveBinding = %q, %v, %v; want absent (exited agent reaped)", pane, running, err)
	}
	if rec.reaped != "%5" {
		t.Errorf("exited agent pane not reaped (reaped=%q)", rec.reaped)
	}
	if !rec.cleared {
		t.Error("exited agent binding not cleared")
	}
}

// Inside the launch grace window the same pane state means "shell ready,
// agent still being launched": the pane must resolve untouched — a reap here
// would close the pane out from under the in-flight Start that provisionally
// bound it.
func TestResolveBindingSparesFreshBindingAtPrompt(t *testing.T) {
	rec := &resolveOpsRec{}
	ops := opsForRec(t, false, nil, "%5", bindModeAgent, paneProbe{Exists: true, Busy: false}, nil, rec)
	ops.boundAge = func() time.Duration { return 5 * time.Second }
	pane, running, err := resolveBinding(ops)
	if err != nil || pane != "%5" || running {
		t.Fatalf("resolveBinding = %q, %v, %v; want %%5, false, nil (mid-launch pane spared)", pane, running, err)
	}
	if rec.reaped != "" || rec.cleared {
		t.Error("mid-launch pane was reaped/cleared")
	}
}

// A bare-shell session (empty command) is its own shell: running while the
// pane exists even with nothing in the foreground.
func TestResolveBindingShellModeExistsIsRunning(t *testing.T) {
	pane, running, err := resolveBinding(opsFor(t, false, nil, "%5", bindModeShell, paneProbe{Exists: true, Busy: false}, nil, nil))
	if err != nil || pane != "%5" || !running {
		t.Fatalf("resolveBinding = %q, %v, %v; want %%5, true, nil", pane, running, err)
	}
}

// A pane herdr confirms gone is a stale binding: absent, not running, cleared.
func TestResolveBindingClearsConfirmedGonePane(t *testing.T) {
	cleared := false
	pane, running, err := resolveBinding(opsFor(t, false, nil, "%5", bindModeAgent, paneProbe{}, nil, &cleared))
	if err != nil || pane != "" || running {
		t.Fatalf("resolveBinding = %q, %v, %v; want absent", pane, running, err)
	}
	if !cleared {
		t.Error("confirmed-gone binding was not cleared")
	}
}

// A transport failure probing the pane proves nothing: surface the error,
// keep the binding — a socket blip must not erase the handle to a live agent.
func TestResolveBindingProbeTransportErrorKeepsBinding(t *testing.T) {
	cleared := false
	blip := errors.New("dial unix: connection refused")
	pane, running, err := resolveBinding(opsFor(t, false, nil, "%5", bindModeShell, paneProbe{}, blip, &cleared))
	if !errors.Is(err, blip) || pane != "" || running {
		t.Fatalf("resolveBinding = %q, %v, %v; want the probe error", pane, running, err)
	}
	if cleared {
		t.Error("binding cleared on a transport error")
	}
}

// No binding and no live name: genuinely absent.
func TestResolveBindingAbsentWithoutBinding(t *testing.T) {
	ops := opsFor(t, false, nil, "", "", paneProbe{}, nil, nil)
	ops.probePane = func(string) (paneProbe, error) { t.Fatal("no binding, no probe"); return paneProbe{}, nil }
	pane, running, err := resolveBinding(ops)
	if err != nil || pane != "" || running {
		t.Fatalf("resolveBinding = %q, %v, %v; want absent", pane, running, err)
	}
}

// ── paneProbeFrom: the busy verdict ──────────────────────────────────────────

func TestPaneProbeFrom(t *testing.T) {
	tests := []struct {
		name     string
		shellPID int
		fg       []proc
		want     paneProbe
	}{
		{"gone", 0, nil, paneProbe{}},
		{"bare prompt (root shell only)", 100, []proc{{PID: 100, Name: "zsh"}}, paneProbe{Exists: true}},
		{"bare prompt, login-shell name", 100, []proc{{PID: 100, Name: "-zsh"}}, paneProbe{Exists: true}},
		{"empty foreground", 100, nil, paneProbe{Exists: true}},
		{"foreground child (launched agent)", 100, []proc{{PID: 101, Name: "claude"}}, paneProbe{Exists: true, Busy: true}},
		{"exec'd command replaced the shell", 100, []proc{{PID: 100, Name: "sleep"}}, paneProbe{Exists: true, Busy: true}},
		{"sh -c wrapper with child", 100, []proc{{PID: 101, Name: "sleep"}, {PID: 100, Name: "bash"}}, paneProbe{Exists: true, Busy: true}},
	}
	for _, tt := range tests {
		if got := paneProbeFrom(tt.shellPID, tt.fg); got != tt.want {
			t.Errorf("%s: paneProbeFrom = %+v; want %+v", tt.name, got, tt.want)
		}
	}
}

// paneRunsCommand recognizes the launched `/bin/sh -c <raw>` in a pane's
// foreground — the signal that the typed launch actually executed (a fresh
// pane's shell-init children read as Busy, so Busy alone cannot tell "our
// command is running" from "zsh is still sourcing rc files").
func TestPaneRunsCommand(t *testing.T) {
	raw := `for i in $(seq 1 60); do echo "tick $i"; sleep 1; done`
	wrapper := proc{PID: 100, Name: "bash", Argv: []string{"/bin/sh", "-c", raw}}
	if !paneRunsCommand([]proc{{PID: 101, Name: "sleep"}, wrapper}, raw) {
		t.Error("wrapper present: want true")
	}
	init := []proc{{PID: 100, Name: "zsh", Argv: []string{"-zsh"}}, {PID: 102, Name: "sw_vers", Argv: []string{"/usr/bin/sw_vers"}}}
	if paneRunsCommand(init, raw) {
		t.Error("shell-init foreground must not read as launched")
	}
	if paneRunsCommand(nil, raw) {
		t.Error("empty foreground must not read as launched")
	}
}

// paneRootReplaced spots a launch whose command exec'd straight through the
// wrapper (e.g. `exec sleep 120`): the pane's root pid is no longer a shell.
// Shell-init children (root still a shell) must not read as replaced.
func TestPaneRootReplaced(t *testing.T) {
	if !paneRootReplaced(100, []proc{{PID: 100, Name: "sleep"}}) {
		t.Error("exec'd root: want replaced")
	}
	if paneRootReplaced(100, []proc{{PID: 100, Name: "-zsh"}, {PID: 102, Name: "sw_vers"}}) {
		t.Error("shell init: want not replaced")
	}
	if paneRootReplaced(100, nil) {
		t.Error("no root visible: want not replaced")
	}
}

// A name-lookup transport failure surfaces without touching the binding.
func TestResolveBindingNameLookupErrorSurfaces(t *testing.T) {
	cleared := false
	boom := errors.New("herdr transport down")
	ops := opsFor(t, false, boom, "%5", bindModeAgent, paneProbe{Exists: true, Busy: true}, nil, &cleared)
	ops.boundPane = func() string { t.Fatal("no fallback on a name-lookup transport error"); return "" }
	_, running, err := resolveBinding(ops)
	if !errors.Is(err, boom) || running {
		t.Fatalf("resolveBinding = _, %v, %v; want the lookup error", running, err)
	}
	if cleared {
		t.Error("binding cleared on a name-lookup transport error")
	}
}
