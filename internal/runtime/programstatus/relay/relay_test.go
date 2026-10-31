//go:build linux || darwin

package relay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/gastownhall/gascity/internal/runtime/runtimetest/osc7501emitter/emit"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		os.Exit(runHelper())
	}
	os.Exit(m.Run())
}

// waitLimit bounds every wait on the relayed program.
const waitLimit = 20 * time.Second

// harness runs Run on an outer pty pair the test plays the pane of: it
// reads everything the relay writes to the pane and types into it.
type harness struct {
	t      *testing.T
	pane   *os.File // the outer pty master: the test's side of the pane
	tty    *os.File // the outer pty slave: the relay's stdin and stdout
	sigs   chan os.Signal
	done   chan runResult
	mu     sync.Mutex
	out    []byte
	grew   chan struct{} // closed and replaced whenever out grows
	closed bool
}

type runResult struct {
	st  Status
	err error
}

// harnessOptions shape one relay run.
type harnessOptions struct {
	size        unix.Winsize
	answerDA1   bool // answer DA1 requests in the output, as tmux does
	forwarded   func(time.Duration)
	beforeStart func(h *harness)
}

// startRelay relays the test binary as the program, in the helper mode
// given.
func startRelay(t *testing.T, o harnessOptions, mode string, args ...string) *harness {
	t.Helper()
	pane, tty, err := OpenPTY()
	if err != nil {
		t.Fatalf("outer pty: %v", err)
	}
	if o.size.Row == 0 {
		o.size = unix.Winsize{Row: 24, Col: 80}
	}
	if err := setSize(pane, &o.size); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, pane: pane, tty: tty, sigs: make(chan os.Signal, 4), done: make(chan runResult, 1), grew: make(chan struct{})}
	t.Cleanup(func() {
		_ = pane.Close()
		_ = tty.Close()
	})
	go h.read(o.answerDA1)
	if o.beforeStart != nil {
		o.beforeStart(h)
	}
	opts := Options{
		Argv:      append([]string{os.Args[0], mode}, args...),
		Env:       helperEnviron(),
		Stdin:     tty,
		Stdout:    tty,
		Signals:   h.sigs,
		forwarded: o.forwarded,
	}
	go func() {
		st, err := Run(opts)
		h.done <- runResult{st, err}
	}()
	return h
}

// helperEnviron is the relayed program's environment: the test's, marked to
// run a helper, without TEST_TIMEOUT, whose presence makes a rules_go test
// binary ignore SIGTERM (bzltestutil.RegisterTimeoutHandler); the program
// must die by SIGTERM as an agent would.
func helperEnviron() []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "TEST_TIMEOUT=") })
	return append(env, helperEnv+"=1")
}

// read collects the pane's output, answering DA1 when asked to.
func (h *harness) read(answerDA1 bool) {
	buf := make([]byte, 64<<10)
	scanned := 0
	for {
		n, err := h.pane.Read(buf)
		if n > 0 {
			h.mu.Lock()
			h.out = append(h.out, buf[:n]...)
			var answers int
			if answerDA1 {
				answers = bytes.Count(h.out[max(scanned-len(da1Request)+1, 0):], []byte(da1Request))
				scanned = len(h.out)
			}
			close(h.grew)
			h.grew = make(chan struct{})
			h.mu.Unlock()
			for range answers {
				if _, err := h.pane.WriteString(da1Reply); err != nil {
					return
				}
			}
		}
		if err != nil {
			h.mu.Lock()
			h.closed = true
			close(h.grew)
			h.grew = make(chan struct{})
			h.mu.Unlock()
			return
		}
	}
}

// output is everything the pane has received.
func (h *harness) output() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.out)
}

// waitOutput waits until the pane's output satisfies ok.
func (h *harness) waitOutput(what string, ok func([]byte) bool) []byte {
	h.t.Helper()
	deadline := time.NewTimer(waitLimit)
	defer deadline.Stop()
	for {
		h.mu.Lock()
		out, grew, closed := slices.Clone(h.out), h.grew, h.closed
		h.mu.Unlock()
		if ok(out) {
			return out
		}
		if bytes.Contains(out, []byte("HELPER-FAIL")) {
			h.t.Fatalf("waiting for %s: the program failed: %q", what, tail(out))
		}
		if closed {
			h.t.Fatalf("waiting for %s: the pane closed; output %q", what, tail(out))
		}
		select {
		case <-grew:
		case <-deadline.C:
			h.t.Fatalf("timed out waiting for %s; output %q", what, tail(out))
		}
	}
}

// waitFor waits until the pane's output contains s.
func (h *harness) waitFor(s string) []byte {
	h.t.Helper()
	return h.waitOutput(strconv.Quote(s), func(b []byte) bool { return bytes.Contains(b, []byte(s)) })
}

// typeIn writes b into the pane's input.
func (h *harness) typeIn(b []byte) {
	h.t.Helper()
	if _, err := h.pane.Write(b); err != nil {
		h.t.Fatalf("typing: %v", err)
	}
}

// result waits for Run to return.
func (h *harness) result() (Status, error) {
	h.t.Helper()
	select {
	case r := <-h.done:
		return r.st, r.err
	case <-time.After(waitLimit):
		h.t.Fatalf("relay did not return; output %q", tail(h.output()))
		return Status{}, nil
	}
}

// status waits for Run to return a status.
func (h *harness) status() Status {
	h.t.Helper()
	st, err := h.result()
	if err != nil {
		h.t.Fatalf("Run: %v", err)
	}
	return st
}

func tail(b []byte) []byte {
	if len(b) > 512 {
		return b[len(b)-512:]
	}
	return b
}

// probeGated is the emitter script of a Claude-like program: BEL
// terminated, reporting only when its probe is answered before DA1.
var probeGated = []string{"term bel", "probe-gated 5s", "report state=working", "raw 444f4e450a"} // DONE\n

func TestRelayAnswersProbeBeforeForwardingDA1(t *testing.T) {
	h := startRelay(t, harnessOptions{answerDA1: true}, "emit", probeGated...)
	out := h.waitFor("DONE\r\n")
	if st := h.status(); st != (Status{}) {
		t.Fatalf("status %+v, want exit 0", st)
	}
	if !bytes.Contains(out, []byte("\x1b]7501;state=working\x07")) {
		t.Fatalf("the probe-gated program did not report: the DA1 reply beat the relay's 7501 reply; output %q", out)
	}
	// The probe itself still reaches the pane: nothing is filtered.
	if !bytes.Contains(out, []byte("\x1b]7501;?\x07\x1b[c")) {
		t.Fatalf("probe and DA1 request not forwarded verbatim; output %q", out)
	}
}

func TestRelayAnswersBELAndSTProbes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []string
	}{
		{"bel", []string{"term bel"}},
		{"st", []string{"term st"}},
		{"bel split across reads", []string{"term bel", "split 4"}},
		{"st split inside its terminator", []string{"term st", "split 3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := append(slices.Clone(tc.steps), "probe-gated 5s", "report state=idle", "raw 444f4e450a")
			h := startRelay(t, harnessOptions{answerDA1: true}, "emit", steps...)
			out := h.waitFor("DONE\r\n")
			h.status()
			if !bytes.Contains(out, []byte("\x1b]7501;state=idle")) {
				t.Fatalf("probe not answered; output %q", out)
			}
		})
	}
}

// Without an answer before DA1 the same program stays silent: the relay is
// what makes it report (the control for the two tests above). The emitter
// runs in process on a plain pty whose pane answers DA1 as tmux does.
func TestProbeGatedProgramIsSilentWithoutRelay(t *testing.T) {
	pane, tty, err := OpenPTY()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pane.Close()
		_ = tty.Close()
	})
	h := &harness{t: t, pane: pane, tty: tty, grew: make(chan struct{})}
	go h.read(true)
	prog, err := emit.ParseLines(probeGated...)
	if err != nil {
		t.Fatal(err)
	}
	raw := func() (func(), error) {
		var old *term.State
		err := control(tty, func(fd int) error {
			var err error
			old, err = term.MakeRaw(fd)
			return err
		})
		return func() { _ = control(tty, func(fd int) error { return term.Restore(fd, old) }) }, err
	}
	if _, err := emit.Run(t.Context(), prog, emit.Terminal{In: tty, Out: tty, RawInput: raw}); err != nil {
		t.Fatalf("emitter: %v", err)
	}
	out := h.waitFor("DONE\r\n")
	if !bytes.Contains(out, []byte("\x1b]7501;?\x07")) {
		t.Fatalf("the program did not probe; output %q", out)
	}
	if bytes.Contains(out, []byte("state=working")) {
		t.Fatalf("the program reported with no 7501 answer; output %q", out)
	}
}

func TestRelayIsByteTransparent(t *testing.T) {
	const outBytes, inBytes, seed = 4 << 20, 1 << 20, 7501
	h := startRelay(t, harnessOptions{}, "transparent", strconv.Itoa(outBytes), strconv.Itoa(inBytes), strconv.Itoa(seed))
	h.waitFor("READY\n")
	// Type every byte value, interleaved with the program's output.
	input := make([]byte, inBytes)
	for i := range input {
		input[i] = byte(i * 131)
	}
	typed := make(chan error, 1)
	go func() {
		for b := input; len(b) > 0; {
			n := min(len(b), 997)
			if _, err := h.pane.Write(b[:n]); err != nil {
				typed <- err
				return
			}
			b = b[n:]
		}
		typed <- nil
	}()
	sum := sha256.Sum256(input)
	trailer := "IN " + hex.EncodeToString(sum[:]) + "\n"
	out := h.waitOutput("output and input hash", func(b []byte) bool {
		return len(b) >= len("READY\n")+outBytes+len(trailer) || bytes.Contains(b, []byte("HELPER-FAIL"))
	})
	if err := <-typed; err != nil {
		t.Fatalf("typing: %v", err)
	}
	if st := h.status(); st != (Status{}) {
		t.Fatalf("status %+v; output tail %q", st, tail(h.output()))
	}
	want, err := io.ReadAll(io.LimitReader(newPattern(seed), outBytes))
	if err != nil {
		t.Fatal(err)
	}
	got := out[len("READY\n"):]
	if len(got) != outBytes+len(trailer) {
		t.Fatalf("pane got %d output bytes, want %d", len(got), outBytes+len(trailer))
	}
	if !bytes.Equal(got[:outBytes], want) {
		i := 0
		for i < outBytes && got[i] == want[i] {
			i++
		}
		t.Fatalf("output differs at byte %d: got %q, want %q", i, got[i:min(i+16, outBytes)], want[i:min(i+16, outBytes)])
	}
	if string(got[outBytes:]) != trailer {
		t.Fatalf("program read input with hash line %q, want %q: input was not relayed verbatim", got[outBytes:], trailer)
	}
}

func TestRelayPropagatesExitCodeAndSignal(t *testing.T) {
	t.Run("exit code", func(t *testing.T) {
		h := startRelay(t, harnessOptions{}, "exit", "7")
		if st := h.status(); st != (Status{Code: 7}) {
			t.Fatalf("status %+v, want exit 7", st)
		}
	})
	for _, sig := range []syscall.Signal{unix.SIGUSR1, unix.SIGTERM, unix.SIGQUIT, unix.SIGSEGV} {
		t.Run(unix.SignalName(sig), func(t *testing.T) {
			h := startRelay(t, harnessOptions{}, "die", strconv.Itoa(int(sig)))
			if st := h.status(); st != (Status{Signal: sig}) {
				t.Fatalf("status %+v, want death by %v; output %q", st, sig, h.output())
			}
		})
	}
}

func TestRelayDrainsOutputAfterProgramExits(t *testing.T) {
	const n = 256 << 10
	h := startRelay(t, harnessOptions{}, "burst", strconv.Itoa(n))
	st := h.status()
	if st != (Status{}) {
		t.Fatalf("status %+v", st)
	}
	out := h.waitOutput("the whole burst", func(b []byte) bool { return len(b) >= n })
	if len(out) != n || bytes.Count(out, []byte("b")) != n {
		t.Fatalf("pane got %d bytes, want %d b's", len(out), n)
	}
}

func TestRelayForwardsWindowSize(t *testing.T) {
	h := startRelay(t, harnessOptions{size: unix.Winsize{Row: 41, Col: 133}}, "winsize", "1")
	h.waitFor("SIZE 133x41\r\n")
	if err := setSize(h.pane, &unix.Winsize{Row: 50, Col: 211}); err != nil {
		t.Fatal(err)
	}
	h.sigs <- unix.SIGWINCH
	h.waitFor("SIZE 211x50\r\n")
	if st := h.status(); st != (Status{}) {
		t.Fatalf("status %+v", st)
	}
}

func TestRelayForwardsSignals(t *testing.T) {
	sigs := []syscall.Signal{unix.SIGTERM, unix.SIGHUP, unix.SIGINT, unix.SIGQUIT}
	h := startRelay(t, harnessOptions{}, "signals", strconv.Itoa(len(sigs)))
	h.waitFor("READY\r\n")
	for _, sig := range sigs {
		h.sigs <- sig
		h.waitFor("GOT " + unix.SignalName(sig) + "\r\n")
	}
	if st := h.status(); st != (Status{}) {
		t.Fatalf("status %+v", st)
	}
}

func TestRelayRestoresTerminal(t *testing.T) {
	var before *unix.Termios
	h := startRelay(t, harnessOptions{beforeStart: func(h *harness) {
		before = termios(t, h.tty)
	}}, "wait")
	h.waitFor("READY\r\n")
	during := termios(t, h.tty)
	if during.Lflag&(unix.ICANON|unix.ECHO|unix.ISIG) != 0 || during.Iflag&unix.IXON != 0 {
		t.Fatalf("pane not raw while relaying: lflag %#x iflag %#x", during.Lflag, during.Iflag)
	}
	// The program dies by the SIGTERM the relay forwards.
	h.sigs <- unix.SIGTERM
	if st := h.status(); st != (Status{Signal: unix.SIGTERM}) {
		t.Fatalf("status %+v, want death by SIGTERM", st)
	}
	after := termios(t, h.tty)
	if *after != *before {
		t.Fatalf("terminal not restored:\nbefore %+v\nafter  %+v", *before, *after)
	}
}

// The program's pty starts with the pane's modes, not the pty defaults: a
// pane without output newline translation gives a program without it.
func TestRelayGivesProgramThePanesModes(t *testing.T) {
	h := startRelay(t, harnessOptions{beforeStart: func(h *harness) {
		tio := termios(t, h.tty)
		tio.Oflag &^= unix.ONLCR
		err := control(h.tty, func(fd int) error { return unix.IoctlSetTermios(fd, ioctlSetTermios, tio) })
		if err != nil {
			t.Fatal(err)
		}
	}}, "wait")
	out := h.waitFor("READY\n")
	if bytes.Contains(out, []byte("READY\r\n")) {
		t.Fatalf("program output translated %q: its pty did not inherit the pane's modes", out)
	}
	h.sigs <- unix.SIGTERM
	h.status()
}

func TestRelayHangsUpProgramWhenPaneGoes(t *testing.T) {
	h := startRelay(t, harnessOptions{}, "wait")
	h.waitFor("READY\r\n")
	_ = h.pane.Close() // the pane goes: the relay's stdin read fails
	if st := h.status(); st != (Status{Signal: unix.SIGHUP}) && st != (Status{Code: 3}) {
		t.Fatalf("status %+v, want the program hung up", st)
	}
}

func TestRelayStartFailureLeavesTerminalAlone(t *testing.T) {
	pane, tty, err := OpenPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer pane.Close() //nolint:errcheck // test cleanup
	defer tty.Close()  //nolint:errcheck // test cleanup
	before := termios(t, tty)
	_, err = Run(Options{Argv: []string{"/nonexistent/program"}, Stdin: tty, Stdout: tty})
	var se *StartError
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "/nonexistent/program") {
		t.Fatalf("Run = %v, want a StartError naming the program", err)
	}
	if after := termios(t, tty); *after != *before {
		t.Fatal("a failed start left the terminal changed")
	}
	if _, err := Run(Options{Stdin: tty, Stdout: tty}); !errors.As(err, &se) {
		t.Fatalf("Run with no argv = %v, want a StartError", err)
	}
}

func TestRelayNoTTYReportsNotTerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck // test cleanup
	defer w.Close() //nolint:errcheck // test cleanup
	if _, err := Run(Options{Argv: []string{"true"}, Stdin: r, Stdout: w}); !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("Run on a pipe = %v, want ErrNotTerminal", err)
	}
}

// Budgets (design §4.3): added forward latency of 1 ms or less at p99 per
// chunk, and a DA1-gated probe round trip of 0.25 ms or less at p50.
const (
	forwardP99Budget = time.Millisecond
	probeP50Budget   = 250 * time.Microsecond
)

func TestRelayLatencyBudget(t *testing.T) {
	var mu sync.Mutex
	var lat []time.Duration
	h := startRelay(t, harnessOptions{forwarded: func(d time.Duration) {
		mu.Lock()
		lat = append(lat, d)
		mu.Unlock()
	}}, "echo")
	h.waitFor("READY\n")
	const rounds = 2000
	msg := []byte("keystroke-sized chunk 0123456789\n")
	start := len(h.output())
	for i := range rounds {
		h.typeIn(msg)
		want := start + (i+1)*len(msg)
		h.waitOutput("echo", func(b []byte) bool { return len(b) >= want })
	}
	h.typeIn([]byte("Q"))
	h.status()
	mu.Lock()
	defer mu.Unlock()
	slices.Sort(lat)
	p50, p99 := lat[len(lat)/2], lat[len(lat)*99/100]
	t.Logf("relay forward latency over %d chunks: p50 %v, p99 %v, max %v", len(lat), p50, p99, lat[len(lat)-1])
	if p99 > forwardP99Budget {
		t.Fatalf("forward latency p99 %v over the %v budget", p99, forwardP99Budget)
	}
}

func TestRelayProbeRoundTripBudget(t *testing.T) {
	const rounds = 400
	h := startRelay(t, harnessOptions{answerDA1: true}, "rtt", strconv.Itoa(rounds))
	out := h.waitOutput("RTT line", func(b []byte) bool {
		i := bytes.Index(b, []byte("RTT "))
		return i >= 0 && bytes.IndexByte(b[i:], '\n') >= 0
	})
	h.status()
	_, line, _ := bytes.Cut(out, []byte("RTT "))
	line, _, _ = bytes.Cut(line, []byte("\n"))
	var p50, p99 time.Duration
	var first int
	if _, err := fmt.Sscanf(string(line), "%d %d %d", &p50, &p99, &first); err != nil {
		t.Fatalf("parsing %q: %v", line, err)
	}
	t.Logf("probe round trip over %d probes: p50 %v, p99 %v; 7501 reply first %d/%d", rounds, p50, p99, first, rounds)
	if first != rounds {
		t.Fatalf("the 7501 reply came after DA1's in %d of %d probes", rounds-first, rounds)
	}
	if p50 > probeP50Budget {
		t.Fatalf("probe round trip p50 %v over the %v budget", p50, probeP50Budget)
	}
}

// termios reads f's terminal modes without taking f out of the poller.
func termios(t *testing.T, f *os.File) *unix.Termios {
	t.Helper()
	var tio *unix.Termios
	err := control(f, func(fd int) error {
		var err error
		tio, err = unix.IoctlGetTermios(fd, ioctlGetTermios)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tio
}
