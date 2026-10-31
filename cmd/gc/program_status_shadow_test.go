package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// payloadEnum returns the enum tag of the payload field whose json name is
// name.
func payloadEnum(t *testing.T, name string) []string {
	t.Helper()
	typ := reflect.TypeFor[events.SessionProgramStatusShadowPayload]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		if strings.Split(f.Tag.Get("json"), ",")[0] == name {
			return strings.Split(f.Tag.Get("enum"), ",")
		}
	}
	t.Fatalf("payload has no field %q", name)
	return nil
}

// Kills a payload enum that drifts from the runtime's closed tokens: the
// decision points, the verdicts, and every validity a reading can carry.
func TestShadowPayloadEnumsMatchRuntimeTokens(t *testing.T) {
	var points, verdicts []string
	for _, p := range runtime.ProgramStatusPoints() {
		points = append(points, string(p))
	}
	for _, v := range runtime.ProgramVerdicts() {
		verdicts = append(verdicts, string(v))
	}
	if got := payloadEnum(t, "point"); !slices.Equal(got, points) {
		t.Errorf("point enum %q, want runtime's %q", got, points)
	}
	for _, field := range []string{"scrape", "program"} {
		if got := payloadEnum(t, field); !slices.Equal(got, verdicts) {
			t.Errorf("%s enum %q, want runtime's %q", field, got, verdicts)
		}
	}
	validities := payloadEnum(t, "validity")
	for _, v := range []runtime.ProgramStatusValidity{
		runtime.ProgramStatusUnread, runtime.ProgramStatusValid, runtime.ProgramStatusNotArmed,
		runtime.ProgramStatusSilent, runtime.ProgramStatusCleared, runtime.ProgramStatusReaderLost,
		runtime.ProgramStatusExited,
	} {
		p := programShadowPayload(runtime.ProgramStatusComparison{Status: runtime.ProgramStatus{Validity: v}}, 0, time.Now())
		if !slices.Contains(validities, p.Validity) {
			t.Errorf("validity %q renders %q, outside the enum %q", v, p.Validity, validities)
		}
	}
	for _, s := range []runtime.ProgramState{runtime.ProgramIdle, runtime.ProgramWorking, runtime.ProgramDone, runtime.ProgramBlocked, runtime.ProgramError} {
		if !slices.Contains(payloadEnum(t, "state"), string(s)) {
			t.Errorf("state %q is outside the state enum", s)
		}
	}
}

// Kills free text on the shadow wire: every string field but the bead id and
// the family is a closed enum, a family outside its short alphabet is
// recorded as other, and the envelope carries no message.
func TestShadowPayloadHasNoFreeText(t *testing.T) {
	typ := reflect.TypeFor[events.SessionProgramStatusShadowPayload]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if f.Type.Kind() != reflect.String || name == "session_id" || name == "family" {
			continue
		}
		if f.Tag.Get("enum") == "" {
			t.Errorf("string field %s has no closed enum", name)
		}
	}
	c := runtime.ProgramStatusComparison{
		Session: "s", SessionID: "gc-1", Family: "Claude Code\n<script>",
		Point: runtime.ProgramPointPending, Scrape: runtime.ProgramVerdictNone, Program: runtime.ProgramVerdictPending,
		Status: runtime.ProgramStatus{
			Mode: runtime.ProgramStatusShadow, Validity: runtime.ProgramStatusValid, State: runtime.ProgramBlocked,
			Kind: runtime.ProgramBlockQuestion, App: "claude-code", Since: time.Now().Add(-time.Second),
		},
	}
	ev, ok := programShadowEvent(c, 0, time.Now())
	if !ok {
		t.Fatal("no event")
	}
	if ev.Type != events.SessionProgramStatusShadow || ev.Message != "" || ev.Subject != "s" || ev.SessionID != "gc-1" {
		t.Fatalf("envelope %+v", ev)
	}
	var p events.SessionProgramStatusShadowPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Family != "other" || p.State != "blocked" || p.Kind != "question" || p.Agree || !p.HoldLive || p.RecordAgeMs <= 0 {
		t.Fatalf("payload %+v", p)
	}
	if strings.Contains(string(ev.Payload), "claude-code") {
		t.Fatalf("payload carries the program's app name: %s", ev.Payload)
	}
	if got := programShadowPayload(runtime.ProgramStatusComparison{Family: "codex"}, 0, time.Now()).Family; got != "codex" {
		t.Fatalf("family codex recorded as %q", got)
	}
}

// eventCollector is an events recorder a shadow recorder writes to.
type eventCollector struct {
	mu    sync.Mutex
	got   []events.Event
	block chan struct{}
}

func (c *eventCollector) Record(e events.Event) {
	if c.block != nil {
		<-c.block
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, e)
}

func (c *eventCollector) payloads(t *testing.T) []events.SessionProgramStatusShadowPayload {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []events.SessionProgramStatusShadowPayload
	for _, e := range c.got {
		var p events.SessionProgramStatusShadowPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// shadowRecorderUnder returns a fresh city's shadow sink writing to c.
func shadowRecorderUnder(t *testing.T, c *eventCollector) (runtime.ProgramStatusComparisonSink, *programShadowRecorder) {
	t.Helper()
	prev := programShadowOpen
	programShadowOpen = func(string) events.Recorder { return c }
	t.Cleanup(func() { programShadowOpen = prev })
	city := t.TempDir()
	sink := programStatusShadowSink(city)
	r, _ := programShadowRecorders.Load(city)
	return sink, r.(*programShadowRecorder)
}

// comparison is a comparison at point with the given verdicts on a valid
// reading.
func comparison(session string, point runtime.ProgramStatusPoint, scrape, program runtime.ProgramVerdict) runtime.ProgramStatusComparison {
	return runtime.ProgramStatusComparison{
		Session: session, Point: point, Scrape: scrape, Program: program,
		Status: runtime.ProgramStatus{Validity: runtime.ProgramStatusValid, State: runtime.ProgramIdle},
	}
}

// Kills a recorder that writes a continuously evaluated point's every
// evaluation: a change point is recorded when its (scrape, program,
// validity) triple for the session changes, an on-demand point every time.
func TestShadowEmitsOnPairChangeOnly(t *testing.T) {
	c := &eventCollector{}
	sink, r := shadowRecorderUnder(t, c)
	hold := runtime.ProgramPointExecutionBackstop
	offers := []runtime.ProgramStatusComparison{
		comparison("a", hold, runtime.ProgramVerdictNoHold, runtime.ProgramVerdictNoHold),
		comparison("a", hold, runtime.ProgramVerdictNoHold, runtime.ProgramVerdictNoHold),
		comparison("b", hold, runtime.ProgramVerdictNoHold, runtime.ProgramVerdictNoHold),
		comparison("a", hold, runtime.ProgramVerdictNoHold, runtime.ProgramVerdictHold),
		comparison("a", hold, runtime.ProgramVerdictNoHold, runtime.ProgramVerdictHold),
		comparison("a", runtime.ProgramPointWaitForIdle, runtime.ProgramVerdictIdle, runtime.ProgramVerdictIdle),
		comparison("a", runtime.ProgramPointWaitForIdle, runtime.ProgramVerdictIdle, runtime.ProgramVerdictIdle),
	}
	silent := comparison("a", hold, runtime.ProgramVerdictNoHold, runtime.ProgramVerdictHold)
	silent.Status.Validity = runtime.ProgramStatusSilent
	offers = append(offers, silent)
	for _, o := range offers {
		sink(o)
	}
	if !r.flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	got := c.payloads(t)
	var points []string
	for _, p := range got {
		points = append(points, p.Point+":"+p.Program+":"+p.Validity)
	}
	want := []string{
		"execution_backstop:no_hold:valid", "execution_backstop:no_hold:valid",
		"execution_backstop:hold:valid", "wait_for_idle:idle:valid", "wait_for_idle:idle:valid",
		"execution_backstop:hold:silent",
	}
	if !slices.Equal(points, want) {
		t.Fatalf("recorded %q, want %q", points, want)
	}
}

// Kills a sink that blocks the decision path: a recorder stuck writing
// still takes every comparison at once, drops what the queue cannot hold,
// and counts the drops into the next recorded payload.
func TestShadowSinkNeverBlocks(t *testing.T) {
	c := &eventCollector{block: make(chan struct{})}
	sink, r := shadowRecorderUnder(t, c)
	const offered = programShadowQueueSize * 4
	start := time.Now()
	for range offered {
		sink(comparison("s", runtime.ProgramPointWaitForIdle, runtime.ProgramVerdictIdle, runtime.ProgramVerdictIdle))
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("offering %d comparisons to a stuck recorder took %v", offered, elapsed)
	}
	close(c.block)
	if !r.flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	sink(comparison("s", runtime.ProgramPointWaitForIdle, runtime.ProgramVerdictIdle, runtime.ProgramVerdictIdle))
	if !r.flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	got := c.payloads(t)
	dropped := 0
	for _, p := range got {
		dropped += p.Dropped
	}
	if len(got)+dropped != offered+1 || dropped == 0 {
		t.Fatalf("recorded %d and counted %d dropped, want %d offered in all with drops", len(got), dropped, offered+1)
	}
}

// Kills a tmux config whose shadow sink depends on the switch it was built
// under: a reload that publishes shadow to a leaf built under off or on must
// find the sink there (the leaf hands it out only under shadow; tmux
// TestShadowSinkFollowsPublishedSwitch). A process with no city records
// nothing.
func TestTmuxConfigComparisonSinkForEveryCity(t *testing.T) {
	city := t.TempDir()
	for _, set := range []string{"", "off", "on", "shadow"} {
		if cfg := tmuxConfigFromSession(config.SessionConfig{ProgramStatus: set}, "city", city); cfg.ComparisonSink == nil {
			t.Errorf("program_status %q: no comparison sink for the city", set)
		}
	}
	if cfg := tmuxConfigFromSession(config.SessionConfig{ProgramStatus: "shadow"}, "city", ""); cfg.ComparisonSink != nil {
		t.Error("program_status shadow with no city: a comparison sink is configured")
	}
}

// shadowLeaf is a running fake session "s" whose program reports a live
// working root under mode and whose screen reads idle, its activity an hour
// old; under shadow its leaf hands comparisons to the returned list.
func shadowLeaf(t *testing.T, mode runtime.ProgramStatusMode) (runtime.FullFake, func() []runtime.ProgramStatusComparison) {
	t.Helper()
	f := runtime.NewFake()
	if err := f.Start(context.Background(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	f.Activity = map[string]time.Time{"s": time.Now().Add(-time.Hour)}
	f.WaitForIdleErrors = map[string]error{"s": nil}
	f.SetProgramPane("s", runtime.ProgramPaneRow{
		Mode: mode, CaptureGate: true, PaneID: "%3", PanePipe: true, Tap: "r1.%3",
		TapLiveness: runtime.ProgramTapAlive, RecordForm: runtime.ProgramRecordDecoded,
		Record: runtime.ProgramStatus{State: runtime.ProgramWorking, Nonce: "r1.%3", Seq: 2, TurnStarts: 1, Progress: -1, Reported: true, Since: time.Now()},
	})
	var mu sync.Mutex
	var got []runtime.ProgramStatusComparison
	if mode == runtime.ProgramStatusShadow {
		f.ProgramShadowSink = func(c runtime.ProgramStatusComparison) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, c)
		}
	}
	return runtime.FullFake{Fake: f}, func() []runtime.ProgramStatusComparison {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// Kills a cmd/gc decision point whose verdict differs under shadow from the
// switch off, or that records a comparison under off or none under shadow.
func TestShadowDecisionIdenticalToOff(t *testing.T) {
	row := beads.Bead{ID: "gc-1", Metadata: map[string]string{"provider": "claude", "session_name": "s"}}
	stale := time.Now().Add(-time.Hour)
	points := []struct {
		point  runtime.ProgramStatusPoint
		decide func(sp runtime.Provider) any
		scrape runtime.ProgramVerdict
		prog   runtime.ProgramVerdict
	}{
		{
			runtime.ProgramPointClaimNudgeHold, func(sp runtime.Provider) any { return claimNudgeBusyHold(sp, row, "s", time.Now()) },
			runtime.ProgramVerdictNoHold, runtime.ProgramVerdictHold,
		},
		{runtime.ProgramPointExecutionBackstop, func(sp runtime.Provider) any {
			return poolExecutionBackstop{sp: sp, now: time.Now()}.quietHold(row, "s")
		}, runtime.ProgramVerdictNoHold, runtime.ProgramVerdictHold},
		{
			runtime.ProgramPointProvedIdle, func(sp runtime.Provider) any {
				return provedIdle(context.Background(), sp, "s", stale.Add(time.Minute))
			},
			runtime.ProgramVerdictIdle, runtime.ProgramVerdictBusy,
		},
		{runtime.ProgramPointQueuedDelivery, func(sp runtime.Provider) any {
			return pollerSessionIdleEnough(nudgeTarget{sessionName: "s", sessionID: "gc-1"}, sp, time.Second, worker.LiveObservation{LastActivity: &stale})
		}, runtime.ProgramVerdictNoHold, runtime.ProgramVerdictHold},
	}
	for _, p := range points {
		t.Run(string(p.point), func(t *testing.T) {
			off, offGot := shadowLeaf(t, runtime.ProgramStatusOff)
			shadow, shadowGot := shadowLeaf(t, runtime.ProgramStatusShadow)
			if a, b := p.decide(off), p.decide(shadow); !reflect.DeepEqual(a, b) {
				t.Fatalf("shadow decided %v, off %v", b, a)
			}
			if got := offGot(); len(got) != 0 {
				t.Fatalf("the switch off recorded %+v", got)
			}
			got := shadowGot()
			if len(got) != 1 || got[0].Point != p.point || got[0].Scrape != p.scrape || got[0].Program != p.prog {
				t.Fatalf("shadow recorded %+v, want one %s %s/%s", got, p.point, p.scrape, p.prog)
			}
		})
	}
}

// Kills a summarizer that counts invalid readings toward agreement, weighs
// change points by count alone, or mis-takes latency percentiles and drops.
func TestShadowReportAgreementTable(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	rec := func(sec int, session, point, family, validity string, agree bool, latency int64, timed bool, dropped int) programShadowRecord {
		return programShadowRecord{At: t0.Add(time.Duration(sec) * time.Second), Session: session, Payload: events.SessionProgramStatusShadowPayload{
			Point: point, Family: family, Validity: validity, Agree: agree, LatencyMs: latency, Timed: timed, Dropped: dropped,
		}}
	}
	report := summarizeProgramShadow([]programShadowRecord{
		// A change point: agreeing for 90 s, disagreeing for 10 s.
		rec(0, "a", "execution_backstop", "claude", "valid", true, 0, false, 0),
		rec(90, "a", "execution_backstop", "claude", "valid", false, 0, false, 0),
		rec(100, "a", "execution_backstop", "claude", "valid", true, 0, false, 0),
		// An on-demand timed point, one invalid reading.
		rec(10, "a", "wait_for_idle", "claude", "valid", true, -200, true, 2),
		rec(20, "a", "wait_for_idle", "claude", "valid", false, 400, true, 0),
		rec(30, "b", "wait_for_idle", "claude", "silent", false, 0, false, 0),
		rec(40, "c", "wait_for_idle", "codex", "valid", true, 100, true, 0),
		rec(100, "c", "pending", "", "valid", true, 0, false, 0),
	})
	if report.Total != 8 || report.Dropped != 2 {
		t.Fatalf("total %d dropped %d, want 8 and 2", report.Total, report.Dropped)
	}
	cells := map[string]programShadowCell{}
	for _, c := range report.Rows {
		cells[c.Point+"/"+c.Family] = c
	}
	if got := []string{report.Rows[0].Point, report.Rows[len(report.Rows)-1].Point}; got[0] != "wait_for_idle" || got[1] != "execution_backstop" {
		t.Fatalf("rows not in design order: %+v", report.Rows)
	}
	back := cells["execution_backstop/claude"]
	if back.Count != 3 || back.AgreePct != 66.7 || back.TimeWeightedAgreePct != 90 {
		t.Fatalf("backstop cell %+v, want 3 comparisons, 66.7%% by count, 90%% by time", back)
	}
	wait := cells["wait_for_idle/claude"]
	if wait.Count != 3 || wait.Valid != 2 || wait.AgreePct != 50 || wait.TimeWeightedAgreePct != -1 ||
		wait.DisagreeByValidity["valid"] != 1 || wait.DisagreeByValidity["silent"] != 1 ||
		wait.Timed != 2 || wait.LatencyP50Ms != -200 || wait.LatencyP95Ms != 400 || wait.Dropped != 2 {
		t.Fatalf("wait cell %+v", wait)
	}
	if _, ok := cells["pending/unknown"]; !ok {
		t.Fatalf("a comparison with no family is not reported as unknown: %+v", report.Rows)
	}
}

// ctxCapturingLeaf is a shadow leaf that keeps the context its idle wait
// was called with.
type ctxCapturingLeaf struct {
	runtime.FullFake
	waitCtx context.Context
}

func (l *ctxCapturingLeaf) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	l.waitCtx = ctx
	return l.FullFake.WaitForIdle(ctx, name, timeout)
}

// Kills an idle proof whose leaf wait records a wait_for_idle of its own
// beside proved_idle (R1B M1): the proof marks the wait's context as
// recorded by its caller, and proved_idle records only when its pair
// changes, so a busy seat's failed proofs are one comparison, not hundreds.
func TestProvedIdleMarksItsWaitCallerRecorded(t *testing.T) {
	c := &eventCollector{}
	sink, r := shadowRecorderUnder(t, c)
	shadow, _ := shadowLeaf(t, runtime.ProgramStatusShadow)
	shadow.ProgramShadowSink = sink
	shadow.WaitForIdleErrors["s"] = errors.New("busy")
	leaf := &ctxCapturingLeaf{FullFake: shadow}
	for range 400 {
		if provedIdle(context.Background(), leaf, "s", time.Now()) {
			t.Fatal("a failing wait proved idle")
		}
		if !runtime.ShadowCallerRecords(leaf.waitCtx) {
			t.Fatal("the idle proof's wait is not marked as recorded by its caller")
		}
	}
	if !r.flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	got := c.payloads(t)
	if len(got) != 1 || got[0].Point != string(runtime.ProgramPointProvedIdle) {
		t.Fatalf("400 failed proofs recorded %+v, want one proved_idle", got)
	}
}

// Kills a hold comparison whose recorded hold_live is not the one its rule
// judged with (R1B M3): a turn past the hold bound by its record's age but
// with fresh runtime activity holds, and is recorded hold_live=true.
func TestShadowHoldLiveIsTheRulesOwn(t *testing.T) {
	f := runtime.NewFake()
	if err := f.Start(context.Background(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	f.Activity = map[string]time.Time{"s": now.Add(-time.Minute)}
	f.SetProgramPane("s", runtime.ProgramPaneRow{
		Mode: runtime.ProgramStatusShadow, CaptureGate: true, PaneID: "%3", PanePipe: true, Tap: "r1.%3",
		TapLiveness: runtime.ProgramTapAlive, RecordForm: runtime.ProgramRecordDecoded,
		Record: runtime.ProgramStatus{State: runtime.ProgramWorking, Nonce: "r1.%3", Seq: 2, TurnStarts: 1, Progress: -1, Reported: true, Since: now.Add(-15 * time.Minute)},
	})
	var got []runtime.ProgramStatusComparison
	f.ProgramShadowSink = func(c runtime.ProgramStatusComparison) { got = append(got, c) }
	shadowHold(runtime.FullFake{Fake: f}, "s", "gc-1", "claude", runtime.ProgramPointClaimNudgeHold, false, now, runtime.ProgramHoldVerdict)
	if len(got) != 1 || got[0].Program != runtime.ProgramVerdictHold {
		t.Fatalf("recorded %+v, want one record hold", got)
	}
	// The payload is written later, as the drain gets to it.
	p := programShadowPayload(got[0], 0, now.Add(2*time.Second))
	if !p.HoldLive || p.Program != string(runtime.ProgramVerdictHold) {
		t.Fatalf("payload program %q hold_live %v, want hold beside hold_live=true", p.Program, p.HoldLive)
	}
}

// Kills an exit flush that returns while the leaf is still judging a
// comparison (R1B M2): the flush waits for the leaf's background work, then
// for the queue, so a short-lived process records what it decided.
func TestShadowExitFlushWaitsForLeafWork(t *testing.T) {
	c := &eventCollector{}
	sink, _ := shadowRecorderUnder(t, c)
	done := programShadowBeginWork()
	release := make(chan struct{})
	go func() {
		<-release
		sink(comparison("s", runtime.ProgramPointWaitForIdle, runtime.ProgramVerdictIdle, runtime.ProgramVerdictBusy))
		done()
	}()
	flushed := make(chan struct{})
	go func() {
		flushProgramStatusShadow(5 * time.Second)
		close(flushed)
	}()
	select {
	case <-flushed:
		t.Fatal("the exit flush returned while the leaf was still judging a comparison")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-flushed
	if got := c.payloads(t); len(got) != 1 || got[0].Point != string(runtime.ProgramPointWaitForIdle) {
		t.Fatalf("after the exit flush recorded %+v, want the comparison the leaf was judging", got)
	}
	if n := programShadowWork.Load(); n != 0 {
		t.Fatalf("%d pieces of shadow work counted after they ended", n)
	}
}

// Kills a tmux config that lets a short-lived process follow comparisons
// up, or hides the leaf's background work from the exit flush.
func TestTmuxConfigShadowFollowUpsOnlyLongLived(t *testing.T) {
	prev := programShadowLongLived.Load()
	t.Cleanup(func() { programShadowLongLived.Store(prev) })
	cfg := tmuxConfigFromSession(config.SessionConfig{ProgramStatus: "shadow"}, "city", t.TempDir())
	if cfg.ComparisonFollowUps == nil || cfg.ComparisonWork == nil {
		t.Fatal("a city's tmux config has no follow-up gate or work count")
	}
	programShadowLongLived.Store(false)
	if cfg.ComparisonFollowUps() {
		t.Fatal("a short-lived process follows comparisons up")
	}
	markProgramShadowLongLived()
	if !cfg.ComparisonFollowUps() {
		t.Fatal("a long-lived process does not follow comparisons up")
	}
	done := programShadowBeginWork()
	done()
	done()
	if n := programShadowWork.Load(); n != 0 {
		t.Fatalf("ending one piece of work twice left the count at %d", n)
	}
}

// panickingRecorder panics on its first write, then records.
type panickingRecorder struct {
	eventCollector
	panicked bool
}

func (p *panickingRecorder) Record(e events.Event) {
	if !p.panicked {
		p.panicked = true
		panic("events log bug")
	}
	p.eventCollector.Record(e)
}

// Kills a drain whose panic stops the city's shadow recording (the
// supervisor hosts several cities): the panicking write is counted as
// dropped and the next comparison is still written.
func TestShadowDrainRecoversPanic(t *testing.T) {
	rec := &panickingRecorder{}
	prev := programShadowOpen
	programShadowOpen = func(string) events.Recorder { return rec }
	t.Cleanup(func() { programShadowOpen = prev })
	city := t.TempDir()
	sink := programStatusShadowSink(city)
	v, _ := programShadowRecorders.Load(city)
	r := v.(*programShadowRecorder)
	sink(comparison("s", runtime.ProgramPointWaitForIdle, runtime.ProgramVerdictIdle, runtime.ProgramVerdictIdle))
	sink(comparison("s", runtime.ProgramPointWaitForIdle, runtime.ProgramVerdictIdle, runtime.ProgramVerdictBusy))
	if !r.flush(5 * time.Second) {
		t.Fatal("flush timed out after a panicking write")
	}
	got := rec.payloads(t)
	if len(got) != 1 || got[0].Dropped != 1 || got[0].Program != string(runtime.ProgramVerdictBusy) {
		t.Fatalf("recorded %+v, want the second comparison carrying the panicked one as dropped", got)
	}
}

// Kills a summarizer that stretches a session's last change record to the
// end of the report: it stands only until the session was last seen.
func TestShadowReportDoesNotStretchLastRecord(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	rec := func(sec int, session, point string, agree bool) programShadowRecord {
		return programShadowRecord{At: t0.Add(time.Duration(sec) * time.Second), Session: session, Payload: events.SessionProgramStatusShadowPayload{
			Point: point, Family: "claude", Validity: "valid", Agree: agree,
		}}
	}
	report := summarizeProgramShadow([]programShadowRecord{
		// Session a agrees 10 s, then its last backstop record disagrees and
		// stands until a's last sign of life at 30 s: 20 s.
		rec(0, "a", "execution_backstop", true),
		rec(10, "a", "execution_backstop", false),
		rec(30, "a", "wait_for_idle", true),
		// Another session keeps the report open for an hour.
		rec(3600, "b", "wait_for_idle", true),
	})
	for _, c := range report.Rows {
		if c.Point == "execution_backstop" && c.TimeWeightedAgreePct != 33.3 {
			t.Fatalf("backstop time-weighted agreement %v, want 33.3 (10 s of 30 s, not stretched to the report end)", c.TimeWeightedAgreePct)
		}
	}
}
