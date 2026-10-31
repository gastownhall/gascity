//go:build integration && linux

package tmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// captureBinary returns the binary Bazel passes in env, else one built here
// from pkg (relative to this package).
func captureBinary(t *testing.T, env, pkg, name string) string {
	t.Helper()
	if bin := bazeltest.DataPath(t, env); bin != "" {
		return bin
	}
	bin := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", bin, pkg)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", name, err, out)
	}
	return bin
}

func captureCompanionBin(t *testing.T) string {
	return captureBinary(t, "GC_TEST_OSC7501_BIN", "../../../cmd/gc-osc7501", programstatus.CompanionName)
}

func captureEmitterBin(t *testing.T) string {
	return captureBinary(t, "GC_TEST_OSC7501_EMITTER_BIN", "../runtimetest/osc7501emitter", "osc7501emitter")
}

// newCaptureScratchServer starts a tmux server on a brand-new socket
// (osc7501x-arm-<rand>, no config file) and kills exactly that server at
// cleanup. It never touches the default server: every command names the
// socket, and the client environment carries no TMUX.
func newCaptureScratchServer(t *testing.T) string {
	t.Helper()
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	socket := "osc7501x-arm-" + hex.EncodeToString(b[:])
	scratch := func(args ...string) *exec.Cmd {
		cmd := exec.Command("tmux", append([]string{"-L", socket, "-f", "/dev/null"}, args...)...)
		cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
			return strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=")
		})
		return cmd
	}
	if out, err := scratch("new-session", "-d", "-s", "boot", "sleep 3600").CombinedOutput(); err != nil {
		t.Fatalf("starting scratch tmux %s: %v: %s", socket, err, out)
	}
	t.Cleanup(func() {
		if out, err := scratch("kill-server").CombinedOutput(); err != nil {
			t.Logf("killing scratch tmux %s: %v: %s", socket, err, out)
		}
	})
	return socket
}

// captureProvider is a tmux provider on socket arming with companion.
func captureProvider(t *testing.T, socket, companion string) *Provider {
	t.Helper()
	cfg := DefaultConfig()
	cfg.SocketName = socket
	cfg.ProgramStatusBin = companion
	cfg.NudgeIdleTimeout = 250 * time.Millisecond
	return NewProviderWithConfig(cfg)
}

// paneOption reads a pane-scope option of name's agent pane, "" when unset.
func paneOption(t *testing.T, p *Provider, name, opt string) string {
	t.Helper()
	target, err := p.tm.agentPaneTarget(name)
	if err != nil {
		t.Fatalf("agent pane of %s: %v", name, err)
	}
	out, err := p.tm.run("show-options", "-p", "-q", "-v", "-t", target, opt)
	if err != nil {
		t.Fatalf("show-options %s: %v", opt, err)
	}
	return strings.TrimSpace(out)
}

// awaitRecord waits for name's agent pane to carry a record published under
// nonce in state, and returns it.
func awaitRecord(t *testing.T, p *Provider, name, nonce string, state runtime.ProgramState) runtime.ProgramStatus {
	t.Helper()
	var got runtime.ProgramStatus
	eventually(t, "a "+string(state)+" record under "+nonce, func() bool {
		st, err := programstatus.Decode(paneOption(t, p, name, programStatusOption))
		got = st
		return err == nil && st.Nonce == nonce && st.State == state
	})
	return got
}

// armedNonce returns the nonce armed on name's agent pane, checked against
// the pane's id.
func armedNonce(t *testing.T, p *Provider, name string) string {
	t.Helper()
	nonce := paneOption(t, p, name, programTapOption)
	pane, err := p.tm.readAgentPane(p.tm.run, name, "#{pane_id}")
	if err != nil {
		t.Fatal(err)
	}
	if !programstatus.ValidNonce(nonce) || !strings.HasSuffix(nonce, "."+strings.TrimSpace(pane)) {
		t.Fatalf("armed nonce %q is not <rand>.%s", nonce, pane)
	}
	return nonce
}

// tapRunning reports whether a tap holding nonce's rand is in the process
// table.
func tapRunning(t *testing.T, nonce string) bool {
	t.Helper()
	return len(tapPIDs(t, nonce)) > 0
}

// tapPIDs returns the pids of the taps holding nonce's rand.
func tapPIDs(t *testing.T, nonce string) []int {
	t.Helper()
	r, _, _ := strings.Cut(nonce, ".")
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(cmdline), "\x00")
		if len(args) > 1 && filepath.Base(args[0]) == programstatus.CompanionName && args[1] == "tap" {
			if i := slices.Index(args, "-n"); i >= 0 && i+1 < len(args) && args[i+1] == r {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

func emitterCommand(bin string, steps ...string) string {
	argv := []string{bin}
	for _, s := range steps {
		argv = append(argv, "-e", s)
	}
	return shellquote.Join(argv)
}

// A report written at the program's first instant is captured, on create and
// again on respawn: the tap is armed in the same tmux command list, before
// tmux reads the pane's first output. The respawn re-arms under a new nonce
// and the previous tap ends.
func TestTapCapturesFirstInstantReport(t *testing.T) {
	socket := newCaptureScratchServer(t)
	p := captureProvider(t, socket, captureCompanionBin(t))
	emit := captureEmitterBin(t)
	const name = "gc-ps-first"
	cfg := runtime.Config{
		Command:       emitterCommand(emit, "report state=working", "hang"),
		WorkDir:       t.TempDir(),
		ProgramStatus: runtime.ProgramStatusArm{Capture: true},
	}
	if err := p.Start(context.Background(), name, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	first := armedNonce(t, p, name)
	if st := awaitRecord(t, p, name, first, runtime.ProgramWorking); st.TurnStarts != 1 {
		t.Fatalf("first record %+v, want one turn start", st)
	}

	cfg.Command = emitterCommand(emit, "report state=idle", "hang")
	if err := p.Relaunch(context.Background(), name, cfg); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	second := armedNonce(t, p, name)
	if second == first {
		t.Fatalf("respawn kept nonce %q", first)
	}
	awaitRecord(t, p, name, second, runtime.ProgramIdle)
	eventually(t, "the first incarnation's tap to end", func() bool { return !tapRunning(t, first) })
	if !tapRunning(t, second) {
		t.Fatalf("no tap holds the respawned pane's nonce %q", second)
	}
}

// A probe-gated program (the Claude shape) reports under capture when its
// provider declares the relay: the relay answers the probe, the tap captures
// the report.
func TestRelayedProbeGatedReportIsCaptured(t *testing.T) {
	socket := newCaptureScratchServer(t)
	p := captureProvider(t, socket, captureCompanionBin(t))
	const name = "gc-ps-relayed"
	cfg := runtime.Config{
		Command:       emitterCommand(captureEmitterBin(t), "probe-gated 5s", "report state=working", "hang"),
		WorkDir:       t.TempDir(),
		ProgramStatus: runtime.ProgramStatusArm{Capture: true, Relay: true},
	}
	if err := p.Start(context.Background(), name, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	st := awaitRecord(t, p, name, armedNonce(t, p, name), runtime.ProgramWorking)
	if !st.Probed {
		t.Fatalf("record %+v does not show the probe", st)
	}
	if cmd, err := p.tm.GetPaneCommand(name); err != nil || cmd != programstatus.CompanionName {
		t.Fatalf("pane command = %q, %v; want the relay %s", cmd, err, programstatus.CompanionName)
	}
}

// startRawCatRelayed is startRawCat with capture and the relay armed: the
// pane runs the relay, which runs rawCatCommand on its own pty.
func startRawCatRelayed(t *testing.T, p *Provider, name string, f runtime.InputFence) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "typed")
	env := fenceEnv(f)
	env["GC_PROVIDER"] = "opencode" // the single-attempt fallback submit; no Escape
	cfg := runtime.Config{
		Command:       rawCatCommand(file),
		WorkDir:       dir,
		Env:           maps.Clone(env),
		ProgramStatus: runtime.ProgramStatusArm{Capture: true, Relay: true},
	}
	if err := p.Start(context.Background(), name, cfg); err != nil {
		t.Fatalf("Start %s: %v", name, err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	waitCaptureFile(t, file)
	if cmd, err := p.tm.GetPaneCommand(name); err != nil || cmd != programstatus.CompanionName {
		t.Fatalf("pane command = %q, %v; want the relay %s", cmd, err, programstatus.CompanionName)
	}
	return file
}

// RUNTIME-CONTRACT-011 through the relay: literal text arrives byte for byte,
// a trailing ";" and a leading "-" included.
func TestLiteralTextByteExactThroughRelay(t *testing.T) {
	socket := newCaptureScratchServer(t)
	p := captureProvider(t, socket, captureCompanionBin(t))
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "token-relay"}
	texts := []string{fenceTextE4, fenceTextE4 + ";", ";", "x;", "x;;", `x\;`, "-x", "-l", "- item"}
	deliveries := []struct {
		name string
		send func(name, text string) error
	}{
		{"nudge", func(name, text string) error { return p.NudgeNow(name, runtime.TextContent(text)) }},
		{"debounced", func(name, text string) error { return p.tm.SendKeysDebounced(name, text, 0) }},
	}
	for _, d := range deliveries {
		name := "gc-relay-exact-" + d.name
		file := startRawCatRelayed(t, p, name, f)
		var sent []string
		for _, text := range texts {
			if err := d.send(name, text); err != nil {
				t.Errorf("%s(%q) = %v", d.name, text, err)
				continue
			}
			sent = append(sent, text)
			n := len(sent)
			waitTyped(t, file, func(got string) bool { return strings.Count(got, "\r") >= n })
		}
		got := strings.Split(strings.TrimSuffix(typedSoFar(file), "\r"), "\r")
		if len(got) != len(sent) {
			t.Fatalf("%s typed %q, want %d lines %q", d.name, got, len(sent), sent)
		}
		for i, text := range sent {
			if strings.TrimLeft(got[i], "\x15") != text {
				t.Errorf("%s(%q) typed %q through the relay", d.name, text, got[i])
			}
		}
	}
}

// RUNTIME-CONTRACT-009 through the relay: fenced input reaches the named
// incarnation byte for byte, and another incarnation's fence sends nothing.
func TestFencedInputThroughRelay(t *testing.T) {
	socket := newCaptureScratchServer(t)
	p := captureProvider(t, socket, captureCompanionBin(t))
	const name = "gc-relay-fenced"
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "token-own"}
	other := runtime.InputFence{Generation: 1, Epoch: 2, Token: "token-own"}
	file := startRawCatRelayed(t, p, name, f)

	err := p.NudgeFenced(context.Background(), name, runtime.TextContent("for-other"), immediate(other))
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Partial {
		t.Fatalf("NudgeFenced(other fence) = %v, want a non-partial miss", err)
	}
	if err := p.SendKeysFenced(context.Background(), name, other, "Enter"); !errors.Is(err, runtime.ErrFenceMismatch) {
		t.Fatalf("SendKeysFenced(other fence) = %v, want a miss", err)
	}
	if err := p.NudgeFenced(context.Background(), name, runtime.TextContent(fenceTextE4), immediate(f)); err != nil {
		t.Fatalf("NudgeFenced(own fence) = %v", err)
	}
	// The own delivery is the signal that anything sent before it landed.
	got := waitTyped(t, file, containsFn("\r"))
	if strings.Contains(got, "for-other") || strings.Count(got, "\r") != 1 {
		t.Fatalf("typed %q: another incarnation's input reached the pane", got)
	}
	if line := strings.TrimLeft(strings.TrimSuffix(got, "\r"), "\x15"); line != fenceTextE4 {
		t.Fatalf("fenced text through the relay = %q, want %q", line, fenceTextE4)
	}
}

// A respawn that does not arm while the switch is on removes the previous
// incarnation's capture: the nonce and record are gone, the old tap ends,
// and the new program's report is never published.
func TestUnarmedRespawnRemovesPreviousCapture(t *testing.T) {
	socket := newCaptureScratchServer(t)
	p := captureProvider(t, socket, captureCompanionBin(t))
	p.tm.cfg.ProgramStatus = runtime.ProgramStatusOn
	emit := captureEmitterBin(t)
	const name = "gc-ps-unarmed-respawn"
	cfg := runtime.Config{
		Command:       emitterCommand(emit, "report state=working", "hang"),
		WorkDir:       t.TempDir(),
		ProgramStatus: runtime.ProgramStatusArm{Capture: true},
	}
	if err := p.Start(context.Background(), name, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	first := armedNonce(t, p, name)
	awaitRecord(t, p, name, first, runtime.ProgramWorking)

	cfg.Command = emitterCommand(emit, "report state=idle", "hang")
	cfg.ProgramStatus = runtime.ProgramStatusArm{}
	if err := p.Relaunch(context.Background(), name, cfg); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	for _, opt := range []string{programTapOption, programStatusOption, programTextOption} {
		if got := paneOption(t, p, name, opt); got != "" {
			t.Fatalf("after an unarmed respawn %s = %q, want unset", opt, got)
		}
	}
	// With no reader on the pane and the previous tap gone, nothing can
	// publish the new program's report.
	eventually(t, "the previous incarnation's tap to end", func() bool { return !tapRunning(t, first) })
	target, err := p.tm.agentPaneTarget(name)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := p.tm.run("display-message", "-p", "-t", target, "#{pane_pipe}"); err != nil || strings.TrimSpace(out) != "0" {
		t.Fatalf("pane_pipe = %q, %v after an unarmed respawn, want 0", out, err)
	}
	if got := paneOption(t, p, name, programStatusOption); got != "" {
		t.Fatalf("the unarmed incarnation's report was published: %q", got)
	}
}

// relayedStubbornCommand is a program that ignores SIGTERM, SIGHUP, SIGINT
// and SIGQUIT and records its pid in file. Under the relay it leads a
// session and process group of its own.
func relayedStubbornCommand(file string) string {
	return "trap '' TERM HUP INT QUIT; echo $$ > " + shellquote.Join([]string{file}) + "; while :; do sleep 0.2; done"
}

// startStubbornRelayed starts a relay-wrapped stubborn program in session
// name, tagged with sessionID, and returns its pid.
func startStubbornRelayed(t *testing.T, p *Provider, name, sessionID string) int {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "pid")
	cfg := runtime.Config{
		Command:       relayedStubbornCommand(file),
		WorkDir:       dir,
		Env:           map[string]string{"GC_SESSION_ID": sessionID},
		ProgramStatus: runtime.ProgramStatusArm{Capture: true, Relay: true},
	}
	if err := p.Start(context.Background(), name, cfg); err != nil {
		t.Fatalf("Start %s: %v", name, err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	var pid int
	eventually(t, "the relayed program's pid", func() bool {
		data, err := os.ReadFile(file)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid > 1
	})
	if cmd, err := p.tm.GetPaneCommand(name); err != nil || cmd != programstatus.CompanionName {
		t.Fatalf("pane command = %q, %v; want the relay %s", cmd, err, programstatus.CompanionName)
	}
	return pid
}

// processGone reports whether pid has exited (gone or a zombie).
func processGone(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return true
	}
	stat := string(data)
	if i := strings.LastIndexByte(stat, ')'); i >= 0 && i+2 < len(stat) {
		return stat[i+2] == 'Z'
	}
	return false
}

// uniqueSessionID is a GC_SESSION_ID no other process on the host carries.
func uniqueSessionID(t *testing.T, prefix string) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

// The reaper's path: the process-table scan looks through the relay and
// reports its program as the runtime's root, so TerminateRuntime signals the
// program's own process group and a program that ignores the relay's
// SIGTERM and hangup still dies.
func TestReaperKillsRelayedProgramThatIgnoresTermAndHup(t *testing.T) {
	socket := newCaptureScratchServer(t)
	p := captureProvider(t, socket, captureCompanionBin(t))
	sessionID := uniqueSessionID(t, "osc7501x-reap")
	program := startStubbornRelayed(t, p, "gc-relay-reap", sessionID)

	// The scan reads the live process table through a link, which the
	// guard against reaping the host's agents admits; the unique session id
	// confines it to this test's runtime.
	procLink := filepath.Join(t.TempDir(), "proc")
	if err := os.Symlink("/proc", procLink); err != nil {
		t.Fatal(err)
	}
	proctable.SetScanRootForTesting(t, procLink)
	roots, err := proctable.ScanBySessionID(sessionID)
	if len(roots) != 1 {
		t.Fatalf("scan roots = %+v (err %v), want exactly the relayed program", roots, err)
	}
	if roots[0].PID != program {
		t.Fatalf("scan root pid %d (%s), want the relayed program %d, not the relay", roots[0].PID, roots[0].Name, program)
	}
	if err := p.TerminateRuntime(roots[0]); err != nil {
		t.Fatalf("TerminateRuntime: %v", err)
	}
	if !processGone(program) {
		t.Fatalf("relayed program %d survived TerminateRuntime", program)
	}
}

// The stop path: a relayed program that ignores SIGTERM and the hangup is
// killed by Stop.
func TestStopKillsRelayedProgramThatIgnoresTermAndHup(t *testing.T) {
	socket := newCaptureScratchServer(t)
	p := captureProvider(t, socket, captureCompanionBin(t))
	const name = "gc-relay-stop"
	program := startStubbornRelayed(t, p, name, uniqueSessionID(t, "osc7501x-stop"))
	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	eventually(t, "the relayed program to die", func() bool { return processGone(program) })
}
