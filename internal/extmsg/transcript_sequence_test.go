package extmsg

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// ambiguousCreateStore commits the first transcript-entry Create and then
// reports a transport error, like a bd write whose commit landed before the
// connection dropped.
type ambiguousCreateStore struct {
	beads.Store
	failed bool
}

func (s *ambiguousCreateStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := s.Store.Create(b)
	if err != nil {
		return created, err
	}
	if !s.failed && slices.Contains(b.Labels, labelTranscriptBase) {
		s.failed = true
		return beads.Bead{}, errors.New("invalid connection")
	}
	return created, nil
}

func TestTranscriptAppendSequenceUniqueAfterAmbiguousCreate(t *testing.T) {
	freezeTestClock(t)
	store := &ambiguousCreateStore{Store: beads.NewMemStore()}
	svc := NewServices(beadstest.Mail(store), beadstest.Sessions(store)).Transcript
	ref := testConversationRef()

	appendMsg := func(id string) error {
		_, err := svc.Append(context.Background(), AppendTranscriptInput{
			Caller:            testAdapterCaller(),
			Conversation:      ref,
			Kind:              TranscriptMessageInbound,
			Provenance:        TranscriptProvenanceLive,
			ProviderMessageID: id,
			Text:              id,
			CreatedAt:         testNow(),
		})
		return err
	}
	if err := appendMsg("msg-1"); err == nil {
		t.Fatal("Append(msg-1) succeeded, want the injected ambiguous create error")
	}
	if err := appendMsg("msg-2"); err != nil {
		t.Fatalf("Append(msg-2): %v", err)
	}

	entries, err := store.List(beads.ListQuery{Label: labelTranscriptBase})
	if err != nil {
		t.Fatalf("List transcript entries: %v", err)
	}
	seqs := map[string]string{}
	bySeq := map[string][]string{}
	for _, b := range entries {
		id, seq := b.Metadata["provider_message_id"], b.Metadata["sequence"]
		seqs[id] = seq
		bySeq[seq] = append(bySeq[seq], id)
	}
	for seq, ids := range bySeq {
		if len(ids) > 1 {
			t.Errorf("duplicate transcript sequence #%s=%v (all entries: %v)", seq, ids, seqs)
		}
	}
}

// TestTranscriptPinnedAppendAdoptsLegacyMintedEntryAtLaggingHead pins the
// upgrade path over a legacy store: a pre-upgrade writer minted its entry
// directly through the store and lost the head bump, so a committed entry
// sits at the very sequence the state bead still names as next. With the old
// writers quiesced, an upgraded append must adopt that entry — publish it
// where it is, allocate the sequence above it — leaving both readable with
// the legacy entry's id, sequence and text untouched.
func TestTranscriptPinnedAppendAdoptsLegacyMintedEntryAtLaggingHead(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(beadstest.Mail(store), beadstest.Sessions(store)).Transcript
	ref := testConversationRef()
	ctx := context.Background()

	// The conversation's transcript state bead, head at sequence 1, as both
	// the legacy and the upgraded writers know it.
	state, err := store.Create(beads.Bead{
		Title: conversationTitle(ref) + "/state",
		Type:  "task",
		Labels: []string{
			"gc:extmsg-transcript-state", labelTranscriptStateBase, transcriptStateLabel(ref),
		},
		Metadata: map[string]string{
			"schema_version":              strconv.Itoa(schemaVersion),
			"scope_id":                    ref.ScopeID,
			"provider":                    ref.Provider,
			"account_id":                  ref.AccountID,
			"conversation_id":             ref.ConversationID,
			"parent_conversation_id":      ref.ParentConversationID,
			"conversation_kind":           string(ref.Kind),
			"next_sequence":               "1",
			"earliest_available_sequence": "1",
			"hydration_status":            string(HydrationLiveOnly),
			"max_retained_entries":        "0",
		},
	})
	if err != nil {
		t.Fatalf("create transcript state: %v", err)
	}

	// The legacy writer's entry: minted directly through the store at the
	// sequence the head still names, its head bump lost. Old writers are
	// quiesced from here on.
	legacy, err := store.Create(beads.Bead{
		Title:       conversationTitle(ref) + "#1",
		Type:        "task",
		Description: "written by the legacy writer",
		Labels: []string{
			"gc:extmsg-transcript", labelTranscriptBase,
			transcriptConversationLabel(ref), transcriptBucketLabel(ref, transcriptBucket(1)),
			transcriptProviderMessageLabel(ref, "legacy-1"),
		},
		Metadata: map[string]string{
			"schema_version":         strconv.Itoa(schemaVersion),
			"scope_id":               ref.ScopeID,
			"provider":               ref.Provider,
			"account_id":             ref.AccountID,
			"conversation_id":        ref.ConversationID,
			"parent_conversation_id": ref.ParentConversationID,
			"conversation_kind":      string(ref.Kind),
			"sequence":               "1",
			"kind":                   string(TranscriptMessageInbound),
			"provenance":             string(TranscriptProvenanceLive),
			"provider_message_id":    "legacy-1",
			"created_at":             formatTime(testNow()),
		},
	})
	if err != nil {
		t.Fatalf("create legacy transcript entry: %v", err)
	}

	appended, err := svc.Append(ctx, AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: "msg-new",
		Text:              "from the upgraded writer",
		CreatedAt:         testNow(),
	})
	if err != nil {
		t.Fatalf("Append(msg-new): %v", err)
	}
	if appended.Sequence != 2 {
		t.Fatalf("Append(msg-new) allocated sequence %d, want 2 above the legacy entry at 1", appended.Sequence)
	}
	if appended.ID == legacy.ID {
		t.Fatalf("the appended entry reused the legacy entry's id %s", legacy.ID)
	}

	// The legacy entry is adopted in place: same id, same sequence, same text
	// — no renumbering, no remint.
	stored, err := store.Get(legacy.ID)
	if err != nil {
		t.Fatalf("Get legacy entry: %v", err)
	}
	if got := stored.Metadata["sequence"]; got != "1" {
		t.Fatalf("legacy entry renumbered to sequence %q, want 1", got)
	}
	if got := stored.Metadata["provider_message_id"]; got != "legacy-1" {
		t.Fatalf("legacy entry provider message = %q, want legacy-1", got)
	}
	if stored.Description != "written by the legacy writer" {
		t.Fatalf("legacy entry text = %q, want it unchanged", stored.Description)
	}

	// Both entries stay readable, in order, with distinct sequences.
	records, err := svc.List(ctx, ListTranscriptInput{Caller: testControllerCaller(), Conversation: ref})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var got []string
	for _, r := range records {
		got = append(got, fmt.Sprintf("%d:%s:%s", r.Sequence, r.ProviderMessageID, r.ID))
	}
	want := []string{
		fmt.Sprintf("1:legacy-1:%s", legacy.ID),
		fmt.Sprintf("2:msg-new:%s", appended.ID),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("transcript = %v, want %v (both entries readable, ordered, unrenumbered)", got, want)
	}

	// The head now sits above both published entries.
	stateNow, err := store.Get(state.ID)
	if err != nil {
		t.Fatalf("Get transcript state: %v", err)
	}
	if got := stateNow.Metadata["next_sequence"]; got != "3" {
		t.Fatalf("next_sequence = %q, want 3 (the legacy entry published, then the append)", got)
	}
}
