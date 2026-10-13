package beads

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/journalops"

	"github.com/gastownhall/gascity/internal/beads/ledgerfeed"
)

// nativeJournalReaderSpy is a storage that implements the journal read role
// with a scripted answer.
type nativeJournalReaderSpy struct {
	nativeDoltStorageSpy
	read   func(ctx context.Context, since int64, limit int) (journalops.Page, error)
	closed bool
}

func (s *nativeJournalReaderSpy) ReadEventsJournalPage(ctx context.Context, since int64, limit int) (journalops.Page, error) {
	return s.read(ctx, since, limit)
}

func (s *nativeJournalReaderSpy) SetEventsJournalEnabled(bool) {}

func (s *nativeJournalReaderSpy) IsClosed() bool { return s.closed }

func TestNativeDoltStoreReadEventsJournalPageForwardsTheRead(t *testing.T) {
	want := journalops.Page{Rows: []journalops.Row{{Seq: 4, IssueID: "gc-1"}}, Head: 9}
	var gotSince int64
	var gotLimit int
	spy := &nativeJournalReaderSpy{read: func(_ context.Context, since int64, limit int) (journalops.Page, error) {
		gotSince, gotLimit = since, limit
		return want, nil
	}}
	store := newNativeDoltStoreForTest(spy)

	page, err := store.ReadEventsJournalPage(context.Background(), 3, 50)
	if err != nil {
		t.Fatalf("ReadEventsJournalPage: %v", err)
	}
	if gotSince != 3 || gotLimit != 50 || page.Head != 9 || len(page.Rows) != 1 || page.Rows[0].Seq != 4 {
		t.Fatalf("page = %+v (since=%d limit=%d)", page, gotSince, gotLimit)
	}
}

func TestNativeDoltStoreReadEventsJournalPageKeepsTruncationTyped(t *testing.T) {
	spy := &nativeJournalReaderSpy{read: func(context.Context, int64, int) (journalops.Page, error) {
		return journalops.Page{}, &journalops.TruncatedError{Since: 1, Floor: 10, Head: 20}
	}}
	_, err := newNativeDoltStoreForTest(spy).ReadEventsJournalPage(context.Background(), 1, 0)
	var truncated *journalops.TruncatedError
	if !errors.As(err, &truncated) || truncated.Floor != 10 || truncated.Head != 20 {
		t.Fatalf("error = %v, want a *journalops.TruncatedError carrying its window", err)
	}
}

func TestNativeDoltStoreReadEventsJournalPageHonorsCallerCancel(t *testing.T) {
	spy := &nativeJournalReaderSpy{read: func(ctx context.Context, _ int64, _ int) (journalops.Page, error) {
		<-ctx.Done()
		return journalops.Page{}, errors.New("read aborted")
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newNativeDoltStoreForTest(spy).ReadEventsJournalPage(ctx, 0, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestNativeDoltStoreReadEventsJournalPageRefusesABackendWithoutAJournal(t *testing.T) {
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{})
	if _, err := store.ReadEventsJournalPage(context.Background(), 0, 0); !errors.Is(err, ErrEventsJournalUnsupported) {
		t.Fatalf("error = %v, want ErrEventsJournalUnsupported", err)
	}
	if store.EventsJournalActive() {
		t.Fatal("a store over a storage nobody activated reports an active journal")
	}
}

func TestNativeDoltStoreReadEventsJournalPageOnAClosedStore(t *testing.T) {
	spy := &nativeJournalReaderSpy{nativeDoltStorageSpy: nativeDoltStorageSpy{close: func() error { return nil }}}
	store := newNativeDoltStoreForTest(spy)
	if err := store.CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}
	if _, err := store.ReadEventsJournalPage(context.Background(), 0, 0); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("error = %v, want ErrStoreClosed", err)
	}
	if store.EventsJournalActive() {
		t.Fatal("a closed store reports an active journal")
	}
}

func TestNativeDoltStoreEventsJournalActiveReportsTheOpenActivation(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			t.Setenv("BD_EVENTS_JOURNAL", "")
			beadsDir := t.TempDir()
			if enabled {
				if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("events-journal: true\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			spy := &nativeJournalReaderSpy{}
			oldOpen := nativeDoltOpenBestAvailable
			t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
			nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) { return spy, nil }

			storage, err := openNativeDoltStorage(context.Background(), beadsDir)
			if err != nil {
				t.Fatalf("openNativeDoltStorage: %v", err)
			}
			if got := newNativeDoltStoreForTest(storage).EventsJournalActive(); got != enabled {
				t.Fatalf("EventsJournalActive = %v, want %v", got, enabled)
			}
		})
	}
}

func TestNativeJournalActivationSetDropsClosedHandles(t *testing.T) {
	set := &nativeJournalActivationSet{}
	first := &nativeJournalReaderSpy{}
	second := &nativeJournalReaderSpy{}
	set.record(first)
	if !set.active(first) || set.active(second) {
		t.Fatal("the set does not track handles by identity")
	}
	first.closed = true
	set.record(second)
	if set.active(first) {
		t.Fatal("a closed handle survived the next record")
	}
	if !set.active(second) || len(set.handles) != 1 {
		t.Fatalf("handles = %v, want only the live one", set.handles)
	}
}

func TestProxiedStoreEventsJournalReadsThroughTheNativeLeaf(t *testing.T) {
	store, native, _ := newSplitFixture(t)
	want := journalops.Page{Rows: []journalops.Row{{Seq: 1, IssueID: "prx-1"}}, Head: 1}
	native.storage = &nativeJournalReaderSpy{read: func(context.Context, int64, int) (journalops.Page, error) {
		return want, nil
	}}

	page, err := store.ReadEventsJournalPage(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("ReadEventsJournalPage: %v", err)
	}
	if page.Head != 1 || len(page.Rows) != 1 || page.Rows[0].IssueID != "prx-1" {
		t.Fatalf("page = %+v", page)
	}
}

func TestProxiedStoreEventsJournalUnavailableWhenDemoted(t *testing.T) {
	store, _, _ := newSplitFixture(t)
	store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone, "bd restarted its proxy", nil))

	if _, err := store.ReadEventsJournalPage(context.Background(), 0, 0); !errors.Is(err, ErrEventsJournalUnavailable) {
		t.Fatalf("error = %v, want ErrEventsJournalUnavailable", err)
	}
	if store.EventsJournalActive() {
		t.Fatal("a demoted store reports an active journal")
	}
}

// TestNativeDoltStoreLedgerfeedTailsRealWrites drives a real embedded store
// opened through gc's production open: the tailer sees the store's own
// writes in commit order, each carrying the issue's post-image.
func TestNativeDoltStoreLedgerfeedTailsRealWrites(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{name: "enabled", enabled: true},
		{name: "disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("BD_EVENTS_JOURNAL", "")
			ctx := context.Background()
			scopeRoot := t.TempDir()
			beadsDir := filepath.Join(scopeRoot, ".beads")
			seed, err := beadslib.OpenBestAvailable(ctx, beadsDir)
			if err != nil {
				t.Skipf("upstream beads storage unavailable: %v", err)
			}
			if err := seed.SetConfig(ctx, nativeIssuePrefixConfigKey, "gc"); err != nil {
				t.Fatalf("SetConfig(issue_prefix): %v", err)
			}
			if err := seed.Close(); err != nil {
				t.Fatalf("close seed storage: %v", err)
			}
			if tc.enabled {
				if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("events-journal: true\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			store, err := OpenNativeDoltStoreAt(ctx, scopeRoot, nil)
			if err != nil {
				t.Fatalf("OpenNativeDoltStoreAt: %v", err)
			}
			t.Cleanup(func() {
				if err := store.CloseStore(); err != nil {
					t.Fatalf("CloseStore: %v", err)
				}
			})
			if got := store.EventsJournalActive(); got != tc.enabled {
				t.Fatalf("EventsJournalActive = %v, want %v", got, tc.enabled)
			}

			var rows []journalops.Row
			tailer := &ledgerfeed.Tailer{
				Source:    store,
				PageLimit: 2,
				Apply: func(_ context.Context, page journalops.Page) error {
					rows = append(rows, page.Rows...)
					return nil
				},
				OnGap: func(gap ledgerfeed.Gap) { t.Errorf("unexpected gap: %+v", gap) },
			}
			if _, err := tailer.Baseline(ctx); err != nil {
				t.Fatalf("Baseline: %v", err)
			}

			first, err := store.Create(Bead{Title: "first"})
			if err != nil {
				t.Fatalf("Create first: %v", err)
			}
			retitled := "first, retitled"
			if err := store.Update(first.ID, UpdateOpts{Title: &retitled}); err != nil {
				t.Fatalf("Update: %v", err)
			}
			second, err := store.Create(Bead{Title: "second"})
			if err != nil {
				t.Fatalf("Create second: %v", err)
			}
			if err := store.Close(first.ID); err != nil {
				t.Fatalf("Close: %v", err)
			}

			drainTailer(ctx, t, tailer)
			if !tc.enabled {
				if len(rows) != 0 {
					t.Fatalf("a workspace with the journal off recorded %d rows", len(rows))
				}
				return
			}

			var ids []string
			for i, row := range rows {
				if i > 0 && row.Seq != rows[i-1].Seq+1 {
					t.Fatalf("rows are not gapless and seq-ascending: %+v", rows)
				}
				ids = append(ids, row.IssueID)
			}
			if want := []string{first.ID, first.ID, second.ID, first.ID}; !slices.Equal(ids, want) {
				t.Fatalf("journal issue order = %v, want %v (commit order)", ids, want)
			}
			if title := postImage(t, rows[1]).Title; title != retitled {
				t.Fatalf("update row post-image title = %q, want %q", title, retitled)
			}
			if status := postImage(t, rows[3]).Status; status != "closed" {
				t.Fatalf("close row post-image status = %q, want closed", status)
			}
			if cp := tailer.Checkpoint(); cp.Seq != rows[3].Seq || cp.Last.IssueID != first.ID {
				t.Fatalf("checkpoint = %+v, want the last row applied", cp)
			}
		})
	}
}

// drainTailer steps until the tailer reports it is caught up.
func drainTailer(ctx context.Context, t *testing.T, tailer *ledgerfeed.Tailer) {
	t.Helper()
	for range 100 {
		more, err := tailer.Step(ctx)
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if !more {
			return
		}
	}
	t.Fatal("tailer did not catch up in 100 steps")
}

// journalPostImage is the slice of a journal row's IssueJSON these tests read.
type journalPostImage struct {
	Title  string `json:"title"`
	Status string `json:"status"`
}

func postImage(t *testing.T, row journalops.Row) journalPostImage {
	t.Helper()
	var image journalPostImage
	if err := json.Unmarshal([]byte(row.IssueJSON), &image); err != nil {
		t.Fatalf("decoding row %d IssueJSON %q: %v", row.Seq, row.IssueJSON, err)
	}
	return image
}
