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
