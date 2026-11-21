package main

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
)

// retiredCleanupBudgetStore charges the waits leg and the external-message leg
// of retired-session cleanup separate per-call delays, so a synctest bubble
// can drive either leg past its budget. Wait lookups (the session's wait
// label) and every CloseAll (only waits batch-close) cost waitDelay; every
// other List costs extmsgDelay and has its label recorded.
type retiredCleanupBudgetStore struct {
	*beads.MemStore
	waitLabel   string
	waitDelay   time.Duration
	extmsgDelay time.Duration
	// closeAllErr, when set, is returned by every CloseAll after its delay.
	closeAllErr error

	mu          sync.Mutex
	extmsgLists []string
}

func (s *retiredCleanupBudgetStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == s.waitLabel {
		time.Sleep(s.waitDelay)
		return s.MemStore.List(query)
	}
	s.mu.Lock()
	s.extmsgLists = append(s.extmsgLists, query.Label)
	s.mu.Unlock()
	time.Sleep(s.extmsgDelay)
	return s.MemStore.List(query)
}

func (s *retiredCleanupBudgetStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	time.Sleep(s.waitDelay)
	if s.closeAllErr != nil {
		return 0, s.closeAllErr
	}
	return s.MemStore.CloseAll(ids, metadata)
}

// runRetiredCleanupInBubble runs retired-session cleanup over store inside a
// synctest bubble and returns its stderr and the bubble time it took.
func runRetiredCleanupInBubble(t *testing.T, store *retiredCleanupBudgetStore, sessionID string, now time.Time) (string, time.Duration) {
	t.Helper()
	var stderr bytes.Buffer
	var elapsed time.Duration
	synctest.Test(t, func(*testing.T) {
		start := time.Now()
		cancelStateAssignedToRetiredSessionBead(store, sessionID, now, &stderr)
		elapsed = time.Since(start)
	})
	return stderr.String(), elapsed
}

func newRetiredCleanupSessionBead(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
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
	return b
}

// seedRetiredCleanupWaits creates pending waits and then terminal (expired
// but still open) waits for sessionID.
func seedRetiredCleanupWaits(t *testing.T, store beads.Store, sessionID string, pending, terminal int) {
	t.Helper()
	for i := 0; i < pending+terminal; i++ {
		state := waitStatePending
		if i >= pending {
			state = "expired"
		}
		if _, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("wait-%d", i),
			Type:   waitBeadType,
			Labels: []string{waitBeadLabel, "session:" + sessionID},
			Metadata: map[string]string{
				"session_id": sessionID,
				"state":      state,
			},
		}); err != nil {
			t.Fatalf("create wait %d: %v", i, err)
		}
	}
}

// retiredCleanupExtmsg seeds and counts the external-message state of one
// session: bindings (each with its binding-owned membership), group
// participants (each with its group-owned membership) and standalone manual
// memberships.
type retiredCleanupExtmsg struct {
	store     beads.Store
	fabric    extmsg.Services
	caller    extmsg.Caller
	sessionID string
}

func newRetiredCleanupExtmsg(store beads.Store, sessionID string) retiredCleanupExtmsg {
	return retiredCleanupExtmsg{
		store:     store,
		fabric:    extmsg.NewServices(beadstest.Mail(store), beadstest.Sessions(store)),
		caller:    extmsg.Caller{Kind: extmsg.CallerController, ID: "test"},
		sessionID: sessionID,
	}
}

func retiredCleanupConversation(kind string, i int) extmsg.ConversationRef {
	return extmsg.ConversationRef{
		ScopeID:        "city-1",
		Provider:       "slack",
		AccountID:      "T0WORKSPACE",
		ConversationID: fmt.Sprintf("C0%s%03d", strings.ToUpper(kind), i),
		Kind:           extmsg.ConversationRoom,
	}
}

func (x retiredCleanupExtmsg) seed(t *testing.T, bindings, participants, manual int, now time.Time) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < bindings; i++ {
		if _, err := x.fabric.Bindings.Bind(ctx, x.caller, extmsg.BindInput{
			Conversation: retiredCleanupConversation("bind", i),
			SessionID:    x.sessionID,
			Now:          now,
		}); err != nil {
			t.Fatalf("Bind %d: %v", i, err)
		}
	}
	for i := 0; i < participants; i++ {
		group, err := x.fabric.Groups.EnsureGroup(ctx, x.caller, extmsg.EnsureGroupInput{
			RootConversation: retiredCleanupConversation("group", i),
			Mode:             extmsg.GroupModeLauncher,
		})
		if err != nil {
			t.Fatalf("EnsureGroup %d: %v", i, err)
		}
		if _, err := x.fabric.Groups.UpsertParticipant(ctx, x.caller, extmsg.UpsertParticipantInput{
			GroupID:   group.ID,
			Handle:    fmt.Sprintf("worker%d", i),
			SessionID: x.sessionID,
		}); err != nil {
			t.Fatalf("UpsertParticipant %d: %v", i, err)
		}
	}
	for i := 0; i < manual; i++ {
		if _, err := x.fabric.Transcript.EnsureMembership(ctx, extmsg.EnsureMembershipInput{
			Caller:       x.caller,
			Conversation: retiredCleanupConversation("manual", i),
			SessionID:    x.sessionID,
			Owner:        extmsg.MembershipOwnerManual,
			Now:          now,
		}); err != nil {
			t.Fatalf("EnsureMembership %d: %v", i, err)
		}
	}
}

// counts returns the open bindings, participants and memberships still
// naming the session.
func (x retiredCleanupExtmsg) counts(t *testing.T) (bindings, participants, memberships int) {
	t.Helper()
	ctx := context.Background()
	bound, err := x.fabric.Bindings.ListBySession(ctx, x.sessionID)
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	items, err := x.store.List(beads.ListQuery{Label: "extmsg:group:participant:session:v1:" + x.sessionID})
	if err != nil {
		t.Fatalf("list participants: %v", err)
	}
	for _, item := range items {
		if item.Status != "closed" && slices.Contains(item.Labels, "gc:extmsg-participant") {
			participants++
		}
	}
	members, err := x.fabric.Transcript.ListConversationsBySession(ctx, x.caller, x.sessionID)
	if err != nil {
		t.Fatalf("ListConversationsBySession: %v", err)
	}
	return len(bound), participants, len(members)
}

// retiredCleanupWaitCounts returns the session's open waits and its closed
// waits, failing if a closed wait lacks the canceled state stamped at now.
func retiredCleanupWaitCounts(t *testing.T, store beads.Store, sessionID string, now time.Time) (open, closed int) {
	t.Helper()
	waits, err := store.List(beads.ListQuery{Label: "session:" + sessionID, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list waits: %v", err)
	}
	for _, w := range waits {
		if !session.IsWaitBead(w) {
			continue
		}
		if w.Status != "closed" {
			open++
			continue
		}
		closed++
		if w.Metadata["state"] == waitStatePending {
			t.Fatalf("closed wait %s kept state pending", w.ID)
		}
		if w.Metadata["state"] == waitStateCanceled && w.Metadata["canceled_at"] != now.UTC().Format(time.RFC3339) {
			t.Fatalf("canceled wait %s canceled_at = %q, want %q", w.ID, w.Metadata["canceled_at"], now.UTC().Format(time.RFC3339))
		}
	}
	return open, closed
}

// TestRetiredCleanupHealthyStoreReachesFinalState: on healthy stores
// retirement cleanup cancels pending waits, closes terminal waits, ends
// bindings and closes participants and memberships, with no diagnostics.
func TestRetiredCleanupHealthyStoreReachesFinalState(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	mem := beads.NewMemStore()
	sessionBead := newRetiredCleanupSessionBead(t, mem)
	seedRetiredCleanupWaits(t, mem, sessionBead.ID, 23, 7)
	x := newRetiredCleanupExtmsg(mem, sessionBead.ID)
	x.seed(t, 2, 2, 1, now)
	if b, p, m := x.counts(t); b != 2 || p != 2 || m != 5 {
		t.Fatalf("seeded bindings/participants/memberships = %d/%d/%d, want 2/2/5", b, p, m)
	}

	store := &retiredCleanupBudgetStore{MemStore: mem, waitLabel: "session:" + sessionBead.ID}
	stderr, _ := runRetiredCleanupInBubble(t, store, sessionBead.ID, now)

	if stderr != "" {
		t.Fatalf("stderr = %q, want no diagnostics on a healthy store", stderr)
	}
	if open, closed := retiredCleanupWaitCounts(t, mem, sessionBead.ID, now); open != 0 || closed != 30 {
		t.Fatalf("open/closed waits = %d/%d, want 0/30", open, closed)
	}
	if b, p, m := x.counts(t); b != 0 || p != 0 || m != 0 {
		t.Fatalf("open bindings/participants/memberships = %d/%d/%d, want 0/0/0", b, p, m)
	}
}
