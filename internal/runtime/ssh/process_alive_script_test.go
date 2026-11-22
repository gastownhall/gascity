package ssh

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// bwkAwkShim stands in for the box's awk with the One True AWK's rule (the
// awk macOS ships): a -v value holding a newline is a fatal "newline in
// string". Anything else runs the host's awk.
const bwkAwkShim = `#!/bin/sh
prev=
for a in "$@"; do
	if [ "$prev" = -v ]; then
		case $a in *'
'*) echo "awk: newline in string $a" >&2; exit 2 ;; esac
	fi
	prev=$a
done
exec "$REAL_AWK" "$@"
`

// ProcessAlive's remote script runs on the box's own sh, ps and awk, and the
// box can be a Mac. A session with more than one pane lists more than one
// pane pid, and passing them to awk newline-separated made macOS awk refuse
// the whole snapshot: ProcessAlive then degraded to the panes' liveness and
// read a session alive by a process only the human's window ran.
func TestProcessAliveScriptRunsUnderBWKAwk(t *testing.T) {
	realAwk, err := exec.LookPath("awk")
	if err != nil {
		t.Skipf("no awk on PATH: %v", err)
	}
	bin := t.TempDir()
	stubs := map[string]string{
		"awk":  bwkAwkShim,
		"tmux": "#!/bin/sh\nprintf 'agent 100\\norphan 400\\n'\n",
		// The same table for the comm and the args pass.
		"ps": "#!/bin/sh\nprintf '  100     1 zsh\\n  101   100 claude\\n  400     1 zsh\\n  401   400 sleep\\n  500     1 sleep\\n'\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", processAliveScript("worker-1"))
	cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "REAL_AWK=" + realAwk}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || stderr.Len() > 0 {
		t.Fatalf("processAliveScript: %v\nstderr: %s\nout:\n%s", err, stderr.String(), out)
	}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"claude", true}, // the agent pane's child
		{"sleep", false}, // only the human window and another tree run it
	} {
		alive, degraded := paneTreeRunsNames(string(out), proctable.ProcessNameSet([]string{tc.name}))
		if degraded {
			t.Fatalf("snapshot degraded under BWK awk; output:\n%s", out)
		}
		if alive != tc.want {
			t.Errorf("%s alive = %v, want %v; output:\n%s", tc.name, alive, tc.want, out)
		}
	}
}

// TestPaneTreeRunsNamesAcceptsRunningGCExecutable pins #7551 on the ssh
// provider's ProcessAlive output: an agent pane whose child runs the gc
// binary under its own on-disk name (GC_BIN = os.Executable, e.g. Nix
// wrapProgram's .gc-wrapped) is alive for process_names = ["gc"]. The
// running test binary plays the remote gc binary, just as GC_BIN plays it
// in production.
func TestPaneTreeRunsNamesAcceptsRunningGCExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v, want the running test binary's path", err)
	}
	self := filepath.Base(exe)
	if self == "gc" {
		t.Fatalf("running executable is named %q; the alias under test is a no-op here", self)
	}
	out := "agent 100\n" +
		processSectionComm + "\n" +
		"  100     1 zsh\n" +
		"  101   100 " + self + "\n" +
		processSectionArgs + "\n" +
		"  100     1 zsh\n" +
		"  101   100 " + exe + " convoy control --serve --follow ntrecv/core.control-dispatcher\n"

	alive, degraded := paneTreeRunsNames(out, proctable.ProcessNameSet([]string{"gc"}))
	if degraded {
		t.Fatal("paneTreeRunsNames reported degraded on a readable snapshot")
	}
	if !alive {
		t.Errorf("paneTreeRunsNames([gc]) = false, want true: the pane's child runs the gc binary as %q", self)
	}
	if alive, _ := paneTreeRunsNames(out, proctable.ProcessNameSet([]string{"claude"})); alive {
		t.Error(`paneTreeRunsNames([claude]) = true, want false: the gc executable's name must not satisfy another provider's names`)
	}
}
