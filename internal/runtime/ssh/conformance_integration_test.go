//go:build integration

package ssh

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
	"github.com/gastownhall/gascity/internal/testutil"
)

// fakeSSHScript stands in for the ssh(1) client. sshArgs always shell-quotes
// the remote command into the final argv element regardless of how many
// "-o"/"-i"/"-p" flags precede it, so grabbing the last positional argument
// and handing it to a shell reproduces exactly what a real ssh client does on
// the far end: hand the quoted command string to a shell for interpretation.
// It never parses its own flags, so it touches no real known_hosts file and
// dials no network.
const fakeSSHScript = "#!/bin/sh\nfor last; do :; done\nexec sh -c \"$last\"\n"

// sshFixture caches the hermetic environment sshConformanceEndpoint builds
// for one subtest, so repeat factory calls within that subtest converge on
// the same fake-ssh-on-PATH directory and the same isolated tmux server.
type sshFixture struct {
	once     sync.Once
	endpoint Endpoint
}

var (
	sshConformanceFixturesMu sync.Mutex
	sshConformanceFixtures   = map[*testing.T]*sshFixture{}
)

// sshConformanceEndpoint builds the hermetic production-boundary fixture for
// TestSSHConformance: a fake "ssh" executable placed first on PATH so
// shellRunner's real exec.CommandContext(ctx, "ssh", ...) resolves to it
// instead of a real client, plus an isolated tmux server so the fixture's
// tmux traffic (issued by Provider.tmux over the fake connection) never
// reaches any tmux server this process happens to already be running under.
//
// Several runtimetest subtests call the provider factory more than once --
// starting a session, then calling the factory again for a second session --
// before either session is torn down. Rebuilding the fixture on every call
// would mint a fresh TMUX_TMPDIR each time, repointing tmux's default-socket
// resolution out from under the first, already-running session and orphaning
// it on an abandoned server that no later ListRunning call would ever see
// again. Caching the fixture per subtest *testing.T (via sync.Once, so
// concurrent goroutines within one subtest -- see Start_ConcurrentDistinctSessions
// -- don't race on setup) keeps it stable for that subtest's whole lifetime.
// The cache is deliberately scoped to one subtest, not the package or binary:
// t.Setenv's automatic revert on subtest completion keeps this fixture from
// leaking into ssh_test.go's own real-infra tests, which must still see a
// real (or absent) ssh client. tmux resolves its server socket from $TMUX
// (the enclosing session, when set) before it ever consults $TMUX_TMPDIR, so
// isolation requires clearing $TMUX explicitly -- t.Setenv can only set a
// value, never remove a key. In keeping with internal/runtime/tmux's own
// conformance precedent (adapter_test.go, isolated via a fixed socket name
// with no explicit teardown), the per-subtest tmux server started here is
// left running rather than explicitly killed; it is bound to a directory
// this call alone created and is reaped with normal temp-dir cleanup.
func sshConformanceEndpoint(t *testing.T) Endpoint {
	t.Helper()

	sshConformanceFixturesMu.Lock()
	f, ok := sshConformanceFixtures[t]
	if !ok {
		f = &sshFixture{}
		sshConformanceFixtures[t] = f
		t.Cleanup(func() {
			sshConformanceFixturesMu.Lock()
			delete(sshConformanceFixtures, t)
			sshConformanceFixturesMu.Unlock()
		})
	}
	sshConformanceFixturesMu.Unlock()

	f.once.Do(func() { f.endpoint = buildSSHConformanceFixture(t) })
	return f.endpoint
}

func buildSSHConformanceFixture(t *testing.T) Endpoint {
	t.Helper()

	fixtureDir := t.TempDir()
	scriptPath := filepath.Join(fixtureDir, "ssh")
	if err := os.WriteFile(scriptPath, []byte(fakeSSHScript), 0o755); err != nil {
		t.Fatalf("writing fake ssh fixture: %v", err)
	}
	t.Setenv("PATH", fixtureDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// tmux binds its server socket at $TMUX_TMPDIR/tmux-<uid>/default, and a
	// Unix socket path is capped at 104 bytes on macOS. There t.TempDir()
	// nests the subtest name under the long per-user /var/folders temp root,
	// so the socket path overran the cap, the server never started, and every
	// subtest failed with "tmux new-session exited 1". ShortTempDir roots the
	// directory at /tmp on macOS.
	t.Setenv("TMUX_TMPDIR", testutil.ShortTempDir(t, "gcssh"))

	prevTMUX, hadTMUX := os.LookupEnv("TMUX")
	if err := os.Unsetenv("TMUX"); err != nil {
		t.Fatalf("unsetting TMUX: %v", err)
	}
	t.Cleanup(func() {
		if hadTMUX {
			os.Setenv("TMUX", prevTMUX)
		}
	})

	return Endpoint{User: "conformance", Host: "fixture"}
}

// TestSSHConformance proves internal/runtime/ssh.NewSeamBacked — the
// production SSH constructor — against the full runtime.Provider contract.
func TestSSHConformance(t *testing.T) {
	var counter int64
	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		return NewSeamBacked(sshConformanceEndpoint(t)), runtime.Config{
			Command: "sleep 300",
			WorkDir: t.TempDir(),
		}, fmt.Sprintf("gc-test-conform-%d", atomic.AddInt64(&counter, 1))
	})
}

// A bare -t name is a unique-prefix match in tmux: with worker-1 gone and
// worker-10 live on the same box, "-t worker-1" names worker-10. Through the
// production connection (fake ssh, real tmux), every verb on the absent
// worker-1 must miss worker-10 rather than observe, read, type into, respawn
// or kill it (mc-k609w).
func TestSSHProviderTargetsSessionsExactlyOnRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	p := NewProvider(sshConformanceEndpoint(t))
	p.postStartSettle = 0
	ctx := context.Background()
	if err := p.Start(ctx, "worker-10", runtime.Config{Command: "cat", WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("Start worker-10: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop("worker-10") })
	if err := p.SetMeta("worker-10", "GC_SESSION_ID", "ten"); err != nil {
		t.Fatalf("SetMeta worker-10: %v", err)
	}
	pidBefore, _, err := p.tmux(ctx, "worker-10", "display-message", "-p", "-t", "=worker-10:", "#{pane_pid}")
	if err != nil || pidBefore == "" {
		t.Fatalf("worker-10 pane pid = (%q, %v)", pidBefore, err)
	}

	if p.IsRunning("worker-1") {
		t.Error("IsRunning(worker-1) = true: has-session prefix-matched worker-10")
	}
	if got, _ := p.GetMeta("worker-1", "GC_SESSION_ID"); got != "" {
		t.Errorf("GetMeta(worker-1) = %q: show-environment read worker-10's environment", got)
	}
	_ = p.SendKeys("worker-1", "Enter")
	_ = p.Nudge("worker-1", runtime.TextContent("STRAY-NUDGE"))
	if err := p.Relaunch(ctx, "worker-1", runtime.Config{Command: "sleep 300"}); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Errorf("Relaunch(worker-1) = %v, want ErrSessionNotFound", err)
	}
	if err := p.Stop("worker-1"); err != nil {
		t.Errorf("Stop(worker-1) = %v, want idempotent nil", err)
	}

	if !p.IsRunning("worker-10") {
		t.Fatal("worker-10 is gone: kill-session -t worker-1 prefix-matched it")
	}
	if pid, _, _ := p.tmux(ctx, "worker-10", "display-message", "-p", "-t", "=worker-10:", "#{pane_pid}"); pid != pidBefore {
		t.Errorf("worker-10 pane pid %q -> %q: respawn-pane -t worker-1 replaced it", pidBefore, pid)
	}
	if peek, _ := p.Peek("worker-10", 20); strings.Contains(peek, "STRAY-NUDGE") {
		t.Errorf("worker-10 received worker-1's nudge: %q", peek)
	}
}

// ProcessAlive is scoped to the session's own panes: a session whose agent is
// gone reads dead even while another session on the box runs the same
// program (mc-pnu61). A name that is not a safe tmux target is refused before
// it reaches tmux, where "worker-1*" glob-matches worker-10 (mc-xm7mg).
func TestSSHProviderProcessAliveAndNamesOnRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	p := NewProvider(sshConformanceEndpoint(t))
	p.postStartSettle = 0
	ctx := context.Background()
	// worker-10 starts sleep, then signals; the signal proves sleep is running.
	ready := fmt.Sprintf("gc-ssh-pa-ready-%d", time.Now().UnixNano())
	for name, cmd := range map[string]string{
		"worker-1":  "cat",
		"worker-10": "sh -c 'sleep 300 & tmux wait-for -S " + ready + "; wait'",
	} {
		if err := p.Start(ctx, name, runtime.Config{Command: cmd, WorkDir: t.TempDir()}); err != nil {
			t.Fatalf("Start %s: %v", name, err)
		}
		t.Cleanup(func() { _ = p.Stop(name) })
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, code, err := p.tmux(waitCtx, "worker-10", "wait-for", ready); err != nil || code != 0 {
		t.Fatalf("waiting for worker-10's sleep: code %d, %v", code, err)
	}
	if !p.ProcessAlive("worker-10", []string{"sleep"}) {
		t.Fatal("ProcessAlive(worker-10, sleep) = false, want true")
	}
	if p.ProcessAlive("worker-1", []string{"sleep"}) {
		t.Error("ProcessAlive(worker-1, sleep) = true: matched worker-10's sleep box-wide")
	}

	// The box's whole process table never crosses the connection: only the
	// session's pane trees do.
	if out, _, err := p.conn.Exec(ctx, "worker-10", []string{"sh", "-c", processAliveScript("worker-10")}); err != nil {
		t.Fatalf("process script: %v", err)
	} else if rows := strings.Count(string(out), "\n"); rows > 20 {
		t.Errorf("process script returned %d lines, want only worker-10's pane tree:\n%s", rows, out)
	}

	// A ps that fails on the box degrades to alive rather than reading every
	// session dead; a missing session still reads dead.
	shadow := t.TempDir()
	if err := os.WriteFile(filepath.Join(shadow, "ps"), []byte("#!/bin/sh\necho 'ps: unknown option' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	t.Setenv("PATH", shadow+string(os.PathListSeparator)+path)
	if !p.ProcessAlive("worker-1", []string{"sleep"}) {
		t.Error("ProcessAlive with a failing ps = false, want degraded true")
	}
	if p.ProcessAlive("worker-404", []string{"sleep"}) {
		t.Error("ProcessAlive of a missing session with a failing ps = true, want false")
	}
	t.Setenv("PATH", path)

	if err := p.Stop("worker-1*"); !errors.Is(err, ErrInvalidSessionName) {
		t.Errorf("Stop(worker-1*) = %v, want ErrInvalidSessionName", err)
	}
	if p.IsRunning("worker-1*") {
		t.Error("IsRunning(worker-1*) = true: the glob reached tmux")
	}
	if !p.IsRunning("worker-10") {
		t.Error("worker-10 is gone: kill-session -t 'worker-1*' glob-matched it")
	}
}

// Through the production connection (fake ssh, real tmux): Relaunch and the
// driving verbs reach the session's agent pane, not a human's split or window
// that is active, and fail closed when a human killed the agent pane. Another
// gc session's agent pane joined in front reads as foreign (mc-zndi7.103).
func TestSSHProviderAgentPaneOnRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	p := NewProvider(sshConformanceEndpoint(t))
	p.postStartSettle = 0
	// A remote ssh session (or a CI sandbox) often has no UTF-8 locale, and
	// tmux run without -u there prints control characters in format output
	// as "_": a tab-separated row read "agent_@0.%0" and nothing resolved.
	t.Setenv("LANG", "C")
	t.Setenv("LC_ALL", "C")
	ctx := context.Background()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out, code, err := p.tmux(ctx, "", args...)
		if err != nil || code != 0 {
			t.Fatalf("tmux %q: exit %d, %v", args, code, err)
		}
		return strings.TrimSpace(out)
	}
	record := func(path string) string {
		b, _ := os.ReadFile(path)
		return string(b)
	}
	// Capture the provider's log (a failed mark is logged) for diagnosis.
	var logged strings.Builder
	prevLog := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prevLog) })
	const name = "ssh-agent-pane"
	agentIn, humanIn := filepath.Join(dir, "agent-in"), filepath.Join(dir, "human-in")
	// diagnose renders the session's pane table, with the marker column, the
	// session flag, the records and the provider log.
	diagnose := func() string {
		panes, _, _ := p.tmux(ctx, "", "list-panes", "-s", "-t", "="+name+":", "-F",
			"#{pane_id} #{window_id} active=#{pane_active} dead=#{pane_dead} cmd=#{pane_current_command} @gc_agent=#{@gc_agent} session=#{session_id} row="+runtime.TmuxAgentPaneMarkerFormat)
		flag, _, _ := p.tmux(ctx, "", "show-options", "-t", "="+name+":", "-v", runtime.TmuxAgentSessionOption)
		return fmt.Sprintf("panes:\n%s\nsession flag: %q\nagent record: %q\nhuman record: %q\nprovider log:\n%s",
			panes, flag, record(agentIn), record(humanIn), logged.String())
	}
	waitFile := func(path, want string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ready := fmt.Sprintf("gc-ssh-ap-%d", time.Now().UnixNano())
		// Ask the box to signal once the record holds want, instead of polling.
		go func() {
			_, _, _ = p.conn.Exec(ctx, "", []string{"sh", "-c", "until grep -qx " + shellQuote([]string{want}) + " " + shellQuote([]string{path}) + " 2>/dev/null; do sleep 0.05; done; tmux wait-for -S " + ready})
		}()
		if _, code, err := p.tmux(ctx, "", "wait-for", ready); err != nil || code != 0 {
			t.Fatalf("waiting for %s to hold %q: exit %d, %v\n%s", path, want, code, err, diagnose())
		}
	}

	if err := p.Start(ctx, name, runtime.Config{Command: "cat > " + agentIn, WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	agent := run("list-panes", "-t", "="+name+":", "-F", "#{pane_id}")
	human := run("split-window", "-b", "-P", "-F", "#{pane_id}", "-t", agent, "cat > "+humanIn)
	run("new-window", "-t", "="+name+":", "sleep 300")

	// The driving verbs are best-effort and return nil even when the lookup
	// fails, so check the resolution itself before sending.
	if got, err := p.agentPane(ctx, name, false); err != nil || !strings.HasSuffix(got, "."+agent) {
		t.Fatalf("agentPane = (%q, %v), want the agent pane %s\n%s", got, err, agent, diagnose())
	}
	if err := p.SendKeys(name, "Enter"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	if err := p.Nudge(name, runtime.TextContent("to-the-agent")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	waitFile(agentIn, "to-the-agent")
	if got := record(humanIn); got != "" {
		t.Errorf("human pane received %q, want nothing\n%s", got, diagnose())
	}
	pidBefore := run("display-message", "-p", "-t", agent, "#{pane_pid}")
	if err := p.Relaunch(ctx, name, runtime.Config{Command: "cat > " + agentIn + "-2", WorkDir: dir}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	if pid := run("display-message", "-p", "-t", agent, "#{pane_pid}"); pid == pidBefore {
		t.Errorf("agent pane %s still runs pid %s, want a respawned agent", agent, pid)
	}
	if dead := run("display-message", "-p", "-t", human, "#{pane_dead}"); dead != "0" {
		t.Errorf("human pane %s dead=%s after Relaunch, want untouched", human, dead)
	}
	// The respawned agent is reading (this also proves cat is running before
	// ProcessAlive looks for it).
	if err := p.Nudge(name, runtime.TextContent("after-relaunch")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	waitFile(agentIn+"-2", "after-relaunch")
	// ProcessAlive judges the agent pane only: the human's window runs sleep,
	// which must not keep the session's agent alive.
	if p.ProcessAlive(name, []string{"sleep"}) {
		t.Errorf("ProcessAlive(sleep) = true: matched the human window's sleep\n%s", diagnose())
	}
	if !p.ProcessAlive(name, []string{"cat"}) {
		t.Errorf("ProcessAlive(cat) = false, want the agent pane's cat\n%s", diagnose())
	}

	// Another gc session's agent pane joined in front reads as foreign.
	if err := p.Start(ctx, "ssh-agent-other", runtime.Config{Command: "sleep 300", WorkDir: dir}); err != nil {
		t.Fatalf("Start other: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop("ssh-agent-other") })
	foreign := run("list-panes", "-t", "=ssh-agent-other:", "-F", "#{pane_id}")
	run("new-session", "-d", "-s", "ssh-agent-keepalive", "sleep 300")
	t.Cleanup(func() { _ = p.Stop("ssh-agent-keepalive") })
	run("join-pane", "-b", "-s", foreign, "-t", agent)
	if got, err := p.agentPane(ctx, name, false); err != nil || !strings.HasSuffix(got, "."+agent) {
		t.Errorf("agentPane with a foreign agent pane joined = (%q, %v), want the session's own %s", got, err, agent)
	}

	// A human kills the agent pane: nothing reaches the panes left behind.
	run("kill-pane", "-t", agent)
	if err := p.Relaunch(ctx, name, runtime.Config{Command: "sleep 300", WorkDir: dir}); !errors.Is(err, runtime.ErrTmuxAgentPaneGone) {
		t.Errorf("Relaunch with the agent pane killed = %v, want ErrTmuxAgentPaneGone", err)
	}
	_ = p.Nudge(name, runtime.TextContent("stray"))
	_ = p.SendKeys(name, "Enter")
	run("send-keys", "-t", human, "-l", "SENTINEL")
	run("send-keys", "-t", human, "Enter")
	waitFile(humanIn, "SENTINEL")
	if got := strings.TrimSpace(record(humanIn)); got != "SENTINEL" {
		t.Errorf("human pane received %q, want only the sentinel", got)
	}
}
