package arming

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

func liveArm(template string, armedAt, expiresAt time.Time) Arm {
	return Arm{
		ScopeType:  "template",
		ScopeValue: template,
		Source:     "manual",
		Level:      "detail",
		ArmedAt:    armedAt,
		ExpiresAt:  expiresAt,
	}
}

func boundaryOf(at time.Time, arms ...Arm) Boundary {
	return Boundary{At: at, Observations: observeArms([]string{"alpha"}, Status{ActiveArms: arms})}
}

func TestTemplatesNeedingArmFindsMissingExpiredAndExpiringArms(t *testing.T) {
	status := Status{ActiveArms: []Arm{
		liveArm("healthy", base.Add(-time.Minute), base.Add(time.Hour)),
		liveArm("expiring", base.Add(-14*time.Minute), base.Add(time.Minute)),
		liveArm("expired", base.Add(-30*time.Minute), base.Add(-time.Minute)),
		{ScopeType: "template", ScopeValue: "baseline-only", Source: "manual", Level: "baseline", ArmedAt: base, ExpiresAt: base.Add(time.Hour)},
	}}
	templates := []string{"healthy", "expiring", "expired", "baseline-only", "missing"}

	need := TemplatesNeedingArm(templates, status, base, base.Add(10*time.Minute))

	want := []string{"expiring", "expired", "baseline-only", "missing"}
	if strings.Join(need, ",") != strings.Join(want, ",") {
		t.Fatalf("TemplatesNeedingArm = %v, want %v", need, want)
	}
}

func TestCoverageGapsAcceptsAContinuousArm(t *testing.T) {
	prev := boundaryOf(base, liveArm("alpha", base.Add(-time.Hour), base.Add(30*time.Minute)))
	cur := boundaryOf(base.Add(10*time.Minute), liveArm("alpha", base.Add(-time.Hour), base.Add(40*time.Minute)))

	if gaps := CoverageGaps([]string{"alpha"}, prev, cur); len(gaps) != 0 {
		t.Fatalf("CoverageGaps = %+v, want none", gaps)
	}
}

func TestCoverageGapsFlagsAnArmThatExpiredInsideTheInterval(t *testing.T) {
	prev := boundaryOf(base, liveArm("alpha", base.Add(-time.Hour), base.Add(2*time.Minute)))
	cur := boundaryOf(base.Add(10*time.Minute), liveArm("alpha", base.Add(-time.Hour), base.Add(40*time.Minute)))

	gaps := CoverageGaps([]string{"alpha"}, prev, cur)
	if len(gaps) != 1 || gaps[0].Reason != GapExpiredInsideInterval {
		t.Fatalf("CoverageGaps = %+v, want one %s gap", gaps, GapExpiredInsideInterval)
	}
}

func TestCoverageGapsFlagsAnArmRecreatedInsideTheInterval(t *testing.T) {
	prev := boundaryOf(base, liveArm("alpha", base.Add(-time.Hour), base.Add(30*time.Minute)))
	cur := boundaryOf(base.Add(10*time.Minute), liveArm("alpha", base.Add(5*time.Minute), base.Add(40*time.Minute)))

	gaps := CoverageGaps([]string{"alpha"}, prev, cur)
	if len(gaps) != 1 || gaps[0].Reason != GapRearmedInsideInterval {
		t.Fatalf("CoverageGaps = %+v, want one %s gap", gaps, GapRearmedInsideInterval)
	}
}

func TestCoverageGapsFlagsAWindowThatRanUnarmed(t *testing.T) {
	prev := Boundary{At: base, Observations: map[string]Observation{}}
	cur := Boundary{At: base.Add(10 * time.Minute), Observations: map[string]Observation{}}

	gaps := CoverageGaps([]string{"alpha"}, prev, cur)
	if len(gaps) != 1 || gaps[0].Reason != GapUnarmedAtOpen {
		t.Fatalf("CoverageGaps = %+v, want one %s gap", gaps, GapUnarmedAtOpen)
	}
}

// fakeGC records invocations and replays a scripted arm store.
type fakeGC struct {
	arms  map[string]Arm
	calls []string
	now   func() time.Time
	fail  map[string]error
	// deaf drops arm writes so the harness sees an arming attempt that never took.
	deaf bool
}

func newFakeGC(now func() time.Time) *fakeGC {
	return &fakeGC{arms: map[string]Arm{}, now: now, fail: map[string]error{}}
}

func (f *fakeGC) run(_ context.Context, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	f.calls = append(f.calls, joined)
	if err := f.fail[args[0]+" "+args[1]]; err != nil {
		return nil, err
	}
	switch {
	case strings.Contains(joined, "agent list"):
		return json.Marshal(agentList{Agents: []agentListItem{{Name: "alpha"}, {Name: "beta"}}})
	case strings.Contains(joined, "trace start"):
		if f.deaf {
			return []byte("armed\n"), nil
		}
		template := flagValue(args, "--template")
		window, err := time.ParseDuration(flagValue(args, "--for"))
		if err != nil {
			return nil, err
		}
		existing, ok := f.arms[template]
		armedAt := f.now()
		if ok {
			armedAt = existing.ArmedAt
		}
		f.arms[template] = liveArm(template, armedAt, f.now().Add(window))
		return []byte("armed\n"), nil
	case strings.Contains(joined, "trace status"):
		status := Status{AsOf: f.now()}
		for _, template := range []string{"alpha", "beta"} {
			if arm, ok := f.arms[template]; ok {
				status.ActiveArms = append(status.ActiveArms, arm)
			}
		}
		return json.Marshal(status)
	}
	return nil, fmt.Errorf("fake gc: unexpected args %q", joined)
}

func flagValue(args []string, name string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func testHarness(t *testing.T, clock *time.Time, fake *fakeGC) *Harness {
	t.Helper()
	h, err := New(Config{
		CityPath: "/tmp/city",
		Window:   30 * time.Minute,
		Interval: 10 * time.Minute,
		Lead:     2 * time.Minute,
	}, fake.run, func() time.Time { return *clock })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.templates = []string{"alpha", "beta"}
	return h
}

func TestNewRejectsAnArmWindowThatCannotCoverASample(t *testing.T) {
	_, err := New(Config{CityPath: "/c", Window: 5 * time.Minute, Interval: 10 * time.Minute, Lead: time.Minute}, nil, nil)
	if err == nil {
		t.Fatalf("New accepted a 5m arm window for a 10m sample interval")
	}
}

func TestDiscoverTemplatesReadsAgentList(t *testing.T) {
	clock := base
	fake := newFakeGC(func() time.Time { return clock })
	h := testHarness(t, &clock, fake)
	h.templates = nil

	templates, err := h.DiscoverTemplates(context.Background())
	if err != nil {
		t.Fatalf("DiscoverTemplates: %v", err)
	}
	if strings.Join(templates, ",") != "alpha,beta" {
		t.Fatalf("DiscoverTemplates = %v, want [alpha beta]", templates)
	}
}

// The harness must detect an expired arm at a sample boundary and re-arm before
// the cycle is counted.
func TestSampleRearmsAnExpiringArmBeforeTheBoundaryIsCounted(t *testing.T) {
	clock := base
	fake := newFakeGC(func() time.Time { return clock })
	h := testHarness(t, &clock, fake)
	fake.arms["alpha"] = liveArm("alpha", base.Add(-25*time.Minute), base.Add(time.Minute))
	fake.arms["beta"] = liveArm("beta", base.Add(-time.Minute), base.Add(29*time.Minute))

	boundary, gaps, err := h.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(gaps) != 0 {
		t.Fatalf("first boundary reported gaps: %+v", gaps)
	}
	alpha := boundary.Observations["alpha"]
	if !alpha.Live || !alpha.ExpiresAt.After(base.Add(h.cfg.Interval)) {
		t.Fatalf("alpha was not re-armed past the next boundary: %+v", alpha)
	}
	if alpha.ArmedAt != base.Add(-25*time.Minute) {
		t.Fatalf("re-arm reset armed_at to %s; extension must preserve it", alpha.ArmedAt)
	}
	if h.Report().Rearms != 1 {
		t.Fatalf("rearms = %d, want 1 (beta had enough life left)", h.Report().Rearms)
	}
}

func TestSampleFailsLoudlyWhenArmingDoesNotTake(t *testing.T) {
	clock := base
	fake := newFakeGC(func() time.Time { return clock })
	fake.deaf = true
	h := testHarness(t, &clock, fake)

	if _, _, err := h.Sample(context.Background()); err == nil {
		t.Fatalf("Sample returned success while every arm write was dropped")
	}
}

func TestSampleSurfacesTraceStartFailures(t *testing.T) {
	clock := base
	fake := newFakeGC(func() time.Time { return clock })
	fake.fail["trace start"] = errors.New("controller refused")
	h := testHarness(t, &clock, fake)

	_, _, err := h.Sample(context.Background())
	if err == nil || !strings.Contains(err.Error(), "controller refused") {
		t.Fatalf("Sample error = %v, want the trace start failure surfaced", err)
	}
}

// Two consecutive samples over an arm that never lapses produce a clean report;
// a lapse between them marks the window unarmed.
func TestReportMarksTheWindowUnarmedWhenACycleRanWithoutAnArm(t *testing.T) {
	clock := base
	fake := newFakeGC(func() time.Time { return clock })
	h := testHarness(t, &clock, fake)

	if _, _, err := h.Sample(context.Background()); err != nil {
		t.Fatalf("first Sample: %v", err)
	}
	if !h.Report().Armed {
		t.Fatalf("report is unarmed after a clean first boundary: %+v", h.Report())
	}

	// The arm store loses alpha's arm mid-interval; the next sample re-creates it.
	clock = base.Add(10 * time.Minute)
	delete(fake.arms, "alpha")
	if _, gaps, err := h.Sample(context.Background()); err != nil {
		t.Fatalf("second Sample: %v", err)
	} else if len(gaps) != 1 || gaps[0].Template != "alpha" || gaps[0].Reason != GapRearmedInsideInterval {
		t.Fatalf("gaps = %+v, want one alpha %s gap", gaps, GapRearmedInsideInterval)
	}

	report := h.Report()
	if report.Armed {
		t.Fatalf("report claims an armed window despite a gap: %+v", report)
	}
	if report.Boundaries != 2 || len(report.Gaps) != 1 {
		t.Fatalf("report = %+v, want 2 boundaries and 1 gap", report)
	}
}

func TestRunSamplesUntilTheWindowClosesAndStaysArmed(t *testing.T) {
	clock := base
	fake := newFakeGC(func() time.Time { return clock })
	h := testHarness(t, &clock, fake)
	h.sleep = func(_ context.Context, d time.Duration) error {
		clock = clock.Add(d)
		return nil
	}

	report, err := h.Run(context.Background(), 30*time.Minute)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Boundaries != 4 {
		t.Fatalf("boundaries = %d, want 4 (open plus three 10m samples)", report.Boundaries)
	}
	if !report.Armed || len(report.Gaps) != 0 {
		t.Fatalf("report = %+v, want a clean armed window", report)
	}
}
