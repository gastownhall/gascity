package nudgequeue

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// newRecordingNudgeStore wires a RecordingStore over a fresh MemStore behind the
// nudges class tag, returning both the front door and the recorder so a test can
// assert byte-identical bead writes.
func newRecordingNudgeStore(t *testing.T) (*Store, *beadstest.RecordingStore) {
	t.Helper()
	rec := beadstest.NewRecordingStore(beads.NewMemStore())
	return NewStore(beads.NudgesStore{Store: rec}), rec
}

func sampleNudgeItem() Item {
	return Item{
		ID:                "nudge-xyz",
		Agent:             "polecat-3",
		SessionID:         "sess-1",
		ContinuationEpoch: "epoch-7",
		Source:            "controller",
		Message:           "wake up",
		Reference:         &Reference{Kind: "bead", ID: "gc-99"},
		DeliverAfter:      time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC),
		ExpiresAt:         time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC),
	}
}

// TestSaveEmitsByteIdenticalCreate proves Save creates the shadow bead with the
// exact metadata map, labels, title, and type the prior raw ensureQueuedNudgeBead
// helper produced. The literal expected map IS the byte-identical contract.
func TestSaveEmitsByteIdenticalCreate(t *testing.T) {
	st, rec := newRecordingNudgeStore(t)
	item := sampleNudgeItem()

	beadID, created, err := st.Save(item)
	if err != nil || !created || beadID == "" {
		t.Fatalf("Save = (%q,%v,%v), want (non-empty,true,nil)", beadID, created, err)
	}

	creates := rec.CallsForOp("Create")
	if len(creates) != 1 {
		t.Fatalf("Create calls = %d, want 1", len(creates))
	}
	got := creates[0].Bead
	wantMeta := beads.StringMap{
		"nudge_id":           "nudge-xyz",
		"agent":              "polecat-3",
		"session_id":         "sess-1",
		"continuation_epoch": "epoch-7",
		"state":              "queued",
		"source":             "controller",
		"message":            "wake up",
		"deliver_after":      "2026-06-02T09:00:00Z",
		"expires_at":         "2026-06-02T10:00:00Z",
		"reference_json":     `{"kind":"bead","id":"gc-99"}`,
		"last_attempt_at":    "",
		"last_error":         "",
		"terminal_reason":    "",
		"commit_boundary":    "",
		"terminal_at":        "",
	}
	if !reflect.DeepEqual(got.Metadata, wantMeta) {
		t.Errorf("metadata mismatch:\n got=%#v\nwant=%#v", got.Metadata, wantMeta)
	}
	wantLabels := []string{"gc:nudge", "agent:polecat-3", "nudge:nudge-xyz", "source:controller"}
	if !reflect.DeepEqual(got.Labels, wantLabels) {
		t.Errorf("labels = %#v, want %#v", got.Labels, wantLabels)
	}
	if got.Title != "nudge:nudge-xyz" || got.Type != "chore" {
		t.Errorf("title/type = (%q,%q), want (nudge:nudge-xyz, chore)", got.Title, got.Type)
	}
}

// TestSaveIdempotentNoSecondCreate proves Save does not re-create when a shadow
// bead already exists for the nudge id (existence-gate parity with the raw op).
func TestSaveIdempotentNoSecondCreate(t *testing.T) {
	st, rec := newRecordingNudgeStore(t)
	item := sampleNudgeItem()
	first, created, err := st.Save(item)
	if err != nil || !created {
		t.Fatalf("first Save = (%q,%v,%v)", first, created, err)
	}
	second, created2, err := st.Save(item)
	if err != nil {
		t.Fatalf("second Save err = %v", err)
	}
	if created2 {
		t.Errorf("second Save created a duplicate, want created=false")
	}
	if second != first {
		t.Errorf("second Save id = %q, want existing %q", second, first)
	}
	if n := len(rec.CallsForOp("Create")); n != 1 {
		t.Errorf("Create calls = %d, want 1 (no duplicate)", n)
	}
}

// TestTerminalizeEmitsByteIdenticalWrites proves Terminalize stamps the exact
// update map (including the canonical close_reason floor) and then closes the
// bead — the byte-identical contract for the prior markQueuedNudgeTerminal.
func TestTerminalizeEmitsByteIdenticalWrites(t *testing.T) {
	st, rec := newRecordingNudgeStore(t)
	item := sampleNudgeItem()
	beadID, _, err := st.Save(item)
	if err != nil {
		t.Fatalf("Save err = %v", err)
	}
	item.BeadID = beadID
	rec.Reset()

	now := time.Date(2026, 6, 2, 9, 30, 0, 0, time.UTC)
	if err := st.Terminalize(item, "failed", "boom", "post-commit", now); err != nil {
		t.Fatalf("Terminalize err = %v", err)
	}

	batches := rec.CallsForOp("SetMetadataBatch")
	if len(batches) != 1 {
		t.Fatalf("SetMetadataBatch calls = %d, want 1", len(batches))
	}
	wantUpdate := map[string]string{
		"state":           "failed",
		"last_attempt_at": "",
		"last_error":      "",
		"terminal_reason": "boom",
		"commit_boundary": "post-commit",
		"terminal_at":     "2026-06-02T09:30:00Z",
		"close_reason":    "nudge failed: queue terminalization rejected delivery",
	}
	if !reflect.DeepEqual(batches[0].Metadata, wantUpdate) {
		t.Errorf("update map mismatch:\n got=%#v\nwant=%#v", batches[0].Metadata, wantUpdate)
	}
	if batches[0].ID != beadID {
		t.Errorf("SetMetadataBatch id = %q, want %q", batches[0].ID, beadID)
	}
	closes := rec.CallsForOp("Close")
	if len(closes) != 1 || closes[0].ID != beadID {
		t.Errorf("Close calls = %+v, want one close of %q", closes, beadID)
	}
}

// TestTerminalizeFindsBeadWhenBeadIDEmpty proves the BeadID-then-find fallback:
// with no BeadID on the item, Terminalize resolves the shadow by label and
// terminalizes it.
func TestTerminalizeFindsBeadWhenBeadIDEmpty(t *testing.T) {
	st, rec := newRecordingNudgeStore(t)
	item := sampleNudgeItem()
	beadID, _, err := st.Save(item)
	if err != nil {
		t.Fatalf("Save err = %v", err)
	}
	rec.Reset()

	// item carries no BeadID — force the find fallback.
	item.BeadID = ""
	if err := st.Terminalize(item, "superseded", "superseded", "", time.Now().UTC()); err != nil {
		t.Fatalf("Terminalize err = %v", err)
	}
	batches := rec.CallsForOp("SetMetadataBatch")
	if len(batches) != 1 || batches[0].ID != beadID {
		t.Fatalf("expected one SetMetadataBatch on %q, got %+v", beadID, batches)
	}
	if batches[0].Metadata["close_reason"] != "nudge superseded by newer queued entry" {
		t.Errorf("close_reason = %q, want canonical superseded reason", batches[0].Metadata["close_reason"])
	}
}

// TestTerminalizeMissingBeadIsNoOp proves terminalizing a nudge with no shadow
// bead is a tolerated no-op (no writes), matching the raw op's missing-bead path.
func TestTerminalizeMissingBeadIsNoOp(t *testing.T) {
	st, rec := newRecordingNudgeStore(t)
	item := Item{ID: "ghost", Agent: "a", Source: "s"}
	if err := st.Terminalize(item, "failed", "x", "", time.Now().UTC()); err != nil {
		t.Fatalf("Terminalize err = %v", err)
	}
	if n := len(rec.Calls()); n != 0 {
		t.Errorf("recorded %d mutating calls, want 0 for a missing nudge", n)
	}
}

// TestRollbackEnqueueEmitsByteIdenticalWrites proves RollbackEnqueue stamps the
// canonical rollback close_reason then closes — the byte-identical contract for
// the prior inline rollback in enqueueQueuedNudgeWithStore.
func TestRollbackEnqueueEmitsByteIdenticalWrites(t *testing.T) {
	st, rec := newRecordingNudgeStore(t)
	item := sampleNudgeItem()
	beadID, _, err := st.Save(item)
	if err != nil {
		t.Fatalf("Save err = %v", err)
	}
	rec.Reset()

	if err := st.RollbackEnqueue(beadID); err != nil {
		t.Fatalf("RollbackEnqueue err = %v", err)
	}
	sets := rec.CallsForOp("SetMetadata")
	if len(sets) != 1 || sets[0].ID != beadID || sets[0].Key != "close_reason" ||
		sets[0].Value != EnqueueRollbackCloseReason {
		t.Errorf("SetMetadata = %+v, want close_reason=%q on %q", sets, EnqueueRollbackCloseReason, beadID)
	}
	closes := rec.CallsForOp("Close")
	if len(closes) != 1 || closes[0].ID != beadID {
		t.Errorf("Close = %+v, want one close of %q", closes, beadID)
	}
}

// TestSweepStaleEmitsByteIdenticalWrites proves SweepStale stamps the exact
// five-key gc-swept terminal map and then closes the bead — the byte-identical
// contract for the prior inline stamp+close block in cmd/gc/nudge_mail_sweep.go.
func TestSweepStaleEmitsByteIdenticalWrites(t *testing.T) {
	st, rec := newRecordingNudgeStore(t)
	beadID, _, err := st.Save(sampleNudgeItem())
	if err != nil {
		t.Fatalf("Save err = %v", err)
	}
	rec.Reset()

	now := time.Date(2026, 6, 2, 9, 30, 0, 0, time.UTC)
	const closeReason = "nudge gc-swept: stale nudge bead past gc retention window"
	if err := st.SweepStale(beadID, closeReason, now); err != nil {
		t.Fatalf("SweepStale err = %v", err)
	}

	batches := rec.CallsForOp("SetMetadataBatch")
	if len(batches) != 1 {
		t.Fatalf("SetMetadataBatch calls = %d, want 1", len(batches))
	}
	wantUpdate := map[string]string{
		"state":           "gc-swept",
		"terminal_reason": "gc-swept-stale",
		"commit_boundary": "gc-swept",
		"terminal_at":     "2026-06-02T09:30:00Z",
		"close_reason":    closeReason,
	}
	if !reflect.DeepEqual(batches[0].Metadata, wantUpdate) {
		t.Errorf("update map mismatch:\n got=%#v\nwant=%#v", batches[0].Metadata, wantUpdate)
	}
	if batches[0].ID != beadID {
		t.Errorf("SetMetadataBatch id = %q, want %q", batches[0].ID, beadID)
	}
	closes := rec.CallsForOp("Close")
	if len(closes) != 1 || closes[0].ID != beadID {
		t.Errorf("Close calls = %+v, want one close of %q", closes, beadID)
	}
}

// failingSetMetadataBatchStore wraps a beads.Store but fails every
// SetMetadataBatch, so a test can prove SweepStale skips Close when the metadata
// write fails.
type failingSetMetadataBatchStore struct {
	beads.Store
	err error
}

func (f failingSetMetadataBatchStore) SetMetadataBatch(string, map[string]string) error {
	return f.err
}

// TestSweepStaleSetMetadataFailureSkipsClose proves a failed SetMetadataBatch
// returns a bead-ID-bearing error and never reaches Close, preserving the sweep's
// current continue-without-close semantics.
func TestSweepStaleSetMetadataFailureSkipsClose(t *testing.T) {
	rec := beadstest.NewRecordingStore(beads.NewMemStore())
	failing := failingSetMetadataBatchStore{Store: rec, err: errors.New("batch boom")}
	st := NewStore(beads.NudgesStore{Store: failing})

	err := st.SweepStale("nb-fail", "nudge gc-swept: stale nudge bead past gc retention window", time.Now().UTC())
	if err == nil {
		t.Fatalf("SweepStale err = nil, want non-nil on SetMetadataBatch failure")
	}
	if !strings.Contains(err.Error(), "nb-fail") || !strings.Contains(err.Error(), "set metadata") {
		t.Errorf("err = %q, want it to contain the bead id and \"set metadata\"", err)
	}
	if n := len(rec.CallsForOp("Close")); n != 0 {
		t.Errorf("Close calls = %d, want 0 (SetMetadataBatch failure must skip Close)", n)
	}
}

// TestSweepStaleNilStoreIsNoOp pins the nil-safety contract shared by every Store
// method: a nil *Store and a Store over a nil embedded store both no-op.
func TestSweepStaleNilStoreIsNoOp(t *testing.T) {
	const reason = "nudge gc-swept: stale nudge bead past gc retention window"
	now := time.Now().UTC()

	var s *Store // nil receiver: shadow bead store unavailable
	if err := s.SweepStale("gc-1", reason, now); err != nil {
		t.Errorf("SweepStale on nil store = %v, want nil no-op", err)
	}
	empty := NewStore(beads.NudgesStore{}) // Store over a nil embedded store
	if err := empty.SweepStale("gc-1", reason, now); err != nil {
		t.Errorf("SweepStale on nil embedded store = %v, want nil no-op", err)
	}
}

// listCaptureNudgeStore records every List query and returns a fixed candidate
// set, so a test can pin the exact query shape StaleShadowsBefore emits and drive
// the decode/live-exclusion logic against a controlled bead set. Non-List ops
// delegate to the embedded store.
type listCaptureNudgeStore struct {
	beads.Store
	queries []beads.ListQuery
	result  []beads.Bead
	err     error
}

func (s *listCaptureNudgeStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, q)
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

// TestNudgeShadowOpenFromBeadStatus proves NudgeShadow.Open is bead-authoritative:
// true only for a bead whose Status is "open", replacing the caller's b.Status
// crack.
func TestNudgeShadowOpenFromBeadStatus(t *testing.T) {
	if got := decodeNudgeItem(beads.Bead{Status: "open"}); !got.Open {
		t.Error("Open = false for an open bead, want true")
	}
	if got := decodeNudgeItem(beads.Bead{Status: "closed"}); got.Open {
		t.Error("Open = true for a closed bead, want false")
	}
	if got := decodeNudgeItem(beads.Bead{}); got.Open {
		t.Error("Open = true for a bead with empty status, want false")
	}
}

// TestNudgeShadowCloseReasonDecode proves the shadow codec reads back the
// bead-lifecycle close_reason (the reason forwarded to `bd close --reason`), so
// callers assert it off the typed view instead of cracking bead metadata.
func TestNudgeShadowCloseReasonDecode(t *testing.T) {
	const reason = "nudge rollback: enqueue transaction failed"
	got := decodeNudgeItem(beads.Bead{
		Status:   "closed",
		Metadata: map[string]string{"close_reason": reason},
	})
	if got.CloseReason != reason {
		t.Errorf("CloseReason = %q, want %q", got.CloseReason, reason)
	}
	if empty := decodeNudgeItem(beads.Bead{}); empty.CloseReason != "" {
		t.Errorf("CloseReason = %q, want empty when close_reason absent", empty.CloseReason)
	}
}

// TestStaleShadowsBeforeQueryShape pins the byte-identical retention-sweep query
// StaleShadowsBefore emits — the same gc:nudge label, CreatedBefore cutoff,
// oldest-first sort, and both-tier read the prior StaleCandidatesBefore used.
// The negative limit is normalized to 0 (unbounded) before it reaches the store.
func TestStaleShadowsBeforeQueryShape(t *testing.T) {
	capture := &listCaptureNudgeStore{Store: beads.NewMemStore()}
	st := NewStore(beads.NudgesStore{Store: capture})
	cutoff := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)

	if _, err := st.StaleShadowsBefore(cutoff, -1, nil); err != nil {
		t.Fatalf("StaleShadowsBefore err = %v", err)
	}
	if len(capture.queries) != 1 {
		t.Fatalf("List calls = %d, want 1", len(capture.queries))
	}
	want := beads.ListQuery{
		Label:         nudgeBeadLabel,
		CreatedBefore: cutoff,
		Limit:         0,
		Sort:          beads.SortCreatedAsc,
		TierMode:      beads.TierBoth,
	}
	if !reflect.DeepEqual(capture.queries[0], want) {
		t.Errorf("query = %#v, want %#v", capture.queries[0], want)
	}
}

// TestStaleShadowsBeforeDecodesAndExcludesLive proves StaleShadowsBefore returns
// typed shadows in candidate order (oldest-first), carries the open/terminal
// status via NudgeShadow.Open, and drops any candidate whose durable nudge id is
// in the live flock-queue exclusion set — the behavior the cmd/gc sweep loop used
// to inline as DecodeShadow + b.Status + the liveIDs check.
func TestStaleShadowsBeforeDecodesAndExcludesLive(t *testing.T) {
	capture := &listCaptureNudgeStore{
		Store: beads.NewMemStore(),
		result: []beads.Bead{
			{ID: "nb-a", Status: "open", Metadata: map[string]string{"nudge_id": "a"}},
			{ID: "nb-b", Status: "closed", Metadata: map[string]string{"nudge_id": "b"}},
			{ID: "nb-c", Status: "open", Metadata: map[string]string{"nudge_id": "c"}},
		},
	}
	st := NewStore(beads.NudgesStore{Store: capture})

	shadows, err := st.StaleShadowsBefore(time.Now(), 10, map[string]bool{"c": true})
	if err != nil {
		t.Fatalf("StaleShadowsBefore err = %v", err)
	}
	if len(shadows) != 2 {
		t.Fatalf("shadows = %d, want 2 (live nudge c excluded)", len(shadows))
	}
	if shadows[0].BeadID != "nb-a" || shadows[0].ID != "a" || !shadows[0].Open {
		t.Errorf("shadows[0] = %+v, want open nb-a/a", shadows[0])
	}
	if shadows[1].BeadID != "nb-b" || shadows[1].ID != "b" || shadows[1].Open {
		t.Errorf("shadows[1] = %+v, want closed nb-b/b", shadows[1])
	}
}

// TestStaleShadowsBeforePropagatesListError proves a store List failure surfaces
// to the caller (which wraps it as a fatal listing error).
func TestStaleShadowsBeforePropagatesListError(t *testing.T) {
	capture := &listCaptureNudgeStore{Store: beads.NewMemStore(), err: errors.New("list boom")}
	st := NewStore(beads.NudgesStore{Store: capture})
	if _, err := st.StaleShadowsBefore(time.Now(), 0, nil); err == nil {
		t.Fatal("StaleShadowsBefore err = nil, want the store List error propagated")
	}
}

// TestStaleShadowsBeforeNilStoreIsNoOp pins the shared nil-safety contract for the
// new read: a nil *Store and a Store over a nil embedded store both return no
// shadows without touching a store.
func TestStaleShadowsBeforeNilStoreIsNoOp(t *testing.T) {
	var s *Store // nil receiver: shadow bead store unavailable
	if shadows, err := s.StaleShadowsBefore(time.Now(), 0, nil); err != nil || shadows != nil {
		t.Errorf("StaleShadowsBefore on nil store = (%v,%v), want (nil,nil)", shadows, err)
	}
	empty := NewStore(beads.NudgesStore{}) // Store over a nil embedded store
	if shadows, err := empty.StaleShadowsBefore(time.Now(), 0, nil); err != nil || shadows != nil {
		t.Errorf("StaleShadowsBefore on nil embedded store = (%v,%v), want (nil,nil)", shadows, err)
	}
}

// TestFindReturnsTypedShadow proves Find returns a decoded NudgeShadow (open
// bead) and FindIncludingTerminal reads the controller-stamped terminal fields
// off a closed bead.
func TestFindReturnsTypedShadow(t *testing.T) {
	st, _ := newRecordingNudgeStore(t)
	item := sampleNudgeItem()
	beadID, _, err := st.Save(item)
	if err != nil {
		t.Fatalf("Save err = %v", err)
	}

	open, ok, err := st.Find(item.ID)
	if err != nil || !ok {
		t.Fatalf("Find = (%+v,%v,%v)", open, ok, err)
	}
	if open.State != "queued" || open.BeadID != beadID || open.ID != item.ID {
		t.Errorf("open shadow = %+v, want queued/%s/%s", open, beadID, item.ID)
	}
	if open.Reference == nil || open.Reference.ID != "gc-99" {
		t.Errorf("reference not decoded: %+v", open.Reference)
	}

	// After terminalization, Find (open-only) must miss; FindIncludingTerminal hits.
	item.BeadID = beadID
	if err := st.Terminalize(item, "injected", "delivered", "cb", time.Now().UTC()); err != nil {
		t.Fatalf("Terminalize err = %v", err)
	}
	if _, ok, err := st.Find(item.ID); err != nil || ok {
		t.Errorf("Find after terminal = (%v,%v), want (false,nil)", ok, err)
	}
	term, ok, err := st.FindIncludingTerminal(item.ID)
	if err != nil || !ok {
		t.Fatalf("FindIncludingTerminal = (%+v,%v,%v)", term, ok, err)
	}
	if term.State != "injected" || term.TerminalReason != "delivered" || term.CommitBoundary != "cb" {
		t.Errorf("terminal shadow = %+v, want injected/delivered/cb", term)
	}
}

// nudgeShadowBeadFromItem builds a nudge shadow bead the way
// cmd/gc/nudge_beads.go ensureQueuedNudgeBead does, so the decoder test asserts
// a true round-trip against the real write codec's output shape.
func nudgeShadowBeadFromItem(item Item, state, terminalReason, commitBoundary string) beads.Bead {
	refJSON := ""
	if item.Reference != nil {
		data, _ := json.Marshal(item.Reference)
		refJSON = string(data)
	}
	return beads.Bead{
		ID:    "nb-1",
		Title: "nudge:" + item.ID,
		Type:  nudgeBeadType,
		Labels: []string{
			nudgeBeadLabel,
			"agent:" + item.Agent,
			"nudge:" + item.ID,
			"source:" + item.Source,
		},
		Metadata: map[string]string{
			"nudge_id":        item.ID,
			"agent":           item.Agent,
			"session_id":      item.SessionID,
			"state":           state,
			"source":          item.Source,
			"message":         item.Message,
			"deliver_after":   item.DeliverAfter.UTC().Format(time.RFC3339),
			"expires_at":      item.ExpiresAt.UTC().Format(time.RFC3339),
			"reference_json":  refJSON,
			"terminal_reason": terminalReason,
			"commit_boundary": commitBoundary,
		},
	}
}

// TestDecodeNudgeItemRoundTrip proves the net-new decoder reads back every
// controller-stamped field — including the previously write-only reference_json
// — that the write codec stamps.
func TestDecodeNudgeItemRoundTrip(t *testing.T) {
	deliver := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	expires := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)
	item := Item{
		ID:           "nudge-abc",
		Agent:        "polecat-7",
		SessionID:    "s-1",
		Source:       "controller",
		Message:      "wake up",
		Reference:    &Reference{Kind: "bead", ID: "gc-42"},
		DeliverAfter: deliver,
		ExpiresAt:    expires,
	}
	b := nudgeShadowBeadFromItem(item, "injected", "delivered", "post-commit")

	got := decodeNudgeItem(b)

	if got.ID != "nudge-abc" || got.BeadID != "nb-1" {
		t.Errorf("id/beadid = (%q,%q), want (nudge-abc, nb-1)", got.ID, got.BeadID)
	}
	if got.State != "injected" || got.TerminalReason != "delivered" || got.CommitBoundary != "post-commit" {
		t.Errorf("terminal fields = (%q,%q,%q), want (injected, delivered, post-commit)",
			got.State, got.TerminalReason, got.CommitBoundary)
	}
	if got.Reference == nil || got.Reference.Kind != "bead" || got.Reference.ID != "gc-42" {
		t.Errorf("reference = %+v, want {bead gc-42} (reference_json must finally be read back)", got.Reference)
	}
	if !got.DeliverAfter.Equal(deliver) || !got.ExpiresAt.Equal(expires) {
		t.Errorf("times = (%v,%v), want (%v,%v)", got.DeliverAfter, got.ExpiresAt, deliver, expires)
	}
	if got.Agent != "polecat-7" || got.SessionID != "s-1" || got.Source != "controller" || got.Message != "wake up" {
		t.Errorf("identity fields mismatch: %+v", got)
	}
}

// TestDecodeNudgeItemNoReference proves a missing reference_json yields a nil
// Reference (not a zero-value struct), matching the write codec which stores ""
// for a nil reference.
func TestDecodeNudgeItemNoReference(t *testing.T) {
	item := Item{ID: "n", Agent: "a", Source: "s"}
	b := nudgeShadowBeadFromItem(item, "queued", "", "")
	got := decodeNudgeItem(b)
	if got.Reference != nil {
		t.Errorf("Reference = %+v, want nil when reference_json is empty", got.Reference)
	}
	if got.State != "queued" {
		t.Errorf("state = %q, want queued", got.State)
	}
}

// TestNewStoreHoldsTypedStore proves the wrapper holds the typed nudges store
// (skeleton wiring sanity).
func TestNewStoreHoldsTypedStore(t *testing.T) {
	mem := beads.NewMemStore()
	st := NewStore(beads.NudgesStore{Store: mem})
	if st.store.Store != mem {
		t.Errorf("NewStore did not retain the embedded store")
	}
}

// TestNilStoreIsNoOp pins the nil-receiver contract: a nil *Store is the front
// door cmd/gc passes when the shadow bead store fails to open (openNudgeBeadStore
// returns a zero NudgesStore). The flock'd state.json — not the shadow bead — is
// the queue authority, so a missing shadow store must degrade every method to a
// no-op instead of panicking the command. A nil-receiver dereference here used to
// crash the expired-nudge cleanup helpers in a loop.
func TestNilStoreIsNoOp(t *testing.T) {
	var s *Store // nil receiver: shadow bead store unavailable
	item := sampleNudgeItem()
	item.BeadID = "gc-1"
	now := time.Now().UTC()

	if err := s.Terminalize(item, "expired", "expired", "", now); err != nil {
		t.Errorf("Terminalize on nil store = %v, want nil no-op", err)
	}
	if beadID, created, err := s.Save(item); err != nil || created || beadID != "" {
		t.Errorf("Save on nil store = (%q,%v,%v), want (\"\",false,nil)", beadID, created, err)
	}
	if err := s.RollbackEnqueue("gc-1"); err != nil {
		t.Errorf("RollbackEnqueue on nil store = %v, want nil no-op", err)
	}
	if shadow, ok, err := s.Find(item.ID); err != nil || ok {
		t.Errorf("Find on nil store = (%+v,%v,%v), want (zero,false,nil)", shadow, ok, err)
	}
	if shadow, ok, err := s.FindIncludingTerminal(item.ID); err != nil || ok {
		t.Errorf("FindIncludingTerminal on nil store = (%+v,%v,%v), want (zero,false,nil)", shadow, ok, err)
	}
}
