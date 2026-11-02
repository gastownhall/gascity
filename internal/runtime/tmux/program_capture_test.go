package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
	"github.com/gastownhall/gascity/internal/shellquote"
)

const (
	testCompanion = "/opt/gc/bin/gc-osc7501"
	testTmuxBin   = "/usr/bin/tmux"
	// testShell is the server default-shell the test gate reports: not
	// /bin/sh, so a relay that ignores it shows.
	testShell = "/usr/local/bin/bash"
	// testSecret is not on the argv allow list, so it forces the command
	// file: the primary path, because every managed session carries one.
	testSecretKey = "ANTHROPIC_AUTH_TOKEN"
	testSecret    = "sk-test-not-a-real-credential"
)

// stagedRecorder is a fakeExecutor that also records each staged command
// file's contents at the moment tmux would source it.
type stagedRecorder struct {
	*fakeExecutor
	staged []string
}

func (r *stagedRecorder) execute(args []string) (string, error) {
	if i := slices.Index(args, "source-file"); i >= 0 && i+1 < len(args) {
		body, err := os.ReadFile(args[i+1])
		if err != nil {
			return "", err
		}
		r.staged = append(r.staged, strings.TrimSuffix(string(body), "\n"))
	}
	return r.fakeExecutor.execute(args)
}

func (r *stagedRecorder) executeCtx(_ context.Context, args []string) (string, error) {
	return r.execute(args)
}

// captureTestTmux returns a Tmux whose capability gate passes without a
// companion or a server (the companion runs gc's relay protocol, the server's
// default-shell is testShell), its executor, and the capture warnings it
// logs. probes counts gate evaluations.
func captureTestTmux(t *testing.T) (tm *Tmux, rec *stagedRecorder, warnings *strings.Builder, probes *int) {
	t.Helper()
	rec = &stagedRecorder{fakeExecutor: &fakeExecutor{}}
	tm = NewTmux()
	tm.exec = rec
	probes = new(int)
	tm.capture.probe = func() (captureCapability, error) {
		*probes++
		return captureCapability{companion: testCompanion, relay: programstatus.RelayVersion, tmux: testTmuxBin, shell: testShell}, nil
	}
	warnings = captureWarnings(t)
	return tm, rec, warnings, probes
}

// captureWarnings redirects capture warnings for the test.
func captureWarnings(t *testing.T) *strings.Builder {
	t.Helper()
	var b strings.Builder
	prev := captureWarningOut
	captureWarningOut = &b
	t.Cleanup(func() { captureWarningOut = prev })
	return &b
}

var tapRandRe = regexp.MustCompile(`'@gc_ps_tap' '([A-Za-z0-9_-]+)\.#\{pane_id\}'`)

// stagedRand returns the nonce rand an armed command file carries.
func stagedRand(t *testing.T, line string) string {
	t.Helper()
	m := tapRandRe.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no @gc_ps_tap nonce in %q", line)
	}
	return m[1]
}

func wantTapCommand(rand, app string) string {
	cmd := "exec " + testCompanion + " tap -t #{pane_id} -n " + rand + " -x " + testTmuxBin + " -S #{socket_path}"
	if app != "" {
		cmd += " -a " + app
	}
	return cmd
}

func callsContaining(calls [][]string, needle string) [][]string {
	var out [][]string
	for _, c := range calls {
		for _, a := range c {
			if strings.Contains(a, needle) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func TestTmuxCommandLineUnchangedForSingleCommand(t *testing.T) {
	args := []string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "gc-x", "-e", "K=it's;$x", "claude ;"}
	want := `'new-session' '-d' '-P' '-F' '#{pane_id}' '-s' 'gc-x' '-e' 'K=it'\''s;$x' 'claude ;'`
	if got := tmuxCommandLine(args); got != want {
		t.Fatalf("tmuxCommandLine = %q, want %q", got, want)
	}
	if got := tmuxCommandLines([][]string{args}); got != want {
		t.Fatalf("tmuxCommandLines(one) = %q, want today's line %q", got, want)
	}
	if got := tmuxArgvList([][]string{args}); !slices.Equal(got, args) {
		t.Fatalf("tmuxArgvList(one) = %q, want the command as it is", got)
	}
}

func TestTmuxCommandLineKeepsSeparatorsBare(t *testing.T) {
	cmds := [][]string{{"new-session", "-s", "a;b"}, {"set-option", "-p", "@x", ";"}, {"pipe-pane", "-O", "cat"}}
	want := `'new-session' '-s' 'a;b' ; 'set-option' '-p' '@x' ';' ; 'pipe-pane' '-O' 'cat'`
	if got := tmuxCommandLines(cmds); got != want {
		t.Fatalf("tmuxCommandLines = %q, want %q", got, want)
	}
	wantArgv := []string{"new-session", "-s", "a;b", ";", "set-option", "-p", "@x", ";", ";", "pipe-pane", "-O", "cat"}
	if got := tmuxArgvList(cmds); !slices.Equal(got, wantArgv) {
		t.Fatalf("tmuxArgvList = %q, want %q", got, wantArgv)
	}
}

func TestNewSessionArmsOnCommandFilePath(t *testing.T) {
	tm, rec, warnings, _ := captureTestTmux(t)
	rec.outs = []string{"%7"}
	env := map[string]string{testSecretKey: testSecret, "GC_RIG": "rig-a"}
	arm := runtime.ProgramStatusArm{Capture: true, App: "codex"}
	if err := tm.newSessionWithCommandAndEnv("gc-arm", "/work", "codex", env, arm); err != nil {
		t.Fatalf("newSessionWithCommandAndEnv: %v", err)
	}
	create := rec.calls[0]
	if !slices.Equal(create[len(create)-4:len(create)-1], []string{"start-server", ";", "source-file"}) {
		t.Fatalf("armed create did not take the command file: %q", create)
	}
	for _, c := range rec.calls {
		for _, a := range c {
			if strings.Contains(a, testSecret) {
				t.Fatalf("secret reached argv: %q", c)
			}
		}
	}
	if len(rec.staged) != 1 {
		t.Fatalf("staged %d command files, want 1", len(rec.staged))
	}
	line := rec.staged[0]
	r := stagedRand(t, line)
	if !programstatus.ValidNonce(r + ".%7") {
		t.Fatalf("nonce rand %q is not codec-valid", r)
	}
	want := tmuxCommandLines([][]string{
		{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "gc-arm", "-c", "/work", "-e", "ANTHROPIC_AUTH_TOKEN=" + testSecret, "-e", "GC_RIG=rig-a", "codex"},
		{"set-option", "-p", "-F", "-t", "=gc-arm:", programTapOption, r + ".#{pane_id}"},
		{"pipe-pane", "-O", "-t", "=gc-arm:", wantTapCommand(r, "codex")},
	})
	if line != want {
		t.Fatalf("staged line\n got %q\nwant %q", line, want)
	}
	if strings.Count(line, " ; ") != 2 {
		t.Fatalf("separators are not bare: %q", line)
	}
	if got := callsContaining(rec.calls, "pipe-pane"); len(got) != 0 {
		t.Fatalf("pipe-pane ran outside the create list: %q", got)
	}
	if warnings.Len() != 0 {
		t.Fatalf("warnings on a clean arm: %q", warnings.String())
	}
}

func TestNewSessionArmsOnArgvPath(t *testing.T) {
	tm, rec, _, _ := captureTestTmux(t)
	rec.outs = []string{"%3"}
	arm := runtime.ProgramStatusArm{Capture: true}
	if err := tm.newSessionWithCommandAndEnv("gc-argv", "", "codex", map[string]string{"GC_RIG": "r"}, arm); err != nil {
		t.Fatalf("newSessionWithCommandAndEnv: %v", err)
	}
	create := rec.calls[0]
	i := slices.Index(create, "@gc_ps_tap")
	if i < 0 {
		t.Fatalf("argv create did not arm: %q", create)
	}
	r := strings.TrimSuffix(create[i+1], ".#{pane_id}")
	want := append([]string{"-u"}, tmuxArgvList([][]string{
		{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "gc-argv", "-e", "GC_RIG=r", "codex"},
		{"set-option", "-p", "-F", "-t", "=gc-argv:", programTapOption, r + ".#{pane_id}"},
		{"pipe-pane", "-O", "-t", "=gc-argv:", wantTapCommand(r, "")},
	})...)
	if !slices.Equal(create, want) {
		t.Fatalf("argv create\n got %q\nwant %q", create, want)
	}
}

// A later command of the create list failing leaves the session created (its
// pane id printed): it is marked, fenced and has its credentials withheld as
// any session, the arm failure is logged, and create succeeds.
func TestNewSessionArmFailureStillMarksSessionAndWithholdsCredentials(t *testing.T) {
	tm, rec, warnings, _ := captureTestTmux(t)
	rec.errs = []error{&tmuxOutputError{Stdout: "%9", Err: errors.New("tmux new-session: can't find pane")}}
	env := map[string]string{
		testSecretKey: testSecret,
		"BEADS_DIR":   "", // a durable withholding
	}
	for k, v := range fenceEnv(runtime.InputFence{Generation: 2, Epoch: 1, Token: "tok"}) {
		env[k] = v
	}
	err := tm.newSessionWithCommandAndEnv("gc-armfail", "", "codex", env, runtime.ProgramStatusArm{Capture: true})
	if err != nil {
		t.Fatalf("create with a failed arm = %v, want nil (the session exists)", err)
	}
	var marked, fenced, withheld bool
	for _, c := range rec.calls[1:] {
		joined := strings.Join(c, " ")
		switch {
		case strings.Contains(joined, "set-option -p -F -t %9 "+agentPaneOption):
			marked = true
		case strings.Contains(joined, "set-option -p -F -t %9 "+inputFenceOption):
			fenced = true
		case strings.Contains(joined, "set-environment -t =gc-armfail -r BEADS_DIR"):
			withheld = true
		}
	}
	if !marked || !fenced || !withheld {
		t.Fatalf("after a partial create: marked=%v fenced=%v withheld=%v; calls %q", marked, fenced, withheld, rec.calls)
	}
	if !strings.Contains(warnings.String(), "gc-armfail") || !strings.Contains(warnings.String(), "can't find pane") {
		t.Fatalf("arm failure not logged: %q", warnings.String())
	}
}

// A list that failed before printing a pane id created nothing: the error is
// returned unchanged, so ErrSessionExists still classifies.
func TestNewSessionArmedListFailureWithoutPaneIsReturned(t *testing.T) {
	tm, rec, _, _ := captureTestTmux(t)
	rec.errs = []error{ErrSessionExists}
	err := tm.newSessionWithCommandAndEnv("gc-dup", "", "codex", map[string]string{testSecretKey: testSecret}, runtime.ProgramStatusArm{Capture: true})
	if !errors.Is(err, ErrSessionExists) {
		t.Fatalf("err = %v, want ErrSessionExists", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("calls after a failed create: %q", rec.calls)
	}
}

func TestRespawnRearmsTapAndClearsRecordInOneList(t *testing.T) {
	tm, rec, _, _ := captureTestTmux(t)
	arm := runtime.ProgramStatusArm{Capture: true, App: "claude-code", Relay: true}
	var rands []string
	for range 2 {
		rec.calls = nil
		if err := tm.respawnPaneWithWorkDir("%5", "/w", "claude --x", arm); err != nil {
			t.Fatalf("respawnPaneWithWorkDir: %v", err)
		}
		if len(rec.calls) != 1 {
			t.Fatalf("respawn ran %d tmux invocations, want one list: %q", len(rec.calls), rec.calls)
		}
		got := rec.calls[0]
		i := slices.Index(got, programTapOption)
		if i < 0 {
			t.Fatalf("respawn did not re-arm: %q", got)
		}
		r := strings.TrimSuffix(got[i+1], ".#{pane_id}")
		if !programstatus.ValidNonce(r + ".%5") {
			t.Fatalf("nonce rand %q is not codec-valid", r)
		}
		relayed := shellquote.Join([]string{testCompanion, "relay", "--", testShell, "-c", "claude --x"})
		want := append([]string{"-u"}, tmuxArgvList([][]string{
			{"respawn-pane", "-k", "-t", "%5", "-c", "/w", relayed},
			{"display-message", "-p", "-t", "%5", "#{pane_id}"},
			{"set-option", "-p", "-F", "-t", "%5", programTapOption, r + ".#{pane_id}"},
			{"set-option", "-pu", "-t", "%5", programStatusOption},
			{"set-option", "-pu", "-t", "%5", programTextOption},
			{"pipe-pane", "-O", "-t", "%5", wantTapCommand(r, "claude-code")},
		})...)
		if !slices.Equal(got, want) {
			t.Fatalf("respawn list\n got %q\nwant %q", got, want)
		}
		if slices.Contains(got, "-o") {
			t.Fatalf("pipe-pane -o toggles; never use it: %q", got)
		}
		rands = append(rands, r)
	}
	if rands[0] == rands[1] {
		t.Fatalf("two respawns armed the same nonce %q", rands[0])
	}
}

func TestRespawnArmFailureKeepsTheRespawn(t *testing.T) {
	tm, rec, warnings, _ := captureTestTmux(t)
	rec.errs = []error{&tmuxOutputError{Stdout: "%5", Err: errors.New("tmux respawn-pane: target pane has exited")}}
	if err := tm.respawnPaneWithWorkDir("%5", "", "codex", runtime.ProgramStatusArm{Capture: true}); err != nil {
		t.Fatalf("respawn whose arm failed = %v, want nil (the marker proves it respawned)", err)
	}
	if !strings.Contains(warnings.String(), "target pane has exited") {
		t.Fatalf("arm failure not logged: %q", warnings.String())
	}
}

func TestRespawnFailureIsReturned(t *testing.T) {
	tm, rec, _, _ := captureTestTmux(t)
	rec.errs = []error{ErrSessionNotFound}
	err := tm.respawnPaneWithWorkDir("%5", "", "codex", runtime.ProgramStatusArm{Capture: true})
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err = %v, want the respawn's own failure", err)
	}
}

// An undeclared provider is never armed: its create reaches tmux without any
// capture command, and its respawn runs only the removal of a capture an
// armed incarnation left bound to the pane (while the switch is off,
// disarmIfArmedCommands).
func TestUndeclaredProviderIsNeverArmed(t *testing.T) {
	for _, arm := range []runtime.ProgramStatusArm{{}, {App: "codex", Relay: true}} {
		tm, rec, _, probes := captureTestTmux(t)
		rec.outs = []string{"%1"}
		env := map[string]string{testSecretKey: testSecret}
		if err := tm.newSessionWithCommandAndEnv("gc-undeclared", "/w", "codex", env, arm); err != nil {
			t.Fatal(err)
		}
		created := len(rec.calls)
		if err := tm.respawnPaneWithWorkDir("%1", "/w", "codex", arm); err != nil {
			t.Fatal(err)
		}
		if *probes != 0 {
			t.Fatalf("arm %+v evaluated the capability gate", arm)
		}
		for _, needle := range []string{"pipe-pane", "@gc_ps", programstatus.CompanionName} {
			if got := callsContaining(rec.calls[:created], needle); len(got) != 0 {
				t.Fatalf("arm %+v: %q reached tmux on create: %q", arm, needle, got)
			}
		}
		for _, line := range rec.staged {
			if strings.Contains(line, " ; ") || strings.Contains(line, "@gc_ps") {
				t.Fatalf("arm %+v: staged a list: %q", arm, line)
			}
		}
		want := append([]string{"-u"}, tmuxArgvList([][]string{
			{"respawn-pane", "-k", "-t", "%1", "-c", "/w", "codex"},
			{"display-message", "-p", "-t", "%1", "#{pane_id}"},
			{"if-shell", "-F", "-t", "%1", "#{m:*.#{pane_id},#{@gc_ps_tap}}", offDisarmLine("%1")},
		})...)
		if respawn := rec.calls[created:]; len(respawn) != 1 || !slices.Equal(respawn[0], want) {
			t.Fatalf("arm %+v: respawn calls\n got %q\nwant [%q]", arm, respawn, want)
		}
	}
}

// offDisarmLine is the command line the switch-off respawn's guard runs on
// target: disarmCommands as one tmux command line.
func offDisarmLine(target string) string {
	return "'set-option' '-pu' '-t' '" + target + "' '@gc_ps_tap' ; " +
		"'set-option' '-pu' '-t' '" + target + "' '@gc_ps' ; " +
		"'set-option' '-pu' '-t' '" + target + "' '@gc_ps_text' ; " +
		"'pipe-pane' '-t' '" + target + "'"
}

// With the switch off every runtime.Config carries the zero arm (the config
// derivation's tests pin that), and the tmux command lines are today's: a
// create's byte for byte, and a respawn's respawn-pane, which the respawn
// follows only with the removal of a capture bound to the pane
// (TestRespawnUnderSwitchOffRemovesOnlyABoundCapture).
func TestSwitchOffIsByteIdentical(t *testing.T) {
	tm, rec, _, probes := captureTestTmux(t)
	ops := newTmuxStartOps(tm, "", 0, runtime.Config{}, true)
	if err := ops.createSession("gc-off", "/work", "codex", map[string]string{testSecretKey: testSecret}); err != nil {
		t.Fatal(err)
	}
	if err := ops.createSession("gc-off2", "/work", "codex", map[string]string{"GC_RIG": "r"}); err != nil {
		t.Fatal(err)
	}
	if err := ops.respawnAgent("%4", "/work", "codex", nil); err != nil {
		t.Fatal(err)
	}
	wantLine := `'new-session' '-d' '-P' '-F' '#{pane_id}' '-s' 'gc-off' '-c' '/work' '-e' 'ANTHROPIC_AUTH_TOKEN=` + testSecret + `' 'codex'`
	if len(rec.staged) != 1 || rec.staged[0] != wantLine {
		t.Fatalf("staged %q, want exactly %q", rec.staged, wantLine)
	}
	wantArgv := []string{"-u", "new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "gc-off2", "-c", "/work", "-e", "GC_RIG=r", "codex"}
	wantRespawn := []string{"-u", "respawn-pane", "-k", "-t", "%4", "-c", "/work", "codex", ";"}
	var sawArgv, sawRespawn bool
	for _, c := range rec.calls {
		sawArgv = sawArgv || slices.Equal(c, wantArgv)
		sawRespawn = sawRespawn || (len(c) > len(wantRespawn) && slices.Equal(c[:len(wantRespawn)], wantRespawn))
	}
	if !sawArgv || !sawRespawn {
		t.Fatalf("switch-off create/respawn argv changed (argv=%v respawn=%v): %q", sawArgv, sawRespawn, rec.calls)
	}
	if *probes != 0 {
		t.Fatal("switch off evaluated the capability gate")
	}
}

// The relay sits inside the env -u withholding (the relay and the agent both
// start without withheld credentials) and inside the GC_AGENT_SLICE scope
// (the slice wraps the pane's whole command). The slice runs the command
// with sh -c, so the relay runs it with /bin/sh there, not the server's
// default-shell.
func TestRelayWrapInsideEnvUnsetInsideSlice(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	tm, rec, _, _ := captureTestTmux(t)
	tm.agentSlice.probe = func(string) error { return nil }
	rec.outs = []string{"%2"}
	arm := runtime.ProgramStatusArm{Capture: true, Relay: true}
	if err := tm.newSessionWithCommandAndEnv("gc-order", "", "claude", map[string]string{"LC_ALL": ""}, arm); err != nil {
		t.Fatal(err)
	}
	relayed := shellquote.Join([]string{testCompanion, "relay", "--", "/bin/sh", "-c", "claude"})
	want := shellquote.Join([]string{
		"systemd-run", "--user", "--scope", "--slice=gascity-agents.slice", "--collect", "--quiet", "--",
		"sh", "-c", "env -u LC_ALL " + relayed,
	})
	create := rec.calls[0]
	sep := slices.Index(create, ";")
	if sep < 1 || create[sep-1] != want {
		t.Fatalf("pane command\n got %q\nwant %q", create, want)
	}
}

func TestRelayWrapDoesNotMoveFingerprint(t *testing.T) {
	tm, rec, _, _ := captureTestTmux(t)
	rec.outs = []string{"%6"}
	base := runtime.Config{Command: "claude", WorkDir: "/w", Env: map[string]string{"GC_RIG": "r"}}
	cfg := base
	cfg.ProgramStatus = runtime.ProgramStatusArm{Capture: true, App: "claude-code", Relay: true}
	before := []string{runtime.CoreFingerprint(cfg), runtime.ConfigFingerprint(cfg), runtime.LiveFingerprint(cfg)}
	ops := newTmuxStartOps(tm, "", 0, cfg, true)
	if err := ops.createSession("gc-fp", cfg.WorkDir, cfg.Command, cfg.Env); err != nil {
		t.Fatal(err)
	}
	if got := callsContaining(rec.calls, " relay -- "); len(got) != 1 {
		t.Fatalf("the leaf did not wrap the pane command in the relay: %q", rec.calls)
	}
	if cfg.Command != "claude" {
		t.Fatalf("relay wrap reached runtime.Config.Command: %q", cfg.Command)
	}
	after := []string{runtime.CoreFingerprint(cfg), runtime.ConfigFingerprint(cfg), runtime.LiveFingerprint(cfg)}
	unarmed := []string{runtime.CoreFingerprint(base), runtime.ConfigFingerprint(base), runtime.LiveFingerprint(base)}
	if !slices.Equal(before, after) || !slices.Equal(after, unarmed) {
		t.Fatalf("fingerprints moved: armed before %q, after %q, unarmed %q", before, after, unarmed)
	}
}

func TestWaitForCommandTreatsRelayAsWrapper(t *testing.T) {
	if !isWrapperCommand(programstatus.CompanionName) {
		t.Fatalf("%s is not a wrapper command", programstatus.CompanionName)
	}
	exec := &wrappedWaitExecutor{paneCommand: programstatus.CompanionName, deadPID: "999999999", livePID: "999999999"}
	tm := NewTmux()
	tm.exec = exec
	if err := tm.WaitForCommand(context.Background(), "gc-test-relay-wait", supportedShells, 250*time.Millisecond); err == nil {
		t.Fatal("WaitForCommand returned on sight of the relay; want it to wait for the agent descendant")
	}
}

// A relay-declared provider whose companion does not run gc's relay protocol
// (relay=0: a build without the relay; a later version gc does not speak) is
// never launched under it: create and respawn start the program as they
// would unarmed, its capture still armed, and the cause is logged once.
func TestRelayDeclaredWithoutCompanionRelayStartsUnwrapped(t *testing.T) {
	for _, relay := range []int{0, programstatus.RelayVersion + 1} {
		t.Run(fmt.Sprintf("relay=%d", relay), func(t *testing.T) {
			tm, rec, warnings, _ := captureTestTmux(t)
			tm.capture.probe = func() (captureCapability, error) {
				return captureCapability{companion: testCompanion, relay: relay, tmux: testTmuxBin, shell: testShell}, nil
			}
			rec.outs = []string{"%2"}
			arm := runtime.ProgramStatusArm{Capture: true, App: "claude-code", Relay: true}
			if err := tm.newSessionWithCommandAndEnv("gc-norelay", "/w", "claude --x", map[string]string{"GC_RIG": "r"}, arm); err != nil {
				t.Fatalf("newSessionWithCommandAndEnv: %v", err)
			}
			create := rec.calls[0]
			i := slices.Index(create, programTapOption)
			if i < 0 {
				t.Fatalf("create did not arm capture: %q", create)
			}
			r := strings.TrimSuffix(create[i+1], ".#{pane_id}")
			wantCreate := append([]string{"-u"}, tmuxArgvList([][]string{
				{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "gc-norelay", "-c", "/w", "-e", "GC_RIG=r", "claude --x"},
				{"set-option", "-p", "-F", "-t", "=gc-norelay:", programTapOption, r + ".#{pane_id}"},
				{"pipe-pane", "-O", "-t", "=gc-norelay:", wantTapCommand(r, "claude-code")},
			})...)
			if !slices.Equal(create, wantCreate) {
				t.Fatalf("create\n got %q\nwant %q", create, wantCreate)
			}

			rec.calls = nil
			if err := tm.respawnPaneWithWorkDir("%2", "/w", "claude --x", arm); err != nil {
				t.Fatalf("respawnPaneWithWorkDir: %v", err)
			}
			if len(rec.calls) != 1 {
				t.Fatalf("respawn ran %d tmux invocations, want one list: %q", len(rec.calls), rec.calls)
			}
			respawn := rec.calls[0]
			i = slices.Index(respawn, programTapOption)
			if i < 0 {
				t.Fatalf("respawn did not re-arm capture: %q", respawn)
			}
			r = strings.TrimSuffix(respawn[i+1], ".#{pane_id}")
			wantRespawn := append([]string{"-u"}, tmuxArgvList([][]string{
				{"respawn-pane", "-k", "-t", "%2", "-c", "/w", "claude --x"},
				{"display-message", "-p", "-t", "%2", "#{pane_id}"},
				{"set-option", "-p", "-F", "-t", "%2", programTapOption, r + ".#{pane_id}"},
				{"set-option", "-pu", "-t", "%2", programStatusOption},
				{"set-option", "-pu", "-t", "%2", programTextOption},
				{"pipe-pane", "-O", "-t", "%2", wantTapCommand(r, "claude-code")},
			})...)
			if !slices.Equal(respawn, wantRespawn) {
				t.Fatalf("respawn\n got %q\nwant %q", respawn, wantRespawn)
			}
			if n := strings.Count(warnings.String(), "warning:"); n != 1 || !strings.Contains(warnings.String(), fmt.Sprintf("reports relay=%d", relay)) {
				t.Fatalf("relay warnings = %d, want 1 naming relay=%d: %q", n, relay, warnings.String())
			}
		})
	}
}

// gateTmux returns a Tmux whose capability gate runs for real against a fake
// companion answering `version` with line, and an executor that answers the
// gate's tmux reads (version and socket path, then default-shell) and the
// create's pane id. The tap publishes through a fake tmux on PATH.
func gateTmux(t *testing.T, line, defaultShell string) (tm *Tmux, rec *stagedRecorder, companion, runs string) {
	t.Helper()
	companion, runs = writeFakeCompanion(t, line)
	pathDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pathDir, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	cfg := DefaultConfig()
	cfg.ProgramStatusBin = companion
	tm = NewTmuxWithConfig(cfg)
	rec = &stagedRecorder{fakeExecutor: &fakeExecutor{outs: []string{"3.4|/tmp/s", defaultShell, "%1"}}}
	tm.exec = rec
	return tm, rec, companion, runs
}

// The gate keeps what the companion's version handshake reported: a
// companion running gc's relay protocol launches the relay-declared program
// under it, a tap-only one (relay=0) arms capture without it, and either is
// asked once.
func TestCapabilityGateCarriesCompanionRelayVersion(t *testing.T) {
	for _, tc := range []struct {
		relay   int
		relayed int    // of create and respawn, the launches under the relay
		warning string // the one warning logged; "" for none
	}{
		{relay: programstatus.RelayVersion, relayed: 2},
		{relay: 0, relayed: 0, warning: "reports relay=0"},
	} {
		t.Run(fmt.Sprintf("relay=%d", tc.relay), func(t *testing.T) {
			warnings := captureWarnings(t)
			tm, rec, bin, runs := gateTmux(t, fmt.Sprintf("gc-osc7501 dev codec=%d relay=%d", programstatus.CodecVersion, tc.relay), testShell)
			arm := runtime.ProgramStatusArm{Capture: true, Relay: true}
			if err := tm.newSessionWithCommandAndEnv("gc-relayver", "", "claude", nil, arm); err != nil {
				t.Fatal(err)
			}
			if err := tm.respawnPaneWithWorkDir("%1", "", "claude", arm); err != nil {
				t.Fatal(err)
			}
			if got := callsContaining(rec.calls, "pipe-pane"); len(got) != 2 {
				t.Fatalf("armed %d panes, want create and respawn: %q", len(got), rec.calls)
			}
			relayed := shellquote.Join([]string{bin, "relay", "--", testShell, "-c", "claude"})
			if got := len(callsContaining(rec.calls, relayed)); got != tc.relayed {
				t.Fatalf("%d of create and respawn ran under the relay, want %d: %q", got, tc.relayed, rec.calls)
			}
			if n := countLines(t, runs); n != 1 {
				t.Fatalf("companion probed %d times, want once per path", n)
			}
			n := strings.Count(warnings.String(), "warning:")
			if (tc.warning == "" && n != 0) || (tc.warning != "" && (n != 1 || !strings.Contains(warnings.String(), tc.warning))) {
				t.Fatalf("warnings %q, want %q", warnings.String(), tc.warning)
			}
		})
	}
}

// The relay runs the pane's command with the shell tmux would have run it
// with: tmux runs a single-string pane command as `<default-shell> -c
// <command>`, so the gate reads the server's default-shell once, with its
// tmux check, and create and respawn both wrap with it. A command written
// for a bash or zsh default-shell then means the same under the relay. With
// no absolute path read, the relay falls back to /bin/sh, as tmux does.
func TestRelayShellIsServerDefaultShell(t *testing.T) {
	for _, tc := range []struct{ reported, want string }{
		{reported: testShell, want: testShell},
		{reported: "", want: "/bin/sh"},
	} {
		t.Run("default-shell="+tc.reported, func(t *testing.T) {
			captureWarnings(t)
			tm, rec, bin, _ := gateTmux(t, fmt.Sprintf("gc-osc7501 dev codec=%d relay=%d", programstatus.CodecVersion, programstatus.RelayVersion), tc.reported)
			const command = "[[ -n $GC_RIG ]] && claude"
			arm := runtime.ProgramStatusArm{Capture: true, Relay: true}
			if err := tm.newSessionWithCommandAndEnv("gc-shell", "", command, nil, arm); err != nil {
				t.Fatal(err)
			}
			if err := tm.respawnPaneWithWorkDir("%1", "", command, arm); err != nil {
				t.Fatal(err)
			}
			relayed := shellquote.Join([]string{bin, "relay", "--", tc.want, "-c", command})
			if got := callsContaining(rec.calls, relayed); len(got) != 2 {
				t.Fatalf("create and respawn did not both run %q: %q", relayed, rec.calls)
			}
			read := []string{"-u", "start-server", ";", "show-options", "-gv", "default-shell"}
			n := 0
			for _, c := range rec.calls {
				if slices.Equal(c, read) {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("default-shell read %d times, want once with the tmux check: %q", n, rec.calls)
			}
		})
	}
}

// writeFakeCompanion writes an executable that answers `version` with line
// and counts its runs in a file beside it.
func writeFakeCompanion(t *testing.T, line string) (bin, runs string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, programstatus.CompanionName)
	runs = filepath.Join(dir, "runs")
	script := "#!/bin/sh\necho run >> " + shellquote.Quote(runs) + "\necho " + shellquote.Quote(line) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, runs
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func TestCapabilityGateDisablesArmingWhenCompanionMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	warnings := captureWarnings(t)
	rec := &stagedRecorder{fakeExecutor: &fakeExecutor{out: "%1"}}
	tm := NewTmux()
	tm.exec = rec
	for range 2 {
		if err := tm.newSessionWithCommandAndEnv("gc-nocomp", "", "codex", nil, runtime.ProgramStatusArm{Capture: true, Relay: true}); err != nil {
			t.Fatalf("create without a companion = %v, want today's create", err)
		}
	}
	for _, needle := range []string{"pipe-pane", "@gc_ps", " relay -- ", "display-message"} {
		if got := callsContaining(rec.calls, needle); len(got) != 0 {
			t.Fatalf("%q reached tmux without a companion: %q", needle, got)
		}
	}
	if n := strings.Count(warnings.String(), "warning:"); n != 1 || !strings.Contains(warnings.String(), "not found") {
		t.Fatalf("companion-missing warnings = %d, want 1 naming the missing companion: %q", n, warnings.String())
	}
}

func TestCapabilityGateRefusesCompanionCodecMismatchOnce(t *testing.T) {
	bin, runs := writeFakeCompanion(t, "gc-osc7501 dev codec=99 relay=1")
	warnings := captureWarnings(t)
	cfg := DefaultConfig()
	cfg.ProgramStatusBin = bin
	tm := NewTmuxWithConfig(cfg)
	rec := &stagedRecorder{fakeExecutor: &fakeExecutor{out: "%1"}}
	tm.exec = rec
	for range 2 {
		if err := tm.newSessionWithCommandAndEnv("gc-codec", "", "codex", nil, runtime.ProgramStatusArm{Capture: true}); err != nil {
			t.Fatal(err)
		}
	}
	if got := callsContaining(rec.calls, "pipe-pane"); len(got) != 0 {
		t.Fatalf("armed under a codec mismatch: %q", got)
	}
	if n := countLines(t, runs); n != 1 {
		t.Fatalf("companion probed %d times, want once per path", n)
	}
	if !strings.Contains(warnings.String(), "codec") || strings.Count(warnings.String(), "warning:") != 1 {
		t.Fatalf("codec mismatch warnings: %q", warnings.String())
	}
}

func TestCapabilityGateRefusesOldTmuxAndUnsafeSocket(t *testing.T) {
	for _, tc := range []struct {
		out string
		ok  bool
	}{
		{"3.4|/tmp/tmux-1000/default", true},
		{"3.2|/tmp/tmux-1000/gc-city.x_y-z", true},
		{"3.2a|/tmp/s", true},
		{"next-3.6|/tmp/s", true},
		{"master|/tmp/s", true},
		{"4.0|/tmp/s", true},
		{"3.1c|/tmp/s", false},
		{"2.9|/tmp/s", false},
		{"3.4|/tmp/has space/s", false},
		{"3.4|/tmp/it's", false},
		{"3.4|/tmp/#{x}", false},
		{"3.4|", false},
		{"3.4", false},
		{"|/tmp/s", false},
	} {
		_, err := captureTmuxVerdict(tc.out)
		if (err == nil) != tc.ok {
			t.Errorf("captureTmuxVerdict(%q) = %v, want ok=%v", tc.out, err, tc.ok)
		}
	}

	// Through the gate: the verdict is cached once tmux answers.
	bin, _ := writeFakeCompanion(t, "gc-osc7501 dev codec=1 relay=1")
	warnings := captureWarnings(t)
	cfg := DefaultConfig()
	cfg.ProgramStatusBin = bin
	tm := NewTmuxWithConfig(cfg)
	rec := &stagedRecorder{fakeExecutor: &fakeExecutor{outs: []string{"3.1|/tmp/s"}, out: "%1"}}
	tm.exec = rec
	for _, name := range []string{"gc-old1", "gc-old2"} {
		if err := tm.newSessionWithCommandAndEnv(name, "", "codex", nil, runtime.ProgramStatusArm{Capture: true}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(callsContaining(rec.calls, "#{version}|#{socket_path}")); n != 1 {
		t.Fatalf("tmux checked %d times, want once", n)
	}
	if got := callsContaining(rec.calls, "pipe-pane"); len(got) != 0 {
		t.Fatalf("armed under tmux 3.1: %q", got)
	}
	if !strings.Contains(warnings.String(), "older than 3.2") {
		t.Fatalf("old tmux not logged: %q", warnings.String())
	}
}

func TestCaptureNonceRandIsCodecValid(t *testing.T) {
	seen := make(map[string]bool)
	for range 200 {
		r, err := newCaptureRand()
		if err != nil {
			t.Fatal(err)
		}
		if !programstatus.ValidNonce(r+".%12") || seen[r] {
			t.Fatalf("rand %q invalid or repeated", r)
		}
		seen[r] = true
	}
}

func TestTapCommandEscapesFormatAndShellCharacters(t *testing.T) {
	// pipe-pane runs its command through strftime as well as format
	// expansion: a literal '%' is doubled like a literal '#'.
	p := &capturePlan{
		capability: captureCapability{companion: "/opt/a#b/my bin/100%/gc-osc7501", tmux: "/usr/local/%Y/tmux"},
		app:        "claude-code",
		rand:       "r_1-x",
	}
	want := `exec '/opt/a##b/my bin/100%%/gc-osc7501' tap -t #{pane_id} -n r_1-x -x /usr/local/%%Y/tmux -S #{socket_path} -a claude-code`
	if got := p.tapCommand(); got != want {
		t.Fatalf("tapCommand = %q, want %q", got, want)
	}
}

func TestInvalidAppIsNeverArmed(t *testing.T) {
	tm, rec, warnings, probes := captureTestTmux(t)
	rec.outs = []string{"%1"}
	if err := tm.newSessionWithCommandAndEnv("gc-badapp", "", "codex", nil, runtime.ProgramStatusArm{Capture: true, App: strings.Repeat("a", 33)}); err != nil {
		t.Fatal(err)
	}
	if *probes != 0 || len(callsContaining(rec.calls, "pipe-pane")) != 0 {
		t.Fatalf("armed an app the codec rejects: %q", rec.calls)
	}
	if !strings.Contains(warnings.String(), "app") {
		t.Fatalf("bad app not logged: %q", warnings.String())
	}
}

// An unarmed respawn while the switch is shadow or on (the provider declares
// no capture, or the capability gate refuses) removes the previous
// incarnation's capture in the respawn's own list, after the marker: the
// nonce, the record and then the pipe, so the old tap can never publish the
// new program under its nonce.
func TestUnarmedRespawnUnderLiveSwitchRemovesCapture(t *testing.T) {
	for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusShadow, runtime.ProgramStatusOn} {
		for name, arm := range map[string]runtime.ProgramStatusArm{
			"undeclared":   {},
			"gate refused": {Capture: true},
		} {
			t.Run(string(mode)+"/"+name, func(t *testing.T) {
				tm, rec, _, _ := captureTestTmux(t)
				tm.cfg.ProgramStatus = mode
				tm.capture.probe = func() (captureCapability, error) {
					return captureCapability{}, errors.New("companion missing")
				}
				if err := tm.respawnPaneWithWorkDir("%5", "/w", "codex", arm); err != nil {
					t.Fatalf("respawnPaneWithWorkDir: %v", err)
				}
				want := append([]string{"-u"}, tmuxArgvList([][]string{
					{"respawn-pane", "-k", "-t", "%5", "-c", "/w", "codex"},
					{"display-message", "-p", "-t", "%5", "#{pane_id}"},
					{"set-option", "-pu", "-t", "%5", programTapOption},
					{"set-option", "-pu", "-t", "%5", programStatusOption},
					{"set-option", "-pu", "-t", "%5", programTextOption},
					{"pipe-pane", "-t", "%5"},
				})...)
				if len(rec.calls) != 1 || !slices.Equal(rec.calls[0], want) {
					t.Fatalf("respawn calls\n got %q\nwant [%q]", rec.calls, want)
				}
			})
		}
	}
}

// A respawn that ran but whose capture removal failed is still a respawn:
// the failure is logged, never returned, whatever the switch.
func TestUnarmedRespawnDisarmFailureKeepsTheRespawn(t *testing.T) {
	for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusOff, runtime.ProgramStatusOn} {
		t.Run(string(mode), func(t *testing.T) {
			tm, rec, warnings, _ := captureTestTmux(t)
			tm.cfg.ProgramStatus = mode
			rec.errs = []error{&tmuxOutputError{Stdout: "%5", Err: errors.New("tmux pipe-pane: target pane has exited")}}
			if err := tm.respawnPaneWithWorkDir("%5", "", "codex", runtime.ProgramStatusArm{}); err != nil {
				t.Fatalf("respawn whose disarm failed = %v, want nil (the marker proves it respawned)", err)
			}
			if !strings.Contains(warnings.String(), "removing program-status capture") {
				t.Fatalf("disarm failure not logged: %q", warnings.String())
			}
			rec.errs, rec.idx = []error{ErrSessionNotFound}, 0
			if err := tm.respawnPaneWithWorkDir("%5", "", "codex", runtime.ProgramStatusArm{}); !errors.Is(err, ErrSessionNotFound) {
				t.Fatalf("err = %v, want the respawn's own failure", err)
			}
		})
	}
}

// An unarmed respawn while the switch is off (the provider undeclared, or
// the capability gate refused) runs today's respawn-pane and then, after the
// marker, removes the capture only from a pane bearing a nonce bound to it:
// a capture armed before the switch went off cannot read valid for the new
// program once the switch is back on, and a pane never armed keeps a pipe
// of its own.
func TestRespawnUnderSwitchOffRemovesOnlyABoundCapture(t *testing.T) {
	for _, mode := range []runtime.ProgramStatusMode{"", runtime.ProgramStatusOff} {
		for name, arm := range map[string]runtime.ProgramStatusArm{
			"undeclared":   {},
			"gate refused": {Capture: true},
		} {
			t.Run("switch "+string(mode)+"/"+name, func(t *testing.T) {
				tm, rec, _, _ := captureTestTmux(t)
				tm.cfg.ProgramStatus = mode
				tm.capture.probe = func() (captureCapability, error) {
					return captureCapability{}, errors.New("companion missing")
				}
				if err := tm.respawnPaneWithWorkDir("%5", "/w", "codex", arm); err != nil {
					t.Fatalf("respawnPaneWithWorkDir: %v", err)
				}
				want := append([]string{"-u"}, tmuxArgvList([][]string{
					{"respawn-pane", "-k", "-t", "%5", "-c", "/w", "codex"},
					{"display-message", "-p", "-t", "%5", "#{pane_id}"},
					{"if-shell", "-F", "-t", "%5", "#{m:*.#{pane_id},#{@gc_ps_tap}}", offDisarmLine("%5")},
				})...)
				if len(rec.calls) != 1 || !slices.Equal(rec.calls[0], want) {
					t.Fatalf("respawn calls\n got %q\nwant [%q]", rec.calls, want)
				}
			})
		}
	}
}
