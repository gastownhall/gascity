//go:build linux || darwin

package relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/gastownhall/gascity/internal/runtime/runtimetest/osc7501emitter/emit"
)

// helperEnv makes the test binary run as the relayed program (a helper mode
// named by its first argument) instead of running tests.
const helperEnv = "GC_OSC7501_RELAY_TEST_HELPER"

// helperModes are the programs the tests relay. Each runs on the pty the
// relay gave it and returns its exit code.
var helperModes = map[string]func(args []string) int{
	"emit":        helperEmit,
	"exit":        helperExit,
	"die":         helperDie,
	"transparent": helperTransparent,
	"winsize":     helperWinsize,
	"signals":     helperSignals,
	"wait":        helperWait,
	"rtt":         helperRTT,
	"echo":        helperEcho,
	"burst":       helperBurst,
}

// runHelper runs the helper mode os.Args names.
func runHelper() int {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "relay helper: no mode")
		return 2
	}
	mode, ok := helperModes[os.Args[1]]
	if !ok {
		fmt.Fprintln(os.Stderr, "relay helper: unknown mode", os.Args[1])
		return 2
	}
	return mode(os.Args[2:])
}

// helperFail reports a helper failure on its terminal, where the test sees
// it, and returns the failure exit code.
func helperFail(format string, a ...any) int {
	_, _ = fmt.Fprintf(os.Stdout, "\r\nHELPER-FAIL "+format+"\r\n", a...) // the exit code reports it too
	return 99
}

// makeRaw puts the helper's terminal in raw mode.
func makeRaw() (func(), error) {
	old, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(int(os.Stdin.Fd()), old) }, nil
}

// helperEmit runs emitter script steps, as the osc7501emitter binary does.
func helperEmit(args []string) int {
	prog, err := emit.ParseLines(args...)
	if err != nil {
		return helperFail("%v", err)
	}
	out, err := emit.Run(context.Background(), prog, emit.Terminal{
		In:         os.Stdin,
		Out:        os.Stdout,
		Getenv:     os.Getenv,
		RawInput:   makeRaw,
		SplitPause: 5 * time.Millisecond,
	})
	if err != nil {
		return helperFail("%v", err)
	}
	if out.Signal != 0 {
		dieBySignal(out.Signal)
	}
	return out.Exit
}

// helperExit exits with the code given.
func helperExit(args []string) int {
	code, err := strconv.Atoi(args[0])
	if err != nil {
		return helperFail("%v", err)
	}
	return code
}

// helperDie dies by the signal number given.
func helperDie(args []string) int {
	n, err := strconv.Atoi(args[0])
	if err != nil {
		return helperFail("%v", err)
	}
	dieBySignal(syscall.Signal(n))
	return helperFail("survived signal %d", n)
}

// helperTransparent writes outBytes of seeded pseudo-random output after
// READY while it reads inBytes of input, then writes the input's SHA-256.
func helperTransparent(args []string) int {
	outBytes, _ := strconv.Atoi(args[0])
	inBytes, _ := strconv.Atoi(args[1])
	seed, _ := strconv.ParseUint(args[2], 10, 64)
	restore, err := makeRaw()
	if err != nil {
		return helperFail("%v", err)
	}
	defer restore()
	if _, err := os.Stdout.WriteString("READY\n"); err != nil {
		return 98
	}
	wrote := make(chan error, 1)
	go func() {
		_, err := io.Copy(os.Stdout, io.LimitReader(newPattern(seed), int64(outBytes)))
		wrote <- err
	}()
	h := sha256.New()
	if _, err := io.CopyN(h, os.Stdin, int64(inBytes)); err != nil {
		return helperFail("reading input: %v", err)
	}
	if err := <-wrote; err != nil {
		return 98
	}
	if _, err := fmt.Fprintf(os.Stdout, "IN %s\n", hex.EncodeToString(h.Sum(nil))); err != nil {
		return 98
	}
	return 0
}

// newPattern is the transparency test's output: every byte value, seeded,
// with no ESC so it never forms a probe.
func newPattern(seed uint64) io.Reader {
	return &pattern{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

type pattern struct{ r *rand.Rand }

func (p *pattern) Read(b []byte) (int, error) {
	for i := range b {
		c := byte(p.r.Uint32())
		if c == 0x1b {
			c = 0x1a
		}
		b[i] = c
	}
	return len(b), nil
}

// helperWinsize prints its terminal's size, then again after each of the
// SIGWINCHes it is told to expect.
func helperWinsize(args []string) int {
	changes, _ := strconv.Atoi(args[0])
	winch := make(chan os.Signal, 4)
	signal.Notify(winch, unix.SIGWINCH)
	report := func() bool {
		ws, err := unix.IoctlGetWinsize(int(os.Stdin.Fd()), unix.TIOCGWINSZ)
		if err != nil {
			helperFail("%v", err)
			return false
		}
		_, err = fmt.Fprintf(os.Stdout, "SIZE %dx%d\n", ws.Col, ws.Row)
		return err == nil
	}
	if !report() {
		return 98
	}
	for range changes {
		<-winch
		if !report() {
			return 98
		}
	}
	return 0
}

// helperSignals prints each catchable stop signal it receives, and exits
// after the count it is told to expect.
func helperSignals(args []string) int {
	n, _ := strconv.Atoi(args[0])
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, unix.SIGTERM, unix.SIGHUP, unix.SIGINT, unix.SIGQUIT)
	if _, err := os.Stdout.WriteString("READY\n"); err != nil {
		return 98
	}
	for range n {
		sig := <-sigs
		if _, err := fmt.Fprintf(os.Stdout, "GOT %s\n", unix.SignalName(sig.(syscall.Signal))); err != nil {
			return 98
		}
	}
	return 0
}

// helperWait prints READY and reads its terminal until the read fails.
func helperWait([]string) int {
	if _, err := os.Stdout.WriteString("READY\n"); err != nil {
		return 98
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 3
}

// helperRTT probes n times (probe then DA1, as Claude does) and prints the
// p50 and p99 of the time from writing the probe to reading the 7501 reply,
// and how many times the 7501 reply came before the DA1 reply.
func helperRTT(args []string) int {
	n, _ := strconv.Atoi(args[0])
	restore, err := makeRaw()
	if err != nil {
		return helperFail("%v", err)
	}
	defer restore()
	const query = "\x1b]7501;?\x07\x1b[c"
	rtts := make([]time.Duration, 0, n)
	first := 0
	buf := make([]byte, 256)
	for range n {
		var got []byte
		start := time.Now()
		if _, err := os.Stdout.WriteString(query); err != nil {
			return 98
		}
		var rtt time.Duration
		for {
			k, err := os.Stdin.Read(buf)
			if err != nil {
				return helperFail("reading replies: %v", err)
			}
			got = append(got, buf[:k]...)
			if rtt == 0 && bytes.Contains(got, []byte(ProbeReply)) {
				rtt = time.Since(start)
			}
			if bytes.HasSuffix(got, []byte("c")) && bytes.Contains(got, []byte("\x1b[?")) {
				break
			}
		}
		if rtt == 0 {
			return helperFail("no 7501 reply in %q", got)
		}
		if bytes.Index(got, []byte(ProbeReply)) < bytes.Index(got, []byte("\x1b[?")) {
			first++
		}
		rtts = append(rtts, rtt)
	}
	slices.Sort(rtts)
	_, err = fmt.Fprintf(os.Stdout, "RTT %d %d %d\n", rtts[len(rtts)/2].Nanoseconds(), rtts[len(rtts)*99/100].Nanoseconds(), first)
	if err != nil {
		return 98
	}
	return 0
}

// helperEcho echoes its raw input back until it reads Q.
func helperEcho([]string) int {
	restore, err := makeRaw()
	if err != nil {
		return helperFail("%v", err)
	}
	defer restore()
	if _, err := os.Stdout.WriteString("READY\n"); err != nil {
		return 98
	}
	buf := make([]byte, 4096)
	for {
		k, err := os.Stdin.Read(buf)
		if k > 0 {
			if i := bytes.IndexByte(buf[:k], 'Q'); i >= 0 {
				_, _ = os.Stdout.Write(buf[:i])
				return 0
			}
			if _, werr := os.Stdout.Write(buf[:k]); werr != nil {
				return 98
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0
			}
			return helperFail("%v", err)
		}
	}
}

// helperBurst writes n bytes as fast as it can and exits at once.
func helperBurst(args []string) int {
	n, _ := strconv.Atoi(args[0])
	restore, err := makeRaw()
	if err != nil {
		return helperFail("%v", err)
	}
	defer restore()
	if _, err := os.Stdout.Write(bytes.Repeat([]byte("b"), n)); err != nil {
		return 98
	}
	return 0
}
