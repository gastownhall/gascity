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
