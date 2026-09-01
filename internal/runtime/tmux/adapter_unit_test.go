package tmux

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestBuildLaunchCommandUnsetsColorKillersForInteractiveExecutables(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		command  string
		want     string
	}{
		{name: "claude", provider: "claude", command: "claude", want: "env -u CI -u NO_COLOR claude"},
		{name: "claude alias", provider: "qlandia/claude", command: "claude", want: "env -u CI -u NO_COLOR claude"},
		{name: "claude without provider", command: "claude", want: "env -u CI -u NO_COLOR claude"},
		{name: "codex", provider: "codex", command: "codex", want: "env -u CI -u NO_COLOR codex"},
		{name: "kiro command", provider: "claude", command: "kiro-cli", want: "kiro-cli"},
		{name: "omp", provider: "omp", command: "omp", want: "omp"},
		{name: "custom", provider: "custom", command: "custom-agent", want: "custom-agent"},
		{name: "custom codex", provider: "custom-codex", command: "custom-codex", want: "custom-codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := buildLaunchCommand("worker", runtime.Config{Command: tc.command, ProviderName: tc.provider})
			if err != nil {
				t.Fatalf("buildLaunchCommand: %v", err)
			}
			if got != tc.want {
				t.Fatalf("command = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildLaunchCommandColorWrapsLongPromptCommand(t *testing.T) {
	got, promptFile, err := buildLaunchCommand("worker", runtime.Config{
		Command:      "/opt/bin/claude",
		ProviderName: "kiro",
		WorkDir:      t.TempDir(),
		PromptSuffix: strings.Repeat("prompt ", maxInlinePromptLen),
	})
	if err != nil {
		t.Fatalf("buildLaunchCommand: %v", err)
	}
	if promptFile == "" {
		t.Fatal("long prompt did not create a prompt file")
	}
	if !strings.HasPrefix(got, "env -u CI -u NO_COLOR sh -c ") {
		t.Fatalf("command = %q, want env wrapper around final sh -c command", got)
	}
}

func TestProviderAttachRefusesDeadPane(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{"", "1"},
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	err := p.Attach("runner")
	if err == nil {
		t.Fatal("Attach = nil, want dead pane error")
	}
	if !strings.Contains(err.Error(), "dead pane") {
		t.Fatalf("Attach error = %v, want dead pane context", err)
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "attach-session") {
			t.Fatalf("Attach attempted tmux attach-session for dead pane: %v", fe.calls)
		}
	}
}

func TestProviderAttachMissingSessionWrapsRuntimeSentinel(t *testing.T) {
	fe := &fakeExecutor{
		err: ErrSessionNotFound,
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	err := p.Attach("runner")
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Attach error = %v, want runtime.ErrSessionNotFound", err)
	}
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Attach error = %v, want tmux ErrSessionNotFound", err)
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "attach-session") {
			t.Fatalf("Attach attempted tmux attach-session for missing session: %v", fe.calls)
		}
	}
}

func TestProviderListRunningReportsPartialOnNoServer(t *testing.T) {
	fe := &fakeExecutor{err: ErrNoServer}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	names, err := p.ListRunning("")
	if names != nil {
		t.Fatalf("ListRunning names = %v, want nil on unreachable server", names)
	}
	if !runtime.IsPartialListError(err) {
		t.Fatalf("ListRunning err = %v, want runtime.PartialListError so reconciler guards defer", err)
	}
	if !errors.Is(err, ErrNoServer) {
		t.Fatalf("ListRunning err = %v, want wrapped ErrNoServer cause", err)
	}
}

func TestProviderListRunningPropagatesNonServerError(t *testing.T) {
	sentinel := errors.New("tmux exploded")
	fe := &fakeExecutor{err: sentinel}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	names, err := p.ListRunning("")
	if names != nil {
		t.Fatalf("ListRunning names = %v, want nil on error", names)
	}
	if runtime.IsPartialListError(err) {
		t.Fatalf("ListRunning err = %v, want a plain error (not partial) for a real tmux failure", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("ListRunning err = %v, want the underlying tmux error", err)
	}
}

// TestListSessionsAbsorbsNoServer pins the tmux-internal contract that the
// change deliberately preserves: ListSessions still reports an unreachable
// server as an empty result so FindSessionByWorkDir and CleanupOrphanedSessions
// keep treating "server down" as "no sessions". Only Provider.ListRunning
// surfaces the outage as a PartialListError.
func TestListSessionsAbsorbsNoServer(t *testing.T) {
	fe := &fakeExecutor{err: ErrNoServer}
	tm := NewTmux()
	tm.exec = fe

	names, err := tm.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions err = %v, want nil (no server absorbed)", err)
	}
	if names != nil {
		t.Fatalf("ListSessions names = %v, want nil", names)
	}
}

func TestProviderAttachReportsHasSessionError(t *testing.T) {
	fe := &fakeExecutor{
		err: errors.New("tmux unavailable"),
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	err := p.Attach("runner")
	if err == nil {
		t.Fatal("Attach = nil, want has-session error")
	}
	if !strings.Contains(err.Error(), "checking tmux session before attach") {
		t.Fatalf("Attach error = %v, want checking context", err)
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "attach-session") {
			t.Fatalf("Attach attempted tmux attach-session after has-session error: %v", fe.calls)
		}
	}
}

// The reconciler probes attachment and last activity for every session it
// tracks. Before gcy-8gwi each probe forked its own tmux process —
// `display-message -t <s>` plus `list-windows -t <s>` per session, 2N forks
// per pass on top of the fleet snapshot IsRunning already takes. Both now read
// the snapshot the cache already holds, so a whole pass costs the one
// list-panes call and nothing per session.
func TestProviderAttachmentAndActivityServedFromFleetSnapshot(t *testing.T) {
	fe := &fakeExecutor{
		out: strings.Join([]string{
			"agent-1\t0\tclaude\t101\t0\t1000",
			"agent-2\t0\tclaude\t102\t1\t2000",
			"agent-3\t0\tclaude\t103\t0\t3000",
		}, "\n"),
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	for name, want := range map[string]bool{"agent-1": false, "agent-2": true, "agent-3": false} {
		if got := p.IsAttached(name); got != want {
			t.Errorf("IsAttached(%s) = %t, want %t", name, got, want)
		}
	}
	for name, want := range map[string]int64{"agent-1": 1000, "agent-2": 2000, "agent-3": 3000} {
		got, err := p.GetLastActivity(name)
		if err != nil {
			t.Errorf("GetLastActivity(%s) error = %v", name, err)
			continue
		}
		if !got.Equal(time.Unix(want, 0)) {
			t.Errorf("GetLastActivity(%s) = %v, want %v", name, got, time.Unix(want, 0))
		}
	}

	if len(fe.calls) != 1 {
		t.Fatalf("tmux calls = %d, want 1 (list-panes only); per-session forks are the bug: %v", len(fe.calls), fe.calls)
	}
	if joined := strings.Join(fe.calls[0], " "); !strings.Contains(joined, "list-panes") {
		t.Fatalf("single tmux call = %q, want the list-panes fleet snapshot", joined)
	}
}

// A session absent from the snapshot (created since the last refresh, or a
// target that is not a bare session name) must fall back to the direct
// per-session reads rather than reporting detached-with-no-activity.
func TestProviderAttachmentAndActivityFallBackWhenSnapshotMissesSession(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{
			"agent-1\t0\tclaude\t101\t0\t1000", // list-panes: no agent-2
			"0",                                // display-message for agent-2
			"5000",                             // list-windows for agent-2
		},
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	if p.IsAttached("agent-2") {
		t.Error("IsAttached(agent-2) = true, want false from the direct read")
	}
	got, err := p.GetLastActivity("agent-2")
	if err != nil {
		t.Fatalf("GetLastActivity(agent-2) error = %v", err)
	}
	if !got.Equal(time.Unix(5000, 0)) {
		t.Fatalf("GetLastActivity(agent-2) = %v, want %v from the direct read", got, time.Unix(5000, 0))
	}

	joined := strings.Join(fe.calls[len(fe.calls)-1], " ")
	if !strings.Contains(joined, "list-windows") {
		t.Fatalf("last tmux call = %q, want the per-session list-windows fallback", joined)
	}
}

// Reading activity from the snapshot must not lose the poke discount: a
// woken-but-unresponsive agent whose only "activity" is gc's own send-keys
// echo still reports its pre-poke activity (#3049). Serving the raw timestamp
// from a batch would make every parked agent look freshly active.
func TestProviderLastActivityFromSnapshotStillDiscountsPoke(t *testing.T) {
	now := time.Now()
	poke := now.Add(-time.Minute)
	prior := now.Add(-time.Hour)
	fe := &fakeExecutor{
		out: fmt.Sprintf("agent-1\t0\tclaude\t101\t0\t%d", poke.Unix()),
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe
	p.tm.recordPokeAt("agent-1", prior, poke)

	got, err := p.GetLastActivity("agent-1")
	if err != nil {
		t.Fatalf("GetLastActivity error = %v", err)
	}
	if got.Unix() != prior.Unix() {
		t.Fatalf("GetLastActivity = %v, want the pre-poke activity %v: the snapshot path must apply the same poke discount as the per-session read", got, prior)
	}
}
