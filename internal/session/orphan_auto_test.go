package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
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

// scanErrorFake is a Fake whose process-table scan reports roots and fails
// with err.
type scanErrorFake struct {
	*runtime.Fake
	roots []runtime.LiveRuntime
	err   error
}

func (f *scanErrorFake) FindRuntimesBySessionID(string) ([]runtime.LiveRuntime, error) {
	return slices.Clone(f.roots), f.err
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

// entryScanError is a scan error of one unreadable entry per pid, each
// failing as format (with %d for its pid) describes.
func entryScanError(format string, pids ...int) error {
	var err error
	for _, pid := range pids {
		err = errors.Join(err, &proctable.EntryError{PID: pid, Err: fmt.Errorf(format, pid)})
	}
	return err
}

const scanErrorLine = "session: scanning for orphaned runtimes for "

// TestKillExistingOrphansPacesScanErrors (ga-zpcphq): every start builds a
// new Manager, and an unreadable host helper fails every pre-start scan the
// same way. The scan line is logged per city the first time a shape of entry
// errors is seen, again when the shape changes or ten minutes later, and not
// for unchanged repeats in between.
func TestKillExistingOrphansPacesScanErrors(t *testing.T) {
	logged := captureLog(t)
	city, otherCity := t.TempDir(), t.TempDir()
	t0 := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	perm := "reading environ for pid %d: permission denied"
	owner := "reading owner for pid %d: no such device"
	for _, step := range []struct {
		name string
		city string
		err  error
		at   time.Duration
		want bool
	}{
		{"first", city, entryScanError(perm, 11, 12), 0, true},
		{"same shape, other pids and count", city, entryScanError(perm, 13, 14, 15), time.Minute, false},
		{"another city", otherCity, entryScanError(perm, 11), time.Minute, true},
		{"a new kind", city, entryScanError(owner, 16), 2 * time.Minute, true},
		{"the new kind again", city, entryScanError(owner, 17), 3 * time.Minute, false},
		{"just inside ten minutes", city, entryScanError(owner, 18), 12*time.Minute - time.Nanosecond, false},
		{"ten minutes on", city, entryScanError(owner, 19), 12 * time.Minute, true},
		{"another city's kind again", otherCity, entryScanError(perm, 12), 2 * time.Minute, false},
	} {
		before := logged.Len()
		sp := &scanErrorFake{Fake: runtime.NewFake(), err: step.err}
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(step.city), WithClock(&clock.Fake{Time: t0.Add(step.at)}))
		if err := mgr.killExistingOrphans(context.Background(), "sid-paced"); err != nil {
			t.Fatalf("%s: killExistingOrphans: %v", step.name, err)
		}
		line := logged.String()[before:]
		if got := strings.Contains(line, scanErrorLine); got != step.want {
			t.Fatalf("%s: logged %q; scan line logged = %t, want %t", step.name, line, got, step.want)
		}
	}
}
