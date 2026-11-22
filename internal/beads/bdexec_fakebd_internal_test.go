//go:build !windows

package beads

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeBDScript is a bd stand-in for exec-gate and coalescing tests. Each child:
//   - appends "<pid> <argv>" to the spawn log, whose line number is its spawn
//     sequence;
//   - publishes an alive marker "<marks>/<pid>" holding its argv (written to a
//     temp name and renamed, so a reader never sees a half-written marker);
//   - optionally dumps its environment to "<GC_TEST_ENV_DUMP>.<seq>";
//   - blocks until "<release>" or "<release>.<seq>" exists;
//   - removes its marker and prints "<seq> <argv>".
const fakeBDScript = `#!/bin/sh
printf '%s %s\n' "$$" "$*" >> "$GC_TEST_SPAWN_LOG"
seq=$(grep -n "^$$ " "$GC_TEST_SPAWN_LOG" | cut -d: -f1)
printf '%s\n' "$*" > "$GC_TEST_MARK_DIR/.$$.tmp"
mv -f "$GC_TEST_MARK_DIR/.$$.tmp" "$GC_TEST_MARK_DIR/$$"
if [ -n "$GC_TEST_ENV_DUMP" ]; then env > "$GC_TEST_ENV_DUMP.$seq"; fi
while [ ! -e "$GC_TEST_RELEASE" ] && [ ! -e "$GC_TEST_RELEASE.$seq" ]; do sleep 0.01; done
rm -f "$GC_TEST_MARK_DIR/$$"
printf '%s %s\n' "$seq" "$*"
`

// fakeBD is one isolated fake-bd harness rooted in a test temp dir.
type fakeBD struct {
	t        *testing.T
	root     string
	bin      string
	marks    string
	spawnLog string
	release  string
}

func newFakeBD(t *testing.T) *fakeBD {
	t.Helper()
	root := t.TempDir()
	f := &fakeBD{
		t:        t,
		root:     root,
		bin:      filepath.Join(root, "bd"),
		marks:    filepath.Join(root, "marks"),
		spawnLog: filepath.Join(root, "spawns.log"),
		release:  filepath.Join(root, "release"),
	}
	if err := os.Mkdir(f.marks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.spawnLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.bin, []byte(fakeBDScript), 0o700); err != nil {
		t.Fatal(err)
	}
	// Never leave a child blocked behind a failed assertion.
	t.Cleanup(f.releaseAll)
	return f
}

// env returns the runner overrides that pin the fake bd and its files.
func (f *fakeBD) env() map[string]string {
	return map[string]string{
		"BD_BIN":            f.bin,
		"GC_TEST_MARK_DIR":  f.marks,
		"GC_TEST_SPAWN_LOG": f.spawnLog,
		"GC_TEST_RELEASE":   f.release,
	}
}

// runner returns an exec runner bound to ctx and gate.
func (f *fakeBD) runner(ctx context.Context, gate *bdExecGate) CommandRunner {
	return execCommandRunner(ctx, f.env(), false, processEnvSnapshotExcludingNativeDoltOpen, gate)
}

func (f *fakeBD) releaseAll() {
	if err := os.WriteFile(f.release, nil, 0o600); err != nil {
		f.t.Errorf("writing release file: %v", err)
	}
}

// releaseSeq unblocks only the child with spawn sequence seq.
func (f *fakeBD) releaseSeq(seq int) {
	f.t.Helper()
	if err := os.WriteFile(f.release+"."+strconv.Itoa(seq), nil, 0o600); err != nil {
		f.t.Fatalf("writing release file for child %d: %v", seq, err)
	}
}

// alive returns the argv of every bd child currently alive.
func (f *fakeBD) alive() []string {
	f.t.Helper()
	entries, err := os.ReadDir(f.marks)
	if err != nil {
		f.t.Errorf("reading marker dir: %v", err)
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(f.marks, e.Name()))
		if err != nil {
			// The child removed its marker between ReadDir and ReadFile.
			continue
		}
		out = append(out, strings.TrimSpace(string(data)))
	}
	return out
}

// spawns returns the argv of every child spawned so far, in spawn order.
func (f *fakeBD) spawns() []string {
	f.t.Helper()
	data, err := os.ReadFile(f.spawnLog)
	if err != nil {
		f.t.Fatalf("reading spawn log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		_, argv, _ := strings.Cut(line, " ")
		out = append(out, argv)
	}
	return out
}

// spawnPIDs returns the pid of every child spawned so far, in spawn order.
func (f *fakeBD) spawnPIDs() []int {
	f.t.Helper()
	data, err := os.ReadFile(f.spawnLog)
	if err != nil {
		f.t.Fatalf("reading spawn log: %v", err)
	}
	var out []int
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		field, _, _ := strings.Cut(line, " ")
		pid, err := strconv.Atoi(field)
		if err != nil {
			f.t.Fatalf("spawn log line %q: %v", line, err)
		}
		out = append(out, pid)
	}
	return out
}

// waitFor polls cond until it holds, failing the test after 5s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after 5s waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func countReads(argvs []string) int {
	n := 0
	for _, argv := range argvs {
		if bdCommandIsRead(strings.Fields(argv)) {
			n++
		}
	}
	return n
}
