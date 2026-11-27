package extmsg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// transcriptWarnings records what a transcript service logs.
type transcriptWarnings struct {
	mu    sync.Mutex
	lines []string
}

func (w *transcriptWarnings) warnf(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, fmt.Sprintf(format, args...))
}

func (w *transcriptWarnings) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.lines...)
}

// captureTranscriptWarnings routes svc's log lines into the returned record.
func captureTranscriptWarnings(t *testing.T, svc TranscriptService) *transcriptWarnings {
	t.Helper()
	ts, ok := svc.(*transcriptService)
	if !ok {
		t.Fatalf("transcript service is %T, want *transcriptService", svc)
	}
	w := &transcriptWarnings{}
	ts.warnf = w.warnf
	return w
}

// transcriptStateID reads the conversation's transcript state ID.
func transcriptStateID(t *testing.T, svc TranscriptService, ref ConversationRef) string {
	t.Helper()
	state, err := svc.State(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	return state.ID
}

// storeOnlyWrapper embeds a Store and so hides every optional capability of
// the store it wraps.
type storeOnlyWrapper struct {
	beads.Store
}

// TestTranscriptAppendOverReleasedBdRunsUnguarded is the stock bd deployment:
// a bd store on the installable default bd (deps.env BD_VERSION, v1.3.4),
// which has no --if-revision, so it can neither pin a create nor fence the
// head. Appends still record their entries, unguarded, and the service logs
// that once.
func TestTranscriptAppendOverReleasedBdRunsUnguarded(t *testing.T) {
	freezeTestClock(t)
	fake := beadstest.NewFakeBd()
	fake.SetVersion("1.3.4")
	store := fake.NewBdStore()
	fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	warnings := captureTranscriptWarnings(t, fabric.Transcript)
	ref := testConversationRef()

	first, err := appendTestTranscript(fabric.Transcript, ref, "msg-1", "one")
	if err != nil {
		t.Fatalf("Append(one) over a released bd: %v; want it recorded unguarded", err)
	}
	second, err := appendTestTranscript(fabric.Transcript, ref, "msg-2", "two")
	if err != nil {
		t.Fatalf("Append(two) over a released bd: %v", err)
	}
	again, err := appendTestTranscript(fabric.Transcript, ref, "msg-1", "one")
	if err != nil {
		t.Fatalf("Append(one) again over a released bd: %v", err)
	}
	if first.Sequence != 1 || second.Sequence != 2 || again.ID != first.ID {
		t.Fatalf("appends = (%s at %d, %s at %d, repeat %s); want sequences 1 and 2 and the repeat answered by the first entry",
			first.ID, first.Sequence, second.ID, second.Sequence, again.ID)
	}
	assertListedTexts(t, fabric.Transcript, ref, "one", "two")
	stateID := transcriptStateID(t, fabric.Transcript, ref)
	if first.ID == transcriptEntryID(stateID, 1) || second.ID == transcriptEntryID(stateID, 2) {
		t.Fatalf("entries %s and %s sit at pinned slot IDs; a bd without --if-revision cannot pin a create", first.ID, second.ID)
	}
	lines := warnings.all()
	if len(lines) != 1 || !strings.Contains(lines[0], "without a store-level guard") || !strings.Contains(lines[0], "--if-revision") {
		t.Fatalf("warnings = %q, want one line saying the appends run unguarded because bd lacks --if-revision", lines)
	}
	if err := ProbeTranscriptSequencing(beadstest.Mail(store)); !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Fatalf("ProbeTranscriptSequencing over a released bd = %v, want ErrConditionalWriteUnsupported", err)
	}
}

// TestTranscriptAppendOverReleasedBdSurvivesALaggingHead pins that the
// capability probe decides before the atomic allocator reads anything. A head
// left behind its entry (a head write that failed, #7977's second repro) would
// send the allocator to publish that entry with a compare-and-set a released
// bd refuses, so every append to the conversation would fail. Unguarded, the
// append records its entry at the head.
func TestTranscriptAppendOverReleasedBdSurvivesALaggingHead(t *testing.T) {
	freezeTestClock(t)
	store := beadstest.NewFakeBd().NewBdStore()
	fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	captureTranscriptWarnings(t, fabric.Transcript)
	ref := testConversationRef()

	if _, err := appendTestTranscript(fabric.Transcript, ref, "msg-1", "one"); err != nil {
		t.Fatalf("Append(one): %v", err)
	}
	stateID := transcriptStateID(t, fabric.Transcript, ref)
	// The head write of "one" never landed.
	if err := store.SetMetadata(stateID, "next_sequence", "1"); err != nil {
		t.Fatalf("lowering the head: %v", err)
	}
	if _, err := appendTestTranscript(fabric.Transcript, ref, "msg-2", "two"); err != nil {
		t.Fatalf("Append(two) behind a lagging head over a released bd: %v; want it recorded unguarded", err)
	}
}

// unguardedTranscriptInput is an inbound append of text under providerMessageID.
func unguardedTranscriptInput(ref ConversationRef, providerMessageID, text string) AppendTranscriptInput {
	return AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: providerMessageID,
		Text:              text,
		CreatedAt:         testNow(),
	}
}

// TestTranscriptUnguardedRetryPublishesItsStoredEntry: over a store that
// appends unguarded, an append whose head write fails has still stored its
// entry, and the in-process retry finds that entry by its provider message ID.
// The retry writes the head past the entry before it answers, so the message
// it reports recorded is readable and the next message takes the next
// sequence. A repeat of a message the head already passed leaves the head
// where it is.
func TestTranscriptUnguardedRetryPublishesItsStoredEntry(t *testing.T) {
	freezeTestClock(t)
	fault := &transcriptFaultStore{Store: beads.NewMemStore()}
	store := storeOnlyWrapper{fault}
	svc := NewServices(beadstest.Mail(store), beadstest.Sessions(store)).Transcript
	captureTranscriptWarnings(t, svc)
	ref := testConversationRef()

	if _, err := appendTestTranscript(svc, ref, "msg-1", "one"); err != nil {
		t.Fatalf("Append(one): %v", err)
	}
	fault.setFaults(func(f *transcriptFaultStore) { f.failNextSequenceWrites = 1 })
	two, err := appendTranscriptRecovering(context.Background(), svc, unguardedTranscriptInput(ref, "msg-2", "two"))
	if err != nil {
		t.Fatalf("recovering Append(two) after one failed head write: %v; want the retry to publish the stored entry", err)
	}
	if two.Sequence != 2 {
		t.Fatalf("two.Sequence = %d, want 2", two.Sequence)
	}
	assertListedTexts(t, svc, ref, "one", "two")

	again, err := appendTestTranscript(svc, ref, "msg-1", "one")
	if err != nil {
		t.Fatalf("Append(one) again: %v", err)
	}
	if again.Sequence != 1 {
		t.Fatalf("repeat of one answered sequence %d, want 1", again.Sequence)
	}
	if _, err := appendTestTranscript(svc, ref, "msg-3", "three"); err != nil {
		t.Fatalf("Append(three): %v", err)
	}
	if stored := assertTranscriptSequencesUnique(t, fault); len(stored) != 3 {
		t.Fatalf("stored %d transcript entries, want 3", len(stored))
	}
	assertListedTexts(t, svc, ref, "one", "two", "three")
	fault.assertFrontierNeverDecreased(t)
}

// TestTranscriptUnguardedRetryReportsAFailedPublication: when the retry's own
// head write fails too, the append reports the failure instead of answering
// success for an entry no reader returns, and a later append of the same
// message publishes the stored entry without storing it again.
func TestTranscriptUnguardedRetryReportsAFailedPublication(t *testing.T) {
	freezeTestClock(t)
	fault := &transcriptFaultStore{Store: beads.NewMemStore()}
	store := storeOnlyWrapper{fault}
	svc := NewServices(beadstest.Mail(store), beadstest.Sessions(store)).Transcript
	captureTranscriptWarnings(t, svc)
	ref := testConversationRef()

	if _, err := appendTestTranscript(svc, ref, "msg-1", "one"); err != nil {
		t.Fatalf("Append(one): %v", err)
	}
	fault.setFaults(func(f *transcriptFaultStore) { f.failNextSequenceWrites = 2 })
	if entry, err := appendTranscriptRecovering(context.Background(), svc, unguardedTranscriptInput(ref, "msg-2", "two")); err == nil {
		t.Fatalf("recovering Append(two) answered %s at sequence %d while both head writes failed; want an error", entry.ID, entry.Sequence)
	}
	assertListedTexts(t, svc, ref, "one")

	two, err := appendTestTranscript(svc, ref, "msg-2", "two")
	if err != nil {
		t.Fatalf("Append(two) once the store recovered: %v", err)
	}
	if two.Sequence != 2 {
		t.Fatalf("two.Sequence = %d, want 2", two.Sequence)
	}
	if stored := assertTranscriptSequencesUnique(t, fault); len(stored) != 2 {
		t.Fatalf("stored %d transcript entries, want 2", len(stored))
	}
	assertListedTexts(t, svc, ref, "one", "two")
	fault.assertFrontierNeverDecreased(t)
}

// TestTranscriptAppendOverStoreThatCannotFenceWritesRunsUnguarded covers a
// store with both capabilities whose conditional-write probe fails: it
// refuses every fenced write, the head's compare-and-set included.
func TestTranscriptAppendOverStoreThatCannotFenceWritesRunsUnguarded(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	store.DisableConditionalWrites = true
	fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	warnings := captureTranscriptWarnings(t, fabric.Transcript)
	ref := testConversationRef()

	first, err := appendTestTranscript(fabric.Transcript, ref, "", "one")
	if err != nil {
		t.Fatalf("Append(one): %v; want it recorded unguarded", err)
	}
	if _, err := appendTestTranscript(fabric.Transcript, ref, "", "two"); err != nil {
		t.Fatalf("Append(two): %v", err)
	}
	assertListedTexts(t, fabric.Transcript, ref, "one", "two")
	if first.ID == transcriptEntryID(transcriptStateID(t, fabric.Transcript, ref), 1) {
		t.Fatalf("entry %s sits at its pinned slot ID; the unguarded append mints entry IDs", first.ID)
	}
	if lines := warnings.all(); len(lines) != 1 || !strings.Contains(lines[0], "conditional writes disabled") {
		t.Fatalf("warnings = %q, want one line carrying the store's probe reason", lines)
	}
}

// TestTranscriptAppendFallsBackWhenThePinnedCreateIsRefused covers a store
// that passes the probe but refuses the pinned create itself, before writing
// it: a bd whose version cannot be read, so its --id create cannot be proven
// insert-only. The refused create stored nothing, so the append runs
// unguarded instead of failing.
func TestTranscriptAppendFallsBackWhenThePinnedCreateIsRefused(t *testing.T) {
	freezeTestClock(t)
	fake := beadstest.NewFakeBd()
	fake.SetVersion("unknown")
	fake.AdvertiseConditionalWrites()
	store := fake.NewBdStore()
	if err := ProbeTranscriptSequencing(beadstest.Mail(store)); err != nil {
		t.Fatalf("ProbeTranscriptSequencing = %v; want the store to pass the probe, so the refusal comes from the create", err)
	}
	fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	warnings := captureTranscriptWarnings(t, fabric.Transcript)
	ref := testConversationRef()

	first, err := appendTestTranscript(fabric.Transcript, ref, "msg-1", "one")
	if err != nil {
		t.Fatalf("Append(one) over a refused pinned create: %v; want it recorded unguarded", err)
	}
	second, err := appendTestTranscript(fabric.Transcript, ref, "msg-2", "two")
	if err != nil {
		t.Fatalf("Append(two) over a refused pinned create: %v", err)
	}
	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("sequences = %d, %d; want 1, 2", first.Sequence, second.Sequence)
	}
	assertListedTexts(t, fabric.Transcript, ref, "one", "two")
	stateID := transcriptStateID(t, fabric.Transcript, ref)
	for _, sequence := range []int64{1, 2} {
		if _, err := fake.Bead(transcriptEntryID(stateID, sequence)); err == nil {
			t.Fatalf("the refused pinned create stored slot %d", sequence)
		}
	}
	if lines := warnings.all(); len(lines) != 1 || !strings.Contains(lines[0], beads.ErrPinnedCreateUnsupported.Error()) {
		t.Fatalf("warnings = %q, want one line carrying the pinned create's refusal", lines)
	}
}

// openFencedSQLiteBinding opens a store the way storebinding/sqlite's
// OpenEngine opens the binding of a whole split: minting gcg-, fenced to the
// five infrastructure namespaces.
func openFencedSQLiteBinding(t *testing.T) beads.Store {
	t.Helper()
	store, err := beads.OpenSQLiteStore(t.TempDir(),
		beads.WithSQLiteStoreIDPrefix("gcg"),
		beads.WithSQLiteStoreReservedIDPrefixes("gcg", "gcm", "gcs", "gco", "gcn"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := store.(interface{ CloseStore() error }); ok {
			_ = closer.CloseStore()
		}
	})
	return store
}

// migrateTranscriptRows copies every transcript state and entry of from into
// to with its ID preserved, as gc storage migrate carries the messaging class
// into a binding.
func migrateTranscriptRows(t *testing.T, from, to beads.Store) {
	t.Helper()
	creator, ok := to.(beads.ForeignIDCreator)
	if !ok {
		t.Fatalf("%T has no foreign-id create", to)
	}
	for _, label := range []string{labelTranscriptStateBase, labelTranscriptBase} {
		rows, err := from.List(beads.ListQuery{Label: label, IncludeClosed: true})
		if err != nil {
			t.Fatalf("List(%s): %v", label, err)
		}
		for _, row := range rows {
			if _, err := creator.CreateWithForeignID(row); err != nil {
				t.Fatalf("migrating %s: %v", row.ID, err)
			}
		}
	}
}

// TestTranscriptAppendToAMigratedConversationRunsUnguarded: gc storage
// migrate carries a conversation's transcript state into a fenced class
// binding under its work-store ID, so every pinned slot ID the state yields
// lies outside the binding's namespaces, and the store refuses each one before
// writing anything. Those appends run unguarded, minting their entries inside
// the binding, and the service logs that once; a conversation born in the
// binding still allocates at its pinned slot IDs.
func TestTranscriptAppendToAMigratedConversationRunsUnguarded(t *testing.T) {
	freezeTestClock(t)
	work := beads.NewMemStore()
	ref := testConversationRef()
	if _, err := appendTestTranscript(NewServices(beadstest.Mail(work), beadstest.Sessions(work)).Transcript, ref, "msg-1", "one"); err != nil {
		t.Fatalf("Append(one) in the work store: %v", err)
	}
	binding := openFencedSQLiteBinding(t)
	migrateTranscriptRows(t, work, binding)
	fabric := NewServices(beadstest.Mail(binding), beadstest.Sessions(binding))
	warnings := captureTranscriptWarnings(t, fabric.Transcript)
	stateID := transcriptStateID(t, fabric.Transcript, ref)
	if strings.HasPrefix(stateID, "gcg-") {
		t.Fatalf("migrated state ID %s sits inside the binding's namespaces; want the work store's ID", stateID)
	}

	two, err := appendTestTranscript(fabric.Transcript, ref, "msg-2", "two")
	if err != nil {
		t.Fatalf("Append(two) to the migrated conversation: %v; want it recorded unguarded", err)
	}
	three, err := appendTestTranscript(fabric.Transcript, ref, "msg-3", "three")
	if err != nil {
		t.Fatalf("Append(three) to the migrated conversation: %v", err)
	}
	for _, entry := range []ConversationTranscriptRecord{two, three} {
		if !strings.HasPrefix(entry.ID, "gcg-") || entry.ID == transcriptEntryID(stateID, entry.Sequence) {
			t.Fatalf("entry %s at sequence %d; want an ID the binding minted", entry.ID, entry.Sequence)
		}
	}
	assertListedTexts(t, fabric.Transcript, ref, "one", "two", "three")
	if lines := warnings.all(); len(lines) != 1 || !strings.Contains(lines[0], beads.ErrPinnedIDOutsideNamespace.Error()) {
		t.Fatalf("warnings = %q, want one line carrying the namespace fence's refusal", lines)
	}

	born := ref
	born.ConversationID = "thread-born-in-the-binding"
	first, err := appendTestTranscript(fabric.Transcript, born, "msg-4", "four")
	if err != nil {
		t.Fatalf("Append(four) to a conversation born in the binding: %v", err)
	}
	if want := transcriptEntryID(transcriptStateID(t, fabric.Transcript, born), 1); first.ID != want {
		t.Fatalf("entry ID = %s, want the pinned slot ID %s", first.ID, want)
	}
}

// TestTranscriptAppendOverACapableStoreIsAtomicAndQuiet: a store with both
// capabilities allocates at the pinned slot ID and logs nothing.
func TestTranscriptAppendOverACapableStoreIsAtomicAndQuiet(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	warnings := captureTranscriptWarnings(t, fabric.Transcript)
	ref := testConversationRef()

	first, err := appendTestTranscript(fabric.Transcript, ref, "msg-1", "one")
	if err != nil {
		t.Fatalf("Append(one): %v", err)
	}
	if want := transcriptEntryID(transcriptStateID(t, fabric.Transcript, ref), 1); first.ID != want {
		t.Fatalf("entry ID = %s, want the pinned slot ID %s", first.ID, want)
	}
	if lines := warnings.all(); len(lines) != 0 {
		t.Fatalf("warnings = %q, want none", lines)
	}
}

func TestProbeTranscriptSequencing(t *testing.T) {
	disabled := beads.NewMemStore()
	disabled.DisableConditionalWrites = true
	for _, tc := range []struct {
		name   string
		store  beads.Store
		want   error
		reason string
	}{
		{name: "a capable store", store: beads.NewMemStore()},
		{name: "a store that cannot fence writes", store: disabled, want: beads.ErrConditionalWriteUnsupported, reason: "conditional writes disabled"},
		{name: "a released bd", store: beadstest.NewFakeBd().NewBdStore(), want: beads.ErrConditionalWriteUnsupported, reason: "--if-revision"},
		{name: "a store without pinned creates", store: storeOnlyWrapper{beads.NewMemStore()}, want: beads.ErrPinnedCreateUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ProbeTranscriptSequencing(beadstest.Mail(tc.store))
			if tc.want == nil {
				if err != nil {
					t.Fatalf("ProbeTranscriptSequencing = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("ProbeTranscriptSequencing = %v, want %v carrying %q", err, tc.want, tc.reason)
			}
		})
	}
}

func TestHasTranscriptState(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	mail := beadstest.Mail(store)
	if has, err := HasTranscriptState(mail); err != nil || has {
		t.Fatalf("HasTranscriptState(empty store) = %v, %v; want false", has, err)
	}
	fabric := NewServices(mail, beadstest.Sessions(store))
	ref := testConversationRef()
	if _, err := appendTestTranscript(fabric.Transcript, ref, "msg-1", "one"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if has, err := HasTranscriptState(mail); err != nil || !has {
		t.Fatalf("HasTranscriptState after an append = %v, %v; want true", has, err)
	}
	if err := store.Close(transcriptStateID(t, fabric.Transcript, ref)); err != nil {
		t.Fatalf("closing the state: %v", err)
	}
	if has, err := HasTranscriptState(mail); err != nil || !has {
		t.Fatalf("HasTranscriptState with only a closed state = %v, %v; want true", has, err)
	}
}

// TestTranscriptAppendLogsARecoveredAmbiguousCreate: a create that stored the
// entry and reported an error is recovered from its slot, and logged.
func TestTranscriptAppendLogsARecoveredAmbiguousCreate(t *testing.T) {
	freezeTestClock(t)
	fault := &transcriptFaultStore{Store: beads.NewMemStore(), commitThenFailCreates: 1}
	svc := NewServices(beadstest.Mail(fault), beadstest.Sessions(fault)).Transcript
	warnings := captureTranscriptWarnings(t, svc)

	if _, err := appendTestTranscript(svc, testConversationRef(), "msg-1", "one"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if lines := warnings.all(); len(lines) != 1 || !strings.Contains(lines[0], "was stored by a create that reported invalid connection") {
		t.Fatalf("warnings = %q, want one line naming the recovered create's error", lines)
	}
}

// TestTranscriptAppendLogsAnExhaustedPublication: an append whose head
// writes fail past the publication budget returns a retryable error and logs
// the give-up.
func TestTranscriptAppendLogsAnExhaustedPublication(t *testing.T) {
	freezeTestClock(t)
	fault := &transcriptFaultStore{Store: beads.NewMemStore(), failNextSequenceWrites: 1000}
	svc := NewServices(beadstest.Mail(fault), beadstest.Sessions(fault)).Transcript
	warnings := captureTranscriptWarnings(t, svc)

	if _, err := appendTestTranscript(svc, testConversationRef(), "msg-1", "one"); !errors.Is(err, errTranscriptUnresolved) {
		t.Fatalf("Append with every head write failing = %v, want errTranscriptUnresolved", err)
	}
	want := fmt.Sprintf("gave up publishing through sequence 1 after %d failed compare-and-sets: next_sequence write failed", transcriptPublishAttempts)
	if lines := warnings.all(); len(lines) != 1 || !strings.Contains(lines[0], want) {
		t.Fatalf("warnings = %q, want one line containing %q", lines, want)
	}
}

// TestHandleOutboundLogsAPublishedMessageItCouldNotRecord: the transcript
// append after a publish is non-fatal, but a recording that fails is logged
// with the message and the conversation, since the message is never resent
// to record it.
func TestHandleOutboundLogsAPublishedMessageItCouldNotRecord(t *testing.T) {
	freezeTestClock(t)
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	fault := &transcriptFaultStore{Store: beads.NewMemStore()}
	fabric := NewServices(beadstest.Mail(fault), beadstest.Sessions(fault))
	ref := testConversationRef()
	adapter := newStubAdapter("stub", ref)
	reg := NewAdapterRegistry()
	reg.Register(AdapterKey{Provider: ref.Provider, AccountID: ref.AccountID}, adapter)
	deps := OutboundDeps{
		Services:               fabric,
		Registry:               reg,
		ResolveSessionSelector: agentTestResolver(map[string]string{"rig-a/helper": "sess-live-1"}),
	}
	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}

	fault.setFaults(func(f *transcriptFaultStore) { f.failNextSequenceWrites = 1000 })
	result, err := HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
		Conversation: ref,
		Text:         "published, not recorded",
	})
	if err != nil {
		t.Fatalf("HandleOutbound with a failing recording: %v; the publish succeeded", err)
	}
	if result.TranscriptEntry != nil {
		t.Fatalf("TranscriptEntry = %#v, want none for a failed recording", result.TranscriptEntry)
	}
	if len(adapter.publishs) != 1 {
		t.Fatalf("adapter publishes = %d, want 1", len(adapter.publishs))
	}
	want := fmt.Sprintf("extmsg: outbound message %q to %s was published but not recorded in its transcript", result.Receipt.MessageID, conversationTitle(ref))
	if !strings.Contains(logged.String(), want) {
		t.Fatalf("log = %q, want a line containing %q", logged.String(), want)
	}
}
