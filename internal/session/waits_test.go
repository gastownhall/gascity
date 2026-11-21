package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestWaitInfoFromBead_ProjectsAllFields(t *testing.T) {
	created := time.Date(2026, 5, 15, 9, 30, 0, 0, time.UTC)
	b := beads.Bead{
		ID:          "gc-wait-1",
		Type:        WaitBeadType,
		Status:      "closed",
		Title:       "wait:worker",
		Description: "Continue after review closes.",
		CreatedAt:   created,
		Labels:      []string{WaitBeadLabel, "session:gc-session"},
		Metadata: map[string]string{
			"session_id":       "gc-session",
			"session_name":     "worker",
			"kind":             "deps",
			"state":            "ready",
			"dep_ids":          "gc-1,gc-2",
			"dep_mode":         "all",
			"registered_epoch": "3",
			"delivery_attempt": "2",
			"nudge_id":         "wait-gc-wait-1-3-2",
			"expires_at":       "2026-05-16T09:30:00Z",
		},
	}
	got := WaitInfoFromBead(b)
	want := WaitInfo{
		ID:              "gc-wait-1",
		SessionID:       "gc-session",
		SessionName:     "worker",
		Kind:            "deps",
		State:           "ready",
		DepIDs:          []string{"gc-1", "gc-2"},
		DepMode:         "all",
		RegisteredEpoch: "3",
		DeliveryAttempt: "2",
		NudgeID:         "wait-gc-wait-1-3-2",
		ExpiresAt:       "2026-05-16T09:30:00Z",
		Note:            "Continue after review closes.",
		Status:          "closed",
		CreatedAt:       created,
		Labels:          []string{WaitBeadLabel, "session:gc-session"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WaitInfoFromBead = %#v, want %#v", got, want)
	}
}

func TestWaitInfoFromBead_DepIDsSplitTrimEmpty(t *testing.T) {
	cases := []struct {
		name   string
		depIDs string
		want   []string
	}{
		{"trims and drops empties", " a , b ,,c ", []string{"a", "b", "c"}},
		{"empty string", "", nil},
		{"single id", "gc-1", []string{"gc-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WaitInfoFromBead(beads.Bead{Metadata: map[string]string{"dep_ids": tc.depIDs}}).DepIDs
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DepIDs = %#v, want %#v", got, tc.want)
			}
		})
	}
	if got := WaitInfoFromBead(beads.Bead{}).DepIDs; got != nil {
		t.Fatalf("DepIDs for absent dep_ids key = %#v, want nil", got)
	}
}

func TestListSessionWaits_ReturnsProjectedWaitInfo(t *testing.T) {
	store := beads.NewMemStore()
	created, err := store.Create(beads.Bead{
		Type:   WaitBeadType,
		Labels: []string{WaitBeadLabel, "session:gc-session"},
		Metadata: map[string]string{
			"session_id": "gc-session",
			"state":      "ready",
			"nudge_id":   "wait-nudge",
		},
	})
	if err != nil {
		t.Fatalf("create wait: %v", err)
	}
	waits, err := waitStoreOver(store).WaitsForSession("gc-session")
	if err != nil {
		t.Fatalf("ListSessionWaits: %v", err)
	}
	if len(waits) != 1 {
		t.Fatalf("wait count = %d, want 1", len(waits))
	}
	if w := waits[0]; w.ID != created.ID || w.SessionID != "gc-session" || w.State != "ready" || w.NudgeID != "wait-nudge" {
		t.Fatalf("WaitInfo = %#v, want id=%s session=gc-session state=ready nudge=wait-nudge", w, created.ID)
	}
}

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

	got, err := waitStoreOver(store).WaitNudgeIDs("gc-session")
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

	got, err := waitStoreOver(store).WaitNudgeIDs("gc-session")
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

func TestListSessionWaits_AllowsExactLookupLimit(t *testing.T) {
	store := &sessionWaitExactLimitStore{Store: beads.NewMemStore()}

	waits, err := waitStoreOver(store).WaitsForSession("gc-session")
	if err != nil {
		t.Fatalf("ListSessionWaits: %v", err)
	}
	if len(waits) != SessionWaitLookupLimit {
		t.Fatalf("wait count = %d, want %d", len(waits), SessionWaitLookupLimit)
	}
}

func TestListSessionWaits_ReportsLimitWithFilteredPartial(t *testing.T) {
	waits, err := waitStoreOver(sessionWaitLimitStore{Store: beads.NewMemStore()}).WaitsForSession("gc-session")
	if !beads.IsLookupLimitError(err) {
		t.Fatalf("ListSessionWaits error = %v, want lookup limit", err)
	}
	if len(waits) != SessionWaitLookupLimit {
		t.Fatalf("wait count = %d, want filtered partial count %d", len(waits), SessionWaitLookupLimit)
	}
}

func TestWaitNudgeIDs_ReportsSessionWaitLookupLimit(t *testing.T) {
	_, err := waitStoreOver(sessionWaitLimitStore{Store: beads.NewMemStore()}).WaitNudgeIDs("gc-session")
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

	nudgeIDs, err := waitStoreOver(store).wakeSessionFromBead(sessionBead, now)
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
	if got := updatedSession.Metadata["state"]; got != string(StateAsleep) {
		t.Fatalf("state = %q, want asleep", got)
	}
	if got := updatedSession.Metadata["wake_request"]; got != string(WakeCauseExplicit) {
		t.Fatalf("wake_request = %q, want explicit", got)
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

	nudgeIDs, capped, err := waitStoreOver(store).CancelWaits(context.Background(), sessionID, now)
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

	nudgeIDs, capped, err := waitStoreOver(store).CancelWaits(context.Background(), "gc-session", time.Now().UTC())
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

	nudgeIDs, err := waitStoreOver(store).wakeSessionFromBead(sessionBead, now)
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

	if err := waitStoreOver(store).ReassignWaits(oldSessionID, newSessionID); err != nil {
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

	if _, _, err := waitStoreOver(store).CancelWaits(context.Background(), "gc-session", time.Now().UTC()); err != nil {
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

func TestWakeSessionRecordsExplicitWakeForSuspendedBead(t *testing.T) {
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

	if _, err := waitStoreOver(store).wakeSessionFromBead(sessionBead, now); err != nil {
		t.Fatalf("WakeSession: %v", err)
	}

	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := updated.Metadata["state"]; got != string(StateAsleep) {
		t.Fatalf("state = %q, want asleep", got)
	}
	if got := updated.Metadata["state_reason"]; got != "user-hold" {
		t.Fatalf("state_reason = %q, want user-hold", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "" {
		t.Fatalf("pending_create_claim = %q, want empty", got)
	}
	if got := updated.Metadata["pending_create_started_at"]; got != "" {
		t.Fatalf("pending_create_started_at = %q, want empty", got)
	}
	if got := updated.Metadata["wake_request"]; got != string(WakeCauseExplicit) {
		t.Fatalf("wake_request = %q, want explicit", got)
	}
	if got, want := updated.Metadata["wake_requested_at"], now.UTC().Format(time.RFC3339); got != want {
		t.Fatalf("wake_requested_at = %q, want %q", got, want)
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

	if _, err := waitStoreOver(store).wakeSessionFromBead(sessionBead, time.Now().UTC()); err == nil {
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

func TestWakeSessionRecordsExplicitWakeForContinuityEligibleArchivedBead(t *testing.T) {
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

	if _, err := waitStoreOver(store).wakeSessionFromBead(sessionBead, now); err != nil {
		t.Fatalf("WakeSession: %v", err)
	}

	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := updated.Metadata["state"]; got != "archived" {
		t.Fatalf("state = %q, want archived", got)
	}
	if got := updated.Metadata["state_reason"]; got != "quarantine-recovery" {
		t.Fatalf("state_reason = %q, want quarantine-recovery", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "" {
		t.Fatalf("pending_create_claim = %q, want empty", got)
	}
	if got := updated.Metadata["pending_create_started_at"]; got != "" {
		t.Fatalf("pending_create_started_at = %q, want empty", got)
	}
	if got := updated.Metadata["wake_request"]; got != string(WakeCauseExplicit) {
		t.Fatalf("wake_request = %q, want explicit", got)
	}
	if got, want := updated.Metadata["wake_requested_at"], now.UTC().Format(time.RFC3339); got != want {
		t.Fatalf("wake_requested_at = %q, want %q", got, want)
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

func TestWaitInfoFromBead_DecodesRegisteredResetPending(t *testing.T) {
	flagged := WaitInfoFromBead(beads.Bead{Metadata: map[string]string{
		"registered_epoch":         "2",
		"registered_reset_pending": "true",
	}})
	if !flagged.RegisteredResetPending {
		t.Fatal("RegisteredResetPending = false, want true for registered_reset_pending=true")
	}
	unflagged := WaitInfoFromBead(beads.Bead{Metadata: map[string]string{"registered_epoch": "2"}})
	if unflagged.RegisteredResetPending {
		t.Fatal("RegisteredResetPending = true, want false when registered_reset_pending is absent")
	}
}

func TestWaitInfoContinuationStale(t *testing.T) {
	for _, tc := range []struct {
		name         string
		registered   string
		flagged      bool
		sessionEpoch string
		want         bool
	}{
		{name: "unflagged/equal", registered: "2", sessionEpoch: "2"},
		{name: "unflagged/different", registered: "2", sessionEpoch: "3", want: true},
		{name: "unflagged/empty-registered", sessionEpoch: "3"},
		{name: "unflagged/empty-session", registered: "2"},
		{name: "flagged/at-registered-epoch", registered: "2", flagged: true, sessionEpoch: "2"},
		{name: "flagged/at-next-epoch", registered: "2", flagged: true, sessionEpoch: "3"},
		{name: "flagged/two-epochs-on", registered: "2", flagged: true, sessionEpoch: "4", want: true},
		{name: "flagged/epoch-before", registered: "2", flagged: true, sessionEpoch: "1", want: true},
		{name: "flagged/non-numeric-session", registered: "2", flagged: true, sessionEpoch: "x", want: true},
		{name: "flagged/non-numeric-registered", registered: "x", flagged: true, sessionEpoch: "3", want: true},
		{name: "flagged/empty-session", registered: "2", flagged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := WaitInfo{RegisteredEpoch: tc.registered, RegisteredResetPending: tc.flagged}
			if got := w.ContinuationStale(Info{ContinuationEpoch: tc.sessionEpoch}); got != tc.want {
				t.Fatalf("ContinuationStale(registered=%q flagged=%v, session=%q) = %v, want %v",
					tc.registered, tc.flagged, tc.sessionEpoch, got, tc.want)
			}
		})
	}
}

func TestWaitInfoDeliveryEpoch(t *testing.T) {
	for _, tc := range []struct {
		name         string
		registered   string
		flagged      bool
		sessionEpoch string
		want         string
	}{
		{name: "unflagged/at-registered-epoch", registered: "2", sessionEpoch: "2", want: "2"},
		{name: "unflagged/stale", registered: "2", sessionEpoch: "3", want: "2"},
		{name: "flagged/at-registered-epoch", registered: "2", flagged: true, sessionEpoch: "2", want: "2"},
		{name: "flagged/at-next-epoch", registered: "2", flagged: true, sessionEpoch: "3", want: "3"},
		{name: "flagged/two-epochs-on", registered: "2", flagged: true, sessionEpoch: "4", want: "2"},
		{name: "flagged/epoch-before", registered: "2", flagged: true, sessionEpoch: "1", want: "2"},
		{name: "flagged/non-numeric-session", registered: "2", flagged: true, sessionEpoch: "x", want: "2"},
		{name: "flagged/empty-session", registered: "2", flagged: true, want: "2"},
		{name: "flagged/empty-registered", flagged: true, sessionEpoch: "3", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := WaitInfo{RegisteredEpoch: tc.registered, RegisteredResetPending: tc.flagged}
			if got := w.DeliveryEpoch(Info{ContinuationEpoch: tc.sessionEpoch}); got != tc.want {
				t.Fatalf("DeliveryEpoch(registered=%q flagged=%v, session=%q) = %q, want %q",
					tc.registered, tc.flagged, tc.sessionEpoch, got, tc.want)
			}
		})
	}
}

func TestWaitInfoHeldForPendingReset(t *testing.T) {
	const committedAt = "2026-10-24T16:07:00Z"
	for _, tc := range []struct {
		name             string
		flagged          bool
		sessionEpoch     string
		resetPending     string
		restartRequested string
		resetCommittedAt string
		want             bool
	}{
		{name: "flagged/outgoing-incarnation", flagged: true, sessionEpoch: "2", resetPending: "true", restartRequested: "true", want: true},
		{name: "flagged/outgoing-incarnation-after-handoff", flagged: true, sessionEpoch: "2", resetPending: "true", resetCommittedAt: committedAt, want: true},
		{name: "flagged/marker-without-restart", flagged: true, sessionEpoch: "2", resetPending: "true"},
		{name: "flagged/reset-withdrawn", flagged: true, sessionEpoch: "2"},
		{name: "flagged/reset-withdrawn-after-handoff", flagged: true, sessionEpoch: "2", resetCommittedAt: committedAt},
		{name: "flagged/started", flagged: true, sessionEpoch: "3", resetCommittedAt: committedAt},
		{name: "flagged/second-reset-at-next-epoch", flagged: true, sessionEpoch: "3", resetPending: "true", restartRequested: "true"},
		{name: "flagged/blank-marker", flagged: true, sessionEpoch: "2", resetPending: "  ", restartRequested: "true"},
		{name: "unflagged/reset-pending", sessionEpoch: "2", resetPending: "true", restartRequested: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := WaitInfo{RegisteredEpoch: "2", RegisteredResetPending: tc.flagged}
			s := Info{
				ContinuationEpoch:        tc.sessionEpoch,
				ContinuationResetPending: tc.resetPending,
				RestartRequested:         tc.restartRequested,
				ResetCommittedAt:         tc.resetCommittedAt,
			}
			if got := w.HeldForPendingReset(s); got != tc.want {
				t.Fatalf("HeldForPendingReset(flagged=%v, session epoch=%q reset_pending=%q restart_requested=%q reset_committed_at=%q) = %v, want %v",
					tc.flagged, tc.sessionEpoch, tc.resetPending, tc.restartRequested, tc.resetCommittedAt, got, tc.want)
			}
		})
	}
}

// TestHeldForPendingResetAfterAManagerStartConsumedTheReset drives the
// Manager paths through a restart handoff whose committed reset a message's
// start consumed, then a resume that dies during startup: the stale-key
// recovery relaunches the session at the new epoch and leaves the reset marker
// for the next start. No restart is in flight, so the reminder of a wait
// registered there is not held.
func TestHeldForPendingResetAfterAManagerStartConsumedTheReset(t *testing.T) {
	ctx := context.Background()
	store := openLeaseStore(t, t.TempDir())
	b := mintRow(t, store, "session_key", "k-1", "resume_flag", "--resume")
	sp := &mintSeeingProvider{Fake: runtime.NewFake(), store: store, id: b.ID}
	mgr := newTestManager(t, store, sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	by := testActor(mgr, ActorOperator)

	// An operator's fresh restart that the controller handed off: the
	// runtime is stopped and the reset committed.
	if err := mgr.RequestFreshRestart(b.ID); err != nil {
		t.Fatalf("RequestFreshRestart: %v", err)
	}
	if err := store.SetMetadataBatch(b.ID, RestartRequestPatchAt("", mintT0)); err != nil {
		t.Fatal(err)
	}
	// A message starts the session before the controller does.
	if _, err := mgr.Send(ctx, by, b.ID, "hello", "claude", runtime.Config{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if meta := rowMeta(t, store, b.ID); meta["continuation_epoch"] != "3" || meta["continuation_reset_pending"] != "" || meta[ResetCommittedAtKey] != "" {
		t.Fatalf("row after the start that consumed the reset = %v; want epoch 3, the reset and its commit time cleared", meta)
	}

	// Stopped again, its resume dies during startup: the stale-key recovery
	// relaunches it at epoch 3 and records the reset marker for the next start.
	if err := mgr.Suspend(ctx, by, b.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sp.failFirst = len(sp.seen) + 1
	if _, err := mgr.Send(ctx, by, b.ID, "hello again", "claude --resume k-1", runtime.Config{}); err != nil {
		t.Fatalf("Send after the stop: %v", err)
	}
	info := infoOf(t, store, b.ID)
	if info.ContinuationEpoch != "3" || info.ContinuationResetPending != "true" || info.RestartRequested != "" || info.ResetCommittedAt != "" {
		t.Fatalf("session after the stale-key recovery: epoch %q, reset pending %q, restart requested %q, reset_committed_at %q; want 3, true, none, none",
			info.ContinuationEpoch, info.ContinuationResetPending, info.RestartRequested, info.ResetCommittedAt)
	}
	if w := (WaitInfo{RegisteredEpoch: "3", RegisteredResetPending: true}); w.HeldForPendingReset(info) {
		t.Fatal("a wait registered after the stale-key recovery is held, though no restart is in flight to consume the reset")
	}
}

// contextCancelWaiter is the context-first CancelWaits contract. Tests reach it
// through a runtime assertion so they compile against a context-free method
// and report the missing parameter as a failure instead of a build error.
type contextCancelWaiter interface {
	CancelWaits(ctx context.Context, sessionID string, now time.Time) ([]string, bool, error)
}

func contextFirstCancelWaits(t *testing.T, s *Store) contextCancelWaiter {
	t.Helper()
	cw, ok := any(s).(contextCancelWaiter)
	if !ok {
		t.Fatal("(*Store).CancelWaits does not take a context.Context as its first parameter")
	}
	return cw
}

// cancelWaitsProbeStore counts wait lookups and records every CloseAll batch,
// and runs an optional hook after each successful call so a test can cancel
// a context at a chosen store-call boundary.
type cancelWaitsProbeStore struct {
	*beads.MemStore
	lists    int
	batches  [][]string
	metadata []map[string]string
	failAt   int // 1-based CloseAll call that fails; 0 never fails
	after    func(kind string, n int)
}

func (s *cancelWaitsProbeStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	items, err := s.MemStore.List(query)
	if s.after != nil {
		s.after("list", s.lists)
	}
	return items, err
}

func (s *cancelWaitsProbeStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	s.batches = append(s.batches, append([]string(nil), ids...))
	s.metadata = append(s.metadata, metadata)
	if s.failAt == len(s.batches) {
		return 0, errors.New("close batch failed")
	}
	n, err := s.MemStore.CloseAll(ids, metadata)
	if s.after != nil {
		s.after("closeall", len(s.batches))
	}
	return n, err
}

func (s *cancelWaitsProbeStore) batchSizes() []int {
	sizes := make([]int, 0, len(s.batches))
	for _, b := range s.batches {
		sizes = append(sizes, len(b))
	}
	return sizes
}

// seedCancelWaits creates pending waits followed by terminal (canceled) open
// waits for sessionID; nudge IDs repeat every nudgeEvery waits.
func seedCancelWaits(t *testing.T, store beads.Store, sessionID string, pending, terminal, nudgeEvery int) {
	t.Helper()
	for i := 0; i < pending+terminal; i++ {
		state := "pending"
		if i >= pending {
			state = waitStateCanceled
		}
		if _, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("wait-%d", i),
			Type:   WaitBeadType,
			Labels: []string{WaitBeadLabel, "session:" + sessionID},
			Metadata: map[string]string{
				"session_id": sessionID,
				"state":      state,
				"nudge_id":   fmt.Sprintf("nudge-%d", i%nudgeEvery),
			},
		}); err != nil {
			t.Fatalf("create wait %d: %v", i, err)
		}
	}
}

func openWaitCount(t *testing.T, store beads.Store, sessionID string) int {
	t.Helper()
	open, err := store.List(beads.ListQuery{Status: "open", Label: "session:" + sessionID})
	if err != nil {
		t.Fatalf("list open waits: %v", err)
	}
	return len(open)
}

// TestCancelWaitsBackgroundBulkCompatibility: a context that can never be
// canceled keeps one CloseAll per nonempty ID class per lookup page, for
// CancelWaits and for wake's shared cancellation helper.
func TestCancelWaitsBackgroundBulkCompatibility(t *testing.T) {
	const sessionID = "gc-session"
	// Created newest-last; lookups sort newest first, so page one is the
	// 1000 newest: all 30 terminal waits plus 970 pending. Page two holds the
	// remaining 41 pending waits.
	const pending, terminal = SessionWaitLookupLimit + 11, 30

	t.Run("CancelWaits", func(t *testing.T) {
		probe := &cancelWaitsProbeStore{MemStore: beads.NewMemStore()}
		seedCancelWaits(t, probe, sessionID, pending, terminal, 7)
		probe.lists = 0

		nudgeIDs, capped, err := contextFirstCancelWaits(t, waitStoreOver(probe)).CancelWaits(context.Background(), sessionID, time.Now().UTC())
		if err != nil {
			t.Fatalf("CancelWaits: %v", err)
		}
		if !capped {
			t.Fatal("capped = false, want true")
		}
		if got, want := probe.batchSizes(), []int{970, 30, 41}; !reflect.DeepEqual(got, want) {
			t.Fatalf("CloseAll batch sizes = %v, want %v (one per nonempty class per page)", got, want)
		}
		if probe.lists != 2 {
			t.Fatalf("wait lookups = %d, want 2", probe.lists)
		}
		if len(nudgeIDs) != 7 {
			t.Fatalf("nudge IDs = %v, want 7 unique", nudgeIDs)
		}
		if got := openWaitCount(t, probe, sessionID); got != 0 {
			t.Fatalf("open waits = %d, want 0", got)
		}
	})

	t.Run("WakeHelper", func(t *testing.T) {
		probe := &cancelWaitsProbeStore{MemStore: beads.NewMemStore()}
		sessionBead, err := probe.Create(beads.Bead{
			Type:     BeadType,
			Labels:   []string{LabelSession},
			Metadata: map[string]string{"state": string(StateSuspended), "sleep_reason": "deps"},
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		seedCancelWaits(t, probe, sessionBead.ID, pending, terminal, 7)
		contextFirstCancelWaits(t, waitStoreOver(probe))

		if _, err := waitStoreOver(probe).wakeSessionFromBead(sessionBead, time.Now().UTC()); err != nil {
			t.Fatalf("wakeSessionFromBead: %v", err)
		}
		if got, want := probe.batchSizes(), []int{970, 30, 41}; !reflect.DeepEqual(got, want) {
			t.Fatalf("CloseAll batch sizes = %v, want %v (one per nonempty class per page)", got, want)
		}
		if got := openWaitCount(t, probe, sessionBead.ID); got != 0 {
			t.Fatalf("open waits = %d, want 0", got)
		}
	})
}

// TestCancelWaitsContextCheckpoints: under a cancellable context CancelWaits
// makes the same store calls as under a context that is never canceled (one
// CloseAll per nonempty ID class per lookup page), checks the context before
// each lookup and each of those CloseAll calls, and once cancellation is
// observed returns the nudge IDs and cap it accumulated with an error
// matching ctx.Err().
func TestCancelWaitsContextCheckpoints(t *testing.T) {
	const sessionID = "gc-session"
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	type run struct {
		probe    *cancelWaitsProbeStore
		nudgeIDs []string
		capped   bool
		err      error
	}
	// cancelAt runs CancelWaits under a cancellable context that is canceled
	// right after the given store call succeeds ("" never cancels; n == 0
	// cancels before the first call).
	cancelAt := func(t *testing.T, pending, terminal int, kind string, n int, failAt int) run {
		t.Helper()
		probe := &cancelWaitsProbeStore{MemStore: beads.NewMemStore(), failAt: failAt}
		seedCancelWaits(t, probe, sessionID, pending, terminal, 1<<30)
		probe.lists = 0
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if kind != "" && n == 0 {
			cancel()
		}
		probe.after = func(k string, i int) {
			if k == kind && i == n {
				cancel()
			}
		}
		nudgeIDs, capped, err := contextFirstCancelWaits(t, waitStoreOver(probe)).CancelWaits(ctx, sessionID, now)
		return run{probe: probe, nudgeIDs: nudgeIDs, capped: capped, err: err}
	}
	wantCanceled := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CancelWaits error = %v, want one matching context.Canceled", err)
		}
	}

	t.Run("SameCallsAsBackground", func(t *testing.T) {
		// The seed of TestCancelWaitsBackgroundBulkCompatibility: page one is
		// 970 pending plus all 30 terminal waits, page two the other 41
		// pending.
		r := cancelAt(t, SessionWaitLookupLimit+11, 30, "", 0, 0)
		if r.err != nil {
			t.Fatalf("CancelWaits: %v", r.err)
		}
		if got, want := r.probe.batchSizes(), []int{970, 30, 41}; !reflect.DeepEqual(got, want) {
			t.Fatalf("CloseAll batch sizes = %v, want %v (one per nonempty class per page)", got, want)
		}
		if r.probe.lists != 2 {
			t.Fatalf("wait lookups = %d, want 2", r.probe.lists)
		}
		for i, md := range r.probe.metadata {
			if pendingBatch := i != 1; pendingBatch != (md["state"] == waitStateCanceled) {
				t.Fatalf("batch %d metadata = %v, want canceled metadata only on pending batches", i, md)
			}
		}
		if len(r.nudgeIDs) != SessionWaitLookupLimit+41 || !r.capped {
			t.Fatalf("nudgeIDs/capped = %d/%v, want %d/true", len(r.nudgeIDs), r.capped, SessionWaitLookupLimit+41)
		}
		if got := openWaitCount(t, r.probe, sessionID); got != 0 {
			t.Fatalf("open waits = %d, want 0", got)
		}
	})

	t.Run("AlreadyCanceled", func(t *testing.T) {
		r := cancelAt(t, 5, 0, "list", 0, 0)
		wantCanceled(t, r.err)
		if r.probe.lists != 0 || len(r.probe.batches) != 0 {
			t.Fatalf("lookups/CloseAlls = %d/%d, want 0/0", r.probe.lists, len(r.probe.batches))
		}
		if got := openWaitCount(t, r.probe, sessionID); got != 5 {
			t.Fatalf("open waits = %d, want 5", got)
		}
	})

	t.Run("CanceledDuringLookup", func(t *testing.T) {
		r := cancelAt(t, 5, 0, "list", 1, 0)
		wantCanceled(t, r.err)
		if len(r.probe.batches) != 0 {
			t.Fatalf("CloseAll calls = %d, want 0 after cancellation", len(r.probe.batches))
		}
		if len(r.nudgeIDs) != 5 {
			t.Fatalf("nudgeIDs = %v, want the 5 the lookup observed", r.nudgeIDs)
		}
	})

	t.Run("CanceledBetweenClasses", func(t *testing.T) {
		r := cancelAt(t, 10, 4, "closeall", 1, 0)
		wantCanceled(t, r.err)
		if got, want := r.probe.batchSizes(), []int{10}; !reflect.DeepEqual(got, want) {
			t.Fatalf("CloseAll batch sizes = %v, want %v (no terminal batch)", got, want)
		}
		if got := openWaitCount(t, r.probe, sessionID); got != 4 {
			t.Fatalf("open waits = %d, want the 4 terminal waits left open", got)
		}
	})

	t.Run("CanceledBetweenPages", func(t *testing.T) {
		r := cancelAt(t, SessionWaitLookupLimit+3, 0, "closeall", 1, 0)
		wantCanceled(t, r.err)
		if r.probe.lists != 1 {
			t.Fatalf("wait lookups = %d, want 1 (no second page after cancellation)", r.probe.lists)
		}
		if got, want := r.probe.batchSizes(), []int{SessionWaitLookupLimit}; !reflect.DeepEqual(got, want) {
			t.Fatalf("CloseAll batch sizes = %v, want %v", got, want)
		}
		if !r.capped {
			t.Fatal("capped = false, want the first page's cap")
		}
		if len(r.nudgeIDs) != SessionWaitLookupLimit {
			t.Fatalf("nudgeIDs = %d, want %d", len(r.nudgeIDs), SessionWaitLookupLimit)
		}
		if got := openWaitCount(t, r.probe, sessionID); got != 3 {
			t.Fatalf("open waits = %d, want 3", got)
		}
		closed, err := r.probe.List(beads.ListQuery{Status: "closed", Label: "session:" + sessionID})
		if err != nil {
			t.Fatalf("list closed waits: %v", err)
		}
		if len(closed) != SessionWaitLookupLimit {
			t.Fatalf("closed waits = %d, want %d", len(closed), SessionWaitLookupLimit)
		}
		for _, w := range closed {
			if w.Metadata["state"] != waitStateCanceled || w.Metadata["canceled_at"] != now.Format(time.RFC3339) {
				t.Fatalf("closed wait %s metadata = %v, want canceled at %s", w.ID, w.Metadata, now.Format(time.RFC3339))
			}
		}
	})

	t.Run("CanceledDuringFinalCloseAllIsSuccess", func(t *testing.T) {
		r := cancelAt(t, 15, 0, "closeall", 1, 0)
		if r.err != nil {
			t.Fatalf("CancelWaits error = %v, want nil: completed work is not turned into an error", r.err)
		}
		if got := openWaitCount(t, r.probe, sessionID); got != 0 {
			t.Fatalf("open waits = %d, want 0", got)
		}
	})

	t.Run("OrdinaryStoreError", func(t *testing.T) {
		r := cancelAt(t, 20, 10, "", 0, 2)
		if r.err == nil || !strings.Contains(r.err.Error(), "close batch failed") || errors.Is(r.err, context.Canceled) {
			t.Fatalf("CancelWaits error = %v, want the store's close batch failure", r.err)
		}
		if got, want := r.probe.batchSizes(), []int{20, 10}; !reflect.DeepEqual(got, want) {
			t.Fatalf("CloseAll batch sizes = %v, want %v (stop at the failing terminal CloseAll)", got, want)
		}
		if got := openWaitCount(t, r.probe, sessionID); got != 10 {
			t.Fatalf("open waits = %d, want the 10 terminal waits left open", got)
		}
	})
}
