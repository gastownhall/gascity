//go:build integration && linux

package tmux

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
	"github.com/gastownhall/gascity/internal/runtime/programstatus/tap"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest/osc7501emitter/emit"
)

// RUNTIME-CONTRACT-017 on the real tmux: the runtimetest program-status
// suite (PS-001…PS-012) against sessions whose agent pane runs the scripted
// emitter, armed with the built companion, each on its own scratch server.
// The relayed fixture and the heal cases (PS-013's heal half, PS-014) skip:
// the relay's tmux fixture and ProgramCaptureHealer land with later slices.
func TestTmuxProgramStatusContract(t *testing.T) {
	companion := captureCompanionBin(t)
	emitter := captureEmitterBin(t)
	start := func(t *testing.T, mode runtime.ProgramStatusMode, armed bool, script []string) *programSession {
		return startProgramSession(t, companion, emitter, mode, armed, script)
	}
	runtimetest.RunProgramStatusContract(t, runtimetest.ProgramStatusFixtures{
		Armed: func(t *testing.T, mode runtime.ProgramStatusMode, script ...string) runtimetest.ProgramStatusSession {
			return start(t, mode, true, script).session()
		},
		Unarmed: func(t *testing.T, mode runtime.ProgramStatusMode, script ...string) runtimetest.ProgramStatusSession {
			return start(t, mode, false, script).session()
		},
		Inherited: func(t *testing.T, mode runtime.ProgramStatusMode) runtimetest.ProgramStatusSession {
			s := start(t, mode, false, nil)
			s.inheritForeignCapture(t)
			return s.session()
		},
	})
}

// programSession is one contract fixture session: an emitter in a tmux pane,
// armed for capture or not, on a scratch server of its own.
type programSession struct {
	p       *Provider
	name    string
	emitter string
	cfg     runtime.Config
	nonce   string // the armed incarnation's nonce; "" when unarmed
	out     []byte // everything this incarnation's program wrote
	stale   string // the record the incarnation before the last respawn published
}

// startProgramSession starts the emitter running script, then taking steps
// typed into its pane, and waits until the capture has published what the
// script wrote.
func startProgramSession(t *testing.T, companion, emitter string, mode runtime.ProgramStatusMode, armed bool, script []string) *programSession {
	t.Helper()
	p := captureProvider(t, newCaptureScratchServer(t), companion)
	p.tm.cfg.ProgramStatus, p.cfg.ProgramStatus = mode, mode
	s := &programSession{p: p, name: "gc-ps-contract", emitter: emitter}
	s.cfg = runtime.Config{Command: s.command(script), WorkDir: t.TempDir()}
	if armed {
		s.cfg.ProgramStatus = runtime.ProgramStatusArm{Capture: true}
	}
	if err := p.Start(context.Background(), s.name, s.cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(s.name) })
	if armed {
		s.nonce = armedNonce(t, p, s.name)
	}
	s.settle(t, script)
	return s
}

// command is the pane command: the emitter running script, then each typed
// line as a step.
func (s *programSession) command(script []string) string {
	return emitterCommand(s.emitter, append(append([]string(nil), script...), "interactive")...)
}

// session is the fixture's runtimetest view.
func (s *programSession) session() runtimetest.ProgramStatusSession {
	return runtimetest.ProgramStatusSession{
		Provider:     s.p,
		Name:         s.name,
		Emit:         s.emit,
		Respawn:      s.respawn,
		PublishStale: s.publishStale,
		ClosePipe:    s.closePipe,
		KillReader:   s.killReader,
		ForeignPipe:  s.foreignPipe,
		SetMode:      s.setMode,
	}
}

// target is the session's agent pane.
func (s *programSession) target(t *testing.T) string {
	t.Helper()
	target, err := s.p.tm.agentPaneTarget(s.name)
	if err != nil {
		t.Fatalf("agent pane of %s: %v", s.name, err)
	}
	return target
}

// tmux runs a tmux command on the session's server.
func (s *programSession) tmux(t *testing.T, args ...string) string {
	t.Helper()
	out, err := s.p.tm.run(args...)
	if err != nil {
		t.Fatalf("tmux %q: %v", args, err)
	}
	return strings.TrimSpace(out)
}

// paneFormat expands format for the session's agent pane.
func (s *programSession) paneFormat(t *testing.T, format string) string {
	t.Helper()
	return s.tmux(t, "display-message", "-p", "-t", s.target(t), format)
}

// emit types each step into the pane as one line, then waits until the
// capture has published what they wrote.
func (s *programSession) emit(t *testing.T, steps ...string) {
	t.Helper()
	target := s.target(t)
	for _, step := range steps {
		s.tmux(t, "send-keys", "-t", target, "-l", step)
		s.tmux(t, "send-keys", "-t", target, "Enter")
	}
	s.settle(t, steps)
}

// settle waits until the capture has processed steps: the pane is dead when
// they end the program, and an armed pane carries the record the tap engine
// makes of everything this incarnation wrote (its Seq counts every change).
// The next read then sees it.
func (s *programSession) settle(t *testing.T, steps []string) {
	t.Helper()
	ended := s.run(t, steps)
	if ended {
		eventually(t, "the pane to die", func() bool { return s.paneFormat(t, "#{pane_dead}") == "1" })
	}
	if want := s.expectedSeq(t); want > 0 {
		eventually(t, "the capture to publish record "+strconv.FormatUint(want, 10), func() bool {
			rec, err := programstatus.Decode(paneOption(t, s.p, s.name, programStatusOption))
			return err == nil && rec.Nonce == s.nonce && rec.Seq == want
		})
	}
	s.p.cache.Invalidate()
}

// run runs steps as the pane's program does, appending what it writes to
// the incarnation's output, and reports whether they end the program. No
// probe is answered, as tmux answers none.
func (s *programSession) run(t *testing.T, steps []string) bool {
	t.Helper()
	if len(steps) == 0 {
		return false
	}
	prog, err := emit.ParseLines(steps...)
	if err != nil {
		t.Fatalf("emitter script: %v", err)
	}
	var out bytes.Buffer
	outcome, err := emit.Run(t.Context(), prog, emit.Terminal{Out: &out})
	if err != nil {
		t.Fatalf("running emitter script: %v", err)
	}
	s.out = append(s.out, out.Bytes()...)
	return outcome.Ended
}

// expectedSeq is the Seq of the record the tap engine publishes for this
// incarnation's output, 0 when it publishes none or the pane is unarmed.
func (s *programSession) expectedSeq(t *testing.T) uint64 {
	t.Helper()
	if s.nonce == "" {
		return 0
	}
	var last lastPublish
	if err := tap.Run(t.Context(), bytes.NewReader(s.out), &last, tap.Options{Nonce: s.nonce}); err != nil {
		t.Fatalf("predicting the capture: %v", err)
	}
	if last.status == "" {
		return 0
	}
	rec, err := programstatus.Decode(last.status)
	if err != nil {
		t.Fatalf("predicted record %q: %v", last.status, err)
	}
	return rec.Seq
}

// lastPublish keeps a tap engine's last write.
type lastPublish struct{ status string }

func (l *lastPublish) Publish(_ context.Context, w tap.Write) error {
	l.status = w.Status
	return nil
}

// respawn restarts the pane's program, a new capture incarnation, keeping
// the outgoing incarnation's record for publishStale.
func (s *programSession) respawn(t *testing.T, script ...string) {
	t.Helper()
	s.stale = paneOption(t, s.p, s.name, programStatusOption)
	s.cfg.Command = s.command(script)
	if err := s.p.Relaunch(context.Background(), s.name, s.cfg); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	prev := s.nonce
	s.nonce, s.out = armedNonce(t, s.p, s.name), nil
	if s.nonce == prev {
		t.Fatalf("respawn kept nonce %q", prev)
	}
	s.settle(t, script)
}

// publishStale writes the previous incarnation's record onto the pane, as a
// late write that got past the nonce guard would.
func (s *programSession) publishStale(t *testing.T) {
	t.Helper()
	if s.stale == "" {
		t.Fatal("no previous incarnation's record to publish")
	}
	s.tmux(t, "set-option", "-p", "-t", s.target(t), programStatusOption, s.stale)
	s.p.cache.Invalidate()
}

// closePipe closes the pane's pipe; the tap reads EOF.
func (s *programSession) closePipe(t *testing.T) {
	t.Helper()
	s.tmux(t, "pipe-pane", "-t", s.target(t))
	eventually(t, "the pipe to close", func() bool { return s.paneFormat(t, "#{pane_pipe}") == "0" })
	s.p.cache.Invalidate()
}

// killReader kills the tap, leaving the pipe as tmux holds it.
func (s *programSession) killReader(t *testing.T) {
	t.Helper()
	pids := tapPIDs(t, s.nonce)
	if len(pids) == 0 {
		t.Fatalf("no tap holds %s", s.nonce)
	}
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatalf("killing tap %d: %v", pid, err)
		}
	}
	eventually(t, "the tap to die", func() bool { return !tapRunning(t, s.nonce) })
	s.p.cache.Invalidate()
}

// foreignPipe pipes the pane into a reader that is not ours, which replaces
// the tap's pipe: the tap reads EOF and exits.
func (s *programSession) foreignPipe(t *testing.T) {
	t.Helper()
	s.tmux(t, "pipe-pane", "-O", "-t", s.target(t), "cat > /dev/null")
	eventually(t, "the displaced tap to exit", func() bool { return !tapRunning(t, s.nonce) })
	s.p.cache.Invalidate()
}

// setMode flips the switch the provider reads under.
func (s *programSession) setMode(t *testing.T, mode runtime.ProgramStatusMode) {
	t.Helper()
	s.p.tm.cfg.ProgramStatus, s.p.cfg.ProgramStatus = mode, mode
	s.p.cache.Invalidate()
}

// inheritForeignCapture sets a global tap nonce and a working record under
// it that name another pane than the session's, which every pane without
// values of its own inherits.
func (s *programSession) inheritForeignCapture(t *testing.T) {
	t.Helper()
	pane := s.paneFormat(t, "#{pane_id}")
	n, err := strconv.Atoi(strings.TrimPrefix(pane, "%"))
	if err != nil {
		t.Fatalf("pane id %q: %v", pane, err)
	}
	nonce := "inH3r1t.%" + strconv.Itoa(n+1000)
	store := programstatus.NewStore(nonce)
	store.Apply(programstatus.Report{State: runtime.ProgramWorking, Progress: -1}, time.Now())
	record, err := programstatus.Encode(store.Status())
	if err != nil {
		t.Fatalf("encoding the inherited record: %v", err)
	}
	s.tmux(t, "set-option", "-g", programTapOption, nonce)
	s.tmux(t, "set-option", "-g", programStatusOption, record)
	if got := s.paneFormat(t, "#{"+programTapOption+"}"); got != nonce {
		t.Fatalf("pane reads tap %q, want the inherited %q", got, nonce)
	}
	s.p.cache.Invalidate()
}
