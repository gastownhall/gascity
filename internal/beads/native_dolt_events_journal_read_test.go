package beads

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

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

// activatedJournalSpy records spy as an activated handle, as
// openNativeDoltStorage does for a workspace with the journal on, and
// retires it when the test ends.
func activatedJournalSpy(t *testing.T, spy *nativeJournalReaderSpy) *nativeJournalReaderSpy {
	t.Helper()
	nativeEventsJournalActivations.record(spy)
	t.Cleanup(func() { spy.closed = true })
	return spy
}

func TestNativeDoltStoreReadEventsJournalPageForwardsTheRead(t *testing.T) {
	want := journalops.Page{Rows: []journalops.Row{{Seq: 4, IssueID: "gc-1"}}, Head: 9}
	var gotSince int64
	var gotLimit int
	spy := activatedJournalSpy(t, &nativeJournalReaderSpy{read: func(_ context.Context, since int64, limit int) (journalops.Page, error) {
		gotSince, gotLimit = since, limit
		return want, nil
	}})
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
	spy := activatedJournalSpy(t, &nativeJournalReaderSpy{read: func(context.Context, int64, int) (journalops.Page, error) {
		return journalops.Page{}, &journalops.TruncatedError{Since: 1, Floor: 10, Head: 20}
	}})
	_, err := newNativeDoltStoreForTest(spy).ReadEventsJournalPage(context.Background(), 1, 0)
	// journalops asks implementations to return the error itself rather than
	// wrap it, so a type assertion recovers it, not only errors.As.
	truncated, ok := err.(*journalops.TruncatedError) //nolint:errorlint // the contract is the bare pointer
	if !ok || truncated.Floor != 10 || truncated.Head != 20 {
		t.Fatalf("error = %v (%T), want the bare *journalops.TruncatedError carrying its window", err, err)
	}
}

func TestNativeDoltStoreReadEventsJournalPageHonorsCallerCancel(t *testing.T) {
	spy := activatedJournalSpy(t, &nativeJournalReaderSpy{read: func(ctx context.Context, _ int64, _ int) (journalops.Page, error) {
		<-ctx.Done()
		return journalops.Page{}, errors.New("read aborted")
	}})
	// The store's own budget is far longer than the caller's patience, so a
	// read that ignored the caller's ctx would sit out the budget instead.
	store := newNativeDoltStoreForTest(spy, WithNativeReadRetryBudget(10*time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := store.ReadEventsJournalPage(ctx, 0, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("read took %v after the caller canceled; it waited out the store's budget instead of the caller's ctx", elapsed)
	}
}

func TestNativeDoltStoreReadEventsJournalPageRefusesAnInactiveJournal(t *testing.T) {
	read := false
	spy := &nativeJournalReaderSpy{read: func(context.Context, int64, int) (journalops.Page, error) {
		read = true
		return journalops.Page{}, nil
	}}
	store := newNativeDoltStoreForTest(spy)
	if _, err := store.ReadEventsJournalPage(context.Background(), 0, 0); !errors.Is(err, ErrEventsJournalInactive) {
		t.Fatalf("error = %v, want ErrEventsJournalInactive", err)
	}
	if read {
		t.Fatal("an inactive journal was read; an idle answer would read as caught up")
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
	store.journal = proxiedJournalActivation{resolved: true, active: true}
	want := journalops.Page{Rows: []journalops.Row{{Seq: 1, IssueID: "prx-1"}}, Head: 1}
	// The leaf's own activation is not consulted: bd does the writes.
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
	if !store.EventsJournalActive() {
		t.Fatal("EventsJournalActive = false on a serving, journaled scope")
	}
}

func TestProxiedStoreRefusesAnInactiveJournal(t *testing.T) {
	store, _, _ := newSplitFixture(t)
	if _, err := store.ReadEventsJournalPage(context.Background(), 0, 0); !errors.Is(err, ErrEventsJournalInactive) {
		t.Fatalf("error = %v, want ErrEventsJournalInactive", err)
	}
	if store.EventsJournalActive() {
		t.Fatal("EventsJournalActive = true on a scope whose writes are not journaled")
	}
}

// bd does every proxied write and resolves activation env-first, so the
// proxied answer must too, even though the native leaf opens with the BD_
// namespace withheld.
func TestProxiedEventsJournalActiveFollowsBdPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, env, config string
		want              bool
	}{
		{name: "config on", config: "events-journal: true\n", want: true},
		{name: "env off beats config on", env: "0", config: "events-journal: true\n"},
		{name: "env on without config", env: "1", want: true},
		{name: "neither"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BD_EVENTS_JOURNAL", tc.env)
			scope := t.TempDir()
			if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.config != "" {
				if err := os.WriteFile(filepath.Join(scope, ".beads", "config.yaml"), []byte(tc.config), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := proxiedEventsJournalActive(PinForTest(scope, "", "beads")); got != tc.want {
				t.Fatalf("proxiedEventsJournalActive = %v, want %v", got, tc.want)
			}
		})
	}
}

// proxiedJournalFixture is newSplitFixture with the native leaf latched onto
// the proxied verdict lane, as OpenNativeDoltStoreAtProxied latches every
// production proxied leaf, reading through spy.
func proxiedJournalFixture(t *testing.T, spy *nativeJournalReaderSpy) (*ProxiedStore, *NativeDoltStore) {
	t.Helper()
	store, native, _ := newSplitFixture(t)
	store.journal = proxiedJournalActivation{resolved: true, active: true}
	native.proxiedReadVerdicts = true
	native.storage = spy
	return store, native
}

// A caller that gives up mid-reconnect is not a verdict about the endpoint: on
// the proxied lane the exhausted chain would otherwise surface as a
// budget_exhausted verdict, and the wrapper would stand the leaf down.
func TestProxiedStoreEventsJournalCallerDeadlineDoesNotDemote(t *testing.T) {
	store, native := proxiedJournalFixture(t, &nativeJournalReaderSpy{read: func(context.Context, int64, int) (journalops.Page, error) {
		return journalops.Page{}, errors.New("begin read tx: dial tcp 127.0.0.1:3307: i/o timeout")
	}})
	native.readRetryBudgetOverride = 10 * time.Second
	native.reopen = func(ctx context.Context) (NativeStorage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := store.ReadEventsJournalPage(ctx, 0, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the caller's context.DeadlineExceeded", err)
	}
	if _, typed := ProxiedVerdictOf(err); typed {
		t.Fatalf("error = %v carries a proxied verdict for the caller's own deadline", err)
	}
	if store.Demoted() {
		t.Fatal("the caller's deadline stood the native leaf down")
	}
}

// A terminal endpoint answer read through the journal demotes the store the
// way every other native read does, and the journal is then unavailable.
func TestProxiedStoreEventsJournalTerminalVerdictDemotes(t *testing.T) {
	store, _ := proxiedJournalFixture(t, &nativeJournalReaderSpy{read: func(context.Context, int64, int) (journalops.Page, error) {
		return journalops.Page{}, errors.New("begin read tx: Error 1049 (42000): Unknown database 'beads'")
	}})

	_, err := store.ReadEventsJournalPage(context.Background(), 0, 0)
	if verdict, typed := ProxiedVerdictOf(err); !typed || !verdict.Terminal() {
		t.Fatalf("error = %v, want a terminal proxied verdict", err)
	}
	if !store.Demoted() {
		t.Fatal("a terminal verdict on the journal read left the native leaf serving")
	}
	if _, err := store.ReadEventsJournalPage(context.Background(), 0, 0); !errors.Is(err, ErrEventsJournalUnavailable) {
		t.Fatalf("read after the demotion = %v, want ErrEventsJournalUnavailable", err)
	}
	if store.EventsJournalActive() {
		t.Fatal("a demoted store reports an active journal")
	}
}

func TestProxiedStoreEventsJournalUnavailableWhenDemoted(t *testing.T) {
	store, _, _ := newSplitFixture(t)
	store.journal = proxiedJournalActivation{resolved: true, active: true}
	store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone, "bd restarted its proxy", nil))

	if _, err := store.ReadEventsJournalPage(context.Background(), 0, 0); !errors.Is(err, ErrEventsJournalUnavailable) {
		t.Fatalf("error = %v, want ErrEventsJournalUnavailable", err)
	}
	if store.EventsJournalActive() {
		t.Fatal("a demoted store reports an active journal")
	}
}

// The refusals are ordered permanent-first, so a caller that retries on the
// transient ErrEventsJournalUnavailable never spins on a store that is closed
// or never journals.
func TestProxiedStoreEventsJournalRefusalOrder(t *testing.T) {
	t.Run("closed", func(t *testing.T) {
		store, _, _ := newSplitFixture(t)
		store.journal = proxiedJournalActivation{resolved: true, active: true}
		if err := store.CloseStore(); err != nil {
			t.Fatalf("CloseStore: %v", err)
		}
		if _, err := store.ReadEventsJournalPage(context.Background(), 0, 0); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("error = %v, want ErrStoreClosed", err)
		}
	})
	t.Run("demoted and inactive", func(t *testing.T) {
		store, _, _ := newSplitFixture(t)
		store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone, "bd restarted its proxy", nil))
		_, err := store.ReadEventsJournalPage(context.Background(), 0, 0)
		if !errors.Is(err, ErrEventsJournalInactive) || errors.Is(err, ErrEventsJournalUnavailable) {
			t.Fatalf("error = %v, want ErrEventsJournalInactive alone", err)
		}
	})
}

// writeScopeJournalConfig writes the scope's bd config with body.
func writeScopeJournalConfig(t *testing.T, scopeRoot, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Opening a proxied store reads no journal config: activation is resolved on
// the first journal question, so a scope nobody tails pays nothing for it.
func TestProxiedStoreResolvesJournalActivationOnFirstUse(t *testing.T) {
	t.Setenv("BD_EVENTS_JOURNAL", "")
	f := newGuardFixture(t)
	t.Cleanup(func() { _ = f.store.CloseStore() })
	writeScopeJournalConfig(t, f.admitted.scopeRoot, "events-journal: true\n")
	if !f.store.EventsJournalActive() {
		t.Fatal("EventsJournalActive = false: activation was resolved at open, before the scope enabled the journal")
	}
}

// bd's CLI re-resolves activation on every invocation, so a scope that turns
// the journal off stops journaling at once. The guard tick re-resolves it, so
// the store stops answering "caught up" over writes nobody recorded.
func TestProxiedGuardTickRefreshesJournalActivation(t *testing.T) {
	t.Setenv("BD_EVENTS_JOURNAL", "")
	f := newGuardFixture(t)
	f.start()
	writeScopeJournalConfig(t, f.admitted.scopeRoot, "events-journal: true\n")
	if !f.store.EventsJournalActive() {
		t.Fatal("EventsJournalActive = false on a scope that enables the journal")
	}

	writeScopeJournalConfig(t, f.admitted.scopeRoot, "events-journal: false\n")
	if step := f.tick(); step != proxiedGuardHeld {
		t.Fatalf("tick = %s, want held", step)
	}
	if f.store.EventsJournalActive() {
		t.Fatal("EventsJournalActive = true after a tick on a scope that turned the journal off")
	}
	if _, err := f.store.ReadEventsJournalPage(context.Background(), 0, 0); !errors.Is(err, ErrEventsJournalInactive) {
		t.Fatalf("error = %v, want ErrEventsJournalInactive", err)
	}
}

// A re-pin ends its tick before the journal slot, so the re-pin re-resolves
// activation itself.
func TestProxiedGuardRepinRefreshesJournalActivation(t *testing.T) {
	t.Setenv("BD_EVENTS_JOURNAL", "")
	f := newGuardFixture(t)
	f.start()
	writeScopeJournalConfig(t, f.admitted.scopeRoot, "events-journal: true\n")
	if !f.store.EventsJournalActive() {
		t.Fatal("EventsJournalActive = false on a scope that enables the journal")
	}

	t.Setenv("BD_EVENTS_JOURNAL", "0")
	f.admitted.writeRecord(6002, "99887766")
	if step := f.tick(); step != proxiedGuardRepinned {
		t.Fatalf("tick = %s, want repinned", step)
	}
	if f.store.EventsJournalActive() {
		t.Fatal("EventsJournalActive = true after a re-pin under BD_EVENTS_JOURNAL=0")
	}
}

// TestNativeDoltStoreLedgerfeedTailsRealWrites drives a real embedded store
// opened through gc's production open with the journal on: the tailer sees
// the store's own writes in commit order, each carrying the issue's
// post-image. The journal-off answer is covered over a spy
// (TestNativeDoltStoreEventsJournalActiveReportsTheOpenActivation), which
// keeps this to one embedded Dolt open.
func TestNativeDoltStoreLedgerfeedTailsRealWrites(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("events-journal: true\n"), 0o644); err != nil {
		t.Fatal(err)
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
	if !store.EventsJournalActive() {
		t.Fatal("EventsJournalActive = false on a workspace that enables the journal")
	}

	var rows []journalops.Row
	tailer := &ledgerfeed.Tailer{
		Source:    store,
		PageLimit: 2,
		Apply: func(_ context.Context, page journalops.Page) error {
			rows = append(rows, page.Rows...)
			return nil
		},
		OnGap: func(_ context.Context, gap ledgerfeed.Gap) error {
			t.Errorf("unexpected gap: %+v", gap)
			return nil
		},
	}
	// Baseline before the writes, as a consumer baselines before its scan.
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

func TestCachingStoreForwardsTheEventsJournalToItsBacking(t *testing.T) {
	want := journalops.Page{Rows: []journalops.Row{{Seq: 2, IssueID: "gc-2"}}, Head: 2}
	spy := &nativeJournalReaderSpy{read: func(_ context.Context, since int64, _ int) (journalops.Page, error) {
		if since != 1 {
			t.Errorf("since = %d, want 1", since)
		}
		return want, nil
	}}
	activatedJournalSpy(t, spy)
	cache := NewCachingStoreForTest(newNativeDoltStoreForTest(spy), nil)

	page, err := cache.ReadEventsJournalPage(context.Background(), 1, 10)
	if err != nil || page.Head != 2 || len(page.Rows) != 1 {
		t.Fatalf("ReadEventsJournalPage = (%+v, %v)", page, err)
	}
	if !cache.EventsJournalActive() {
		t.Fatal("the cache hides its backing's activation")
	}
}

func TestCachingStoreOverAStoreWithoutAJournalRefuses(t *testing.T) {
	cache := NewCachingStoreForTest(NewMemStore(), nil)
	if _, err := cache.ReadEventsJournalPage(context.Background(), 0, 0); !errors.Is(err, ErrEventsJournalUnsupported) {
		t.Fatalf("error = %v, want ErrEventsJournalUnsupported", err)
	}
	if cache.EventsJournalActive() {
		t.Fatal("a cache over a store with no journal reports one active")
	}
}
