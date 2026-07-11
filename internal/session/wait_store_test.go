package session

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// waitBeadFixture builds a durable wait bead carrying the canonical type and
// labels so IsWaitBead recognizes it.
func waitBeadFixture(id, status, sessionID string, meta map[string]string) beads.Bead {
	m := map[string]string{"session_id": sessionID}
	for k, v := range meta {
		m[k] = v
	}
	return beads.Bead{
		ID:        id,
		Type:      WaitBeadType,
		Status:    status,
		Title:     m["__title"],
		Labels:    []string{WaitBeadLabel, "session:" + sessionID},
		Metadata:  m,
		CreatedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	}
}

// waitStoreOver wraps a raw store as the session front door for wait tests that
// exercise the typed methods directly.
func waitStoreOver(store beads.Store) *Store {
	return NewStore(beads.SessionStore{Store: store})
}

// recordingWaitStore seeds beads verbatim into a recording-fake store and
// returns the front door plus recorder for op-stream equivalence assertions.
func recordingWaitStore(t *testing.T, seed ...beads.Bead) (*Store, *beadstest.RecordingStore) {
	t.Helper()
	mem := beads.NewMemStoreFrom(len(seed)+1, seed, nil)
	rec := beadstest.NewRecordingStore(mem)
	return NewStore(beads.SessionStore{Store: rec}), rec
}

var waitStoreNow = time.Date(2026, 3, 2, 4, 5, 6, 0, time.UTC)

// --- terminal-write intents: byte-identical SetMetadataBatch+Close pairs ---

func TestCancelWait_EmitsCanceledBatchThenClose(t *testing.T) {
	b := waitBeadFixture("w-1", "open", "gc-session", map[string]string{"state": "ready"})
	s, rec := recordingWaitStore(t, b)

	if err := s.CancelWait("w-1", waitStoreNow, ""); err != nil {
		t.Fatalf("CancelWait: %v", err)
	}
	assertBatchThenClose(t, rec, "w-1", map[string]string{
		"state":       "canceled",
		"canceled_at": waitStoreNow.UTC().Format(time.RFC3339),
	})
}

func TestCancelWait_WithLastErrorAddsKey(t *testing.T) {
	b := waitBeadFixture("w-1", "open", "gc-session", map[string]string{"state": "ready"})
	s, rec := recordingWaitStore(t, b)

	if err := s.CancelWait("w-1", waitStoreNow, "continuation-stale"); err != nil {
		t.Fatalf("CancelWait: %v", err)
	}
	assertBatchThenClose(t, rec, "w-1", map[string]string{
		"state":       "canceled",
		"canceled_at": waitStoreNow.UTC().Format(time.RFC3339),
		"last_error":  "continuation-stale",
	})
}

func TestExpireWait_EmitsExpiredBatchThenClose(t *testing.T) {
	b := waitBeadFixture("w-1", "open", "gc-session", map[string]string{"state": "pending"})
	s, rec := recordingWaitStore(t, b)

	if err := s.ExpireWait("w-1", waitStoreNow); err != nil {
		t.Fatalf("ExpireWait: %v", err)
	}
	assertBatchThenClose(t, rec, "w-1", map[string]string{
		"state":      "expired",
		"expired_at": waitStoreNow.UTC().Format(time.RFC3339),
	})
}

func TestFailWait_EmitsFailedBatchThenClose(t *testing.T) {
	b := waitBeadFixture("w-1", "open", "gc-session", map[string]string{"state": "pending"})
	s, rec := recordingWaitStore(t, b)

	if err := s.FailWait("w-1", waitStoreNow, "dependency gc-9: bead not found"); err != nil {
		t.Fatalf("FailWait: %v", err)
	}
	assertBatchThenClose(t, rec, "w-1", map[string]string{
		"state":      "failed",
		"failed_at":  waitStoreNow.UTC().Format(time.RFC3339),
		"last_error": "dependency gc-9: bead not found",
	})
}

func TestCloseWaitFromNudge_EmitsClosedBatchThenClose(t *testing.T) {
	b := waitBeadFixture("w-1", "open", "gc-session", map[string]string{"state": "ready"})
	s, rec := recordingWaitStore(t, b)

	if err := s.CloseWaitFromNudge("w-1", waitStoreNow, "wait-nudge", "commit-abc"); err != nil {
		t.Fatalf("CloseWaitFromNudge: %v", err)
	}
	assertBatchThenClose(t, rec, "w-1", map[string]string{
		"state":           "closed",
		"closed_at":       waitStoreNow.UTC().Format(time.RFC3339),
		"nudge_id":        "wait-nudge",
		"commit_boundary": "commit-abc",
	})
}

func TestFailWaitFromNudge_EmitsFailedBatchThenClose(t *testing.T) {
	b := waitBeadFixture("w-1", "open", "gc-session", map[string]string{"state": "ready"})
	s, rec := recordingWaitStore(t, b)

	if err := s.FailWaitFromNudge("w-1", waitStoreNow, "wait-nudge", "nudge expired", "commit-abc"); err != nil {
		t.Fatalf("FailWaitFromNudge: %v", err)
	}
	assertBatchThenClose(t, rec, "w-1", map[string]string{
		"state":           "failed",
		"failed_at":       waitStoreNow.UTC().Format(time.RFC3339),
		"nudge_id":        "wait-nudge",
		"last_error":      "nudge expired",
		"commit_boundary": "commit-abc",
	})
}

// --- ready-write intents: SetMetadataBatch only (no Close) ---

func TestMarkWaitReady_EmitsReadyBatchNoClose(t *testing.T) {
	b := waitBeadFixture("w-1", "open", "gc-session", map[string]string{"state": "pending"})
	s, rec := recordingWaitStore(t, b)

	if err := s.MarkWaitReady("w-1", waitStoreNow); err != nil {
		t.Fatalf("MarkWaitReady: %v", err)
	}
	if ops := opsOf(rec.Calls()); !reflect.DeepEqual(ops, []string{"SetMetadataBatch"}) {
		t.Fatalf("MarkWaitReady ops = %v, want [SetMetadataBatch]", ops)
	}
	want := map[string]string{"state": "ready", "ready_at": waitStoreNow.UTC().Format(time.RFC3339)}
	if got := rec.CallsForOp("SetMetadataBatch")[0].Metadata; !reflect.DeepEqual(got, want) {
		t.Fatalf("MarkWaitReady batch = %#v, want %#v", got, want)
	}
}

func TestMarkWaitReadyForRedelivery_WithoutNextAttempt(t *testing.T) {
	b := waitBeadFixture("w-1", "open", "gc-session", map[string]string{"state": "ready"})
	s, rec := recordingWaitStore(t, b)

	if err := s.MarkWaitReadyForRedelivery("w-1", "", waitStoreNow); err != nil {
		t.Fatalf("MarkWaitReadyForRedelivery: %v", err)
	}
	if ops := opsOf(rec.Calls()); !reflect.DeepEqual(ops, []string{"SetMetadataBatch"}) {
		t.Fatalf("ops = %v, want [SetMetadataBatch]", ops)
	}
	want := map[string]string{"state": "ready", "ready_at": waitStoreNow.UTC().Format(time.RFC3339)}
	if got := rec.CallsForOp("SetMetadataBatch")[0].Metadata; !reflect.DeepEqual(got, want) {
		t.Fatalf("batch = %#v, want %#v", got, want)
	}
}

func TestMarkWaitReadyForRedelivery_WithNextAttemptClearsTerminalKeys(t *testing.T) {
	b := waitBeadFixture("w-1", "open", "gc-session", map[string]string{"state": "failed"})
	s, rec := recordingWaitStore(t, b)

	if err := s.MarkWaitReadyForRedelivery("w-1", "3", waitStoreNow); err != nil {
		t.Fatalf("MarkWaitReadyForRedelivery: %v", err)
	}
	want := map[string]string{
		"state":            "ready",
		"ready_at":         waitStoreNow.UTC().Format(time.RFC3339),
		"delivery_attempt": "3",
		"nudge_id":         "",
		"commit_boundary":  "",
		"last_error":       "",
		"closed_at":        "",
		"failed_at":        "",
		"expired_at":       "",
		"canceled_at":      "",
	}
	if got := rec.CallsForOp("SetMetadataBatch")[0].Metadata; !reflect.DeepEqual(got, want) {
		t.Fatalf("batch = %#v, want %#v", got, want)
	}
}

func TestSetWaitNudgeID_EmitsSingleKeySetMetadata(t *testing.T) {
	b := waitBeadFixture("w-1", "open", "gc-session", map[string]string{"state": "ready"})
	s, rec := recordingWaitStore(t, b)

	if err := s.SetWaitNudgeID("w-1", "wait-w-1-0-1"); err != nil {
		t.Fatalf("SetWaitNudgeID: %v", err)
	}
	calls := rec.CallsForOp("SetMetadata")
	if len(rec.Calls()) != 1 || len(calls) != 1 {
		t.Fatalf("ops = %v, want single SetMetadata", opsOf(rec.Calls()))
	}
	if calls[0].ID != "w-1" || calls[0].Key != "nudge_id" || calls[0].Value != "wait-w-1-0-1" {
		t.Fatalf("SetMetadata = %+v, want w-1/nudge_id/wait-w-1-0-1", calls[0])
	}
}

// --- CreateWait: byte-identical meta map / labels / title ---

func TestCreateWait_ProducesLiteralBeadShape(t *testing.T) {
	sess := sessionBeadFixture("gc-session", "open", map[string]string{
		"__title":            "worker",
		"session_name":       "worker-1",
		"continuation_epoch": "5",
	})
	s, rec := recordingWaitStore(t, sess)

	got, err := s.CreateWait(WaitSpec{
		SessionID:        "gc-session",
		Kind:             "deps",
		DepIDs:           []string{"gc-1", "gc-2"},
		DepMode:          "any",
		Note:             "Continue after review.",
		CreatedBySession: "gc-origin",
		Now:              waitStoreNow,
	})
	if err != nil {
		t.Fatalf("CreateWait: %v", err)
	}
	creates := rec.CallsForOp("Create")
	if len(creates) != 1 {
		t.Fatalf("want 1 Create, got %d", len(creates))
	}
	created := creates[0].Bead
	if created.Title != "wait:worker" {
		t.Errorf("title = %q, want wait:worker", created.Title)
	}
	if created.Type != WaitBeadType {
		t.Errorf("type = %q, want %q", created.Type, WaitBeadType)
	}
	if created.Description != "Continue after review." {
		t.Errorf("description = %q", created.Description)
	}
	wantLabels := []string{WaitBeadLabel, "session:gc-session"}
	if !reflect.DeepEqual(created.Labels, wantLabels) {
		t.Errorf("labels = %#v, want %#v", created.Labels, wantLabels)
	}
	wantMeta := map[string]string{
		"session_id":         "gc-session",
		"session_name":       "worker-1",
		"kind":               "deps",
		"state":              "pending",
		"dep_ids":            "gc-1,gc-2",
		"dep_mode":           "any",
		"registered_epoch":   "5",
		"delivery_attempt":   "1",
		"created_by_session": "gc-origin",
		"created_at":         waitStoreNow.Format(time.RFC3339),
	}
	if !reflect.DeepEqual(map[string]string(created.Metadata), wantMeta) {
		t.Errorf("metadata = %#v, want %#v", created.Metadata, wantMeta)
	}
	if got.ID == "" || got.State != "pending" || got.SessionID != "gc-session" {
		t.Errorf("returned WaitInfo = %#v", got)
	}
}

// --- RetryClosedWait: ported oracles from cmd/gc TestRetryClosedWait_* ---

func TestRetryClosedWait_CreatesReplacement(t *testing.T) {
	sess := sessionBeadFixture("gc-session", "open", map[string]string{
		"session_name":       "worker",
		"continuation_epoch": "2",
	})
	wait := waitBeadFixture("w-1", "closed", "gc-session", map[string]string{
		"__title":          "wait:worker",
		"session_name":     "worker",
		"kind":             "deps",
		"state":            "failed",
		"registered_epoch": "1",
		"delivery_attempt": "1",
	})
	wait.Title = "wait:worker"
	wait.Description = "Retry me."
	s, _ := recordingWaitStore(t, sess, wait)

	now := waitStoreNow
	retried, err := s.RetryClosedWait("w-1", "2", now)
	if err != nil {
		t.Fatalf("RetryClosedWait: %v", err)
	}
	if retried.ID == "w-1" {
		t.Fatal("RetryClosedWait reused original wait ID")
	}
	if retried.State != "ready" {
		t.Fatalf("state = %q, want ready", retried.State)
	}
	if retried.DeliveryAttempt != "2" {
		t.Fatalf("delivery_attempt = %q, want 2", retried.DeliveryAttempt)
	}
	if retried.RegisteredEpoch != "2" {
		t.Fatalf("registered_epoch = %q, want 2", retried.RegisteredEpoch)
	}
	if retried.Status == "closed" {
		t.Fatalf("status = %q, want open", retried.Status)
	}
}

func TestRetryClosedWait_FallsBackToOwnAttemptWhenBlank(t *testing.T) {
	wait := waitBeadFixture("w-1", "closed", "gc-session", map[string]string{
		"kind":             "deps",
		"state":            "failed",
		"delivery_attempt": "1",
	})
	s, rec := recordingWaitStore(t, wait)

	if _, err := s.RetryClosedWait("w-1", "", waitStoreNow); err != nil {
		t.Fatalf("RetryClosedWait: %v", err)
	}
	created := rec.CallsForOp("Create")[0].Bead
	if created.Metadata["delivery_attempt"] != "1" {
		t.Fatalf("delivery_attempt = %q, want 1 (fallback)", created.Metadata["delivery_attempt"])
	}
	if created.Metadata["retried_from_wait"] != "w-1" {
		t.Fatalf("retried_from_wait = %q, want w-1", created.Metadata["retried_from_wait"])
	}
	for _, k := range []string{"nudge_id", "last_error", "closed_at", "failed_at", "expired_at", "canceled_at"} {
		if created.Metadata[k] != "" {
			t.Fatalf("%s = %q, want cleared", k, created.Metadata[k])
		}
	}
}

func TestRetryClosedWait_DropsInternalMetadata(t *testing.T) {
	wait := waitBeadFixture("w-1", "closed", "gc-session", map[string]string{
		"session_name":       "worker",
		"kind":               "deps",
		"state":              "failed",
		"dep_ids":            "gc-1",
		"dep_mode":           "all",
		"registered_epoch":   "1",
		"delivery_attempt":   "1",
		"created_by_session": "gc-origin",
		"nudge_id":           "wait-gc-1-1-1",
		"last_error":         "boom",
		"synced_at":          "2026-03-16T10:00:00Z",
		"future_internal":    "should-not-carry",
	})
	s, rec := recordingWaitStore(t, wait)

	if _, err := s.RetryClosedWait("w-1", "2", waitStoreNow); err != nil {
		t.Fatalf("RetryClosedWait: %v", err)
	}
	meta := rec.CallsForOp("Create")[0].Bead.Metadata
	if meta["dep_ids"] != "gc-1" || meta["created_by_session"] != "gc-origin" {
		t.Fatalf("preserved keys wrong: %#v", meta)
	}
	if meta["synced_at"] != "" || meta["future_internal"] != "" {
		t.Fatalf("unknown deps keys leaked: %#v", meta)
	}
}

func TestRetryClosedWait_PreservesNonDepsMetadata(t *testing.T) {
	wait := waitBeadFixture("w-1", "closed", "gc-session", map[string]string{
		"kind":             "probe",
		"state":            "failed",
		"registered_epoch": "1",
		"delivery_attempt": "1",
		"probe_name":       "github-pr-approval",
		"probe_target":     "owner/repo#123",
	})
	s, rec := recordingWaitStore(t, wait)

	if _, err := s.RetryClosedWait("w-1", "2", waitStoreNow); err != nil {
		t.Fatalf("RetryClosedWait: %v", err)
	}
	meta := rec.CallsForOp("Create")[0].Bead.Metadata
	if meta["kind"] != "probe" || meta["probe_name"] != "github-pr-approval" || meta["probe_target"] != "owner/repo#123" {
		t.Fatalf("non-deps metadata not preserved: %#v", meta)
	}
}

// --- GetWait ---

func TestGetWait_ReturnsProjection(t *testing.T) {
	b := waitBeadFixture("w-1", "open", "gc-session", map[string]string{"state": "ready", "nudge_id": "n-1"})
	s, _ := recordingWaitStore(t, b)

	got, err := s.GetWait("w-1")
	if err != nil {
		t.Fatalf("GetWait: %v", err)
	}
	if got.ID != "w-1" || got.State != "ready" || got.NudgeID != "n-1" {
		t.Fatalf("GetWait = %#v", got)
	}
}

func TestGetWait_MissingReturnsBareNotFound(t *testing.T) {
	s, _ := recordingWaitStore(t)
	_, err := s.GetWait("missing")
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("GetWait(missing) err = %v, want wraps beads.ErrNotFound", err)
	}
	if errors.Is(err, ErrNotAWait) {
		t.Fatalf("missing bead must not report ErrNotAWait")
	}
}

func TestGetWait_NonWaitReturnsErrNotAWait(t *testing.T) {
	sess := sessionBeadFixture("gc-session", "open", nil)
	s, _ := recordingWaitStore(t, sess)
	_, err := s.GetWait("gc-session")
	if !errors.Is(err, ErrNotAWait) {
		t.Fatalf("GetWait(non-wait) err = %v, want ErrNotAWait", err)
	}
}

func TestGetWait_AcceptsLegacyWaitType(t *testing.T) {
	b := beads.Bead{
		ID:       "w-legacy",
		Type:     LegacyWaitBeadType,
		Status:   "open",
		Labels:   []string{WaitBeadLabel, "session:gc-session"},
		Metadata: map[string]string{"session_id": "gc-session", "state": "pending"},
	}
	s, _ := recordingWaitStore(t, b)
	got, err := s.GetWait("w-legacy")
	if err != nil {
		t.Fatalf("GetWait(legacy): %v", err)
	}
	if got.State != "pending" {
		t.Fatalf("legacy wait state = %q", got.State)
	}
}

// --- ListWaits ---

func TestListWaits_GlobalExcludesClosedDescending(t *testing.T) {
	older := waitBeadFixture("w-old", "open", "s-1", map[string]string{"state": "pending"})
	older.CreatedAt = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	newer := waitBeadFixture("w-new", "open", "s-2", map[string]string{"state": "ready"})
	newer.CreatedAt = time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)
	closed := waitBeadFixture("w-closed", "closed", "s-3", map[string]string{"state": "canceled"})
	s, _ := recordingWaitStore(t, older, newer, closed)

	got, err := s.ListWaits("", "")
	if err != nil {
		t.Fatalf("ListWaits: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("count = %d, want 2 (closed excluded)", len(got))
	}
	if got[0].ID != "w-new" || got[1].ID != "w-old" {
		t.Fatalf("order = %s,%s, want DESC w-new,w-old", got[0].ID, got[1].ID)
	}
}

func TestListWaits_StateFilter(t *testing.T) {
	a := waitBeadFixture("w-a", "open", "s-1", map[string]string{"state": "pending"})
	b := waitBeadFixture("w-b", "open", "s-2", map[string]string{"state": "ready"})
	s, _ := recordingWaitStore(t, a, b)

	got, err := s.ListWaits("ready", "")
	if err != nil {
		t.Fatalf("ListWaits: %v", err)
	}
	if len(got) != 1 || got[0].ID != "w-b" {
		t.Fatalf("state filter got %#v, want only w-b", got)
	}
}

func TestListWaits_PerSessionDelegatesToWaitsForSession(t *testing.T) {
	a := waitBeadFixture("w-a", "open", "s-1", map[string]string{"state": "pending"})
	b := waitBeadFixture("w-b", "open", "s-2", map[string]string{"state": "ready"})
	s, _ := recordingWaitStore(t, a, b)

	got, err := s.ListWaits("", "s-1")
	if err != nil {
		t.Fatalf("ListWaits: %v", err)
	}
	if len(got) != 1 || got[0].ID != "w-a" {
		t.Fatalf("per-session got %#v, want only w-a", got)
	}
}

func TestListWaits_GlobalReportsLookupLimitWithPartial(t *testing.T) {
	seed := make([]beads.Bead, 0, SessionWaitLookupLimit+5)
	for i := 0; i < SessionWaitLookupLimit+5; i++ {
		seed = append(seed, waitBeadFixture("w-"+padIndex(i), "open", "s-1", map[string]string{"state": "pending"}))
	}
	s, _ := recordingWaitStore(t, seed...)

	got, err := s.ListWaits("", "")
	if !beads.IsLookupLimitError(err) {
		t.Fatalf("err = %v, want LookupLimitError", err)
	}
	if len(got) != SessionWaitLookupLimit {
		t.Fatalf("partial len = %d, want %d", len(got), SessionWaitLookupLimit)
	}
}

// --- WaitNudgeIDs ---

func TestWaitNudgeIDs_Deduplicates(t *testing.T) {
	a := waitBeadFixture("w-a", "open", "s-1", map[string]string{"state": "ready", "nudge_id": "n-1"})
	b := waitBeadFixture("w-b", "open", "s-1", map[string]string{"state": "ready", "nudge_id": "n-1"})
	c := waitBeadFixture("w-c", "open", "s-1", map[string]string{"state": "ready", "nudge_id": "n-2"})
	s, _ := recordingWaitStore(t, a, b, c)

	got, err := s.WaitNudgeIDs("s-1")
	if err != nil {
		t.Fatalf("WaitNudgeIDs: %v", err)
	}
	// Dedup collapses the two n-1 references; tie order across equal created-at
	// beads is store-defined, so assert the deduped set, not the sequence.
	if len(got) != 2 {
		t.Fatalf("WaitNudgeIDs = %#v, want 2 deduped ids", got)
	}
	set := map[string]bool{}
	for _, id := range got {
		set[id] = true
	}
	if !set["n-1"] || !set["n-2"] {
		t.Fatalf("WaitNudgeIDs = %#v, want {n-1, n-2}", got)
	}
}

// --- WakeSession (fused) ---

func wakeSessionBeadFixture(id string, meta map[string]string) beads.Bead {
	return sessionBeadFixture(id, "open", meta)
}

func TestWakeSession_HappyPathBatchEqualsPackageFunc(t *testing.T) {
	meta := map[string]string{"state": "asleep", "wait_hold": "true", "sleep_intent": "wait-hold", "sleep_reason": "wait-hold"}
	// Two identical seeds so the fused method and the (still-present) package
	// func write to independent beads and we can compare the emitted batches.
	fused := wakeSessionBeadFixture("s-fused", meta)
	pkg := wakeSessionBeadFixture("s-pkg", meta)
	s, rec := recordingWaitStore(t, fused, pkg)

	res, err := s.WakeSession("s-fused", waitStoreNow, WakeOpts{})
	if err != nil {
		t.Fatalf("WakeSession: %v", err)
	}
	if res.Info.ID != "s-fused" {
		t.Fatalf("res.Info.ID = %q", res.Info.ID)
	}
	fusedBatch := rec.CallsForOp("SetMetadataBatch")
	if len(fusedBatch) != 1 {
		t.Fatalf("fused emitted %d batches, want 1", len(fusedBatch))
	}
	rec.Reset()
	if _, err := waitStoreOver(rec).wakeSessionFromBead(pkg, waitStoreNow); err != nil {
		t.Fatalf("wakeSessionFromBead: %v", err)
	}
	pkgBatch := rec.CallsForOp("SetMetadataBatch")
	if len(pkgBatch) != 1 {
		t.Fatalf("pkg emitted %d batches, want 1", len(pkgBatch))
	}
	if !reflect.DeepEqual(fusedBatch[0].Metadata, pkgBatch[0].Metadata) {
		t.Fatalf("fused batch %#v != pkg batch %#v", fusedBatch[0].Metadata, pkgBatch[0].Metadata)
	}
}

func TestWakeSession_MissingBeadReturnsBareNotFound(t *testing.T) {
	s, _ := recordingWaitStore(t)
	_, err := s.WakeSession("missing", waitStoreNow, WakeOpts{})
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("err = %v, want wraps beads.ErrNotFound", err)
	}
}

func TestWakeSession_NonSessionReturnsErrNotSessionBead(t *testing.T) {
	wait := waitBeadFixture("w-1", "open", "gc-session", map[string]string{"state": "ready"})
	s, _ := recordingWaitStore(t, wait)
	_, err := s.WakeSession("w-1", waitStoreNow, WakeOpts{})
	if !errors.Is(err, ErrNotSessionBead) {
		t.Fatalf("err = %v, want ErrNotSessionBead", err)
	}
}

func TestWakeSession_RejectClosedYieldsClosedConflict(t *testing.T) {
	b := wakeSessionBeadFixture("s-1", map[string]string{"state": "asleep"})
	b.Status = "closed"
	s, rec := recordingWaitStore(t, b)

	_, err := s.WakeSession("s-1", waitStoreNow, WakeOpts{RejectClosed: true})
	state, conflict := WakeConflictState(err)
	if !conflict || state != "closed" {
		t.Fatalf("err = %v (state=%q conflict=%v), want closed conflict", err, state, conflict)
	}
	if len(rec.CallsForOp("SetMetadataBatch")) != 0 {
		t.Fatalf("RejectClosed must not write before the conflict")
	}
}

func TestWakeSession_InfoSnapshotIsPreWake(t *testing.T) {
	b := wakeSessionBeadFixture("s-1", map[string]string{"state": "asleep", "template": "worker"})
	s, _ := recordingWaitStore(t, b)

	res, err := s.WakeSession("s-1", waitStoreNow, WakeOpts{})
	if err != nil {
		t.Fatalf("WakeSession: %v", err)
	}
	// Pre-wake MetadataState is the value on the bead before the wake batch,
	// which the package func would have written to the store but not to the
	// returned snapshot.
	if res.Info.MetadataState != "asleep" {
		t.Fatalf("res.Info.MetadataState = %q, want asleep (pre-wake)", res.Info.MetadataState)
	}
	if res.Info.Template != "worker" {
		t.Fatalf("res.Info.Template = %q, want worker", res.Info.Template)
	}
}

// padIndex renders i as a fixed-width, lexically-sortable suffix so seeded wait
// ids don't perturb the created-at ordering assertions.
func padIndex(i int) string {
	const width = 5
	digits := []byte("0000000000")
	out := make([]byte, width)
	for k := width - 1; k >= 0; k-- {
		out[k] = digits[i%10]
		i /= 10
	}
	return string(out)
}

func assertBatchThenClose(t *testing.T, rec *beadstest.RecordingStore, id string, wantBatch map[string]string) {
	t.Helper()
	if ops := opsOf(rec.Calls()); !reflect.DeepEqual(ops, []string{"SetMetadataBatch", "Close"}) {
		t.Fatalf("ops = %v, want [SetMetadataBatch Close]", ops)
	}
	batch := rec.CallsForOp("SetMetadataBatch")[0]
	if batch.ID != id {
		t.Errorf("batch target = %q, want %q", batch.ID, id)
	}
	if !reflect.DeepEqual(batch.Metadata, wantBatch) {
		t.Errorf("batch = %#v, want %#v", batch.Metadata, wantBatch)
	}
	if closeCall := rec.CallsForOp("Close")[0]; closeCall.ID != id {
		t.Errorf("close target = %q, want %q", closeCall.ID, id)
	}
}
