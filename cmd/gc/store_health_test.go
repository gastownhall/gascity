package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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

func TestRenderStoreHealthBlockNil(t *testing.T) {
	var buf bytes.Buffer
	renderStoreHealthBlock(&buf, nil)
	if buf.Len() != 0 {
		t.Fatalf("renderStoreHealthBlock(nil) wrote %q, want empty", buf.String())
	}
}

func TestRenderStoreHealthBlockWarning(t *testing.T) {
	h := storeHealthFromInputs("/c", 11_200_000_000, 221)
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
		"\u26a0 size-to-row ratio exceeds threshold",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRenderStoreHealthBlockNoWarning(t *testing.T) {
	h := storeHealthFromInputs("/c", 50_000_000, 221)
	var buf bytes.Buffer
	renderStoreHealthBlock(&buf, h)

	out := buf.String()
	if strings.Contains(out, "\u26a0") {
		t.Errorf("output contains warning glyph when Warning=false:\n%s", out)
	}
	if strings.Contains(out, "overdue") {
		t.Errorf("output contains overdue text when Warning=false:\n%s", out)
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

func TestCollectStoreHealth(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Title: "x"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	h := collectStoreHealth("/c", store)
	if h == nil {
		t.Fatal("collectStoreHealth returned nil")
	}
	if h.LiveRows != 1 {
		t.Errorf("LiveRows = %d, want 1", h.LiveRows)
	}
	if h.Path != storehealth.StorePath("/c") {
		t.Errorf("Path = %q, want %q", h.Path, storehealth.StorePath("/c"))
	}
}
