package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
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
	t.Setenv("BD_EVENTS_JOURNAL", "")
	beadsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("events-journal: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nativeEventsJournalActivations.record(spy, beadsDir)
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

// A caller that has given up by the time the read finds the journal off, or
// missing, still gets that refusal rather than its own context error: the
// refusal answers for the scope, and a tailer marks a lapse only on it. On the
// proxied lane the refusal comes back without the budget verdict the chain
// wraps it in, so a caller's cancel still demotes nothing.
func TestNativeDoltStoreReadEventsJournalPageRefusalOutlivesTheCallerCancel(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	inactive := func() *nativeJournalReaderSpy {
		return &nativeJournalReaderSpy{read: func(context.Context, int64, int) (journalops.Page, error) {
			return journalops.Page{}, nil
		}}
	}
	t.Run("inactive", func(t *testing.T) {
		if _, err := newNativeDoltStoreForTest(inactive()).ReadEventsJournalPage(canceled, 0, 0); !errors.Is(err, ErrEventsJournalInactive) {
			t.Fatalf("error = %v, want ErrEventsJournalInactive", err)
		}
	})
	t.Run("unsupported", func(t *testing.T) {
		if _, err := newNativeDoltStoreForTest(&nativeDoltStorageSpy{}).ReadEventsJournalPage(canceled, 0, 0); !errors.Is(err, ErrEventsJournalUnsupported) {
			t.Fatalf("error = %v, want ErrEventsJournalUnsupported", err)
		}
	})
	t.Run("proxied, unsupported", func(t *testing.T) {
		store, native, _ := newSplitFixture(t)
		store.journal = proxiedJournalActivation{resolved: true, active: true}
		native.proxiedReadVerdicts = true
		native.storage = &nativeDoltStorageSpy{}
		_, err := store.ReadEventsJournalPage(canceled, 0, 0)
		if !errors.Is(err, ErrEventsJournalUnsupported) {
			t.Fatalf("error = %v, want ErrEventsJournalUnsupported", err)
		}
		if _, typed := ProxiedVerdictOf(err); typed {
			t.Fatalf("error = %v carries a proxied verdict for the caller's own cancel", err)
		}
		if store.Demoted() {
			t.Fatal("the caller's cancel stood the native leaf down")
		}
	})
	t.Run("through a tailer", func(t *testing.T) {
		tailer := &ledgerfeed.Tailer{
			Source: newNativeDoltStoreForTest(inactive()),
			Lapse:  func(err error) bool { return errors.Is(err, ErrEventsJournalInactive) },
			Apply:  func(context.Context, journalops.Page) error { return nil },
			OnGap:  func(context.Context, ledgerfeed.Gap) error { return nil },
		}
		if _, err := tailer.Step(canceled); !errors.Is(err, ErrEventsJournalInactive) {
			t.Fatalf("Step error = %v, want ErrEventsJournalInactive", err)
		}
		if cp := tailer.Checkpoint(); !cp.Lapsed {
			t.Fatalf("checkpoint = %+v, want it marked Lapsed", cp)
		}
	})
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

// bd's CLI re-resolves activation on every invocation, so a direct scope
// that turns the journal off stops journaling agents' writes at once. The
// store re-checks the scope's resolution within nativeJournalActivationRecheck,
// so it stops answering "caught up" over writes nobody recorded; turning the
// journal back on restores the handle it was opened with.
func TestNativeDoltStoreEventsJournalActiveFollowsTheScopeAfterOpen(t *testing.T) {
	t.Setenv("BD_EVENTS_JOURNAL", "")
	beadsDir := t.TempDir()
	cfg := filepath.Join(beadsDir, "config.yaml")
	writeConfig := func(body string) {
		t.Helper()
		if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("events-journal: true\n")
	now := time.Unix(1_000_000, 0)
	oldNow := nativeEventsJournalActivations.now
	t.Cleanup(func() { nativeEventsJournalActivations.now = oldNow })
	nativeEventsJournalActivations.now = func() time.Time { return now }

	spy := &nativeJournalReaderSpy{read: func(context.Context, int64, int) (journalops.Page, error) {
		return journalops.Page{Head: 7}, nil
	}}
	t.Cleanup(func() { spy.closed = true })
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) { return spy, nil }
	storage, err := openNativeDoltStorage(context.Background(), beadsDir)
	if err != nil {
		t.Fatalf("openNativeDoltStorage: %v", err)
	}
	store := newNativeDoltStoreForTest(storage)
	if !store.EventsJournalActive() {
		t.Fatal("EventsJournalActive = false on a handle opened with the journal on")
	}

	writeConfig("events-journal: false\n")
	if !store.EventsJournalActive() {
		t.Fatal("EventsJournalActive re-read the scope's config before the recheck interval passed")
	}
	now = now.Add(nativeJournalActivationRecheck)
	if store.EventsJournalActive() {
		t.Fatal("EventsJournalActive = true after the scope turned the journal off")
	}
	if _, err := store.ReadEventsJournalPage(context.Background(), 0, 0); !errors.Is(err, ErrEventsJournalInactive) {
		t.Fatalf("error = %v, want ErrEventsJournalInactive", err)
	}
	if _, ok := ActiveEventsJournal(store); ok {
		t.Fatal("ActiveEventsJournal offered a journal the scope turned off")
	}

	writeConfig("events-journal: true\n")
	now = now.Add(nativeJournalActivationRecheck)
	if !store.EventsJournalActive() {
		t.Fatal("EventsJournalActive = false after the scope turned the journal back on")
	}
	if _, err := store.ReadEventsJournalPage(context.Background(), 0, 0); err != nil {
		t.Fatalf("ReadEventsJournalPage after the journal came back: %v", err)
	}
}

// A consumer recognizes the store's Inactive refusal as a lapse
// (ledgerfeed.Tailer.Lapse), so the scope switching its journal off and back
// on reaches it as one GapInactive gap. Otherwise the tail would resume as if
// caught up over the writes bd made while the journal was off, which have no
// rows.
func TestNativeDoltStoreJournalLapseIsATailerGap(t *testing.T) {
	t.Setenv("BD_EVENTS_JOURNAL", "")
	ctx := context.Background()
	beadsDir := t.TempDir()
	writeConfig := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("events-journal: true\n")
	now := time.Unix(1_000_000, 0)
	oldNow := nativeEventsJournalActivations.now
	t.Cleanup(func() { nativeEventsJournalActivations.now = oldNow })
	nativeEventsJournalActivations.now = func() time.Time { return now }

	// The handle journals its own writes throughout, so the seqs run on
	// across the lapse with nothing missing between them.
	head := int64(2)
	spy := &nativeJournalReaderSpy{read: func(_ context.Context, since int64, _ int) (journalops.Page, error) {
		page := journalops.Page{Head: head}
		for seq := since + 1; seq <= head; seq++ {
			page.Rows = append(page.Rows, journalops.Row{Seq: seq, Op: "update", IssueID: "gc-1"})
		}
		return page, nil
	}}
	t.Cleanup(func() { spy.closed = true })
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) { return spy, nil }
	storage, err := openNativeDoltStorage(ctx, beadsDir)
	if err != nil {
		t.Fatalf("openNativeDoltStorage: %v", err)
	}

	var applied []int64
	var gaps []ledgerfeed.Gap
	tailer := &ledgerfeed.Tailer{
		Source: newNativeDoltStoreForTest(storage),
		Lapse:  func(err error) bool { return errors.Is(err, ErrEventsJournalInactive) },
		Apply: func(_ context.Context, page journalops.Page) error {
			for _, row := range page.Rows {
				applied = append(applied, row.Seq)
			}
			return nil
		},
		OnGap: func(_ context.Context, gap ledgerfeed.Gap) error {
			gaps = append(gaps, gap)
			return nil
		},
	}
	drainTailer(ctx, t, tailer)

	writeConfig("events-journal: false\n")
	now = now.Add(nativeJournalActivationRecheck)
	if _, err := tailer.Step(ctx); !errors.Is(err, ErrEventsJournalInactive) {
		t.Fatalf("Step error = %v, want ErrEventsJournalInactive", err)
	}
	if cp := tailer.Checkpoint(); cp.Seq != 2 || !cp.Lapsed {
		t.Fatalf("checkpoint = %+v, want seq 2 marked Lapsed", cp)
	}

	head = 4
	writeConfig("events-journal: true\n")
	now = now.Add(nativeJournalActivationRecheck)
	drainTailer(ctx, t, tailer)
	if len(gaps) != 1 || gaps[0].Kind != ledgerfeed.GapInactive || gaps[0].To.Seq != 4 {
		t.Fatalf("gaps = %+v, want one GapInactive to seq 4", gaps)
	}
	if !slices.Equal(applied, []int64{1, 2}) {
		t.Fatalf("applied = %v, want [1 2]: nothing applied over the lapse", applied)
	}
}

// A handle opened with the journal off never journals gc's own writes, so
// the scope turning the journal on later does not make it tailable.
func TestNativeDoltStoreEventsJournalActiveNeedsTheOpenActivation(t *testing.T) {
	t.Setenv("BD_EVENTS_JOURNAL", "")
	beadsDir := t.TempDir()
	spy := &nativeJournalReaderSpy{}
	t.Cleanup(func() { spy.closed = true })
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) { return spy, nil }
	storage, err := openNativeDoltStorage(context.Background(), beadsDir)
	if err != nil {
		t.Fatalf("openNativeDoltStorage: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("events-journal: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if newNativeDoltStoreForTest(storage).EventsJournalActive() {
		t.Fatal("EventsJournalActive = true on a handle opened with the journal off")
	}
}

func TestNativeJournalActivationSetDropsClosedHandles(t *testing.T) {
	set := &nativeJournalActivationSet{}
	first := &nativeJournalReaderSpy{}
	second := &nativeJournalReaderSpy{}
	set.record(first, "")
	if !set.active(first) || set.active(second) {
		t.Fatal("the set does not track handles by identity")
	}
	first.closed = true
	set.record(second, "")
	if set.active(first) {
		t.Fatal("a closed handle survived the next record")
	}
	if !set.active(second) || len(set.handles) != 1 {
		t.Fatalf("handles = %v, want only the live one", set.handles)
	}
}

// active drops mu while it resolves the scope, so the handle can close, or be
// recorded anew, before it re-locks. The answer it resolved is then about a
// handle that is gone, and it reports false rather than that answer.
func TestNativeJournalActivationSetRefusesAHandleThatChangedWhileResolving(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(set *nativeJournalActivationSet, spy *nativeJournalReaderSpy)
		want   bool
	}{
		{name: "unchanged", change: func(*nativeJournalActivationSet, *nativeJournalReaderSpy) {}, want: true},
		{name: "closed", change: func(_ *nativeJournalActivationSet, spy *nativeJournalReaderSpy) { spy.closed = true }},
		{name: "recorded anew", change: func(set *nativeJournalActivationSet, spy *nativeJournalReaderSpy) {
			set.record(spy, "/elsewhere/.beads")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(1_000_000, 0)
			set := &nativeJournalActivationSet{now: func() time.Time { return now }}
			spy := &nativeJournalReaderSpy{}
			set.resolve = func(string) bool {
				tc.change(set, spy)
				return true
			}
			set.record(spy, "/scope/.beads")
			now = now.Add(nativeJournalActivationRecheck)
			if got := set.active(spy); got != tc.want {
				t.Fatalf("active = %v, want %v", got, tc.want)
			}
		})
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

// A proxied native open withholds the whole BD_ namespace for its window. A
// proxied activation resolution that ran inside it would read the scope's
// config alone, over an ambient BD_EVENTS_JOURNAL=0, and cache the inverted
// answer. It holds no store lock and has no earlier answer to give, so it
// waits the window out instead.
func TestProxiedEventsJournalResolutionWaitsOutAnOpenWindow(t *testing.T) {
	t.Setenv("BD_EVENTS_JOURNAL", "0")
	scopeRoot := t.TempDir()
	writeScopeJournalConfig(t, scopeRoot, "events-journal: true\n")
	endWindow := holdOpenWindow(t)
	got := make(chan bool, 1)
	go func() { got <- proxiedEventsJournalActive(PinForTest(scopeRoot, "", "beads")) }()
	select {
	case answer := <-got:
		t.Fatalf("the resolution answered %v inside the window; it must wait on nativeDoltOpenEnvMu", answer)
	case <-time.After(100 * time.Millisecond):
	}
	endWindow()
	select {
	case answer := <-got:
		if answer {
			t.Fatal("the resolution answered true over the ambient BD_EVENTS_JOURNAL=0")
		}
	case <-time.After(openWindowPatience):
		t.Fatal("the resolution did not answer after the window closed")
	}
}

// The native re-check runs under a store's read lock, so unlike the proxied
// resolution it does not wait an open window out: inside one it keeps the
// last answer and caches nothing, and the first question after the window
// resolves the scope again.
func TestNativeJournalRecheckKeepsTheLastAnswerThroughAnOpenWindow(t *testing.T) {
	t.Setenv("BD_EVENTS_JOURNAL", "0")
	scopeRoot := t.TempDir()
	writeScopeJournalConfig(t, scopeRoot, "events-journal: true\n")
	recorded := time.Unix(1_000_000, 0)
	now := recorded
	set := &nativeJournalActivationSet{now: func() time.Time { return now }}
	spy := &nativeJournalReaderSpy{}
	set.record(spy, filepath.Join(scopeRoot, ".beads"))
	now = now.Add(nativeJournalActivationRecheck)

	endWindow := holdOpenWindow(t)
	got := make(chan bool, 1)
	go func() { got <- set.active(spy) }()
	select {
	case answer := <-got:
		if !answer {
			t.Fatal("active = false inside the window; it must keep the last answer, true")
		}
	case <-time.After(openWindowPatience):
		t.Fatalf("active is still waiting %v into the window; it must not wait on nativeDoltOpenEnvMu", openWindowPatience)
	}
	endWindow()
	if checked := set.handles[spy].checkedAt; !checked.Equal(recorded) {
		t.Fatalf("checkedAt = %v, want %v: an answer kept inside the window must not be cached", checked, recorded)
	}
	if set.active(spy) {
		t.Fatal("active = true after the window over the ambient BD_EVENTS_JOURNAL=0; the scope was not resolved again")
	}
	if checked := set.handles[spy].checkedAt; !checked.Equal(now) {
		t.Fatalf("checkedAt = %v, want %v after the scope resolved again", checked, now)
	}
}

// A direct native store re-checks its scope's activation under its read lock.
// Another scope's open holds nativeDoltOpenEnvMu for its whole window — up to
// that open's full budget against a wedged proxy — so a re-check that waited
// for it would hold the read lock as long: the journal read would outlive its
// caller's ctx, and a CloseStore queued behind it would stall every reader of
// the store. Each step below must finish while the window still stands.
func TestNativeDoltStoreJournalRecheckDoesNotWaitOutAnOpenWindow(t *testing.T) {
	t.Setenv("BD_EVENTS_JOURNAL", "")
	beadsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("events-journal: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000_000, 0)
	oldNow := nativeEventsJournalActivations.now
	t.Cleanup(func() { nativeEventsJournalActivations.now = oldNow })
	nativeEventsJournalActivations.now = func() time.Time { return now }
	spy := &nativeJournalReaderSpy{read: func(context.Context, int64, int) (journalops.Page, error) {
		return journalops.Page{Head: 7}, nil
	}}
	t.Cleanup(func() { spy.closed = true })
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) { return spy, nil }
	storage, err := openNativeDoltStorage(context.Background(), beadsDir)
	if err != nil {
		t.Fatalf("openNativeDoltStorage: %v", err)
	}
	store := newNativeDoltStoreForTest(storage)
	now = now.Add(nativeJournalActivationRecheck)

	endWindow := holdOpenWindow(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var blocked []chan error
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{name: "a journal read with a 50ms ctx", run: func() error {
			page, err := store.ReadEventsJournalPage(ctx, 0, 0)
			if err == nil && page.Head != 7 {
				return fmt.Errorf("page = %+v, want the spy's head 7", page)
			}
			return err
		}},
		{name: "EventsJournalActive", run: func() error {
			if !store.EventsJournalActive() {
				return errors.New("false; the re-check keeps the last answer, true")
			}
			return nil
		}},
		{name: "CloseStore", run: store.CloseStore},
		{name: "an ordinary reader", run: func() error {
			_, release, err := store.acquireStorage()
			if err == nil {
				release()
				return errors.New("acquired the storage of a closed store")
			}
			if !errors.Is(err, ErrStoreClosed) {
				return err
			}
			return nil
		}},
	} {
		// A step that is still blocked is reported and left running, so the
		// steps after it show what queues behind it.
		done := make(chan error, 1)
		go func() { done <- step.run() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s inside the window: %v", step.name, err)
			}
		case <-time.After(openWindowPatience):
			t.Errorf("%s is still blocked %v into another scope's open window", step.name, openWindowPatience)
			blocked = append(blocked, done)
		}
	}
	// The blocked steps finish once the window ends; wait for them before
	// cleanup resets the clock and the spy they read.
	endWindow()
	for _, done := range blocked {
		select {
		case <-done:
		case <-time.After(openWindowPatience):
			t.Fatal("a blocked step did not finish after the window ended")
		}
	}
}

// openWindowPatience bounds a wait on an answer while a test holds an open
// window. An answer that does not wait on the window comes back far sooner,
// and one that does never comes back while the window stands.
const openWindowPatience = 5 * time.Second

// holdOpenWindow stands up a native open's window — nativeDoltOpenEnvMu held
// and the BEADS_ and BD_ namespaces withheld, as openNativeStorageProxied
// holds them — and returns the func that ends it. The test's cleanup ends it
// too, so a test that fails inside it does not leave it standing.
func holdOpenWindow(t *testing.T) (end func()) {
	t.Helper()
	nativeDoltOpenEnvMu.Lock()
	restore, err := withWithheldPrefixesLocked(beadsEnvPrefix, bdEnvPrefix)
	if err != nil {
		nativeDoltOpenEnvMu.Unlock()
		t.Fatalf("withholding the window's namespaces: %v", err)
	}
	var once sync.Once
	end = func() {
		once.Do(func() {
			restore()
			nativeDoltOpenEnvMu.Unlock()
		})
	}
	t.Cleanup(end)
	return end
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

// The refusals are ordered longest-lasting first, so a caller that retries on
// the transient ErrEventsJournalUnavailable never spins on a store that is
// closed or not journaling. A leaf dropped while the activation resolved is
// refused as unavailable ahead of inactive: that answer may be the dropped
// leaf's pin's, and the next read answers for the current one.
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
	t.Run("closed, inside another open's window", func(t *testing.T) {
		// A closed store resolves no activation, so it does not wait the
		// window out for an answer it cannot use.
		store, _, _ := newSplitFixture(t)
		if err := store.CloseStore(); err != nil {
			t.Fatalf("CloseStore: %v", err)
		}
		holdOpenWindow(t)
		read := make(chan error, 1)
		go func() {
			_, err := store.ReadEventsJournalPage(context.Background(), 0, 0)
			read <- err
		}()
		select {
		case err := <-read:
			if !errors.Is(err, ErrStoreClosed) {
				t.Fatalf("error = %v, want ErrStoreClosed", err)
			}
		case <-time.After(openWindowPatience):
			t.Fatalf("a read on a closed store is still waiting %v into another open's window", openWindowPatience)
		}
	})
	t.Run("closed while resolving", func(t *testing.T) {
		// Closing drops the leaf too, and closed is the longer-lasting answer.
		store, leafClosed := resolvingJournalFixture(t, "events-journal: true\n")
		err := readWhileTheLeafIsDropped(t, store, leafClosed, func() {
			if err := store.CloseStore(); err != nil {
				t.Fatalf("CloseStore: %v", err)
			}
		})
		if !errors.Is(err, ErrStoreClosed) || errors.Is(err, ErrEventsJournalUnavailable) {
			t.Fatalf("error = %v, want ErrStoreClosed alone", err)
		}
	})
	t.Run("leaf dropped while resolving, and inactive", func(t *testing.T) {
		store, leafClosed := resolvingJournalFixture(t, "events-journal: false\n")
		err := readWhileTheLeafIsDropped(t, store, leafClosed, func() {
			store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone, "bd restarted its proxy", nil))
		})
		if !errors.Is(err, ErrEventsJournalUnavailable) || errors.Is(err, ErrEventsJournalInactive) {
			t.Fatalf("error = %v, want ErrEventsJournalUnavailable alone", err)
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

// Resolving the activation can wait out another native open's whole window
// (TestProxiedEventsJournalResolutionWaitsOutAnOpenWindow), and a guard tick
// that stands the leaf down or re-pins it inside that wait drops the leaf the
// read started from. The read must not go on with the dropped leaf: closed by
// then, it refuses with ErrStoreClosed, which tells the caller a store that is
// open has closed. The read refuses as transiently unavailable instead, and
// the next read answers for the leaf now serving. A leaf dropped while the
// read is in flight on it is refused the same way
// (TestProxiedStoreEventsJournalReadRefusesALeafDroppedDuringTheRead).
func TestProxiedStoreEventsJournalReadRefusesALeafDroppedWhileResolving(t *testing.T) {
	t.Run("stand-down", func(t *testing.T) {
		store, leafClosed := resolvingJournalFixture(t, "events-journal: true\n")
		err := readWhileTheLeafIsDropped(t, store, leafClosed, func() {
			store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone, "bd restarted its proxy", nil))
		})
		if !errors.Is(err, ErrEventsJournalUnavailable) || errors.Is(err, ErrStoreClosed) {
			t.Fatalf("error = %v, want ErrEventsJournalUnavailable and not ErrStoreClosed", err)
		}
	})
	t.Run("re-pin", func(t *testing.T) {
		store, leafClosed := resolvingJournalFixture(t, "events-journal: true\n")
		recovered := newNativeDoltStoreForTest(journalPageSpy("prx-recovered"))
		err := readWhileTheLeafIsDropped(t, store, leafClosed, func() {
			if !store.repin(recovered, store.Pin()) {
				t.Fatal("repin refused an open store with no terminal verdict")
			}
		})
		if !errors.Is(err, ErrEventsJournalUnavailable) || errors.Is(err, ErrStoreClosed) || errors.Is(err, ErrEventsJournalInactive) {
			t.Fatalf("error = %v, want ErrEventsJournalUnavailable alone", err)
		}
		page, err := store.ReadEventsJournalPage(context.Background(), 0, 0)
		if err != nil {
			t.Fatalf("the read after the re-pin: %v", err)
		}
		if len(page.Rows) != 1 || page.Rows[0].IssueID != "prx-recovered" {
			t.Fatalf("the read after the re-pin got page %+v, want the re-pinned leaf's", page)
		}
	})
}

// The leaf can also be dropped while the read is in flight on it, and one bd
// proxy restart does both halves: the read meets the dead socket and waits in
// the reconnect its reopen hook runs, and the guard tick or another reader's
// verdict stands the leaf down, or re-pins it, meanwhile. The dropped leaf
// latches closed, so the read comes back with the leaf's ErrStoreClosed from a
// store that is still open. It refuses as transiently unavailable instead.
// Closing the store itself latches it closed before it drops the leaf, so a
// read the close cuts short still answers ErrStoreClosed.
func TestProxiedStoreEventsJournalReadRefusesALeafDroppedDuringTheRead(t *testing.T) {
	t.Run("stand-down", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			store, finish := readHeldInTheReconnect(t)
			store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone, "bd restarted its proxy", nil))
			if err := finish(); !errors.Is(err, ErrEventsJournalUnavailable) || errors.Is(err, ErrStoreClosed) {
				t.Fatalf("error = %v, want ErrEventsJournalUnavailable and not ErrStoreClosed", err)
			}
		})
	})
	t.Run("re-pin", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			store, finish := readHeldInTheReconnect(t)
			if !store.repin(newNativeDoltStoreForTest(journalPageSpy("prx-recovered")), store.Pin()) {
				t.Fatal("repin refused an open store with no terminal verdict")
			}
			if err := finish(); !errors.Is(err, ErrEventsJournalUnavailable) || errors.Is(err, ErrStoreClosed) {
				t.Fatalf("error = %v, want ErrEventsJournalUnavailable and not ErrStoreClosed", err)
			}
			page, err := store.ReadEventsJournalPage(context.Background(), 0, 0)
			if err != nil {
				t.Fatalf("the read after the re-pin: %v", err)
			}
			if len(page.Rows) != 1 || page.Rows[0].IssueID != "prx-recovered" {
				t.Fatalf("the read after the re-pin got page %+v, want the re-pinned leaf's", page)
			}
		})
	})
	t.Run("closed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			store, finish := readHeldInTheReconnect(t)
			// The store closes its leaf in line, and the leaf's close waits for
			// the reconnect the read holds, so the close runs beside the read.
			closed := make(chan error, 1)
			go func() { closed <- store.CloseStore() }()
			if err := finish(); !errors.Is(err, ErrStoreClosed) || errors.Is(err, ErrEventsJournalUnavailable) {
				t.Fatalf("error = %v, want ErrStoreClosed alone", err)
			}
			select {
			case err := <-closed:
				if err != nil {
					t.Fatalf("CloseStore: %v", err)
				}
			default:
				t.Fatal("CloseStore did not return after the reconnect ended")
			}
		})
	})
}

// readHeldInTheReconnect starts a journal read on a proxied store whose leaf
// fails every page read on a dead socket, and returns once the read waits in
// the reconnect that starts, as a reopen hook re-admitting against a
// restarting bd proxy holds it. The caller, in a synctest bubble, then drops
// the leaf; finish checks that the leaf latched closed, releases the
// reconnect, and returns the read's error. A leaf's close latches it before it
// waits for the reconnect the read holds, so the latch has landed once every
// other goroutine in the bubble is blocked. The store is pinned to a scope
// with the journal on, so a read after a re-pin answers for the new leaf.
func readHeldInTheReconnect(t *testing.T) (store *ProxiedStore, finish func() error) {
	t.Helper()
	t.Setenv("BD_EVENTS_JOURNAL", "")
	scopeRoot := t.TempDir()
	writeScopeJournalConfig(t, scopeRoot, "events-journal: true\n")
	store, leaf := proxiedJournalFixture(t, &nativeJournalReaderSpy{read: func(context.Context, int64, int) (journalops.Page, error) {
		return journalops.Page{}, io.EOF
	}})
	store.pin = PinForTest(scopeRoot, "", "beads")
	reconnecting, held := make(chan struct{}), make(chan struct{})
	var reconnectOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(held) }) }
	t.Cleanup(release)
	leaf.reopen = func(context.Context) (NativeStorage, error) {
		reconnectOnce.Do(func() { close(reconnecting) })
		<-held
		return journalPageSpy("prx-reopened"), nil
	}
	read := make(chan error, 1)
	go func() {
		_, err := store.ReadEventsJournalPage(context.Background(), 0, 0)
		read <- err
	}()
	synctest.Wait()
	select {
	case <-reconnecting:
	default:
		t.Fatal("the read did not start a reconnect")
	}
	return store, func() error {
		t.Helper()
		synctest.Wait()
		if !nativeDoltStoreClosedForTest(leaf) {
			t.Fatal("the dropped leaf did not latch closed")
		}
		release()
		synctest.Wait()
		select {
		case err := <-read:
			return err
		default:
			t.Fatal("the read did not answer after the reconnect was released")
			return nil
		}
	}
}

// journalPageSpy is a journal read whose every page is one row for issue.
func journalPageSpy(issue string) *nativeJournalReaderSpy {
	return &nativeJournalReaderSpy{read: func(context.Context, int64, int) (journalops.Page, error) {
		return journalops.Page{Rows: []journalops.Row{{Seq: 1, IssueID: issue}}, Head: 1}, nil
	}}
}

// resolvingJournalFixture is proxiedJournalFixture pinned to a scope whose bd
// config is config, with the activation not resolved yet. The returned
// channel closes when the native leaf is closed, which a stand-down or re-pin
// does off the caller's path to the leaf it drops.
func resolvingJournalFixture(t *testing.T, config string) (*ProxiedStore, <-chan struct{}) {
	t.Helper()
	t.Setenv("BD_EVENTS_JOURNAL", "")
	scopeRoot := t.TempDir()
	writeScopeJournalConfig(t, scopeRoot, config)
	spy := journalPageSpy("prx-dropped")
	leafClosed := make(chan struct{})
	spy.close = func() error {
		close(leafClosed)
		return nil
	}
	store, _ := proxiedJournalFixture(t, spy)
	store.pin = PinForTest(scopeRoot, "", "beads")
	store.forgetJournalActivation()
	return store, leafClosed
}

// readWhileTheLeafIsDropped reads store's journal while another open's window
// stands, runs drop once the read is resolving the activation, waits for the
// dropped leaf to close, and returns the read's error once the window ends.
func readWhileTheLeafIsDropped(t *testing.T, store *ProxiedStore, leafClosed <-chan struct{}, drop func()) error {
	t.Helper()
	resolving := make(chan struct{})
	var once sync.Once
	oldResolve := proxiedJournalResolve
	t.Cleanup(func() { proxiedJournalResolve = oldResolve })
	proxiedJournalResolve = func(pin Pin) bool {
		once.Do(func() { close(resolving) })
		return oldResolve(pin)
	}
	endWindow := holdOpenWindow(t)
	read := make(chan error, 1)
	go func() {
		_, err := store.ReadEventsJournalPage(context.Background(), 0, 0)
		read <- err
	}()
	select {
	case <-resolving:
	case <-time.After(openWindowPatience):
		t.Fatal("the read did not start resolving the activation")
	}
	drop()
	select {
	case <-leafClosed:
	case <-time.After(openWindowPatience):
		t.Fatal("the dropped leaf was not closed")
	}
	endWindow()
	select {
	case err := <-read:
		return err
	case <-time.After(openWindowPatience):
		t.Fatal("the read did not answer after the window ended")
		return nil
	}
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

// A tick that decides nothing (here an unreadable record) still refreshes
// activation: the forget is the first thing every tick does, not a step a
// tick reaches only when the record and cursors hold.
func TestProxiedGuardUndecidedTickRefreshesJournalActivation(t *testing.T) {
	t.Setenv("BD_EVENTS_JOURNAL", "")
	f := newGuardFixture(t)
	f.start()
	writeScopeJournalConfig(t, f.admitted.scopeRoot, "events-journal: true\n")
	if !f.store.EventsJournalActive() {
		t.Fatal("EventsJournalActive = false on a scope that enables the journal")
	}

	writeScopeJournalConfig(t, f.admitted.scopeRoot, "events-journal: false\n")
	f.admitted.removeRecord()
	if step := f.tick(); step != proxiedGuardUndecided {
		t.Fatalf("tick = %s, want undecided", step)
	}
	if f.store.EventsJournalActive() {
		t.Fatal("EventsJournalActive = true after an undecided tick on a scope that turned the journal off")
	}
}

// A forget that lands while a resolution is reading the scope's config wins:
// the raced answer is returned to its own caller but not cached, so the next
// question resolves again.
func TestProxiedStoreDoesNotCacheAResolutionThatRacedAForget(t *testing.T) {
	f := newGuardFixture(t)
	t.Cleanup(func() { _ = f.store.CloseStore() })
	var calls int
	oldResolve := proxiedJournalResolve
	t.Cleanup(func() { proxiedJournalResolve = oldResolve })
	proxiedJournalResolve = func(Pin) bool {
		calls++
		if calls == 1 {
			// A guard tick lands mid-resolution.
			f.store.forgetJournalActivation()
			return true
		}
		return false
	}
	if !f.store.EventsJournalActive() {
		t.Fatal("the first question did not get its own resolution")
	}
	if f.store.EventsJournalActive() {
		t.Fatal("EventsJournalActive = true: a resolution that raced a forget was cached over it")
	}
	if calls != 2 {
		t.Fatalf("resolved %d time(s), want 2", calls)
	}
	if f.store.EventsJournalActive() || calls != 2 {
		t.Fatalf("an unraced resolution was not cached (calls = %d)", calls)
	}
}

// A re-pin installs a new pin and leaf, so the re-pin forgets activation
// itself rather than relying on the tick that ran it.
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

// A non-terminal stand-down recovers through the tick's recoverNative and its
// re-pin. The re-pin itself forgets the cached activation (the second subtest
// pins that without a tick), so a scope that changed its answer while the handle was
// demoted is re-resolved on the recovered leaf. A reader that asked while the
// store was demoted (and was refused as unavailable) cached the old answer,
// which is what the re-pin must not keep.
func TestProxiedGuardRecoveryRefreshesJournalActivation(t *testing.T) {
	newRecoveredLeaf := func() *NativeDoltStore {
		leaf := newNativeDoltStoreForTest(&nativeDoltMemStorage{store: &MemStore{storageOnly: true, IDPrefix: "prx", HonorExplicitIDs: true}})
		leaf.idPrefix = "prx"
		return leaf
	}
	demoteWithACachedAnswer := func(t *testing.T, f *guardFixture) {
		t.Helper()
		writeScopeJournalConfig(t, f.admitted.scopeRoot, "events-journal: true\n")
		if !f.store.EventsJournalActive() {
			t.Fatal("EventsJournalActive = false on a scope that enables the journal")
		}
		f.store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone, "bd restarted its proxy", nil))
		t.Setenv("BD_EVENTS_JOURNAL", "0")
		if _, err := f.store.ReadEventsJournalPage(context.Background(), 0, 0); !errors.Is(err, ErrEventsJournalUnavailable) {
			t.Fatalf("demoted read error = %v, want ErrEventsJournalUnavailable from the cached activation", err)
		}
	}

	t.Run("the recovery tick", func(t *testing.T) {
		t.Setenv("BD_EVENTS_JOURNAL", "")
		f := newGuardFixture(t)
		f.start()
		f.recover = func(context.Context) (*NativeDoltStore, Pin, error) {
			return newRecoveredLeaf(), f.store.Pin(), nil
		}
		demoteWithACachedAnswer(t, f)
		if step := f.tick(); step != proxiedGuardRepinned {
			t.Fatalf("tick = %s, want repinned", step)
		}
		if f.store.EventsJournalActive() {
			t.Fatal("EventsJournalActive = true after recovering under BD_EVENTS_JOURNAL=0")
		}
	})

	t.Run("the re-pin itself", func(t *testing.T) {
		t.Setenv("BD_EVENTS_JOURNAL", "")
		f := newGuardFixture(t)
		t.Cleanup(func() { _ = f.store.CloseStore() })
		demoteWithACachedAnswer(t, f)
		if !f.store.repin(newRecoveredLeaf(), f.store.Pin()) {
			t.Fatal("repin refused a non-terminally demoted store")
		}
		if f.store.EventsJournalActive() {
			t.Fatal("EventsJournalActive = true after a re-pin under BD_EVENTS_JOURNAL=0")
		}
	})
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

// The cache carries no journal methods of its own — carrying them would
// advertise a journal over every cached backing, bd and memory included — so
// a consumer discovers the journal through ActiveEventsJournal, which unwraps
// the cache and applies the activation predicate.
func TestActiveEventsJournalFindsTheJournalBehindACache(t *testing.T) {
	want := journalops.Page{Rows: []journalops.Row{{Seq: 2, IssueID: "gc-2"}}, Head: 2}
	spy := activatedJournalSpy(t, &nativeJournalReaderSpy{read: func(_ context.Context, since int64, _ int) (journalops.Page, error) {
		if since != 1 {
			t.Errorf("since = %d, want 1", since)
		}
		return want, nil
	}})
	var cache Store = NewCachingStoreForTest(newNativeDoltStoreForTest(spy), nil)
	if _, ok := cache.(journalops.Journal); ok {
		t.Fatal("the cache carries the journal read itself; it would advertise one over every backing")
	}

	journal, ok := ActiveEventsJournal(cache)
	if !ok {
		t.Fatal("ActiveEventsJournal missed the active journal behind the cache")
	}
	page, err := journal.ReadEventsJournalPage(context.Background(), 1, 10)
	if err != nil || page.Head != 2 || len(page.Rows) != 1 {
		t.Fatalf("ReadEventsJournalPage = (%+v, %v)", page, err)
	}
}

func TestActiveEventsJournalRefusesWhatCannotBeTailed(t *testing.T) {
	inactive := &nativeJournalReaderSpy{}
	for name, store := range map[string]Store{
		"a cached store with no journal": NewCachingStoreForTest(NewMemStore(), nil),
		"a store with no journal":        NewMemStore(),
		"an inactive native journal":     NewCachingStoreForTest(newNativeDoltStoreForTest(inactive), nil),
		"nil":                            nil,
	} {
		if journal, ok := ActiveEventsJournal(store); ok || journal != nil {
			t.Errorf("%s: ActiveEventsJournal = (%v, %v), want (nil, false)", name, journal, ok)
		}
	}
}

// The outermost store that carries the journal answers for activation: a
// ProxiedStore's Backing is its native leaf, whose own activation is not the
// scope's, so discovery must not unwrap past the proxied answer.
func TestActiveEventsJournalTakesTheProxiedAnswerOverItsLeaf(t *testing.T) {
	store, native, _ := newSplitFixture(t)
	native.storage = activatedJournalSpy(t, &nativeJournalReaderSpy{read: func(context.Context, int64, int) (journalops.Page, error) {
		return journalops.Page{}, nil
	}})
	cache := NewCachingStoreForTest(store, nil)

	store.journal = proxiedJournalActivation{resolved: true, active: false}
	if _, ok := ActiveEventsJournal(cache); ok {
		t.Fatal("discovery unwrapped past an inactive proxied store to its activated native leaf")
	}
	store.journal = proxiedJournalActivation{resolved: true, active: true}
	journal, ok := ActiveEventsJournal(cache)
	if !ok || journal != journalops.Journal(store) {
		t.Fatalf("ActiveEventsJournal = (%T, %v), want the proxied store itself", journal, ok)
	}
}

// cyclicBackingStore is a wrapper whose Backing is itself: a chain that never
// reaches a journal or an end.
type cyclicBackingStore struct{ Store }

func (s *cyclicBackingStore) Backing() Store { return s }

// EventsJournalCarrier leaves activation to its caller, so it finds the
// carrier of a journal that is off — a feed tails one the scope turns on
// later — and, as ActiveEventsJournal does, stops at the outermost carrier.
func TestEventsJournalCarrierFindsTheCarrierActiveOrNot(t *testing.T) {
	native := newNativeDoltStoreForTest(&nativeJournalReaderSpy{})
	cache := NewCachingStoreForTest(native, nil)
	if _, ok := ActiveEventsJournal(cache); ok {
		t.Fatal("the native journal is active; this case needs one that is off")
	}
	if got := EventsJournalCarrier(cache); got != Store(native) {
		t.Errorf("EventsJournalCarrier(a cache over an inactive native journal) = %T, want the native store", got)
	}

	proxied, _, _ := newSplitFixture(t)
	if got := EventsJournalCarrier(NewCachingStoreForTest(proxied, nil)); got != Store(proxied) {
		t.Errorf("EventsJournalCarrier(a cache over a proxied store) = %T, want the proxied store, not its leaf", got)
	}
}

func TestEventsJournalCarrierRefusesAChainWithNoJournal(t *testing.T) {
	for name, store := range map[string]Store{
		"a cached store with no journal": NewCachingStoreForTest(NewMemStore(), nil),
		"a store with no journal":        NewMemStore(),
		"a Backing chain that cycles":    &cyclicBackingStore{Store: NewMemStore()},
		"nil":                            nil,
	} {
		if got := EventsJournalCarrier(store); got != nil {
			t.Errorf("%s: EventsJournalCarrier = %T, want nil", name, got)
		}
	}
}
