package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storehealth"
)

func TestStoreHealthSIBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1_000, "1.0 KB"},
		{1_500, "1.5 KB"},
		{1_000_000, "1.0 MB"},
		{11_200_000_000, "11.2 GB"},
	}
	for _, c := range cases {
		got := storeHealthSIBytes(c.in)
		if got != c.want {
			t.Errorf("storeHealthSIBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStoreHealthFromInputsOmitsLastGCWhenZero(t *testing.T) {
	h := storeHealthFromInputs("/c", 1_000_000, 1, time.Time{}, "")
	if h.LastGCAt != "" {
		t.Errorf("LastGCAt = %q, want empty", h.LastGCAt)
	}
	if h.LastGCStatus != "" {
		t.Errorf("LastGCStatus = %q, want empty", h.LastGCStatus)
	}

	data, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "last_gc_at") {
		t.Errorf("JSON contains last_gc_at when zero, got: %s", data)
	}
}

func TestStoreHealthFromInputsFormatsLastGCAsRFC3339(t *testing.T) {
	ts := time.Date(2026, 4, 1, 3, 15, 30, 0, time.UTC)
	h := storeHealthFromInputs("/c", 0, 0, ts, "success")
	if h.LastGCAt != "2026-04-01T03:15:30Z" {
		t.Errorf("LastGCAt = %q, want 2026-04-01T03:15:30Z", h.LastGCAt)
	}
	if h.LastGCStatus != "success" {
		t.Errorf("LastGCStatus = %q, want success", h.LastGCStatus)
	}
}

func TestRenderStoreHealthBlockNil(t *testing.T) {
	var buf bytes.Buffer
	renderStoreHealthBlock(&buf, nil)
	if buf.Len() != 0 {
		t.Fatalf("renderStoreHealthBlock(nil) wrote %q, want empty", buf.String())
	}
}

func TestRenderStoreHealthBlockWarning(t *testing.T) {
	h := storeHealthFromInputs("/c", 11_200_000_000, 221, time.Date(2026, 4, 1, 3, 0, 0, 0, time.UTC), "success")
	var buf bytes.Buffer
	renderStoreHealthBlock(&buf, h)

	out := buf.String()
	for _, want := range []string{
		"Store health:",
		"Path:        /c/.beads/dolt",
		"Size:        11.2 GB",
		"Live rows:   221",
		"MB/row",
		"(threshold 1.0 MB/row)",
		"\u26a0 maintenance overdue",
		"Last GC:     2026-04-01T03:00:00Z (success)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRenderStoreHealthBlockNoWarning(t *testing.T) {
	h := storeHealthFromInputs("/c", 50_000_000, 221, time.Time{}, "")
	var buf bytes.Buffer
	renderStoreHealthBlock(&buf, h)

	out := buf.String()
	if strings.Contains(out, "\u26a0") {
		t.Errorf("output contains warning glyph when Warning=false:\n%s", out)
	}
	if strings.Contains(out, "maintenance overdue") {
		t.Errorf("output contains overdue text when Warning=false:\n%s", out)
	}
	if strings.Contains(out, "Last GC:") {
		t.Errorf("output contains Last GC when not set:\n%s", out)
	}
}

func TestLiveRowCountNilStore(t *testing.T) {
	if got := liveRowCount(nil); got != 0 {
		t.Fatalf("liveRowCount(nil) = %d, want 0", got)
	}
}

func TestLiveRowCountCountsBeads(t *testing.T) {
	store := beads.NewMemStore()
	for i := 0; i < 3; i++ {
		if _, err := store.Create(beads.Bead{Title: "x"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	if got := liveRowCount(store); got != 3 {
		t.Fatalf("liveRowCount = %d, want 3", got)
	}
}

func TestLiveRowCountIncludesClosedBeads(t *testing.T) {
	store := beads.NewMemStore()
	open, err := store.Create(beads.Bead{Title: "open"})
	if err != nil {
		t.Fatalf("Create open: %v", err)
	}
	closed, err := store.Create(beads.Bead{Title: "closed"})
	if err != nil {
		t.Fatalf("Create closed: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := liveRowCount(store); got != 2 {
		t.Fatalf("liveRowCount = %d, want 2 including closed bead %s and open bead %s", got, closed.ID, open.ID)
	}
}

func TestCollectStoreHealthReadsEvents(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Title: "x"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ep := events.NewFake()
	ts := time.Date(2026, 4, 8, 12, 0, 0, 0, time.UTC)
	payload, _ := json.Marshal(events.StoreMaintenanceDonePayload{DurationSeconds: 5})
	ep.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: ts, Payload: payload})

	h := collectStoreHealth("/c", store, ep)
	if h == nil {
		t.Fatal("collectStoreHealth returned nil")
	}
	if h.LiveRows != 1 {
		t.Errorf("LiveRows = %d, want 1", h.LiveRows)
	}
	if h.LastGCAt != "2026-04-08T12:00:00Z" {
		t.Errorf("LastGCAt = %q, want 2026-04-08T12:00:00Z", h.LastGCAt)
	}
	if h.LastGCStatus != "success" {
		t.Errorf("LastGCStatus = %q, want success", h.LastGCStatus)
	}
	if h.Path != storehealth.StorePath("/c") {
		t.Errorf("Path = %q, want %q", h.Path, storehealth.StorePath("/c"))
	}
}
