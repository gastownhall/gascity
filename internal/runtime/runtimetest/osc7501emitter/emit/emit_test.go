package emit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

// writes records each Write as its own chunk.
type writes struct{ chunks []string }

func (w *writes) Write(p []byte) (int, error) {
	w.chunks = append(w.chunks, string(p))
	return len(p), nil
}

func (w *writes) String() string { return strings.Join(w.chunks, "") }

// run runs lines on a terminal reading in, and returns what it wrote.
func run(t *testing.T, in io.Reader, env map[string]string, lines ...string) (*writes, Outcome) {
	t.Helper()
	p, err := ParseLines(lines...)
	if err != nil {
		t.Fatal(err)
	}
	out := &writes{}
	o, err := Run(t.Context(), p, Terminal{In: in, Out: out, Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	return out, o
}

// events scans what a run wrote as the capture tap would.
func events(t *testing.T, w *writes) []programstatus.Event {
	t.Helper()
	var sc programstatus.Scanner
	var got []programstatus.Event
	for _, c := range w.chunks {
		sc.Feed([]byte(c), func(e programstatus.Event) { got = append(got, e) })
	}
	return got
}

// bodies are the report bodies in evs.
func bodies(evs []programstatus.Event) []string {
	var out []string
	for _, e := range evs {
		if e.Kind == programstatus.EventReport {
			out = append(out, e.Body)
		}
	}
	return out
}

// Kills a passive emitter that does not write its reports in order with the
// terminator the script names, or whose bytes a scanner does not read back.
func TestEmitterWritesReportsWithBELOrST(t *testing.T) {
	out, o := run(t, nil, nil,
		"report state=working:app=demo",
		"term bel",
		"report state=blocked:kind=question",
		"term st",
		"clear",
	)
	want := "\x1b]7501;state=working:app=demo\x1b\\" +
		"\x1b]7501;state=blocked:kind=question\x07" +
		"\x1b]7501;state=clear\x1b\\"
	if out.String() != want {
		t.Fatalf("wrote %q, want %q", out.String(), want)
	}
	if got := bodies(events(t, out)); !slices.Equal(got, []string{"state=working:app=demo", "state=blocked:kind=question", "state=clear"}) {
		t.Fatalf("scanned %q", got)
	}
	if o != (Outcome{}) {
		t.Fatalf("outcome %+v, want exit 0 at the script's end", o)
	}
}

// Kills a split that writes the sequence whole, loses bytes, or never cuts
// an ST between its ESC and its backslash.
func TestEmitterSplitCutsInsideST(t *testing.T) {
	for _, n := range []int{2, 3, 7} {
		out, _ := run(t, nil, nil, "split "+strconv.Itoa(n), "report state=working:id=a/b", "report state=idle")
		if len(out.chunks) != n+1 {
			t.Fatalf("split %d wrote %d chunks %q, want %d then the whole next report", n, len(out.chunks), out.chunks, n+1)
		}
		if !strings.HasSuffix(out.chunks[n-2], "\x1b") || out.chunks[n-1] != "\\" {
			t.Fatalf("split %d chunks %q: no cut between ESC and backslash", n, out.chunks)
		}
		if got := bodies(events(t, out)); !slices.Equal(got, []string{"state=working:id=a/b", "state=idle"}) {
			t.Fatalf("split %d scanned %q", n, got)
		}
	}
}

// Kills an oversize step whose sequence is not exactly the length asked, so
// a limit test would probe the wrong side of the bound.
func TestEmitterOversizeIsExact(t *testing.T) {
	for _, n := range []int{22, 30, 4096, 4097, 6144} {
		for _, term := range []string{"bel", "st"} {
			out, _ := run(t, nil, nil, "term "+term, "oversize "+strconv.Itoa(n))
			if len(out.String()) != n {
				t.Fatalf("oversize %d (%s) wrote %d bytes", n, term, len(out.String()))
			}
		}
	}
	// At the limit the report parses; one byte over, the scanner drops it.
	for _, term := range []string{"bel", "st"} {
		at, _ := run(t, nil, nil, "term "+term, "oversize 4096")
		b := bodies(events(t, at))
		if len(b) != 1 {
			t.Fatalf("a 4096-byte report (%s) scanned as %d reports", term, len(b))
		}
		if r, err := programstatus.ParseBody(b[0]); err != nil || r.State != runtime.ProgramWorking {
			t.Fatalf("a 4096-byte report (%s) parsed as %+v, %v; want working", term, r, err)
		}
		over, _ := run(t, nil, nil, "term "+term, "oversize 4097")
		if b := bodies(events(t, over)); len(b) != 0 {
			t.Fatalf("a 4097-byte report (%s) scanned: %d", term, len(b))
		}
	}
}

// The .11 acceptance test: a probe-gated (Claude-like) program sends the
// probe and DA1 and stays silent when the terminal answers only DA1, or
// nothing at all.
func TestEmitterProbeGatedStaysSilentWithoutReply(t *testing.T) {
	for name, in := range map[string]io.Reader{
		"da1 only":   strings.NewReader("\x1b[?62;22c"),
		"no input":   nil,
		"7501 late":  strings.NewReader("\x1b[?1;2c\x1b]7501;state=idle\x07"),
		"input ends": strings.NewReader(""),
	} {
		t.Run(name, func(t *testing.T) {
			out, _ := run(t, in, nil, "term bel", "probe-gated", "report state=working", "clear")
			evs := events(t, out)
			if len(evs) != 1 || evs[0].Kind != programstatus.EventProbe {
				t.Fatalf("events %+v, want the probe alone", evs)
			}
			if !strings.HasSuffix(out.String(), "\x1b[c") {
				t.Fatalf("wrote %q, want DA1 after the probe", out.String())
			}
		})
	}
}

// Kills a probe-gated program that ignores a timely 7501 reply, or waits
// past its bound on a terminal that never answers.
func TestEmitterProbeGatedReportsAfterReply(t *testing.T) {
	out, _ := run(t, strings.NewReader("\x1b]7501;state=idle\x07\x1b[?62c"), nil, "probe-gated", "report state=working")
	if got := bodies(events(t, out)); !slices.Equal(got, []string{"state=working"}) {
		t.Fatalf("answered program reported %q", got)
	}
	pr, pw := io.Pipe()
	defer pw.Close() //nolint:errcheck // closing a pipe writer cannot fail
	start := time.Now()
	out, _ = run(t, pr, nil, "probe-gated 50ms", "report state=working")
	if got := bodies(events(t, out)); len(got) != 0 {
		t.Fatalf("unanswered program reported %q", got)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("probe wait took %v, want about its 50ms bound", time.Since(start))
	}
}

// Kills a probe-gated program whose handshake leaves reply bytes for its line
// reads, or drops typed input that arrives around the replies: after the
// probe, interactive runs every typed line as a step, whatever terminates the
// 7501 reply, whichever reply comes first, and however reads split them.
func TestEmitterProbeGatedLeavesTypedLinesClean(t *testing.T) {
	const (
		da1   = "\x1b[?62;22c"
		typed = "raw 41\rreport state=working\rexit 7\r"
	)
	for name, c := range map[string]struct {
		in       string
		answered bool
	}{
		"BEL reply then DA1":       {"\x1b]7501;state=idle\x07" + da1 + typed, true},
		"ST reply then DA1":        {"\x1b]7501;?\x1b\\" + da1 + typed, true},
		"typed around the replies": {"raw 41\r\x1b]7501;?\x1b\\report state=working\r" + da1 + "exit 7\r", true},
		"DA1 then a late reply":    {da1 + "\x1b]7501;?\x1b\\" + typed, false},
	} {
		for _, oneByte := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/one byte per read %v", name, oneByte), func(t *testing.T) {
				var in io.Reader = strings.NewReader(c.in)
				if oneByte {
					in = iotest.OneByteReader(in)
				}
				out, o := run(t, in, nil, "term bel", "probe-gated", "interactive")
				if o != (Outcome{Ended: true, Exit: 7}) {
					t.Fatalf("outcome %+v, want the typed exit 7", o)
				}
				if !strings.Contains(out.String(), da1Request+"A") {
					t.Fatalf("wrote %q: the typed raw step did not run first after the probe", out.String())
				}
				var want []string
				if c.answered {
					want = []string{"state=working"}
				}
				if got := bodies(events(t, out)); !slices.Equal(got, want) {
					t.Fatalf("reported %q, want %q", got, want)
				}
			})
		}
	}
}

// Kills a line reader that hands a step a terminal reply arriving after the
// handshake ended: a DA1 reply past the probe's wait, or a 7501 reply after
// the DA1 reply.
func TestEmitterLinesDropLateReplies(t *testing.T) {
	in := newInput(strings.NewReader("raw 41\r\x1b[?62;22cexit 7\x1b]7501;?\x07\r\x1b]7501;state=idle\x1b\\"))
	var got []string
	for {
		text, ok, err := in.line(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		got = append(got, text)
	}
	if !slices.Equal(got, []string{"raw 41", "exit 7"}) {
		t.Fatalf("lines %q, want the typed lines without the replies", got)
	}
}

// Kills a force-env (pi-like) program that reports without its variable,
// stays silent with it, or probes when forced.
func TestEmitterForceEnv(t *testing.T) {
	out, _ := run(t, nil, map[string]string{"PI_OSC7501": "1"}, "force-env PI_OSC7501", "probe-gated", "report state=idle")
	evs := events(t, out)
	if len(evs) != 1 || evs[0].Kind != programstatus.EventReport {
		t.Fatalf("forced program events %+v, want one report, no probe", evs)
	}
	out, _ = run(t, nil, nil, "force-env PI_OSC7501", "report state=idle", "raw 41")
	if out.String() != "A" {
		t.Fatalf("unforced program wrote %q, want only the raw byte", out.String())
	}
}

// Kills exit and signal steps that do not end the program, or end it with
// another outcome.
func TestEmitterExitAndSignal(t *testing.T) {
	out, o := run(t, nil, nil, "report state=error", "exit 3", "report state=idle")
	if o != (Outcome{Ended: true, Exit: 3}) || len(bodies(events(t, out))) != 1 {
		t.Fatalf("exit: %+v after %q", o, out.String())
	}
	if _, o := run(t, nil, nil, "signal TERM"); o != (Outcome{Ended: true, Signal: syscall.SIGTERM}) {
		t.Fatalf("signal: %+v", o)
	}
	if _, o := run(t, nil, nil, "signal sighup"); o.Signal != syscall.SIGHUP {
		t.Fatalf("signal sighup: %+v", o)
	}
}

// Kills on-line and interactive modes that do not run per typed line, or
// that miss lines ended by CR (what tmux's Enter sends).
func TestEmitterOnLineAndInteractive(t *testing.T) {
	p, err := ParseLines("on-line turn.script")
	if err != nil {
		t.Fatal(err)
	}
	out := &writes{}
	files := map[string][]byte{"turn.script": []byte("report state=working\nreport state=idle\n")}
	_, err = Run(t.Context(), p, Terminal{
		In:       strings.NewReader("first\rsecond\n"),
		Out:      out,
		ReadFile: func(name string) ([]byte, error) { return files[name], nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(events(t, out)); len(got) != 4 {
		t.Fatalf("two typed lines ran %d reports, want 4", len(got))
	}

	out2, o := run(t, strings.NewReader("report state=working\r\n# note\nclear\nexit 7\nreport state=idle\n"), nil, "interactive")
	if got := bodies(events(t, out2)); !slices.Equal(got, []string{"state=working", "state=clear"}) || o.Exit != 7 {
		t.Fatalf("interactive reported %q, exit %d", got, o.Exit)
	}
}

// Kills a hang that returns before its context ends, or a run that ignores
// cancellation.
func TestEmitterHangUntilCanceled(t *testing.T) {
	p, err := ParseLines("report state=working", "hang", "report state=idle")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	out := &writes{}
	if _, err := Run(ctx, p, Terminal{Out: out}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hang returned %v, want the context's deadline", err)
	}
	if got := bodies(events(t, out)); !slices.Equal(got, []string{"state=working"}) {
		t.Fatalf("hung program wrote %q: stale working with no heartbeat, then nothing", got)
	}
}

// Kills a parser that accepts a malformed script.
func TestParseRejectsMalformedSteps(t *testing.T) {
	for _, line := range []string{
		"bogus", "report", "raw zz", "split 0", "split x", "term crlf", "sleep soon",
		"probe-gated never", "exit 256", "exit -1", "signal STOP", "signal QUIT", "signal USR1", "signal USR2",
		"oversize 5", "hang now", "clear all",
	} {
		if _, err := ParseLines(line); !errors.Is(err, ErrScript) {
			t.Errorf("ParseLines(%q) err = %v, want ErrScript", line, err)
		}
	}
	if _, err := Parse(bytes.NewReader([]byte("# comment\n\nreport state=idle\n"))); err != nil {
		t.Fatalf("comments and blanks: %v", err)
	}
}
