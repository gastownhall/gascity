package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
)

// slowCleanupStore models slow store legs in retired-session cleanup: each ID
// closed through CloseAll costs closeAllPerID of (synctest-fake) time — a slow
// wait store — and each single-bead Close costs closePerCall — a slow
// external-messaging store, whose binding teardown closes one bead per
// binding.
type slowCleanupStore struct {
	*beads.MemStore
	closeAllPerID time.Duration
	closePerCall  time.Duration
}

func (s *slowCleanupStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	time.Sleep(time.Duration(len(ids)) * s.closeAllPerID)
	return s.MemStore.CloseAll(ids, metadata)
}

func (s *slowCleanupStore) Close(id string) error {
	time.Sleep(s.closePerCall)
	return s.MemStore.Close(id)
}

// retiredSessionBeadFixture creates the archived session bead a retirement
// cleanup targets.
func retiredSessionBeadFixture(t *testing.T, mem *beads.MemStore) beads.Bead {
	t.Helper()
	sessionBead, err := mem.Create(beads.Bead{
		Title:  "retired",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "retired",
			"state":        string(session.StateArchived),
		},
	})
	if err != nil {
		t.Fatalf("create retired session bead: %v", err)
	}
	return sessionBead
}

// seedRetiredSessionWaits creates n pending waits for the retired session.
func seedRetiredSessionWaits(t *testing.T, mem *beads.MemStore, sessionID string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := mem.Create(beads.Bead{
			Title:  fmt.Sprintf("wait-%d", i),
			Type:   waitBeadType,
			Labels: []string{waitBeadLabel, "session:" + sessionID},
			Metadata: map[string]string{
				"session_id": sessionID,
				"state":      waitStatePending,
			},
		}); err != nil {
			t.Fatalf("create wait %d: %v", i, err)
		}
	}
}

// bindRetiredSession creates one active external-message binding between the
// conversation named convID and the retired session, returning the
// conversation ref and the binding record.
func bindRetiredSession(t *testing.T, mem *beads.MemStore, sessionID, convID string) (extmsg.ConversationRef, extmsg.SessionBindingRecord) {
	t.Helper()
	fabric := extmsg.NewServices(mem)
	ref := extmsg.ConversationRef{
		ScopeID:        "test-city",
		Provider:       "discord",
		AccountID:      "acct",
		ConversationID: convID,
		Kind:           extmsg.ConversationThread,
	}
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	record, err := fabric.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: ref,
		SessionID:    sessionID,
		Now:          time.Date(2026, 3, 23, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("bind session %s to %s: %v", sessionID, convID, err)
	}
	return ref, record
}

// openRetiredSessionWaits counts the session's still-open wait beads.
func openRetiredSessionWaits(t *testing.T, mem *beads.MemStore, sessionID string) int {
	t.Helper()
	waits, err := mem.List(beads.ListQuery{Label: "session:" + sessionID, Limit: 5000})
	if err != nil {
		t.Fatalf("list waits: %v", err)
	}
	open := 0
	for _, wait := range waits {
		if wait.Type == waitBeadType && wait.Status == "open" {
			open++
		}
	}
	return open
}

// ga-68eubg: retired-session cleanup against a slow wait store must stop
// issuing new work once its waits budget (10s) is spent, and log a
// distinguishable cut-off line naming the session, instead of running the
// waits leg to completion however long it takes.
func TestCancelStateAssignedToRetiredSessionBeadBoundsSlowWaitsStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mem := beads.NewMemStore()
		store := &slowCleanupStore{MemStore: mem, closeAllPerID: 200 * time.Millisecond}
		sessionBead := retiredSessionBeadFixture(t, mem)
		seedRetiredSessionWaits(t, mem, sessionBead.ID, 300)

		var stderr bytes.Buffer
		start := time.Now()
		cancelStateAssignedToRetiredSessionBead(store, sessionBead.ID, start, &stderr)
		elapsed := time.Since(start)

		// Two independent 10s leg budgets plus one in-flight call of slack.
		if limit := 25 * time.Second; elapsed > limit {
			t.Errorf("cleanup took %v of store time for 300 waits (unbounded waits leg), want <= %v", elapsed, limit)
		}
		if !strings.Contains(stderr.String(), sessionBead.ID) {
			t.Errorf("stderr = %q, want a cut-off diagnostic naming session %s", stderr.String(), sessionBead.ID)
		}
	})
}

// ga-68eubg AC-1: the waits leg stops at its budget checkpoint (a slow store
// leaves the un-closed waits open instead of grinding through all of them),
// and the external-messaging leg still runs after the cut-off.
func TestCancelStateAssignedToRetiredSessionBeadStopsWaitsLegAndStillRunsExtmsg(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mem := beads.NewMemStore()
		store := &slowCleanupStore{MemStore: mem, closeAllPerID: 200 * time.Millisecond}
		sessionBead := retiredSessionBeadFixture(t, mem)
		seedRetiredSessionWaits(t, mem, sessionBead.ID, 300)
		ref, _ := bindRetiredSession(t, mem, sessionBead.ID, "thread-1")
		fabric := extmsg.NewServices(mem)

		var stderr bytes.Buffer
		cancelStateAssignedToRetiredSessionBead(store, sessionBead.ID, time.Now(), &stderr)

		if open := openRetiredSessionWaits(t, mem, sessionBead.ID); open == 0 {
			t.Errorf("all 300 waits closed despite the 10s waits budget: the leg must stop at its deadline checkpoint")
		}
		got, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
		if err != nil {
			t.Fatalf("resolve binding after retirement: %v", err)
		}
		if got != nil {
			t.Errorf("binding after retirement = %#v, want nil: the extmsg leg must run after the waits leg was cut off", got)
		}
	})
}

// ga-68eubg AC-2: many slow external-message teardowns cannot keep
// retired-session cleanup running indefinitely — the extmsg leg stops at its
// own 10-second budget, leaving the un-reached bindings for the backstops.
func TestCancelStateAssignedToRetiredSessionBeadExtmsgLegStopsAtItsOwnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mem := beads.NewMemStore()
		store := &slowCleanupStore{MemStore: mem, closePerCall: 2 * time.Second}
		sessionBead := retiredSessionBeadFixture(t, mem)
		seedRetiredSessionWaits(t, mem, sessionBead.ID, 3)
		refs := make([]extmsg.ConversationRef, 0, 20)
		for i := 0; i < 20; i++ {
			ref, _ := bindRetiredSession(t, mem, sessionBead.ID, fmt.Sprintf("thread-%d", i))
			refs = append(refs, ref)
		}
		fabric := extmsg.NewServices(mem)

		var stderr bytes.Buffer
		start := time.Now()
		cancelStateAssignedToRetiredSessionBead(store, sessionBead.ID, start, &stderr)
		elapsed := time.Since(start)

		if limit := 15 * time.Second; elapsed > limit {
			t.Errorf("cleanup took %v against 20 slow bindings (unbounded extmsg leg), want <= %v", elapsed, limit)
		}
		stillBound := 0
		for _, ref := range refs {
			got, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
			if err != nil {
				t.Fatalf("resolve binding after retirement: %v", err)
			}
			if got != nil {
				stillBound++
			}
		}
		if stillBound == 0 {
			t.Errorf("all 20 bindings unbound despite the 10s extmsg budget: the leg must stop at its deadline checkpoint")
		}
	})
}

// ga-68eubg AC-2: the extmsg leg's budget is its own — after a waits leg that
// consumed its full 10s budget, a fresh deadline still lets moderate extmsg
// work finish, instead of dying on a timer the waits leg already spent.
func TestCancelStateAssignedToRetiredSessionBeadExtmsgLegGetsFreshBudgetAfterWaitsCutoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mem := beads.NewMemStore()
		store := &slowCleanupStore{
			MemStore:      mem,
			closeAllPerID: 200 * time.Millisecond,
			closePerCall:  1 * time.Second,
		}
		sessionBead := retiredSessionBeadFixture(t, mem)
		seedRetiredSessionWaits(t, mem, sessionBead.ID, 300)
		refs := make([]extmsg.ConversationRef, 0, 4)
		for i := 0; i < 4; i++ {
			ref, _ := bindRetiredSession(t, mem, sessionBead.ID, fmt.Sprintf("thread-%d", i))
			refs = append(refs, ref)
		}
		fabric := extmsg.NewServices(mem)

		var stderr bytes.Buffer
		start := time.Now()
		cancelStateAssignedToRetiredSessionBead(store, sessionBead.ID, start, &stderr)
		elapsed := time.Since(start)

		for _, ref := range refs {
			got, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
			if err != nil {
				t.Fatalf("resolve binding after retirement: %v", err)
			}
			if got != nil {
				t.Errorf("binding after retirement = %#v, want nil: the fresh extmsg budget must cover moderate work after the waits cut-off", got)
			}
		}
		if minElapsed, limit := 12*time.Second, 25*time.Second; elapsed < minElapsed || elapsed > limit {
			t.Errorf("cleanup took %v, want between the spent 10s waits budget plus the extmsg work (%v) and %v", elapsed, minElapsed, limit)
		}
	})
}

// ga-68eubg AC-3: a leg its own budget stopped reports exactly one cut-off
// line naming the session, the leg, and the budget — and the ordinary error
// line for that leg stays silent, so a cut-off is distinguishable from a store
// failure by the line alone.
func TestCancelStateAssignedToRetiredSessionBeadWaitsCutoffLineIsTheOnlyWaitsDiagnostic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mem := beads.NewMemStore()
		store := &slowCleanupStore{MemStore: mem, closeAllPerID: 200 * time.Millisecond}
		sessionBead := retiredSessionBeadFixture(t, mem)
		seedRetiredSessionWaits(t, mem, sessionBead.ID, 300)

		var stderr bytes.Buffer
		cancelStateAssignedToRetiredSessionBead(store, sessionBead.ID, time.Now(), &stderr)

		want := fmt.Sprintf("session beads: retired-session cleanup for %s: waits leg cut off after %s budget\n", sessionBead.ID, retiredCleanupLegBudget)
		if got := strings.Count(stderr.String(), want); got != 1 {
			t.Errorf("waits cut-off line count = %d, want exactly 1 of %q (stderr: %q)", got, want, stderr.String())
		}
		if strings.Contains(stderr.String(), "canceling waits for retired session") {
			t.Errorf("ordinary waits error line printed for a cut-off leg: %q", stderr.String())
		}
		if strings.Contains(stderr.String(), "external-messaging leg cut off") {
			t.Errorf("extmsg cut-off line printed though its leg ran: %q", stderr.String())
		}
	})
}

func TestCancelStateAssignedToRetiredSessionBeadExtmsgCutoffLineIsTheOnlyExtmsgDiagnostic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mem := beads.NewMemStore()
		store := &slowCleanupStore{MemStore: mem, closePerCall: 2 * time.Second}
		sessionBead := retiredSessionBeadFixture(t, mem)
		seedRetiredSessionWaits(t, mem, sessionBead.ID, 3)
		for i := 0; i < 20; i++ {
			_, _ = bindRetiredSession(t, mem, sessionBead.ID, fmt.Sprintf("thread-%d", i))
		}

		var stderr bytes.Buffer
		cancelStateAssignedToRetiredSessionBead(store, sessionBead.ID, time.Now(), &stderr)

		want := fmt.Sprintf("session beads: retired-session cleanup for %s: external-messaging leg cut off after %s budget\n", sessionBead.ID, retiredCleanupLegBudget)
		if got := strings.Count(stderr.String(), want); got != 1 {
			t.Errorf("extmsg cut-off line count = %d, want exactly 1 of %q (stderr: %q)", got, want, stderr.String())
		}
		if strings.Contains(stderr.String(), "closing external message bindings") {
			t.Errorf("ordinary extmsg error line printed for a cut-off leg: %q", stderr.String())
		}
		if strings.Contains(stderr.String(), "waits leg cut off") {
			t.Errorf("waits cut-off line printed though its leg finished: %q", stderr.String())
		}
	})
}

var (
	errCleanupCloseWaitBoom    = errors.New("close-wait boom")
	errCleanupCloseBindingBoom = errors.New("close-binding boom")
)

// failingCleanupCloseAllStore fails every batch close with a fixed ordinary
// store error, with the leg's context live throughout.
type failingCleanupCloseAllStore struct {
	*beads.MemStore
}

func (s failingCleanupCloseAllStore) CloseAll(_ []string, _ map[string]string) (int, error) {
	return 0, errCleanupCloseWaitBoom
}

// failingCleanupCloseStore fails closes of the given bead IDs — the binding
// beads the external-messaging leg closes — with err, leaving the leg's
// earlier membership close working.
type failingCleanupCloseStore struct {
	*beads.MemStore
	failIDs map[string]struct{}
	err     error
}

func (s failingCleanupCloseStore) Close(id string) error {
	if _, ok := s.failIDs[id]; ok {
		return s.err
	}
	return s.MemStore.Close(id)
}

// ga-68eubg AC-3: ordinary store errors keep their usual diagnostic lines,
// byte for byte — only a budget stop turns into a cut-off line.
func TestCancelStateAssignedToRetiredSessionBeadOrdinaryErrorsKeepTheirLineFormat(t *testing.T) {
	t.Run("waits store error", func(t *testing.T) {
		mem := beads.NewMemStore()
		sessionBead := retiredSessionBeadFixture(t, mem)
		seedRetiredSessionWaits(t, mem, sessionBead.ID, 2)

		var stderr bytes.Buffer
		cancelStateAssignedToRetiredSessionBead(failingCleanupCloseAllStore{MemStore: mem}, sessionBead.ID, time.Now(), &stderr)

		want := fmt.Sprintf("session beads: canceling waits for retired session %s: %v\n", sessionBead.ID, errCleanupCloseWaitBoom)
		if stderr.String() != want {
			t.Errorf("waits stderr = %q, want exactly %q", stderr.String(), want)
		}
	})
	t.Run("extmsg store error", func(t *testing.T) {
		mem := beads.NewMemStore()
		sessionBead := retiredSessionBeadFixture(t, mem)
		_, binding := bindRetiredSession(t, mem, sessionBead.ID, "thread-1")

		var stderr bytes.Buffer
		cancelStateAssignedToRetiredSessionBead(failingCleanupCloseStore{
			MemStore: mem,
			failIDs:  map[string]struct{}{binding.ID: {}},
			err:      errCleanupCloseBindingBoom,
		}, sessionBead.ID, time.Now(), &stderr)

		prefix := fmt.Sprintf("session beads: closing external message bindings for retired session %s: close binding ", sessionBead.ID)
		line := ""
		for _, candidate := range strings.Split(stderr.String(), "\n") {
			if strings.HasPrefix(candidate, prefix) {
				line = candidate
				break
			}
		}
		if line == "" {
			t.Fatalf("extmsg stderr = %q, want a line starting with %q", stderr.String(), prefix)
		}
		if !strings.HasSuffix(line, fmt.Sprintf(": %v", errCleanupCloseBindingBoom)) {
			t.Errorf("extmsg error line = %q, want it to end with the bare store error %q", line, errCleanupCloseBindingBoom)
		}
		if strings.Contains(stderr.String(), "cut off") {
			t.Errorf("cut-off line printed for an ordinary store error: %q", stderr.String())
		}
	})
}

// sentinelCleanupClose stores report an error that wraps context.Canceled from
// their own store calls while the leg's context is still live — a naive
// "wraps context.Canceled means cut off" classifier would misread these as a
// budget stop.
type sentinelCleanupCloseAllStore struct {
	*beads.MemStore
}

func (s sentinelCleanupCloseAllStore) CloseAll(_ []string, _ map[string]string) (int, error) {
	return 0, fmt.Errorf("store closed the conversation: %w", context.Canceled)
}

// ga-68eubg AC-3: a context.Canceled-wrapped store error while the leg's own
// context is still live is an ordinary error, not a cut-off — only the leg's
// own ended context classifies a leg as cut off.
func TestCancelStateAssignedToRetiredSessionBeadContextSentinelWhileLegLiveIsNotACutoff(t *testing.T) {
	t.Run("waits leg live", func(t *testing.T) {
		mem := beads.NewMemStore()
		sessionBead := retiredSessionBeadFixture(t, mem)
		seedRetiredSessionWaits(t, mem, sessionBead.ID, 2)

		var stderr bytes.Buffer
		cancelStateAssignedToRetiredSessionBead(sentinelCleanupCloseAllStore{MemStore: mem}, sessionBead.ID, time.Now(), &stderr)

		want := fmt.Sprintf("session beads: canceling waits for retired session %s: store closed the conversation: context canceled\n", sessionBead.ID)
		if stderr.String() != want {
			t.Errorf("waits stderr = %q, want exactly the ordinary line %q", stderr.String(), want)
		}
	})
	t.Run("extmsg leg live", func(t *testing.T) {
		mem := beads.NewMemStore()
		sessionBead := retiredSessionBeadFixture(t, mem)
		_, binding := bindRetiredSession(t, mem, sessionBead.ID, "thread-1")

		var stderr bytes.Buffer
		cancelStateAssignedToRetiredSessionBead(failingCleanupCloseStore{
			MemStore: mem,
			failIDs:  map[string]struct{}{binding.ID: {}},
			err:      fmt.Errorf("store closed the conversation: %w", context.Canceled),
		}, sessionBead.ID, time.Now(), &stderr)

		prefix := fmt.Sprintf("session beads: closing external message bindings for retired session %s: close binding ", sessionBead.ID)
		line := ""
		for _, candidate := range strings.Split(stderr.String(), "\n") {
			if strings.HasPrefix(candidate, prefix) {
				line = candidate
				break
			}
		}
		if line == "" {
			t.Fatalf("extmsg stderr = %q, want a line starting with %q", stderr.String(), prefix)
		}
		if !strings.HasSuffix(line, ": store closed the conversation: context canceled") {
			t.Errorf("extmsg error line = %q, want it to keep the store's sentinel error text", line)
		}
		if strings.Contains(stderr.String(), "cut off") {
			t.Errorf("cut-off line printed for a live leg's store error: %q", stderr.String())
		}
	})
}

// ga-68eubg AC-6: a healthy store's full retirement — waits, a binding, a
// group participant's membership — converges with no diagnostics at all:
// the leg budgets and the cut-off classification stay silent when nothing
// is slow. This guard passes on the unbounded base on purpose; it holds the
// bounded implementation to it.
func TestCancelStateAssignedToRetiredSessionBeadHealthyCleanupCleansAllStateWithoutDiagnostics(t *testing.T) {
	mem := beads.NewMemStore()
	sessionBead, fabric, caller := setupSessionWithExtmsgMembership(t, mem, session.StateArchived)
	seedRetiredSessionWaits(t, mem, sessionBead.ID, 5)
	ref, _ := bindRetiredSession(t, mem, sessionBead.ID, "thread-1")

	var stderr bytes.Buffer
	cancelStateAssignedToRetiredSessionBead(mem, sessionBead.ID, time.Now(), &stderr)

	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want no diagnostics for a healthy cleanup", stderr.String())
	}
	if open := openRetiredSessionWaits(t, mem, sessionBead.ID); open != 0 {
		t.Errorf("open waits after retirement = %d, want 0", open)
	}
	got, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("resolve binding after retirement: %v", err)
	}
	if got != nil {
		t.Errorf("binding after retirement = %#v, want nil", got)
	}
	if open := countOpenMembershipsForSession(t, fabric, caller, sessionBead.ID); open != 0 {
		t.Errorf("open memberships after retirement = %d, want 0", open)
	}
}

// ga-68eubg AC-6: the retirement helper's no-op guards stay panic-free and
// issue no work — a nil store, a blank session ID, and a nil stderr (a
// failing store's diagnostics discarded).
func TestCancelStateAssignedToRetiredSessionBeadGuardsNoopInputs(t *testing.T) {
	cancelStateAssignedToRetiredSessionBead(nil, "gc-1", time.Now(), nil)

	mem := beads.NewMemStore()
	sessionBead := retiredSessionBeadFixture(t, mem)
	seedRetiredSessionWaits(t, mem, sessionBead.ID, 3)
	cancelStateAssignedToRetiredSessionBead(mem, "   ", time.Now(), nil)
	if open := openRetiredSessionWaits(t, mem, sessionBead.ID); open != 3 {
		t.Errorf("open waits after a blank-session call = %d, want the 3 untouched", open)
	}

	cancelStateAssignedToRetiredSessionBead(failingCleanupCloseAllStore{MemStore: mem}, sessionBead.ID, time.Now(), nil)
}
