//go:build integration

package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/runtime/programstatus/relay"
)

// emitter returns the scripted OSC 7501 emitter binary: the one Bazel built,
// else one built here.
func emitter(t *testing.T) string {
	t.Helper()
	if bin := bazeltest.DataPath(t, "GC_TEST_OSC7501_EMITTER_BIN"); bin != "" {
		return bin
	}
	bin := filepath.Join(t.TempDir(), "osc7501emitter")
	cmd := exec.Command("go", "build", "-o", bin, "../../internal/runtime/runtimetest/osc7501emitter")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building osc7501emitter: %v\n%s", err, out)
	}
	return bin
}

// shq quotes s for sh.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// shellLine joins argv into one sh command line.
func shellLine(argv ...string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shq(a)
	}
	return strings.Join(q, " ")
}

// emitterArgv is the emitter running steps.
func emitterArgv(bin string, steps ...string) []string {
	argv := []string{bin}
	for _, s := range steps {
		argv = append(argv, "-e", s)
	}
	return argv
}

// TestRelayUnderTmuxMakesProbeGatedProgramReport runs a Claude-like
// probe-gated emitter in two panes of a scratch tmux server, one under the
// relay and one bare, each with its output piped (pipe-pane -O) to a file
// from the first byte. Under the relay the report reaches the pane's output;
// bare, the program stays silent. Neither pane shows any of it on screen,
// and killing the server leaves no process behind.
func TestRelayUnderTmuxMakesProbeGatedProgramReport(t *testing.T) {
	bin, emit := companion(t), emitter(t)
	s := newScratchTmuxNamed(t, "osc7501x-relay-")
	token := randPart(t)
	marker := "DONE-" + token
	steps := []string{"term bel", "probe-gated 5s", "report state=working", "raw " + hex.EncodeToString([]byte(marker+"\r\n")), "hang"}
	program := emitterArgv(emit, steps...)
	dir := t.TempDir()

	start := func(name string, argv []string) (pane, piped string) {
		piped = filepath.Join(dir, name+".out")
		pane = s.must("new-session", "-d", "-P", "-F", "#{pane_id}", "-s", name, "-x", "120", "-y", "30",
			"exec "+shellLine(argv...), ";",
			"pipe-pane", "-O", "-t", "="+name+":", "exec cat >> "+shq(piped))
		return pane, piped
	}
	relayedPane, relayedOut := start("relayed", append([]string{bin, "relay", "--"}, program...))
	barePane, bareOut := start("bare", program)

	readPiped := func(path string) []byte {
		b, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return b
	}
	for _, path := range []string{relayedOut, bareOut} {
		await(t, "the program's marker in "+filepath.Base(path), func() bool {
			return bytes.Contains(readPiped(path), []byte(marker))
		})
	}

	relayed, bare := readPiped(relayedOut), readPiped(bareOut)
	if !bytes.Contains(relayed, []byte("\x1b]7501;?\x07\x1b[c")) || !bytes.Contains(bare, []byte("\x1b]7501;?\x07\x1b[c")) {
		t.Fatalf("a pane's output lacks the probe and DA1 request:\nrelayed %q\nbare %q", relayed, bare)
	}
	if !bytes.Contains(relayed, []byte("\x1b]7501;state=working\x07")) {
		t.Fatalf("under the relay the probe-gated program did not report; pane output %q", relayed)
	}
	if bytes.Contains(bare, []byte("state=working")) {
		t.Fatalf("with no relay the probe-gated program reported; pane output %q", bare)
	}

	for _, pane := range []string{relayedPane, barePane} {
		screen := s.must("capture-pane", "-p", "-J", "-t", pane)
		if !strings.Contains(screen, marker) {
			t.Fatalf("pane %s screen %q lacks the program's marker", pane, screen)
		}
		if strings.Contains(screen, "7501") || strings.Contains(screen, "state=") {
			t.Fatalf("pane %s screen leaks the protocol: %q", pane, screen)
		}
	}

	s.kill()
	if runtime.GOOS == "linux" { // processesNaming reads /proc
		await(t, "every relay and program process to exit", func() bool { return len(processesNaming(t, marker)) == 0 })
	}
}

// processesNaming lists the pids whose command line holds s or its hex.
func processesNaming(t *testing.T, s string) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	hexed := []byte(hex.EncodeToString([]byte(s)))
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue // exited while listing
		}
		if bytes.Contains(cmdline, []byte(s)) || bytes.Contains(cmdline, hexed) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// paneRun is the relay binary running on a pty whose master the test holds
// as the pane.
type paneRun struct {
	t    *testing.T
	cmd  *exec.Cmd
	pane *os.File
	mu   sync.Mutex
	n    int64  // bytes the pane received
	out  []byte // the first outKeep of them
	// eof is closed when the pane's read ends: every holder of the pty's
	// other side closed it, and the pane has received all they wrote.
	eof chan struct{}
}

const outKeep = 64 << 10

// runRelayOnPane starts `gc-osc7501 relay -- argv` with a new pty as its
// controlling terminal, as tmux starts a pane's command.
func runRelayOnPane(t *testing.T, argv ...string) *paneRun {
	t.Helper()
	pane, tty, err := relay.OpenPTY()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(companion(t), append([]string{"relay", "--"}, argv...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = tty.Close() // the relay holds its own copies
	r := &paneRun{t: t, cmd: cmd, pane: pane, eof: make(chan struct{})}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = pane.Close()
	})
	go func() {
		defer close(r.eof)
		buf := make([]byte, 64<<10)
		for {
			n, err := pane.Read(buf)
			r.mu.Lock()
			r.n += int64(n)
			if keep := min(n, outKeep-len(r.out)); keep > 0 {
				r.out = append(r.out, buf[:keep]...)
			}
			r.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return r
}

func (r *paneRun) received() (int64, []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n, bytes.Clone(r.out)
}

// drained waits until the pane has received everything written to it,
// which is only once the relay and its program have exited, and returns it.
func (r *paneRun) drained() []byte {
	r.t.Helper()
	select {
	case <-r.eof:
	case <-time.After(20 * time.Second):
		_, out := r.received()
		r.t.Fatalf("pane read did not end; pane %q", out)
	}
	_, out := r.received()
	return out
}

// waitFor waits until the pane shows s.
func (r *paneRun) waitFor(s string) {
	r.t.Helper()
	await(r.t, strconv.Quote(s), func() bool {
		_, out := r.received()
		return bytes.Contains(out, []byte(s))
	})
}

// wait waits for the relay to exit and returns how it ended.
func (r *paneRun) wait() syscall.WaitStatus {
	r.t.Helper()
	done := make(chan error, 1)
	go func() { done <- r.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_, out := r.received()
		r.t.Fatalf("relay did not exit; pane %q", out)
	}
	return r.cmd.ProcessState.Sys().(syscall.WaitStatus)
}

// The relay process ends as its program did: same exit code, or death by
// the same signal (so #{pane_dead_signal} keeps its fidelity), including the
// signals the Go runtime would otherwise turn into a traceback, and SIGKILL,
// whose disposition cannot be reset. kill(1) signals itself, not the shell:
// bash, macOS's /bin/sh, ignores SIGQUIT.
func TestRelayBinaryEndsAsItsProgramEnded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		code   int
		signal syscall.Signal
	}{
		{"exit 0", "exit 0", 0, 0},
		{"exit 7", "exit 7", 7, 0},
		{"SIGTERM", "exec /bin/kill -s TERM $$", 0, syscall.SIGTERM},
		{"SIGKILL", "exec /bin/kill -s KILL $$", 0, syscall.SIGKILL},
		{"SIGQUIT", "exec /bin/kill -s QUIT $$", 0, syscall.SIGQUIT},
		{"SIGSEGV", "exec /bin/kill -s SEGV $$", 0, syscall.SIGSEGV},
		{"SIGUSR1", "exec /bin/kill -s USR1 $$", 0, syscall.SIGUSR1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := runRelayOnPane(t, "/bin/sh", "-c", tc.script).wait()
			switch {
			case tc.signal != 0 && (!ws.Signaled() || ws.Signal() != tc.signal):
				t.Fatalf("relay ended %#x, want death by %v", ws, tc.signal)
			case tc.signal == 0 && (!ws.Exited() || ws.ExitStatus() != tc.code):
				t.Fatalf("relay ended %#x, want exit %d", ws, tc.code)
			case ws.CoreDump():
				t.Fatalf("relay dumped core re-raising %v", tc.signal)
			}
		})
	}
}

// Signals sent to the relay reach the program; the relay outlives them to
// report the program's end.
func TestRelayBinaryForwardsSignalsToProgram(t *testing.T) {
	emit := emitter(t)
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			r := runRelayOnPane(t, emitterArgv(emit, "raw 5245414459", "hang")...) // READY
			r.waitFor("READY")
			if err := r.cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			// The emitter ends by the signal it received.
			if ws := r.wait(); !ws.Signaled() || ws.Signal() != sig {
				t.Fatalf("relay ended %#x, want the program's death by %v", ws, sig)
			}
		})
	}
}

// With no terminal on stdin there is nothing to relay: the relay execs the
// program in its own place.
func TestRelayBinaryExecsDirectlyWithoutTTY(t *testing.T) {
	cmd := exec.Command(companion(t), "relay", "--", "/bin/sh", "-c", "echo $$")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("relay without a tty: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != strconv.Itoa(cmd.Process.Pid) {
		t.Fatalf("program ran as pid %s, want the relay's own pid %d", got, cmd.Process.Pid)
	}
}

// A program that cannot start: one stderr line, exit 127, as a shell.
func TestRelayBinaryStartFailureExits127(t *testing.T) {
	r := runRelayOnPane(t, "/nonexistent/agent")
	if ws := r.wait(); !ws.Exited() || ws.ExitStatus() != exitStart {
		t.Fatalf("relay ended %#x, want exit %d", ws, exitStart)
	}
	if out := r.drained(); !bytes.Contains(out, []byte("/nonexistent/agent")) || bytes.Count(out, []byte("\n")) != 1 {
		t.Fatalf("pane got %q, want one line naming the program", out)
	}
}

// relayRSSBudget is the relay's peak resident set ceiling (design §4.3).
const relayRSSBudget = 8 << 20

// TestRelayRSSBudget relays 80 MB of program output and checks the relay's
// peak resident set.
func TestRelayRSSBudget(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("peak RSS is read from /proc")
	}
	const size = 80 << 20
	r := runRelayOnPane(t, "/bin/sh", "-c", "head -c "+strconv.Itoa(size)+" /dev/urandom; read line")
	start := time.Now()
	await(t, "80 MB through the relay", func() bool {
		n, _ := r.received()
		return n >= size
	})
	elapsed := time.Since(start)
	hwm, err := peakRSS(r.cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pane.WriteString("\r"); err != nil { // ends the program's read
		t.Fatal(err)
	}
	if ws := r.wait(); !ws.Exited() || ws.ExitStatus() != 0 {
		t.Fatalf("relay ended %#x", ws)
	}
	t.Logf("relay forwarded 80 MB in %v (%.0f MB/s); peak RSS %d KiB", elapsed, float64(size>>20)/elapsed.Seconds(), hwm>>10)
	if hwm > relayRSSBudget {
		t.Fatalf("relay peak RSS %d KiB over the %d KiB budget", hwm>>10, relayRSSBudget>>10)
	}
}
