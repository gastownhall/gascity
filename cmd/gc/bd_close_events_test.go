package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runproj"
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
