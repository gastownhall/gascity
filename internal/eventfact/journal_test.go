package eventfact

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/journalops"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/ledgerfeed"
)

// bdImage renders b the way bd's json.Marshal of its issue does for the
// fields a journal post-image carries (the shape TestDecodeJournalRowReadsA
// RealStoresPostImage checks against a real store).
func bdImage(t *testing.T, b beads.Bead) string {
	t.Helper()
	image := map[string]any{
		"id":         b.ID,
		"title":      b.Title,
		"status":     b.Status,
		"priority":   2,
		"issue_type": b.Type,
		"created_at": "2026-10-15T12:30:45Z",
		"updated_at": "2026-10-15T12:30:45Z",
		"labels":     b.Labels,
		"metadata":   b.Metadata,
		"no_history": b.NoHistory,
	}
	data, err := json.Marshal(image)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDecodeJournalRow(t *testing.T) {
	b, err := Encode("gc", testFact())
	if err != nil {
		t.Fatal(err)
	}
	row := journalops.Row{Seq: 7, Op: "create", IssueID: b.ID, IssueJSON: bdImage(t, b)}
	f, ok, err := DecodeJournalRow(row)
	if err != nil || !ok {
		t.Fatalf("DecodeJournalRow = %v, %v", ok, err)
	}
	want := testFact()
	want.ID = b.ID
	sameFact(t, f, want)

	// A store that creates rows open journals the create of an open row.
	open := b
	open.Status = "open"
	if _, ok, err := DecodeJournalRow(journalops.Row{Seq: 8, Op: "create", IssueID: b.ID, IssueJSON: bdImage(t, open)}); err != nil || !ok {
		t.Fatalf("an open create = %v, %v", ok, err)
	}

	for name, r := range map[string]journalops.Row{
		"the close of a fact": {Seq: 9, Op: "close", IssueID: b.ID, IssueJSON: bdImage(t, b)},
		"an update":           {Seq: 10, Op: "update", IssueID: b.ID, IssueJSON: bdImage(t, b)},
		"a delete":            {Seq: 11, Op: "delete", IssueID: b.ID},
		"a task's create":     {Seq: 12, Op: "create", IssueID: "gc-1", IssueJSON: bdImage(t, beads.Bead{ID: "gc-1", Title: "t", Status: "open", Type: "task"})},
		"an op row's create":  {Seq: 13, Op: "create", IssueID: "gc-op-x", IssueJSON: bdImage(t, beads.Bead{ID: "gc-op-x", Title: "o", Status: "open", Type: "op", Labels: []string{"gc:op"}})},
	} {
		if _, ok, err := DecodeJournalRow(r); ok || err != nil {
			t.Errorf("%s: DecodeJournalRow = %v, %v; want skipped", name, ok, err)
		}
	}

	if _, ok, err := DecodeJournalRow(journalops.Row{Seq: 14, Op: "create", IssueID: b.ID, IssueJSON: "{"}); ok || err == nil {
		t.Errorf("an unreadable post-image = %v, %v; want an error", ok, err)
	}
	if _, ok, err := DecodeJournalRow(journalops.Row{Seq: 15, Op: "create", IssueID: "gc-other", IssueJSON: bdImage(t, b)}); !ok || !errors.Is(err, ErrCorrupt) {
		t.Errorf("a post-image of another row = %v, %v; want ErrCorrupt", ok, err)
	}
	tampered := b
	tampered.Metadata = map[string]string{"gc.ev.schema": "1"}
	if _, ok, err := DecodeJournalRow(journalops.Row{Seq: 16, Op: "create", IssueID: b.ID, IssueJSON: bdImage(t, tampered)}); !ok || !errors.Is(err, ErrCorrupt) {
		t.Errorf("a corrupt fact's create = %v, %v; want ok and ErrCorrupt", ok, err)
	}
}

// A metadata value bd stored as a JSON scalar reads as its text, as the
// store's own projection reads it.
func TestDecodeJournalRowReadsScalarMetadata(t *testing.T) {
	b, err := Encode("gc", testFact())
	if err != nil {
		t.Fatal(err)
	}
	var image map[string]any
	if err := json.Unmarshal([]byte(bdImage(t, b)), &image); err != nil {
		t.Fatal(err)
	}
	meta := image["metadata"].(map[string]any)
	meta["gc.ev.schema"] = 1
	meta["gc.ev.gen"] = 3
	meta["gc.ev.n"] = 1
	data, _ := json.Marshal(image)
	if _, ok, err := DecodeJournalRow(journalops.Row{Seq: 1, Op: "create", IssueID: b.ID, IssueJSON: string(data)}); !ok || err != nil {
		t.Fatalf("DecodeJournalRow = %v, %v", ok, err)
	}
}

// TestDecodeJournalRowReadsARealStoresPostImage writes a fact through a real
// embedded store opened through gc's production open with the journal on,
// tails the journal, and decodes the create from its post-image alone. It is
// the check that bd journals a no-history create with its labels and
// metadata, and that the post-image has the shape journalImage reads.
func TestDecodeJournalRowReadsARealStoresPostImage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BD_EVENTS_JOURNAL", "")
	ctx := context.Background()
	scopeRoot := t.TempDir()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	seed, err := beadslib.OpenBestAvailable(ctx, beadsDir)
	if err != nil {
		t.Skipf("upstream beads storage unavailable: %v", err)
	}
	if err := seed.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("SetConfig(issue_prefix): %v", err)
	}
	if err := seed.SetConfig(ctx, "types.custom", BeadType); err != nil {
		t.Fatalf("SetConfig(types.custom): %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed storage: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("events-journal: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := beads.OpenNativeDoltStoreAt(ctx, scopeRoot, nil)
	if err != nil {
		t.Fatalf("OpenNativeDoltStoreAt: %v", err)
	}
	t.Cleanup(func() {
		if err := store.CloseStore(); err != nil {
			t.Errorf("CloseStore: %v", err)
		}
	})
	if !store.EventsJournalActive() {
		t.Fatal("EventsJournalActive = false on a workspace that enables the journal")
	}

	var rows []journalops.Row
	tailer := &ledgerfeed.Tailer{
		Source: store,
		Apply: func(_ context.Context, page journalops.Page) error {
			rows = append(rows, page.Rows...)
			return nil
		},
		OnGap: func(_ context.Context, gap ledgerfeed.Gap) error {
			t.Errorf("unexpected gap: %+v", gap)
			return nil
		},
	}
	if _, err := tailer.Baseline(ctx); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	f := testFact()
	b, err := Encode("gc", f)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID != b.ID {
		t.Fatalf("the store minted %s for pinned id %s", created.ID, b.ID)
	}
	if created.Status != "closed" {
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("closing the fact the store created open: %v", err)
		}
	}
	// A second create of the same pinned id is refused, so a sealed batch
	// retried after an ambiguous failure cannot record a fact twice; the
	// journal check below sees exactly one create.
	if _, err := store.Create(b); err == nil {
		t.Fatal("a second create of the same pinned id succeeded")
	}
	stored, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fromStore, err := Decode(stored); err != nil {
		t.Fatalf("Decode(the stored row): %v", err)
	} else {
		want := f
		want.ID = b.ID
		sameFact(t, fromStore, want)
	}

	for range 100 {
		more, err := tailer.Step(ctx)
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if !more {
			break
		}
	}
	var decoded []Fact
	creates := 0
	for _, row := range rows {
		if row.IssueID == b.ID && row.Op == "create" {
			creates++
		}
		got, ok, err := DecodeJournalRow(row)
		if err != nil {
			t.Fatalf("DecodeJournalRow(seq %d, %s): %v\npost-image: %s", row.Seq, row.Op, err, row.IssueJSON)
		}
		if ok {
			decoded = append(decoded, got)
		}
	}
	if creates != 1 {
		t.Fatalf("the journal holds %d creates of %s, want 1: %+v", creates, b.ID, rows)
	}
	if len(decoded) != 1 {
		t.Fatalf("decoded %d facts from %d journal rows, want 1: %+v", len(decoded), len(rows), rows)
	}
	want := f
	want.ID = b.ID
	sameFact(t, decoded[0], want)
	if !decoded[0].Event.Ts.Equal(time.Date(2026, 10, 15, 12, 30, 45, 123456789, time.UTC)) {
		t.Fatalf("Ts = %v", decoded[0].Event.Ts)
	}
}

// bd accepts any JSON as metadata, so a row that is not a fact can carry a
// metadata array or string. Classification reads no metadata: such a row is
// skipped, not an error the projector would stall on. A fact row whose
// metadata is not an object is corrupt.
func TestDecodeJournalRowClassifiesBeforeReadingMetadata(t *testing.T) {
	withMetadata := func(b beads.Bead, metadata string) string {
		var image map[string]json.RawMessage
		if err := json.Unmarshal([]byte(bdImage(t, b)), &image); err != nil {
			t.Fatal(err)
		}
		image["metadata"] = json.RawMessage(metadata)
		data, err := json.Marshal(image)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	task := beads.Bead{ID: "gc-1", Title: "t", Status: "open", Type: "task"}
	opRow := beads.Bead{ID: "gc-op-x", Title: "o", Status: "open", Type: "op", Labels: []string{"gc:op"}}
	for _, metadata := range []string{`"oops"`, `[1,2]`, `7`, `null`} {
		for _, b := range []beads.Bead{task, opRow} {
			row := journalops.Row{Seq: 1, Op: "create", IssueID: b.ID, IssueJSON: withMetadata(b, metadata)}
			if _, ok, err := DecodeJournalRow(row); ok || err != nil {
				t.Errorf("%s with metadata %s: DecodeJournalRow = %v, %v; want skipped", b.Type, metadata, ok, err)
			}
		}
	}
	fact, err := Encode("gc", testFact())
	if err != nil {
		t.Fatal(err)
	}
	for _, metadata := range []string{`"oops"`, `[1,2]`, `null`} {
		row := journalops.Row{Seq: 2, Op: "create", IssueID: fact.ID, IssueJSON: withMetadata(fact, metadata)}
		if _, ok, err := DecodeJournalRow(row); !ok || !errors.Is(err, ErrCorrupt) {
			t.Errorf("a fact with metadata %s: DecodeJournalRow = %v, %v; want ok and ErrCorrupt", metadata, ok, err)
		}
	}
}
