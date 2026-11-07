package extmsg

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// transcriptFaultStore wraps a store with switchable faults on the transcript
// append paths. It exposes no metadata CAS; casTranscriptFaultStore adds it.
type transcriptFaultStore struct {
	beads.Store

	mu sync.Mutex
	// commitThenFailCreates is how many more transcript entry creates commit
	// and then report an error, as an ambiguous write does.
	commitThenFailCreates int
	// failCreates makes transcript entry creates fail without committing.
	failCreates bool
	// failFrontier makes next_sequence writes fail without committing.
	failFrontier bool
	// failGets is how many more Gets of an entry this store created
	// fail.
	failGets int
	// afterEntryCreate, when set, runs once after the next entry create
	// commits and before Create returns.
	afterEntryCreate func()

	entryCreates   int
	createdIDs     map[string]bool
	frontierWrites *[]int64
}

func newTranscriptFaultStore(inner beads.Store, frontierWrites *[]int64) *transcriptFaultStore {
	return &transcriptFaultStore{Store: inner, createdIDs: map[string]bool{}, frontierWrites: frontierWrites}
}

func (f *transcriptFaultStore) Create(b beads.Bead) (beads.Bead, error) {
	if !hasLabel(b, "gc:extmsg-transcript") {
		return f.Store.Create(b)
	}
	f.mu.Lock()
	f.entryCreates++
	failCreate := f.failCreates
	commitThenFail := f.commitThenFailCreates > 0
	if commitThenFail {
		f.commitThenFailCreates--
	}
	hook := f.afterEntryCreate
	f.afterEntryCreate = nil
	f.mu.Unlock()
	if failCreate {
		return beads.Bead{}, errors.New("i/o timeout")
	}
	created, err := f.Store.Create(b)
	if err != nil {
		return created, err
	}
	f.mu.Lock()
	f.createdIDs[created.ID] = true
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if commitThenFail {
		return beads.Bead{}, errors.New("invalid connection")
	}
	return created, nil
}

func (f *transcriptFaultStore) entryCreateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entryCreates
}

func (f *transcriptFaultStore) Get(id string) (beads.Bead, error) {
	f.mu.Lock()
	fail := f.failGets > 0 && f.createdIDs[id]
	if fail {
		f.failGets--
	}
	f.mu.Unlock()
	if fail {
		return beads.Bead{}, errors.New("i/o timeout")
	}
	return f.Store.Get(id)
}

func (f *transcriptFaultStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if next, ok := kvs["next_sequence"]; ok {
		if err := f.frontierWrite(next); err != nil {
			return err
		}
	}
	return f.Store.SetMetadataBatch(id, kvs)
}

func (f *transcriptFaultStore) SetMetadata(id, key, value string) error {
	if key == "next_sequence" {
		if err := f.frontierWrite(value); err != nil {
			return err
		}
	}
	return f.Store.SetMetadata(id, key, value)
}

// frontierWrite fails a next_sequence write while failFrontier is set, and
// records the value of every write it lets through.
func (f *transcriptFaultStore) frontierWrite(value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFrontier {
		return errors.New("i/o timeout")
	}
	if f.frontierWrites != nil {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return err
		}
		*f.frontierWrites = append(*f.frontierWrites, n)
	}
	return nil
}

// casTranscriptFaultStore is a transcriptFaultStore that also offers the
// inner store's metadata compare-and-set.
type casTranscriptFaultStore struct {
	*transcriptFaultStore
}

func (f casTranscriptFaultStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	writer, ok := beads.As[beads.MetadataCASWriter](f.Store)
	if !ok {
		return false, beads.ErrConditionalWriteUnsupported
	}
	if key == "next_sequence" {
		f.mu.Lock()
		fail := f.failFrontier
		f.mu.Unlock()
		if fail {
			return false, errors.New("i/o timeout")
		}
	}
	swapped, err := writer.CompareAndSetMetadataKey(id, key, expected, next)
	if err == nil && swapped && key == "next_sequence" {
		if err := f.frontierWrite(next); err != nil {
			return swapped, err
		}
	}
	return swapped, err
}

// honoringMemStore is a MemStore that keeps pinned ids, as the production
// bead stores do.
func honoringMemStore() *beads.MemStore {
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	return mem
}

func appendTestMessage(t *testing.T, svc *transcriptService, providerMessageID string) (ConversationTranscriptRecord, error) {
	t.Helper()
	return svc.Append(context.Background(), AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      testConversationRef(),
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: providerMessageID,
		Text:              "text " + providerMessageID,
		CreatedAt:         testNow(),
	})
}

func mustAppendTestMessage(t *testing.T, svc *transcriptService, providerMessageID string) ConversationTranscriptRecord {
	t.Helper()
	record, err := appendTestMessage(t, svc, providerMessageID)
	if err != nil {
		t.Fatalf("Append(%q): %v", providerMessageID, err)
	}
	return record
}

// assertUniqueTranscriptSequences fails when two stored entries of the test
// conversation share a sequence, and returns the stored entries by sequence.
func assertUniqueTranscriptSequences(t *testing.T, store beads.Store) map[int64]beads.Bead {
	t.Helper()
	entries, err := store.List(beads.ListQuery{Label: transcriptConversationLabel(testConversationRef())})
	if err != nil {
		t.Fatalf("List(entries): %v", err)
	}
	bySeq := map[int64]beads.Bead{}
	for _, b := range entries {
		if !hasLabel(b, "gc:extmsg-transcript") {
			continue
		}
		seq := parseInt64(b.Metadata, "sequence")
		if prior, ok := bySeq[seq]; ok {
			t.Errorf("duplicate transcript sequence #%d: %s (%s) and %s (%s)", seq,
				prior.ID, prior.Metadata["provider_message_id"], b.ID, b.Metadata["provider_message_id"])
			continue
		}
		bySeq[seq] = b
	}
	return bySeq
}

func testTranscriptNextSequence(t *testing.T, store beads.Store) int64 {
	t.Helper()
	states, err := store.List(beads.ListQuery{Label: transcriptStateLabel(testConversationRef())})
	if err != nil {
		t.Fatalf("List(state): %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("transcript states = %d, want 1", len(states))
	}
	return parseInt64(states[0].Metadata, "next_sequence")
}

// TestTranscriptAppendSequenceUniqueAcrossLockPools: two services whose lock
// pools do not serialize each other (two processes, or two Services
// generations) append to one transcript. A blocks after its entry is stored
// and before it advances next_sequence; B appends meanwhile. Each entry keeps
// its own sequence, and next_sequence never moves backward.
func TestTranscriptAppendSequenceUniqueAcrossLockPools(t *testing.T) {
	freezeTestClock(t)
	mem := honoringMemStore()
	var frontier []int64
	storeA := newTranscriptFaultStore(mem, &frontier)
	storeB := newTranscriptFaultStore(mem, &frontier)
	svcA := newTranscriptService(beadstest.Mail(casTranscriptFaultStore{storeA}), newBindingLockPool())
	svcB := newTranscriptService(beadstest.Mail(casTranscriptFaultStore{storeB}), newBindingLockPool())

	mustAppendTestMessage(t, svcA, "msg-0")

	created := make(chan struct{})
	release := make(chan struct{})
	storeA.mu.Lock()
	storeA.afterEntryCreate = func() {
		close(created)
		<-release
	}
	storeA.mu.Unlock()

	type result struct {
		record ConversationTranscriptRecord
		err    error
	}
	done := make(chan result, 1)
	go func() {
		record, err := appendTestMessage(t, svcA, "msg-a")
		done <- result{record, err}
	}()
	<-created
	recordB := mustAppendTestMessage(t, svcB, "msg-b")
	close(release)
	resA := <-done
	if resA.err != nil {
		t.Fatalf("Append(msg-a): %v", resA.err)
	}

	if resA.record.Sequence == recordB.Sequence {
		t.Fatalf("msg-a and msg-b share sequence %d", recordB.Sequence)
	}
	bySeq := assertUniqueTranscriptSequences(t, mem)
	if len(bySeq) != 3 {
		t.Fatalf("stored entries = %d, want 3", len(bySeq))
	}
	for i := 1; i < len(frontier); i++ {
		if frontier[i] < frontier[i-1] {
			t.Fatalf("next_sequence moved backward: writes %v", frontier)
		}
	}
	if got := testTranscriptNextSequence(t, mem); got != 4 {
		t.Fatalf("next_sequence = %d, want 4 (writes %v)", got, frontier)
	}
}

// TestTranscriptAppendRecoversCommittedCreateError: the entry create commits
// and then reports an error, as an ambiguous write does. Append reads the
// entry back at its pinned ID, publishes it, and returns it as the single
// stored entry; the next append gets the next sequence. A blank provider
// message ID still recovers, through the append's own token.
func TestTranscriptAppendRecoversCommittedCreateError(t *testing.T) {
	for _, providerMessageID := range []string{"msg-1", ""} {
		t.Run("provider_message_id="+providerMessageID, func(t *testing.T) {
			freezeTestClock(t)
			mem := honoringMemStore()
			fault := newTranscriptFaultStore(mem, nil)
			fault.commitThenFailCreates = 1
			svc := newTranscriptService(beadstest.Mail(casTranscriptFaultStore{fault}), newBindingLockPool())

			first, err := appendTestMessage(t, svc, providerMessageID)
			if err != nil {
				t.Fatalf("Append(%q) after committed create error: %v", providerMessageID, err)
			}
			if first.Sequence != 1 {
				t.Fatalf("first sequence = %d, want 1", first.Sequence)
			}
			if got := len(assertUniqueTranscriptSequences(t, mem)); got != 1 {
				t.Fatalf("stored entries after first append = %d, want 1", got)
			}

			next := mustAppendTestMessage(t, svc, "msg-next")
			if next.Sequence != 2 {
				t.Fatalf("next sequence = %d, want 2", next.Sequence)
			}
			if got := len(assertUniqueTranscriptSequences(t, mem)); got != 2 {
				t.Fatalf("stored entries = %d, want 2", got)
			}
			if got := testTranscriptNextSequence(t, mem); got != 3 {
				t.Fatalf("next_sequence = %d, want 3", got)
			}
		})
	}
}

// TestTranscriptAppendReplayCoversStoredEntry: an entry is stored but its
// next_sequence advance fails, so Append errors. Replaying the same provider
// message after the fault clears returns the stored entry without creating
// another, and advances next_sequence past it.
func TestTranscriptAppendReplayCoversStoredEntry(t *testing.T) {
	freezeTestClock(t)
	mem := honoringMemStore()
	fault := newTranscriptFaultStore(mem, nil)
	svc := newTranscriptService(beadstest.Mail(casTranscriptFaultStore{fault}), newBindingLockPool())

	fault.mu.Lock()
	fault.failFrontier = true
	fault.mu.Unlock()
	if _, err := appendTestMessage(t, svc, "msg-1"); err == nil {
		t.Fatal("Append(msg-1) with a failing next_sequence advance succeeded, want error")
	}
	fault.mu.Lock()
	fault.failFrontier = false
	fault.mu.Unlock()
	creates := fault.entryCreateCount()

	replayed := mustAppendTestMessage(t, svc, "msg-1")
	if replayed.Sequence != 1 || replayed.ProviderMessageID != "msg-1" {
		t.Fatalf("replay = sequence %d provider message %q, want 1 msg-1", replayed.Sequence, replayed.ProviderMessageID)
	}
	if got := fault.entryCreateCount(); got != creates {
		t.Fatalf("entry creates = %d after replay, want %d (no new create)", got, creates)
	}
	if got := testTranscriptNextSequence(t, mem); got != 2 {
		t.Fatalf("next_sequence = %d after replay, want 2", got)
	}
}
