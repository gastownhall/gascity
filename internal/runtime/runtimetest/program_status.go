package runtimetest

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ProgramStatusFixtures supplies the sessions the RUNTIME-CONTRACT-017 cases
// (PS-001…PS-014) read, for a provider implementing
// [runtime.ProgramStatusProvider]. Each fixture starts a session whose agent
// pane runs the scripted OSC 7501 emitter (package
// runtimetest/osc7501emitter/emit) under the given city switch mode,
// registers its cleanup, and returns it. A nil fixture, or a nil hook on a
// session it returns, skips the cases that need it.
type ProgramStatusFixtures struct {
	// Armed starts a session whose agent pane is armed for capture and whose
	// program runs script, then keeps taking steps through Emit.
	Armed func(t *testing.T, mode runtime.ProgramStatusMode, script ...string) ProgramStatusSession
	// Unarmed starts a session as Armed does, but its pane was never armed:
	// the session declared no capture.
	Unarmed func(t *testing.T, mode runtime.ProgramStatusMode, script ...string) ProgramStatusSession
	// Inherited starts an unarmed session whose pane inherits capture values
	// from an outer scope (a window, session or global tap nonce and a
	// working record) that name another pane.
	Inherited func(t *testing.T, mode runtime.ProgramStatusMode) ProgramStatusSession
	// Relayed starts an armed session whose program runs under the
	// probe-answering relay.
	Relayed func(t *testing.T, mode runtime.ProgramStatusMode, script ...string) ProgramStatusSession
}

// ProgramStatusSession is one session a [ProgramStatusFixtures] fixture
// started, with hooks that act on its pane. Every hook returns once the
// capture has processed what it caused, so the next read sees it.
type ProgramStatusSession struct {
	Provider runtime.Provider
	Name     string
	// Emit has the session's program run steps (emitter script steps).
	Emit func(t *testing.T, steps ...string)
	// Respawn restarts the pane's program in place, a new capture
	// incarnation, running script.
	Respawn func(t *testing.T, script ...string)
	// PublishStale publishes, after a Respawn, the record of the incarnation
	// before it, as a late write from its reader would.
	PublishStale func(t *testing.T)
	// ClosePipe closes the pane's capture pipe.
	ClosePipe func(t *testing.T)
	// KillReader ends the capture reader process, leaving the pipe open.
	KillReader func(t *testing.T)
	// ForeignPipe attaches a reader that is not the runtime's to the pane,
	// displacing the capture reader.
	ForeignPipe func(t *testing.T)
	// PipeOpen reports whether a reader, the runtime's or another, holds
	// the pane's pipe.
	PipeOpen func(t *testing.T) bool
	// SetMode flips the city switch the provider reads under.
	SetMode func(t *testing.T, mode runtime.ProgramStatusMode)
}

// programStatusMissingSession is a session no fixture ever starts, so a
// responsive provider does not have it.
const programStatusMissingSession = "never-started-program-status-session"

// programStatusCase is one RUNTIME-CONTRACT-017 case: nil when the provider
// keeps the contract, errSkip when the fixtures cannot build its state, else
// how the provider breaks it.
type programStatusCase struct {
	name string
	run  func(t *testing.T, f ProgramStatusFixtures) error
}

// skipError is a case that cannot run with the fixtures given.
type skipError struct{ reason string }

func (e skipError) Error() string { return "skipped: " + e.reason }

func skip(reason string) error { return skipError{reason} }

// programStatusCases are PS-001…PS-014 in order.
var programStatusCases = []programStatusCase{
	{"PS-001_SwitchOffReadsUnread", checkPS001},
	{"PS-002_FirstReportIsValid", checkPS002},
	{"PS-003_ArmedSilentReadsSilent", checkPS003},
	{"PS-004_ProbeGatedNeedsRelay", checkPS004},
	{"PS-005_RespawnNeverRevalidatesOldIncarnation", checkPS005},
	{"PS-006_ReaderDeathReadsReaderLost", checkPS006},
	{"PS-007_PaneDeathReadsExitedKeepingLastError", checkPS007},
	{"PS-008_InheritedValuesNeverValid", checkPS008},
	{"PS-009_HoldLiveIsBounded", checkPS009},
	{"PS-010_BlockedKindsAreExact", checkPS010},
	{"PS-011_ChildrenLeaveRootTurnAlone", checkPS011},
	{"PS-012_AdvancedPastCreditsOnlyBoundaryBaselines", checkPS012},
	{"PS-013_ForeignPipeIsNeverTakenOver", checkPS013},
	{"PS-014_HealRearmsAndDisarms", checkPS014},
}

// RunProgramStatusContract runs the RUNTIME-CONTRACT-017 cases, PS-001…PS-014:
// a program-status reading is the pane program's own bounded claim, valid
// only from the pane's own armed incarnation with its reader attached,
// Unread under the switch off, and never revalidated after a respawn. Heal
// cases also need [runtime.ProgramCaptureHealer] and skip without it.
//
// Call it from a standalone test, never from a ledger-checked
// TestXxxConformance proof, which must stay a bare [RunProviderTests] call.
func RunProgramStatusContract(t *testing.T, f ProgramStatusFixtures) {
	t.Helper()
	for _, c := range programStatusCases {
		t.Run(c.name, func(t *testing.T) {
			err := c.run(t, f)
			var s skipError
			switch {
			case errors.As(err, &s):
				t.Skip(s.reason)
			case err != nil:
				t.Error(err)
			}
		})
	}
}

// read takes s's program-status reading through the routed leaf.
func read(s ProgramStatusSession) (runtime.ProgramStatus, error) {
	ps, ok := runtime.ProgramStatusFor(s.Provider, s.Name)
	if !ok {
		return runtime.ProgramStatus{}, fmt.Errorf("provider %T does not implement runtime.ProgramStatusProvider for %q", s.Provider, s.Name)
	}
	r, err := ps.ProgramStatus(s.Name)
	if err != nil {
		return r, fmt.Errorf("ProgramStatus(%q): %w", s.Name, err)
	}
	return r, nil
}

// readWant reads s and checks the reading's validity is one of want and
// that a reading not judged valid carries no state.
func readWant(s ProgramStatusSession, label string, want ...runtime.ProgramStatusValidity) (runtime.ProgramStatus, error) {
	r, err := read(s)
	if err != nil {
		return r, fmt.Errorf("%s: %w", label, err)
	}
	if !slices.Contains(want, r.Validity) {
		return r, fmt.Errorf("%s: validity %q, want one of %q (reading %+v)", label, r.Validity, want, r)
	}
	if r.Validity != runtime.ProgramStatusValid && (r.State != "" || r.Kind != "" || r.Valid()) {
		return r, fmt.Errorf("%s: a %q reading carries state: %+v", label, r.Validity, r)
	}
	if r.Validity == runtime.ProgramStatusValid && !r.Valid() {
		return r, fmt.Errorf("%s: a valid reading with no root state: %+v", label, r)
	}
	return r, nil
}

// readValid reads s and checks it is valid in state st.
func readValid(s ProgramStatusSession, label string, st runtime.ProgramState) (runtime.ProgramStatus, error) {
	r, err := readWant(s, label, runtime.ProgramStatusValid)
	if err == nil && r.State != st {
		err = fmt.Errorf("%s: state %q, want %q (reading %+v)", label, r.State, st, r)
	}
	return r, err
}

// healable is nil when s's routed leaf heals capture, else a skip.
func healable(s ProgramStatusSession) error {
	if _, ok := runtime.ProgramCaptureHealerFor(s.Provider, s.Name); !ok {
		return skip(fmt.Sprintf("provider %T does not implement runtime.ProgramCaptureHealer", s.Provider))
	}
	return nil
}

// heal heals s's capture toward want through its routed leaf and checks
// the action.
func heal(s ProgramStatusSession, label string, want runtime.ProgramStatusArm, action runtime.ProgramCaptureAction) error {
	h, ok := runtime.ProgramCaptureHealerFor(s.Provider, s.Name)
	if !ok {
		return fmt.Errorf("%s: provider %T does not implement runtime.ProgramCaptureHealer", label, s.Provider)
	}
	got, err := h.HealProgramCapture(s.Name, want)
	if err != nil {
		return fmt.Errorf("%s: HealProgramCapture(%q): %w", label, s.Name, err)
	}
	if got != action {
		return fmt.Errorf("%s: HealProgramCapture(%q) = %q, want %q", label, s.Name, got, action)
	}
	return nil
}

// wantSessionNotFound reports an operation on a missing session that did not
// fail with an error wrapping runtime.ErrSessionNotFound.
func wantSessionNotFound(label string, err error) error {
	if err == nil {
		return fmt.Errorf("%s succeeded, want an error wrapping runtime.ErrSessionNotFound", label)
	}
	return fmt.Errorf("%s: %w, want an error wrapping runtime.ErrSessionNotFound", label, err)
}

// captureClockSkew is how far a reading's Since, stamped on the capture
// host's clock, may stray from the test's clock.
const captureClockSkew = time.Minute

// captureAll is the heal target of an armed session.
var captureAll = runtime.ProgramStatusArm{Capture: true}

// PS-001: the switch off reads Unread with no state, even with an armed pane
// and a published record; flipping a valid session off does the same.
func checkPS001(t *testing.T, f ProgramStatusFixtures) error {
	if f.Armed == nil {
		return skip("no armed-session fixture")
	}
	s := f.Armed(t, runtime.ProgramStatusOff, "report state=working:app=psdemo")
	r, err := readWant(s, "switch off", runtime.ProgramStatusUnread)
	if err != nil {
		return err
	}
	if r.Mode != "" && r.Mode != runtime.ProgramStatusOff {
		return fmt.Errorf("switch off: mode %q, want off", r.Mode)
	}
	if r.Decides() || r.Running() || r.InputBoundary() {
		return fmt.Errorf("switch off: an Unread reading answers a predicate: %+v", r)
	}
	on := f.Armed(t, runtime.ProgramStatusOn, "report state=working:app=psdemo")
	if on.SetMode == nil {
		return nil
	}
	if _, err := readValid(on, "switch on", runtime.ProgramWorking); err != nil {
		return err
	}
	on.SetMode(t, runtime.ProgramStatusOff)
	_, err = readWant(on, "switch flipped off", runtime.ProgramStatusUnread)
	return err
}

// PS-002: the first report reads valid with its state, app, progress and
// kind exact; a shadow reading is valid but never decides.
func checkPS002(t *testing.T, f ProgramStatusFixtures) error {
	if f.Armed == nil {
		return skip("no armed-session fixture")
	}
	s := f.Armed(t, runtime.ProgramStatusOn, "report state=working:app=psdemo:progress=40")
	r, err := readValid(s, "first report", runtime.ProgramWorking)
	if err != nil {
		return err
	}
	switch {
	case r.App != "psdemo" || r.Progress != 40 || r.Kind != "":
		return fmt.Errorf("first report: app %q progress %d kind %q, want psdemo 40 none", r.App, r.Progress, r.Kind)
	case r.Mode != runtime.ProgramStatusOn || !r.Decides() || !r.Running():
		return fmt.Errorf("first report under on: %+v, want deciding and running", r)
	case r.Nonce == "" || r.TurnStarts != 1:
		return fmt.Errorf("first report: nonce %q turn starts %d, want a nonce and one turn", r.Nonce, r.TurnStarts)
	}
	s.Emit(t, "report state=blocked:kind=permission:app=psdemo")
	if r, err = readValid(s, "blocked report", runtime.ProgramBlocked); err != nil {
		return err
	}
	if r.Kind != runtime.ProgramBlockPermission || r.Running() || r.Progress != -1 {
		return fmt.Errorf("blocked report: %+v, want kind permission, not running, no progress", r)
	}
	shadow := f.Armed(t, runtime.ProgramStatusShadow, "report state=idle:app=psdemo")
	if r, err = readValid(shadow, "shadow report", runtime.ProgramIdle); err != nil {
		return err
	}
	if r.Mode != runtime.ProgramStatusShadow || r.Decides() || !r.InputBoundary() {
		return fmt.Errorf("shadow report: %+v, want an advisory input boundary", r)
	}
	return nil
}

// PS-003: an armed pane that reported nothing reads silent; an unarmed one
// reads not_armed; a session the provider does not have is an error
// wrapping ErrSessionNotFound.
func checkPS003(t *testing.T, f ProgramStatusFixtures) error {
	if f.Armed == nil {
		return skip("no armed-session fixture")
	}
	s := f.Armed(t, runtime.ProgramStatusOn)
	if _, err := readWant(s, "armed, nothing reported", runtime.ProgramStatusSilent); err != nil {
		return err
	}
	missing := ProgramStatusSession{Provider: s.Provider, Name: programStatusMissingSession}
	if _, err := read(missing); !errors.Is(err, runtime.ErrSessionNotFound) {
		return wantSessionNotFound("read of a missing session", err)
	}
	if f.Unarmed == nil {
		return nil
	}
	u := f.Unarmed(t, runtime.ProgramStatusOn, "report state=working")
	_, err := readWant(u, "unarmed pane", runtime.ProgramStatusNotArmed)
	return err
}

// PS-004: a probe-gated program with no relay stays silent with Probed
// set; under the relay it reports and reads valid.
func checkPS004(t *testing.T, f ProgramStatusFixtures) error {
	if f.Armed == nil {
		return skip("no armed-session fixture")
	}
	s := f.Armed(t, runtime.ProgramStatusOn, "term bel", "probe-gated", "report state=working")
	r, err := readWant(s, "probe-gated, no relay", runtime.ProgramStatusSilent)
	if err != nil {
		return err
	}
	if !r.Probed {
		return fmt.Errorf("probe-gated, no relay: %+v, want Probed", r)
	}
	if f.Relayed == nil {
		return nil
	}
	relayed := f.Relayed(t, runtime.ProgramStatusOn, "term bel", "probe-gated", "report state=working")
	_, err = readValid(relayed, "probe-gated under the relay", runtime.ProgramWorking)
	return err
}

// PS-005: a respawn arms a new nonce; nothing from the old incarnation,
// a late write included, is ever valid, and an old baseline credits nothing.
func checkPS005(t *testing.T, f ProgramStatusFixtures) error {
	if f.Armed == nil {
		return skip("no armed-session fixture")
	}
	s := f.Armed(t, runtime.ProgramStatusOn, "report state=idle")
	if s.Respawn == nil {
		return skip("no respawn hook")
	}
	old, err := readValid(s, "before respawn", runtime.ProgramIdle)
	if err != nil {
		return err
	}
	s.Respawn(t)
	r, err := readWant(s, "after respawn", runtime.ProgramStatusSilent)
	if err != nil {
		return err
	}
	if r.Nonce == old.Nonce {
		return fmt.Errorf("after respawn: nonce %q unchanged", r.Nonce)
	}
	if s.PublishStale != nil {
		s.PublishStale(t)
		if _, err := readWant(s, "late write from the old incarnation", runtime.ProgramStatusSilent); err != nil {
			return err
		}
	}
	s.Emit(t, "report state=working")
	r, err = readValid(s, "new incarnation's report", runtime.ProgramWorking)
	if err != nil {
		return err
	}
	if r.Nonce == old.Nonce {
		return fmt.Errorf("new incarnation reads the old nonce %q", r.Nonce)
	}
	if r.AdvancedPast(old.Baseline()) {
		return fmt.Errorf("a baseline from the old incarnation credited %+v", r)
	}
	return nil
}

// PS-006: reader death reads reader_lost, whether the pipe closed or the
// reader process is gone with the pipe left open.
func checkPS006(t *testing.T, f ProgramStatusFixtures) error {
	if f.Armed == nil {
		return skip("no armed-session fixture")
	}
	ran := false
	for _, death := range []struct {
		label string
		hook  func(ProgramStatusSession) func(*testing.T)
	}{
		{"pipe closed", func(s ProgramStatusSession) func(*testing.T) { return s.ClosePipe }},
		{"reader killed", func(s ProgramStatusSession) func(*testing.T) { return s.KillReader }},
	} {
		s := f.Armed(t, runtime.ProgramStatusOn, "report state=working")
		hook := death.hook(s)
		if hook == nil {
			continue
		}
		ran = true
		hook(t)
		if _, err := readWant(s, death.label, runtime.ProgramStatusReaderLost); err != nil {
			return err
		}
	}
	if !ran {
		return skip("no reader-death hook")
	}
	return nil
}

// PS-007: pane death reads exited, even under a working record, and keeps
// the sticky LastError.
func checkPS007(t *testing.T, f ProgramStatusFixtures) error {
	if f.Armed == nil {
		return skip("no armed-session fixture")
	}
	s := f.Armed(t, runtime.ProgramStatusOn, "report state=error", "report state=working")
	if _, err := readValid(s, "before exit", runtime.ProgramWorking); err != nil {
		return err
	}
	s.Emit(t, "exit 1")
	r, err := readWant(s, "pane dead", runtime.ProgramStatusExited)
	if err != nil {
		return err
	}
	if !r.LastError {
		return fmt.Errorf("pane dead: %+v, want the sticky LastError kept", r)
	}
	return nil
}

// PS-008: capture values inherited from an outer scope never read valid.
func checkPS008(t *testing.T, f ProgramStatusFixtures) error {
	if f.Inherited == nil {
		return skip("no inherited-values fixture")
	}
	s := f.Inherited(t, runtime.ProgramStatusOn)
	_, err := readWant(s, "inherited values", runtime.ProgramStatusNotArmed, runtime.ProgramStatusSilent)
	return err
}

// PS-009: a hold from a valid working or blocked reading lives until
// ProgramStatusHoldLimit past the later of its Since and the last activity,
// and a reading that is not valid holds nothing.
func checkPS009(t *testing.T, f ProgramStatusFixtures) error {
	if f.Armed == nil {
		return skip("no armed-session fixture")
	}
	before := time.Now()
	s := f.Armed(t, runtime.ProgramStatusOn, "report state=working")
	r, err := readValid(s, "working", runtime.ProgramWorking)
	if err != nil {
		return err
	}
	if r.Since.Before(before.Add(-captureClockSkew)) || r.Since.After(time.Now().Add(captureClockSkew)) {
		return fmt.Errorf("working: Since %v, want the capture time (between %v and now)", r.Since, before)
	}
	if !r.HoldLive(time.Time{}, time.Now()) {
		return fmt.Errorf("working: a fresh reading's hold is not live: %+v", r)
	}
	limit := runtime.ProgramStatusHoldLimit
	later := r.Since.Add(5 * time.Minute)
	for _, c := range []struct {
		activity, now time.Time
		live          bool
	}{
		{time.Time{}, r.Since.Add(limit - time.Millisecond), true},
		{time.Time{}, r.Since.Add(limit), false},
		{r.Since.Add(-time.Hour), r.Since.Add(limit), false},
		{later, r.Since.Add(limit), true},
		{later, later.Add(limit - time.Millisecond), true},
		{later, later.Add(limit), false},
	} {
		if got := r.HoldLive(c.activity, c.now); got != c.live {
			return fmt.Errorf("HoldLive(activity %v, now Since+%v) = %v, want %v", c.activity, c.now.Sub(r.Since), got, c.live)
		}
	}
	silent := f.Armed(t, runtime.ProgramStatusOn)
	q, err := readWant(silent, "silent", runtime.ProgramStatusSilent)
	if err != nil {
		return err
	}
	if q.HoldLive(r.Since, r.Since) {
		return fmt.Errorf("a silent reading holds: %+v", q)
	}
	return nil
}

// PS-010: blocked with each kind reads Blocked, not running and not a
// boundary, its kind exact; an unknown kind reads unspecified.
func checkPS010(t *testing.T, f ProgramStatusFixtures) error {
	if f.Armed == nil {
		return skip("no armed-session fixture")
	}
	s := f.Armed(t, runtime.ProgramStatusOn, "report state=working")
	for _, c := range []struct {
		body string
		kind runtime.ProgramBlockKind
	}{
		{"state=blocked:kind=permission", runtime.ProgramBlockPermission},
		{"state=blocked:kind=question", runtime.ProgramBlockQuestion},
		{"state=blocked:kind=auth", runtime.ProgramBlockAuth},
		{"state=blocked", runtime.ProgramBlockUnspecified},
		{"state=blocked:kind=telepathy", runtime.ProgramBlockUnspecified},
	} {
		s.Emit(t, "report "+c.body)
		r, err := readValid(s, c.body, runtime.ProgramBlocked)
		if err != nil {
			return err
		}
		if !r.Blocked() || r.Running() || r.InputBoundary() || r.Kind != c.kind {
			return fmt.Errorf("%s: %+v, want blocked:%q, neither running nor a boundary", c.body, r, c.kind)
		}
	}
	return nil
}

// PS-011: child records count, a working child keeps the root from
// quiescence, and children never start or end the root's turn.
func checkPS011(t *testing.T, f ProgramStatusFixtures) error {
	if f.Armed == nil {
		return skip("no armed-session fixture")
	}
	s := f.Armed(t, runtime.ProgramStatusOn, "report state=idle")
	s.Emit(t, "report state=working:id=sub1", "report state=idle:id=sub2")
	r, err := readValid(s, "children", runtime.ProgramIdle)
	if err != nil {
		return err
	}
	if r.Children != 2 || r.WorkingChildren != 1 || !r.InputBoundary() || r.Quiescent() || r.TurnStarts != 0 {
		return fmt.Errorf("children: %+v, want 2 children (1 working), an idle root boundary, not quiescent, no root turn", r)
	}
	s.Emit(t, "report state=clear:id=sub1")
	if r, err = readValid(s, "child cleared", runtime.ProgramIdle); err != nil {
		return err
	}
	if r.Children != 1 || r.WorkingChildren != 0 || !r.Quiescent() || r.TurnEnds != 0 {
		return fmt.Errorf("child cleared: %+v, want 1 idle child, quiescent, no root turn", r)
	}
	return nil
}

// PS-012: AdvancedPast credits a turn after a boundary baseline, and nothing
// after a busy or silent baseline.
func checkPS012(t *testing.T, f ProgramStatusFixtures) error {
	if f.Armed == nil {
		return skip("no armed-session fixture")
	}
	s := f.Armed(t, runtime.ProgramStatusOn, "report state=idle")
	idle, err := readValid(s, "idle", runtime.ProgramIdle)
	if err != nil {
		return err
	}
	if idle.AdvancedPast(idle.Baseline()) {
		return errors.New("a reading advanced past its own baseline")
	}
	s.Emit(t, "report state=working")
	working, err := readValid(s, "working", runtime.ProgramWorking)
	if err != nil {
		return err
	}
	if !working.AdvancedPast(idle.Baseline()) {
		return fmt.Errorf("a turn after an idle baseline credited nothing: baseline %+v, reading %+v", idle.Baseline(), working)
	}
	s.Emit(t, "report state=done", "report state=working")
	later, err := readValid(s, "next turn", runtime.ProgramWorking)
	if err != nil {
		return err
	}
	if later.AdvancedPast(working.Baseline()) {
		return fmt.Errorf("a busy baseline credited %+v", later)
	}
	quiet := f.Armed(t, runtime.ProgramStatusOn)
	silent, err := readWant(quiet, "silent", runtime.ProgramStatusSilent)
	if err != nil {
		return err
	}
	quiet.Emit(t, "report state=working")
	first, err := readValid(quiet, "first report", runtime.ProgramWorking)
	if err != nil {
		return err
	}
	if first.AdvancedPast(silent.Baseline()) {
		return fmt.Errorf("a silent baseline credited %+v", first)
	}
	return nil
}

// PS-013: a foreign pipe displaces the reader and reads reader_lost; heal
// defers and never takes the pipe over, and a disarm when the switch goes
// off leaves the pipe open.
func checkPS013(t *testing.T, f ProgramStatusFixtures) error {
	if f.Armed == nil {
		return skip("no armed-session fixture")
	}
	s := f.Armed(t, runtime.ProgramStatusOn, "report state=working")
	if s.ForeignPipe == nil {
		return skip("no foreign-pipe hook")
	}
	s.ForeignPipe(t)
	if _, err := readWant(s, "foreign pipe", runtime.ProgramStatusReaderLost); err != nil {
		return err
	}
	if err := healable(s); err != nil {
		return err
	}
	if err := heal(s, "foreign pipe", captureAll, runtime.ProgramCaptureDeferred); err != nil {
		return err
	}
	if _, err := readWant(s, "foreign pipe after heal", runtime.ProgramStatusReaderLost); err != nil {
		return err
	}
	if s.SetMode == nil || s.PipeOpen == nil {
		return nil
	}
	s.SetMode(t, runtime.ProgramStatusOff)
	if err := heal(s, "foreign pipe, switch off", runtime.ProgramStatusArm{}, runtime.ProgramCaptureDisarmed); err != nil {
		return err
	}
	if !s.PipeOpen(t) {
		return errors.New("foreign pipe, switch off: the disarm closed a pipe the runtime's reader does not hold")
	}
	_, err := readWant(s, "foreign pipe, disarmed", runtime.ProgramStatusUnread)
	return err
}

// PS-014: heal arms a not_armed pane, re-arms one whose pipe closed, leaves
// a healthy capture alone, and disarms when the switch goes off; each
// re-armed pane reads valid at its next report.
func checkPS014(t *testing.T, f ProgramStatusFixtures) error {
	if f.Armed == nil || f.Unarmed == nil {
		return skip("needs armed- and unarmed-session fixtures")
	}
	u := f.Unarmed(t, runtime.ProgramStatusOn)
	if err := healable(u); err != nil {
		return err
	}
	if _, err := readWant(u, "unarmed", runtime.ProgramStatusNotArmed); err != nil {
		return err
	}
	if err := heal(u, "unarmed", captureAll, runtime.ProgramCaptureArmed); err != nil {
		return err
	}
	if _, err := readWant(u, "armed by heal", runtime.ProgramStatusSilent); err != nil {
		return err
	}
	u.Emit(t, "report state=working")
	if _, err := readValid(u, "armed by heal, reported", runtime.ProgramWorking); err != nil {
		return err
	}
	if err := heal(u, "healthy capture", captureAll, runtime.ProgramCaptureUnchanged); err != nil {
		return err
	}
	if missing := heal(ProgramStatusSession{Provider: u.Provider, Name: programStatusMissingSession}, "missing session", captureAll, ""); !errors.Is(missing, runtime.ErrSessionNotFound) {
		return wantSessionNotFound("heal of a missing session", missing)
	}

	s := f.Armed(t, runtime.ProgramStatusOn, "report state=working")
	if s.ClosePipe != nil {
		s.ClosePipe(t)
		if _, err := readWant(s, "pipe closed", runtime.ProgramStatusReaderLost); err != nil {
			return err
		}
		if err := heal(s, "pipe closed", captureAll, runtime.ProgramCaptureArmed); err != nil {
			return err
		}
		s.Emit(t, "report state=idle")
		if _, err := readValid(s, "re-armed, reported", runtime.ProgramIdle); err != nil {
			return err
		}
	}
	if s.SetMode == nil {
		return nil
	}
	s.SetMode(t, runtime.ProgramStatusOff)
	if err := heal(s, "switch off", runtime.ProgramStatusArm{}, runtime.ProgramCaptureDisarmed); err != nil {
		return err
	}
	if _, err := readWant(s, "disarmed, switch off", runtime.ProgramStatusUnread); err != nil {
		return err
	}
	s.SetMode(t, runtime.ProgramStatusOn)
	_, err := readWant(s, "disarmed, switch back on", runtime.ProgramStatusNotArmed)
	return err
}
