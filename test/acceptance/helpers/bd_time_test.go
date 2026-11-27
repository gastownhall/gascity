package acceptancehelpers

import (
	"os"
	"strings"
	"testing"
	"time"
)

// writeBDTrace appends raw JSONL lines to a trace sink, standing in for
// TraceBDCall without running a bd.
func writeBDTrace(t *testing.T, trace *BDTrace, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(trace.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open bd trace: %v", err)
	}
	defer f.Close() //nolint:errcheck // test fixture
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatalf("write bd trace: %v", err)
		}
	}
}

// TestTracedChildMillisSumsPassthroughDurations is the instrument the rewritten
// `gc bd list --json` acceptance line stands on.
//
// That command is one bd exec BY CONSTRUCTION — it is a passthrough that never
// opens a store, so no store split can remove its fork — and its wall time is
// therefore bd's time plus gc's. Asserting the total would measure bd on
// whatever box CI landed on. Subtracting the traced child time leaves the part
// gc answers for, and this is the subtraction.
//
// The two records are the real shape: the passthrough exec traced as
// `go:gc-bd-passthrough` (cmd/gc/cmd_bd.go) plus one in-process BdStore call,
// because a step that measures gc-side time must subtract EVERY child, not just
// the one the command is named after.
func TestTracedChildMillisSumsPassthroughDurations(t *testing.T) {
	trace := NewBDTrace(t)

	if got := trace.ChildMillis(); got != 0 {
		t.Fatalf("an untraced run reported %dms of bd child time", got)
	}
	if got := trace.Records(); got != nil {
		t.Fatalf("an absent trace file yielded %d records; absent means nothing ran", len(got))
	}
	if got := trace.Describe(); !strings.Contains(got, "no traced bd calls") {
		t.Errorf("Describe() on an empty trace = %q", got)
	}

	writeBDTrace(t, trace,
		`{"ts":"2026-09-22T10:00:00Z","source":"go:gc-bd-passthrough","args":["list","--json"],"dur_ms":420,"exit_code":0,"pid":11,"ppid":10}`,
		`{"ts":"2026-09-22T10:00:01Z","source":"go:bdstore","args":["show","ga-1","--json"],"dur_ms":80,"exit_code":0,"pid":12,"ppid":10}`,
	)

	if got := trace.ChildMillis(); got != 500 {
		t.Fatalf("ChildMillis() = %d, want 500:\n%s", got, trace.Describe())
	}

	records := trace.Records()
	if len(records) != 2 {
		t.Fatalf("Records() = %d, want 2", len(records))
	}
	if records[0].Source != "go:gc-bd-passthrough" {
		t.Errorf("first record source = %q, want the passthrough tag", records[0].Source)
	}
	if len(records[0].Args) != 2 || records[0].Args[0] != "list" {
		t.Errorf("first record args = %v, want [list --json]", records[0].Args)
	}

	// gc-side time is the whole point: 1.2s of wall around 0.5s of bd.
	if got := trace.GCSide(1200 * time.Millisecond); got != 700*time.Millisecond {
		t.Errorf("GCSide(1.2s) = %v, want 700ms", got)
	}
	// Concurrent children sum past the wall clock they ran in. A negative
	// gc-side budget would pass every upper bound ever asserted against it, so
	// it clamps at zero instead.
	if got := trace.GCSide(100 * time.Millisecond); got != 0 {
		t.Errorf("GCSide below the traced child time = %v, want 0", got)
	}
	if got := trace.Describe(); !strings.Contains(got, "bd list --json") {
		t.Errorf("Describe() = %q, want the passthrough argv in it", got)
	}

	trace.Reset()
	if got := trace.ChildMillis(); got != 0 {
		t.Fatalf("ChildMillis() after Reset = %d, want 0", got)
	}
	// Reset on an already-empty trace is how a step attributes a measurement to
	// its first phase as easily as its fifth.
	trace.Reset()
}

// TestParseBDTraceRefusesAnUndecodableLine pins the refusal, because the silent
// alternative fails in the dangerous direction: a dropped record under-reports
// bd child time, which over-reports gc-side time, and a budget gate then fails
// with a number nobody can reproduce — or passes a slow gc, if the dropped
// record was large.
func TestParseBDTraceRefusesAnUndecodableLine(t *testing.T) {
	good := `{"source":"go:bdstore","args":["ping"],"dur_ms":5,"exit_code":0}`
	records, err := parseBDTrace([]byte(good + "\n\n"))
	if err != nil {
		t.Fatalf("a well-formed trace with a blank line was refused: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("Records = %d, want 1 (a blank line belongs to no record)", len(records))
	}

	if _, err := parseBDTrace([]byte(good + "\n{\"source\":\"go:bdstore\",\n")); err == nil {
		t.Fatal("a truncated JSONL line was accepted; the trace's child-time sum would be short")
	} else if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("the refusal does not name the offending line: %v", err)
	}
}

// TestPassthroughGCSideRefusesAMeasurementOfNothing pins the two ways the
// `gc bd list --json` gc-side budget could pass vacuously (round3 review,
// completeness): a clamped-to-zero gc side from over-reported child time, and a
// clamped-to-zero marginal under a floor that is not a floor. Under an upper
// bound zero passes, so each is refused rather than clamped.
func TestPassthroughGCSideRefusesAMeasurementOfNothing(t *testing.T) {
	passthrough := BDTraceRecord{Source: BDPassthroughTraceSource, Args: []string{"list", "--json"}, DurMs: 420}
	provider := BDTraceRecord{Source: "go:provider-script", Args: []string{"gc-beads-bd", "probe"}, DurMs: 80}

	if got, err := PassthroughGCSide([]BDTraceRecord{passthrough, provider}, 1200*time.Millisecond); err != nil || got != 700*time.Millisecond {
		t.Fatalf("PassthroughGCSide(1.2s around 500ms of children) = (%v, %v), want (700ms, nil): every child is subtracted", got, err)
	}
	if got, err := PassthroughGCSide([]BDTraceRecord{passthrough, provider}, 450*time.Millisecond); err == nil {
		t.Errorf("children summing past the wall clock gave gc side %v with no error; clamped to zero it passes every budget", got)
	}
	if got, err := PassthroughGCSide([]BDTraceRecord{provider}, time.Second); err == nil {
		t.Errorf("a trace with no passthrough record gave gc side %v with no error; it did not measure the passthrough", got)
	}
	if _, err := PassthroughGCSide(nil, time.Second); err == nil {
		t.Error("an empty trace was accepted as a passthrough sample")
	}

	const budget = 100 * time.Millisecond
	if got, err := MarginalOverFloor(180*time.Millisecond, 100*time.Millisecond, budget); err != nil || got != 80*time.Millisecond {
		t.Fatalf("MarginalOverFloor(180ms over 100ms) = (%v, %v), want (80ms, nil)", got, err)
	}
	if got, err := MarginalOverFloor(95*time.Millisecond, 100*time.Millisecond, budget); err != nil || got != 0 {
		t.Errorf("MarginalOverFloor(a 5ms inversion) = (%v, %v), want (0, nil): noise, and the command is within noise of the floor", got, err)
	}
	if got, err := MarginalOverFloor(150*time.Millisecond, 2*time.Second, budget); err == nil {
		t.Errorf("a floor 1.85s above the command's gc side gave marginal %v with no error; it would pass any gc side under 2s", got)
	}
}

// floorScript stands in for the two gc processes MeasureMarginalOverFloor
// samples. Every sample, of either kind, is one tick of the box's clock;
// floorAt and gcSideAt get that tick and how many samples of their own kind
// came before, and return what the sample measured.
type floorScript struct {
	floorAt, gcSideAt func(tick, n int) time.Duration
	calls             []string
	floors, gcSides   int
}

func (s *floorScript) floor() time.Duration {
	d := s.floorAt(len(s.calls), s.floors)
	s.calls = append(s.calls, "floor")
	s.floors++
	return d
}

func (s *floorScript) gcSide() time.Duration {
	d := s.gcSideAt(len(s.calls), s.gcSides)
	s.calls = append(s.calls, "gc-side")
	s.gcSides++
	return d
}

// loadedBox returns a floorScript for a floor and a gc side of fixed cost on a
// box whose load slows every sample taken at a tick by loadPct(tick) percent.
func loadedBox(floorCost, gcSideCost time.Duration, loadPct func(tick int) int) *floorScript {
	return &floorScript{
		floorAt:  func(tick, _ int) time.Duration { return floorCost * time.Duration(loadPct(tick)) / 100 },
		gcSideAt: func(tick, _ int) time.Duration { return gcSideCost * time.Duration(loadPct(tick)) / 100 },
	}
}

// TestMeasureMarginalOverFloorInterleavesTheTwo pins the order the floor and
// the gc side are sampled in: alternately, a floor and then a gc side.
//
// Each best-of is a minimum over samples of one fixed cost plus whatever the
// box was doing at that moment. Taken in two blocks, five floors and then five
// gc sides, the two minima come from two different stretches of the box's
// load, and their difference measures how the load moved between the blocks
// as much as it measures the command (ga-97b2v9wd).
func TestMeasureMarginalOverFloorInterleavesTheTwo(t *testing.T) {
	box := loadedBox(60*time.Millisecond, 80*time.Millisecond, func(int) int { return 100 })
	MeasureMarginalOverFloor(100*time.Millisecond, box.floor, box.gcSide)
	if len(box.calls) == 0 {
		t.Fatal("MeasureMarginalOverFloor took no samples")
	}
	for i, kind := range box.calls {
		want := "floor"
		if i%2 == 1 {
			want = "gc-side"
		}
		if kind != want {
			t.Fatalf("sample %d was a %s, want a %s: the samples were taken in the order %v, so the two minima need not come from the same stretch of load", i+1, kind, want, box.calls)
		}
	}
}

// TestMeasureMarginalOverFloorReadsThroughALoadBurst reproduces the
// 2026-11-24 PR 9470 re-gate's reading (floor 272ms, gc side 144ms) with a
// floor that does strictly less than the gc side: a 60ms process start beside
// an 80ms gc side, on a box whose load runs at 4.53x for the first five
// samples taken and settles at 1.8x. Sampled in two blocks, every floor lands
// in the burst (271.8ms) and every gc side after it (144ms), and the floor
// reads 128ms ABOVE the command it is subtracted from, though neither process
// changed. Interleaved, both minima come from the settled stretch.
func TestMeasureMarginalOverFloorReadsThroughALoadBurst(t *testing.T) {
	burst := func(tick int) int {
		if tick < 5 {
			return 453
		}
		return 180
	}
	box := loadedBox(60*time.Millisecond, 80*time.Millisecond, burst)
	m := MeasureMarginalOverFloor(100*time.Millisecond, box.floor, box.gcSide)
	if m.Inverted {
		t.Fatalf("a burst that came and went inverted the reading (floor %s, gc side %s over %d samples each, taken in the order %v): a floor of 60ms read above an 80ms gc side",
			m.Floor, m.GCSide, m.Pairs, box.calls)
	}
	if m.Floor != 108*time.Millisecond || m.GCSide != 144*time.Millisecond || m.Marginal != 36*time.Millisecond {
		t.Errorf("reading = floor %s, gc side %s, marginal %s; want 108ms, 144ms and 36ms, the two fixed costs at the settled load", m.Floor, m.GCSide, m.Marginal)
	}
	if m.Pairs != FloorMeasurementMinPairs {
		t.Errorf("took %d pairs, want %d: interleaving alone keeps this burst out of the reading, so nothing should need re-measuring", m.Pairs, FloorMeasurementMinPairs)
	}
}

// TestMeasureMarginalOverFloorRemeasuresAnInvertedReading pins what happens to
// a reading whose floor sits more than the budget above the gc side. It is no
// verdict on gc's side, so it is neither passed nor failed: it is measured
// again, one more pair at a time. Here the first five floor samples come back
// slow for a reason that is not the gc side's, and the sixth does not.
func TestMeasureMarginalOverFloorRemeasuresAnInvertedReading(t *testing.T) {
	box := &floorScript{
		floorAt: func(_, n int) time.Duration {
			if n < FloorMeasurementMinPairs {
				return 300 * time.Millisecond
			}
			return 60 * time.Millisecond
		},
		gcSideAt: func(int, int) time.Duration { return 80 * time.Millisecond },
	}
	m := MeasureMarginalOverFloor(100*time.Millisecond, box.floor, box.gcSide)
	if m.Inverted {
		t.Fatalf("the reading stayed inverted (floor %s, gc side %s after %d samples each): an inverted reading must be measured again, not reported", m.Floor, m.GCSide, m.Pairs)
	}
	if m.Pairs != FloorMeasurementMinPairs+1 {
		t.Errorf("took %d pairs, want %d: one more pair settles this reading", m.Pairs, FloorMeasurementMinPairs+1)
	}
	if m.Floor != 60*time.Millisecond || m.Marginal != 20*time.Millisecond {
		t.Errorf("reading = floor %s, marginal %s; want 60ms and 20ms", m.Floor, m.Marginal)
	}
}

// TestMeasureMarginalOverFloorReportsAPersistentInversion pins the end of the
// re-measurement: a floor that never reads as one is still not a verdict on
// the gc side. After every pair FloorMeasurementMaxPairs allows, the reading
// comes back Inverted with no marginal, for the caller to report as
// inconclusive, never as a pass.
func TestMeasureMarginalOverFloorReportsAPersistentInversion(t *testing.T) {
	box := &floorScript{
		floorAt:  func(int, int) time.Duration { return 300 * time.Millisecond },
		gcSideAt: func(int, int) time.Duration { return 80 * time.Millisecond },
	}
	m := MeasureMarginalOverFloor(100*time.Millisecond, box.floor, box.gcSide)
	if !m.Inverted {
		t.Fatalf("a floor 220ms above the gc side on every sample read as a floor (marginal %s after %d pairs)", m.Marginal, m.Pairs)
	}
	if m.Pairs != FloorMeasurementMaxPairs {
		t.Errorf("took %d pairs, want all %d before giving up on the reading", m.Pairs, FloorMeasurementMaxPairs)
	}
	if m.Marginal != 0 {
		t.Errorf("an inverted reading carried marginal %s; it has none", m.Marginal)
	}
}

// TestMeasureMarginalOverFloorNeverRemeasuresAnOverBudgetReading is the other
// half of not weakening the gate: only an inverted reading is measured again.
// A gc side that really is over budget is a verdict, returned after the first
// FloorMeasurementMinPairs pairs for the caller to fail, not sampled until a
// quiet moment lets it through.
func TestMeasureMarginalOverFloorNeverRemeasuresAnOverBudgetReading(t *testing.T) {
	box := &floorScript{
		floorAt:  func(int, int) time.Duration { return 60 * time.Millisecond },
		gcSideAt: func(int, int) time.Duration { return 400 * time.Millisecond },
	}
	m := MeasureMarginalOverFloor(100*time.Millisecond, box.floor, box.gcSide)
	if m.Inverted {
		t.Fatalf("a gc side 340ms above the floor read as inverted")
	}
	if m.Pairs != FloorMeasurementMinPairs {
		t.Errorf("took %d pairs, want %d: an over-budget reading must not be re-measured", m.Pairs, FloorMeasurementMinPairs)
	}
	if m.Marginal != 340*time.Millisecond {
		t.Errorf("marginal = %s, want 340ms, over the 100ms budget for the caller to fail", m.Marginal)
	}
}

// TestMeasureMarginalOverFloorKeepsTheBestSampleOfEach pins "the minimum": the
// best floor and the best gc side are neither the first nor the last sample
// taken, and every other sample is slower. The best of each is the closest any
// sample got to the fixed cost; the last sample, or the largest, would read the
// noise around it.
func TestMeasureMarginalOverFloorKeepsTheBestSampleOfEach(t *testing.T) {
	floors := []time.Duration{90, 60, 120, 75, 110}
	gcSides := []time.Duration{200, 150, 220, 180, 210}
	box := &floorScript{
		floorAt:  func(_, n int) time.Duration { return floors[n%len(floors)] * time.Millisecond },
		gcSideAt: func(_, n int) time.Duration { return gcSides[n%len(gcSides)] * time.Millisecond },
	}
	m := MeasureMarginalOverFloor(100*time.Millisecond, box.floor, box.gcSide)
	if m.Inverted {
		t.Fatalf("a floor 90ms below the gc side read as inverted (floor %s, gc side %s)", m.Floor, m.GCSide)
	}
	if m.Floor != 60*time.Millisecond || m.GCSide != 150*time.Millisecond || m.Marginal != 90*time.Millisecond {
		t.Errorf("reading = floor %s, gc side %s, marginal %s; want 60ms, 150ms and 90ms, the best sample of each kind", m.Floor, m.GCSide, m.Marginal)
	}
}
