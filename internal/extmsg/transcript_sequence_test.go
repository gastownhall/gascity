package extmsg

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// These tests pin the transcript sequence invariant: within one conversation
// every persisted transcript entry carries a distinct sequence, and an entry a
// member has not read is never hidden behind that member's read cursor.
//
// Append allocates a sequence by reading the transcript-state bead's
// next_sequence, creating the entry, and only then bumping next_sequence. The
// only guard is withBindingLock, an in-memory mutex from a lock pool keyed by
// the beads.Store value. Two writers that do not share a lock pool (two
// processes, or the Services generation the controller rebuilds over a fresh
// CachingStore on every config reload) can read the same next_sequence. A
// single writer can reuse a sequence too, when the entry write lands but the
// call reports an error, or when the next_sequence bump fails.

const transcriptSequenceTestTimeout = 10 * time.Second

// isTranscriptEntryCreate reports whether b is a transcript entry (not the
// transcript-state bead, which Append also creates on first touch).
func isTranscriptEntryCreate(b beads.Bead) bool {
	return len(b.Labels) > 0 && b.Labels[0] == "gc:extmsg-transcript"
}

// gatedCreateStore pauses the first transcript-entry Create made through it
// until release is closed, after signaling entered. It models a writer whose
// entry write is slow (a bd subprocess under Dolt load) while another writer
// proceeds.
type gatedCreateStore struct {
	beads.Store
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newGatedCreateStore(inner beads.Store) *gatedCreateStore {
	return &gatedCreateStore{
		Store:   inner,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (s *gatedCreateStore) Create(b beads.Bead) (beads.Bead, error) {
	if isTranscriptEntryCreate(b) {
		s.once.Do(func() {
			close(s.entered)
			<-s.release
		})
	}
	return s.Store.Create(b)
}

// ambiguousCreateStore commits the first transcript-entry Create to the inner
// store and then reports a transport error, the way a bd create whose
// connection drops after commit does. BdStore surfaces such an error rather
// than retrying an id-less create (isBdAmbiguousWriteError).
type ambiguousCreateStore struct {
	beads.Store
	mu    sync.Mutex
	fired bool
}

func (s *ambiguousCreateStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := s.Store.Create(b)
	if err != nil || !isTranscriptEntryCreate(b) {
		return created, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fired {
		return created, nil
	}
	s.fired = true
	return beads.Bead{}, errors.New("bd create: exit status 1: invalid connection")
}

// failingSequenceBumpStore fails the first next_sequence bump on the
// transcript-state bead.
type failingSequenceBumpStore struct {
	beads.Store
	mu    sync.Mutex
	fired bool
}

func (s *failingSequenceBumpStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if _, ok := kvs["next_sequence"]; ok {
		s.mu.Lock()
		fire := !s.fired
		s.fired = true
		s.mu.Unlock()
		if fire {
			return errors.New("bd update: exit status 1: i/o timeout")
		}
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// newPinnedIDMemStore returns an in-memory store that keeps a caller's pinned
// bead ID and refuses a second bead with it, as every production bead store
// does. The default MemStore mints over a pinned ID, so it cannot model the
// store-level refusal that keeps transcript sequences unique.
func newPinnedIDMemStore() *beads.MemStore {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	return store
}

func appendInboundForSequenceTest(ctx context.Context, svc TranscriptService, ref ConversationRef, providerMessageID string, at time.Time) (ConversationTranscriptRecord, error) {
	return svc.Append(ctx, AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: providerMessageID,
		Text:              providerMessageID,
		CreatedAt:         at,
	})
}

// persistedTranscriptSequences reads every transcript entry for ref straight
// from the store, keyed by provider message id. It bypasses
// listTranscriptLocked, whose next_sequence window can hide an entry.
func persistedTranscriptSequences(t *testing.T, store beads.Store, ref ConversationRef) map[string]int64 {
	t.Helper()
	items, err := store.List(beads.ListQuery{Label: transcriptConversationLabel(ref)})
	if err != nil {
		t.Fatalf("list transcript entries: %v", err)
	}
	out := map[string]int64{}
	for _, item := range items {
		if !isTranscriptEntryCreate(item) {
			continue
		}
		record, err := decodeTranscriptBead(item)
		if err != nil {
			t.Fatalf("decode transcript entry %s: %v", item.ID, err)
		}
		out[record.ProviderMessageID] = record.Sequence
	}
	return out
}

// requireDistinctSequences fails when two persisted entries share a sequence.
func requireDistinctSequences(t *testing.T, seqs map[string]int64) {
	t.Helper()
	byKey := map[int64][]string{}
	for id, seq := range seqs {
		byKey[seq] = append(byKey[seq], id)
	}
	var dupes []string
	for seq, ids := range byKey {
		if len(ids) > 1 {
			sort.Strings(ids)
			dupes = append(dupes, fmt.Sprintf("#%d=%v", seq, ids))
		}
	}
	if len(dupes) > 0 {
		sort.Strings(dupes)
		t.Fatalf("duplicate transcript sequences %v (all entries: %v)", dupes, seqs)
	}
}

func awaitSequenceTestSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(transcriptSequenceTestTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// startGatedAppend runs Append for providerMessageID through a writer whose
// entry Create is held at the gate, and returns once that Create has been
// reached (so the writer has already read next_sequence).
func startGatedAppend(t *testing.T, svc TranscriptService, gate *gatedCreateStore, ref ConversationRef, providerMessageID string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := appendInboundForSequenceTest(context.Background(), svc, ref, providerMessageID, testNow().Add(time.Minute))
		done <- err
	}()
	awaitSequenceTestSignal(t, gate.entered, providerMessageID+" to reach its entry write")
	return done
}

func finishGatedAppend(t *testing.T, gate *gatedCreateStore, done <-chan error, providerMessageID string) {
	t.Helper()
	close(gate.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Append(%s): %v", providerMessageID, err)
		}
	case <-time.After(transcriptSequenceTestTimeout):
		t.Fatalf("timed out waiting for Append(%s)", providerMessageID)
	}
}

// TestTranscriptAppendSequenceUniqueAcrossLockPools: two transcript services
// over one store with separate lock pools model two writers to one
// conversation. Writer B reads next_sequence and stalls in its entry write;
// writer A appends meanwhile. Both entries must get distinct sequences.
func TestTranscriptAppendSequenceUniqueAcrossLockPools(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	shared := newPinnedIDMemStore()
	ref := testConversationRef()

	writerA := newTranscriptService(shared, newBindingLockPool())
	gate := newGatedCreateStore(shared)
	writerB := newTranscriptService(gate, newBindingLockPool())

	if _, err := appendInboundForSequenceTest(ctx, writerA, ref, "msg-0", testNow()); err != nil {
		t.Fatalf("Append(msg-0): %v", err)
	}

	doneB := startGatedAppend(t, writerB, gate, ref, "msg-b")
	if _, err := appendInboundForSequenceTest(ctx, writerA, ref, "msg-a", testNow().Add(2*time.Minute)); err != nil {
		t.Fatalf("Append(msg-a): %v", err)
	}
	finishGatedAppend(t, gate, doneB, "msg-b")

	requireDistinctSequences(t, persistedTranscriptSequences(t, shared, ref))
}

// TestTranscriptBackfillCursorDoesNotSkipLateEntry: a member that has read and
// acked the head must still be shown an entry another writer persists later.
// With a reused sequence the late entry sorts at or below last_read_sequence,
// so ListBackfill (sequence > last_read_sequence) never returns it.
func TestTranscriptBackfillCursorDoesNotSkipLateEntry(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	shared := newPinnedIDMemStore()
	ref := testConversationRef()

	writerA := newTranscriptService(shared, newBindingLockPool())
	gate := newGatedCreateStore(shared)
	writerB := newTranscriptService(gate, newBindingLockPool())

	if _, err := appendInboundForSequenceTest(ctx, writerA, ref, "msg-0", testNow()); err != nil {
		t.Fatalf("Append(msg-0): %v", err)
	}
	if _, err := writerA.EnsureMembership(ctx, EnsureMembershipInput{
		Caller:         testControllerCaller(),
		Conversation:   ref,
		SessionID:      "sess-a",
		BackfillPolicy: MembershipBackfillAll,
		Now:            testNow(),
	}); err != nil {
		t.Fatalf("EnsureMembership: %v", err)
	}

	doneB := startGatedAppend(t, writerB, gate, ref, "msg-b")
	if _, err := appendInboundForSequenceTest(ctx, writerA, ref, "msg-a", testNow().Add(2*time.Minute)); err != nil {
		t.Fatalf("Append(msg-a): %v", err)
	}

	// The member reads everything visible and acks the head.
	seen, err := writerA.ListBackfill(ctx, ListBackfillInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		SessionID:    "sess-a",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("ListBackfill(before late entry): %v", err)
	}
	read := map[string]bool{}
	var head int64
	for _, entry := range seen {
		read[entry.ProviderMessageID] = true
		if entry.Sequence > head {
			head = entry.Sequence
		}
	}
	if err := writerA.Ack(ctx, AckMembershipInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		SessionID:    "sess-a",
		Sequence:     head,
	}); err != nil {
		t.Fatalf("Ack(%d): %v", head, err)
	}

	finishGatedAppend(t, gate, doneB, "msg-b")

	after, err := writerA.ListBackfill(ctx, ListBackfillInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		SessionID:    "sess-a",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("ListBackfill(after late entry): %v", err)
	}
	for _, entry := range after {
		read[entry.ProviderMessageID] = true
	}
	if !read["msg-b"] {
		t.Fatalf("msg-b never reached the member: first read %v, acked #%d, then read %v; persisted %v",
			providerMessageIDs(seen), head, providerMessageIDs(after), persistedTranscriptSequences(t, shared, ref))
	}
}

// TestTranscriptAppendSequenceUniqueAfterAmbiguousCreate: a single writer, no
// race. The first entry write commits but reports an error, so Append returns
// an error without bumping next_sequence. The next message must not reuse the
// committed entry's sequence.
func TestTranscriptAppendSequenceUniqueAfterAmbiguousCreate(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	inner := newPinnedIDMemStore()
	svc := NewServices(&ambiguousCreateStore{Store: inner}).Transcript
	ref := testConversationRef()

	if _, err := appendInboundForSequenceTest(ctx, svc, ref, "msg-1", testNow()); err == nil {
		t.Fatalf("Append(msg-1) succeeded; the test store should report the committed write as an error")
	}
	if _, err := appendInboundForSequenceTest(ctx, svc, ref, "msg-2", testNow().Add(21*time.Minute)); err != nil {
		t.Fatalf("Append(msg-2): %v", err)
	}

	seqs := persistedTranscriptSequences(t, inner, ref)
	if len(seqs) != 2 {
		t.Fatalf("persisted entries = %v, want msg-1 and msg-2", seqs)
	}
	requireDistinctSequences(t, seqs)
}

// TestTranscriptAppendSequenceUniqueAfterStateBumpFailure: the entry write
// succeeds and the next_sequence bump fails. The next message must not reuse
// the sequence.
func TestTranscriptAppendSequenceUniqueAfterStateBumpFailure(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	inner := newPinnedIDMemStore()
	svc := NewServices(&failingSequenceBumpStore{Store: inner}).Transcript
	ref := testConversationRef()

	if _, err := appendInboundForSequenceTest(ctx, svc, ref, "msg-1", testNow()); err == nil {
		t.Fatalf("Append(msg-1) succeeded; the test store should fail the next_sequence bump")
	}
	if _, err := appendInboundForSequenceTest(ctx, svc, ref, "msg-2", testNow().Add(time.Minute)); err != nil {
		t.Fatalf("Append(msg-2): %v", err)
	}

	seqs := persistedTranscriptSequences(t, inner, ref)
	if len(seqs) != 2 {
		t.Fatalf("persisted entries = %v, want msg-1 and msg-2", seqs)
	}
	requireDistinctSequences(t, seqs)
}

// TestHandleInboundNormalizedAmbiguousCreateKeepsSequencesDistinct runs the
// ambiguous-create case through the inbound entry point the API handler calls.
// The first inbound returns an error although its transcript entry is stored,
// and the API handler skips the member notification on any error. The second
// inbound succeeds. The two stored entries must not share a sequence.
func TestHandleInboundNormalizedAmbiguousCreateKeepsSequencesDistinct(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	inner := newPinnedIDMemStore()
	fabric := NewServices(&ambiguousCreateStore{Store: inner})
	ref := testConversationRef()
	deps := InboundDeps{
		Services:                    fabric,
		DefaultAgentForConversation: func(ConversationRef) string { return "myrig/frontdesk" },
	}

	first := defaultRouteTestMessage(ref)
	first.ProviderMessageID = "msg-1"
	first.ReceivedAt = testNow()
	if _, err := HandleInboundNormalized(ctx, deps, first); err == nil {
		t.Fatalf("HandleInboundNormalized(msg-1) succeeded; the test store should report the committed write as an error")
	}

	second := defaultRouteTestMessage(ref)
	second.ProviderMessageID = "msg-2"
	second.ReceivedAt = testNow().Add(21 * time.Minute)
	result, err := HandleInboundNormalized(ctx, deps, second)
	if err != nil {
		t.Fatalf("HandleInboundNormalized(msg-2): %v", err)
	}
	if result.TranscriptEntry == nil {
		t.Fatalf("msg-2 TranscriptEntry = nil, want appended entry")
	}

	seqs := persistedTranscriptSequences(t, inner, ref)
	if _, ok := seqs["msg-1"]; !ok {
		t.Fatalf("msg-1 not persisted (entries %v); the failed inbound must still have written its entry for this repro", seqs)
	}
	requireDistinctSequences(t, seqs)
}

func providerMessageIDs(records []ConversationTranscriptRecord) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, fmt.Sprintf("%s#%d", r.ProviderMessageID, r.Sequence))
	}
	return out
}
