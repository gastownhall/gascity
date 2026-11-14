package extmsg

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// These tests pin how Append uses a pinned entry ID per (conversation,
// sequence) and a monotonic next_sequence advance.

func transcriptStateFor(t *testing.T, store beads.Store, ref ConversationRef) ConversationTranscriptStateRecord {
	t.Helper()
	items, err := store.List(beads.ListQuery{Label: transcriptStateLabel(ref)})
	if err != nil {
		t.Fatalf("list transcript state: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("transcript states = %d, want 1", len(items))
	}
	state, err := decodeTranscriptStateBead(items[0])
	if err != nil {
		t.Fatalf("decode transcript state: %v", err)
	}
	return state
}

func listVisibleTranscript(t *testing.T, svc TranscriptService, ref ConversationRef) []ConversationTranscriptRecord {
	t.Helper()
	entries, err := svc.List(context.Background(), ListTranscriptInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		Limit:        50,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return entries
}

func TestTranscriptEntryIDIsPinnedUnderTheStateBead(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	store := newPinnedIDMemStore()
	store.IDPrefix = "gm"
	svc := NewServices(store).Transcript
	ref := testConversationRef()

	first, err := appendInboundForSequenceTest(ctx, svc, ref, "msg-1", testNow())
	if err != nil {
		t.Fatalf("Append(msg-1): %v", err)
	}
	second, err := appendInboundForSequenceTest(ctx, svc, ref, "msg-2", testNow().Add(time.Minute))
	if err != nil {
		t.Fatalf("Append(msg-2): %v", err)
	}
	state := transcriptStateFor(t, store, ref)
	if !strings.HasPrefix(state.ID, "gm-") {
		t.Fatalf("state id %q is not in the store namespace gm-", state.ID)
	}
	for _, rec := range []ConversationTranscriptRecord{first, second} {
		if want := state.ID + "-" + strconv.FormatInt(rec.Sequence, 10); rec.ID != want {
			t.Fatalf("entry %s id = %q, want %q", rec.ProviderMessageID, rec.ID, want)
		}
	}
	if state.NextSequence != 3 {
		t.Fatalf("next_sequence = %d, want 3", state.NextSequence)
	}
}

// upsertingCreateStore overwrites the row at an occupied pinned id and reports
// a fresh create, the way bd before v1.3.0 ran `bd create --id`.
type upsertingCreateStore struct {
	*beads.MemStore
}

func (s *upsertingCreateStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.ID != "" {
		if _, err := s.Get(b.ID); err == nil {
			desc := b.Description
			if err := s.Update(b.ID, beads.UpdateOpts{Description: &desc, Metadata: b.Metadata}); err != nil {
				return beads.Bead{}, err
			}
			return s.Get(b.ID)
		}
	}
	return s.MemStore.Create(b)
}

func TestTranscriptAppendReadsThePinnedIDBeforeCreating(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	inner := &upsertingCreateStore{MemStore: newPinnedIDMemStore()}
	svc := NewServices(&failingSequenceBumpStore{Store: inner}).Transcript
	ref := testConversationRef()

	if _, err := appendInboundForSequenceTest(ctx, svc, ref, "msg-1", testNow()); err == nil {
		t.Fatalf("Append(msg-1) succeeded; the test store should fail the next_sequence advance")
	}
	if _, err := appendInboundForSequenceTest(ctx, svc, ref, "msg-2", testNow().Add(time.Minute)); err != nil {
		t.Fatalf("Append(msg-2): %v", err)
	}

	seqs := persistedTranscriptSequences(t, inner, ref)
	if len(seqs) != 2 {
		t.Fatalf("persisted entries = %v, want msg-1 and msg-2 (an upserting store must not have msg-1 overwritten)", seqs)
	}
	requireDistinctSequences(t, seqs)
}

func TestTranscriptAppendReplayMakesAStoredEntryVisible(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	inner := newPinnedIDMemStore()
	svc := NewServices(&failingSequenceBumpStore{Store: inner}).Transcript
	ref := testConversationRef()

	if _, err := appendInboundForSequenceTest(ctx, svc, ref, "msg-1", testNow()); err == nil {
		t.Fatalf("Append(msg-1) succeeded; the test store should fail the next_sequence advance")
	}
	if got := listVisibleTranscript(t, svc, ref); len(got) != 0 {
		t.Fatalf("visible before replay = %v, want none (next_sequence does not cover msg-1 yet)", providerMessageIDs(got))
	}

	replayed, err := appendInboundForSequenceTest(ctx, svc, ref, "msg-1", testNow())
	if err != nil {
		t.Fatalf("replay Append(msg-1): %v", err)
	}
	if replayed.Sequence != 1 {
		t.Fatalf("replayed sequence = %d, want the stored entry's 1", replayed.Sequence)
	}
	got := listVisibleTranscript(t, svc, ref)
	if len(got) != 1 || got[0].ProviderMessageID != "msg-1" {
		t.Fatalf("visible after replay = %v, want [msg-1#1]", providerMessageIDs(got))
	}
	if seqs := persistedTranscriptSequences(t, inner, ref); len(seqs) != 1 {
		t.Fatalf("persisted entries = %v, want only msg-1", seqs)
	}
}

func TestTranscriptAppendAmbiguousCreateReportsTheErrorAndCoversTheEntry(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	inner := newPinnedIDMemStore()
	svc := NewServices(&ambiguousCreateStore{Store: inner}).Transcript
	ref := testConversationRef()

	if _, err := appendInboundForSequenceTest(ctx, svc, ref, "msg-1", testNow()); err == nil {
		t.Fatalf("Append(msg-1) succeeded; the store reported the write as failed")
	}
	got := listVisibleTranscript(t, svc, ref)
	if len(got) != 1 || got[0].ProviderMessageID != "msg-1" {
		t.Fatalf("visible = %v, want the stored msg-1 covered by next_sequence", providerMessageIDs(got))
	}
}

func TestTranscriptAppendWithoutProviderMessageIDNeverStoresTwice(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	inner := newPinnedIDMemStore()
	svc := NewServices(&ambiguousCreateStore{Store: inner}).Transcript
	ref := testConversationRef()

	if _, err := appendInboundForSequenceTest(ctx, svc, ref, "", testNow()); err == nil {
		t.Fatalf("Append succeeded; the store reported the write as failed and the entry cannot be told from another writer's")
	}
	items, err := inner.List(beads.ListQuery{Label: transcriptConversationLabel(ref)})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	entries := 0
	for _, item := range items {
		if isTranscriptEntryCreate(item) {
			entries++
		}
	}
	if entries != 1 {
		t.Fatalf("stored entries = %d, want 1", entries)
	}
	next, err := appendInboundForSequenceTest(ctx, svc, ref, "", testNow().Add(time.Minute))
	if err != nil {
		t.Fatalf("second Append: %v", err)
	}
	if next.Sequence != 2 {
		t.Fatalf("second sequence = %d, want 2", next.Sequence)
	}
}

func TestTranscriptAppendKeepsEntriesOnAStoreThatMintsOverPinnedIDs(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	store := beads.NewMemStore()
	svc := NewServices(store).Transcript
	ref := testConversationRef()

	for i, id := range []string{"msg-1", "msg-2"} {
		rec, err := appendInboundForSequenceTest(ctx, svc, ref, id, testNow().Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatalf("Append(%s): %v", id, err)
		}
		if rec.Sequence != int64(i+1) {
			t.Fatalf("Append(%s) sequence = %d, want %d", id, rec.Sequence, i+1)
		}
	}
	if got := listVisibleTranscript(t, svc, ref); len(got) != 2 {
		t.Fatalf("visible = %v, want both entries", providerMessageIDs(got))
	}
}

func TestTranscriptAppendRefusesAPinnedIDHeldByAnotherBead(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	store := newPinnedIDMemStore()
	svc := NewServices(store).Transcript
	ref := testConversationRef()

	if _, err := appendInboundForSequenceTest(ctx, svc, ref, "msg-1", testNow()); err != nil {
		t.Fatalf("Append(msg-1): %v", err)
	}
	state := transcriptStateFor(t, store, ref)
	if _, err := store.Create(beads.Bead{ID: transcriptEntryID(state.ID, 2), Title: "squatter"}); err != nil {
		t.Fatalf("create squatter: %v", err)
	}
	_, err := appendInboundForSequenceTest(ctx, svc, ref, "msg-2", testNow().Add(time.Minute))
	if !errors.Is(err, ErrInvariantViolation) {
		t.Fatalf("Append(msg-2) error = %v, want ErrInvariantViolation", err)
	}
}

// casCountingStore counts next_sequence writes by kind.
type casCountingStore struct {
	*beads.MemStore
	mu          sync.Mutex
	casSwaps    int
	plainWrites int
}

func (s *casCountingStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	swapped, err := s.MemStore.CompareAndSetMetadataKey(id, key, expected, next)
	if key == "next_sequence" && swapped {
		s.mu.Lock()
		s.casSwaps++
		s.mu.Unlock()
	}
	return swapped, err
}

func (s *casCountingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if _, ok := kvs["next_sequence"]; ok {
		s.mu.Lock()
		s.plainWrites++
		s.mu.Unlock()
	}
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func TestTranscriptAppendAdvancesNextSequenceByCompareAndSet(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	store := &casCountingStore{MemStore: newPinnedIDMemStore()}
	svc := NewServices(store).Transcript
	ref := testConversationRef()

	for i, id := range []string{"msg-1", "msg-2", "msg-3"} {
		if _, err := appendInboundForSequenceTest(ctx, svc, ref, id, testNow().Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("Append(%s): %v", id, err)
		}
	}
	if store.casSwaps != 3 || store.plainWrites != 0 {
		t.Fatalf("next_sequence writes: cas=%d plain=%d, want cas=3 plain=0", store.casSwaps, store.plainWrites)
	}
}

func TestTranscriptAppendFallsBackWhenCompareAndSetIsUnsupported(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	inner := newPinnedIDMemStore()
	inner.DisableConditionalWrites = true
	store := &casCountingStore{MemStore: inner}
	svc := NewServices(store).Transcript
	ref := testConversationRef()

	for i, id := range []string{"msg-1", "msg-2"} {
		if _, err := appendInboundForSequenceTest(ctx, svc, ref, id, testNow().Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("Append(%s): %v", id, err)
		}
	}
	if store.plainWrites != 2 {
		t.Fatalf("plain next_sequence writes = %d, want 2", store.plainWrites)
	}
	if state := transcriptStateFor(t, inner, ref); state.NextSequence != 3 {
		t.Fatalf("next_sequence = %d, want 3", state.NextSequence)
	}
}

func TestTranscriptAdvanceNextSequenceNeverLowersIt(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	store := newPinnedIDMemStore()
	svc := newTranscriptService(store, newBindingLockPool())
	ref := testConversationRef()

	for i, id := range []string{"msg-1", "msg-2", "msg-3"} {
		if _, err := appendInboundForSequenceTest(ctx, svc, ref, id, testNow().Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("Append(%s): %v", id, err)
		}
	}
	state := transcriptStateFor(t, store, ref)
	if err := svc.advanceNextSequenceLocked(state, 2); err != nil {
		t.Fatalf("advance to 2: %v", err)
	}
	if got := transcriptStateFor(t, store, ref).NextSequence; got != 4 {
		t.Fatalf("next_sequence after a stale advance = %d, want 4", got)
	}
}
