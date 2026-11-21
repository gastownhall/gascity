package main

import (
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/testutil"
)

// mailListSignalStore closes listed on its first List of messages: the hook
// has started reading its session's mail.
type mailListSignalStore struct {
	beads.Store
	once   *sync.Once
	listed chan struct{}
}

func (s mailListSignalStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Type == "message" {
		s.once.Do(func() { close(s.listed) })
	}
	return s.Store.List(q)
}

// heldUntilStore holds each List until until closes, or a bound passes, and
// records whether the bound ended a hold: a controller that has the hook's step
// reminder request in hand and answers only once the hook does something else.
type heldUntilStore struct {
	beads.Store
	until    <-chan struct{}
	timedOut *atomic.Bool
}

func (s heldUntilStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	select {
	case <-s.until:
	case <-time.After(testutil.GoroutineRaceTimeout):
		s.timedOut.Store(true)
	}
	return s.Store.List(q)
}

// TestPrimeHookReadsItsSessionStartMailWhileTheStepReminderIsOutstanding pins
// that a SessionStart prime reads its session's mail while the controller is
// still answering the step reminder, not only once the answer is in. The two
// reads are independent, and on a fresh (proxied-server) city each is a chain of
// bd round trips, so taking the answer first put the mail read's round trips
// after the controller's on the hook's critical path (ga-3js1ayv). The context
// the hook writes is unchanged: the reminder still leads it.
func TestPrimeHookReadsItsSessionStartMailWhileTheStepReminderIsOutstanding(t *testing.T) {
	f, cs := stepHookCity(t)
	f.sendMail(t, "status please")
	want := recordHookCommand(t, func(stdout, stderr io.Writer) int { return primeHook(t, f, stdout, stderr) })

	listed := make(chan struct{})
	var timedOut atomic.Bool
	cs.cityBeadStore = heldUntilStore{Store: cs.cityBeadStore, until: listed, timedOut: &timedOut}
	withActiveWorkStepClient(t, readyWorkTestServer(t, cs), "")
	// The lookup's own bound must not end the hold first: past it the hook reads
	// the reminder in process and the hold would never be the one that ends.
	prevBound := activeWorkStepAPITimeout
	activeWorkStepAPITimeout = 2 * testutil.GoroutineRaceTimeout
	t.Cleanup(func() { activeWorkStepAPITimeout = prevBound })
	var once sync.Once
	prev := observeOpenedStore
	observeOpenedStore = func(_ string, store beads.Store) beads.Store {
		return mailListSignalStore{Store: store, once: &once, listed: listed}
	}
	t.Cleanup(func() { observeOpenedStore = prev })

	resetHookBudgetProcessState(t)
	endStoreMemo := beginInvocationStoreMemo()
	var stdout, stderr strings.Builder
	code := primeHook(t, f, &stdout, &stderr)
	if err := endStoreMemo(); err != nil {
		t.Errorf("releasing the invocation's stores: %v", err)
	}
	observeOpenedStore = prev
	resetHookBudgetProcessState(t)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, stderr.String())
	}
	if timedOut.Load() {
		t.Fatal("the controller held the step reminder to its bound: prime started reading its SessionStart mail only once it had the answer; the mail read must run while the lookup is in flight")
	}
	if got, wantOut := normalizeHookBudgetOutput(t, stdout.String()), normalizeHookBudgetOutput(t, want.stdout); got != wantOut {
		t.Fatalf("prime wrote\n%s\nwant the in-process output\n%s", got, wantOut)
	}
	out := stdout.String()
	if step, mail := strings.Index(out, "inspect the queue"), strings.Index(out, "status please"); step < 0 || mail < 0 || step > mail {
		t.Fatalf("prime output = %q, want the step reminder, then the session's mail", out)
	}
}
