package extmsg

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

type attributionAdapter struct {
	TransportAdapter
	message ExternalInboundMessage
}

func (a attributionAdapter) Name() string { return "attribution-test" }

func (a attributionAdapter) VerifyAndNormalizeInbound(context.Context, InboundPayload) (*ExternalInboundMessage, error) {
	return &a.message, nil
}

func TestInboundAttributionRoundTrip(t *testing.T) {
	for _, raw := range []bool{false, true} {
		for name, attribution := range map[string]map[string]string{
			"absent": nil,
			"present": {
				"app_id": "A123", "bot_id": "B123", "bot_profile_app_id": "A456",
				"composer_user_id": "U123", "adapter-defined": "  unchanged\nvalue  ",
			},
		} {
			t.Run(fmtInboundCase(raw, name), func(t *testing.T) {
				ctx := context.Background()
				store := beads.NewMemStore()
				services := NewServices(store)
				ref := testConversationRef()
				if _, err := services.Bindings.Bind(ctx, testControllerCaller(), BindInput{
					Conversation: ref, SessionID: "sess-a", Now: testNow(),
				}); err != nil {
					t.Fatal(err)
				}
				msg := ExternalInboundMessage{
					Conversation: ref, ProviderMessageID: "message-1",
					Actor: ExternalActor{ID: "user-1", DisplayName: "Sender"},
					Text:  "hello", ReceivedAt: testNow(), Attribution: maps.Clone(attribution),
				}
				deps := InboundDeps{Services: services, Registry: NewAdapterRegistry()}
				var result *InboundResult
				var err error
				if raw {
					key := AdapterKey{Provider: ref.Provider, AccountID: ref.AccountID}
					deps.Registry.Register(key, attributionAdapter{message: msg})
					result, err = HandleInbound(ctx, deps, key, InboundPayload{})
				} else {
					result, err = HandleInboundNormalized(ctx, deps, msg)
				}
				if err != nil {
					t.Fatal(err)
				}
				if result.TranscriptEntry == nil || !maps.Equal(result.TranscriptEntry.Attribution, attribution) {
					t.Fatalf("inbound result = %#v, want attribution %v", result, attribution)
				}
				// Read through a new service so this proves persistence, not an
				// in-memory alias of the adapter's message.
				services = NewServices(store)
				clear(msg.Attribution)
				entries, err := services.Transcript.List(ctx, ListTranscriptInput{
					Caller: testControllerCaller(), Conversation: ref, ProviderMessageID: "message-1",
				})
				if err != nil || len(entries) != 1 {
					t.Fatalf("lookup = %#v, %v", entries, err)
				}
				if !maps.Equal(entries[0].Attribution, attribution) {
					t.Fatalf("persisted attribution = %v, want %v", entries[0].Attribution, attribution)
				}
				wire, err := json.Marshal(entries[0])
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.Contains(string(wire), `"attribution":`); got != (len(attribution) > 0) {
					t.Fatalf("optional attribution wire = %s", wire)
				}
				// Redelivery cannot replace the original adapter assertion.
				again, err := HandleInboundNormalized(ctx, deps, ExternalInboundMessage{
					Conversation: ref, ProviderMessageID: "message-1", Text: "replacement",
					Attribution: map[string]string{"app_id": "replacement"},
				})
				if err != nil || again.TranscriptEntry == nil || !maps.Equal(again.TranscriptEntry.Attribution, attribution) {
					t.Fatalf("redelivery changed assertion: %#v, %v", again, err)
				}
			})
		}
	}
}

func fmtInboundCase(raw bool, name string) string {
	if raw {
		return "raw/" + name
	}
	return "normalized/" + name
}

func TestTranscriptLookupByProviderMessageID(t *testing.T) {
	ctx := context.Background()
	store := beads.NewMemStore()
	services := NewServices(store)
	ref := testConversationRef()
	for _, account := range []string{ref.AccountID, "another-account"} {
		other := ref
		other.AccountID = account
		for _, id := range []string{"first", "wanted", "last"} {
			if _, err := services.Transcript.Append(ctx, AppendTranscriptInput{
				Caller: testControllerCaller(), Conversation: other,
				Kind: TranscriptMessageInbound, ProviderMessageID: id, Text: account + "/" + id,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	input := ListTranscriptInput{
		Caller: testControllerCaller(), Conversation: ref, ProviderMessageID: "wanted",
		Limit: 1, Order: TranscriptOrderDesc,
	}
	got, err := services.Transcript.List(ctx, input)
	if err != nil || len(got) != 1 || got[0].Text != ref.AccountID+"/wanted" {
		t.Fatalf("scoped exact lookup = %#v, %v", got, err)
	}
	input.ProviderMessageID = "missing"
	got, err = services.Transcript.List(ctx, input)
	if err != nil || len(got) != 0 {
		t.Fatalf("missing lookup = %#v, %v", got, err)
	}
	input.ProviderMessageID = "wanted"
	input.AfterSequence = 2
	got, err = services.Transcript.List(ctx, input)
	if err != nil || len(got) != 0 {
		t.Fatalf("lookup before cursor = %#v, %v", got, err)
	}
	input.Caller = testAdapterCaller()
	if _, err := services.Transcript.List(ctx, input); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("adapter lookup error = %v, want ErrUnauthorized", err)
	}
}

func TestTranscriptCorruptAttributionReturnsError(t *testing.T) {
	ctx := context.Background()
	store := beads.NewMemStore()
	services := NewServices(store)
	ref := testConversationRef()
	entry, err := services.Transcript.Append(ctx, AppendTranscriptInput{
		Caller: testControllerCaller(), Conversation: ref,
		Kind: TranscriptMessageInbound, ProviderMessageID: "message-1",
		Attribution: map[string]string{"app_id": "A123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(entry.ID, "attribution_json", "{"); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Transcript.List(ctx, ListTranscriptInput{
		Caller: testControllerCaller(), Conversation: ref, ProviderMessageID: "message-1",
	}); err == nil || !strings.Contains(err.Error(), "attribution") {
		t.Fatalf("corrupt attribution error = %v", err)
	}
}
