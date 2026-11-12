package session

import (
	"bytes"
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

// scanErrorFake is a Fake whose process-table scan returns roots, afresh on
// every call, together with err.
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

// permissionDeniedEntries is a scan error of unreadable environs at pids.
func permissionDeniedEntries(pids ...int) error {
	var err error
	for _, pid := range pids {
		err = errors.Join(err, &proctable.EntryError{PID: pid, Err: fmt.Errorf("reading environ for pid %d: open /proc/%d/environ: permission denied", pid, pid)})
	}
	return err
}

// pacedStart runs one pre-start orphan scan on a newly built Manager for city
// at now, whose provider's scan returns root and scanErr. It returns how many
// scan-error lines it logged, the pids it terminated, and killExistingOrphans'
// error.
func pacedStart(t *testing.T, logged *bytes.Buffer, clk *clock.Fake, city string, now time.Time, root runtime.LiveRuntime, scanErr error) (int, []string, error) {
	t.Helper()
	const line = "session: scanning for orphaned runtimes"
	clk.Time = now
	sp := &scanErrorFake{Fake: runtime.NewFake(), roots: []runtime.LiveRuntime{root}, err: scanErr}
	mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city), WithClock(clk))
	before := strings.Count(logged.String(), line)
	err := mgr.killExistingOrphans(context.Background(), root.SessionID)
	var terminated []string
	for _, c := range sp.Calls {
		if c.Method == "TerminateRuntime" {
			terminated = append(terminated, c.Value)
		}
	}
	return strings.Count(logged.String(), line) - before, terminated, err
}

// AC-5: every start builds a new Manager, yet the pre-start orphan scan's
// error line is paced per city across them: logged first, on a new shape and
// once ScanErrorLogRepeat has passed, and suppressed for an unchanged shape in
// between, whatever its pids and counts.
func TestKillExistingOrphansPacesScanErrors(t *testing.T) {
	logged := captureLog(t)
	city, otherCity := t.TempDir(), t.TempDir()
	clk := &clock.Fake{}
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	ownerGone := &proctable.EntryError{PID: 31, Err: errors.New("reading owner for pid 31: open /proc/31/status: no such device")}

	for _, step := range []struct {
		name    string
		city    string
		err     error
		at      time.Time
		wantLog int
	}{
		{"first", city, permissionDeniedEntries(11, 12), t0, 1},
		{"same shape, other pids and count", city, permissionDeniedEntries(13, 14, 15), t0.Add(time.Minute), 0},
		{"another city", otherCity, permissionDeniedEntries(11, 12), t0.Add(time.Minute), 1},
		{"a new kind", city, errors.Join(permissionDeniedEntries(11), ownerGone), t0.Add(2 * time.Minute), 1},
		{"the new kind again", city, errors.Join(permissionDeniedEntries(12), ownerGone), t0.Add(3 * time.Minute), 0},
		{"just inside the repeat window", city, errors.Join(permissionDeniedEntries(12), ownerGone), t0.Add(2*time.Minute + proctable.ScanErrorLogRepeat - time.Second), 0},
		{"at the end of the repeat window", city, errors.Join(permissionDeniedEntries(12), ownerGone), t0.Add(2*time.Minute + proctable.ScanErrorLogRepeat), 1},
	} {
		root := runtime.LiveRuntime{SessionID: "sid-paced", City: step.city, PID: 100, PPID: 1}
		if n, _, _ := pacedStart(t, logged, clk, step.city, step.at, root, step.err); n != step.wantLog {
			t.Fatalf("%s: logged the scan error %d times, want %d; log=%q", step.name, n, step.wantLog, logged.String())
		}
	}
}

// AC-6: pacing changes only the log line. A start whose scan error is
// suppressed still terminates the orphan the scan found, and still refuses,
// terminating nothing, when the provider claims the root as LiveUnlisted; a
// failure of the whole scan, alone or joined to entry failures, is logged on
// every start.
func TestKillExistingOrphansPacingKeepsTerminationAndRefusal(t *testing.T) {
	logged := captureLog(t)
	city := t.TempDir()
	clk := &clock.Fake{}
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	whole := errors.New("tmux backend: listing sessions: server exited unexpectedly")
	orphan := runtime.LiveRuntime{SessionID: "sid-kept", City: city, PID: 100, PPID: 1}
	unlisted := orphan
	unlisted.LiveUnlisted = true

	for _, step := range []struct {
		name    string
		root    runtime.LiveRuntime
		err     error
		at      time.Time
		wantLog int
	}{
		{"first, orphan", orphan, permissionDeniedEntries(11), t0, 1},
		{"suppressed, orphan", orphan, permissionDeniedEntries(12), t0.Add(time.Second), 0},
		{"suppressed, LiveUnlisted", unlisted, permissionDeniedEntries(13), t0.Add(2 * time.Second), 0},
		{"whole-scan failure, orphan", orphan, whole, t0.Add(3 * time.Second), 1},
		{"whole-scan failure again, orphan", orphan, whole, t0.Add(4 * time.Second), 1},
		{"joined whole-scan failure, LiveUnlisted", unlisted, errors.Join(whole, permissionDeniedEntries(11)), t0.Add(5 * time.Second), 1},
		{"joined whole-scan failure again, LiveUnlisted", unlisted, errors.Join(whole, permissionDeniedEntries(11)), t0.Add(6 * time.Second), 1},
	} {
		n, terminated, err := pacedStart(t, logged, clk, city, step.at, step.root, step.err)
		if n != step.wantLog {
			t.Fatalf("%s: logged the scan error %d times, want %d; log=%q", step.name, n, step.wantLog, logged.String())
		}
		if step.root.LiveUnlisted {
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) || len(terminated) != 0 {
				t.Fatalf("%s: killExistingOrphans = %v, terminated %v; want the LiveUnlisted refusal and nothing terminated", step.name, err, terminated)
			}
			continue
		}
		if err != nil || !slices.Equal(terminated, []string{"100"}) {
			t.Fatalf("%s: killExistingOrphans = %v, terminated %v; want orphan pid 100 terminated", step.name, err, terminated)
		}
	}
}
