//go:build integration

package beads

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// openTestNativeDoltStore opens a native store on a throwaway dolt
// sql-server (startTestDoltServer: a free port, never 3307, 45865 or 8123,
// stopped by its own process handle).
func openTestNativeDoltStore(t *testing.T, prefix string) *NativeDoltStore {
	t.Helper()
	ctx := context.Background()
	port := startTestDoltServer(t)
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","database":"beads","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%d}`, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	storage, err := beadslib.OpenBestAvailable(ctx, beadsDir)
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	if err := storage.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetConfig(ctx, "types.custom", strings.Join(RequiredCustomTypes, ",")); err != nil {
		t.Fatal(err)
	}
	return newNativeDoltStoreWithStorageAndPrefix(storage, "batch-create", prefix)
}

// reverseShapedRows are rows as the reverse migration writes them into a work
// store: foreign and own prefixes, closed rows with a close reason, wisps and
// no-history rows, metadata, labels, priorities, a step ref, and creation
// times in the past.
func reverseShapedRows(n int) []Bead {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]Bead, 0, n)
	for i := 0; i < n; i++ {
		prefix := []string{"gcg", "gcs", "mc"}[i%3]
		b := Bead{
			ID:        fmt.Sprintf("%s-%06d", prefix, i),
			Title:     "row " + strconv.Itoa(i),
			Type:      []string{"task", "message", "session", "molecule"}[i%4],
			Status:    []string{"open", "closed", "in_progress", "closed"}[i%4],
			CreatedAt: base.Add(time.Duration(i) * time.Second),
			UpdatedAt: base.Add(time.Duration(i)*time.Second + time.Hour),
			Assignee:  "mayor",
			From:      "deacon",
			Labels:    []string{"l" + strconv.Itoa(i%5)},
			Metadata:  StringMap{"gc.infra_reversed_from": "infra/run", "n": strconv.Itoa(i)},
			Ephemeral: i%5 == 0,
			NoHistory: i%7 == 3 && i%5 != 0,
			Ref:       "step-" + strconv.Itoa(i),
		}
		if b.Status == "closed" {
			b.CloseReason = "done"
		}
		if i%6 == 0 {
			p := i % 4
			b.Priority = &p
		}
		rows = append(rows, b)
	}
	return rows
}

// TestNativeDoltCreateWithForeignIDBatchKeepsRowsAsCreatedFormSays creates
// reverse-shaped rows in batches of 500, one commit each, and proves every
// row reads back exactly as CreatedForm says it will. GC_TEST_REVERSE_ROWS
// raises the count (41800 for MC's binding) to time it.
func TestNativeDoltCreateWithForeignIDBatchKeepsRowsAsCreatedFormSays(t *testing.T) {
	n := 1500
	if v, err := strconv.Atoi(os.Getenv("GC_TEST_REVERSE_ROWS")); err == nil && v > 0 {
		n = v
	}
	store := openTestNativeDoltStore(t, "mc")
	rows := reverseShapedRows(n)
	if os.Getenv("GC_TEST_REVERSE_DURABLE") != "" {
		for i := range rows {
			rows[i].Ephemeral, rows[i].NoHistory, rows[i].Type = false, false, "task"
		}
	}
	started := time.Now()
	if os.Getenv("GC_TEST_REVERSE_PER_ROW") != "" {
		for _, b := range rows {
			if _, err := store.CreateWithForeignID(b); err != nil {
				t.Fatalf("%s: %v", b.ID, err)
			}
		}
	} else {
		for start := 0; start < len(rows); start += 500 {
			if err := store.CreateWithForeignIDBatch(rows[start:min(start+500, len(rows))]); err != nil {
				t.Fatalf("batch at %d: %v", start, err)
			}
		}
	}
	elapsed := time.Since(started)
	t.Logf("created %d rows (per-row=%v durable-only=%v) in %s (%.2f ms/row)", n, os.Getenv("GC_TEST_REVERSE_PER_ROW") != "", os.Getenv("GC_TEST_REVERSE_DURABLE") != "", elapsed.Round(time.Millisecond), float64(elapsed.Microseconds())/1000/float64(n))
	for i := 1; i < len(rows); i += 10 {
		if err := store.DepAdd(rows[i].ID, rows[i-1].ID, "parent-child"); err != nil {
			t.Fatalf("adding the parent edge of %s: %v", rows[i].ID, err)
		}
		rows[i].ParentID = rows[i-1].ID
	}
	listed, err := store.List(ListQuery{IncludeClosed: true, TierMode: TierBoth, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]Bead, len(listed))
	for _, b := range listed {
		got[b.ID] = b
	}
	if len(got) != n {
		t.Fatalf("the store lists %d rows, want %d", len(got), n)
	}
	for _, want := range rows {
		form, err := store.CreatedForm(want)
		if err != nil {
			t.Fatal(err)
		}
		g, ok := got[want.ID]
		if !ok {
			t.Fatalf("%s is missing", want.ID)
		}
		if diff := createdFormDifference(form, g); diff != "" {
			t.Fatalf("%s reads back unlike its created form: %s", want.ID, diff)
		}
	}
	// A taken id refuses the whole batch.
	if err := store.CreateWithForeignIDBatch([]Bead{{ID: "gcg-new", Title: "new"}, {ID: rows[0].ID, Title: "taken"}}); err == nil {
		t.Fatal("a batch with a taken id was created")
	}
	if _, err := store.Get("gcg-new"); err == nil {
		t.Fatal("a refused batch stored part of itself")
	}
}

// createdFormDifference compares the fields a copy carries, the way the
// reverse's verify does.
func createdFormDifference(want, got Bead) string {
	type view struct {
		ID, Title, Status, Type, Assignee, From, ParentID, Ref, Description, CloseReason string
		Priority                                                                         *int
		Created                                                                          int64
		Updated                                                                          int64
		Labels                                                                           []string
		Metadata                                                                         StringMap
		Ephemeral, NoHistory                                                             bool
	}
	render := func(b Bead) view {
		v := view{b.ID, b.Title, b.Status, b.Type, b.Assignee, b.From, b.ParentID, b.Ref, b.Description, b.CloseReason, b.Priority, b.CreatedAt.Unix(), 0, b.Labels, b.Metadata, b.Ephemeral, b.NoHistory}
		if !b.UpdatedAt.IsZero() {
			v.Updated = b.UpdatedAt.Unix()
		}
		if len(v.Labels) == 0 {
			v.Labels = nil
		}
		return v
	}
	w, g := render(want), render(got)
	if w.Updated == 0 {
		g.Updated = 0
	}
	if !reflect.DeepEqual(w, g) {
		return fmt.Sprintf("\n got  %+v\n want %+v", g, w)
	}
	return ""
}
