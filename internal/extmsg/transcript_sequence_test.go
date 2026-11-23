package extmsg

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/events"
)

// transcriptFaultStore wraps a store and injects faults into transcript
// writes: entry creates that commit and then report a transport error, entry
// reads that fail, and next_sequence writes that fail without writing. It
// forwards pinned create and metadata compare-and-set to the wrapped store, so
// it hides no capability a transcript append needs, and logs every
// next_sequence value written.
type transcriptFaultStore struct {
	beads.Store

	mu sync.Mutex
	// commitThenFailCreates counts transcript entry creates that commit and
	// then return a transport error.
	commitThenFailCreates int
	// failEntryGets counts reads by id of a transcript entry that fail.
	failEntryGets int
	// failNextSequenceWrites counts next_sequence writes that fail unwritten.
	failNextSequenceWrites int
	// afterEntryCreate runs once, after the first transcript entry create
	// commits and before it returns.
	afterEntryCreate func(beads.Bead)
	frontier         []int64
}

func (f *transcriptFaultStore) Create(b beads.Bead) (beads.Bead, error) {
	return f.createEntry(b, f.Store.Create)
}

func (f *transcriptFaultStore) CreatePinned(b beads.Bead) (beads.Bead, error) {
	pinned, ok := f.Store.(interface {
		CreatePinned(beads.Bead) (beads.Bead, error)
	})
	if !ok {
		return beads.Bead{}, fmt.Errorf("wrapped store %T has no pinned create", f.Store)
	}
	return f.createEntry(b, pinned.CreatePinned)
}

func (f *transcriptFaultStore) createEntry(b beads.Bead, create func(beads.Bead) (beads.Bead, error)) (beads.Bead, error) {
	if !slices.Contains(b.Labels, "gc:extmsg-transcript") {
		return create(b)
	}
	created, err := create(b)
	if err != nil {
		return created, err
	}
	f.mu.Lock()
	fail := f.commitThenFailCreates > 0
	if fail {
		f.commitThenFailCreates--
	}
	hook := f.afterEntryCreate
	f.afterEntryCreate = nil
	f.mu.Unlock()
	if hook != nil {
		hook(created)
	}
	if fail {
		return beads.Bead{}, errors.New("invalid connection")
	}
	return created, nil
}

func (f *transcriptFaultStore) Get(id string) (beads.Bead, error) {
	b, err := f.Store.Get(id)
	if err != nil || !slices.Contains(b.Labels, "gc:extmsg-transcript") {
		return b, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failEntryGets > 0 {
		f.failEntryGets--
		return beads.Bead{}, errors.New("read timed out")
	}
	return b, nil
}

func (f *transcriptFaultStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	if key == "next_sequence" && f.takeNextSequenceFault() {
		return false, errors.New("next_sequence write failed")
	}
	cas, ok := beads.As[beads.MetadataCASWriter](f.Store)
	if !ok {
		return false, beads.ErrConditionalWriteUnsupported
	}
	swapped, err := cas.CompareAndSetMetadataKey(id, key, expected, next)
	if err == nil && swapped && key == "next_sequence" {
		f.logFrontier(next)
	}
	return swapped, err
}

func (f *transcriptFaultStore) SetMetadata(id, key, value string) error {
	return f.SetMetadataBatch(id, map[string]string{key: value})
}

func (f *transcriptFaultStore) SetMetadataBatch(id string, kvs map[string]string) error {
	next, bumps := kvs["next_sequence"]
	if bumps && f.takeNextSequenceFault() {
		return errors.New("next_sequence write failed")
	}
	if err := f.Store.SetMetadataBatch(id, kvs); err != nil {
		return err
	}
	if bumps {
		f.logFrontier(next)
	}
	return nil
}

func (f *transcriptFaultStore) takeNextSequenceFault() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNextSequenceWrites > 0 {
		f.failNextSequenceWrites--
		return true
	}
	return false
}

func (f *transcriptFaultStore) logFrontier(value string) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		n = -1
	}
	f.mu.Lock()
	f.frontier = append(f.frontier, n)
	f.mu.Unlock()
}

func (f *transcriptFaultStore) setFaults(apply func(f *transcriptFaultStore)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	apply(f)
}

func (f *transcriptFaultStore) assertFrontierNeverDecreased(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := 1; i < len(f.frontier); i++ {
		if f.frontier[i] < f.frontier[i-1] {
			t.Fatalf("next_sequence regressed: writes %v", f.frontier)
		}
	}
}

func appendTestTranscript(svc TranscriptService, ref ConversationRef, providerMessageID, text string) (ConversationTranscriptRecord, error) {
	return svc.Append(context.Background(), AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: providerMessageID,
		Text:              text,
		CreatedAt:         testNow(),
	})
}

// assertTranscriptSequencesUnique fails when two transcript entries in store
// share a sequence, closed entries included.
func assertTranscriptSequencesUnique(t *testing.T, store beads.Store) []ConversationTranscriptRecord {
	t.Helper()
	items, err := store.List(beads.ListQuery{Label: labelTranscriptBase, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List(transcript entries): %v", err)
	}
	seen := map[int64]string{}
	records := make([]ConversationTranscriptRecord, 0, len(items))
	for _, item := range items {
		record, err := decodeTranscriptBead(item)
		if err != nil {
			t.Fatalf("decode transcript entry %s: %v", item.ID, err)
		}
		if prior, dup := seen[record.Sequence]; dup {
			t.Fatalf("transcript entries %s and %s share sequence %d", prior, record.ID, record.Sequence)
		}
		seen[record.Sequence] = record.ID
		records = append(records, record)
	}
	sortTranscriptRecords(records, false)
	return records
}

// assertListedTexts fails unless List returns exactly texts, in order, at
// sequences 1..len(texts).
func assertListedTexts(t *testing.T, svc TranscriptService, ref ConversationRef, texts ...string) []ConversationTranscriptRecord {
	t.Helper()
	got, err := svc.List(context.Background(), ListTranscriptInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		Limit:        100,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	gotTexts := make([]string, 0, len(got))
	for _, record := range got {
		gotTexts = append(gotTexts, fmt.Sprintf("%d:%s", record.Sequence, record.Text))
	}
	want := make([]string, 0, len(texts))
	for i, text := range texts {
		want = append(want, fmt.Sprintf("%d:%s", i+1, text))
	}
	if !slices.Equal(gotTexts, want) {
		t.Fatalf("List = %v, want %v", gotTexts, want)
	}
	return got
}

// TestTranscriptAppendSequenceUniqueAfterAmbiguousCreate is the reported
// repro, adapted: the fault store forwards pinned create and compare-and-set,
// which an append now requires. A create that commits and then reports a
// transport error is recovered from the slot it was pinned to, so the next
// append gets the next sequence and both entries are published.
func TestTranscriptAppendSequenceUniqueAfterAmbiguousCreate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		providerIDs [2]string
	}{
		{name: "with provider message ids", providerIDs: [2]string{"msg-1", "msg-2"}},
		{name: "without provider message ids"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			freezeTestClock(t)
			fault := &transcriptFaultStore{Store: beads.NewMemStore(), commitThenFailCreates: 1}
			svc := NewServices(beadstest.Mail(fault), beadstest.Sessions(fault)).Transcript
			ref := testConversationRef()

			first, err := appendTestTranscript(svc, ref, tc.providerIDs[0], "first")
			if err != nil {
				t.Fatalf("Append(first) after a committed create that reported a transport error: %v; want the committed entry recovered", err)
			}
			second, err := appendTestTranscript(svc, ref, tc.providerIDs[1], "second")
			if err != nil {
				t.Fatalf("Append(second): %v", err)
			}
			if first.Sequence != 1 || second.Sequence != 2 {
				t.Fatalf("sequences = %d, %d; want 1, 2", first.Sequence, second.Sequence)
			}
			if stored := assertTranscriptSequencesUnique(t, fault); len(stored) != 2 {
				t.Fatalf("stored %d transcript entries, want 2", len(stored))
			}
			assertListedTexts(t, svc, ref, "first", "second")
		})
	}
}

// TestTranscriptAppendSequenceUniqueAfterStateBumpFailure covers a failed
// head advance after the entry was created: a transient failure is retried
// within the append, and a persistent one leaves the entry for the next
// append to publish before it allocates above it.
func TestTranscriptAppendSequenceUniqueAfterStateBumpFailure(t *testing.T) {
	t.Run("a transient failure is retried", func(t *testing.T) {
		freezeTestClock(t)
		fault := &transcriptFaultStore{Store: beads.NewMemStore(), failNextSequenceWrites: 1}
		svc := NewServices(beadstest.Mail(fault), beadstest.Sessions(fault)).Transcript
		ref := testConversationRef()

		if _, err := appendTestTranscript(svc, ref, "msg-1", "first"); err != nil {
			t.Fatalf("Append(first) after one failed head advance: %v; want the advance retried", err)
		}
		if _, err := appendTestTranscript(svc, ref, "msg-2", "second"); err != nil {
			t.Fatalf("Append(second): %v", err)
		}
		assertTranscriptSequencesUnique(t, fault)
		assertListedTexts(t, svc, ref, "first", "second")
	})

	t.Run("a later append publishes the stranded entry", func(t *testing.T) {
		freezeTestClock(t)
		fault := &transcriptFaultStore{Store: beads.NewMemStore(), failNextSequenceWrites: 1000}
		svc := NewServices(beadstest.Mail(fault), beadstest.Sessions(fault)).Transcript
		ref := testConversationRef()

		if _, err := appendTestTranscript(svc, ref, "msg-1", "first"); err == nil {
			t.Fatal("Append(first) succeeded while every head advance failed; want an error")
		}
		fault.setFaults(func(f *transcriptFaultStore) { f.failNextSequenceWrites = 0 })
		second, err := appendTestTranscript(svc, ref, "msg-2", "second")
		if err != nil {
			t.Fatalf("Append(second): %v", err)
		}
		if second.Sequence != 2 {
			t.Fatalf("second.Sequence = %d, want 2 (above the stranded entry)", second.Sequence)
		}
		assertTranscriptSequencesUnique(t, fault)
		assertListedTexts(t, svc, ref, "first", "second")
		fault.assertFrontierNeverDecreased(t)
	})
}

// TestTranscriptAppendSequenceUniqueAcrossLockPools runs writers with
// independent lock pools over one store, as two store generations across a
// config reload do: sequences stay unique and next_sequence never regresses.
func TestTranscriptAppendSequenceUniqueAcrossLockPools(t *testing.T) {
	t.Run("a writer that interleaves inside another's append", func(t *testing.T) {
		freezeTestClock(t)
		fault := &transcriptFaultStore{Store: beads.NewMemStore()}
		svcA := newTranscriptService(beadstest.Mail(fault), newBindingLockPool())
		svcB := newTranscriptService(beadstest.Mail(fault), newBindingLockPool())
		ref := testConversationRef()

		var (
			b    ConversationTranscriptRecord
			bErr error
		)
		fault.afterEntryCreate = func(beads.Bead) {
			b, bErr = appendTestTranscript(svcB, ref, "msg-b", "b")
		}
		a, err := appendTestTranscript(svcA, ref, "msg-a", "a")
		if err != nil {
			t.Fatalf("Append(a): %v", err)
		}
		if bErr != nil {
			t.Fatalf("Append(b) inside a's append: %v", bErr)
		}
		if a.Sequence == b.Sequence {
			t.Fatalf("both writers got sequence %d", a.Sequence)
		}
		if stored := assertTranscriptSequencesUnique(t, fault); len(stored) != 2 {
			t.Fatalf("stored %d transcript entries, want 2", len(stored))
		}
		assertListedTexts(t, svcA, ref, "a", "b")
		fault.assertFrontierNeverDecreased(t)
	})

	t.Run("concurrent writers", func(t *testing.T) {
		freezeTestClock(t)
		fault := &transcriptFaultStore{Store: beads.NewMemStore()}
		pools := []*transcriptService{
			newTranscriptService(beadstest.Mail(fault), newBindingLockPool()),
			newTranscriptService(beadstest.Mail(fault), newBindingLockPool()),
		}
		ref := testConversationRef()
		if _, err := appendTestTranscript(pools[0], ref, "msg-0", "seed"); err != nil {
			t.Fatalf("Append(seed): %v", err)
		}
		const writers = 12
		var wg sync.WaitGroup
		errs := make(chan error, writers)
		for i := range writers {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				id := fmt.Sprintf("msg-%d", i+1)
				if _, err := appendTestTranscript(pools[i%2], ref, id, id); err != nil {
					errs <- fmt.Errorf("Append(%s): %w", id, err)
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		stored := assertTranscriptSequencesUnique(t, fault)
		if len(stored) != writers+1 {
			t.Fatalf("stored %d transcript entries, want %d", len(stored), writers+1)
		}
		for i, record := range stored {
			if record.Sequence != int64(i+1) {
				t.Fatalf("stored sequences have a gap at %d: %d", i+1, record.Sequence)
			}
		}
		fault.assertFrontierNeverDecreased(t)
	})
}

// TestTranscriptBackfillCursorDoesNotSkipLateEntry runs two writers on
// independent primed caches over one store. One writer's entry lands while
// the member reads and acknowledges the head; the member's later backfill
// returns it, and no entry at or below the final acknowledgement goes unseen.
func TestTranscriptBackfillCursorDoesNotSkipLateEntry(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	backing := beads.NewMemStore()
	cacheA := beads.NewCachingStoreForTest(backing, nil)
	cacheB := beads.NewCachingStoreForTest(backing, nil)
	faultA := &transcriptFaultStore{Store: cacheA}
	svcA := newTranscriptService(beadstest.Mail(faultA), newBindingLockPool())
	svcB := newTranscriptService(beadstest.Mail(cacheB), newBindingLockPool())
	ref := testConversationRef()
	const member = "member-1"

	if _, err := svcB.EnsureMembership(ctx, EnsureMembershipInput{
		Caller:         testControllerCaller(),
		Conversation:   ref,
		SessionID:      member,
		BackfillPolicy: MembershipBackfillAll,
		Owner:          MembershipOwnerManual,
		Now:            testNow(),
	}); err != nil {
		t.Fatalf("EnsureMembership: %v", err)
	}
	if _, err := appendTestTranscript(svcB, ref, "msg-0", "seed"); err != nil {
		t.Fatalf("Append(seed): %v", err)
	}
	for name, cache := range map[string]*beads.CachingStore{"A": cacheA, "B": cacheB} {
		if err := cache.PrimeActive(); err != nil {
			t.Fatalf("PrimeActive(%s): %v", name, err)
		}
	}

	seen := map[int64]string{}
	var acked int64
	readAndAck := func(step string) {
		t.Helper()
		got, err := svcB.ListBackfill(ctx, ListBackfillInput{
			Caller:       testControllerCaller(),
			Conversation: ref,
			SessionID:    member,
			Limit:        100,
		})
		if err != nil {
			t.Fatalf("ListBackfill(%s): %v", step, err)
		}
		for _, record := range got {
			seen[record.Sequence] = record.Text
			acked = max(acked, record.Sequence)
		}
		state, err := svcB.State(ctx, testControllerCaller(), ref)
		if err != nil || state == nil {
			t.Fatalf("State(%s) = %v, %v", step, state, err)
		}
		acked = max(acked, state.NextSequence-1)
		if err := svcB.Ack(ctx, AckMembershipInput{
			Caller:       testControllerCaller(),
			Conversation: ref,
			SessionID:    member,
			Sequence:     acked,
		}); err != nil {
			t.Fatalf("Ack(%s, %d): %v", step, acked, err)
		}
	}
	readAndAck("before the late writer")

	var bErr error
	faultA.afterEntryCreate = func(beads.Bead) {
		readAndAck("while the late entry is unpublished")
		if _, bErr = appendTestTranscript(svcB, ref, "msg-2", "after"); bErr == nil {
			readAndAck("after the next append")
		}
	}
	if _, err := appendTestTranscript(svcA, ref, "msg-late", "late"); err != nil {
		t.Fatalf("Append(late): %v", err)
	}
	if bErr != nil {
		t.Fatalf("Append(after) on the other cache: %v", bErr)
	}
	readAndAck("after the late writer returned")

	stored := assertTranscriptSequencesUnique(t, backing)
	for _, record := range stored {
		if record.Sequence <= acked && seen[record.Sequence] != record.Text {
			t.Fatalf("entry %d (%q) is at or below the acknowledged sequence %d but backfill never returned it; seen %v",
				record.Sequence, record.Text, acked, seen)
		}
	}
	if !slices.Contains(mapValues(seen), "late") {
		t.Fatalf("backfill never returned the late entry; seen %v", seen)
	}
}

func mapValues(m map[int64]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// TestTranscriptPinnedAppendAdoptsLegacyMintedEntry covers an entry an older
// writer created under a minted id at next_sequence without advancing it: an
// upgraded writer publishes it unchanged and allocates above it.
func TestTranscriptPinnedAppendAdoptsLegacyMintedEntry(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(beadstest.Mail(store), beadstest.Sessions(store)).Transcript
	ref := testConversationRef()

	seed, err := appendTestTranscript(svc, ref, "msg-0", "seed")
	if err != nil {
		t.Fatalf("Append(seed): %v", err)
	}
	seedBead, err := store.Get(seed.ID)
	if err != nil {
		t.Fatalf("Get(seed): %v", err)
	}
	metadata := map[string]string{}
	for k, v := range seedBead.Metadata {
		if k == "append_token" {
			continue
		}
		metadata[k] = v
	}
	metadata["sequence"] = "2"
	metadata["provider_message_id"] = "msg-legacy"
	labels := make([]string, 0, len(seedBead.Labels))
	for _, label := range seedBead.Labels {
		if label == transcriptProviderMessageLabel(ref, "msg-0") {
			label = transcriptProviderMessageLabel(ref, "msg-legacy")
		}
		labels = append(labels, label)
	}
	legacy, err := store.Create(beads.Bead{
		Title:       conversationTitle(ref) + "#2",
		Type:        "task",
		Description: "legacy",
		Labels:      labels,
		Metadata:    metadata,
	})
	if err != nil {
		t.Fatalf("Create(legacy entry): %v", err)
	}

	next, err := appendTestTranscript(svc, ref, "msg-new", "upgraded")
	if err != nil {
		t.Fatalf("Append(upgraded): %v", err)
	}
	if next.Sequence != 3 {
		t.Fatalf("upgraded append got sequence %d, want 3 (above the legacy entry)", next.Sequence)
	}
	assertTranscriptSequencesUnique(t, store)
	listed := assertListedTexts(t, svc, ref, "seed", "legacy", "upgraded")
	if listed[1].ID != legacy.ID {
		t.Fatalf("legacy entry listed as %s, want its original id %s (no renumbering)", listed[1].ID, legacy.ID)
	}
}

// inboundRecoveryDeps builds inbound deps that route by default to an agent
// and count ExtMsgInbound emissions.
func inboundRecoveryDeps(fabric Services, emitted *int) InboundDeps {
	return InboundDeps{
		Services:                    fabric,
		DefaultAgentForConversation: func(ConversationRef) string { return "myrig/frontdesk" },
		EmitEvent: func(eventType, _ string, _ events.Payload) {
			if eventType == events.ExtMsgInbound {
				*emitted++
			}
		},
	}
}

func inboundRecoveryMessage(ref ConversationRef, providerMessageID, text string) ExternalInboundMessage {
	return ExternalInboundMessage{
		ProviderMessageID: providerMessageID,
		Conversation:      ref,
		Actor:             ExternalActor{ID: "user-1", DisplayName: "User One"},
		Text:              text,
		ReceivedAt:        testNow(),
	}
}

// TestHandleInboundNormalizedAmbiguousCreateKeepsSequencesDistinct drives the
// inbound path through a create that commits and reports an error, followed
// by a failed read of the slot it was pinned to: the inbound recovers the
// entry once, emits one event, and the next inbound gets the next sequence.
func TestHandleInboundNormalizedAmbiguousCreateKeepsSequencesDistinct(t *testing.T) {
	freezeTestClock(t)
	fault := &transcriptFaultStore{Store: beads.NewMemStore()}
	fabric := NewServices(beadstest.Mail(fault), beadstest.Sessions(fault))
	ref := testConversationRef()
	emitted := 0
	deps := inboundRecoveryDeps(fabric, &emitted)

	fault.setFaults(func(f *transcriptFaultStore) {
		f.commitThenFailCreates = 1
		f.failEntryGets = 1
	})
	first, err := HandleInboundNormalized(context.Background(), deps, inboundRecoveryMessage(ref, "msg-1", "first"))
	if err != nil {
		t.Fatalf("HandleInboundNormalized(first) with a recoverable transcript fault: %v", err)
	}
	if first.TranscriptEntry == nil || first.TranscriptEntry.Sequence != 1 {
		t.Fatalf("first transcript entry = %#v, want sequence 1", first.TranscriptEntry)
	}
	if emitted != 1 {
		t.Fatalf("ExtMsgInbound emitted %d times for the first inbound, want 1", emitted)
	}
	second, err := HandleInboundNormalized(context.Background(), deps, inboundRecoveryMessage(ref, "msg-2", "second"))
	if err != nil {
		t.Fatalf("HandleInboundNormalized(second): %v", err)
	}
	if second.TranscriptEntry == nil || second.TranscriptEntry.Sequence != 2 {
		t.Fatalf("second transcript entry = %#v, want sequence 2", second.TranscriptEntry)
	}
	if stored := assertTranscriptSequencesUnique(t, fault); len(stored) != 2 {
		t.Fatalf("stored %d transcript entries, want 2", len(stored))
	}
	assertListedTexts(t, fabric.Transcript, ref, "first", "second")
}
