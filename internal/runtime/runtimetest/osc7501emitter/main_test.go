package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/runtimetest/osc7501emitter/emit"
)

// emitterHelperEnv makes the test binary run the emitter's main on its
// arguments instead of its tests, so a test can watch how the process ends.
const emitterHelperEnv = "GC_OSC7501EMITTER_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(emitterHelperEnv) == "1" {
		main() // exits, or ends by a signal
	}
	os.Exit(m.Run())
}

// emitterProcess is the test binary started as the emitter.
type emitterProcess struct {
	cmd    *exec.Cmd
	out    *bufio.Reader
	stderr bytes.Buffer
}

// startEmitter starts the emitter on args, its output on p.out.
func startEmitter(t *testing.T, args ...string) *emitterProcess {
	t.Helper()
	p := &emitterProcess{cmd: exec.Command(os.Args[0], args...)}
	p.cmd.Env = append(os.Environ(), emitterHelperEnv+"=1")
	p.cmd.Stderr = &p.stderr
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("starting the emitter: %v", err)
	}
	t.Cleanup(func() {
		if p.cmd.ProcessState == nil {
			// The test failed before waiting: stop and reap the emitter.
			_ = p.cmd.Process.Kill()
			_ = p.cmd.Wait()
		}
	})
	p.out = bufio.NewReader(stdout)
	return p
}

// deathSignal waits for p and returns the signal it died by, failing the
// test when it exited instead.
func (p *emitterProcess) deathSignal(t *testing.T) syscall.Signal {
	t.Helper()
	err := p.cmd.Wait()
	status, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		t.Fatalf("the emitter ended %v (%v), want death by a signal; stderr:\n%s", p.cmd.ProcessState, err, p.stderr.String())
	}
	return status.Signal()
}

// Kills an emitter process that does not die by its signal step's signal,
// or by the signal that ended its run: a pane reads that death signal as
// the program's own.
func TestEmitterProcessDiesByItsSignal(t *testing.T) {
	for _, c := range []struct {
		name string
		sig  syscall.Signal
	}{{"TERM", syscall.SIGTERM}, {"INT", syscall.SIGINT}, {"HUP", syscall.SIGHUP}, {"KILL", syscall.SIGKILL}} {
		t.Run("step "+c.name, func(t *testing.T) {
			skipIfIgnored(t, c.sig)
			if got := startEmitter(t, "-e", "signal "+c.name).deathSignal(t); got != c.sig {
				t.Fatalf("signal %s step: died by %v", c.name, got)
			}
		})
		if c.sig == syscall.SIGKILL {
			continue // nothing catches KILL
		}
		t.Run("received "+c.name, func(t *testing.T) {
			skipIfIgnored(t, c.sig)
			p := startEmitter(t, "-e", "report state=working", "-e", "hang")
			// The run writes the report after the emitter starts catching signals.
			if _, err := p.out.ReadString('\\'); err != nil {
				t.Fatalf("reading the report: %v; stderr:\n%s", err, p.stderr.String())
			}
			if err := p.cmd.Process.Signal(c.sig); err != nil {
				t.Fatal(err)
			}
			if got := p.deathSignal(t); got != c.sig {
				t.Fatalf("a hung emitter sent %v died by %v", c.sig, got)
			}
		})
	}
}

// skipIfIgnored skips when this process ignores sig: the emitter inherits
// the ignore, and Go restores an inherited ignore of INT or HUP when it
// resets their handling, so the emitter could not die by sig.
func skipIfIgnored(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if signal.Ignored(sig) {
		t.Skipf("%v is ignored in this process, and the emitter would inherit that", sig)
	}
}

// Kills a loader that accepts both a script file and inline steps, neither,
// or two files, or that does not parse what it loads.
func TestLoadTakesAScriptFileOrInlineSteps(t *testing.T) {
	script := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(script, []byte("report state=idle\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := load([]string{script}, nil); err != nil {
		t.Fatalf("script file: %v", err)
	}
	if _, err := load(nil, steps{"report state=idle", "hang"}); err != nil {
		t.Fatalf("inline steps: %v", err)
	}
	for name, c := range map[string]struct {
		args   []string
		inline steps
	}{
		"neither":       {},
		"both":          {[]string{script}, steps{"hang"}},
		"two files":     {[]string{script, script}, nil},
		"missing file":  {[]string{filepath.Join(t.TempDir(), "absent")}, nil},
		"bad inline":    {nil, steps{"bogus"}},
		"bad file line": {[]string{writeScript(t, "term crlf\n")}, nil},
	} {
		if _, err := load(c.args, c.inline); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	if _, err := load(nil, steps{"bogus"}); !errors.Is(err, emit.ErrScript) {
		t.Errorf("bad inline step: %v, want emit.ErrScript", err)
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
