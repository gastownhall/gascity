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

// slowCleanupStore models a store whose calls cost time, the way a bd-backed
// CachingStore does, so the retired-session cleanup's per-leg budget can be
// exercised against the real 10 s budget in a synctest bubble's virtual time.
// Only the call kinds each test names are slow; everything else answers at once.
type slowCleanupStore struct {
	beads.Store
	// closeAllPerID is how long CloseAll takes for each id it is given. A
	// delay proportional to the batch is the faithful CachingStore model: it
	// refreshes every id with a separate read.
	closeAllPerID time.Duration
	// writeDelay is how long each single-bead Get, Update, Close and
	// SetMetadataBatch takes. The waits leg never issues them; the external
	// messaging leg does.
	writeDelay time.Duration
}

func (s *slowCleanupStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if d := s.closeAllPerID * time.Duration(len(ids)); d > 0 {
		<-time.After(d)
	}
	return s.Store.CloseAll(ids, metadata)
}

func (s *slowCleanupStore) Get(id string) (beads.Bead, error) {
	s.wait()
	return s.Store.Get(id)
}

func (s *slowCleanupStore) Update(id string, opts beads.UpdateOpts) error {
	s.wait()
	return s.Store.Update(id, opts)
}

func (s *slowCleanupStore) Close(id string) error {
	s.wait()
	return s.Store.Close(id)
}

func (s *slowCleanupStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.wait()
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *slowCleanupStore) wait() {
	if s.writeDelay > 0 {
		<-time.After(s.writeDelay)
	}
}

// cleanupErrorStore injects an ordinary error in one cleanup store operation.
type cleanupErrorStore struct {
	beads.Store
	closeAllErr error
	listLabel   string
	listErr     error
}

func (s *cleanupErrorStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if s.closeAllErr != nil {
		return 0, s.closeAllErr
	}
	return s.Store.CloseAll(ids, metadata)
}

func (s *cleanupErrorStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if s.listErr != nil && query.Label == s.listLabel {
		return nil, s.listErr
	}
	return s.Store.List(query)
}

// seedRetiredSessionCleanupState gives a session bead from
// setupSessionWithExtmsgMembership extra participants (each in the same group,
// so each has its own removal) and waitCount pending waits, and returns the
// extmsg fixtures the assertions read back through.
func seedRetiredSessionCleanupState(t *testing.T, store beads.Store, extraParticipants, waitCount int) (beads.Bead, extmsg.Services, extmsg.Caller) {
	t.Helper()
	sessionBead, fabric, caller := setupSessionWithExtmsgMembership(t, store, session.StateArchived)
	group, err := fabric.Groups.EnsureGroup(context.Background(), caller, extmsg.EnsureGroupInput{
		RootConversation: extmsg.ConversationRef{
			ScopeID:        "ds-research",
			Provider:       "slack",
			AccountID:      "T0WORKSPACE",
			ConversationID: "C0CHANNEL",
			Kind:           extmsg.ConversationRoom,
		},
		Mode: extmsg.GroupModeLauncher,
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	for i := 0; i < extraParticipants; i++ {
		if _, err := fabric.Groups.UpsertParticipant(context.Background(), caller, extmsg.UpsertParticipantInput{
			GroupID:   group.ID,
			Handle:    fmt.Sprintf("worker-extra-%d", i),
			SessionID: sessionBead.ID,
		}); err != nil {
			t.Fatalf("UpsertParticipant %d: %v", i, err)
		}
	}
	for i := 0; i < waitCount; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("wait-%d", i),
			Type:   waitBeadType,
			Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
			Metadata: map[string]string{
				"session_id": sessionBead.ID,
				"state":      waitStatePending,
			},
		}); err != nil {
			t.Fatalf("create wait %d: %v", i, err)
		}
	}
	return sessionBead, fabric, caller
}

// countClosedAndOpenWaits returns how many of the session's waits are closed
// and how many are still open.
func countClosedAndOpenWaits(t *testing.T, store beads.Store, sessionID string) (closed, open int) {
	t.Helper()
	waits, err := store.List(beads.ListQuery{Label: "session:" + sessionID, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list waits: %v", err)
	}
	for _, w := range waits {
		if !session.IsWaitBead(w) {
			continue
		}
		if w.Status == "closed" {
			closed++
		} else {
			open++
		}
	}
	return closed, open
}

// countOpenExtmsgParticipants returns how many gc:extmsg-participant beads are
// still open. The store holds only the session under test's participants.
func countOpenExtmsgParticipants(t *testing.T, store beads.Store) int {
	t.Helper()
	items, err := store.List(beads.ListQuery{Label: "gc:extmsg-participant"})
	if err != nil {
		t.Fatalf("list participants: %v", err)
	}
	open := 0
	for _, item := range items {
		if item.Status != "closed" {
			open++
		}
	}
	return open
}

// TestCancelStateAssignedToRetiredSessionBeadBoundsSlowWaitsLeg pins that a
// waits leg whose CloseAll calls are slow is cut off at its 10 s budget: some
// but not all waits end closed, the log names the session, the leg and the
// budget, the call returns within the budget plus one chunk's overshoot, and
// the external messaging leg still ran under its own budget.
func TestCancelStateAssignedToRetiredSessionBeadBoundsSlowWaitsLeg(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const waitCount = 100
		store := beads.NewMemStore()
		sessionBead, fabric, caller := seedRetiredSessionCleanupState(t, store, 0, waitCount)
		if got := countOpenMembershipsForSession(t, fabric, caller, sessionBead.ID); got != 1 {
			t.Fatalf("open memberships before cleanup = %d, want 1 (setup precondition)", got)
		}
		slow := &slowCleanupStore{Store: store, closeAllPerID: 400 * time.Millisecond}

		var stderr bytes.Buffer
		start := time.Now()
		cancelStateAssignedToRetiredSessionBead(slow, sessionBead.ID, time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC), &stderr)
		elapsed := time.Since(start)

		closed, open := countClosedAndOpenWaits(t, store, sessionBead.ID)
		if closed == 0 || open == 0 {
			t.Fatalf("waits closed/open = %d/%d, want some closed and some left open by the cut-off", closed, open)
		}
		log := stderr.String()
		for _, want := range []string{sessionBead.ID, "waits leg cut off", "10s"} {
			if !strings.Contains(log, want) {
				t.Fatalf("stderr = %q, want it to contain %q", log, want)
			}
		}
		if strings.Contains(log, "extmsg leg cut off") {
			t.Fatalf("stderr = %q, want only the waits leg cut off", log)
		}
		if elapsed > 20*time.Second {
			t.Fatalf("cleanup took %s of store time, want the waits leg held near its 10s budget", elapsed)
		}
		if got := countOpenMembershipsForSession(t, fabric, caller, sessionBead.ID); got != 0 {
			t.Fatalf("open memberships after cleanup = %d, want 0: the extmsg leg must run after a cut-off waits leg", got)
		}
	})
}

// TestCancelStateAssignedToRetiredSessionBeadBoundsSlowExtmsgLeg pins that an
// external messaging leg whose writes are slow is cut off at its own 10 s
// budget: the log names the leg, something is left open, and the waits leg
// that ran first is unaffected.
func TestCancelStateAssignedToRetiredSessionBeadBoundsSlowExtmsgLeg(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const waitCount = 3
		store := beads.NewMemStore()
		sessionBead, fabric, caller := seedRetiredSessionCleanupState(t, store, 3, waitCount)
		slow := &slowCleanupStore{Store: store, writeDelay: 6 * time.Second}

		var stderr bytes.Buffer
		cancelStateAssignedToRetiredSessionBead(slow, sessionBead.ID, time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC), &stderr)

		log := stderr.String()
		for _, want := range []string{sessionBead.ID, "extmsg leg cut off", "10s"} {
			if !strings.Contains(log, want) {
				t.Fatalf("stderr = %q, want it to contain %q", log, want)
			}
		}
		if strings.Contains(log, "waits leg cut off") {
			t.Fatalf("stderr = %q, want the waits leg left alone", log)
		}
		if closed, open := countClosedAndOpenWaits(t, store, sessionBead.ID); closed != waitCount || open != 0 {
			t.Fatalf("waits closed/open = %d/%d, want %d/0: the waits leg is independent of the extmsg leg", closed, open, waitCount)
		}
		leftover := countOpenExtmsgParticipants(t, store) + countOpenMembershipsForSession(t, fabric, caller, sessionBead.ID)
		if leftover == 0 {
			t.Fatal("no participant or membership left open, want the cut-off to leave at least one")
		}
	})
}

// TestCancelStateAssignedToRetiredSessionBeadHealthyStoreIsNotCutOff pins that
// a store that answers at once logs nothing and leaves nothing behind.
func TestCancelStateAssignedToRetiredSessionBeadHealthyStoreIsNotCutOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const waitCount = 25
		store := beads.NewMemStore()
		sessionBead, fabric, caller := seedRetiredSessionCleanupState(t, store, 2, waitCount)

		var stderr bytes.Buffer
		cancelStateAssignedToRetiredSessionBead(store, sessionBead.ID, time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC), &stderr)

		if strings.Contains(stderr.String(), "cut off") {
			t.Fatalf("stderr = %q, want no cut-off line on a healthy store", stderr.String())
		}
		if closed, open := countClosedAndOpenWaits(t, store, sessionBead.ID); closed != waitCount || open != 0 {
			t.Fatalf("waits closed/open = %d/%d, want %d/0", closed, open, waitCount)
		}
		if got := countOpenExtmsgParticipants(t, store); got != 0 {
			t.Fatalf("open participants = %d, want 0", got)
		}
		if got := countOpenMembershipsForSession(t, fabric, caller, sessionBead.ID); got != 0 {
			t.Fatalf("open memberships = %d, want 0", got)
		}
	})
}

// TestCancelStateAssignedToRetiredSessionBeadOrdinaryWaitErrorIsNotCutOff pins
// that canceling the waits context after an ordinary CloseAll error does not
// reclassify that error as a deadline cut-off.
func TestCancelStateAssignedToRetiredSessionBeadOrdinaryWaitErrorIsNotCutOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := beads.NewMemStore()
		sessionBead, _, _ := seedRetiredSessionCleanupState(t, store, 0, 1)
		storeErr := fmt.Errorf("wait store unavailable")
		failing := &cleanupErrorStore{Store: store, closeAllErr: storeErr}

		var stderr bytes.Buffer
		cancelStateAssignedToRetiredSessionBead(failing, sessionBead.ID, time.Now(), &stderr)

		log := stderr.String()
		want := fmt.Sprintf("canceling waits for retired session %s: %v", sessionBead.ID, storeErr)
		if !strings.Contains(log, want) || strings.Contains(log, "cut off") {
			t.Fatalf("stderr = %q, want ordinary wait error %q and no cut-off line", log, want)
		}
	})
}

// TestCancelStateAssignedToRetiredSessionBeadOrdinaryParticipantErrorIsNotCutOff
// pins that an ordinary residual-participant listing error keeps the external
// message bindings error line while the extmsg context is still live.
func TestCancelStateAssignedToRetiredSessionBeadOrdinaryParticipantErrorIsNotCutOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := beads.NewMemStore()
		sessionBead, _, _ := seedRetiredSessionCleanupState(t, store, 0, 1)
		storeErr := fmt.Errorf("participant store unavailable")
		failing := &cleanupErrorStore{
			Store:     store,
			listLabel: "extmsg:group:participant:session:v1:" + sessionBead.ID,
			listErr:   storeErr,
		}

		var stderr bytes.Buffer
		cancelStateAssignedToRetiredSessionBead(failing, sessionBead.ID, time.Now(), &stderr)

		log := stderr.String()
		want := fmt.Sprintf("closing external message bindings for retired session %s:", sessionBead.ID)
		if !strings.Contains(log, want) || !strings.Contains(log, storeErr.Error()) || strings.Contains(log, "cut off") {
			t.Fatalf("stderr = %q, want ordinary bindings error %q with %q and no cut-off line", log, want, storeErr)
		}
	})
}

// TestCancelStateAssignedToRetiredSessionBeadSuccessfulWaitOvershootIsNotCutOff
// pins that a final CloseAll which succeeds after its deadline closes every
// wait and logs no cut-off, since no further store call was left to truncate.
func TestCancelStateAssignedToRetiredSessionBeadSuccessfulWaitOvershootIsNotCutOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const waitCount = 3
		store := beads.NewMemStore()
		sessionBead, _, _ := seedRetiredSessionCleanupState(t, store, 0, waitCount)
		slow := &slowCleanupStore{Store: store, closeAllPerID: 4 * time.Second}

		var stderr bytes.Buffer
		start := time.Now()
		cancelStateAssignedToRetiredSessionBead(slow, sessionBead.ID, start, &stderr)

		if elapsed := time.Since(start); elapsed <= retiredSessionCleanupLegBudget {
			t.Fatalf("cleanup took %s of store time, want a successful CloseAll past its %s budget", elapsed, retiredSessionCleanupLegBudget)
		}
		if log := stderr.String(); log != "" {
			t.Fatalf("stderr = %q, want no error or cut-off line after successful wait cleanup", log)
		}
		if closed, open := countClosedAndOpenWaits(t, store, sessionBead.ID); closed != waitCount || open != 0 {
			t.Fatalf("waits closed/open = %d/%d, want %d/0", closed, open, waitCount)
		}
	})
}

// requireOneCutOffLine fails unless log is exactly one line that names the
// session, the cut-off leg and the leg budget, and carries none of the ordinary
// error line (ordinary is its message prefix) that the same error would
// otherwise produce.
func requireOneCutOffLine(t *testing.T, log, sessionID, leg, ordinary string) {
	t.Helper()
	if !strings.HasSuffix(log, "\n") || strings.Count(log, "\n") != 1 {
		t.Fatalf("stderr = %q, want exactly one line for a cut-off %s leg", log, leg)
	}
	for _, want := range []string{sessionID, fmt.Sprintf("%s leg cut off at its %s budget", leg, retiredSessionCleanupLegBudget)} {
		if !strings.Contains(log, want) {
			t.Fatalf("stderr = %q, want it to contain %q", log, want)
		}
	}
	if strings.Contains(log, ordinary) {
		t.Fatalf("stderr = %q, want the ordinary %q line left out of a cut-off %s leg", log, ordinary, leg)
	}
}

// TestCancelStateAssignedToRetiredSessionBeadWaitsLegCutOffLogsExactlyOneLine
// pins that a cut-off waits leg logs the cut-off line alone, not also the
// ordinary "canceling waits" error line the same error would produce.
func TestCancelStateAssignedToRetiredSessionBeadWaitsLegCutOffLogsExactlyOneLine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := beads.NewMemStore()
		sessionBead, _, _ := seedRetiredSessionCleanupState(t, store, 0, 100)
		slow := &slowCleanupStore{Store: store, closeAllPerID: 400 * time.Millisecond}

		var stderr bytes.Buffer
		cancelStateAssignedToRetiredSessionBead(slow, sessionBead.ID, time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC), &stderr)

		requireOneCutOffLine(t, stderr.String(), sessionBead.ID, "waits", "canceling waits for retired session")
	})
}

// TestCancelStateAssignedToRetiredSessionBeadExtmsgLegCutOffLogsExactlyOneLine
// pins the same for a cut-off external messaging leg, against its ordinary
// "closing external message bindings" error line.
func TestCancelStateAssignedToRetiredSessionBeadExtmsgLegCutOffLogsExactlyOneLine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := beads.NewMemStore()
		sessionBead, _, _ := seedRetiredSessionCleanupState(t, store, 3, 3)
		slow := &slowCleanupStore{Store: store, writeDelay: 6 * time.Second}

		var stderr bytes.Buffer
		cancelStateAssignedToRetiredSessionBead(slow, sessionBead.ID, time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC), &stderr)

		requireOneCutOffLine(t, stderr.String(), sessionBead.ID, "extmsg", "closing external message bindings for retired session")
	})
}
