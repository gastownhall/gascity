package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// Kills: a non-listening or other-path entry read as a listener; a path
// with spaces split.
func TestProcNetUnixListening(t *testing.T) {
	table := "Num       RefCount Protocol Flags    Type St Inode Path\n" +
		"0000000000000000: 00000002 00000000 00010000 0001 01 2799733289 /tmp/tmux-1000/city\n" +
		"0000000000000000: 00000003 00000000 00000000 0001 03 2799733290 /tmp/tmux-1000/client\n" +
		"0000000000000000: 00000002 00000000 00010000 0001 01 2799733291 /tmp/a b/city\n"
	for path, want := range map[string]bool{
		"/tmp/tmux-1000/city": true, "/tmp/tmux-1000/client": false, "/tmp/tmux-1000/cit": false, "/tmp/a b/city": true,
	} {
		if got := procNetUnixListening(table, path); got != want {
			t.Errorf("procNetUnixListening(%q) = %v, want %v", path, got, want)
		}
	}
}

// Kills: another or prefixed path, or a pid or fd field, read as a socket
// bound to the path, any of which would hold a dead server alive; a bound
// name missed, which would read a live server dead.
func TestLsofUnixBound(t *testing.T) {
	out := "p501\nf3\nn->0x1f2e3d4c5b6a7980\nf4\nn/private/tmp/tmux-501/client\n" +
		"p502\nf6\nn/private/tmp/tmux-501/city\nf7\nn/private/tmp/a b/city\n"
	for path, want := range map[string]bool{
		"/private/tmp/tmux-501/city": true, "/private/tmp/a b/city": true, "/private/tmp/tmux-501/cit": false,
		"/tmp/tmux-501/city": false, "501": false, "3": false,
	} {
		if got := lsofUnixBound(out, path); got != want {
			t.Errorf("lsofUnixBound(%q) = %v, want %v", path, got, want)
		}
	}
}

type exitCodeError int

func (e exitCodeError) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitCodeError) ExitCode() int { return int(e) }

// Kills: a failure (stderr, partial output, any exit, a timeout's kill, a
// missing binary) read as an answer, which would confirm a live server dead;
// lsof's empty exit 0, its "nothing bound", read as an error, which would
// leave every dead server unconfirmed on macOS and fail gc suspend there.
func TestLsofAnswer(t *testing.T) {
	if out, err := lsofAnswer([]byte("p1\nf3\nn/x\n"), "", nil); err != nil || out != "p1\nf3\nn/x\n" {
		t.Errorf("lsofAnswer(exit 0) = (%q, %v), want the output", out, err)
	}
	if out, err := lsofAnswer(nil, "", nil); err != nil || out != "" {
		t.Errorf("lsofAnswer(empty exit 0) = (%q, %v), want the empty answer", out, err)
	}
	for name, run := range map[string]struct {
		out    string
		stderr string
		err    error
	}{
		"silent exit 1":      {err: exitCodeError(1)},
		"exit 1 with stderr": {stderr: "lsof: can't read", err: exitCodeError(1)},
		"exit 1 with output": {out: "p1\nf3\n", err: exitCodeError(1)},
		"killed by timeout":  {err: exitCodeError(-1)},
		"not found on PATH":  {err: exec.ErrNotFound},
	} {
		if _, err := lsofAnswer([]byte(run.out), run.stderr, run.err); err == nil {
			t.Errorf("%s: lsofAnswer error = nil, want an error", name)
		}
	}
}

// lsof escapes a backslash, control bytes and bytes from 0x80, so such a path
// never matches its listing. Kills: an escaped path accepted, which would read
// its live server dead; a plain path, spaces included, refused, which would
// leave its dead server unconfirmed.
func TestLsofPrintsVerbatim(t *testing.T) {
	for path, want := range map[string]bool{
		"/private/tmp/tmux-501/city": true, "/private/tmp/a b/city": true, "/tmp/a\\b": false,
		"/tmp/a\nb": false, "/tmp/a\tb": false, "/tmp/a\x7fb": false, "/tmp/caf\u00e9": false,
	} {
		if got := lsofPrintsVerbatim(path); got != want {
			t.Errorf("lsofPrintsVerbatim(%q) = %v, want %v", path, got, want)
		}
	}
}

// A server may bind under the resolved directory (tmux does), so the
// candidates rejoin the path under its longest existing ancestor, even once
// the socket directory is removed. Kills: resolving only the socket directory, which loses the bound
// path when that directory is gone and reads its live server dead.
func TestUnixSocketPathCandidates(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(root, "link", "city"):                  filepath.Join(resolvedRoot, "real", "city"),
		filepath.Join(root, "link", "tmux-501", "city"):      filepath.Join(resolvedRoot, "real", "tmux-501", "city"),
		filepath.Join(root, "link", "gone", "tmux-501", "c"): filepath.Join(resolvedRoot, "real", "gone", "tmux-501", "c"),
	} {
		if got := unixSocketPathCandidates(path); len(got) != 2 || got[0] != path || got[1] != want {
			t.Errorf("unixSocketPathCandidates(%q) = %q, want [%q %q]", path, got, path, want)
		}
	}
}
