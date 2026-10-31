package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// healFake is a FullFake running each named session with an agent-pane row.
func healFake(t *testing.T, rows map[string]runtime.ProgramPaneRow) (*runtime.Fake, runtime.FullFake) {
	t.Helper()
	f := runtime.NewFake()
	for name, row := range rows {
		if err := f.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		f.SetProgramPane(name, row)
	}
	return f, runtime.FullFake{Fake: f}
}

func healedNames(f *runtime.Fake) []string {
	var names []string
	for _, c := range f.Calls {
		if c.Method == "HealProgramCapture" {
			names = append(names, c.Name)
		}
	}
	slices.Sort(names)
	return names
}

func paneRow(id, tap string, pipe bool) runtime.ProgramPaneRow {
	return runtime.ProgramPaneRow{Mode: runtime.ProgramStatusOn, CaptureGate: true, PaneID: id, Tap: tap, PanePipe: pipe, TapLiveness: runtime.ProgramTapAlive}
}

func TestInventoryLaneHealsOnlyReadingsThatCallForIt(t *testing.T) {
	declared := runtime.ProgramStatusArm{Capture: true, App: "agent"}
	f, ff := healFake(t, map[string]runtime.ProgramPaneRow{
		"lost":     paneRow("%1", "r1.%1", false),
		"unarmed":  paneRow("%2", "", false),
		"stale":    paneRow("%3", "r3.%3", true),
		"plain":    paneRow("%4", "", false),
		"healthy":  paneRow("%5", "r5.%5", true),
		"dead":     paneRow("%6", "r6.%6", false),
		"stranger": paneRow("%7", "", false),
	})
	programs := map[string]runtime.ProgramStatus{
		"lost":     {Validity: runtime.ProgramStatusReaderLost},
		"unarmed":  {Validity: runtime.ProgramStatusNotArmed},
		"stale":    {Validity: runtime.ProgramStatusValid},
		"plain":    {Validity: runtime.ProgramStatusNotArmed},
		"healthy":  {Validity: runtime.ProgramStatusSilent},
		"dead":     {Validity: runtime.ProgramStatusExited},
		"stranger": {Validity: runtime.ProgramStatusNotArmed},
		"unread":   {},
	}
	wants := map[string]runtime.ProgramStatusArm{
		"lost": declared, "unarmed": declared, "stale": {}, "plain": {}, "healthy": declared, "dead": declared, "unread": declared,
	}
	f.Calls = nil
	tally := healProgramCaptureReadings(ff, programs, func() (map[string]runtime.ProgramStatusArm, error) { return wants, nil })

	if got, want := healedNames(f), []string{"lost", "stale", "unarmed"}; !slices.Equal(got, want) {
		t.Fatalf("healed %q, want %q", got, want)
	}
	if tally.actions[runtime.ProgramCaptureArmed] != 2 || tally.actions[runtime.ProgramCaptureDisarmed] != 1 || len(tally.errs) != 0 {
		t.Fatalf("tally %+v, want 2 armed and 1 disarmed", tally)
	}
	if !tally.acted() || tally.String() != "armed=2,disarmed=1,swept=0" {
		t.Fatalf("tally renders %q (acted %v)", tally.String(), tally.acted())
	}
	if row, _ := f.ProgramPane("stale"); row.Tap != "" {
		t.Fatalf("the undeclared session's capture was not disarmed: %+v", row)
	}
}

func TestInventoryLaneHealSkipsWhenDeclarationsUnreadable(t *testing.T) {
	f, ff := healFake(t, map[string]runtime.ProgramPaneRow{"lost": paneRow("%1", "r1.%1", false)})
	f.Calls = nil
	tally := healProgramCaptureReadings(ff, map[string]runtime.ProgramStatus{"lost": {Validity: runtime.ProgramStatusReaderLost}},
		func() (map[string]runtime.ProgramStatusArm, error) { return nil, errors.New("store down") })
	if len(healedNames(f)) != 0 || len(tally.errs) != 1 || !strings.Contains(tally.errs[0], "store down") {
		t.Fatalf("healed %q, tally %+v: want nothing healed and the error recorded", healedNames(f), tally)
	}

	// With nothing to heal, declarations are not read at all.
	read := false
	healProgramCaptureReadings(ff, map[string]runtime.ProgramStatus{"x": {}, "y": {Validity: runtime.ProgramStatusExited}},
		func() (map[string]runtime.ProgramStatusArm, error) { read = true; return nil, nil })
	if read {
		t.Fatal("declarations were read for Unread and exited readings only")
	}
}

func TestProgramCaptureHealNeeded(t *testing.T) {
	on, off := runtime.ProgramStatusArm{Capture: true}, runtime.ProgramStatusArm{}
	for _, tc := range []struct {
		v    runtime.ProgramStatusValidity
		want runtime.ProgramStatusArm
		heal bool
	}{
		{runtime.ProgramStatusUnread, on, false},
		{runtime.ProgramStatusExited, on, false},
		{runtime.ProgramStatusExited, off, false},
		{runtime.ProgramStatusNotArmed, on, true},
		{runtime.ProgramStatusNotArmed, off, false},
		{runtime.ProgramStatusReaderLost, on, true},
		{runtime.ProgramStatusReaderLost, off, true},
		{runtime.ProgramStatusValid, on, false},
		{runtime.ProgramStatusValid, off, true},
		{runtime.ProgramStatusSilent, on, false},
		{runtime.ProgramStatusSilent, off, true},
		{runtime.ProgramStatusCleared, on, false},
		{runtime.ProgramStatusCleared, off, true},
	} {
		if got := programCaptureHealNeeded(runtime.ProgramStatus{Validity: tc.v}, tc.want); got != tc.heal {
			t.Errorf("programCaptureHealNeeded(%q, capture=%v) = %v, want %v", tc.v, tc.want.Capture, got, tc.heal)
		}
	}
}

// sweepLeaf is a leaf backend with a disarm sweep, counting its runs and
// failing with *err.
type sweepLeaf struct {
	runtime.Provider
	runs *int
	err  *error
}

func (l sweepLeaf) DisarmProgramCaptures() (int, error) {
	*l.runs++
	return 1, *l.err
}

func TestInventoryLaneDisarmSweepsOncePerConfigLoadWhenSwitchOff(t *testing.T) {
	runs := 0
	var sweepErr error
	sp := sweepLeaf{Provider: runtime.NewFake(), runs: &runs, err: &sweepErr}
	var stderr strings.Builder
	lane := &runtimeInventoryLane{stderr: &stderr, logPrefix: "gc"}
	cr := &CityRuntime{}
	off := &config.City{}
	pass := func(cfg *config.City) inventoryPassReport {
		var report inventoryPassReport
		cr.healProgramCaptures(lane, cfg, sp, &report)
		return report
	}

	if r := pass(off); runs != 1 || r.captureHeals.swept != 1 || !r.captureHeals.acted() {
		t.Fatalf("first pass under off: %d sweeps, tally %+v; want one sweep", runs, r.captureHeals)
	}
	pass(off)
	pass(off)
	if runs != 1 {
		t.Fatalf("%d sweeps over one config load, want 1", runs)
	}
	reloaded := &config.City{}
	pass(reloaded)
	if runs != 2 {
		t.Fatalf("%d sweeps after a config reload, want 2", runs)
	}
	on := &config.City{Session: config.SessionConfig{ProgramStatus: "on"}}
	if r := pass(on); runs != 2 || r.captureHeals.acted() {
		t.Fatalf("a pass under on swept: %d sweeps, tally %+v", runs, r.captureHeals)
	}

	sweepErr = errors.New("tmux: boom")
	if r := pass(&config.City{}); runs != 3 || len(r.captureHeals.errs) != 1 || !strings.Contains(r.captureHeals.String(), "tmux: boom") {
		t.Fatalf("a failed sweep: %d sweeps, tally %q", runs, r.captureHeals.String())
	}
	if !strings.Contains(stderr.String(), "program-status capture heal") || !strings.Contains(stderr.String(), "tmux: boom") {
		t.Fatalf("a failed sweep was not logged: %q", stderr.String())
	}
}

func TestProgramCaptureWantFollowsProviderDeclaration(t *testing.T) {
	cfg := &config.City{
		Session: config.SessionConfig{ProgramStatus: "shadow"},
		Providers: map[string]config.ProviderSpec{
			"emitter": {Command: "sh", PromptMode: "none", ProgramStatus: programStatusTestBinding()},
			"plain":   {Command: "sh", PromptMode: "none"},
		},
		Agents: []config.Agent{{Name: "runner", Provider: "emitter"}, {Name: "other", Provider: "plain"}},
	}
	wants := programCaptureWantsFrom(cfg, []session.Info{
		{SessionName: "s-runner", Template: "runner"},
		{SessionName: "s-other", Template: "other"},
		{SessionName: "s-acp", Template: "runner", Transport: config.SessionTransportACP},
		{SessionName: "", Template: "runner"},
	})
	if got := wants["s-runner"]; got != programStatusTestArm {
		t.Errorf("declared provider's want = %+v, want %+v", got, programStatusTestArm)
	}
	if got := wants["s-other"]; got.Capture {
		t.Errorf("undeclared provider's want = %+v, want no capture", got)
	}
	if got := wants["s-acp"]; got.Capture {
		t.Errorf("ACP transport's want = %+v, want no capture", got)
	}
	if len(wants) != 3 {
		t.Errorf("wants %v, want one per named session", wants)
	}
	cfg.Session.ProgramStatus = "off"
	if got := programCaptureWantsFrom(cfg, []session.Info{{SessionName: "s-runner", Template: "runner"}})["s-runner"]; got.Capture {
		t.Errorf("want under the switch off = %+v, want no capture", got)
	}
}

// floorLeaf is a leaf backend whose capture floor answers err.
type floorLeaf struct {
	runtime.FullFake
	err error
}

func (l floorLeaf) ProgramCaptureFloor() error { return l.err }

func TestProgramCaptureDoctorReadsDeclarationsAndReadings(t *testing.T) {
	cfg := &config.City{
		Session: config.SessionConfig{ProgramStatus: "on"},
		Providers: map[string]config.ProviderSpec{
			"emitter": {Command: "sh", PromptMode: "none", ProgramStatus: programStatusTestBinding()},
		},
		Agents: []config.Agent{{Name: "runner", Provider: "emitter"}},
	}
	silentRow := paneRow("%1", "r1.%1", true)
	silentRow.RecordForm, silentRow.Record = runtime.ProgramRecordDecoded, runtime.ProgramStatus{Nonce: "r1.%1", Probed: true}
	_, ff := healFake(t, map[string]runtime.ProgramPaneRow{"s-runner": silentRow, "s-unknown": paneRow("%2", "", false)})
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Title: "runner", Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
		"template": "runner", "session_name": "s-runner", "state": "active",
	}}); err != nil {
		t.Fatal(err)
	}
	loadSessions := func() (*sessionBeadSnapshot, error) { return loadSessionBeadSnapshot(store) }
	floor := errors.New("tmux \"3.1\" is older than 3.2")

	st, err := readProgramCaptureState(cfg, loadSessions, floorLeaf{FullFake: ff, err: floor}, nil)
	if err != nil {
		t.Fatalf("readProgramCaptureState: %v", err)
	}
	if st.Mode != runtime.ProgramStatusOn || !st.FloorChecked || !errors.Is(st.Floor, floor) {
		t.Fatalf("state %+v, want mode on and the floor's verdict", st)
	}
	if len(st.Sessions) != 1 || st.Sessions[0].Name != "s-runner" || !st.Sessions[0].Declared ||
		st.Sessions[0].Reading.Validity != runtime.ProgramStatusSilent || !st.Sessions[0].Reading.Probed {
		t.Fatalf("sessions %+v, want s-runner declared, silent and probed", st.Sessions)
	}
	r := newProgramCaptureDoctorCheck(cfg, loadSessions, floorLeaf{FullFake: ff, err: floor}, nil).Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusWarning || !strings.Contains(strings.Join(r.Details, "\n"), "probed but unanswered") {
		t.Fatalf("check result %+v, want the probed-but-unanswered finding", r)
	}

	cfg.Session.ProgramStatus = "off"
	if st, err := readProgramCaptureState(cfg, loadSessions, nil, errors.New("unused")); err != nil || st.Mode != runtime.ProgramStatusOff || len(st.Sessions) != 0 {
		t.Fatalf("switch off: %+v, %v; want off with nothing read", st, err)
	}
	cfg.Session.ProgramStatus = "on"
	if _, err := readProgramCaptureState(cfg, loadSessions, nil, errors.New("no provider")); err == nil || !strings.Contains(err.Error(), "no provider") {
		t.Fatalf("no provider: %v, want its error", err)
	}
}
