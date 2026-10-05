//go:build integration

package tmux

import (
	"errors"
	"strconv"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// tmux resolves a bare "-t name" by unique prefix. Once gc-test-worker-1 has
// exited, gc-test-worker-10 is its only prefix match, so a bare-target Stop
// killed worker-10 (its pane process first, via the kill plan) and meta reads
// and writes landed in worker-10's environment. Against a real server, every
// operation on the exited session must now miss, and worker-10 must be
// untouched.
func TestExactTargetsNeverReachPrefixMatchedSessionRealTmux(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName + "-exact"
	p := NewProviderWithConfig(cfg)
	tm := p.Tmux()
	t.Cleanup(func() { _ = tm.TeardownServer() })

	const gone, live = "gc-test-worker-1", "gc-test-worker-10"
	for _, name := range []string{gone, live} {
		if err := tm.NewSessionWithCommand(name, t.TempDir(), "sleep 300"); err != nil {
			t.Fatalf("NewSessionWithCommand(%s): %v", name, err)
		}
	}
	if err := tm.SetEnvironment(live, "GC_EXACT_PROBE", "live"); err != nil {
		t.Fatalf("SetEnvironment(%s): %v", live, err)
	}
	livePID, err := tm.GetPanePID(live)
	if err != nil {
		t.Fatalf("GetPanePID(%s): %v", live, err)
	}
	if err := tm.KillSession(gone); err != nil {
		t.Fatalf("KillSession(%s): %v", gone, err)
	}

	if val, err := p.GetMeta(gone, "GC_EXACT_PROBE"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Errorf("GetMeta(%s) = (%q, %v), want runtime.ErrSessionNotFound", gone, val, err)
	}
	if err := p.SetMeta(gone, "GC_EXACT_PROBE", "stale"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("SetMeta(%s) = %v, want ErrSessionNotFound", gone, err)
	}
	if err := p.RemoveMeta(gone, "GC_EXACT_PROBE"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("RemoveMeta(%s) = %v, want ErrSessionNotFound", gone, err)
	}
	if out, err := p.Peek(gone, 5); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Peek(%s) = (%q, %v), want ErrSessionNotFound", gone, out, err)
	}
	if err := p.Stop(gone); err != nil {
		t.Errorf("Stop(%s) on an exited session = %v, want nil", gone, err)
	}

	if alive, err := tm.HasSession(live); err != nil || !alive {
		t.Fatalf("HasSession(%s) = (%t, %v) after operations on %s, want the live session untouched", live, alive, err, gone)
	}
	if pid, err := tm.GetPanePID(live); err != nil || pid != livePID {
		t.Errorf("GetPanePID(%s) = (%q, %v), want the original pane %q", live, pid, err, livePID)
	}
	pid, err := strconv.Atoi(livePID)
	if err != nil {
		t.Fatalf("parsing pane PID %q: %v", livePID, err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("pane process %d of %s: %v, want it still running", pid, live, err)
	}
	if val, err := p.GetMeta(live, "GC_EXACT_PROBE"); err != nil || val != "live" {
		t.Errorf("GetMeta(%s) = (%q, %v), want (\"live\", nil)", live, val, err)
	}

	// tmux's own "unset" answers: "unknown variable" for a key never set, and
	// "-KEY" for one marked for removal.
	if val, err := p.GetMeta(live, "GC_EXACT_NEVER_SET"); err != nil || val != "" {
		t.Errorf("GetMeta(%s, never set) = (%q, %v), want (\"\", nil)", live, val, err)
	}
	if err := tm.markSessionEnvRemoved(live, []string{"GC_EXACT_PROBE"}); err != nil {
		t.Fatalf("markSessionEnvRemoved(%s): %v", live, err)
	}
	if val, err := p.GetMeta(live, "GC_EXACT_PROBE"); err != nil || val != "" {
		t.Errorf("GetMeta(%s, marked removed) = (%q, %v), want (\"\", nil)", live, val, err)
	}
}
