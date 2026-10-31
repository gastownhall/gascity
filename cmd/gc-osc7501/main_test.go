package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

func runMain(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(args, strings.NewReader(""), &out, &errOut, nil)
	return code, out.String(), errOut.String()
}

func TestVersionReportsCodec(t *testing.T) {
	code, out, errOut := runMain("version")
	if code != 0 || errOut != "" {
		t.Fatalf("version: exit %d, stderr %q", code, errOut)
	}
	v, err := programstatus.ParseCompanionVersion(out)
	if err != nil {
		t.Fatalf("version line %q: %v", out, err)
	}
	if err := programstatus.CheckCompanionVersion(v); err != nil {
		t.Fatalf("version line %q: %v", out, err)
	}
	if v.Version != version || v.Relay != relayVersion {
		t.Fatalf("version line %q parsed as %+v", out, v)
	}
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("version output %q, want exactly one line", out)
	}
}

func TestVersionRejectsArguments(t *testing.T) {
	if code, _, _ := runMain("version", "extra"); code != exitUsage {
		t.Fatalf("version extra: exit %d, want %d", code, exitUsage)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"relay"}, // part B
		{"bogus"},
		{"tap"},
		{"tap", "-x", "tmux", "-S", "/tmp/s", "-t", "%3"},                 // no -n
		{"tap", "-x", "tmux", "-t", "%3", "-n", "r4nd"},                   // no -S
		{"tap", "-S", "/tmp/s", "-t", "%3", "-n", "r4nd"},                 // no -x
		{"tap", "-x", "tmux", "-S", "/tmp/s", "-t", "3", "-n", "r4nd"},    // bad pane
		{"tap", "-x", "tmux", "-S", "/tmp/s", "-t", "%3", "-n", "r4nd.x"}, // bad rand
		{"tap", "-x", "tmux", "-S", "/tmp/s", "-t", "%3", "-n", strings.Repeat("a", 65)},
		{"tap", "-x", "tmux", "-S", "/tmp/s", "-t", "%3", "-n", "r4nd", "-a", "bad app"},
		{"tap", "-x", "tmux", "-S", "/tmp/s", "-t", "%3", "-n", "r4nd", "stray"},
	} {
		code, out, errOut := runMain(args...)
		if code != exitUsage {
			t.Errorf("%q: exit %d, want %d", args, code, exitUsage)
		}
		if out != "" || errOut == "" {
			t.Errorf("%q: stdout %q stderr %q; usage errors go to stderr only", args, out, errOut)
		}
	}
}

func TestTapExitsOneWhenWritesKeepFailing(t *testing.T) {
	var out, errOut bytes.Buffer
	// The pane stays open: the tap must give up on its own.
	hold := make(chan struct{})
	defer close(hold)
	in := io.MultiReader(strings.NewReader("\x1b]7501;state=working:app=gen\a"), blockingReader(hold))
	code := run([]string{"tap", "-x", "/nonexistent/tmux", "-S", "/tmp/s", "-t", "%3", "-n", "r4nd"}, in, &out, &errOut, nil)
	if code != exitFailure {
		t.Fatalf("exit %d, want %d; stderr %q", code, exitFailure, errOut.String())
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "/nonexistent/tmux") {
		t.Fatalf("stdout %q stderr %q; want the failing write named on stderr", out.String(), errOut.String())
	}
}

func TestTapExitsZeroOnEmptyInput(t *testing.T) {
	// No output, no record: nothing to publish, so the tmux binary is never run.
	code, out, errOut := runMain("tap", "-x", "/nonexistent/tmux", "-S", "/tmp/s", "-t", "%3", "-n", "r4nd", "-a", "gen")
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("tap on empty input: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// blockingReader blocks every Read until hold closes, then reports EOF.
type blockingReader chan struct{}

func (r blockingReader) Read([]byte) (int, error) {
	<-r
	return 0, io.EOF
}
