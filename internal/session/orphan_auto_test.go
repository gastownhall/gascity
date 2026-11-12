package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// In a mixed city the manager's provider is the auto router. The pre-start
// orphan sweep must reach through it to the backends' process-table scanners
// and terminate only the untracked roots of the starting session in this city:
// not a root the ACP backend still tracks (even though the default backend,
// scanning the same process table, does not), and not another city's root.
func TestKillExistingOrphansThroughAutoTerminatesOnlyUntrackedSameCityRoots(t *testing.T) {
	const sessionID = "sid-mixed"
	city := t.TempDir()
	otherCity := t.TempDir()

	def, acp := runtime.NewFake(), runtime.NewFake()
	// ACP hosts the live session: its root (Fake pane root pid 4242) is
	// tracked by the ACP backend only.
	if err := acp.Start(context.Background(), "acp-live", runtime.Config{
		Command: "agent",
		Env:     map[string]string{"GC_SESSION_ID": sessionID, "GC_CITY_PATH": city},
	}); err != nil {
		t.Fatalf("Start(acp-live): %v", err)
	}
	liveSeenByDefault := runtime.LiveRuntime{SessionID: sessionID, City: city, PID: 4242}
	orphan := runtime.LiveRuntime{SessionID: sessionID, City: city, PID: 100}
	foreign := runtime.LiveRuntime{SessionID: sessionID, City: otherCity, PID: 200}
	otherSession := runtime.LiveRuntime{SessionID: "sid-other", City: city, PID: 300}
	def.ExtraRuntimes = []runtime.LiveRuntime{liveSeenByDefault, orphan, foreign, otherSession}
	acp.ExtraRuntimes = []runtime.LiveRuntime{orphan, foreign, otherSession}

	mgr := newTestManager(t, beads.NewMemStore(), sessionauto.New(def, acp), WithCityPath(city))
	if err := mgr.killExistingOrphans(context.Background(), sessionID); err != nil {
		t.Fatalf("killExistingOrphans: %v", err)
	}

	var terminated []string
	for _, fake := range []*runtime.Fake{def, acp} {
		for _, c := range fake.Calls {
			if c.Method == "TerminateRuntime" {
				terminated = append(terminated, c.Value)
			}
		}
	}
	if len(terminated) != 1 || terminated[0] != "100" {
		t.Fatalf("terminated pids = %v, want only the untracked same-city orphan 100", terminated)
	}
}

// scanErrorFake is a Fake whose process-table scan fails with err.
type scanErrorFake struct {
	*runtime.Fake
	err error
}

func (f *scanErrorFake) FindRuntimesBySessionID(string) ([]runtime.LiveRuntime, error) {
	return nil, f.err
}

// Every session start runs the pre-start orphan scan, and a scan that cannot
// read dozens of same-uid /proc entries used to log one line per entry each
// time. It logs one bounded summary line instead.
func TestKillExistingOrphansSummarizesScanError(t *testing.T) {
	var entries error
	for pid := 1000; pid < 1070; pid++ {
		entries = errors.Join(entries, &proctable.EntryError{PID: pid, Err: fmt.Errorf("reading environ for pid %d: permission denied", pid)})
	}
	var buf strings.Builder
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	mgr := newTestManager(t, beads.NewMemStore(), &scanErrorFake{Fake: runtime.NewFake(), err: entries})
	if err := mgr.killExistingOrphans(context.Background(), "sid-scan"); err != nil {
		t.Fatalf("killExistingOrphans: %v", err)
	}

	logged := buf.String()
	want := `session: scanning for orphaned runtimes for sid-scan (failing closed): 70 unreadable process entries in 1 classes: 70 like "reading environ for pid 1000: permission denied" (pids 1000, 1001, 1002, ...)` + "\n"
	if logged != want {
		t.Fatalf("logged %q, want one summary line %q", logged, want)
	}
}

// TestKillExistingOrphansPacesScanErrors (ga-zpcphq, PR #8182): a start whose
// pre-start scan hits unreadable entries logged the bounded summary every
// time, and a production supervisor logged the same line thousands of times
// over days. Every start builds a fresh Manager, so only a pacer that outlives
// managers can keep an unchanged-shape error quiet across starts: the line
// appears initially, again when the shape changes, and again after ten
// minutes; new PIDs or counts of the same class wait for the interval, and
// another city's starts report their own errors.
func TestKillExistingOrphansPacesScanErrors(t *testing.T) {
	entryErr := func(reason string, firstPID, lastPID int) error {
		var err error
		for pid := firstPID; pid < lastPID; pid++ {
			err = errors.Join(err, &proctable.EntryError{PID: pid, Err: fmt.Errorf("%s for pid %d: permission denied", reason, pid)})
		}
		return err
	}
	environErr := func() error { return entryErr("reading environ", 1000, 1070) }

	city, otherCity := t.TempDir(), t.TempDir()
	t0 := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	var clk clock.Fake
	clk.Time = t0
	var buf strings.Builder
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	startManager := func(sp runtime.Provider, cityPath string) *Manager {
		return newTestManager(t, beads.NewMemStore(), sp, WithCityPath(cityPath), WithClock(&clk))
	}
	scanErrLine := "session: scanning for orphaned runtimes for sid-scan"
	logCount := func() int { return strings.Count(buf.String(), scanErrLine) }
	run := func(sp runtime.Provider, cityPath string) {
		t.Helper()
		if err := startManager(sp, cityPath).killExistingOrphans(context.Background(), "sid-scan"); err != nil {
			t.Fatalf("killExistingOrphans on %s: %v", cityPath, err)
		}
	}

	failingScans := func(err error) runtime.Provider {
		return &scanErrorFake{Fake: runtime.NewFake(), err: err}
	}

	// The first start reports the unreadable entries.
	run(failingScans(environErr()), city)
	if n := logCount(); n != 1 {
		t.Fatalf("logged %d times after the first start, want 1", n)
	}
	// The same error on a reconstructed manager of the same city stays quiet.
	clk.Advance(time.Minute)
	run(failingScans(environErr()), city)
	if n := logCount(); n != 1 {
		t.Fatalf("logged %d times after an unchanged-shape repeat, want still 1", n)
	}
	// New PIDs and counts of the same class stay quiet too.
	clk.Advance(time.Minute)
	run(failingScans(entryErr("reading environ", 2000, 2071)), city)
	if n := logCount(); n != 1 {
		t.Fatalf("logged %d times after new pids of the same class, want still 1", n)
	}
	// Another city's start reports its own error.
	clk.Advance(time.Minute)
	run(failingScans(environErr()), otherCity)
	if n := logCount(); n != 2 {
		t.Fatalf("logged %d times after another city's start, want 2", n)
	}
	// A new kind of failure reports.
	clk.Advance(time.Minute)
	run(failingScans(fmt.Errorf("reading owner for pid 2100: no such device")), city)
	if n := logCount(); n != 3 {
		t.Fatalf("logged %d times after a shape change, want 3", n)
	}
	clk.Advance(time.Minute)
	run(failingScans(environErr()), city)
	if n := logCount(); n != 4 {
		t.Fatalf("logged %d times after returning to the first shape, want 4", n)
	}
	// The same shape again within the interval stays quiet.
	clk.Advance(time.Minute)
	run(failingScans(environErr()), city)
	if n := logCount(); n != 4 {
		t.Fatalf("logged %d times within the interval, want still 4", n)
	}
	// Once ten minutes have passed (ScanErrorLogRepeat, asserted in
	// proctable's own tests), the same shape reports again.
	clk.Advance(10*time.Minute + time.Second)
	run(failingScans(environErr()), city)
	if n := logCount(); n != 5 {
		t.Fatalf("logged %d times after the repeat interval, want 5", n)
	}
}

// scanRootsFake is a Fake process-table scanner whose scan returns both roots
// and an error, the way a fail-closed partial scan does.
type scanRootsFake struct {
	*runtime.Fake
	roots []runtime.LiveRuntime
	err   error
}

func (f *scanRootsFake) FindRuntimesBySessionID(string) ([]runtime.LiveRuntime, error) {
	return f.roots, f.err
}

// TestKillExistingOrphansSuppressedLogStillTerminatesAndRefuses
// (ga-zpcphq): pacing only quiets the log line. A suppressed start still
// terminates the orphans the incomplete scan surfaced, still refuses over a
// LiveUnlisted root, and a failure of the whole scan is still logged every
// time, not paceable: it can never be mistaken for a quiet scan.
func TestKillExistingOrphansSuppressedLogStillTerminatesAndRefuses(t *testing.T) {
	const sessionID = "sid-term"
	entries := func() error {
		var err error
		for pid := 1000; pid < 1070; pid++ {
			err = errors.Join(err, &proctable.EntryError{PID: pid, Err: fmt.Errorf("reading environ for pid %d: permission denied", pid)})
		}
		return err
	}()
	wholeScanErr := errors.Join(entries, errors.New("enumerating /proc: permission denied"))

	city := t.TempDir()
	t0 := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	var clk clock.Fake
	clk.Time = t0
	var buf strings.Builder
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	orphan := runtime.LiveRuntime{SessionID: sessionID, City: city, PID: 100}
	liveUnlisted := runtime.LiveRuntime{SessionID: sessionID, City: city, PID: 300, LiveUnlisted: true}
	start := func(roots []runtime.LiveRuntime, err error) (*Manager, *runtime.Fake) {
		fake := &scanRootsFake{Fake: runtime.NewFake(), roots: roots, err: err}
		return newTestManager(t, beads.NewMemStore(), fake, WithCityPath(city), WithClock(&clk)), fake.Fake
	}
	scanErrCount := func() int {
		return strings.Count(buf.String(), "session: scanning for orphaned runtimes for "+sessionID)
	}
	terminated := func(fake *runtime.Fake) int {
		n := 0
		for _, c := range fake.Calls {
			if c.Method == "TerminateRuntime" {
				n++
			}
		}
		return n
	}

	// The first start reports the scan and terminates the orphan.
	m1, f1 := start([]runtime.LiveRuntime{orphan}, entries)
	if err := m1.killExistingOrphans(context.Background(), sessionID); err != nil {
		t.Fatalf("killExistingOrphans: %v", err)
	}
	if n := terminated(f1); n != 1 {
		t.Fatalf("terminated %d runtimes, want the orphan", n)
	}
	if n := scanErrCount(); n != 1 {
		t.Fatalf("logged %d times after the first start, want 1", n)
	}

	// A suppressed start still terminates what the scan surfaced.
	clk.Advance(time.Minute)
	m2, f2 := start([]runtime.LiveRuntime{orphan}, entries)
	if err := m2.killExistingOrphans(context.Background(), sessionID); err != nil {
		t.Fatalf("killExistingOrphans: %v", err)
	}
	if n := terminated(f2); n != 1 {
		t.Fatalf("terminated %d runtimes on a suppressed start, want the orphan", n)
	}
	if n := scanErrCount(); n != 1 {
		t.Fatalf("logged %d times on a suppressed start, want still 1", n)
	}

	// A suppressed start still refuses over a LiveUnlisted root: a
	// replacement would race the provider's runtime for the session's work.
	clk.Advance(time.Minute)
	m3, f3 := start([]runtime.LiveRuntime{liveUnlisted}, entries)
	err := m3.killExistingOrphans(context.Background(), sessionID)
	if err == nil || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("killExistingOrphans error = %v; want the LiveUnlisted refusal", err)
	}
	if n := terminated(f3); n != 0 {
		t.Fatalf("terminated %d runtimes over a LiveUnlisted root, want none", n)
	}
	if n := scanErrCount(); n != 1 {
		t.Fatalf("logged %d times over a LiveUnlisted refusal, want the scan line still suppressed", n)
	}
	if !strings.Contains(buf.String(), "refusing to start "+sessionID) {
		t.Fatalf("the refusal line is missing: %q", buf.String())
	}

	// A whole-scan failure is logged every time, entry errors or not.
	clk.Advance(time.Minute)
	m4, f4 := start([]runtime.LiveRuntime{orphan}, wholeScanErr)
	if err := m4.killExistingOrphans(context.Background(), sessionID); err != nil {
		t.Fatalf("killExistingOrphans: %v", err)
	}
	if n := terminated(f4); n != 1 {
		t.Fatalf("terminated %d runtimes during a whole-scan failure, want the surfaced orphan", n)
	}
	if n := scanErrCount(); n != 2 {
		t.Fatalf("logged %d times during a whole-scan failure, want 2", n)
	}
	clk.Advance(time.Minute)
	m5, _ := start(nil, wholeScanErr)
	if err := m5.killExistingOrphans(context.Background(), sessionID); err != nil {
		t.Fatalf("killExistingOrphans: %v", err)
	}
	if n := scanErrCount(); n != 3 {
		t.Fatalf("logged %d times during a repeated whole-scan failure, want 3", n)
	}
}
