package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// stubInvocationStoreFactory replaces the store factory with one that hands out
// a fresh close-counting store per open and returns every store it opened.
func stubInvocationStoreFactory(t *testing.T) *[]*closeCountingStore {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	opened := &[]*closeCountingStore{}
	restore := openStoreFactoryForCity
	openStoreFactoryForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		store := newCloseCountingStore(t, false)
		*opened = append(*opened, store)
		return beads.StoreOpenResult{Store: store}, nil
	}
	t.Cleanup(func() { openStoreFactoryForCity = restore })
	return opened
}

func TestInvocationStoreMemoOpensEachScopeOnce(t *testing.T) {
	opened := stubInvocationStoreFactory(t)
	city := t.TempDir()
	end := beginInvocationStoreMemo()
	t.Cleanup(func() { _ = end() })

	first, err := openStoreAtForCity(city, city)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	second, err := openCityStoreAt(city)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if len(*opened) != 1 {
		t.Fatalf("opened %d stores for one scope, want 1", len(*opened))
	}
	created, err := first.Create(beads.Bead{Title: "shared"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := second.Get(created.ID); err != nil {
		t.Fatalf("second handle does not see the first handle's write: %v", err)
	}
	other := t.TempDir()
	if _, err := openStoreAtForCity(other, other); err != nil {
		t.Fatalf("open other scope: %v", err)
	}
	if len(*opened) != 2 {
		t.Fatalf("opened %d stores for two scopes, want 2", len(*opened))
	}
}

func TestInvocationStoreMemoBorrowedCloseLeavesTheStoreOpen(t *testing.T) {
	opened := stubInvocationStoreFactory(t)
	city := t.TempDir()
	end := beginInvocationStoreMemo()

	// An owning frame closes what it opened, as openOwnedNudgeBeadStore's
	// callers do.
	borrowed, err := openStoreAtForCity(city, city)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := closeBeadStoreHandle(borrowed); err != nil {
		t.Fatalf("closing a borrowed handle: %v", err)
	}
	shared := (*opened)[0]
	if got := shared.closes(); got != 0 {
		t.Fatalf("closing a borrowed handle closed the shared store %d times", got)
	}
	again, err := openStoreAtForCity(city, city)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := again.Create(beads.Bead{Title: "still open"}); err != nil {
		t.Fatalf("store unusable after a borrowed close: %v", err)
	}
	if len(*opened) != 1 {
		t.Fatalf("reopen after a borrowed close opened %d stores, want 1", len(*opened))
	}

	if err := end(); err != nil {
		t.Fatalf("ending the memo: %v", err)
	}
	if got := shared.closes(); got != 1 {
		t.Fatalf("ending the memo closed the shared store %d times, want 1", got)
	}
	if err := end(); err != nil {
		t.Fatalf("ending the memo twice: %v", err)
	}
	if got := shared.closes(); got != 1 {
		t.Fatalf("ending the memo twice closed the shared store %d times, want 1", got)
	}
	if activeInvocationStoreMemo() != nil {
		t.Fatal("memo still active after it ended")
	}
}

func TestInvocationStoreMemoNeverServesLongLivedOpens(t *testing.T) {
	opened := stubInvocationStoreFactory(t)
	city := t.TempDir()
	end := beginInvocationStoreMemo()
	t.Cleanup(func() { _ = end() })

	for i := 0; i < 2; i++ {
		result, err := openStoreResultAtForCityWithMode(city, city, gate.ModeUnset, false, true, nil)
		if err != nil {
			t.Fatalf("long-lived open %d: %v", i, err)
		}
		if err := closeBeadStoreHandle(result.Store); err != nil {
			t.Fatalf("close long-lived handle: %v", err)
		}
	}
	if len(*opened) != 2 {
		t.Fatalf("two long-lived opens opened %d stores, want 2", len(*opened))
	}
	for i, store := range *opened {
		if got := store.closes(); got != 1 {
			t.Errorf("long-lived store %d closed %d times, want 1 (its owner's close is real)", i, got)
		}
	}
}

func TestInvocationStoreMemoOpensAnotherWriteDisciplineUnmemoized(t *testing.T) {
	opened := stubInvocationStoreFactory(t)
	city := t.TempDir()
	end := beginInvocationStoreMemo()
	t.Cleanup(func() { _ = end() })

	if _, err := openStoreAtForCity(city, city); err != nil {
		t.Fatalf("open: %v", err)
	}
	result, err := openStoreResultAtForCityWithMode(city, city, gate.Auto, true, false, nil)
	if err != nil {
		t.Fatalf("open with another mode: %v", err)
	}
	if len(*opened) != 2 {
		t.Fatalf("a different write discipline reused the memoized store (%d opens), want its own open", len(*opened))
	}
	if err := closeBeadStoreHandle(result.Store); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := (*opened)[1].closes(); got != 1 {
		t.Fatalf("the unmemoized open's close closed it %d times, want 1", got)
	}
}

func TestStoreOpensWithoutTheMemoStayOwned(t *testing.T) {
	opened := stubInvocationStoreFactory(t)
	city := t.TempDir()
	for i := 0; i < 2; i++ {
		store, err := openStoreAtForCity(city, city)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := closeBeadStoreHandle(store); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
	if len(*opened) != 2 {
		t.Fatalf("without the memo, two opens opened %d stores, want 2", len(*opened))
	}
	for i, store := range *opened {
		if got := store.closes(); got != 1 {
			t.Errorf("store %d closed %d times, want 1", i, got)
		}
	}
}

// TestInvocationStoreMemoCoversTheHookVerbs pins which commands run() opens the
// memo for: every session-side hook verb, and no long-lived entry point.
func TestInvocationStoreMemoCoversTheHookVerbs(t *testing.T) {
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"mail", "check", "--inject"}, true},
		{[]string{"mail", "send", "human", "-s", "x"}, true},
		{[]string{"nudge", "drain", "--inject"}, true},
		{[]string{"prime", "--hook"}, true},
		{[]string{"handoff", "--auto", "subject"}, true},
		{[]string{"hook", "--claim"}, true},
		{[]string{"nudge", "poll"}, false},
		{[]string{"supervisor", "run"}, false},
		{[]string{"start"}, false},
		{[]string{"hook", "run", "--", "mail", "check"}, false},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			cmd, _, err := root.Find(tc.args)
			if err != nil {
				t.Fatalf("Find(%v): %v", tc.args, err)
			}
			if got := invocationStoreMemoEligible(cmd); got != tc.want {
				t.Fatalf("invocationStoreMemoEligible(%q) = %v, want %v", cmd.CommandPath(), got, tc.want)
			}
		})
	}
	for path := range invocationStoreMemoCommands {
		args := strings.Fields(strings.TrimPrefix(path, "gc "))
		cmd, _, err := root.Find(args)
		if err != nil || cmd.CommandPath() != path {
			t.Errorf("memo command %q does not name a command (found %v, err %v)", path, cmd, err)
		}
	}
}

// TestEndInvocationStoreMemoLogsCloseErrorsAtDebug pins what run() does with
// an error releasing the invocation's stores: the command's outcome is already
// decided, so it is not a failure, but it is not swallowed either — GC_DEBUG
// shows it on stderr.
func TestEndInvocationStoreMemoLogsCloseErrorsAtDebug(t *testing.T) {
	failing := func() error { return errors.New("store close refused") }
	for _, tc := range []struct {
		debug string
		want  bool
	}{{debug: "", want: false}, {debug: "1", want: true}} {
		t.Setenv("GC_DEBUG", tc.debug)
		var stderr strings.Builder
		endInvocationStoreMemoLogged(failing, &stderr)
		if got := strings.Contains(stderr.String(), "store close refused"); got != tc.want {
			t.Fatalf("GC_DEBUG=%q: logged = %v, want %v; stderr=%q", tc.debug, got, tc.want, stderr.String())
		}
	}
	t.Setenv("GC_DEBUG", "1")
	var stderr strings.Builder
	endInvocationStoreMemoLogged(func() error { return nil }, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("a clean release logged %q", stderr.String())
	}
}
