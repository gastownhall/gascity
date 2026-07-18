package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// runEventsPath is the file the warm projector folds for a fake-state city.
func runEventsPath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "events.jsonl")
}

// appendRunEventLog appends events to a city's log without truncating it, so a
// test can drive the incremental byte-offset tail (writeRunEventLog rewrites the
// whole file, which the tail would instead treat as a shrink/rotation).
func appendRunEventLog(t *testing.T, cityPath string, evts ...events.Event) {
	t.Helper()
	logPath := runEventsPath(cityPath)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	defer f.Close() //nolint:errcheck
	for _, e := range evts {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatalf("append event: %v", err)
		}
	}
}

// decodeMissEvent is a bead.created event whose payload carries a bead with no
// id, so the projector counts it as a decode miss rather than folding it.
func decodeMissEvent(seq uint64) events.Event {
	return events.Event{Seq: seq, Type: events.BeadCreated, Payload: json.RawMessage(`{"bead":{"title":"no id"}}`)}
}

// beadEventOfType builds a bead lifecycle event of the given type carrying b, so
// a test can drive a bead.updated/closed (not just bead.created) through the tail.
func beadEventOfType(seq uint64, typ string, b beads.Bead) events.Event {
	payload, _ := json.Marshal(struct {
		Bead beads.Bead `json:"bead"`
	}{b})
	return events.Event{Seq: seq, Type: typ, Payload: payload}
}

func runIDs(out *RunsListOutput) []string {
	ids := make([]string, 0, len(out.Body.Runs))
	for _, r := range out.Body.Runs {
		ids = append(ids, r.RunID)
	}
	return ids
}

func hasPartial(out *RunsListOutput, substr string) bool {
	for _, e := range out.Body.PartialErrors {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

func mustRunsList(t *testing.T, s *Server) *RunsListOutput {
	t.Helper()
	out, err := s.humaHandleRunsList(context.Background(), &RunsListInput{CityScope: CityScope{CityName: "test-city"}})
	if err != nil {
		t.Fatalf("humaHandleRunsList error: %v", err)
	}
	return out
}

// TestRunProjectorFirstAccessServesFullForSmallLog is the common case: a small
// log's asynchronous cold replay completes within the bounded first-access wait,
// so the very first request serves the full projection (not a warming partial).
func TestRunProjectorFirstAccessServesFullForSmallLog(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run-a", "mol-adopt-pr-v2", "open")),
	)
	out := mustRunsList(t, s)
	if ids := runIDs(out); len(ids) != 1 || ids[0] != "run-a" {
		t.Fatalf("runs = %v, want [run-a]", ids)
	}
	if out.Body.Partial {
		t.Errorf("Partial = true on a warm small-log read, want false; errors=%v", out.Body.PartialErrors)
	}
	if got := s.runProj.coldLoadCount.Load(); got != 1 {
		t.Errorf("coldLoadCount = %d, want 1 (one async cold replay)", got)
	}
}

// TestRunProjectorFirstAccessWarmingPartial proves the first request does not
// block on a full replay: with the cold replay held open past the bounded wait,
// the request returns promptly with a truthful warming partial, then serves the
// full list once the replay completes.
func TestRunProjectorFirstAccessWarmingPartial(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run-a", "mol-adopt-pr-v2", "open")),
	)

	release := make(chan struct{})
	rp := newRunProjector(runEventsPath(s.state.CityPath()))
	rp.coldLoadWait = 20 * time.Millisecond
	rp.coldLoadRead = func(p string, f events.Filter) ([]events.Event, error) {
		<-release // hold the replay open past coldLoadWait
		return events.ReadFilteredWithInFlight(p, f)
	}
	s.runProj = rp

	start := time.Now()
	out := mustRunsList(t, s)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("first access took %v, want it to return promptly (~coldLoadWait), not block on the full replay", elapsed)
	}
	if len(out.Body.Runs) != 0 {
		t.Errorf("warming read returned %d runs, want 0 while the replay is still in flight", len(out.Body.Runs))
	}
	if !out.Body.Partial || !hasPartial(out, "warming") {
		t.Errorf("warming read Partial=%v errors=%v, want a warming partial", out.Body.Partial, out.Body.PartialErrors)
	}

	close(release)
	<-rp.readyCh // the cold replay has now published

	warm := mustRunsList(t, s)
	if ids := runIDs(warm); len(ids) != 1 || ids[0] != "run-a" {
		t.Fatalf("post-warm runs = %v, want [run-a]", ids)
	}
	if warm.Body.Partial {
		t.Errorf("post-warm Partial = true, want false; errors=%v", warm.Body.PartialErrors)
	}
}

// TestRunProjectorIncrementalAppend proves steady-state reads apply only newly
// appended events via the byte-offset tail — no second full cold replay.
func TestRunProjectorIncrementalAppend(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run-a", "mol-adopt-pr-v2", "open")),
	)

	first := mustRunsList(t, s)
	if ids := runIDs(first); len(ids) != 1 || ids[0] != "run-a" {
		t.Fatalf("first runs = %v, want [run-a]", ids)
	}
	if got := s.runProj.coldLoadCount.Load(); got != 1 {
		t.Fatalf("coldLoadCount = %d after warm-up, want 1", got)
	}

	appendRunEventLog(t, s.state.CityPath(),
		beadCreatedEvent(2, runRootBead("run-b", "mol-design-review-v2", "open")),
	)

	second := mustRunsList(t, s)
	ids := runIDs(second)
	if len(ids) != 2 {
		t.Fatalf("after append runs = %v, want 2 (run-a, run-b)", ids)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if !seen["run-a"] || !seen["run-b"] {
		t.Errorf("after append runs = %v, want both run-a and run-b", ids)
	}
	if got := s.runProj.coldLoadCount.Load(); got != 1 {
		t.Errorf("coldLoadCount = %d after incremental append, want 1 (the tail must not re-cold-load)", got)
	}
}

// TestRunProjectorConcurrentCallersShareOneColdLoad proves concurrent first
// callers collapse onto a single cold replay and all observe the same result.
// Run under -race for the locking.
func TestRunProjectorConcurrentCallersShareOneColdLoad(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run-a", "mol-adopt-pr-v2", "open")),
	)

	release := make(chan struct{})
	rp := newRunProjector(runEventsPath(s.state.CityPath()))
	rp.coldLoadRead = func(p string, f events.Filter) ([]events.Event, error) {
		<-release // keep the single replay in flight until every caller is waiting
		return events.ReadFilteredWithInFlight(p, f)
	}
	s.runProj = rp

	const callers = 16
	var wg sync.WaitGroup
	results := make([][]string, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := s.humaHandleRunsList(context.Background(), &RunsListInput{CityScope: CityScope{CityName: "test-city"}})
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = runIDs(out)
		}(i)
	}

	<-time.After(50 * time.Millisecond) // let all callers reach the shared wait
	close(release)
	wg.Wait()

	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d error: %v", i, errs[i])
		}
		if len(results[i]) != 1 || results[i][0] != "run-a" {
			t.Fatalf("caller %d runs = %v, want [run-a]", i, results[i])
		}
	}
	if got := rp.coldLoadCount.Load(); got != 1 {
		t.Errorf("coldLoadCount = %d, want 1 (all concurrent callers share one cold replay)", got)
	}
}

// TestRunProjectorRotationReset proves a log rotation (a fresh active-file
// identity) triggers a fresh asynchronous cold replay rather than tailing the new
// inode at the stale offset — so the projection rebuilds across the rotated
// archive and the fresh active file without mixing streams or dropping runs.
func TestRunProjectorRotationReset(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run-a", "mol-adopt-pr-v2", "open")),
	)

	if ids := runIDs(mustRunsList(t, s)); len(ids) != 1 || ids[0] != "run-a" {
		t.Fatalf("pre-rotation runs = %v, want [run-a]", ids)
	}
	if got := s.runProj.coldLoadCount.Load(); got != 1 {
		t.Fatalf("coldLoadCount = %d pre-rotation, want 1", got)
	}

	// Rotate like the recorder does: rename the active log to an in-flight
	// rotating-* sibling (still readable by the cold replay's in-flight scan),
	// then create a FRESH active file — a new inode — carrying a new run.
	gcDir := filepath.Join(s.state.CityPath(), ".gc")
	rotating := filepath.Join(gcDir, "events.jsonl.rotating-20260601T120000Z-seq-1-1")
	if err := os.Rename(runEventsPath(s.state.CityPath()), rotating); err != nil {
		t.Fatalf("rotate rename: %v", err)
	}
	writeRunEventLog(t, s.state.CityPath(),
		beadCreatedEvent(2, runRootBead("run-b", "mol-design-review-v2", "open")),
	)

	// The reset replay is asynchronous: poll until it publishes the rebuilt union.
	var got []string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got = runIDs(mustRunsList(t, s))
		if len(got) == 2 {
			break
		}
		<-time.After(10 * time.Millisecond)
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if !seen["run-a"] || !seen["run-b"] {
		t.Fatalf("post-rotation runs = %v, want both run-a (rotated archive) and run-b (fresh active)", got)
	}
	if c := s.runProj.coldLoadCount.Load(); c != 2 {
		t.Errorf("coldLoadCount = %d, want 2 (initial warm-up + one rotation reset)", c)
	}
}

// TestRunProjectorTruncationReset proves a truncation (the active file shrinks
// below the tail cursor on the SAME identity) triggers a rebuild rather than a
// rewind-and-tail, so a stale cursor can never splice the old projection onto the
// new, smaller stream.
func TestRunProjectorTruncationReset(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run-a", "mol-adopt-pr-v2", "open")),
		beadCreatedEvent(2, runRootBead("run-b", "mol-design-review-v2", "open")),
	)
	if ids := runIDs(mustRunsList(t, s)); len(ids) != 2 {
		t.Fatalf("pre-truncation runs = %v, want run-a and run-b", ids)
	}

	// Rewrite the log in place (same inode) with strictly less content, so the
	// warm tail cursor now points past EOF.
	writeRunEventLog(t, s.state.CityPath(),
		beadCreatedEvent(1, runRootBead("run-a", "mol-adopt-pr-v2", "open")),
	)

	var got []string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got = runIDs(mustRunsList(t, s))
		if len(got) == 1 {
			break
		}
		<-time.After(10 * time.Millisecond)
	}
	if len(got) != 1 || got[0] != "run-a" {
		t.Fatalf("post-truncation runs = %v, want just [run-a] (the rebuild must reflect the truncated log)", got)
	}
	if c := s.runProj.coldLoadCount.Load(); c != 2 {
		t.Errorf("coldLoadCount = %d, want 2 (warm-up + one truncation reset)", c)
	}
}

// TestRunProjectorDecodeMissPartial proves the decode-miss signal survives the
// warm projection: a bead.* event that fails to decode is counted and surfaced as
// a partial rather than silently dropping the view to empty.
func TestRunProjectorDecodeMissPartial(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run-a", "mol-adopt-pr-v2", "open")),
		decodeMissEvent(2),
	)
	out := mustRunsList(t, s)
	if ids := runIDs(out); len(ids) != 1 || ids[0] != "run-a" {
		t.Fatalf("runs = %v, want [run-a] (the good run still folds)", ids)
	}
	if !out.Body.Partial || !hasPartial(out, "could not be decoded") {
		t.Errorf("Partial=%v errors=%v, want a decode-miss partial", out.Body.Partial, out.Body.PartialErrors)
	}
}

// TestRunProjectorTailDecodeMissSurfaced proves a decode miss arriving via the
// incremental tail (not just the cold replay) still surfaces as partial. Apply
// reports changed=false for a decode-miss-only batch, so publishing only on a
// fold change would strand the miss — and since the offset already advanced past
// it, no later poll re-reads it. This is the silent-projection-starve signal the
// endpoint must keep observable.
func TestRunProjectorTailDecodeMissSurfaced(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run-a", "mol-adopt-pr-v2", "open")),
	)
	if out := mustRunsList(t, s); out.Body.Partial {
		t.Fatalf("warm-up Partial=true, want a clean projection; errors=%v", out.Body.PartialErrors)
	}

	appendRunEventLog(t, s.state.CityPath(), decodeMissEvent(2))

	out := mustRunsList(t, s)
	if !out.Body.Partial || !hasPartial(out, "could not be decoded") {
		t.Fatalf("after tail decode-miss Partial=%v errors=%v, want a decode-miss partial", out.Body.Partial, out.Body.PartialErrors)
	}
}

// TestRunProjectorTailReadsFromByteOffset proves the steady-state tail resumes at
// the byte offset (O(delta)) rather than re-scanning the whole log from zero:
// seq-dedup would mask a full re-read, so this guards the projector's core reason
// for existing against a silent regression.
func TestRunProjectorTailReadsFromByteOffset(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run-a", "mol-adopt-pr-v2", "open")),
	)
	var maxOffset atomic.Int64
	rp := newRunProjector(runEventsPath(s.state.CityPath()))
	realTail := rp.tailRead
	rp.tailRead = func(p string, off int64) ([]events.Event, int64, error) {
		for {
			cur := maxOffset.Load()
			if off <= cur || maxOffset.CompareAndSwap(cur, off) {
				break
			}
		}
		return realTail(p, off)
	}
	s.runProj = rp

	mustRunsList(t, s) // warm
	appendRunEventLog(t, s.state.CityPath(),
		beadCreatedEvent(2, runRootBead("run-b", "mol-design-review-v2", "open")),
	)
	if ids := runIDs(mustRunsList(t, s)); len(ids) != 2 {
		t.Fatalf("after append runs = %v, want run-a and run-b", ids)
	}
	if maxOffset.Load() == 0 {
		t.Fatal("tail always read from offset 0 — it must resume at the byte offset, not re-scan the whole log")
	}
}

// TestRunProjectorTailAppliesStatusTransition proves the tail applies bead
// lifecycle deltas beyond creation: a run's root closing (bead.closed) flows
// through the incremental tail and flips the run's status, with no re-cold-load.
func TestRunProjectorTailAppliesStatusTransition(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run-a", "mol-adopt-pr-v2", "open")),
	)
	first := mustRunsList(t, s)
	if len(first.Body.Runs) != 1 || first.Body.Runs[0].Status != RunStatusPending {
		t.Fatalf("initial run = %+v, want one pending run", first.Body.Runs)
	}

	closedRoot := runRootBead("run-a", "mol-adopt-pr-v2", "closed")
	closedRoot.Metadata["gc.outcome"] = "pass"
	appendRunEventLog(t, s.state.CityPath(), beadEventOfType(2, events.BeadClosed, closedRoot))

	second := mustRunsList(t, s)
	if len(second.Body.Runs) != 1 || second.Body.Runs[0].Status != RunStatusCompleted {
		t.Fatalf("after close run = %+v, want one completed run (the tail must apply the close delta)", second.Body.Runs)
	}
	if got := s.runProj.coldLoadCount.Load(); got != 1 {
		t.Errorf("coldLoadCount = %d, want 1 (a status transition tails, it does not re-cold-load)", got)
	}
}

// TestRunProjectorFirstLoadErrorIs503 proves a cold-replay failure with no
// snapshot yet surfaces as a retryable 503, preserving the pre-warm contract.
func TestRunProjectorFirstLoadErrorIs503(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run-a", "mol-adopt-pr-v2", "open")),
	)

	rp := newRunProjector(runEventsPath(s.state.CityPath()))
	rp.coldLoadRead = func(string, events.Filter) ([]events.Event, error) {
		return nil, errors.New("boom reading events")
	}
	s.runProj = rp

	_, err := s.humaHandleRunsList(context.Background(), &RunsListInput{CityScope: CityScope{CityName: "test-city"}})
	if err == nil {
		t.Fatal("humaHandleRunsList = nil error, want a 503 on cold-load failure")
	}
	if !strings.Contains(err.Error(), "run projection unavailable") {
		t.Errorf("error = %q, want run-projection-unavailable (503)", err.Error())
	}
}

// TestRunProjectorRetriesAfterColdLoadFailure proves a first-load failure is not
// sticky: a later request re-attempts the cold replay (the pre-warm path re-read
// every request, so a one-shot warm-up that pinned the 503 would regress that),
// and once the replay succeeds the endpoint serves the run.
func TestRunProjectorRetriesAfterColdLoadFailure(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run-a", "mol-adopt-pr-v2", "open")),
	)

	var calls atomic.Int64
	rp := newRunProjector(runEventsPath(s.state.CityPath()))
	rp.coldLoadRead = func(p string, f events.Filter) ([]events.Event, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("boom reading events")
		}
		return events.ReadFilteredWithInFlight(p, f)
	}
	s.runProj = rp

	// First request: the cold replay failed, so a retryable 503.
	if _, err := s.humaHandleRunsList(context.Background(), &RunsListInput{CityScope: CityScope{CityName: "test-city"}}); err == nil {
		t.Fatal("first request = nil error, want a 503 on the initial cold-load failure")
	}

	// Later requests re-attempt the replay; once it succeeds the run appears.
	var got []string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, err := s.humaHandleRunsList(context.Background(), &RunsListInput{CityScope: CityScope{CityName: "test-city"}})
		if err == nil {
			got = runIDs(out)
			if len(got) == 1 {
				break
			}
		}
		<-time.After(10 * time.Millisecond)
	}
	if len(got) != 1 || got[0] != "run-a" {
		t.Fatalf("after retry runs = %v, want [run-a] (a failed cold load must recover)", got)
	}
}

// TestRunProjectorPostWarmTailErrorKeepsLastGood proves a tail read error after
// the projection is warm neither errors the request nor corrupts the last-good
// snapshot: the prior runs are still served, and a later successful tail recovers.
func TestRunProjectorPostWarmTailErrorKeepsLastGood(t *testing.T) {
	s := newRunServer(t,
		beadCreatedEvent(1, runRootBead("run-a", "mol-adopt-pr-v2", "open")),
	)
	// Warm via the real reader path, then swap in a failing tail.
	if ids := runIDs(mustRunsList(t, s)); len(ids) != 1 {
		t.Fatalf("warm-up runs = %v, want [run-a]", ids)
	}

	realTail := s.runProj.tailRead
	s.runProj.tailRead = func(_ string, offset int64) ([]events.Event, int64, error) {
		return nil, offset, errors.New("boom tailing events")
	}

	appendRunEventLog(t, s.state.CityPath(),
		beadCreatedEvent(2, runRootBead("run-b", "mol-design-review-v2", "open")),
	)

	out := mustRunsList(t, s) // tail errors: last-good must remain, no error
	if ids := runIDs(out); len(ids) != 1 || ids[0] != "run-a" {
		t.Fatalf("during tail error runs = %v, want last-good [run-a]", ids)
	}

	// Recovery: a working tail then folds the appended run.
	s.runProj.tailRead = realTail
	recovered := mustRunsList(t, s)
	if ids := runIDs(recovered); len(ids) != 2 {
		t.Fatalf("after tail recovery runs = %v, want run-a and run-b", ids)
	}
}
