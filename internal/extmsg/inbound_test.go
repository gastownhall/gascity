package extmsg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// testInboundAdapter implements TransportAdapter for inbound tests.
type testInboundAdapter struct {
	name      string
	verifyFn  func(context.Context, InboundPayload) (*ExternalInboundMessage, error)
	publishFn func(context.Context, PublishRequest) (*PublishReceipt, error)
}

func (a *testInboundAdapter) Name() string                      { return a.name }
func (a *testInboundAdapter) Capabilities() AdapterCapabilities { return AdapterCapabilities{} }
func (a *testInboundAdapter) VerifyAndNormalizeInbound(ctx context.Context, p InboundPayload) (*ExternalInboundMessage, error) {
	if a.verifyFn != nil {
		return a.verifyFn(ctx, p)
	}
	return nil, ErrAdapterUnsupported
}

func (a *testInboundAdapter) Publish(ctx context.Context, req PublishRequest) (*PublishReceipt, error) {
	if a.publishFn != nil {
		return a.publishFn(ctx, req)
	}
	return nil, ErrAdapterUnsupported
}

func (a *testInboundAdapter) EnsureChildConversation(_ context.Context, _ ConversationRef, _ string) (*ConversationRef, error) {
	return nil, ErrAdapterUnsupported
}

func testConvRef() ConversationRef {
	return ConversationRef{
		ScopeID:        "scope1",
		Provider:       "discord",
		AccountID:      "bot-1",
		ConversationID: "chan-42",
		Kind:           ConversationRoom,
	}
}

func testAdapterKey() AdapterKey {
	return AdapterKey{Provider: "discord", AccountID: "bot-1"}
}

func testMsg() ExternalInboundMessage {
	return ExternalInboundMessage{
		ProviderMessageID: "msg-001",
		Conversation:      testConvRef(),
		Actor:             ExternalActor{ID: "user-1", DisplayName: "Alice"},
		Text:              "hello world",
		ReceivedAt:        time.Now(),
	}
}

//nolint:unparam // store returned for caller flexibility
func setupInboundDeps(t *testing.T) (InboundDeps, *AdapterRegistry, beads.Store) {
	t.Helper()
	store := beads.NewMemStore()
	svc := NewServices(store)
	reg := NewAdapterRegistry()
	deps := InboundDeps{
		Services: svc,
		Registry: reg,
		EmitEvent: func(_, _ string, _ map[string]any) {
			// no-op for tests
		},
	}
	return deps, reg, store
}

func TestHandleInboundWithBinding(t *testing.T) {
	deps, reg, _ := setupInboundDeps(t)
	ctx := context.Background()
	key := testAdapterKey()
	msg := testMsg()

	// Register adapter.
	reg.Register(key, &testInboundAdapter{
		name: "test-discord",
		verifyFn: func(_ context.Context, _ InboundPayload) (*ExternalInboundMessage, error) {
			return &msg, nil
		},
	})

	// Create a binding for the conversation.
	caller := Caller{Kind: CallerController, ID: "test"}
	_, err := deps.Services.Bindings.Bind(ctx, caller, BindInput{
		Conversation: msg.Conversation,
		SessionID:    "session-alpha",
		Now:          time.Now(),
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	// Process inbound.
	result, err := HandleInbound(ctx, deps, key, InboundPayload{Body: []byte("raw")})
	if err != nil {
		t.Fatalf("HandleInbound: %v", err)
	}

	if result.TargetSessionID != "session-alpha" {
		t.Fatalf("expected session-alpha, got %s", result.TargetSessionID)
	}
	if result.Binding == nil {
		t.Fatal("expected binding in result")
	}
	if result.TranscriptEntry == nil {
		t.Fatal("expected transcript entry")
	}
}

func TestHandleInboundWithGroupRoute(t *testing.T) {
	deps, reg, _ := setupInboundDeps(t)
	ctx := context.Background()
	key := testAdapterKey()
	msg := testMsg()

	reg.Register(key, &testInboundAdapter{
		name: "test-discord",
		verifyFn: func(_ context.Context, _ InboundPayload) (*ExternalInboundMessage, error) {
			return &msg, nil
		},
	})

	// Set up a group with a participant instead of a direct binding.
	caller := Caller{Kind: CallerController, ID: "test"}
	group, err := deps.Services.Groups.EnsureGroup(ctx, caller, EnsureGroupInput{
		RootConversation: msg.Conversation,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "worker",
	})
	if err != nil {
		t.Fatalf("ensure group: %v", err)
	}
	_, err = deps.Services.Groups.UpsertParticipant(ctx, caller, UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "worker",
		SessionID: "session-beta",
		Public:    true,
	})
	if err != nil {
		t.Fatalf("upsert participant: %v", err)
	}

	result, err := HandleInbound(ctx, deps, key, InboundPayload{Body: []byte("raw")})
	if err != nil {
		t.Fatalf("HandleInbound: %v", err)
	}

	if result.TargetSessionID != "session-beta" {
		t.Fatalf("expected session-beta, got %s", result.TargetSessionID)
	}
	if result.GroupRoute == nil {
		t.Fatal("expected group route in result")
	}
	if result.Binding != nil {
		t.Fatal("expected no binding when routed via group")
	}
}

func TestHandleInboundNoBindingNoGroup(t *testing.T) {
	deps, reg, _ := setupInboundDeps(t)
	ctx := context.Background()
	key := testAdapterKey()
	msg := testMsg()

	reg.Register(key, &testInboundAdapter{
		name: "test-discord",
		verifyFn: func(_ context.Context, _ InboundPayload) (*ExternalInboundMessage, error) {
			return &msg, nil
		},
	})

	// No binding, no group — should return result with empty target.
	result, err := HandleInbound(ctx, deps, key, InboundPayload{Body: []byte("raw")})
	if err != nil {
		t.Fatalf("HandleInbound: %v", err)
	}
	if result.TargetSessionID != "" {
		t.Fatalf("expected empty target, got %s", result.TargetSessionID)
	}
}

func TestHandleInboundAdapterVerificationFailure(t *testing.T) {
	deps, reg, _ := setupInboundDeps(t)
	ctx := context.Background()
	key := testAdapterKey()

	reg.Register(key, &testInboundAdapter{
		name: "test-discord",
		verifyFn: func(_ context.Context, _ InboundPayload) (*ExternalInboundMessage, error) {
			return nil, errors.New("invalid signature")
		},
	})

	_, err := HandleInbound(ctx, deps, key, InboundPayload{Body: []byte("bad")})
	if err == nil {
		t.Fatal("expected error for verification failure")
	}
}

func TestHandleInboundNoAdapter(t *testing.T) {
	deps, _, _ := setupInboundDeps(t)
	ctx := context.Background()

	_, err := HandleInbound(ctx, deps, AdapterKey{Provider: "missing", AccountID: "x"}, InboundPayload{})
	if err == nil {
		t.Fatal("expected error for missing adapter")
	}
}

func TestHandleInboundNilRegistry(t *testing.T) {
	deps := InboundDeps{Registry: nil}
	_, err := HandleInbound(context.Background(), deps, AdapterKey{}, InboundPayload{})
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
}

func TestHandleInboundNormalizedWithBinding(t *testing.T) {
	deps, _, _ := setupInboundDeps(t)
	ctx := context.Background()
	msg := testMsg()

	// Create binding.
	caller := Caller{Kind: CallerController, ID: "test"}
	_, err := deps.Services.Bindings.Bind(ctx, caller, BindInput{
		Conversation: msg.Conversation,
		SessionID:    "session-gamma",
		Now:          time.Now(),
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	result, err := HandleInboundNormalized(ctx, deps, msg)
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if result.TargetSessionID != "session-gamma" {
		t.Fatalf("expected session-gamma, got %s", result.TargetSessionID)
	}
}

func TestHandleInboundTranscriptVisibleToAllMembers(t *testing.T) {
	deps, reg, _ := setupInboundDeps(t)
	ctx := context.Background()
	key := testAdapterKey()
	msg := testMsg()
	caller := Caller{Kind: CallerController, ID: "test"}

	reg.Register(key, &testInboundAdapter{
		name: "test-discord",
		verifyFn: func(_ context.Context, _ InboundPayload) (*ExternalInboundMessage, error) {
			return &msg, nil
		},
	})

	// Create binding for session-alpha.
	_, err := deps.Services.Bindings.Bind(ctx, caller, BindInput{
		Conversation: msg.Conversation,
		SessionID:    "session-alpha",
		Now:          time.Now(),
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	// Add three members to the conversation transcript.
	for _, sid := range []string{"session-alpha", "session-beta", "session-gamma"} {
		_, err := deps.Services.Transcript.EnsureMembership(ctx, EnsureMembershipInput{
			Caller: caller, Conversation: msg.Conversation, SessionID: sid,
			BackfillPolicy: MembershipBackfillAll, Owner: MembershipOwnerGroup, Now: time.Now(),
		})
		if err != nil {
			t.Fatalf("membership %s: %v", sid, err)
		}
	}

	// Process inbound.
	result, err := HandleInbound(ctx, deps, key, InboundPayload{Body: []byte("hello everyone")})
	if err != nil {
		t.Fatalf("HandleInbound: %v", err)
	}

	if result.TargetSessionID != "session-alpha" {
		t.Fatalf("expected target session-alpha, got %s", result.TargetSessionID)
	}

	// All members should see the entry via ListBackfill.
	for _, sid := range []string{"session-alpha", "session-beta", "session-gamma"} {
		entries, err := deps.Services.Transcript.ListBackfill(ctx, ListBackfillInput{
			Caller: caller, Conversation: msg.Conversation, SessionID: sid, Limit: 10,
		})
		if err != nil {
			t.Fatalf("backfill %s: %v", sid, err)
		}
		if len(entries) == 0 {
			t.Fatalf("expected %s to see transcript entry via backfill", sid)
		}
	}
}
