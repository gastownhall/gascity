package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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

	mgr := NewManagerWithOptions(beads.NewMemStore(), sessionauto.New(def, acp), WithCityPath(city))
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

	mgr := NewManagerWithOptions(beads.NewMemStore(), &scanErrorFake{Fake: runtime.NewFake(), err: entries})
	if err := mgr.killExistingOrphans(context.Background(), "sid-scan"); err != nil {
		t.Fatalf("killExistingOrphans: %v", err)
	}

	logged := buf.String()
	want := `session: scanning for orphaned runtimes for sid-scan (failing closed): 70 unreadable process entries in 1 classes: 70 like "reading environ for pid 1000: permission denied" (pids 1000, 1001, 1002, ...)` + "\n"
	if logged != want {
		t.Fatalf("logged %q, want one summary line %q", logged, want)
	}
}
