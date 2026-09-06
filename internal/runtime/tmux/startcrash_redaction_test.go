package tmux

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The pane is whatever the agent printed before it died, which routinely
// includes a setup command echoing its own environment. That text goes two
// places at once: into the returned error (logged, evented, shown) and into a
// durable file. Redacting at the capture chokepoint covers both.
func TestStartupDeadSessionErrorRedactsPaneSecrets(t *testing.T) {
	const secret = "sk-ant-NOT-A-REAL-KEY-0123456789"
	exec := &fakeExecutor{out: "+ export ANTHROPIC_API_KEY=" + secret + "\nclaude: command not found\n"}
	tm := NewTmux()
	tm.exec = exec

	ops := newTmuxStartOps(tm, "", 0, runtime.Config{Env: map[string]string{
		"ANTHROPIC_API_KEY": secret,
		"GC_RIG":            "kernel",
	}})

	err := startupDeadSessionError(ops, "gc-test-crash")
	if err == nil {
		t.Fatal("a dead startup session must produce an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Error("the startup error carries the credential verbatim")
	}
	// Control: redaction must not swallow the diagnostic. The whole point of
	// folding pane output into the error is that an operator can read why the
	// agent died, and an empty pane takes a different branch entirely.
	if !strings.Contains(err.Error(), "command not found") {
		t.Errorf("the pane diagnostic was lost, not redacted: %v", err)
	}
	if !strings.Contains(err.Error(), runtime.RedactedValue) {
		t.Errorf("the credential was dropped rather than marked redacted: %v", err)
	}
}

// The durable artifact is the copy that outlives the session, so it gets its
// own redaction pass rather than trusting whatever the caller hands it.
func TestRecordStartCrashRedactsPaneSecrets(t *testing.T) {
	const secret = "sk-ant-NOT-A-REAL-KEY-0123456789"
	dir := t.TempDir()
	tm := NewTmux()
	tm.exec = &fakeExecutor{}
	ops := newTmuxStartOps(tm, dir, 0, runtime.Config{Env: map[string]string{"ANTHROPIC_API_KEY": secret}})

	path := ops.recordStartCrash("gc-test-crash", "+ export ANTHROPIC_API_KEY="+secret+"\nboom\n")
	if path == "" {
		t.Fatal("recordStartCrash returned no artifact path")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(b), secret) {
		t.Error("the start-crash artifact holds the credential verbatim")
	}
	// Control: the artifact must still be a diagnostic.
	if !strings.Contains(string(b), "boom") {
		t.Errorf("the artifact lost its pane output entirely: %q", b)
	}
}

// A start-crash artifact is written into the city runtime directory and kept
// indefinitely. Even redacted it is a transcript of a failed agent launch.
func TestRecordStartCrashWritesOwnerOnlyArtifact(t *testing.T) {
	dir := t.TempDir()
	tm := NewTmux()
	tm.exec = &fakeExecutor{}
	ops := newTmuxStartOps(tm, dir, 0, runtime.Config{})

	path := ops.recordStartCrash("gc-test-crash", "boom\n")
	if path == "" {
		t.Fatal("recordStartCrash returned no artifact path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("artifact mode = %04o, want 0600", got)
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Lstat parent: %v", err)
	}
	if got := parent.Mode().Perm(); got != 0o700 {
		t.Errorf("artifact directory mode = %04o, want 0700", got)
	}
}

// capture-pane returns the pane as it is displayed, so a value wider than the
// pane arrives split across lines by a newline tmux inserted. Substring
// redaction cannot match that, and an API key is comfortably wider than a
// default pane. -J rejoins the wrapped lines first.
func TestStartOpsCapturePaneJoinsWrappedLines(t *testing.T) {
	exec := &fakeExecutor{out: "hello\n"}
	tm := NewTmux()
	tm.exec = exec
	ops := newTmuxStartOps(tm, "", 0, runtime.Config{})

	if _, err := ops.capturePane("gc-test-crash", 80); err != nil {
		t.Fatalf("capturePane: %v", err)
	}
	if len(exec.calls) == 0 {
		t.Fatal("capturePane issued no tmux command")
	}
	args := exec.calls[0]
	if !slices.Contains(args, "-J") {
		t.Errorf("capture-pane args %q omit -J; wrapped credentials survive redaction", args)
	}
	// Control: the joined capture is still a capture of the right pane.
	if !slices.Contains(args, "capture-pane") || !slices.Contains(args, "gc-test-crash") {
		t.Errorf("capture-pane args %q lost the command or target", args)
	}
}

// The secrets a startOps redacts with come from the session config, so a
// production call site that builds the struct literally gets an empty secret
// list and redacts nothing — silently, because the output still looks like a
// diagnostic. Tests may use literals; production code must use the constructor.
func TestProductionStartOpsUseTheConstructor(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	checked, literals := 0, 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		checked++
		found := startOpsConstructionsOutsideConstructor(string(b))
		literals += found.total
		for _, line := range found.offending {
			t.Errorf("%s builds tmuxStartOps outside newTmuxStartOps; the redaction secrets would be empty:\n\t%s",
				name, line)
		}
	}
	// Controls: a scan that read no files, or that never saw the constructor's
	// own literal, would pass vacuously.
	if checked == 0 {
		t.Fatal("no non-test Go files were scanned")
	}
	if literals == 0 {
		t.Fatal("the scan never matched the constructor's own composite literal")
	}
}

type startOpsScan struct {
	total     int
	offending []string
}

// startOpsConstructionsOutsideConstructor finds every place source builds a
// tmuxStartOps, and reports the ones that are not inside newTmuxStartOps.
//
// It matches three spellings because `&tmuxStartOps{...}` is only the tidiest
// way to get a value with no redaction secrets. `ops := tmuxStartOps{...}`
// followed by `&ops`, and `new(tmuxStartOps)` with field assignment, satisfy
// the startOps interface just as well and read as ordinary Go.
func startOpsConstructionsOutsideConstructor(source string) startOpsScan {
	var scan startOpsScan
	inConstructor := false
	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(line, "func ") {
			inConstructor = strings.HasPrefix(line, "func newTmuxStartOps(")
		}
		if !strings.Contains(line, "tmuxStartOps{") && !strings.Contains(line, "new(tmuxStartOps)") {
			continue
		}
		scan.total++
		if !inConstructor {
			scan.offending = append(scan.offending, strings.TrimSpace(line))
		}
	}
	return scan
}

// The guard above is only as good as what its matcher recognizes, and a matcher
// that missed the natural workarounds would report a clean tree forever.
func TestStartOpsConstructionScanCatchesEveryConstructionSpelling(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		wantCaught bool
	}{
		{"address-of literal", "func doStart() {\n\to := &tmuxStartOps{tm: tm}\n}\n", true},
		{"value literal then address-of", "func doStart() {\n\to := tmuxStartOps{tm: tm}\n\treturn &o\n}\n", true},
		{"new plus field assignment", "func doStart() {\n\to := new(tmuxStartOps)\n\to.tm = tm\n}\n", true},
		{"inside the constructor", "func newTmuxStartOps(tm *Tmux) *tmuxStartOps {\n\treturn &tmuxStartOps{tm: tm}\n}\n", false},
		{"type declaration is not a construction", "type tmuxStartOps struct {\n\ttm *Tmux\n}\n", false},
		{"unrelated code", "func doStart() {\n\to := newTmuxStartOps(tm, \"\", 0, cfg)\n}\n", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scan := startOpsConstructionsOutsideConstructor(tc.source)
			if caught := len(scan.offending) > 0; caught != tc.wantCaught {
				t.Errorf("caught = %t, want %t (offending: %q)", caught, tc.wantCaught, scan.offending)
			}
		})
	}
}
