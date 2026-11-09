package runtime_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// armedRow is an agent-pane row armed with nonce "r1.%3" on pane %3, its tap
// alive and a working root record published under that nonce, read under on.
func armedRow() runtime.ProgramPaneRow {
	return runtime.ProgramPaneRow{
		Mode:        runtime.ProgramStatusOn,
		CaptureGate: true,
		PaneID:      "%3",
		PanePipe:    true,
		Tap:         "r1.%3",
		TapLiveness: runtime.ProgramTapAlive,
		RecordForm:  runtime.ProgramRecordDecoded,
		Record: runtime.ProgramStatus{
			State:      runtime.ProgramWorking,
			App:        "codex",
			Progress:   -1,
			Nonce:      "r1.%3",
			Seq:        4,
			TurnStarts: 1,
			Probed:     true,
			LastError:  true,
			Reported:   true,
		},
	}
}

// valid is a valid reading in state st under nonce "r1.%3".
func valid(st runtime.ProgramState, seq uint64, starts uint32) runtime.ProgramStatus {
	return runtime.ProgramStatus{
		Mode:       runtime.ProgramStatusOn,
		Validity:   runtime.ProgramStatusValid,
		State:      st,
		Nonce:      "r1.%3",
		Seq:        seq,
		TurnStarts: starts,
	}
}

// Kills a zero reading that carries state or may be used: the zero value is
// Unread, reads as off, and answers no to every predicate.
func TestProgramStatusZeroValueIsUnread(t *testing.T) {
	var s runtime.ProgramStatus
	if s.Validity != runtime.ProgramStatusUnread {
		t.Fatalf("zero Validity = %q, want Unread", s.Validity)
	}
	if s.Valid() || s.Decides() || s.Running() || s.Blocked() || s.InputBoundary() || s.Quiescent() {
		t.Fatalf("zero reading answers yes to a predicate: %+v", s)
	}
	if s.HoldLive(time.Now(), time.Now()) {
		t.Fatal("zero reading holds")
	}
	if b := s.Baseline(); b != (runtime.ProgramBaseline{}) {
		t.Fatalf("zero baseline = %+v, want zero", b)
	}
	// A record with state but no validity is still Unread: state without a
	// judgment carries nothing.
	s.State = runtime.ProgramWorking
	s.Mode = runtime.ProgramStatusOn
	if s.Valid() || s.Running() || s.Decides() {
		t.Fatalf("unjudged record reads as valid: %+v", s)
	}
	if got := runtime.JudgeProgramStatus(runtime.ProgramPaneRow{}); got.Validity != runtime.ProgramStatusUnread || got.State != "" {
		t.Fatalf("zero row judged %+v, want Unread with no state", got)
	}
}

// Kills a nonce judged without naming the pane it was read on: a tap value
// set for another pane, at window, session or global scope and inherited
// here, or without a rand part, is not armed for this pane.
func TestNonceMustNameReadPane(t *testing.T) {
	cases := []struct {
		name string
		pane string
		tap  string
		want runtime.ProgramStatusValidity
	}{
		{"own pane", "%3", "r1.%3", runtime.ProgramStatusValid},
		{"another pane's nonce inherited here", "%4", "r1.%3", runtime.ProgramStatusNotArmed},
		{"pane id that only shares a prefix", "%3", "r1.%33", runtime.ProgramStatusNotArmed},
		{"pane id that only shares a suffix", "%33", "r1.%3", runtime.ProgramStatusNotArmed},
		{"no separator", "%3", "r1%3", runtime.ProgramStatusNotArmed},
		{"no rand", "%3", ".%3", runtime.ProgramStatusNotArmed},
		{"bare pane id", "%3", "%3", runtime.ProgramStatusNotArmed},
		{"no tap", "%3", "", runtime.ProgramStatusNotArmed},
		{"no pane id read", "", "r1.", runtime.ProgramStatusNotArmed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := armedRow()
			row.PaneID, row.Tap = tc.pane, tc.tap
			row.Record.Nonce = tc.tap
			got := runtime.JudgeProgramStatus(row)
			if got.Validity != tc.want {
				t.Fatalf("validity = %q, want %q", got.Validity, tc.want)
			}
			if tc.want != runtime.ProgramStatusValid && (got.State != "" || got.Valid()) {
				t.Fatalf("unarmed reading carries state: %+v", got)
			}
		})
	}
	// A record published under another nonce on our own armed pane is absent.
	row := armedRow()
	row.Record.Nonce = "r0.%3"
	if got := runtime.JudgeProgramStatus(row); got.Validity != runtime.ProgramStatusSilent || got.State != "" || got.Probed {
		t.Fatalf("stale-nonce record judged %+v, want silent with nothing carried", got)
	}
}

// Kills a derivation that applies its rules out of order or lets a reading
// it does not judge valid carry state.
func TestJudgeProgramStatusFirstMatchingRuleWins(t *testing.T) {
	cases := []struct {
		name     string
		mut      func(*runtime.ProgramPaneRow)
		want     runtime.ProgramStatusValidity
		mode     runtime.ProgramStatusMode
		verified bool
		lastErr  bool
	}{
		{"valid under on", func(*runtime.ProgramPaneRow) {}, runtime.ProgramStatusValid, runtime.ProgramStatusOn, true, true},
		{"valid under shadow", func(r *runtime.ProgramPaneRow) { r.Mode = runtime.ProgramStatusShadow }, runtime.ProgramStatusValid, runtime.ProgramStatusShadow, true, true},
		{"tap unknown is valid, unverified", func(r *runtime.ProgramPaneRow) { r.TapLiveness = runtime.ProgramTapUnknown }, runtime.ProgramStatusValid, runtime.ProgramStatusOn, false, true},
		{"off beats everything", func(r *runtime.ProgramPaneRow) { r.Mode = runtime.ProgramStatusOff; r.PaneDead = true }, runtime.ProgramStatusUnread, runtime.ProgramStatusOff, false, false},
		{"empty mode reads as off", func(r *runtime.ProgramPaneRow) { r.Mode = "" }, runtime.ProgramStatusUnread, runtime.ProgramStatusOff, false, false},
		{"unknown mode reads as off", func(r *runtime.ProgramPaneRow) { r.Mode = "auto" }, runtime.ProgramStatusUnread, runtime.ProgramStatusOff, false, false},
		{"gate failed", func(r *runtime.ProgramPaneRow) { r.CaptureGate = false }, runtime.ProgramStatusUnread, runtime.ProgramStatusOn, false, false},
		{"not armed beats dead", func(r *runtime.ProgramPaneRow) { r.Tap = ""; r.PaneDead = true }, runtime.ProgramStatusNotArmed, runtime.ProgramStatusOn, false, false},
		{"pane dead keeps last error", func(r *runtime.ProgramPaneRow) { r.PaneDead = true; r.PanePipe = false }, runtime.ProgramStatusExited, runtime.ProgramStatusOn, false, true},
		{"agent gone", func(r *runtime.ProgramPaneRow) { r.AgentGone = true }, runtime.ProgramStatusExited, runtime.ProgramStatusOn, false, true},
		{"dead under another nonce drops last error", func(r *runtime.ProgramPaneRow) { r.PaneDead = true; r.Record.Nonce = "r0.%3" }, runtime.ProgramStatusExited, runtime.ProgramStatusOn, false, false},
		{"pipe closed", func(r *runtime.ProgramPaneRow) { r.PanePipe = false }, runtime.ProgramStatusReaderLost, runtime.ProgramStatusOn, false, false},
		{"tap gone", func(r *runtime.ProgramPaneRow) { r.TapLiveness = runtime.ProgramTapGone }, runtime.ProgramStatusReaderLost, runtime.ProgramStatusOn, false, false},
		{"unknown codec under a live tap", func(r *runtime.ProgramPaneRow) { r.RecordForm = runtime.ProgramRecordUnknownVersion }, runtime.ProgramStatusReaderLost, runtime.ProgramStatusOn, false, false},
		{"unknown codec, tap unverified", func(r *runtime.ProgramPaneRow) {
			r.RecordForm, r.TapLiveness = runtime.ProgramRecordUnknownVersion, runtime.ProgramTapUnknown
		}, runtime.ProgramStatusSilent, runtime.ProgramStatusOn, false, false},
		{"undecodable", func(r *runtime.ProgramPaneRow) { r.RecordForm = runtime.ProgramRecordUndecodable }, runtime.ProgramStatusSilent, runtime.ProgramStatusOn, false, false},
		{"absent", func(r *runtime.ProgramPaneRow) { r.RecordForm = runtime.ProgramRecordAbsent }, runtime.ProgramStatusSilent, runtime.ProgramStatusOn, false, false},
		{"unknown record form", func(r *runtime.ProgramPaneRow) { r.RecordForm = "v9" }, runtime.ProgramStatusSilent, runtime.ProgramStatusOn, false, false},
		{"unknown root word", func(r *runtime.ProgramPaneRow) { r.Record.State = "sleeping" }, runtime.ProgramStatusSilent, runtime.ProgramStatusOn, false, false},
		{"no root, never reported", func(r *runtime.ProgramPaneRow) { r.Record.State, r.Record.Seq, r.Record.Reported = "", 0, false }, runtime.ProgramStatusSilent, runtime.ProgramStatusOn, false, true},
		{"probed only, no root ever reported", func(r *runtime.ProgramPaneRow) { r.Record.State, r.Record.Seq, r.Record.Reported = "", 1, false }, runtime.ProgramStatusSilent, runtime.ProgramStatusOn, false, true},
		{"child reports only, no root ever reported", func(r *runtime.ProgramPaneRow) {
			r.Record.State, r.Record.Reported, r.Record.Children, r.Record.WorkingChildren = "", false, 1, 1
		}, runtime.ProgramStatusSilent, runtime.ProgramStatusOn, false, true},
		{"no root after reports", func(r *runtime.ProgramPaneRow) { r.Record.State = "" }, runtime.ProgramStatusCleared, runtime.ProgramStatusOn, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := armedRow()
			tc.mut(&row)
			got := runtime.JudgeProgramStatus(row)
			if got.Validity != tc.want || got.Mode != tc.mode {
				t.Fatalf("judged %q under %q, want %q under %q", got.Validity, got.Mode, tc.want, tc.mode)
			}
			if got.ReaderVerified != tc.verified {
				t.Errorf("ReaderVerified = %v, want %v", got.ReaderVerified, tc.verified)
			}
			if got.LastError != tc.lastErr {
				t.Errorf("LastError = %v, want %v", got.LastError, tc.lastErr)
			}
			if tc.want == runtime.ProgramStatusValid {
				if got.State != runtime.ProgramWorking || got.App != "codex" || got.Seq != 4 || got.Nonce != "r1.%3" {
					t.Errorf("valid reading lost its record: %+v", got)
				}
				return
			}
			// Past its mode, validity, nonce and annotations, a reading that
			// is not valid is zero: it carries none of the record's claims.
			claims := got
			claims.Mode, claims.Validity, claims.Nonce = "", "", ""
			claims.Probed, claims.LastError, claims.Reported = false, false, false
			if claims != (runtime.ProgramStatus{}) {
				t.Errorf("%q reading carries the record's claims: %+v", got.Validity, got)
			}
		})
	}
}

// Kills a rule 6 that tells silent from cleared by Seq, which a probe or a
// child report bumps, and a reading with no root that carries the record's
// claims: it is silent until a root was reported this incarnation, then
// cleared, and keeps only Probed (doctor), Reported and LastError.
func TestJudgeProgramStatusNoRootCarriesNoClaims(t *testing.T) {
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		rec  runtime.ProgramStatus
		want runtime.ProgramStatusValidity
	}{
		{"nothing reported", runtime.ProgramStatus{}, runtime.ProgramStatusSilent},
		{"probed only", runtime.ProgramStatus{Seq: 1, Probed: true}, runtime.ProgramStatusSilent},
		{"child reports only", runtime.ProgramStatus{
			Seq: 3, App: "codex", Progress: -1, Children: 2, WorkingChildren: 1, Probed: true,
		}, runtime.ProgramStatusSilent},
		{"root cleared after reports", runtime.ProgramStatus{
			Seq: 9, App: "codex", Progress: 40, Since: t0, QuietSince: t0, TurnStarts: 2, TurnEnds: 2,
			Children: 2, WorkingChildren: 1, Probed: true, LastError: true, Reported: true,
		}, runtime.ProgramStatusCleared},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := armedRow()
			row.Record = tc.rec
			row.Record.Nonce = row.Tap
			want := runtime.ProgramStatus{
				Mode:      runtime.ProgramStatusOn,
				Validity:  tc.want,
				Nonce:     row.Tap,
				Probed:    tc.rec.Probed,
				LastError: tc.rec.LastError,
				Reported:  tc.rec.Reported,
			}
			if got := runtime.JudgeProgramStatus(row); got != want {
				t.Fatalf("judged %+v, want %+v", got, want)
			}
		})
	}
}

// Kills a judged record that keeps a block kind its state does not take, or
// one spec rev 0.4 does not name.
func TestJudgeProgramStatusKeepsKindOnlyWithBlocked(t *testing.T) {
	row := armedRow()
	row.Record.State, row.Record.Kind = runtime.ProgramBlocked, runtime.ProgramBlockAuth
	if got := runtime.JudgeProgramStatus(row); !got.Blocked() || got.Kind != runtime.ProgramBlockAuth {
		t.Fatalf("blocked reading %+v, want blocked:auth", got)
	}
	row.Record.Kind = "dialog"
	if got := runtime.JudgeProgramStatus(row); !got.Blocked() || got.Kind != runtime.ProgramBlockUnspecified {
		t.Fatalf("blocked reading of an unknown kind %+v, want blocked, kind unspecified", got)
	}
	row.Record.State, row.Record.Kind = runtime.ProgramIdle, runtime.ProgramBlockAuth
	if got := runtime.JudgeProgramStatus(row); got.Kind != runtime.ProgramBlockUnspecified {
		t.Fatalf("idle reading kept kind %q", got.Kind)
	}
}

// Kills predicates that answer from an invalid reading, read a shadow reading
// as deciding, or count blocked as a boundary or children as root state.
func TestProgramStatusPredicates(t *testing.T) {
	type want struct{ valid, decides, running, blocked, boundary, quiescent bool }
	cases := []struct {
		name string
		s    runtime.ProgramStatus
		want want
	}{
		{"working", valid(runtime.ProgramWorking, 1, 1), want{true, true, true, false, false, false}},
		{"blocked", valid(runtime.ProgramBlocked, 1, 1), want{true, true, false, true, false, false}},
		{"idle", valid(runtime.ProgramIdle, 1, 1), want{true, true, false, false, true, true}},
		{"done", valid(runtime.ProgramDone, 1, 1), want{true, true, false, false, true, true}},
		{"error", valid(runtime.ProgramError, 1, 1), want{true, true, false, false, true, true}},
		{"idle with a working child", func() runtime.ProgramStatus {
			s := valid(runtime.ProgramIdle, 1, 1)
			s.Children, s.WorkingChildren = 2, 1
			return s
		}(), want{true, true, false, false, true, false}},
		{"shadow is advisory", func() runtime.ProgramStatus {
			s := valid(runtime.ProgramWorking, 1, 1)
			s.Mode = runtime.ProgramStatusShadow
			return s
		}(), want{true, false, true, false, false, false}},
		{"valid without state", valid("", 1, 1), want{}},
		{"silent with state", func() runtime.ProgramStatus {
			s := valid(runtime.ProgramIdle, 1, 1)
			s.Validity = runtime.ProgramStatusSilent
			return s
		}(), want{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s
			got := want{s.Valid(), s.Decides(), s.Running(), s.Blocked(), s.InputBoundary(), s.Quiescent()}
			if got != tc.want {
				t.Fatalf("predicates = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Kills delivery evidence credited across incarnations, from a baseline
// taken while the program was busy or silent, or from a reading that only
// re-published the baseline's boundary.
func TestAdvancedPastIgnoresOtherNonceAndBusyBaseline(t *testing.T) {
	idle := valid(runtime.ProgramIdle, 5, 2).Baseline()
	cases := []struct {
		name string
		b    runtime.ProgramBaseline
		s    runtime.ProgramStatus
		want bool
	}{
		{"new turn started", idle, valid(runtime.ProgramIdle, 9, 3), true},
		{"working past the boundary", idle, valid(runtime.ProgramWorking, 6, 2), true},
		{"blocked past the boundary", idle, valid(runtime.ProgramBlocked, 6, 2), true},
		{"done baseline", valid(runtime.ProgramDone, 5, 2).Baseline(), valid(runtime.ProgramWorking, 6, 3), true},
		{"error baseline", valid(runtime.ProgramError, 5, 2).Baseline(), valid(runtime.ProgramWorking, 6, 3), true},
		{"unchanged", idle, valid(runtime.ProgramIdle, 5, 2), false},
		{"re-published boundary", idle, valid(runtime.ProgramDone, 6, 2), false},
		{"working, seq not past", idle, valid(runtime.ProgramWorking, 5, 2), false},
		{"other nonce", idle, func() runtime.ProgramStatus {
			s := valid(runtime.ProgramWorking, 9, 9)
			s.Nonce = "r2.%3"
			return s
		}(), false},
		{"busy baseline", valid(runtime.ProgramWorking, 5, 2).Baseline(), valid(runtime.ProgramWorking, 9, 3), false},
		{"blocked baseline", valid(runtime.ProgramBlocked, 5, 2).Baseline(), valid(runtime.ProgramWorking, 9, 3), false},
		{"silent baseline", runtime.ProgramStatus{Mode: runtime.ProgramStatusOn, Validity: runtime.ProgramStatusSilent, Nonce: "r1.%3"}.Baseline(), valid(runtime.ProgramWorking, 1, 1), false},
		{"zero baseline", runtime.ProgramBaseline{}, valid(runtime.ProgramWorking, 1, 1), false},
		{"invalid reading", idle, func() runtime.ProgramStatus {
			s := valid(runtime.ProgramWorking, 9, 3)
			s.Validity = runtime.ProgramStatusReaderLost
			return s
		}(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.AdvancedPast(tc.b); got != tc.want {
				t.Fatalf("AdvancedPast(%+v) on %+v = %v, want %v", tc.b, tc.s, got, tc.want)
			}
		})
	}
}

// Kills a baseline that records state an invalid reading does not carry.
func TestBaselineRecordsStateOnlyFromAValidReading(t *testing.T) {
	s := valid(runtime.ProgramIdle, 5, 2)
	if got, want := s.Baseline(), (runtime.ProgramBaseline{Nonce: "r1.%3", Seq: 5, TurnStarts: 2, State: runtime.ProgramIdle}); got != want {
		t.Fatalf("baseline = %+v, want %+v", got, want)
	}
	s.Validity = runtime.ProgramStatusExited
	if got := s.Baseline(); got.State != "" {
		t.Fatalf("exited baseline state = %q, want none", got.State)
	}
}

// Kills a hold bound measured from the earlier of the record's Since and the
// pane's last output, one that counts an invalid reading, or one that a Since
// or activity later than now keeps live past the bound.
func TestHoldLiveUsesLaterOfSinceAndActivity(t *testing.T) {
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	limit := runtime.ProgramStatusHoldLimit
	if limit != 10*time.Minute {
		t.Fatalf("hold limit = %v, want 10m", limit)
	}
	s := valid(runtime.ProgramWorking, 1, 1)
	s.Since = t0
	cases := []struct {
		name     string
		activity time.Time
		now      time.Time
		want     bool
	}{
		{"fresh record", time.Time{}, t0.Add(time.Minute), true},
		{"just inside the bound", time.Time{}, t0.Add(limit - time.Nanosecond), true},
		{"at the bound", time.Time{}, t0.Add(limit), false},
		{"past the bound, older activity", t0.Add(-time.Hour), t0.Add(limit + time.Second), false},
		{"recent activity extends it", t0.Add(5 * time.Minute), t0.Add(limit + time.Minute), true},
		{"activity bound too", t0.Add(5 * time.Minute), t0.Add(5*time.Minute + limit), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.HoldLive(tc.activity, tc.now); got != tc.want {
				t.Fatalf("HoldLive = %v, want %v", got, tc.want)
			}
		})
	}
	noSince := valid(runtime.ProgramWorking, 1, 1)
	if !noSince.HoldLive(t0, t0.Add(time.Minute)) {
		t.Fatal("a record with no Since did not hold on recent activity")
	}
	if noSince.HoldLive(time.Time{}, t0) {
		t.Fatal("a record with no Since and no activity held")
	}
	s.Validity = runtime.ProgramStatusReaderLost
	if s.HoldLive(time.Time{}, t0.Add(time.Minute)) {
		t.Fatal("an invalid reading held")
	}
	// A Since or activity later than now is no evidence: a forged since_ms,
	// a skewed capture host or a clock stepped back holds nothing.
	for _, tc := range []struct {
		name            string
		since, activity time.Time
	}{
		{"future Since", t0.Add(time.Hour), time.Time{}},
		{"future Since beside recent activity", t0.Add(time.Hour), t0},
		{"future activity", t0, t0.Add(time.Hour)},
		{"Since at the end of time", time.UnixMilli(math.MaxInt64), time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := valid(runtime.ProgramWorking, 1, 1)
			f.Since = tc.since
			for _, now := range []time.Time{t0.Add(time.Minute), t0.AddDate(1, 0, 0)} {
				if f.HoldLive(tc.activity, now) {
					t.Fatalf("HoldLive(activity %v, now %v) with Since %v held", tc.activity, now, tc.since)
				}
			}
		})
	}
}

// Kills a hold that ages into idle: past the bound a working, blocked or
// child-working record still reads as no boundary and no quiescence.
func TestHoldBoundNeverAgesIntoIdle(t *testing.T) {
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	late := t0.Add(24 * time.Hour)
	for _, s := range []runtime.ProgramStatus{
		valid(runtime.ProgramWorking, 1, 1),
		valid(runtime.ProgramBlocked, 1, 1),
		func() runtime.ProgramStatus {
			s := valid(runtime.ProgramIdle, 1, 1)
			s.WorkingChildren = 1
			return s
		}(),
	} {
		s.Since = t0
		if s.HoldLive(t0, late) {
			t.Fatalf("%s: hold still live a day later", s.State)
		}
		if s.Quiescent() || (s.State != runtime.ProgramIdle && s.InputBoundary()) {
			t.Fatalf("%s: an expired hold read as idle: %+v", s.State, s)
		}
	}
}

// programLeaf is a leaf that reads program status and heals capture.
type programLeaf struct {
	runtime.Provider
	read runtime.ProgramStatus
	err  error
	heal []runtime.ProgramStatusArm
}

func (l *programLeaf) ProgramStatus(string) (runtime.ProgramStatus, error) { return l.read, l.err }

func (l *programLeaf) HealProgramCapture(_ string, want runtime.ProgramStatusArm) (runtime.ProgramCaptureAction, error) {
	l.heal = append(l.heal, want)
	return runtime.ProgramCaptureArmed, nil
}

var (
	_ runtime.ProgramStatusProvider = (*programLeaf)(nil)
	_ runtime.ProgramCaptureHealer  = (*programLeaf)(nil)
)

// Kills a program-status read or a heal taken off the composite instead of
// the session's routed leaf, and a composite that grows either interface.
func TestProgramStatusForResolvesTheRoutedLeaf(t *testing.T) {
	boom := errors.New("boom")
	leaf := &programLeaf{Provider: runtime.NewFake(), read: valid(runtime.ProgramIdle, 1, 1), err: boom}
	other := runtime.NewFake()
	composite := hybrid.New(leaf, other, func(name string) bool { return name == "remote" })
	for name, c := range map[string]runtime.Provider{"auto": auto.New(leaf, other), "hybrid": composite} {
		if _, ok := c.(runtime.ProgramStatusProvider); ok {
			t.Errorf("%s implements ProgramStatusProvider; it must be read on the routed leaf", name)
		}
		if _, ok := c.(runtime.ProgramCaptureHealer); ok {
			t.Errorf("%s implements ProgramCaptureHealer; heal must reach the routed leaf", name)
		}
	}
	cases := []struct {
		name    string
		p       runtime.Provider
		session string
		want    bool
	}{
		{"leaf", leaf, "s", true},
		{"hybrid routed to the leaf", composite, "local", true},
		{"hybrid routed elsewhere", composite, "remote", false},
		{"backend without the capability", other, "s", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps, ok := runtime.ProgramStatusFor(tc.p, tc.session)
			if ok != tc.want {
				t.Fatalf("ProgramStatusFor ok = %v, want %v", ok, tc.want)
			}
			h, hok := runtime.ProgramCaptureHealerFor(tc.p, tc.session)
			if hok != tc.want {
				t.Fatalf("ProgramCaptureHealerFor ok = %v, want %v", hok, tc.want)
			}
			if !ok {
				return
			}
			if got, err := ps.ProgramStatus(tc.session); !errors.Is(err, boom) || got != leaf.read {
				t.Fatalf("read = %+v, %v; want the leaf's", got, err)
			}
			arm := runtime.ProgramStatusArm{Capture: true, App: "codex"}
			if act, err := h.HealProgramCapture(tc.session, arm); err != nil || act != runtime.ProgramCaptureArmed {
				t.Fatalf("heal = %q, %v; want the leaf's", act, err)
			}
		})
	}
	if len(leaf.heal) != 2 || leaf.heal[0] != (runtime.ProgramStatusArm{Capture: true, App: "codex"}) {
		t.Fatalf("leaf heals = %+v, want two forwarded arms", leaf.heal)
	}
}

// Kills a ReadProgramStatus that drops the routed leaf's reading or error,
// or invents state for a backend without the capability.
func TestReadProgramStatusReadsTheRoutedLeaf(t *testing.T) {
	boom := errors.New("boom")
	leaf := &programLeaf{Provider: runtime.NewFake(), read: valid(runtime.ProgramBlocked, 2, 1), err: boom}
	composite := hybrid.New(leaf, runtime.NewFake(), func(name string) bool { return name == "remote" })
	if got, err := runtime.ReadProgramStatus(composite, "local"); !errors.Is(err, boom) || got != leaf.read {
		t.Fatalf("routed to the leaf: %+v, %v; want the leaf's reading and error", got, err)
	}
	if got, err := runtime.ReadProgramStatus(composite, "remote"); err != nil || got != (runtime.ProgramStatus{}) {
		t.Fatalf("routed elsewhere: %+v, %v; want an Unread reading and no error", got, err)
	}
	if got, err := runtime.ReadProgramStatus(nil, "s"); err != nil || got.Validity != runtime.ProgramStatusUnread {
		t.Fatalf("nil provider: %+v, %v; want Unread", got, err)
	}
}

// Kills an inventory entry whose zero program reading is not Unread.
func TestInventoryEntryProgramDefaultsToUnread(t *testing.T) {
	var e runtime.InventoryEntry
	if e.Program.Validity != runtime.ProgramStatusUnread || e.Program.Valid() {
		t.Fatalf("zero inventory program = %+v, want Unread", e.Program)
	}
}
