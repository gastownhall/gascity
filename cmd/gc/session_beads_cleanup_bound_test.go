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

// retiredCleanupTimedStore charges synctest-fake time for store calls: every
// CloseAll costs closePerID per ID, and every List other than the session's
// wait lookup (the external-messaging leg's reads) costs extmsgList.
type retiredCleanupTimedStore struct {
	*beads.MemStore
	waitLabel  string
	closePerID time.Duration
	extmsgList time.Duration
}

func (s *retiredCleanupTimedStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	time.Sleep(time.Duration(len(ids)) * s.closePerID)
	return s.MemStore.CloseAll(ids, metadata)
}

func (s *retiredCleanupTimedStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label != s.waitLabel {
		time.Sleep(s.extmsgList)
	}
	return s.MemStore.List(query)
}

// seedRetiredSessionWaits creates count pending waits for sessionID.
func seedRetiredSessionWaits(t *testing.T, store beads.Store, sessionID string, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		if _, err := store.Create(beads.Bead{
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

// bindRetiredSession binds sessionID to count distinct conversations.
func bindRetiredSession(t *testing.T, fabric extmsg.Services, caller extmsg.Caller, sessionID string, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		if _, err := fabric.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
			Conversation: extmsg.ConversationRef{
				ScopeID:        "ds-research",
				Provider:       "slack",
				AccountID:      "T0WORKSPACE",
				ConversationID: fmt.Sprintf("C0BOUND%d", i),
				Kind:           extmsg.ConversationRoom,
			},
			SessionID: sessionID,
		}); err != nil {
			t.Fatalf("Bind %d: %v", i, err)
		}
	}
}

func countRetiredSessionWaits(t *testing.T, store beads.Store, sessionID string) (open, closed int) {
	t.Helper()
	items, err := store.List(beads.ListQuery{Label: "session:" + sessionID, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list waits: %v", err)
	}
	for _, item := range items {
		if !session.IsWaitBead(item) {
			continue
		}
		if item.Status == "closed" {
			closed++
		} else {
			open++
		}
	}
	return open, closed
}

func countActiveBindings(t *testing.T, fabric extmsg.Services, sessionID string) int {
	t.Helper()
	records, err := fabric.Bindings.ListBySession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	active := 0
	for _, record := range records {
		if record.Status == extmsg.BindingActive {
			active++
		}
	}
	return active
}

// Slow wait closes stop at the waits leg's 10s budget, and the
// external-messaging leg still gets a fresh budget of its own: its reads
// cost fake time after the waits deadline has passed, and it finishes.
func TestCancelStateAssignedToRetiredSessionBeadStopsSlowWaitsAtTheirBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mem := beads.NewMemStore()
		sessionBead, fabric, caller := setupSessionWithExtmsgMembership(t, mem, session.StateArchived)
		bindRetiredSession(t, fabric, caller, sessionBead.ID, 1)
		const waitCount = 300
		seedRetiredSessionWaits(t, mem, sessionBead.ID, waitCount)
		store := &retiredCleanupTimedStore{
			MemStore:   mem,
			waitLabel:  "session:" + sessionBead.ID,
			closePerID: 200 * time.Millisecond,
			extmsgList: time.Second,
		}

		var stderr bytes.Buffer
		cancelStateAssignedToRetiredSessionBead(store, sessionBead.ID, time.Now(), &stderr)

		// 10-ID batches cost 2s each: batches start at 0, 2, 4, 6 and 8s
		// (and at 10s if the deadline fires after that batch's close).
		open, closed := countRetiredSessionWaits(t, mem, sessionBead.ID)
		if closed < 50 || closed > 60 || open != waitCount-closed {
			t.Fatalf("waits closed/open = %d/%d, want 50-60 closed before the 10s waits budget ran out", closed, open)
		}
		if !strings.Contains(stderr.String(), sessionBead.ID) {
			t.Fatalf("stderr = %q, want a diagnostic naming session %s", stderr.String(), sessionBead.ID)
		}
		if got := countActiveBindings(t, fabric, sessionBead.ID); got != 0 {
			t.Fatalf("active bindings = %d, want 0: external messaging needs its own budget after the waits cut-off", got)
		}
		if got := countOpenMembershipsForSession(t, fabric, caller, sessionBead.ID); got != 0 {
			t.Fatalf("open memberships = %d, want 0: external messaging needs its own budget after the waits cut-off", got)
		}
	})
}

// Fast waits and many slow external-messaging operations: the
// external-messaging leg stops at its existing cancellation checkpoints once
// its own 10s budget runs out, leaving bindings for later passes.
func TestCancelStateAssignedToRetiredSessionBeadStopsSlowExternalMessagingAtItsBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mem := beads.NewMemStore()
		sessionBead, fabric, caller := setupSessionWithExtmsgMembership(t, mem, session.StateArchived)
		const bindingCount = 40
		bindRetiredSession(t, fabric, caller, sessionBead.ID, bindingCount)
		seedRetiredSessionWaits(t, mem, sessionBead.ID, 5)
		store := &retiredCleanupTimedStore{
			MemStore:   mem,
			waitLabel:  "session:" + sessionBead.ID,
			extmsgList: time.Second,
		}

		var stderr bytes.Buffer
		start := time.Now()
		cancelStateAssignedToRetiredSessionBead(store, sessionBead.ID, start, &stderr)
		elapsed := time.Since(start)

		if open, _ := countRetiredSessionWaits(t, mem, sessionBead.ID); open != 0 {
			t.Fatalf("open waits = %d, want 0 (fast waits leg finishes)", open)
		}
		active := countActiveBindings(t, fabric, sessionBead.ID)
		if active == 0 || active == bindingCount {
			t.Fatalf("active bindings = %d of %d, want the leg stopped part way through", active, bindingCount)
		}
		// Every extmsg read costs 1s of fake time; each binding's unbind is a
		// few reads, so the leg ends within a few seconds of its deadline.
		if limit := retiredCleanupLegBudget + 5*time.Second; elapsed > limit {
			t.Fatalf("cleanup took %v of fake store time, want <= %v", elapsed, limit)
		}
		if !strings.Contains(stderr.String(), sessionBead.ID) {
			t.Fatalf("stderr = %q, want a diagnostic naming session %s", stderr.String(), sessionBead.ID)
		}
	})
}
