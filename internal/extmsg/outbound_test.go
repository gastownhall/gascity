package extmsg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

//nolint:unparam // store returned for caller flexibility
func setupOutboundDeps(t *testing.T) (OutboundDeps, *AdapterRegistry, beads.Store) {
	t.Helper()
	store := beads.NewMemStore()
	svc := NewServices(store)
	reg := NewAdapterRegistry()
	deps := OutboundDeps{
		Services: svc,
		Registry: reg,
		EmitEvent: func(_, _ string, _ map[string]any) {
			// no-op for tests
		},
	}
	return deps, reg, store
}

//nolint:unparam // name kept as parameter for test readability
func successPublishAdapter(name string) *testInboundAdapter {
	return &testInboundAdapter{
		name: name,
		publishFn: func(_ context.Context, req PublishRequest) (*PublishReceipt, error) {
			return &PublishReceipt{
				MessageID:    "out-msg-001",
				Conversation: req.Conversation,
				Delivered:    true,
			}, nil
		},
	}
}

func TestHandleOutboundHappyPath(t *testing.T) {
	deps, reg, _ := setupOutboundDeps(t)
	ctx := context.Background()
	conv := testConvRef()
	key := testAdapterKey()
	caller := Caller{Kind: CallerController, ID: "test"}

	// Register adapter.
	reg.Register(key, successPublishAdapter("test-discord"))

	// Create binding.
	_, err := deps.Services.Bindings.Bind(ctx, caller, BindInput{
		Conversation: conv,
		SessionID:    "session-alpha",
		Now:          time.Now(),
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	// Publish.
	result, err := HandleOutbound(ctx, deps, caller, OutboundRequest{
		SessionID:    "session-alpha",
		Conversation: conv,
		Text:         "hello external world",
	})
	if err != nil {
		t.Fatalf("HandleOutbound: %v", err)
	}
	if !result.Receipt.Delivered {
		t.Fatal("expected delivered=true")
	}
	if result.Receipt.MessageID != "out-msg-001" {
		t.Fatalf("expected out-msg-001, got %s", result.Receipt.MessageID)
	}
	if result.TranscriptEntry == nil {
		t.Fatal("expected transcript entry")
	}
}

func TestHandleOutboundNoBinding(t *testing.T) {
	deps, reg, _ := setupOutboundDeps(t)
	ctx := context.Background()
	conv := testConvRef()
	key := testAdapterKey()
	caller := Caller{Kind: CallerController, ID: "test"}

	reg.Register(key, successPublishAdapter("test-discord"))

	// No binding — should fail.
	_, err := HandleOutbound(ctx, deps, caller, OutboundRequest{
		SessionID:    "session-alpha",
		Conversation: conv,
		Text:         "hello",
	})
	if err == nil {
		t.Fatal("expected error for no binding")
	}
}

func TestHandleOutboundWrongSession(t *testing.T) {
	deps, reg, _ := setupOutboundDeps(t)
	ctx := context.Background()
	conv := testConvRef()
	key := testAdapterKey()
	caller := Caller{Kind: CallerController, ID: "test"}

	reg.Register(key, successPublishAdapter("test-discord"))

	// Bind to session-alpha.
	_, err := deps.Services.Bindings.Bind(ctx, caller, BindInput{
		Conversation: conv,
		SessionID:    "session-alpha",
		Now:          time.Now(),
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	// Try to publish from session-beta — should fail.
	_, err = HandleOutbound(ctx, deps, caller, OutboundRequest{
		SessionID:    "session-beta",
		Conversation: conv,
		Text:         "hello",
	})
	if err == nil {
		t.Fatal("expected error for wrong session")
	}
}

func TestHandleOutboundNoAdapter(t *testing.T) {
	deps, _, _ := setupOutboundDeps(t)
	ctx := context.Background()
	conv := testConvRef()
	caller := Caller{Kind: CallerController, ID: "test"}

	// Bind but don't register adapter.
	_, err := deps.Services.Bindings.Bind(ctx, caller, BindInput{
		Conversation: conv,
		SessionID:    "session-alpha",
		Now:          time.Now(),
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	_, err = HandleOutbound(ctx, deps, caller, OutboundRequest{
		SessionID:    "session-alpha",
		Conversation: conv,
		Text:         "hello",
	})
	if err == nil {
		t.Fatal("expected error for missing adapter")
	}
}

func TestHandleOutboundPublishFailure(t *testing.T) {
	deps, reg, _ := setupOutboundDeps(t)
	ctx := context.Background()
	conv := testConvRef()
	key := testAdapterKey()
	caller := Caller{Kind: CallerController, ID: "test"}

	// Register adapter that fails.
	reg.Register(key, &testInboundAdapter{
		name: "failing-adapter",
		publishFn: func(_ context.Context, _ PublishRequest) (*PublishReceipt, error) {
			return nil, errors.New("network timeout")
		},
	})

	_, err := deps.Services.Bindings.Bind(ctx, caller, BindInput{
		Conversation: conv,
		SessionID:    "session-alpha",
		Now:          time.Now(),
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	_, err = HandleOutbound(ctx, deps, caller, OutboundRequest{
		SessionID:    "session-alpha",
		Conversation: conv,
		Text:         "hello",
	})
	if err == nil {
		t.Fatal("expected error for publish failure")
	}
}

func TestHandleOutboundNotDelivered(t *testing.T) {
	deps, reg, _ := setupOutboundDeps(t)
	ctx := context.Background()
	conv := testConvRef()
	key := testAdapterKey()
	caller := Caller{Kind: CallerController, ID: "test"}

	// Adapter returns receipt with Delivered=false.
	reg.Register(key, &testInboundAdapter{
		name: "rate-limited",
		publishFn: func(_ context.Context, req PublishRequest) (*PublishReceipt, error) {
			return &PublishReceipt{
				Conversation: req.Conversation,
				Delivered:    false,
				FailureKind:  PublishFailureRateLimited,
				RetryAfter:   5 * time.Second,
			}, nil
		},
	})

	_, err := deps.Services.Bindings.Bind(ctx, caller, BindInput{
		Conversation: conv,
		SessionID:    "session-alpha",
		Now:          time.Now(),
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	result, err := HandleOutbound(ctx, deps, caller, OutboundRequest{
		SessionID:    "session-alpha",
		Conversation: conv,
		Text:         "hello",
	})
	if err != nil {
		t.Fatalf("HandleOutbound: %v", err)
	}
	if result.Receipt.Delivered {
		t.Fatal("expected delivered=false")
	}
	if result.Receipt.FailureKind != PublishFailureRateLimited {
		t.Fatalf("expected rate_limited, got %s", result.Receipt.FailureKind)
	}
	// No transcript or delivery context when not delivered.
	if result.TranscriptEntry != nil {
		t.Fatal("expected no transcript entry when not delivered")
	}
}

func TestHandleOutboundNilRegistry(t *testing.T) {
	deps := OutboundDeps{Registry: nil}
	caller := Caller{Kind: CallerController, ID: "test"}
	_, err := HandleOutbound(context.Background(), deps, caller, OutboundRequest{})
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
}

func TestHandleOutboundTranscriptVisibleToPeers(t *testing.T) {
	deps, reg, _ := setupOutboundDeps(t)
	ctx := context.Background()
	conv := testConvRef()
	key := testAdapterKey()
	caller := Caller{Kind: CallerController, ID: "test"}

	reg.Register(key, successPublishAdapter("test-discord"))

	// Bind and add memberships.
	_, err := deps.Services.Bindings.Bind(ctx, caller, BindInput{
		Conversation: conv, SessionID: "session-alpha", Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	for _, sid := range []string{"session-alpha", "session-beta"} {
		_, err := deps.Services.Transcript.EnsureMembership(ctx, EnsureMembershipInput{
			Caller: caller, Conversation: conv, SessionID: sid,
			BackfillPolicy: MembershipBackfillAll, Owner: MembershipOwnerGroup, Now: time.Now(),
		})
		if err != nil {
			t.Fatalf("membership %s: %v", sid, err)
		}
	}

	// Publish from session-alpha.
	_, err = HandleOutbound(ctx, deps, caller, OutboundRequest{
		SessionID: "session-alpha", Conversation: conv, Text: "hello peers",
	})
	if err != nil {
		t.Fatalf("HandleOutbound: %v", err)
	}

	// session-beta should see the entry via ListBackfill.
	entries, err := deps.Services.Transcript.ListBackfill(ctx, ListBackfillInput{
		Caller: caller, Conversation: conv, SessionID: "session-beta", Limit: 10,
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected session-beta to see outbound entry via transcript backfill")
	}
}
