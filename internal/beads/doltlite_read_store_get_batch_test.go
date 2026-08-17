//go:build gascity_native_beads

package beads

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDoltliteReadStoreGetBatchMatchesPointGetAcrossTiers(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	ids := []string{"gc-tier-wisp", "gc-child", "gc-order-closed", "gc-tier-nohistory", "gc-tier-wisp"}
	wantByID := make(map[string]Bead)
	for _, id := range ids {
		if _, seen := wantByID[id]; seen {
			continue
		}
		bead, err := store.Get(id)
		if err != nil {
			t.Fatalf("point Get(%s): %v", id, err)
		}
		wantByID[id] = bead
	}

	got, err := store.GetBatch(ids)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	wantIDs := []string{"gc-tier-wisp", "gc-child", "gc-order-closed", "gc-tier-nohistory"}
	if gotIDs := batchRowIDs(got); !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("GetBatch IDs = %v, want %v", gotIDs, wantIDs)
	}
	for i, id := range wantIDs {
		if !reflect.DeepEqual(got[i], wantByID[id]) {
			t.Fatalf("GetBatch(%s) = %#v\npoint Get = %#v", id, got[i], wantByID[id])
		}
	}
	if got[1].ParentID != "gc-parent" {
		t.Fatalf("gc-child ParentID = %q, want gc-parent", got[1].ParentID)
	}
	if !got[0].Ephemeral || !got[3].NoHistory {
		t.Fatalf("storage flags lost: wisp=%#v no-history=%#v", got[0], got[3])
	}
}

func TestDoltliteReadStoreGetBatchIssuesWinCrossTierCollision(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	insertTestDoltliteIssue(t, writer, "wisps", "wisp_labels", "wisp_dependencies", testDoltliteIssue{
		ID: "gc-tier-issue", Title: "lower-authority wisp", Status: "closed", IssueType: "message",
		CreatedAt: time.Now().UTC().Add(time.Hour), Labels: []string{"wisp-only"},
		Metadata: map[string]string{"authority": "wisp"}, Ephemeral: true,
	})

	want, err := store.Get("gc-tier-issue")
	if err != nil {
		t.Fatalf("point Get: %v", err)
	}
	got, err := store.GetBatch([]string{"gc-tier-wisp", "gc-tier-issue"})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if !reflect.DeepEqual(got[1], want) {
		t.Fatalf("collision winner = %#v\npoint Get winner = %#v", got[1], want)
	}
	if got[1].Title != "tier issue" || got[1].Ephemeral || slices.Contains(got[1].Labels, "wisp-only") {
		t.Fatalf("cross-tier collision mixed lower-authority fields: %#v", got[1])
	}
}

func TestDoltliteReadStoreGetBatchFailsWholeBatch(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		store, closeStore := newTestDoltliteReadStore(t)
		defer closeStore()
		got, err := store.GetBatch([]string{"gc-session", "gc-missing"})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetBatch error = %v, want ErrNotFound", err)
		}
		if got != nil {
			t.Fatalf("GetBatch rows = %#v, want nil", got)
		}
	})

	t.Run("duplicate primary rows", func(t *testing.T) {
		store, closeStore := newTestDoltliteReadStore(t)
		defer closeStore()
		writer := openTestDoltliteWriter(t, store.db)
		defer writer.Close() //nolint:errcheck // test cleanup
		if _, err := writer.Exec(`INSERT INTO dependencies (
			issue_id, depends_on_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external, type
		) VALUES (?, ?, ?, '', '', 'parent-child')`, "gc-child", "gc-ready", "gc-ready"); err != nil {
			t.Fatalf("insert duplicate parent row: %v", err)
		}

		got, err := store.GetBatch([]string{"gc-child"})
		if err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("GetBatch error = %v, want duplicate-row failure", err)
		}
		if got != nil {
			t.Fatalf("GetBatch rows = %#v, want nil", got)
		}
	})

	t.Run("late wisp hydration error", func(t *testing.T) {
		store, closeStore := newTestDoltliteReadStore(t)
		defer closeStore()
		writer := openTestDoltliteWriter(t, store.db)
		if _, err := writer.Exec(`DROP TABLE wisp_labels`); err != nil {
			_ = writer.Close()
			t.Fatalf("drop wisp_labels: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("close writer: %v", err)
		}

		got, err := store.GetBatch([]string{"gc-session", "gc-tier-wisp"})
		if err == nil {
			t.Fatal("GetBatch error = nil, want late hydration failure")
		}
		if got != nil {
			t.Fatalf("GetBatch rows = %#v, want nil after partial hydration", got)
		}
	})
}

func TestDoltliteReadStoreGetBatchSkipsWispHydrationWhenPrimaryComplete(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	if _, err := writer.Exec(`DROP TABLE wisp_labels`); err != nil {
		_ = writer.Close()
		t.Fatalf("drop wisp_labels: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	got, err := store.GetBatch([]string{"gc-session", "gc-child"})
	if err != nil {
		t.Fatalf("GetBatch primary-only: %v", err)
	}
	if gotIDs := batchRowIDs(got); !slices.Equal(gotIDs, []string{"gc-session", "gc-child"}) {
		t.Fatalf("GetBatch IDs = %v", gotIDs)
	}
}

func TestDoltliteReadStoreGetBatchMatchesPointGetOnLegacySchema(t *testing.T) {
	store, closeStore := newLegacyTestDoltliteReadStore(t)
	defer closeStore()

	ids := []string{"gc-legacy-wisp", "gc-legacy-issue"}
	want := make([]Bead, 0, len(ids))
	for _, id := range ids {
		bead, err := store.Get(id)
		if err != nil {
			t.Fatalf("point Get(%s): %v", id, err)
		}
		want = append(want, bead)
	}

	got, err := store.GetBatch(ids)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GetBatch = %#v\npoint Gets = %#v", got, want)
	}
	if !got[0].Ephemeral {
		t.Fatalf("legacy wisp = %#v, want Ephemeral=true", got[0])
	}
}

func TestDoltliteReadStoreGetBatchEmptyDoesNotRead(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	closeStore()
	got, err := store.GetBatch(nil)
	if err != nil || got != nil {
		t.Fatalf("GetBatch(nil) after close = (%#v, %v), want (nil, nil)", got, err)
	}
}

var _ BatchGetter = (*DoltliteReadStore)(nil)
