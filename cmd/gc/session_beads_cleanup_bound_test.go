package main

import (
	"bytes"
	"context"
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
// conversation named convID and the retired session.
func bindRetiredSession(t *testing.T, mem *beads.MemStore, sessionID, convID string) extmsg.ConversationRef {
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
	if _, err := fabric.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: ref,
		SessionID:    sessionID,
		Now:          time.Date(2026, 3, 23, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("bind session %s to %s: %v", sessionID, convID, err)
	}
	return ref
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
		ref := bindRetiredSession(t, mem, sessionBead.ID, "thread-1")
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
