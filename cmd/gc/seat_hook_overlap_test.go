package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/testutil"
)

// atFirstStoreOpen runs hook and evaluates check when the hook opens its first
// store, returning check's answer. check waits up to a
// bound for work the hook should already have started in the background: work
// started only after that open never happens while the hook is held there.
func atFirstStoreOpen(t *testing.T, hook func(stdout, stderr io.Writer) int, check func() bool) bool {
	t.Helper()
	var once sync.Once
	held := false
	prev := observeOpenedStore
	observeOpenedStore = func(_ string, store beads.Store) beads.Store {
		once.Do(func() { held = check() })
		return store
	}
	t.Cleanup(func() { observeOpenedStore = prev })
	resetHookBudgetProcessState(t)
	endStoreMemo := beginInvocationStoreMemo()
	var stdout, stderr strings.Builder
	code := hook(&stdout, &stderr)
	if err := endStoreMemo(); err != nil {
		t.Errorf("releasing the invocation's stores: %v", err)
	}
	observeOpenedStore = prev
	resetHookBudgetProcessState(t)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, stderr.String())
	}
	return held
}

// TestPrimeHookReapsPollersAlongsideItsReads pins that the SessionStart hook
// reaps stale poller PID files in the background while it does its own reads.
// The reap lists the whole pollers directory, which keeps a lock file for
// every session that ever ran a poller: 97k entries and 0.14-0.21 s on
// maintainer-city, on the critical path of every session start. The reap still
// happens, and prime waits for it before returning.
func TestPrimeHookReapsPollersAlongsideItsReads(t *testing.T) {
	f := newHookBudgetFixture(t)
	stale := filepath.Join(citylayout.RuntimePath(f.cityDir, "nudges", "pollers"), "gone-session.pid")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(stale, []byte("not-a-pid\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	started := make(chan struct{})
	var once sync.Once
	prev := nudgePollerReaper
	nudgePollerReaper = func(cityPath string) error {
		once.Do(func() { close(started) })
		return reapStaleNudgePollers(cityPath)
	}
	t.Cleanup(func() { nudgePollerReaper = prev })
	inBackground := atFirstStoreOpen(t, func(stdout, stderr io.Writer) int { return primeHook(t, f, stdout, stderr) }, func() bool {
		return startedWithin(started)
	})
	if !inBackground {
		t.Fatal("the pollers reap had not started when prime opened its first store; it must run alongside the hook's reads")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("prime returned without reaping the stale poller PID file (stat err %v)", err)
	}
}

// TestSeatHooksResolveStorageRoutesAlongsideTheCityStoreOpen pins that each
// seat hook resolves its one-shot storage routes while it opens the city
// store, not after. The routing read composes the city config again and opens
// the class binding (0.16 s on maintainer-city), and the store open behind it
// is independent of it. It starts only once the hook's own config load has run
// the builtin readiness pass the routing read's composition needs.
func TestSeatHooksResolveStorageRoutesAlongsideTheCityStoreOpen(t *testing.T) {
	for _, sc := range []struct {
		name string
		seed func(t *testing.T, f hookBudgetFixture)
		run  func(t *testing.T, f hookBudgetFixture, stdout, stderr io.Writer) int
	}{
		{name: "prime --hook", seed: func(*testing.T, hookBudgetFixture) {}, run: primeHook},
		{name: "nudge drain --inject", seed: func(t *testing.T, f hookBudgetFixture) { f.queueNudge(t) }, run: nudgeDrainInject},
		{name: "mail check --inject", seed: func(t *testing.T, f hookBudgetFixture) { f.sendMail(t, "status please") }, run: mailCheckInject},
	} {
		t.Run(sc.name, func(t *testing.T) {
			f := newHookBudgetFixture(t)
			sc.seed(t, f)
			started := make(chan struct{})
			var once sync.Once
			prev := cliStorageRoutesPrewarm
			cliStorageRoutesPrewarm = func(cityPath string) {
				once.Do(func() { close(started) })
				prev(cityPath)
			}
			t.Cleanup(func() { cliStorageRoutesPrewarm = prev })
			alongside := atFirstStoreOpen(t, func(stdout, stderr io.Writer) int { return sc.run(t, f, stdout, stderr) }, func() bool {
				return startedWithin(started)
			})
			if !alongside {
				t.Fatal("the hook had not started resolving its storage routes when it opened the city store; the routing read must run alongside the open")
			}
		})
	}
}

// startedWithin reports whether started closes within the race timeout: work
// the hook started in the background before the point that asks has started by
// then, and work it starts only afterwards never does while the hook is held
// at that point.
func startedWithin(started <-chan struct{}) bool {
	select {
	case <-started:
		return true
	case <-time.After(testutil.GoroutineRaceTimeout):
		return false
	}
}
