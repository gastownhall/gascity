package events

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// typesFixture writes an active log mixing three event types, including one
// hand-written line with whitespace around the type key so the byte prefilter
// is proven to be a superset check rather than a format assumption.
func typesFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	ts := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	lines := []string{
		fmt.Sprintf(`{"seq":1,"type":%q,"ts":%q,"actor":"a","subject":"o1"}`, OrderCompleted, ts),
		fmt.Sprintf(`{"seq":2,"type":%q,"ts":%q,"actor":"a","subject":"noise"}`, BeadCreated, ts),
		fmt.Sprintf(`{"seq":3,"type":%q,"ts":%q,"actor":"a","subject":"o1"}`, OrderFailed, ts),
		fmt.Sprintf(`{ "seq": 4, "type" : %q, "ts": %q, "actor": "a", "subject": "sys" }`, ControllerStarted, ts),
		`not json but mentions ` + OrderFailed,
		fmt.Sprintf(`{"seq":5,"type":%q,"ts":%q,"actor":"a","subject":"o2"}`, OrderFailed, ts),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFilterTypesMatchesAnyListedTypeInSeqOrder(t *testing.T) {
	path := typesFixture(t)

	got, err := ReadFiltered(path, Filter{Types: []string{OrderCompleted, OrderFailed, ControllerStarted}})
	if err != nil {
		t.Fatalf("ReadFiltered: %v", err)
	}
	if want := []uint64{1, 3, 4, 5}; !reflect.DeepEqual(seqsOf(got), want) {
		t.Fatalf("seqs = %v, want %v", seqsOf(got), want)
	}
}

func TestFilterTypesCombinesWithType(t *testing.T) {
	path := typesFixture(t)

	got, err := ReadFiltered(path, Filter{Type: OrderFailed, Types: []string{OrderCompleted}})
	if err != nil {
		t.Fatalf("ReadFiltered: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Type and Types are ANDed; got %v, want none", seqsOf(got))
	}
}

func TestFilterTypesSpansArchiveAndActiveLog(t *testing.T) {
	path := typesFixture(t)
	dir := filepath.Dir(path)
	ts := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	// Archive seqs sit below the active log's so ordering stays archive-first.
	writeArchiveWithEvents(t, dir, "20260927T000000Z", 100, 199,
		Event{Seq: 100, Type: OrderFailed, Ts: ts},
		Event{Seq: 101, Type: BeadCreated, Ts: ts},
		Event{Seq: 102, Type: ControllerStarted, Ts: ts})

	got, err := ReadFiltered(path, Filter{Types: []string{OrderFailed, ControllerStarted}})
	if err != nil {
		t.Fatalf("ReadFiltered: %v", err)
	}
	if want := []uint64{100, 102, 3, 4, 5}; !reflect.DeepEqual(seqsOf(got), want) {
		t.Fatalf("seqs = %v, want %v", seqsOf(got), want)
	}
}

func TestApplyFilterHonoursTypes(t *testing.T) {
	evs := []Event{{Seq: 1, Type: OrderFailed}, {Seq: 2, Type: BeadCreated}, {Seq: 3, Type: OrderCompleted}}

	got := ApplyFilter(evs, Filter{Types: []string{OrderFailed, OrderCompleted}})
	if want := []uint64{1, 3}; !reflect.DeepEqual(seqsOf(got), want) {
		t.Fatalf("seqs = %v, want %v", seqsOf(got), want)
	}
}

func TestReadFilteredContextTypesMatchesReadFiltered(t *testing.T) {
	path := typesFixture(t)
	filter := Filter{Types: []string{OrderCompleted, OrderFailed}}

	want, err := ReadFiltered(path, filter)
	if err != nil {
		t.Fatalf("ReadFiltered: %v", err)
	}
	got, err := ReadFilteredContext(context.Background(), path, filter)
	if err != nil {
		t.Fatalf("ReadFilteredContext: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadFilteredContext = %v, want %v", seqsOf(got), seqsOf(want))
	}
}

// TestReadFilteredContextAbortsMidActiveScan proves a canceled context stops
// the active-log scan itself, not only the walk between archives: a single
// large events.jsonl is exactly the shape that made gc doctor time out.
func TestReadFilteredContextAbortsMidActiveScan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var b strings.Builder
	ts := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	const lines = 3 * ctxCheckInterval
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&b, `{"seq":%d,"type":%q,"ts":%q,"actor":"a"}`+"\n", i, BeadCreated, ts)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	// Err() call #1 is the entry check; #2 lands after the first interval.
	ctx := newCancelAfter(2)
	got, err := ReadFilteredContext(ctx, path, Filter{})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(got) == 0 || len(got) >= lines {
		t.Fatalf("got %d events, want partial progress (0 < n < %d)", len(got), lines)
	}
}
