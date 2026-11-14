package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runproj"
	"github.com/gastownhall/gascity/internal/testutil"
)

// closingFakeBdScript is a bd whose one step, demo-abc, is open until a
// `bd close` lands and closed after it, so the post-close read-back sees the
// close the passthrough made.
const closingFakeBdScript = `#!/bin/sh
state="${FAKE_BD_CLOSED:?}"
case "$1" in
show)
  if [ -f "$state" ]; then st=closed; reason=',"close_reason":"Done: implemented"'; else st=open; reason=''; fi
  printf '[{"id":"demo-abc","title":"implement","status":"%s","issue_type":"task","assignee":"worker-1","metadata":{"gc.root_bead_id":"demo-root"}%s}]\n' "$st" "$reason"
  ;;
close)
  : > "$state"
  printf '[{"id":"demo-abc","status":"closed"}]\n'
  ;;
dep)
  printf '[]\n'
  ;;
*)
  echo "fake bd: unexpected invocation: $*" >&2
  exit 1
  ;;
esac
`

// A worker closes its step with `gc bd close`, which hands the close to a bd
// subprocess against the work store. Like a hook claim, that write reached
// the run views only when the supervisor read the row back; the passthrough
// now announces the close itself.
func TestGcBdCloseAnnouncesTheCloseToRunProjection(t *testing.T) {
	silentFallbackTestSetup(t, closingFakeBdScript)
	cityDir := os.Getenv("GC_CITY_PATH")
	t.Setenv("FAKE_BD_CLOSED", filepath.Join(t.TempDir(), "closed"))

	// The supervisor recorded the step claimed before the worker closed it.
	eventsPath := filepath.Join(cityDir, ".gc", "events.jsonl")
	rec, err := openCityEventsLog(cityDir, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []beads.Bead{
		{ID: "demo-root", Title: "run", Status: "open", Type: "molecule"},
		{
			ID: "demo-abc", Title: "implement", Status: "in_progress", Type: "task", Assignee: "worker-1",
			Metadata: map[string]string{"gc.root_bead_id": "demo-root"},
		},
	} {
		payload, err := beads.EncodeBeadEventPayload(b)
		if err != nil {
			t.Fatal(err)
		}
		rec.Record(events.Event{Type: events.BeadUpdated, Actor: cacheReconcileActor, Subject: b.ID, Payload: payload})
	}
	_ = rec.Close()

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"close", "demo-abc", "--reason", "Done: implemented"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd(close) = %d, want 0; stderr=%s", code, stderr.String())
	}

	closes, err := events.ReadFiltered(eventsPath, events.Filter{Type: events.BeadClosed, Subject: "demo-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(closes) != 1 {
		t.Fatalf("bead.closed events for demo-abc = %d, want 1", len(closes))
	}
	if closes[0].RunID != "demo-root" {
		t.Fatalf("bead.closed run_id = %q, want demo-root", closes[0].RunID)
	}

	p := runproj.NewProjector()
	if err := p.ColdLoad(eventsPath); err != nil {
		t.Fatal(err)
	}
	for _, m := range runproj.RunMembers(p.Beads(), "demo-root") {
		if m.ID != "demo-abc" {
			continue
		}
		if m.Status != "closed" || m.CloseReason != "Done: implemented" || m.Title != "implement" {
			t.Fatalf("projected step = status %q reason %q title %q, want the closed row", m.Status, m.CloseReason, m.Title)
		}
		return
	}
	t.Fatal("demo-abc is not a member of demo-root in the projection")
}

// A bulk close announces every bead it closed while reading them back in one
// batched read and confirming their edges in one more, not one or two bd forks
// per id.
func TestGcBdBulkCloseAnnouncesEachCloseWithBatchedReads(t *testing.T) {
	_, cityDir := bdSQLRefusalCityDir(t, "")
	setCwd(t, cityDir)
	binDir := t.TempDir()
	calls := filepath.Join(t.TempDir(), "bd-calls.log")
	state := filepath.Join(t.TempDir(), "closed")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + calls + `"
st=open
[ -f "` + state + `" ] && st=closed
case "$1" in
show)
  printf '[{"id":"demo-a","title":"a","status":"%s","issue_type":"task"},{"id":"demo-b","title":"b","status":"%s","issue_type":"task"},{"id":"demo-c","title":"c","status":"%s","issue_type":"task"}]\n' "$st" "$st" "$st" ;;
close) : > "` + state + `" ;;
dep) printf '[{"issue_id":"demo-b","depends_on_id":"demo-a","type":"blocks"}]\n' ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"close", "demo-a", "demo-b", "demo-c"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd(close) = %d, want 0; stderr=%s", code, stderr.String())
	}

	closes, err := events.ReadFiltered(filepath.Join(cityDir, ".gc", "events.jsonl"), events.Filter{Type: events.BeadClosed})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range closes {
		b, ok := beads.DecodeBeadEventPayload(e.Payload)
		if !ok || b.Status != "closed" {
			t.Fatalf("bead.closed payload %s, want a closed snapshot", e.Payload)
		}
		if b.ID == "demo-b" && (len(b.Dependencies) != 1 || b.Dependencies[0].DependsOnID != "demo-a") {
			t.Fatalf("demo-b edges = %+v, want its batched blocks edge on demo-a", b.Dependencies)
		}
		got = append(got, b.ID)
	}
	if strings.Join(got, ",") != "demo-a,demo-b,demo-c" {
		t.Fatalf("bead.closed for %v, want demo-a,demo-b,demo-c", got)
	}

	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	_, after, _ := strings.Cut(string(data), "close demo-a demo-b demo-c")
	if shows, deps := strings.Count(after, "show "), strings.Count(after, "dep list"); shows != 1 || deps != 1 {
		t.Fatalf("after the close bd ran show %d and dep list %d times, want one of each:\n%s", shows, deps, data)
	}
}

// A passthrough that closed nothing — the bead was already closed — is not a
// close, and must not record one: the supervisor runs a committed close's
// completion and autoclose effects on every bead.closed.
func TestGcBdCloseOfAnAlreadyClosedBeadAnnouncesNothing(t *testing.T) {
	silentFallbackTestSetup(t, closingFakeBdScript)
	cityDir := os.Getenv("GC_CITY_PATH")
	closed := filepath.Join(t.TempDir(), "closed")
	if err := os.WriteFile(closed, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_BD_CLOSED", closed)

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"close", "demo-abc"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd(close) = %d, want 0; stderr=%s", code, stderr.String())
	}
	closes, err := events.ReadFiltered(filepath.Join(cityDir, ".gc", "events.jsonl"), events.Filter{Type: events.BeadClosed})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(closes) != 0 {
		t.Fatalf("bead.closed events = %d, want 0 for a bead that was already closed", len(closes))
	}
}

// TestGcBdCloseConfirmsEdgesWhileItReadsTheRowBack pins that a close's
// announcement reads an edgeless bead's edges at the same time as it reads the
// closed row back. Each read is its own bd subprocess on a bd-backed store, and
// a close does not change a bead's edges, so the edge read does not depend on
// the read-back. Read one after the other they put two serial bd round trips
// after every close of an edgeless bead, which doubled a gc bd close's cost and
// pushed it past TestGcBdLatencyBudget's 1 s under full remote integration load
// (ga-b5kq9js).
func TestGcBdCloseConfirmsEdgesWhileItReadsTheRowBack(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	showArrived, depArrived := make(chan struct{}), make(chan struct{})
	var showOnce, depOnce sync.Once
	// Each read waits, bounded, for the other to arrive: reads made at once
	// both arrive, reads made in series never do.
	awaitOther := func(other <-chan struct{}, self string) error {
		select {
		case <-other:
			return nil
		case <-time.After(testutil.GoroutineRaceTimeout):
			return fmt.Errorf("%s ran alone: the other read had not started", self)
		}
	}
	var mu sync.Mutex
	var alone []string
	runner := func(_, name string, args ...string) ([]byte, error) {
		full := name + " " + strings.Join(args, " ")
		switch {
		case strings.HasPrefix(full, "bd show --json demo-abc"):
			showOnce.Do(func() { close(showArrived) })
			if err := awaitOther(depArrived, "the read-back"); err != nil {
				mu.Lock()
				alone = append(alone, err.Error())
				mu.Unlock()
			}
			return []byte(`[{"id":"demo-abc","title":"implement","status":"closed","issue_type":"task","close_reason":"Done: implemented","metadata":{"gc.root_bead_id":"demo-root"}}]`), nil
		case strings.HasPrefix(full, "bd dep list demo-abc"):
			depOnce.Do(func() { close(depArrived) })
			if err := awaitOther(showArrived, "the edge read"); err != nil {
				mu.Lock()
				alone = append(alone, err.Error())
				mu.Unlock()
			}
			return []byte(`[]`), nil
		}
		return nil, errors.New("unexpected: " + full)
	}
	store := beads.NewBdStore(cityDir, runner)
	// The exact-ID guard's pre-read: open, and no edges inline.
	before := map[string]beads.Bead{"demo-abc": {ID: "demo-abc", Title: "implement", Status: "in_progress", Type: "task"}}

	announceBdPassthroughCloses(cityDir, store, before, []string{"close", "demo-abc", "--reason", "Done: implemented"})

	if len(alone) > 0 {
		t.Fatalf("the close's read-back and edge read ran one after the other: %s", strings.Join(alone, "; "))
	}
	closes, err := events.ReadFiltered(filepath.Join(cityDir, ".gc", "events.jsonl"), events.Filter{Type: events.BeadClosed, Subject: "demo-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(closes) != 1 {
		t.Fatalf("bead.closed events for demo-abc = %d, want 1", len(closes))
	}
	b, ok := beads.DecodeBeadEventPayload(closes[0].Payload)
	if !ok || b.Status != "closed" || b.CloseReason != "Done: implemented" {
		t.Fatalf("bead.closed payload %s, want the closed row read back", closes[0].Payload)
	}
	if !strings.Contains(string(closes[0].Payload), `"dependencies":[]`) {
		t.Fatalf("bead.closed payload %s, want an explicit confirmed-empty edge set", closes[0].Payload)
	}
}

// TestGcBdCloseFallsBackWhenTheSpeculativeEdgeReadFails pins that a failed
// edge read made beside the read-back answers for no bead: the closed bead's
// edges are then confirmed on their own, so the announcement still carries
// the edge set bd holds rather than none.
func TestGcBdCloseFallsBackWhenTheSpeculativeEdgeReadFails(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	depReads := 0
	runner := func(_, name string, args ...string) ([]byte, error) {
		full := name + " " + strings.Join(args, " ")
		switch {
		case strings.HasPrefix(full, "bd show --json demo-abc"):
			return []byte(`[{"id":"demo-abc","title":"implement","status":"closed","issue_type":"task","close_reason":"Done: implemented"}]`), nil
		case strings.HasPrefix(full, "bd dep list demo-abc"):
			mu.Lock()
			depReads++
			first := depReads == 1
			mu.Unlock()
			if first {
				return nil, errors.New("transient")
			}
			// A single-id dep list answers with the depended-on beads' rows.
			return []byte(`[{"id":"demo-root","title":"root","status":"open","issue_type":"task","dependency_type":"parent-child"}]`), nil
		}
		return nil, errors.New("unexpected: " + full)
	}
	store := beads.NewBdStore(cityDir, runner)
	before := map[string]beads.Bead{"demo-abc": {ID: "demo-abc", Title: "implement", Status: "in_progress", Type: "task"}}

	announceBdPassthroughCloses(cityDir, store, before, []string{"close", "demo-abc", "--reason", "Done: implemented"})

	closes, err := events.ReadFiltered(filepath.Join(cityDir, ".gc", "events.jsonl"), events.Filter{Type: events.BeadClosed, Subject: "demo-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(closes) != 1 {
		t.Fatalf("bead.closed events for demo-abc = %d, want 1", len(closes))
	}
	b, ok := beads.DecodeBeadEventPayload(closes[0].Payload)
	if !ok || len(b.Dependencies) != 1 || b.Dependencies[0].DependsOnID != "demo-root" {
		t.Fatalf("bead.closed payload %s, want the one demo-root edge the fallback confirmed", closes[0].Payload)
	}
	mu.Lock()
	defer mu.Unlock()
	if depReads != 2 {
		t.Fatalf("dep list reads = %d, want 2: the failed read beside the read-back, then the fallback", depReads)
	}
}

// TestGcBdCloseKeepsInlineEdgesOverTheSpeculativeRead pins that edges the
// read-back carries inline win over the edge read made beside it: that read
// was made because the pre-close row had no edges, and its answer must not
// replace the committed row's own edge set.
func TestGcBdCloseKeepsInlineEdgesOverTheSpeculativeRead(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := func(_, name string, args ...string) ([]byte, error) {
		full := name + " " + strings.Join(args, " ")
		switch {
		case strings.HasPrefix(full, "bd show --json demo-abc"):
			return []byte(`[{"id":"demo-abc","title":"implement","status":"closed","issue_type":"task","close_reason":"Done: implemented","dependencies":[{"issue_id":"demo-abc","depends_on_id":"demo-x","type":"blocks"}]}]`), nil
		case strings.HasPrefix(full, "bd dep list demo-abc"):
			return []byte(`[]`), nil
		}
		return nil, errors.New("unexpected: " + full)
	}
	store := beads.NewBdStore(cityDir, runner)
	before := map[string]beads.Bead{"demo-abc": {ID: "demo-abc", Title: "implement", Status: "in_progress", Type: "task"}}

	announceBdPassthroughCloses(cityDir, store, before, []string{"close", "demo-abc", "--reason", "Done: implemented"})

	closes, err := events.ReadFiltered(filepath.Join(cityDir, ".gc", "events.jsonl"), events.Filter{Type: events.BeadClosed, Subject: "demo-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(closes) != 1 {
		t.Fatalf("bead.closed events for demo-abc = %d, want 1", len(closes))
	}
	b, ok := beads.DecodeBeadEventPayload(closes[0].Payload)
	if !ok || len(b.Dependencies) != 1 || b.Dependencies[0].DependsOnID != "demo-x" || b.Dependencies[0].Type != "blocks" {
		t.Fatalf("bead.closed payload %s, want the read-back's inline demo-x edge, not the empty edge read", closes[0].Payload)
	}
}
