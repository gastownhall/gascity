package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// answeringScanFake is a Fake whose process-table scan answers found with
// scanErr at once, and whose terminations run onTerminate.
type answeringScanFake struct {
	*runtime.Fake
	found       []runtime.LiveRuntime
	scanErr     error
	onTerminate func(runtime.LiveRuntime) error
	terminated  []runtime.LiveRuntime
}

func (f *answeringScanFake) FindRuntimesBySessionID(string) ([]runtime.LiveRuntime, error) {
	return f.found, f.scanErr
}

func (f *answeringScanFake) TerminateRuntime(live runtime.LiveRuntime) error {
	f.terminated = append(f.terminated, live)
	if f.onTerminate == nil {
		return nil
	}
	return f.onTerminate(live)
}

func (f *answeringScanFake) terminatedPIDs() []int {
	pids := make([]int, 0, len(f.terminated))
	for _, live := range f.terminated {
		pids = append(pids, live.PID)
	}
	return pids
}

// sameCityOrphans are three untracked roots of sessionID in city.
func sameCityOrphans(city, sessionID string) []runtime.LiveRuntime {
	return []runtime.LiveRuntime{
		{SessionID: sessionID, City: city, PID: 101},
		{SessionID: sessionID, City: city, PID: 102},
		{SessionID: sessionID, City: city, PID: 103},
	}
}

// A caller that cancels while the orphans are being terminated starts no
// further termination, and its error keeps the terminations that had already
// failed alongside the cancellation.
func TestKillExistingOrphansStartsNoTerminationAfterTheCallerCancels(t *testing.T) {
	const sessionID = "sid-cancel-terminating"
	city := t.TempDir()
	firstFailure := errors.New("first orphan survived SIGKILL")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sp := &answeringScanFake{
		Fake:  runtime.NewFake(),
		found: sameCityOrphans(city, sessionID),
		onTerminate: func(runtime.LiveRuntime) error {
			cancel()
			return firstFailure
		},
	}
	mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))

	err := mgr.killExistingOrphans(ctx, sessionID)

	if got := sp.terminatedPIDs(); len(got) != 1 || got[0] != 101 {
		t.Fatalf("terminated pids = %v, want only 101, the one begun before the cancel", got)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cleanup error = %v, want one wrapping context.Canceled", err)
	}
	if !errors.Is(err, firstFailure) {
		t.Errorf("cleanup error = %v, want it to keep the earlier termination failure", err)
	}
}

// A completed scan that answers alongside a scan error still has its roots
// fenced (tracked, another session's, another city's) and terminates the rest;
// a termination that fails refuses the start as an unconfirmed orphan, not as
// an unavailable scan.
func TestKillExistingOrphansFencesACompletedScanThatReportedAnError(t *testing.T) {
	const sessionID = "sid-completed"
	city := t.TempDir()
	orphan := runtime.LiveRuntime{SessionID: sessionID, City: city, PID: 101}
	sp := &answeringScanFake{
		Fake: runtime.NewFake(),
		found: []runtime.LiveRuntime{
			{SessionID: sessionID, City: city, PID: 100, IsTracked: true},
			{SessionID: "sid-other", City: city, PID: 200},
			{SessionID: sessionID, City: t.TempDir(), PID: 300},
			orphan,
		},
		scanErr:     errors.New("reading environ for pid 999: permission denied"),
		onTerminate: func(runtime.LiveRuntime) error { return errors.New("still alive") },
	}
	mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))

	err := mgr.killExistingOrphans(context.Background(), sessionID)

	if got := sp.terminatedPIDs(); len(got) != 1 || got[0] != orphan.PID {
		t.Fatalf("terminated pids = %v, want only the unfenced orphan %d", got, orphan.PID)
	}
	if err == nil || !strings.Contains(err.Error(), "1 orphaned runtime(s) not confirmed dead") {
		t.Fatalf("cleanup error = %v, want the unconfirmed orphan reported", err)
	}
	if errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Errorf("cleanup error = %v, want an unconfirmed orphan, not an unavailable scan", err)
	}
}
