package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// newTestProvider creates a FileRecorder-backed Provider for testing.
func newTestProvider(t *testing.T, dir string) *events.FileRecorder {
	t.Helper()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := events.NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() }) //nolint:errcheck // test cleanup
	return rec
}

func TestEventsEmpty(t *testing.T) {
	dir := t.TempDir()
	ep := newTestProvider(t, dir)
	// No events recorded.

	var stdout, stderr bytes.Buffer
	code := doEvents(ep, "", "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No events.") {
		t.Errorf("stdout = %q, want 'No events.'", stdout.String())
	}
}

func TestEventsShowsAll(t *testing.T) {
	dir := t.TempDir()
	ep := newTestProvider(t, dir)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-1", Message: "Build Tower of Hanoi"})
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "mayor", Message: "mayor"})

	var stdout, stderr bytes.Buffer
	code := doEvents(ep, "", "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"SEQ", "TYPE", "ACTOR", "SUBJECT", "MESSAGE", "TIME",
		"1", "bead.created", "human", "gc-1", "Build Tower of Hanoi",
		"2", "session.woke", "gc", "mayor",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestEventsFilterByType(t *testing.T) {
	dir := t.TempDir()
	ep := newTestProvider(t, dir)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-1"})
	ep.Record(events.Event{Type: events.BeadClosed, Actor: "human", Subject: "gc-1"})
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "mayor"})

	var stdout, stderr bytes.Buffer
	code := doEvents(ep, events.BeadCreated, "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "bead.created") {
		t.Errorf("stdout missing 'bead.created': %q", out)
	}
	if strings.Contains(out, "bead.closed") {
		t.Errorf("stdout should not contain 'bead.closed': %q", out)
	}
	if strings.Contains(out, "session.woke") {
		t.Errorf("stdout should not contain 'agent.started': %q", out)
	}
}

func TestEventsFilterBySince(t *testing.T) {
	dir := t.TempDir()
	ep := newTestProvider(t, dir)
	old := time.Now().Add(-2 * time.Hour)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-1", Ts: old})
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "mayor"})

	var stdout, stderr bytes.Buffer
	code := doEvents(ep, "", "1h", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if strings.Contains(out, "bead.created") {
		t.Errorf("stdout should not contain old event: %q", out)
	}
	if !strings.Contains(out, "session.woke") {
		t.Errorf("stdout missing recent event: %q", out)
	}
}

func TestEventsInvalidSince(t *testing.T) {
	dir := t.TempDir()
	ep := newTestProvider(t, dir)

	var stdout, stderr bytes.Buffer
	code := doEvents(ep, "", "notaduration", nil, &stdout, &stderr)
	if code != 1 {
		t.Errorf("doEvents = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "invalid --since") {
		t.Errorf("stderr = %q, want 'invalid --since'", stderr.String())
	}
}

func TestEventsPayloadMatchStandard(t *testing.T) {
	dir := t.TempDir()
	ep := newTestProvider(t, dir)
	ep.Record(events.Event{
		Type:    events.BeadCreated,
		Actor:   "human",
		Subject: "gc-1",
		Payload: json.RawMessage(`{"type":"task"}`),
	})
	ep.Record(events.Event{
		Type:    events.BeadCreated,
		Actor:   "human",
		Subject: "gc-2",
		Payload: json.RawMessage(`{"type":"merge-request"}`),
	})

	pm := map[string][]string{"type": {"merge-request"}}
	var stdout, stderr bytes.Buffer
	code := doEvents(ep, "", "", pm, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "gc-2") {
		t.Errorf("stdout missing gc-2 (merge-request): %q", out)
	}
	if strings.Contains(out, "gc-1") {
		t.Errorf("stdout should not contain gc-1 (task): %q", out)
	}
}

// --- JSON mode tests ---

func TestEventsJSON(t *testing.T) {
	dir := t.TempDir()
	ep := newTestProvider(t, dir)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-1", Message: "Build Tower of Hanoi"})
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "mayor", Message: "mayor"})

	var stdout, stderr bytes.Buffer
	code := doEventsJSON(ep, "", "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsJSON = %d, want 0; stderr: %s", code, stderr.String())
	}

	var evts []events.Event
	if err := json.Unmarshal(stdout.Bytes(), &evts); err != nil {
		t.Fatalf("unmarshal: %v; output: %s", err, stdout.String())
	}
	if len(evts) != 2 {
		t.Fatalf("got %d events, want 2", len(evts))
	}
	if evts[0].Type != events.BeadCreated {
		t.Errorf("evts[0].Type = %q, want %q", evts[0].Type, events.BeadCreated)
	}
	if evts[1].Subject != "mayor" {
		t.Errorf("evts[1].Subject = %q, want %q", evts[1].Subject, "mayor")
	}
}

func TestEventsJSONEmpty(t *testing.T) {
	dir := t.TempDir()
	ep := newTestProvider(t, dir)

	var stdout, stderr bytes.Buffer
	code := doEventsJSON(ep, "", "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsJSON = %d, want 0; stderr: %s", code, stderr.String())
	}

	var evts []events.Event
	if err := json.Unmarshal(stdout.Bytes(), &evts); err != nil {
		t.Fatalf("unmarshal: %v; output: %s", err, stdout.String())
	}
	if len(evts) != 0 {
		t.Errorf("got %d events, want 0", len(evts))
	}
}

func TestEventsJSONWithTypeFilter(t *testing.T) {
	dir := t.TempDir()
	ep := newTestProvider(t, dir)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-1"})
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "mayor"})

	var stdout, stderr bytes.Buffer
	code := doEventsJSON(ep, events.BeadCreated, "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsJSON = %d, want 0; stderr: %s", code, stderr.String())
	}

	var evts []events.Event
	if err := json.Unmarshal(stdout.Bytes(), &evts); err != nil {
		t.Fatalf("unmarshal: %v; output: %s", err, stdout.String())
	}
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	if evts[0].Type != events.BeadCreated {
		t.Errorf("evts[0].Type = %q, want %q", evts[0].Type, events.BeadCreated)
	}
}

// --- Seq mode tests ---

func TestDoEventsSeq(t *testing.T) {
	dir := t.TempDir()
	ep := newTestProvider(t, dir)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
	ep.Record(events.Event{Type: events.BeadClosed, Actor: "human"})
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})

	var stdout, stderr bytes.Buffer
	code := doEventsSeq(ep, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsSeq = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "3" {
		t.Errorf("seq = %q, want 3", strings.TrimSpace(stdout.String()))
	}
}

func TestDoEventsSeqEmpty(t *testing.T) {
	dir := t.TempDir()
	ep := newTestProvider(t, dir)

	var stdout, stderr bytes.Buffer
	code := doEventsSeq(ep, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsSeq = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "0" {
		t.Errorf("seq = %q, want 0", strings.TrimSpace(stdout.String()))
	}
}

// --- Watch mode tests ---

func TestDoEventsWatchImmediate(t *testing.T) {
	dir := t.TempDir()
	ep := newTestProvider(t, dir)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-1"})
	ep.Record(events.Event{Type: events.BeadClosed, Actor: "human", Subject: "gc-1"})

	// afterSeq=1 → seq 2 is already there, should return immediately.
	var stdout, stderr bytes.Buffer
	code := doEventsWatch(ep, "", nil, 1, 100*time.Millisecond, 0, 10*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if out == "" {
		t.Fatal("expected JSON output, got empty")
	}

	// Parse the output JSON line.
	var e events.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &e); err != nil {
		t.Fatalf("unmarshal output: %v; output: %q", err, out)
	}
	if e.Seq != 2 {
		t.Errorf("Seq = %d, want 2", e.Seq)
	}
	if e.Type != events.BeadClosed {
		t.Errorf("Type = %q, want %q", e.Type, events.BeadClosed)
	}
}

func TestDoEventsWatchTimeout(t *testing.T) {
	dir := t.TempDir()
	ep := newTestProvider(t, dir)

	var stdout, stderr bytes.Buffer
	code := doEventsWatch(ep, "", nil, 0, 50*time.Millisecond, 0, 10*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("expected empty stdout on timeout, got %q", stdout.String())
	}
}

func TestDoEventsWatchTypeFilter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	ep := newTestProvider(t, dir)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "human"}) // seq 1

	// Watch for bead.closed after seq 0. A goroutine will append it.
	go func() {
		time.Sleep(30 * time.Millisecond)
		var stderrBuf bytes.Buffer
		rec2, err := events.NewFileRecorder(path, &stderrBuf)
		if err != nil {
			return
		}
		rec2.Record(events.Event{Type: events.BeadCreated, Actor: "human"}) // seq 2 — not matching
		rec2.Record(events.Event{Type: events.BeadClosed, Actor: "human"})  // seq 3 — matching
		rec2.Close()                                                        //nolint:errcheck // test cleanup
	}()

	var stdout, stderr bytes.Buffer
	code := doEventsWatch(ep, events.BeadClosed, nil, 0, 2*time.Second, 0, 10*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "bead.closed") {
		t.Errorf("output missing 'bead.closed': %q", out)
	}
	if strings.Contains(out, "bead.created") {
		t.Errorf("output should not contain 'bead.created': %q", out)
	}
}

func TestDoEventsWatchAfterSeq(t *testing.T) {
	dir := t.TempDir()
	ep := newTestProvider(t, dir)
	for i := 0; i < 5; i++ {
		ep.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
	}

	// Watch with explicit afterSeq=3 — should return seq 4 and 5.
	var stdout, stderr bytes.Buffer
	code := doEventsWatch(ep, "", nil, 3, 100*time.Millisecond, 0, 10*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr: %s", code, stderr.String())
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2; output: %q", len(lines), stdout.String())
	}

	var e1, e2 events.Event
	if err := json.Unmarshal([]byte(lines[0]), &e1); err != nil {
		t.Fatalf("unmarshal line 0: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &e2); err != nil {
		t.Fatalf("unmarshal line 1: %v", err)
	}
	if e1.Seq != 4 {
		t.Errorf("line 0 Seq = %d, want 4", e1.Seq)
	}
	if e2.Seq != 5 {
		t.Errorf("line 1 Seq = %d, want 5", e2.Seq)
	}
}

func TestDoEventsWatchDefaultAfterSeq(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	ep := newTestProvider(t, dir)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "human"}) // seq 1

	// afterSeq=0 means "current head" (seq 1). A goroutine appends after delay.
	go func() {
		time.Sleep(30 * time.Millisecond)
		var stderrBuf bytes.Buffer
		rec2, err := events.NewFileRecorder(path, &stderrBuf)
		if err != nil {
			return
		}
		rec2.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "mayor"}) // seq 2
		rec2.Close()                                                                       //nolint:errcheck // test cleanup
	}()

	var stdout, stderr bytes.Buffer
	code := doEventsWatch(ep, "", nil, 0, 2*time.Second, 0, 10*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	// Should only contain the new event (seq 2), not the existing one (seq 1).
	if !strings.Contains(out, "session.woke") {
		t.Errorf("output missing 'session.woke': %q", out)
	}

	var e events.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &e); err != nil {
		t.Fatalf("unmarshal: %v; output: %q", err, out)
	}
	if e.Seq != 2 {
		t.Errorf("Seq = %d, want 2", e.Seq)
	}
}

func TestDoEventsWatchNoTypeFilter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	ep := newTestProvider(t, dir)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "human"}) // seq 1

	// Watch with no type filter. Append mixed event types after delay.
	go func() {
		time.Sleep(30 * time.Millisecond)
		var stderrBuf bytes.Buffer
		rec2, err := events.NewFileRecorder(path, &stderrBuf)
		if err != nil {
			return
		}
		rec2.Record(events.Event{Type: events.BeadClosed, Actor: "human"}) // seq 2
		rec2.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})   // seq 3
		rec2.Close()                                                       //nolint:errcheck // test cleanup
	}()

	var stdout, stderr bytes.Buffer
	code := doEventsWatch(ep, "", nil, 0, 2*time.Second, 0, 10*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 1 {
		t.Fatalf("got %d lines, want >= 1; output: %q", len(lines), out)
	}
	// At least one event type should be present.
	if !strings.Contains(out, "bead.closed") && !strings.Contains(out, "session.woke") {
		t.Errorf("output missing events: %q", out)
	}
}

func TestDoEventsWatchPayloadMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	ep := newTestProvider(t, dir)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "human"}) // seq 1 — no payload

	// Append events with payloads after a delay.
	go func() {
		time.Sleep(30 * time.Millisecond)
		var stderrBuf bytes.Buffer
		rec2, err := events.NewFileRecorder(path, &stderrBuf)
		if err != nil {
			return
		}
		// seq 2: task bead — should NOT match
		rec2.Record(events.Event{
			Type:    events.BeadCreated,
			Actor:   "human",
			Subject: "gc-10",
			Payload: json.RawMessage(`{"type":"task","title":"Build thing"}`),
		})
		// seq 3: merge-request bead — should match
		rec2.Record(events.Event{
			Type:    events.BeadCreated,
			Actor:   "polecat",
			Subject: "gc-11",
			Payload: json.RawMessage(`{"type":"merge-request","title":"Fix login"}`),
		})
		rec2.Close() //nolint:errcheck // test cleanup
	}()

	pm := map[string][]string{"type": {"merge-request"}}
	var stdout, stderr bytes.Buffer
	code := doEventsWatch(ep, events.BeadCreated, pm, 0, 2*time.Second, 0, 10*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "gc-11") {
		t.Errorf("output missing merge-request bead 'gc-11': %q", out)
	}
	if strings.Contains(out, "gc-10") {
		t.Errorf("output should not contain task bead 'gc-10': %q", out)
	}
}

func TestDoEventsWatchPayloadMatchTimeout(t *testing.T) {
	dir := t.TempDir()
	ep := newTestProvider(t, dir)
	// Only task beads — no merge-request, so payload-match should timeout.
	ep.Record(events.Event{
		Type:    events.BeadCreated,
		Actor:   "human",
		Payload: json.RawMessage(`{"type":"task"}`),
	})

	pm := map[string][]string{"type": {"merge-request"}}
	var stdout, stderr bytes.Buffer
	code := doEventsWatch(ep, events.BeadCreated, pm, 1, 50*time.Millisecond, 0, 10*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("expected empty stdout on timeout, got %q", stdout.String())
	}
}

// --- Debounce tests ---

func TestDoEventsWatchDebounceAccumulates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	ep := newTestProvider(t, dir)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "human"}) // seq 1

	// Append events in batches during the debounce window.
	go func() {
		time.Sleep(20 * time.Millisecond)
		var stderrBuf bytes.Buffer
		rec2, err := events.NewFileRecorder(path, &stderrBuf)
		if err != nil {
			return
		}
		rec2.Record(events.Event{Type: events.BeadUpdated, Actor: "polecat", Subject: "gc-1"}) // seq 2
		rec2.Close()                                                                           //nolint:errcheck // test cleanup

		time.Sleep(30 * time.Millisecond)
		rec3, err := events.NewFileRecorder(path, &stderrBuf)
		if err != nil {
			return
		}
		rec3.Record(events.Event{Type: events.BeadUpdated, Actor: "polecat", Subject: "gc-2"}) // seq 3
		rec3.Close()                                                                           //nolint:errcheck // test cleanup
	}()

	var stdout, stderr bytes.Buffer
	// debounce=150ms — should accumulate both events before returning.
	code := doEventsWatch(ep, events.BeadUpdated, nil, 0, 2*time.Second, 150*time.Millisecond, 10*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr: %s", code, stderr.String())
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("got %d lines, want >= 2 (debounce should accumulate); output: %q", len(lines), stdout.String())
	}

	// Both bead.updated events should be present.
	out := stdout.String()
	if !strings.Contains(out, "gc-1") {
		t.Errorf("output missing gc-1: %q", out)
	}
	if !strings.Contains(out, "gc-2") {
		t.Errorf("output missing gc-2: %q", out)
	}
}

func TestDoEventsWatchDebounceZeroIsImmediate(t *testing.T) {
	// With debounce=0, behavior is identical to pre-debounce: returns on first match.
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	ep := newTestProvider(t, dir)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "human"}) // seq 1

	go func() {
		time.Sleep(20 * time.Millisecond)
		var stderrBuf bytes.Buffer
		rec2, err := events.NewFileRecorder(path, &stderrBuf)
		if err != nil {
			return
		}
		rec2.Record(events.Event{Type: events.BeadUpdated, Actor: "polecat", Subject: "gc-1"}) // seq 2
		rec2.Close()                                                                           //nolint:errcheck // test cleanup

		time.Sleep(30 * time.Millisecond)
		rec3, err := events.NewFileRecorder(path, &stderrBuf)
		if err != nil {
			return
		}
		rec3.Record(events.Event{Type: events.BeadUpdated, Actor: "polecat", Subject: "gc-2"}) // seq 3
		rec3.Close()                                                                           //nolint:errcheck // test cleanup
	}()

	var stdout, stderr bytes.Buffer
	// debounce=0 — should return immediately on first match.
	code := doEventsWatch(ep, events.BeadUpdated, nil, 0, 2*time.Second, 0, 10*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr: %s", code, stderr.String())
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	// Should return only the first batch (seq 2), not wait for seq 3.
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1 (no debounce = immediate return); output: %q", len(lines), stdout.String())
	}
	if !strings.Contains(stdout.String(), "gc-1") {
		t.Errorf("output missing gc-1: %q", stdout.String())
	}
}

func TestDoEventsWatchDebounceTimeoutBeforeMatch(t *testing.T) {
	// Timeout expires before any events arrive — debounce doesn't change this.
	dir := t.TempDir()
	ep := newTestProvider(t, dir)

	var stdout, stderr bytes.Buffer
	code := doEventsWatch(ep, events.BeadUpdated, nil, 0, 80*time.Millisecond, 40*time.Millisecond, 10*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("expected empty stdout on timeout, got %q", stdout.String())
	}
}

func TestDoEventsWatchDebounceWithExplicitAfterSeq(t *testing.T) {
	// Debounce should work when events already exist past afterSeq.
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	ep := newTestProvider(t, dir)
	ep.Record(events.Event{Type: events.BeadUpdated, Actor: "polecat", Subject: "gc-1"}) // seq 1
	ep.Record(events.Event{Type: events.BeadUpdated, Actor: "polecat", Subject: "gc-2"}) // seq 2

	// Append a third event during the debounce window.
	go func() {
		time.Sleep(30 * time.Millisecond)
		var stderrBuf bytes.Buffer
		rec2, err := events.NewFileRecorder(path, &stderrBuf)
		if err != nil {
			return
		}
		rec2.Record(events.Event{Type: events.BeadUpdated, Actor: "polecat", Subject: "gc-3"}) // seq 3
		rec2.Close()                                                                           //nolint:errcheck // test cleanup
	}()

	var stdout, stderr bytes.Buffer
	// afterSeq=0 means "current head" (seq 2). Only seq 3 should appear.
	// But with afterSeq=1, seqs 2 and 3 should appear after debounce.
	code := doEventsWatch(ep, events.BeadUpdated, nil, 1, 2*time.Second, 100*time.Millisecond, 10*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// Should have at least 2 events: gc-2 (already past afterSeq=1) and gc-3 (appended during debounce).
	if len(lines) < 2 {
		t.Fatalf("got %d lines, want >= 2; output: %q", len(lines), out)
	}
	if !strings.Contains(out, "gc-2") {
		t.Errorf("output missing gc-2: %q", out)
	}
	if !strings.Contains(out, "gc-3") {
		t.Errorf("output missing gc-3: %q", out)
	}
}

func TestParsePayloadMatch(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		m, err := parsePayloadMatch([]string{"type=merge-request", "status=open"})
		if err != nil {
			t.Fatal(err)
		}
		if len(m["type"]) != 1 || m["type"][0] != "merge-request" {
			t.Errorf("type = %v, want [merge-request]", m["type"])
		}
		if len(m["status"]) != 1 || m["status"][0] != "open" {
			t.Errorf("status = %v, want [open]", m["status"])
		}
	})

	t.Run("or_same_key", func(t *testing.T) {
		m, err := parsePayloadMatch([]string{"type=merge-request", "type=message"})
		if err != nil {
			t.Fatal(err)
		}
		if len(m["type"]) != 2 {
			t.Fatalf("type has %d values, want 2", len(m["type"]))
		}
		if m["type"][0] != "merge-request" || m["type"][1] != "message" {
			t.Errorf("type = %v, want [merge-request message]", m["type"])
		}
	})

	t.Run("empty", func(t *testing.T) {
		m, err := parsePayloadMatch(nil)
		if err != nil {
			t.Fatal(err)
		}
		if m != nil {
			t.Errorf("got %v, want nil", m)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		_, err := parsePayloadMatch([]string{"noequals"})
		if err == nil {
			t.Error("expected error for missing =")
		}
	})

	t.Run("value_with_equals", func(t *testing.T) {
		m, err := parsePayloadMatch([]string{"expr=a=b"})
		if err != nil {
			t.Fatal(err)
		}
		if len(m["expr"]) != 1 || m["expr"][0] != "a=b" {
			t.Errorf("expr = %v, want [a=b]", m["expr"])
		}
	})
}

func TestMatchPayload(t *testing.T) {
	t.Run("nil_match_always_true", func(t *testing.T) {
		if !matchPayload(nil, nil) {
			t.Error("nil match should return true")
		}
	})

	t.Run("empty_payload_no_match", func(t *testing.T) {
		if matchPayload(nil, map[string][]string{"type": {"task"}}) {
			t.Error("nil payload should not match")
		}
	})

	t.Run("string_value", func(t *testing.T) {
		p := json.RawMessage(`{"type":"merge-request","title":"Fix"}`)
		if !matchPayload(p, map[string][]string{"type": {"merge-request"}}) {
			t.Error("should match")
		}
		if matchPayload(p, map[string][]string{"type": {"task"}}) {
			t.Error("should not match wrong value")
		}
	})

	t.Run("multiple_keys", func(t *testing.T) {
		p := json.RawMessage(`{"type":"merge-request","assignee":"refinery"}`)
		if !matchPayload(p, map[string][]string{"type": {"merge-request"}, "assignee": {"refinery"}}) {
			t.Error("should match both keys")
		}
		if matchPayload(p, map[string][]string{"type": {"merge-request"}, "assignee": {"deacon"}}) {
			t.Error("should not match wrong second value")
		}
	})

	t.Run("missing_key", func(t *testing.T) {
		p := json.RawMessage(`{"type":"task"}`)
		if matchPayload(p, map[string][]string{"assignee": {"refinery"}}) {
			t.Error("should not match missing key")
		}
	})

	t.Run("non_string_value", func(t *testing.T) {
		p := json.RawMessage(`{"count":42}`)
		if !matchPayload(p, map[string][]string{"count": {"42"}}) {
			t.Error("should match numeric value via raw comparison")
		}
	})

	t.Run("or_same_key", func(t *testing.T) {
		mr := json.RawMessage(`{"type":"merge-request"}`)
		msg := json.RawMessage(`{"type":"message"}`)
		task := json.RawMessage(`{"type":"task"}`)
		filter := map[string][]string{"type": {"merge-request", "message"}}
		if !matchPayload(mr, filter) {
			t.Error("merge-request should match OR filter")
		}
		if !matchPayload(msg, filter) {
			t.Error("message should match OR filter")
		}
		if matchPayload(task, filter) {
			t.Error("task should NOT match OR filter")
		}
	})

	t.Run("or_with_and", func(t *testing.T) {
		// type=merge-request OR message, AND assignee=refinery
		p1 := json.RawMessage(`{"type":"merge-request","assignee":"refinery"}`)
		p2 := json.RawMessage(`{"type":"message","assignee":"refinery"}`)
		p3 := json.RawMessage(`{"type":"merge-request","assignee":"deacon"}`)
		filter := map[string][]string{"type": {"merge-request", "message"}, "assignee": {"refinery"}}
		if !matchPayload(p1, filter) {
			t.Error("MR+refinery should match")
		}
		if !matchPayload(p2, filter) {
			t.Error("message+refinery should match")
		}
		if matchPayload(p3, filter) {
			t.Error("MR+deacon should NOT match (wrong assignee)")
		}
	})
}
