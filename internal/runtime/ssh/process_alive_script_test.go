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

// #7551: an agent pane running gc under its executable's on-disk name (a Nix
// wrapProgram target is `.gc-wrapped`) satisfies process_names = ["gc"]. The
// running test binary plays the gc binary, as GC_BIN = os.Executable() does.
func TestPaneTreeRunsNamesAcceptsRunningGCExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	self := filepath.Base(exe)
	if self == "gc" {
		t.Fatalf("test executable is named %q; the wrapped-name case is vacuous", self)
	}
	out := strings.Join([]string{
		"agent 100",
		processSectionComm,
		"100 1 zsh",
		"101 100 " + self,
		processSectionArgs,
		"100 1 zsh",
		"101 100 " + exe + " convoy control --serve --follow r/core.control-dispatcher",
	}, "\n")

	alive, degraded := paneTreeRunsNames(out, proctable.ProcessNameSet([]string{"gc"}))
	if !alive || degraded {
		t.Errorf("paneTreeRunsNames(%s child, [gc]) = alive %v, degraded %v; want alive, not degraded", self, alive, degraded)
	}
	if alive, _ := paneTreeRunsNames(out, proctable.ProcessNameSet([]string{"claude"})); alive {
		t.Errorf("paneTreeRunsNames(%s child, [claude]) = alive, want not alive", self)
	}
}
