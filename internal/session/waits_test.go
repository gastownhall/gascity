package session

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

type rejectLegacyWaitTypeQueryStore struct {
	*beads.MemStore
}

type sessionWaitListQueryCaptureStore struct {
	beads.Store
	queries []beads.ListQuery
}

type sessionWaitLimitStore struct {
	beads.Store
}

type sessionWaitExactLimitStore struct {
	beads.Store
}

type cancelWaitMetadataFailStore struct {
	*beads.MemStore
	failID string
}

func (s rejectLegacyWaitTypeQueryStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Type == LegacyWaitBeadType {
		return nil, errors.New("legacy wait type query should not be used")
	}
	return s.MemStore.List(query)
}

func (s *sessionWaitListQueryCaptureStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, query)
	return s.Store.List(query)
}

func (s sessionWaitLimitStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	return sessionWaitItems(query, query.Limit), nil
}

func (s sessionWaitExactLimitStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	return sessionWaitItems(query, SessionWaitLookupLimit), nil
}

func (s cancelWaitMetadataFailStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if id == s.failID {
		return errors.New("cancel wait metadata failed")
	}
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func (s cancelWaitMetadataFailStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	for _, id := range ids {
		if id == s.failID {
			return 0, errors.New("cancel wait metadata failed")
		}
	}
	return s.MemStore.CloseAll(ids, metadata)
}

func sessionWaitItems(query beads.ListQuery, count int) []beads.Bead {
	items := make([]beads.Bead, count)
	for i := range items {
		items[i] = beads.Bead{
			ID:     "wait",
			Type:   WaitBeadType,
			Status: "open",
			Labels: []string{WaitBeadLabel, query.Label},
			Metadata: map[string]string{
				"session_id": strings.TrimPrefix(query.Label, "session:"),
				"state":      "pending",
			},
		}
	}
	return items
}

func TestWaitNudgeIDs_AcceptsLegacyWaitBeadsWithoutLegacyTypeQuery(t *testing.T) {
	store := rejectLegacyWaitTypeQueryStore{MemStore: beads.NewMemStore()}
	if _, err := store.Create(beads.Bead{
		Type:   LegacyWaitBeadType,
		Labels: []string{WaitBeadLabel, "session:gc-session"},
		Metadata: map[string]string{
			"session_id": "gc-session",
			"state":      "pending",
			"nudge_id":   "wait-nudge",
		},
	}); err != nil {
		t.Fatalf("create legacy wait: %v", err)
	}

	got, err := WaitNudgeIDs(store, "gc-session")
	if err != nil {
		t.Fatalf("WaitNudgeIDs: %v", err)
	}
	if len(got) != 1 || got[0] != "wait-nudge" {
		t.Fatalf("WaitNudgeIDs = %#v, want [wait-nudge]", got)
	}
}

func TestWaitNudgeIDs_UsesBoundedDeterministicSessionLookup(t *testing.T) {
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{
		Type:   WaitBeadType,
		Labels: []string{WaitBeadLabel, "session:gc-session"},
		Metadata: map[string]string{
			"session_id": "gc-session",
			"state":      "pending",
			"nudge_id":   "wait-nudge",
		},
	}); err != nil {
		t.Fatalf("create wait: %v", err)
	}
	store := &sessionWaitListQueryCaptureStore{Store: mem}

	got, err := WaitNudgeIDs(store, "gc-session")
	if err != nil {
		t.Fatalf("WaitNudgeIDs: %v", err)
	}
	if len(got) != 1 || got[0] != "wait-nudge" {
		t.Fatalf("WaitNudgeIDs = %#v, want [wait-nudge]", got)
	}
	if len(store.queries) != 1 {
		t.Fatalf("List calls = %d, want 1", len(store.queries))
	}
	if got := store.queries[0].Limit; got != SessionWaitLookupLimit+1 {
		t.Fatalf("List limit = %d, want %d", got, SessionWaitLookupLimit+1)
	}
	if got := store.queries[0].Status; got != "open" {
		t.Fatalf("List status = %q, want open", got)
	}
	if got := store.queries[0].Sort; got != beads.SortCreatedDesc {
		t.Fatalf("List sort = %q, want %q", got, beads.SortCreatedDesc)
	}
}

func TestListSessionWaitBeads_AllowsExactLookupLimit(t *testing.T) {
	store := &sessionWaitExactLimitStore{Store: beads.NewMemStore()}

	waits, err := ListSessionWaitBeads(store, "gc-session")
	if err != nil {
		t.Fatalf("ListSessionWaitBeads: %v", err)
	}
	if len(waits) != SessionWaitLookupLimit {
		t.Fatalf("wait count = %d, want %d", len(waits), SessionWaitLookupLimit)
	}
}

func TestListSessionWaitBeads_ReportsLimitWithFilteredPartial(t *testing.T) {
	waits, err := ListSessionWaitBeads(sessionWaitLimitStore{Store: beads.NewMemStore()}, "gc-session")
	if !beads.IsLookupLimitError(err) {
		t.Fatalf("ListSessionWaitBeads error = %v, want lookup limit", err)
	}
	if len(waits) != SessionWaitLookupLimit {
		t.Fatalf("wait count = %d, want filtered partial count %d", len(waits), SessionWaitLookupLimit)
	}
}

func TestWaitNudgeIDs_ReportsSessionWaitLookupLimit(t *testing.T) {
	_, err := WaitNudgeIDs(sessionWaitLimitStore{Store: beads.NewMemStore()}, "gc-session")
	if !beads.IsLookupLimitError(err) || !strings.Contains(err.Error(), "wait lookup hit limit") {
		t.Fatalf("WaitNudgeIDs error = %v, want wait lookup limit", err)
	}
}

func TestWakeSessionContinuesAfterWaitLookupLimit(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Date(2026, 5, 15, 9, 30, 0, 0, time.UTC)
	sessionBead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state":        string(StateSuspended),
			"state_reason": "wait-hold",
			"held_until":   now.Add(time.Hour).Format(time.RFC3339),
			"wait_hold":    "true",
			"sleep_reason": "deps",
		},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	for i := 0; i < SessionWaitLookupLimit+1; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("wait-%d", i),
			Type:   WaitBeadType,
			Labels: []string{WaitBeadLabel, "session:" + sessionBead.ID},
			Metadata: map[string]string{
				"session_id": sessionBead.ID,
				"state":      "pending",
				"nudge_id":   fmt.Sprintf("nudge-%d", i),
			},
		}); err != nil {
			t.Fatalf("create wait %d: %v", i, err)
		}
	}

	nudgeIDs, err := WakeSession(store, sessionBead, now)
	if err != nil {
		t.Fatalf("WakeSession: %v", err)
	}
	if len(nudgeIDs) != SessionWaitLookupLimit+1 {
		t.Fatalf("nudge ID count = %d, want %d", len(nudgeIDs), SessionWaitLookupLimit+1)
	}
	updatedSession, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := updatedSession.Metadata["state"]; got != string(StateCreating) {
		t.Fatalf("state = %q, want creating", got)
	}
	if got := updatedSession.Metadata["wait_lookup_capped_label"]; got != "session:"+sessionBead.ID {
		t.Fatalf("wait_lookup_capped_label = %q, want session label", got)
	}
	if got := updatedSession.Metadata["wait_lookup_capped_limit"]; got != "1000" {
		t.Fatalf("wait_lookup_capped_limit = %q, want 1000", got)
	}
	if got := updatedSession.Metadata["wait_lookup_capped_source"]; got != "wake-session" {
		t.Fatalf("wait_lookup_capped_source = %q, want wake-session", got)
	}
	waits, err := store.List(beads.ListQuery{Label: "session:" + sessionBead.ID, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list waits: %v", err)
	}
	for _, wait := range waits {
		if !IsWaitBead(wait) {
			continue
		}
		if wait.Status != "closed" || wait.Metadata["state"] != waitStateCanceled {
			t.Fatalf("wait %s status/state = %q/%q, want closed/canceled", wait.ID, wait.Status, wait.Metadata["state"])
		}
	}
}

func TestCancelWaitsAndCollectNudgeIDsReturnsAllNudgesAfterLookupLimit(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	sessionID := "gc-session"
	for i := 0; i < SessionWaitLookupLimit+1; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("wait-%d", i),
			Type:   WaitBeadType,
			Labels: []string{WaitBeadLabel, "session:" + sessionID},
			Metadata: map[string]string{
				"session_id": sessionID,
				"state":      "pending",
				"nudge_id":   fmt.Sprintf("nudge-%d", i),
			},
		}); err != nil {
			t.Fatalf("create wait %d: %v", i, err)
		}
	}

	nudgeIDs, capped, err := CancelWaitsAndCollectNudgeIDs(store, sessionID, now)
	if err != nil {
		t.Fatalf("CancelWaitsAndCollectNudgeIDs: %v", err)
	}
	if !capped {
		t.Fatal("capped = false, want true")
	}
	if len(nudgeIDs) != SessionWaitLookupLimit+1 {
		t.Fatalf("nudge ID count = %d, want %d", len(nudgeIDs), SessionWaitLookupLimit+1)
	}
	seen := map[string]bool{}
	for _, id := range nudgeIDs {
		seen[id] = true
	}
	for _, id := range []string{"nudge-0", fmt.Sprintf("nudge-%d", SessionWaitLookupLimit)} {
		if !seen[id] {
			t.Fatalf("nudge IDs missing %q from first or later capped page", id)
		}
	}
	waits, err := store.List(beads.ListQuery{Label: "session:" + sessionID, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list waits: %v", err)
	}
	for _, wait := range waits {
		if !IsWaitBead(wait) {
			continue
		}
		if wait.Status != "closed" || wait.Metadata["state"] != waitStateCanceled {
			t.Fatalf("wait %s status/state = %q/%q, want closed/canceled", wait.ID, wait.Status, wait.Metadata["state"])
		}
	}
}

func TestCancelWaitsAndCollectNudgeIDsReturnsObservedNudgesOnCancelError(t *testing.T) {
	mem := beads.NewMemStore()
	wait, err := mem.Create(beads.Bead{
		Title:  "wait",
		Type:   WaitBeadType,
		Labels: []string{WaitBeadLabel, "session:gc-session"},
		Metadata: map[string]string{
			"session_id": "gc-session",
			"state":      "pending",
			"nudge_id":   "wait-nudge",
		},
	})
	if err != nil {
		t.Fatalf("create wait: %v", err)
	}
	store := cancelWaitMetadataFailStore{MemStore: mem, failID: wait.ID}

	nudgeIDs, capped, err := CancelWaitsAndCollectNudgeIDs(store, "gc-session", time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "cancel wait metadata failed") {
		t.Fatalf("CancelWaitsAndCollectNudgeIDs error = %v, want cancel wait metadata failed", err)
	}
	if capped {
		t.Fatal("capped = true, want false")
	}
	if len(nudgeIDs) != 1 || nudgeIDs[0] != "wait-nudge" {
		t.Fatalf("nudgeIDs = %#v, want [wait-nudge]", nudgeIDs)
	}
}

func TestWakeSessionClosesTerminalOpenWaitsAfterLookupLimit(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Date(2026, 5, 15, 9, 45, 0, 0, time.UTC)
	sessionBead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state":        string(StateSuspended),
			"state_reason": "wait-hold",
			"wait_hold":    "true",
			"sleep_reason": "deps",
		},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	for i := 0; i < SessionWaitLookupLimit+1; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("wait-%d", i),
			Type:   WaitBeadType,
			Labels: []string{WaitBeadLabel, "session:" + sessionBead.ID},
			Metadata: map[string]string{
				"session_id": sessionBead.ID,
				"state":      waitStateCanceled,
				"nudge_id":   fmt.Sprintf("nudge-%d", i),
			},
		}); err != nil {
			t.Fatalf("create wait %d: %v", i, err)
		}
	}

	nudgeIDs, err := WakeSession(store, sessionBead, now)
	if err != nil {
		t.Fatalf("WakeSession: %v", err)
	}
	if len(nudgeIDs) != SessionWaitLookupLimit+1 {
		t.Fatalf("nudge ID count = %d, want %d", len(nudgeIDs), SessionWaitLookupLimit+1)
	}
	waits, err := store.List(beads.ListQuery{Label: "session:" + sessionBead.ID, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list waits: %v", err)
	}
	for _, wait := range waits {
		if !IsWaitBead(wait) {
			continue
		}
		if wait.Status != "closed" || wait.Metadata["state"] != waitStateCanceled {
			t.Fatalf("wait %s status/state = %q/%q, want closed/canceled", wait.ID, wait.Status, wait.Metadata["state"])
		}
	}
	updatedSession, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := updatedSession.Metadata["wait_lookup_capped_source"]; got != "wake-session" {
		t.Fatalf("wait_lookup_capped_source = %q, want wake-session", got)
	}
}

func TestReassignWaitsConvergesAfterWaitLookupLimit(t *testing.T) {
	store := beads.NewMemStore()
	oldSessionID := "old-session"
	newSessionID := "new-session"
	oldLabel := "session:" + oldSessionID
	newLabel := "session:" + newSessionID
	for i := 0; i < SessionWaitLookupLimit+1; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("wait-%d", i),
			Type:   WaitBeadType,
			Labels: []string{WaitBeadLabel, oldLabel},
			Metadata: map[string]string{
				"session_id": oldSessionID,
				"state":      "pending",
			},
		}); err != nil {
			t.Fatalf("create wait %d: %v", i, err)
		}
	}

	if err := ReassignWaits(store, oldSessionID, newSessionID); err != nil {
		t.Fatalf("ReassignWaits: %v", err)
	}
	oldRows, err := store.List(beads.ListQuery{Label: oldLabel})
	if err != nil {
		t.Fatalf("list old session waits: %v", err)
	}
	if len(oldRows) != 0 {
		t.Fatalf("old session wait count = %d, want 0", len(oldRows))
	}
	newRows, err := store.List(beads.ListQuery{Label: newLabel})
	if err != nil {
		t.Fatalf("list new session waits: %v", err)
	}
	if len(newRows) != SessionWaitLookupLimit+1 {
		t.Fatalf("new session wait count = %d, want %d", len(newRows), SessionWaitLookupLimit+1)
	}
	for _, wait := range newRows {
		if wait.Metadata["session_id"] != newSessionID {
			t.Fatalf("wait %s session_id = %q, want %q", wait.ID, wait.Metadata["session_id"], newSessionID)
		}
		if beadHasLabel(wait, oldLabel) {
			t.Fatalf("wait %s still has old label %q", wait.ID, oldLabel)
		}
		if !beadHasLabel(wait, newLabel) {
			t.Fatalf("wait %s missing new label %q", wait.ID, newLabel)
		}
	}
}

func TestCancelWaits_CancelsLegacyWaitBeadsWithoutLegacyTypeQuery(t *testing.T) {
	store := rejectLegacyWaitTypeQueryStore{MemStore: beads.NewMemStore()}
	wait, err := store.Create(beads.Bead{
		Type:   LegacyWaitBeadType,
		Labels: []string{WaitBeadLabel, "session:gc-session"},
		Metadata: map[string]string{
			"session_id": "gc-session",
			"state":      "pending",
		},
	})
	if err != nil {
		t.Fatalf("create legacy wait: %v", err)
	}

	if err := CancelWaits(store, "gc-session", time.Now().UTC()); err != nil {
		t.Fatalf("CancelWaits: %v", err)
	}
	updated, err := store.Get(wait.ID)
	if err != nil {
		t.Fatalf("Get(wait): %v", err)
	}
	if updated.Metadata["state"] != waitStateCanceled {
		t.Fatalf("state = %q, want %q", updated.Metadata["state"], waitStateCanceled)
	}
	if updated.Status != "closed" {
		t.Fatalf("status = %q, want closed", updated.Status)
	}
}

func TestWakeSessionRequestsStartForSuspendedBead(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Date(2026, 5, 3, 8, 30, 0, 0, time.UTC)
	sessionBead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state":        string(StateSuspended),
			"state_reason": "user-hold",
			"held_until":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"wait_hold":    "true",
			"sleep_reason": "user-hold",
		},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if _, err := WakeSession(store, sessionBead, now); err != nil {
		t.Fatalf("WakeSession: %v", err)
	}

	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := updated.Metadata["state"]; got != string(StateCreating) {
		t.Fatalf("state = %q, want creating", got)
	}
	if got := updated.Metadata["state_reason"]; got != string(WakeCauseExplicit) {
		t.Fatalf("state_reason = %q, want explicit", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true", got)
	}
	if got, want := updated.Metadata["pending_create_started_at"], now.UTC().Format(time.RFC3339); got != want {
		t.Fatalf("pending_create_started_at = %q, want %q", got, want)
	}
	for _, key := range []string{"held_until", "wait_hold", "sleep_reason"} {
		if got := updated.Metadata[key]; got != "" {
			t.Fatalf("%s = %q, want cleared", key, got)
		}
	}
}

func TestWakeSessionRejectsArchivedHistoricalBead(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state":               "archived",
			"continuity_eligible": "false",
			"held_until":          time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	wait, err := store.Create(beads.Bead{
		Type:   WaitBeadType,
		Labels: []string{WaitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id": sessionBead.ID,
			"state":      "pending",
		},
	})
	if err != nil {
		t.Fatalf("create wait: %v", err)
	}

	if _, err := WakeSession(store, sessionBead, time.Now().UTC()); err == nil {
		t.Fatal("WakeSession returned nil error, want archived-session rejection")
	}

	updatedSession, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := updatedSession.Metadata["held_until"]; got == "" {
		t.Fatal("held_until was cleared on rejected archived wake")
	}
	updatedWait, err := store.Get(wait.ID)
	if err != nil {
		t.Fatalf("Get(wait): %v", err)
	}
	if updatedWait.Status == "closed" || updatedWait.Metadata["state"] == waitStateCanceled {
		t.Fatalf("wait was canceled on rejected archived wake: status=%q state=%q", updatedWait.Status, updatedWait.Metadata["state"])
	}
}

func TestWakeSessionRequestsStartForContinuityEligibleArchivedBead(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Date(2026, 5, 3, 8, 45, 0, 0, time.UTC)
	sessionBead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state":                "archived",
			"state_reason":         "quarantine-recovery",
			"continuity_eligible":  "true",
			"archived_at":          time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			"held_until":           time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"quarantined_until":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"wait_hold":            "true",
			"sleep_intent":         "wait-hold",
			"sleep_reason":         "quarantine",
			"pending_create_claim": "",
		},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	wait, err := store.Create(beads.Bead{
		Type:   WaitBeadType,
		Labels: []string{WaitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id": sessionBead.ID,
			"state":      "pending",
		},
	})
	if err != nil {
		t.Fatalf("create wait: %v", err)
	}

	if _, err := WakeSession(store, sessionBead, now); err != nil {
		t.Fatalf("WakeSession: %v", err)
	}

	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := updated.Metadata["state"]; got != string(StateCreating) {
		t.Fatalf("state = %q, want creating", got)
	}
	if got := updated.Metadata["state_reason"]; got != "explicit" {
		t.Fatalf("state_reason = %q, want explicit", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true", got)
	}
	if got, want := updated.Metadata["pending_create_started_at"], now.UTC().Format(time.RFC3339); got != want {
		t.Fatalf("pending_create_started_at = %q, want %q", got, want)
	}
	for _, key := range []string{"held_until", "quarantined_until", "wait_hold", "sleep_intent", "sleep_reason", "archived_at"} {
		if got := updated.Metadata[key]; got != "" {
			t.Fatalf("%s = %q, want cleared", key, got)
		}
	}
	updatedWait, err := store.Get(wait.ID)
	if err != nil {
		t.Fatalf("Get(wait): %v", err)
	}
	if updatedWait.Status != "closed" || updatedWait.Metadata["state"] != waitStateCanceled {
		t.Fatalf("wait status/state = %q/%q, want closed/canceled", updatedWait.Status, updatedWait.Metadata["state"])
	}
}
