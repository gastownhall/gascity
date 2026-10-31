package tmux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

// Heal and disarm of program-status capture (design §5.4). Only the
// controller calls these, on its runtime inventory lane's cadence. Each acts
// on one agent pane at a time, judged from a fresh read of that pane: never
// on a dead pane (pipe-pane refuses one), never on a pipe that is not
// provably this runtime's tap (a foreign pipe is never taken over), and
// never on a pane whose last attempt failed until its backoff has passed.

// A pane whose heal keeps failing is retried after captureHealBackoffBase,
// doubling each failure up to captureHealBackoffMax. The backoff is soft
// controller memory: losing it costs one extra attempt.
const (
	captureHealBackoffBase = 30 * time.Second
	captureHealBackoffMax  = 10 * time.Minute
)

// captureHealFormat is heal's per-pane list-panes row of one session:
// pane_dead, the window id the heal target fences to, then the capture
// fields, terminated against the executor's trim.
const captureHealFormat = "#{pane_dead}\t#{window_id}\t" + programPaneFormat + "\t" + runtimeInventoryEnd

// captureHealFields is the field count of captureHealFormat.
const captureHealFields = 2 + programPaneFields + 1

// captureSweepFormat is the disarm sweep's list-panes -a row: the pane's
// session, window and id, whether it is dead, and its sanitized tap nonce.
const captureSweepFormat = "#{session_name}\t#{window_id}\t#{pane_id}\t#{pane_dead}\t" +
	"#{s/" + programReadSanitizer + "//:" + programTapOption + "}\t" + runtimeInventoryEnd

// captureSweepFields is the field count of captureSweepFormat.
const captureSweepFields = 6

// captureHealBackoff is heal's per-pane backoff, keyed by pane id.
type captureHealBackoff struct {
	// now is the backoff clock; nil selects time.Now. Tests inject one.
	now func() time.Time

	mu    sync.Mutex
	panes map[string]captureHealRetry
}

// captureHealRetry is one pane's failed heals.
type captureHealRetry struct {
	failures int
	next     time.Time
}

func (b *captureHealBackoff) clock() time.Time {
	if b.now == nil {
		return time.Now()
	}
	return b.now()
}

// waiting reports whether pane is still inside its backoff window.
func (b *captureHealBackoff) waiting(pane string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.panes[pane]
	return ok && b.clock().Before(r.next)
}

// failed records a failed heal of pane and returns the window before the
// next attempt. Entries long past their window are dropped, so panes that
// vanished do not accumulate.
func (b *captureHealBackoff) failed(pane string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	if b.panes == nil {
		b.panes = make(map[string]captureHealRetry)
	}
	for id, r := range b.panes {
		if now.Sub(r.next) > captureHealBackoffMax {
			delete(b.panes, id)
		}
	}
	r := b.panes[pane]
	window := captureHealBackoffBase << min(r.failures, 8)
	if window > captureHealBackoffMax || window <= 0 {
		window = captureHealBackoffMax
	}
	r.failures++
	r.next = now.Add(window)
	b.panes[pane] = r
	return window
}

// succeeded clears pane's backoff.
func (b *captureHealBackoff) succeeded(pane string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.panes, pane)
}

// healPane is a session's agent pane as heal reads it: its capture fields,
// the decoded record's form, and the fenced target heal acts on.
type healPane struct {
	programPane
	target string
	form   runtime.ProgramRecordForm
}

// ownNonce reports whether the pane carries a tap nonce bound to its own pane
// id: one this runtime armed on it.
func (p programPane) ownNonce() bool {
	rand, ok := strings.CutSuffix(p.tap, "."+p.id)
	return ok && rand != "" && wellFormedTmuxID(p.id, '%')
}

// HealProgramCapture implements [runtime.ProgramCaptureHealer]: it converges
// session name's agent-pane capture toward want, reading the pane afresh.
//
//   - want declares capture: a pane with no pipe is armed under a new nonce
//     by an if-shell on #{pane_pipe} around the arm list, so a pipe that
//     opened in between is never replaced; an open pipe that is not ours, or
//     ours with its tap gone from the process table (tmux closes it at the
//     pane's next output), is deferred; our live tap writing a codec this gc
//     does not read is replaced; anything else is unchanged.
//   - want declares nothing: a pane carrying our nonce is disarmed (its pipe
//     closed only when the process table shows our tap holding it, then the
//     capture options unset); any other pane is unchanged.
//
// A dead pane, a session whose agent pane cannot be named, a failed
// capability gate, or a pane inside its backoff after a failed heal is
// deferred. A missing session is an error wrapping
// [runtime.ErrSessionNotFound]. Only the controller calls it.
func (p *Provider) HealProgramCapture(name string, want runtime.ProgramStatusArm) (runtime.ProgramCaptureAction, error) {
	pane, named, err := p.tm.readHealPane(name)
	if err != nil {
		return "", err
	}
	if !named || pane.dead {
		return runtime.ProgramCaptureDeferred, nil
	}
	ours := pane.ownNonce()
	if !want.Capture {
		if !ours {
			return runtime.ProgramCaptureUnchanged, nil
		}
		return p.healAct(pane, runtime.ProgramCaptureDisarmed, func() (bool, error) {
			return true, p.tm.disarmPane(pane.target, p.captureTaps().liveness(pane.tap) == runtime.ProgramTapAlive)
		})
	}
	if pane.pipe {
		if !ours {
			return runtime.ProgramCaptureDeferred, nil
		}
		switch p.captureTaps().liveness(pane.tap) {
		case runtime.ProgramTapGone:
			return runtime.ProgramCaptureDeferred, nil
		case runtime.ProgramTapAlive:
			if pane.form == runtime.ProgramRecordUnknownVersion {
				return p.healAct(pane, runtime.ProgramCaptureArmed, func() (bool, error) {
					return p.tm.replaceTap(pane, want)
				})
			}
		}
		return runtime.ProgramCaptureUnchanged, nil
	}
	return p.healAct(pane, runtime.ProgramCaptureArmed, func() (bool, error) {
		return p.tm.armIdlePane(pane, want)
	})
}

// healAct runs one heal mutation of pane unless the pane is inside its
// backoff. act reports whether it acted (false: the gate failed or the pipe
// was taken in between, which defers without counting as a failure). A
// failure starts or extends the pane's backoff.
func (p *Provider) healAct(pane healPane, done runtime.ProgramCaptureAction, act func() (bool, error)) (runtime.ProgramCaptureAction, error) {
	if p.heal.waiting(pane.id) {
		return runtime.ProgramCaptureDeferred, nil
	}
	acted, err := act()
	if err != nil {
		window := p.heal.failed(pane.id)
		return "", fmt.Errorf("healing program capture of pane %s (next attempt in %v): %w", pane.id, window, err)
	}
	p.heal.succeeded(pane.id)
	if !acted {
		return runtime.ProgramCaptureDeferred, nil
	}
	p.invalidateCache()
	return done, nil
}

// invalidateCache makes the next read refetch, so it sees what heal changed.
func (p *Provider) invalidateCache() {
	if p.cache != nil {
		p.cache.Invalidate()
	}
}

// readHealPane reads session name's panes and returns its agent pane
// (foldProgramPane), reporting false when no pane can be named.
func (t *Tmux) readHealPane(name string) (healPane, bool, error) {
	out, err := t.run("list-panes", "-s", "-t", paneTarget(name), "-F", captureHealFormat)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoServer) {
			return healPane{}, false, fmt.Errorf("healing program capture of %q: %w: %w", name, runtime.ErrSessionNotFound, err)
		}
		return healPane{}, false, fmt.Errorf("healing program capture of %q: reading its panes: %w", name, err)
	}
	var rows []programPane
	windows := make(map[string]string)
	records := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != captureHealFields || parts[captureHealFields-1] != runtimeInventoryEnd || !wellFormedTmuxID(parts[1], '@') {
			return healPane{}, false, nil
		}
		pane, ok := parseProgramPane(parts[2:captureHealFields-1], parts[0] == "1")
		if !ok {
			return healPane{}, false, nil
		}
		rows = append(rows, pane)
		windows[pane.id] = parts[1]
		records[pane.id] = pane.record
	}
	pane, ok := foldProgramPane(rows)
	if !ok {
		return healPane{}, false, nil
	}
	_, decodeErr := programstatus.Decode(records[pane.id])
	return healPane{
		programPane: pane,
		target:      paneTarget(name) + windows[pane.id] + "." + pane.id,
		form:        programstatus.RecordForm(decodeErr),
	}, true, nil
}

// armIdlePane arms pane under a new nonce only if it still has no pipe when
// tmux runs the list, and reports whether it did: the nonce it reads back
// is the one it set.
func (t *Tmux) armIdlePane(pane healPane, want runtime.ProgramStatusArm) (bool, error) {
	plan := t.planCapture(want)
	if plan == nil {
		return false, nil
	}
	armList := tmuxCommandLines(plan.armCommands(pane.target, true))
	out, err := t.run("if-shell", "-F", "-t", pane.target, "#{pane_pipe}", "", armList,
		";", "display-message", "-p", "-t", pane.target, "#{s/"+programReadSanitizer+"//:"+programTapOption+"}")
	if err != nil {
		return false, fmt.Errorf("arming %s: %w", pane.target, err)
	}
	return strings.TrimSpace(out) == plan.rand+"."+pane.id, nil
}

// replaceTap replaces our live tap, whose pipe is provably ours, with one
// under a new nonce from the current companion, clearing the record the old
// codec wrote.
func (t *Tmux) replaceTap(pane healPane, want runtime.ProgramStatusArm) (bool, error) {
	plan := t.planCapture(want)
	if plan == nil {
		return false, nil
	}
	if _, err := t.run(tmuxArgvList(plan.armCommands(pane.target, true))...); err != nil {
		return false, fmt.Errorf("replacing the tap of %s: %w", pane.target, err)
	}
	return true, nil
}

// disarmPane unsets target's capture options, first closing its pipe when
// closePipe says the pipe is our tap's. A pipe that may be foreign is never
// closed.
func (t *Tmux) disarmPane(target string, closePipe bool) error {
	var cmds [][]string
	if closePipe {
		cmds = append(cmds, []string{"pipe-pane", "-t", target})
	}
	for _, opt := range []string{programTapOption, programStatusOption, programTextOption} {
		cmds = append(cmds, []string{"set-option", "-pu", "-t", target, opt})
	}
	if _, err := t.run(tmuxArgvList(cmds)...); err != nil {
		return fmt.Errorf("disarming %s: %w", target, err)
	}
	return nil
}

// captureTaps reads the process table afresh for tap liveness, through the
// state cache fetcher's snapshot and its failure backoff. An unread table
// knows no tap's liveness.
func (p *Provider) captureTaps() programTaps {
	if p.cache == nil {
		return programTaps{}
	}
	f, ok := p.cache.fetcher.(*tmuxFetcher)
	if !ok || !f.snapshotGate.allow(time.Now()) {
		return programTaps{}
	}
	snapshot := f.snapshot
	if snapshot == nil {
		snapshot = fetchProcessSnapshot
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	processes, err := snapshot(ctx)
	if err != nil {
		f.snapshotGate.failed(time.Now())
		return programTaps{}
	}
	f.snapshotGate.succeeded()
	return programTaps{known: true, nonces: tapNonces(processes)}
}

// DisarmProgramCaptures is the controller's disarm sweep for a city whose
// program-status switch is off, run once per config load: one list-panes -a
// of every pane's tap nonce, then the disarm rule of [Provider.HealProgramCapture]
// for each live pane bearing a nonce bound to it. It returns how many panes it
// disarmed. With no server, or one holding no sessions, there is nothing to
// disarm.
func (p *Provider) DisarmProgramCaptures() (int, error) {
	out, err := p.tm.run("list-panes", "-a", "-F", captureSweepFormat)
	if err != nil {
		if errors.Is(err, ErrNoServer) {
			return 0, nil
		}
		return 0, fmt.Errorf("listing panes for the program-capture disarm sweep: %w", err)
	}
	type armed struct{ target, tap string }
	var panes []armed
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != captureSweepFields || parts[captureSweepFields-1] != runtimeInventoryEnd || parts[3] != "0" {
			continue
		}
		pane := programPane{id: parts[2], tap: parts[4]}
		if parts[0] == "" || !wellFormedTmuxID(parts[1], '@') || !pane.ownNonce() {
			continue
		}
		panes = append(panes, armed{target: paneTarget(parts[0]) + parts[1] + "." + pane.id, tap: pane.tap})
	}
	if len(panes) == 0 {
		return 0, nil
	}
	taps := p.captureTaps()
	disarmed := 0
	var errs []error
	for _, pane := range panes {
		if err := p.tm.disarmPane(pane.target, taps.liveness(pane.tap) == runtime.ProgramTapAlive); err != nil {
			errs = append(errs, err)
			continue
		}
		disarmed++
	}
	if disarmed > 0 {
		p.invalidateCache()
	}
	return disarmed, errors.Join(errs...)
}

// ProgramCaptureFloor checks the capture capability floor (design §5.5) for
// gc doctor without starting a tmux server: the companion resolves and
// writes this gc's codec, and the city's running server is tmux 3.2 or later
// on a socket path the tap command can carry. It returns nil when capture can
// arm, an error wrapping [runtime.ErrRuntimeUnavailable] when no server is
// running to check, and otherwise the failed check's cause.
func (p *Provider) ProgramCaptureFloor() error {
	t := p.tm
	if t.capture.probe != nil {
		_, err := t.capture.probe()
		return err
	}
	companion := t.cfg.ProgramStatusBin
	if companion == "" {
		var err error
		if companion, err = programstatus.LookupCompanion(); err != nil {
			return err
		}
	}
	if err := t.checkCompanion(companion); err != nil {
		return err
	}
	g := &t.capture
	g.mu.Lock()
	done, verdict := g.tmuxDone, g.tmuxErr
	g.mu.Unlock()
	if done {
		return verdict
	}
	out, err := t.run("display-message", "-p", "#{version}|#{socket_path}")
	if err != nil {
		if errors.Is(err, ErrNoServer) {
			return fmt.Errorf("tmux server not running: %w: %w", runtime.ErrRuntimeUnavailable, err)
		}
		return fmt.Errorf("reading tmux version and socket path: %w", err)
	}
	_, verdict = captureTmuxVerdict(strings.TrimSpace(out))
	return verdict
}

var _ runtime.ProgramCaptureHealer = (*Provider)(nil)
