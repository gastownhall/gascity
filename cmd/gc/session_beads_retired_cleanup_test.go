package main

import (
	"bytes"
	"context"
	"errors"
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
	// listErr, when set, is returned by every other List whose label starts
	// with listErrPrefix, after its delay.
	listErr       error
	listErrPrefix string
	// createErr, when set, is returned by every Create.
	createErr error

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
	if s.listErr != nil && strings.HasPrefix(query.Label, s.listErrPrefix) {
		return nil, s.listErr
	}
	return s.MemStore.List(query)
}

func (s *retiredCleanupBudgetStore) Create(b beads.Bead) (beads.Bead, error) {
	if s.createErr != nil {
		return beads.Bead{}, s.createErr
	}
	return s.MemStore.Create(b)
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

func wantOnlyCutoffLine(t *testing.T, stderr, sessionID, leg string) {
	t.Helper()
	lines := nonEmptyLines(stderr)
	want := fmt.Sprintf("session beads: retired-session cleanup for %s: %s leg ran out of its 10s budget; what it did not reach stays open", sessionID, leg)
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("stderr lines = %q, want exactly [%q]", lines, want)
	}
}

// TestRetiredCleanupBoundsSlowWaitsLeg: when wait cancellation on a slow store
// outlasts its 10s budget, the next checkpoint stops it: the pending waits it
// already canceled keep their metadata, the terminal waits it had not reached
// stay open, and exactly one line names the session, the waits leg and the
// budget.
func TestRetiredCleanupBoundsSlowWaitsLeg(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	mem := beads.NewMemStore()
	sessionBead := newRetiredCleanupSessionBead(t, mem)
	seedRetiredCleanupWaits(t, mem, sessionBead.ID, 50, 10)

	// The lookup ends at 6s and the pending waits' CloseAll at 12s; the
	// checkpoint before the terminal waits' CloseAll sees the expired budget.
	store := &retiredCleanupBudgetStore{MemStore: mem, waitLabel: "session:" + sessionBead.ID, waitDelay: 6 * time.Second}
	stderr, elapsed := runRetiredCleanupInBubble(t, store, sessionBead.ID, now)

	wantOnlyCutoffLine(t, stderr, sessionBead.ID, "waits")
	if open, closed := retiredCleanupWaitCounts(t, mem, sessionBead.ID, now); open != 10 || closed != 50 {
		t.Fatalf("open/closed waits = %d/%d, want 10/50 (the terminal waits left open by the cut-off)", open, closed)
	}
	if elapsed != 12*time.Second {
		t.Fatalf("cleanup took %s, want 12s (one in-flight CloseAll past the 10s budget)", elapsed)
	}
}

// TestRetiredCleanupHealthyStoreLargeBacklog: a full lookup page of pending
// waits on a healthy store at 100ms per call is canceled in one lookup and
// one CloseAll, well inside the budget: no cut-off and no wait left open.
func TestRetiredCleanupHealthyStoreLargeBacklog(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	mem := beads.NewMemStore()
	sessionBead := newRetiredCleanupSessionBead(t, mem)
	seedRetiredCleanupWaits(t, mem, sessionBead.ID, waitLookupLimit, 0)

	store := &retiredCleanupBudgetStore{MemStore: mem, waitLabel: "session:" + sessionBead.ID, waitDelay: 100 * time.Millisecond}
	stderr, elapsed := runRetiredCleanupInBubble(t, store, sessionBead.ID, now)

	if stderr != "" {
		t.Fatalf("stderr = %q, want no diagnostics on a healthy store", stderr)
	}
	if open, closed := retiredCleanupWaitCounts(t, mem, sessionBead.ID, now); open != 0 || closed != waitLookupLimit {
		t.Fatalf("open/closed waits = %d/%d, want 0/%d", open, closed, waitLookupLimit)
	}
	if elapsed != 200*time.Millisecond {
		t.Fatalf("waits leg took %s, want 200ms (one lookup and one CloseAll)", elapsed)
	}
}

// TestRetiredCleanupOutcomeClassification: only a leg that fails with its own
// budget's deadline is reported as cut off; every other failure keeps its
// ordinary diagnostic, and work that succeeds past the budget reports nothing.
func TestRetiredCleanupOutcomeClassification(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		waitDelay   time.Duration
		closeAllErr error
		wantLine    string // "" wants no stderr
	}{
		{
			name:        "OrdinaryError",
			closeAllErr: errors.New("store unavailable"),
			wantLine:    "session beads: canceling waits for retired session %s: store unavailable",
		},
		{
			name:        "ForeignDeadlineBeforeBudget",
			closeAllErr: fmt.Errorf("store call: %w", context.DeadlineExceeded),
			wantLine:    "session beads: canceling waits for retired session %s: store call: context deadline exceeded",
		},
		{
			name:        "OrdinaryErrorAfterBudget",
			waitDelay:   6 * time.Second,
			closeAllErr: errors.New("store unavailable"),
			wantLine:    "session beads: canceling waits for retired session %s: store unavailable",
		},
		{
			name:      "FinalBatchOverrunsBudget",
			waitDelay: 6 * time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			sessionBead := newRetiredCleanupSessionBead(t, mem)
			seedRetiredCleanupWaits(t, mem, sessionBead.ID, 5, 0)
			store := &retiredCleanupBudgetStore{
				MemStore:    mem,
				waitLabel:   "session:" + sessionBead.ID,
				waitDelay:   tc.waitDelay,
				closeAllErr: tc.closeAllErr,
			}
			stderr, _ := runRetiredCleanupInBubble(t, store, sessionBead.ID, now)
			lines := nonEmptyLines(stderr)
			if tc.wantLine == "" {
				if len(lines) != 0 {
					t.Fatalf("stderr lines = %q, want none", lines)
				}
				if open, _ := retiredCleanupWaitCounts(t, mem, sessionBead.ID, now); open != 0 {
					t.Fatalf("open waits = %d, want 0", open)
				}
				return
			}
			if want := fmt.Sprintf(tc.wantLine, sessionBead.ID); len(lines) != 1 || lines[0] != want {
				t.Fatalf("stderr lines = %q, want exactly [%q]", lines, want)
			}
		})
	}

	t.Run("NilStderrAndEmptySession", func(t *testing.T) {
		mem := beads.NewMemStore()
		sessionBead := newRetiredCleanupSessionBead(t, mem)
		seedRetiredCleanupWaits(t, mem, sessionBead.ID, 20, 0)
		store := &retiredCleanupBudgetStore{MemStore: mem, waitLabel: "session:" + sessionBead.ID, waitDelay: 3 * time.Second}
		synctest.Test(t, func(*testing.T) {
			cancelStateAssignedToRetiredSessionBead(store, " ", now, nil)
			cancelStateAssignedToRetiredSessionBead(store, sessionBead.ID, now, nil)
		})
		if open, _ := retiredCleanupWaitCounts(t, mem, sessionBead.ID, now); open != 0 {
			t.Fatalf("open waits = %d, want 0", open)
		}
	})
}

// TestRetiredCleanupLegsHaveIndependentBudgets: a waits leg that exhausts its
// budget leaves the external-message leg a fresh one, which still ends every
// binding and closes every participant and membership; the lookup-cap stamp
// survives the waits cut-off.
func TestRetiredCleanupLegsHaveIndependentBudgets(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	mem := beads.NewMemStore()
	sessionBead := newRetiredCleanupSessionBead(t, mem)
	seedRetiredCleanupWaits(t, mem, sessionBead.ID, waitLookupLimit+5, 0)
	x := newRetiredCleanupExtmsg(mem, sessionBead.ID)
	x.seed(t, 2, 2, 1, now)

	store := &retiredCleanupBudgetStore{
		MemStore:    mem,
		waitLabel:   "session:" + sessionBead.ID,
		waitDelay:   4 * time.Second,
		extmsgDelay: 10 * time.Millisecond,
	}
	stderr, _ := runRetiredCleanupInBubble(t, store, sessionBead.ID, now)

	wantOnlyCutoffLine(t, stderr, sessionBead.ID, "waits")
	if open, closed := retiredCleanupWaitCounts(t, mem, sessionBead.ID, now); open == 0 || closed == 0 {
		t.Fatalf("open/closed waits = %d/%d, want some of each after the waits cut-off", open, closed)
	}
	if b, p, m := x.counts(t); b != 0 || p != 0 || m != 0 {
		t.Fatalf("open bindings/participants/memberships = %d/%d/%d, want 0/0/0", b, p, m)
	}
	updated, err := mem.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got := updated.Metadata["wait_lookup_capped_source"]; got != "retired-session-cleanup" {
		t.Fatalf("wait_lookup_capped_source = %q, want retired-session-cleanup", got)
	}
}

const (
	retiredCleanupParticipantLabelPrefix = "extmsg:group:participant:session:v1:"
	retiredCleanupBindingLabelPrefix     = "extmsg:binding:session:v1:"
)

// listsWithPrefix returns the recorded external-message lookups whose label
// starts with prefix.
func (s *retiredCleanupBudgetStore) listsWithPrefix(prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, l := range s.extmsgLists {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

// openRetiredCleanupTeardowns returns the open pending-teardown records naming
// sessionID.
func openRetiredCleanupTeardowns(t *testing.T, store beads.Store, sessionID string) int {
	t.Helper()
	items, err := store.List(beads.ListQuery{Label: "gc:extmsg-session-teardown"})
	if err != nil {
		t.Fatalf("list teardown records: %v", err)
	}
	open := 0
	for _, item := range items {
		if item.Status != "closed" && item.Metadata["session_id"] == sessionID {
			open++
		}
	}
	return open
}

// resumeRetiredCleanupInBubble runs the controller's pass over recorded
// external-message teardowns inside a synctest bubble and returns its stderr.
func resumeRetiredCleanupInBubble(t *testing.T, store beads.Store, now time.Time) string {
	t.Helper()
	var stderr bytes.Buffer
	synctest.Test(t, func(*testing.T) {
		resumePendingExtmsgSessionTeardowns(context.Background(), beads.SessionStore{Store: store}, now, &stderr)
	})
	return stderr.String()
}

// wantNoInboundRoute fails when a group participant handle the fixture seeded
// for the session still routes an inbound message to it.
func (x retiredCleanupExtmsg) wantNoInboundRoute(t *testing.T, participants int) {
	t.Helper()
	for i := 0; i < participants; i++ {
		decision, err := x.fabric.Groups.ResolveInbound(context.Background(), extmsg.ExternalInboundMessage{
			Conversation:   retiredCleanupConversation("group", i),
			ExplicitTarget: fmt.Sprintf("worker%d", i),
		})
		if err != nil {
			t.Fatalf("ResolveInbound %d: %v", i, err)
		}
		if decision.Match != extmsg.GroupRouteNoMatch || decision.TargetSessionID == x.sessionID {
			t.Fatalf("inbound to worker%d = %+v, want no route to retired session %s", i, decision, x.sessionID)
		}
	}
}

// TestRetiredCleanupExternalMessageCutoff: a slow external-message leg stops at
// the next checkpoint once its own budget expires, starts no residual
// participant lookup on the expired budget, never leaves a participant half
// removed, records the unfinished teardown and logs exactly one
// external-message cut-off line; once the store recovers, the controller's
// resume pass closes what the cut-off left and nothing routes to the retired
// session.
func TestRetiredCleanupExternalMessageCutoff(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

	t.Run("BindingLookupOutlastsBudget", func(t *testing.T) {
		mem := beads.NewMemStore()
		sessionBead := newRetiredCleanupSessionBead(t, mem)
		x := newRetiredCleanupExtmsg(mem, sessionBead.ID)
		x.seed(t, 0, 2, 1, now)
		store := &retiredCleanupBudgetStore{MemStore: mem, waitLabel: "session:" + sessionBead.ID, extmsgDelay: 11 * time.Second}

		stderr, _ := runRetiredCleanupInBubble(t, store, sessionBead.ID, now)

		wantOnlyCutoffLine(t, stderr, sessionBead.ID, "external-message")
		if got := store.listsWithPrefix(retiredCleanupBindingLabelPrefix); len(got) != 1 {
			t.Fatalf("binding lookups = %v, want 1", got)
		}
		if got := store.listsWithPrefix(retiredCleanupParticipantLabelPrefix); len(got) != 0 {
			t.Fatalf("participant lookups = %v, want none on the expired budget", got)
		}
		if _, p, m := x.counts(t); p != 2 || m != 3 {
			t.Fatalf("open participants/memberships = %d/%d, want 2/3 left open", p, m)
		}
		if got := openRetiredCleanupTeardowns(t, mem, sessionBead.ID); got != 1 {
			t.Fatalf("open teardown records = %d, want 1", got)
		}

		store.extmsgDelay = 0
		if got := resumeRetiredCleanupInBubble(t, store, now); got != "session reconciler: extmsg session teardowns resumed (finished=1 dropped=0 pending=1)\n" {
			t.Fatalf("resume stderr = %q, want the one finished teardown", got)
		}
		if b, p, m := x.counts(t); b != 0 || p != 0 || m != 0 {
			t.Fatalf("open bindings/participants/memberships after resume = %d/%d/%d, want 0/0/0", b, p, m)
		}
		x.wantNoInboundRoute(t, 2)
		if got := openRetiredCleanupTeardowns(t, mem, sessionBead.ID); got != 0 {
			t.Fatalf("open teardown records after resume = %d, want 0", got)
		}
	})

	t.Run("ParticipantSweepOutlastsBudget", func(t *testing.T) {
		mem := beads.NewMemStore()
		sessionBead := newRetiredCleanupSessionBead(t, mem)
		x := newRetiredCleanupExtmsg(mem, sessionBead.ID)
		x.seed(t, 0, 8, 0, now)
		// No call boundary lands on the 10s deadline, so the cut-off point
		// does not depend on whether the timer or the call wakes first: the
		// binding and participant lookups end at 3s and 6s, the first
		// participant's removal runs whole to 15s, and the checkpoint before
		// the second participant sees the expired budget.
		store := &retiredCleanupBudgetStore{MemStore: mem, waitLabel: "session:" + sessionBead.ID, extmsgDelay: 3 * time.Second}

		stderr, _ := runRetiredCleanupInBubble(t, store, sessionBead.ID, now)

		wantOnlyCutoffLine(t, stderr, sessionBead.ID, "external-message")
		// A removal the cut-off reached runs whole, so every participant left
		// open keeps its membership and no closed one leaves one behind.
		if _, p, m := x.counts(t); p != 7 || m != 7 {
			t.Fatalf("open participants/memberships = %d/%d, want 7/7 (one participant removed whole)", p, m)
		}
		if got := openRetiredCleanupTeardowns(t, mem, sessionBead.ID); got != 1 {
			t.Fatalf("open teardown records = %d, want 1", got)
		}

		store.extmsgDelay = 0
		resumeRetiredCleanupInBubble(t, store, now)
		if b, p, m := x.counts(t); b != 0 || p != 0 || m != 0 {
			t.Fatalf("open bindings/participants/memberships after resume = %d/%d/%d, want 0/0/0", b, p, m)
		}
		x.wantNoInboundRoute(t, 8)
		if got := openRetiredCleanupTeardowns(t, mem, sessionBead.ID); got != 0 {
			t.Fatalf("open teardown records after resume = %d, want 0", got)
		}
	})
}

// TestRetiredCleanupRecordsFailedTeardown: an external-message teardown that
// fails with an ordinary error is recorded for the resume pass too, and a
// record that cannot be written gets its own diagnostic after the leg's.
func TestRetiredCleanupRecordsFailedTeardown(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	errLookup := errors.New("binding lookup failed")
	for _, tc := range []struct {
		name        string
		createErr   error
		wantRecords int
		wantRecord  string // a second stderr line, "" for none
	}{
		{name: "Recorded", wantRecords: 1},
		{
			name:       "RecordFails",
			createErr:  errors.New("store unavailable"),
			wantRecord: "session beads: recording the unfinished external-message teardown of retired session %s for the controller to finish: record pending teardown for session %s: store unavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			sessionBead := newRetiredCleanupSessionBead(t, mem)
			x := newRetiredCleanupExtmsg(mem, sessionBead.ID)
			x.seed(t, 0, 1, 0, now)
			store := &retiredCleanupBudgetStore{
				MemStore:      mem,
				waitLabel:     "session:" + sessionBead.ID,
				listErr:       errLookup,
				listErrPrefix: retiredCleanupBindingLabelPrefix,
				createErr:     tc.createErr,
			}

			stderr, _ := runRetiredCleanupInBubble(t, store, sessionBead.ID, now)

			want := []string{fmt.Sprintf("session beads: closing external message bindings for retired session %s: list bindings for retired session %s: binding lookup failed", sessionBead.ID, sessionBead.ID)}
			if tc.wantRecord != "" {
				want = append(want, fmt.Sprintf(tc.wantRecord, sessionBead.ID, sessionBead.ID))
			}
			if lines := nonEmptyLines(stderr); !slices.Equal(lines, want) {
				t.Fatalf("stderr lines = %q, want %q", lines, want)
			}
			if got := openRetiredCleanupTeardowns(t, mem, sessionBead.ID); got != tc.wantRecords {
				t.Fatalf("open teardown records = %d, want %d", got, tc.wantRecords)
			}
		})
	}
}
