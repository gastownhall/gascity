package exec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
)

// The exec provider drives its in-box tmux session ("main") by sending tmux
// commands over the pack's exec op, and fixtures in this package — the
// runtimecontract golden reference above all — run that op on the test host.
// Every tmux client those tests start must therefore land on this binary's
// private socket root, never on the developer's tmux server: TMUX and
// TMUX_PANE (which bind a client to the enclosing session's server) must be
// gone, and TMUX_TMPDIR must name the root TestMain created.
func TestExecOpTmuxClientsCannotReachTheHostServer(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "golden")
	if err := os.WriteFile(script, []byte(reference.Script(state, reference.Bodies())), 0o755); err != nil {
		t.Fatal(err)
	}
	sp := NewProviderForCity(script, "host-tmux-isolation", "/host-tmux-isolation")
	out, code, err := sp.Exec(t.Context(), "s", []string{"sh", "-c", `printf '%s|%s|%s' "${TMUX-}" "${TMUX_PANE-}" "${TMUX_TMPDIR-}"`})
	if err != nil || code != 0 {
		t.Fatalf("exec op: code=%d err=%v", code, err)
	}
	got := strings.Split(string(out), "|")
	if len(got) != 3 {
		t.Fatalf("exec op output = %q, want three fields", out)
	}
	if got[0] != "" || got[1] != "" {
		t.Errorf("exec op inherits TMUX=%q TMUX_PANE=%q; a tmux client there drives the enclosing session's server", got[0], got[1])
	}
	if hostTmuxIsolationRoot == "" || got[2] != hostTmuxIsolationRoot {
		t.Errorf("exec op TMUX_TMPDIR = %q, want the private root %q from TestMain; tmux there addresses the host's default socket", got[2], hostTmuxIsolationRoot)
	}
}
