package scripts_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Kills go_test_run.sh (tools/bazel/rules) losing a failing simulator seed
// from the build's output, caching a failure, or a hang printing nothing: a
// failure's violated: and replay: lines and its --- FAIL line survive a long
// report; a Go test timeout prints its panic block; a hard kill with no
// panic falls back to the log's tail; a pass exits 0 and leaves each output.
func TestGoTestRunScript(t *testing.T) {
	script := filepath.Join(repoRoot(t), "tools", "bazel", "rules", "go_test_run.sh")
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("timeout (coreutils) not on PATH")
	}
	noise := strings.Repeat("beads cache: reconciled rig=x\nstep line\n", 400) // a report well past the 400-line tail
	for _, c := range []struct {
		name, body, timeout string
		wantRC              bool // a non-zero exit
		want                []string
	}{
		{
			name:   "a late violation with a long report",
			body:   "printf '%s\\n' '--- FAIL: TestSimCorpus (1.00s)' '    --- FAIL: TestSimCorpus/459 (0.20s)' 'seed 459 (inventory lag true) violated:' '  I15 I-STOP-3/4: v2 rewrote ...' 'replay: GC_V2_SIM_SEED=459 go test -run TestSimCorpus ./cmd/gc/'\ncat <<'NOISE'\n" + noise + "NOISE\necho 'last line'\nexit 1",
			wantRC: true,
			want:   []string{"--- FAIL: TestSimCorpus/459", "seed 459 (inventory lag true) violated:", "replay: GC_V2_SIM_SEED=459", "last line", "failed (exit 1)"},
		},
		{
			name:   "a Go test timeout",
			body:   "echo early\nprintf '%s\\n' 'panic: test timed out after 1s' '	running tests:' '		TestSimCorpus/34 (0s)'\nexit 2",
			wantRC: true,
			want:   []string{"panic: test timed out after 1s", "TestSimCorpus/34", "outlived its -test.timeout"},
		},
		{
			name:    "a hard kill with no panic",
			body:    "echo 'the last thing it said'\nexec sleep 30",
			timeout: "1",
			wantRC:  true,
			want:    []string{"the last thing it said", "outlived its timeout (1s), killed"},
		},
		{name: "a pass", body: "echo ok\nexit 0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			exe := filepath.Join(dir, "fake_test")
			if err := os.WriteFile(exe, []byte("#!/usr/bin/env bash\n"+c.body+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			timeout := c.timeout
			if timeout == "" {
				timeout = "60"
			}
			cmd := harnessBash(fmt.Sprintf("exec bash %q ./fake_test run.log %s '-test.run=^X$'", script, timeout))
			cmd.Dir, cmd.Env = dir, append(os.Environ(), "OUTS=COV=census.json")
			out, err := cmd.CombinedOutput()
			if (err != nil) != c.wantRC {
				t.Fatalf("exit error %v, want failure %t:\n%s", err, c.wantRC, out)
			}
			for _, w := range c.want {
				if !strings.Contains(string(out), w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			if strings.Contains(string(out), "beads cache: reconciled") {
				t.Errorf("output carries the simulator cache's reconcile lines")
			}
			if _, err := os.Stat(filepath.Join(dir, "census.json")); !c.wantRC && err != nil {
				t.Errorf("a pass left no output: %v", err)
			}
		})
	}
}
