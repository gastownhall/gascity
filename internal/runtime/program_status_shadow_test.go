package runtime_test

import (
	"errors"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
)

// shadowNow is the clock the rule tests judge holds at.
var shadowNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// live is a valid reading in st under nonce "r1.%3" that entered st a
// second ago, so every hold it creates is live.
func live(st runtime.ProgramState, seq uint64, starts uint32) runtime.ProgramStatus {
	s := valid(st, seq, starts)
	s.Mode = runtime.ProgramStatusShadow
	s.Since = shadowNow.Add(-time.Second)
	return s
}

// stale is live(st) entered past the hold bound.
func stale(st runtime.ProgramState) runtime.ProgramStatus {
	s := live(st, 3, 1)
	s.Since = shadowNow.Add(-runtime.ProgramStatusHoldLimit - time.Second)
	return s
}

// Kills a token that strays from the payload's closed vocabulary: every
// point and verdict is lower_snake_case, unique, and the point list has the
// design's sixteen.
func TestProgramStatusShadowTokensAreClosed(t *testing.T) {
	token := regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)
	points := runtime.ProgramStatusPoints()
	if len(points) != 16 {
		t.Fatalf("%d decision points, want the design's 16", len(points))
	}
	seen := map[string]bool{}
	for _, p := range points {
		if !token.MatchString(string(p)) || seen[string(p)] {
			t.Errorf("point %q is not a unique lower_snake_case token", p)
		}
		seen[string(p)] = true
	}
	for _, v := range runtime.ProgramVerdicts() {
		if !token.MatchString(string(v)) || seen["v:"+string(v)] {
			t.Errorf("verdict %q is not a unique lower_snake_case token", v)
		}
		seen["v:"+string(v)] = true
	}
}

// Kills a high-frequency point recorded per evaluation: the continuously
// evaluated points, and the four the shadow targets add (pending,
// wait_idle_preread, proved_idle, drain_reminder), record on change only;
// the on-demand points record once per decision.
func TestProgramStatusPointsRecordOnChange(t *testing.T) {
	onChange := []runtime.ProgramStatusPoint{
		runtime.ProgramPointExecutionBackstop, runtime.ProgramPointClaimNudgeHold,
		runtime.ProgramPointWorkLadder, runtime.ProgramPointObservation,
		runtime.ProgramPointQueuedDelivery, runtime.ProgramPointPending,
		runtime.ProgramPointWaitIdlePreread, runtime.ProgramPointProvedIdle,
		runtime.ProgramPointDrainReminder,
	}
	for _, p := range runtime.ProgramStatusPoints() {
		if got, want := p.RecordsOnChange(), slices.Contains(onChange, p); got != want {
			t.Errorf("%s.RecordsOnChange() = %v, want %v", p, got, want)
		}
	}
}

// Kills an idle rule that reads idle from age or from an invalid record, or
// lets a hold outlive its bound.
func TestProgramIdleVerdict(t *testing.T) {
	cases := []struct {
		name string
		s    runtime.ProgramStatus
		want runtime.ProgramVerdict
	}{
		{"unread", runtime.ProgramStatus{}, runtime.ProgramVerdictIndeterminate},
		{"silent", runtime.ProgramStatus{Mode: runtime.ProgramStatusShadow, Validity: runtime.ProgramStatusSilent}, runtime.ProgramVerdictIndeterminate},
		{"working", live(runtime.ProgramWorking, 2, 1), runtime.ProgramVerdictBusy},
		{"blocked", live(runtime.ProgramBlocked, 2, 1), runtime.ProgramVerdictPending},
		{"idle", live(runtime.ProgramIdle, 1, 0), runtime.ProgramVerdictIdle},
		{"done", live(runtime.ProgramDone, 3, 1), runtime.ProgramVerdictIdle},
		{"error", live(runtime.ProgramError, 3, 1), runtime.ProgramVerdictIdle},
		{"working past the bound", stale(runtime.ProgramWorking), runtime.ProgramVerdictIndeterminate},
		{"blocked past the bound", stale(runtime.ProgramBlocked), runtime.ProgramVerdictIndeterminate},
	}
	for _, c := range cases {
		if got := runtime.ProgramIdleVerdict(c.s, time.Time{}, shadowNow); got != c.want {
			t.Errorf("%s: ProgramIdleVerdict = %q, want %q", c.name, got, c.want)
		}
	}
}

// Kills pending and hold rules that grant anything but a live positive
// claim.
func TestProgramPendingAndHoldVerdicts(t *testing.T) {
	children := live(runtime.ProgramIdle, 4, 1)
	children.WorkingChildren = 1
	cases := []struct {
		name                     string
		s                        runtime.ProgramStatus
		pending, hold, drainHold runtime.ProgramVerdict
	}{
		{"unread", runtime.ProgramStatus{}, runtime.ProgramVerdictNone, runtime.ProgramVerdictNoHold, runtime.ProgramVerdictNoHold},
		{"working", live(runtime.ProgramWorking, 2, 1), runtime.ProgramVerdictNone, runtime.ProgramVerdictHold, runtime.ProgramVerdictNoHold},
		{"blocked", live(runtime.ProgramBlocked, 2, 1), runtime.ProgramVerdictPending, runtime.ProgramVerdictHold, runtime.ProgramVerdictHold},
		{"idle", live(runtime.ProgramIdle, 1, 0), runtime.ProgramVerdictNone, runtime.ProgramVerdictNoHold, runtime.ProgramVerdictNoHold},
		{"idle with a working child", children, runtime.ProgramVerdictNone, runtime.ProgramVerdictHold, runtime.ProgramVerdictNoHold},
		{"working past the bound", stale(runtime.ProgramWorking), runtime.ProgramVerdictNone, runtime.ProgramVerdictNoHold, runtime.ProgramVerdictNoHold},
		{"blocked past the bound", stale(runtime.ProgramBlocked), runtime.ProgramVerdictNone, runtime.ProgramVerdictNoHold, runtime.ProgramVerdictNoHold},
	}
	for _, c := range cases {
		if got := runtime.ProgramPendingVerdict(c.s, time.Time{}, shadowNow); got != c.pending {
			t.Errorf("%s: ProgramPendingVerdict = %q, want %q", c.name, got, c.pending)
		}
		if got := runtime.ProgramHoldVerdict(c.s, time.Time{}, shadowNow); got != c.hold {
			t.Errorf("%s: ProgramHoldVerdict = %q, want %q", c.name, got, c.hold)
		}
		if got := runtime.ProgramBlockedHoldVerdict(c.s, time.Time{}, shadowNow); got != c.drainHold {
			t.Errorf("%s: ProgramBlockedHoldVerdict = %q, want %q", c.name, got, c.drainHold)
		}
	}
	// Recent pane output keeps a hold live past the record's own age.
	if got := runtime.ProgramHoldVerdict(stale(runtime.ProgramWorking), shadowNow.Add(-time.Second), shadowNow); got != runtime.ProgramVerdictHold {
		t.Errorf("stale working with recent activity: ProgramHoldVerdict = %q, want hold", got)
	}
}

// Kills positive-evidence rules that credit a busy, other-nonce or invalid
// baseline: confirmation, the startup ladder and the Escape re-send all
// credit only AdvancedPast.
func TestProgramDeliveryVerdicts(t *testing.T) {
	idleBase := live(runtime.ProgramIdle, 1, 0).Baseline()
	busyBase := live(runtime.ProgramWorking, 2, 1).Baseline()
	started := live(runtime.ProgramWorking, 2, 1)
	other := started
	other.Nonce = "r2.%3"
	cases := []struct {
		name                     string
		s                        runtime.ProgramStatus
		b                        runtime.ProgramBaseline
		confirm, startup, resend runtime.ProgramVerdict
	}{
		{"turn started past an idle baseline", started, idleBase, runtime.ProgramVerdictConfirmed, runtime.ProgramVerdictConfirmed, runtime.ProgramVerdictNoResend},
		{"busy baseline", live(runtime.ProgramWorking, 3, 1), busyBase, runtime.ProgramVerdictUnconfirmed, runtime.ProgramVerdictUnconfirmed, runtime.ProgramVerdictResend},
		{"other nonce", other, idleBase, runtime.ProgramVerdictUnconfirmed, runtime.ProgramVerdictUnconfirmed, runtime.ProgramVerdictResend},
		{"unchanged", live(runtime.ProgramIdle, 1, 0), idleBase, runtime.ProgramVerdictUnconfirmed, runtime.ProgramVerdictUnconfirmed, runtime.ProgramVerdictResend},
		{"blocked past an idle baseline", live(runtime.ProgramBlocked, 2, 0), idleBase, runtime.ProgramVerdictConfirmed, runtime.ProgramVerdictPending, runtime.ProgramVerdictNoResend},
		{"unread", runtime.ProgramStatus{}, idleBase, runtime.ProgramVerdictUnconfirmed, runtime.ProgramVerdictUnconfirmed, runtime.ProgramVerdictResend},
	}
	for _, c := range cases {
		if got := runtime.ProgramConfirmVerdict(c.s, c.b); got != c.confirm {
			t.Errorf("%s: ProgramConfirmVerdict = %q, want %q", c.name, got, c.confirm)
		}
		if got := runtime.ProgramStartupVerdict(c.s, c.b, time.Time{}, shadowNow); got != c.startup {
			t.Errorf("%s: ProgramStartupVerdict = %q, want %q", c.name, got, c.startup)
		}
		if got := runtime.ProgramResendVerdict(c.s, c.b); got != c.resend {
			t.Errorf("%s: ProgramResendVerdict = %q, want %q", c.name, got, c.resend)
		}
	}
}

// Kills a readiness rule that is ready before the settle has passed, or with
// no valid report.
func TestProgramReadyVerdict(t *testing.T) {
	first := shadowNow.Add(-runtime.ProgramStatusReadySettle)
	if got := runtime.ProgramReadyVerdict(live(runtime.ProgramIdle, 1, 0), first, shadowNow); got != runtime.ProgramVerdictReady {
		t.Errorf("settled: %q, want ready", got)
	}
	if got := runtime.ProgramReadyVerdict(live(runtime.ProgramIdle, 1, 0), first.Add(time.Millisecond), shadowNow); got != runtime.ProgramVerdictNotReady {
		t.Errorf("inside the settle: %q, want not_ready", got)
	}
	if got := runtime.ProgramReadyVerdict(live(runtime.ProgramIdle, 1, 0), time.Time{}, shadowNow); got != runtime.ProgramVerdictNotReady {
		t.Errorf("no first report: %q, want not_ready", got)
	}
	if got := runtime.ProgramReadyVerdict(runtime.ProgramStatus{}, first, shadowNow); got != runtime.ProgramVerdictNotReady {
		t.Errorf("unread: %q, want not_ready", got)
	}
}

// Kills an interrupt settle that credits a boundary from before the
// interrupt, another incarnation, or a dialog.
func TestProgramSettleVerdict(t *testing.T) {
	busyBase := live(runtime.ProgramWorking, 4, 2).Baseline()
	idleBase := live(runtime.ProgramIdle, 4, 2).Baseline()
	cases := []struct {
		name string
		s    runtime.ProgramStatus
		b    runtime.ProgramBaseline
		want runtime.ProgramVerdict
	}{
		{"boundary past a busy baseline", live(runtime.ProgramIdle, 5, 2), busyBase, runtime.ProgramVerdictSettled},
		{"still working", live(runtime.ProgramWorking, 4, 2), busyBase, runtime.ProgramVerdictUnsettled},
		{"boundary with no change", live(runtime.ProgramIdle, 4, 2), busyBase, runtime.ProgramVerdictUnsettled},
		{"idle baseline", live(runtime.ProgramIdle, 4, 2), idleBase, runtime.ProgramVerdictSettled},
		{"blocked", live(runtime.ProgramBlocked, 5, 2), busyBase, runtime.ProgramVerdictPending},
		{"no baseline", live(runtime.ProgramIdle, 5, 2), runtime.ProgramBaseline{}, runtime.ProgramVerdictUnsettled},
		{"unread", runtime.ProgramStatus{}, busyBase, runtime.ProgramVerdictUnsettled},
	}
	for _, c := range cases {
		if got := runtime.ProgramSettleVerdict(c.s, c.b, time.Time{}, shadowNow); got != c.want {
			t.Errorf("%s: ProgramSettleVerdict = %q, want %q", c.name, got, c.want)
		}
	}
}

// Kills a sleep proof from one reading, a changed record, a working child,
// or an invalid read.
func TestProgramQuiescentVerdict(t *testing.T) {
	idle := live(runtime.ProgramIdle, 4, 1)
	moved := live(runtime.ProgramIdle, 5, 1)
	child := idle
	child.WorkingChildren = 1
	cases := []struct {
		name          string
		first, second runtime.ProgramStatus
		want          runtime.ProgramVerdict
	}{
		{"two quiet reads", idle, idle, runtime.ProgramVerdictIdle},
		{"changed between", idle, moved, runtime.ProgramVerdictBusy},
		{"working child", child, child, runtime.ProgramVerdictBusy},
		{"working", live(runtime.ProgramWorking, 4, 1), live(runtime.ProgramWorking, 4, 1), runtime.ProgramVerdictBusy},
		{"invalid second read", idle, runtime.ProgramStatus{}, runtime.ProgramVerdictIndeterminate},
	}
	for _, c := range cases {
		if got := runtime.ProgramQuiescentVerdict(c.first, c.second); got != c.want {
			t.Errorf("%s: ProgramQuiescentVerdict = %q, want %q", c.name, got, c.want)
		}
	}
}

// Kills a boundary time that is not the record's own: the last turn end when
// stamped, else entry into the state, and nothing off a boundary.
func TestProgramStatusBoundaryAt(t *testing.T) {
	s := live(runtime.ProgramIdle, 1, 0)
	if got := s.BoundaryAt(); !got.Equal(s.Since) {
		t.Errorf("never worked: BoundaryAt = %v, want Since %v", got, s.Since)
	}
	s.QuietSince = shadowNow.Add(-3 * time.Second)
	if got := s.BoundaryAt(); !got.Equal(s.QuietSince) {
		t.Errorf("after a turn: BoundaryAt = %v, want QuietSince %v", got, s.QuietSince)
	}
	if got := live(runtime.ProgramWorking, 2, 1).BoundaryAt(); !got.IsZero() {
		t.Errorf("working: BoundaryAt = %v, want zero", got)
	}
}

// Kills a shadow read that records without a sink, under a switch other
// than shadow, on a read error, or that misses the leaf behind a composite.
func TestProgramStatusShadowFor(t *testing.T) {
	f := runtime.NewFake()
	if err := f.Start(t.Context(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	ff := runtime.FullFake{Fake: f}
	row := armedRow()
	row.Mode = runtime.ProgramStatusShadow
	f.SetProgramPane("s", row)
	if _, _, ok := runtime.ProgramStatusShadowFor(ff, "s"); ok {
		t.Fatal("a leaf with no sink shadowed")
	}
	if _, _, ok := runtime.ProgramStatusShadowFor(runtime.NewFake(), "s"); ok {
		t.Fatal("a leaf without the capability shadowed")
	}
	var got []runtime.ProgramStatusComparison
	f.ProgramShadowSink = func(c runtime.ProgramStatusComparison) { got = append(got, c) }
	s, sink, ok := runtime.ProgramStatusShadowFor(auto.New(ff, runtime.NewFake()), "s")
	if !ok || sink == nil || !s.Running() || s.Mode != runtime.ProgramStatusShadow {
		t.Fatalf("shadowing leaf behind a composite: %+v, %v; want its working shadow reading and sink", s, ok)
	}
	sink(runtime.ProgramStatusComparison{Session: "s"})
	if len(got) != 1 {
		t.Fatalf("the sink handed out is not the leaf's: %d comparisons reached it", len(got))
	}
	row.Mode = runtime.ProgramStatusOn
	f.SetProgramPane("s", row)
	if _, _, ok := runtime.ProgramStatusShadowFor(ff, "s"); ok {
		t.Fatal("an on reading shadowed")
	}
	f.ProgramStatusErrors = map[string]error{"s": errors.New("boom")}
	if _, _, ok := runtime.ProgramStatusShadowFor(ff, "s"); ok {
		t.Fatal("a failed read shadowed")
	}
}

// Kills a hold comparison that does not carry the activity and time its rule
// judged with, so a long turn kept live by fresh activity is recorded as a
// dead hold, and one that reads the activity clock while not shadowing.
func TestShadowCompareHoldCarriesTheRulesHold(t *testing.T) {
	f := runtime.NewFake()
	if err := f.Start(t.Context(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	f.Activity = map[string]time.Time{"s": now.Add(-time.Minute)}
	row := armedRow()
	row.Mode = runtime.ProgramStatusShadow
	row.Record.Since = now.Add(-15 * time.Minute)
	f.SetProgramPane("s", row)
	var got []runtime.ProgramStatusComparison
	f.ProgramShadowSink = func(c runtime.ProgramStatusComparison) { got = append(got, c) }
	runtime.ShadowCompareHold(runtime.FullFake{Fake: f}, "s", "gc-1", "claude", runtime.ProgramPointExecutionBackstop, runtime.ProgramVerdictNoHold, now, runtime.ProgramHoldVerdict)
	if len(got) != 1 || got[0].Program != runtime.ProgramVerdictHold {
		t.Fatalf("recorded %+v, want one hold", got)
	}
	if c := got[0]; !c.HoldActivity.Equal(now.Add(-time.Minute)) || !c.JudgedAt.Equal(now) || !c.HoldLive(now.Add(time.Hour)) {
		t.Fatalf("comparison activity %v at %v, hold live %v; want the rule's activity and time, live", c.HoldActivity, c.JudgedAt, c.HoldLive(now.Add(time.Hour)))
	}
	if (runtime.ProgramStatusComparison{Status: got[0].Status}).HoldLive(now) {
		t.Fatal("the record alone is live past the hold bound: the test does not exercise the activity")
	}
	off := runtime.NewFake()
	if err := off.Start(t.Context(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	runtime.ShadowCompareHold(runtime.FullFake{Fake: off}, "s", "", "", runtime.ProgramPointExecutionBackstop, runtime.ProgramVerdictNoHold, now, runtime.ProgramHoldVerdict)
	for _, c := range off.Calls {
		if c.Method == "GetLastActivity" {
			t.Fatal("a leaf not shadowing had its activity clock read")
		}
	}
}

// Kills a caller-records mark that leaks into an unmarked context.
func TestShadowCallerRecords(t *testing.T) {
	if runtime.ShadowCallerRecords(t.Context()) {
		t.Fatal("an unmarked context reads as caller-recorded")
	}
	if !runtime.ShadowCallerRecords(runtime.WithShadowCallerRecords(t.Context())) {
		t.Fatal("a marked context does not read as caller-recorded")
	}
}
