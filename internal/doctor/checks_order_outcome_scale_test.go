package doctor

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/events"
)

// writeOrderOutcomeScaleLog writes an events.jsonl of roughly noiseLines
// unrelated bead events (padded to ~1.4 KB like a live log) with the wanted
// outcome and controller-start events interleaved through it. Returns the path.
func writeOrderOutcomeScaleLog(tb testing.TB, cityPath string, noiseLines int, wanted []events.Event) string {
	tb.Helper()
	path := filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tb.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test fixture
	w := bufio.NewWriterSize(f, 1<<20)

	ts := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	padding := make([]byte, 1200)
	for i := range padding {
		padding[i] = 'x'
	}
	every := noiseLines / (len(wanted) + 1)
	if every == 0 {
		every = 1
	}
	next := 0
	seq := uint64(0)
	for i := 0; i < noiseLines; i++ {
		seq++
		if _, err := fmt.Fprintf(w, `{"seq":%d,"type":"bead.updated","ts":%q,"actor":"a","subject":"gc-%d","message":"%s"}`+"\n", seq, ts, i, padding); err != nil {
			tb.Fatal(err)
		}
		if i%every == 0 && next < len(wanted) {
			e := wanted[next]
			next++
			seq++
			if _, err := fmt.Fprintf(w, `{"seq":%d,"type":%q,"ts":%q,"actor":"controller","subject":%q,"message":%q}`+"\n",
				seq, e.Type, e.Ts.UTC().Format(time.RFC3339Nano), e.Subject, e.Message); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := w.Flush(); err != nil {
		tb.Fatal(err)
	}
	return path
}

func TestReadOrderEventsSplitsAndGroupsInOnePass(t *testing.T) {
	base := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	cityPath := t.TempDir()
	path := writeOrderOutcomeScaleLog(t, cityPath, 40, []events.Event{
		{Type: events.ControllerStarted, Ts: base, Subject: "gc"},
		{Type: events.OrderCompleted, Ts: base.Add(time.Minute), Subject: "a"},
		{Type: events.OrderFailed, Ts: base.Add(2 * time.Minute), Subject: "b", Message: "boom"},
		{Type: events.OrderFailed, Ts: base.Add(3 * time.Minute), Subject: "a", Message: "bang"},
		{Type: events.ControllerStarted, Ts: base.Add(4 * time.Minute), Subject: "gc"},
	})

	got, err := readOrderOutcomeLog(nil, path)
	if err != nil {
		t.Fatalf("readOrderOutcomeLog: %v", err)
	}

	if len(got.starts) != 2 || !got.starts[0].Equal(base) || !got.starts[1].Equal(base.Add(4*time.Minute)) {
		t.Fatalf("starts = %v, want both controller starts in log order", got.starts)
	}
	if n := len(got.bySubject["a"]); n != 2 {
		t.Fatalf("subject a outcomes = %d, want 2", n)
	}
	if n := len(got.bySubject["b"]); n != 1 {
		t.Fatalf("subject b outcomes = %d, want 1", n)
	}
	a := got.bySubject["a"]
	if a[0].Type != events.OrderCompleted || a[1].Type != events.OrderFailed || a[0].Seq >= a[1].Seq {
		t.Fatalf("subject a outcomes must be seq-ordered, got %+v", a)
	}
	if _, ok := got.bySubject["gc"]; ok {
		t.Fatal("controller.started must not be treated as an order outcome")
	}
}

func TestReadOrderEventsAbortsWhenCheckAbandoned(t *testing.T) {
	cityPath := t.TempDir()
	path := writeOrderOutcomeScaleLog(t, cityPath, 20000, nil)
	done := make(chan struct{})
	close(done)

	_, err := readOrderOutcomeLog(&CheckContext{Done: done}, path)

	if err == nil {
		t.Fatal("readOrderOutcomeLog on an abandoned check returned nil error, want cancellation")
	}
}

// TestOrderOutcomeHealthyScalesToLargeEventLog pins the ga-wu18 fix: the check
// took ~90s on a 151MB events.jsonl because it decoded the whole log three
// times. The fixture is a fraction of that size; the budget is ~100x the
// expected runtime, so only a return to decoding every line trips it.
func TestOrderOutcomeHealthyScalesToLargeEventLog(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a ~20MB event log")
	}
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "orders"), "scale-order", "cooldown", "5m")

	var wanted []events.Event
	for i := 0; i < 3; i++ {
		wanted = append(wanted, events.Event{Type: events.OrderFailed, Ts: now.Add(time.Duration(-3+i) * time.Hour), Subject: "scale-order", Message: "exit status 1"})
	}
	writeOrderOutcomeScaleLog(t, cityPath, 16000, wanted)

	start := time.Now()
	result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})
	elapsed := time.Since(start)

	if result.Status != StatusWarning {
		t.Fatalf("status = %v (%s), want StatusWarning for 3 trailing failures", result.Status, result.Message)
	}
	if elapsed > time.Second {
		t.Fatalf("Run took %s on a large event log, want well under 1s", elapsed)
	}
}
