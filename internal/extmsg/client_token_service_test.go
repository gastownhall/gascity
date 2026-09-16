package extmsg

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestClientRegister_Idempotent(t *testing.T) {
	store := beads.NewMemStore()

	first, err := RegisterClient(t.Context(), store, RegisterClientInput{
		Credential:        "my-api-key",
		AllowedSessions:   []string{"mayor"},
		AllowNoCredential: false,
	})
	if err != nil {
		t.Fatalf("RegisterClient(first): %v", err)
	}
	if !first.Created {
		t.Fatal("first registration: Created = false, want true")
	}
	if first.Token == "" {
		t.Fatal("first registration: Token is empty")
	}
	if first.ClientID == "" {
		t.Fatal("first registration: ClientID is empty")
	}

	// Second call with same credential returns the same bead, no token.
	second, err := RegisterClient(t.Context(), store, RegisterClientInput{
		Credential:        "my-api-key",
		AllowedSessions:   []string{"mayor"},
		AllowNoCredential: false,
	})
	if err != nil {
		t.Fatalf("RegisterClient(second): %v", err)
	}
	if second.Created {
		t.Fatal("second registration: Created = true, want false (idempotent)")
	}
	if second.Token != "" {
		t.Fatalf("second registration: Token should be empty on re-issuance, got %q", second.Token)
	}
	if second.ClientID != first.ClientID {
		t.Fatalf("second registration: ClientID %q != first %q", second.ClientID, first.ClientID)
	}
}

func TestClientRegister_NoCredentialRejected(t *testing.T) {
	store := beads.NewMemStore()

	_, err := RegisterClient(t.Context(), store, RegisterClientInput{
		Credential:        "",
		AllowNoCredential: false,
	})
	if err == nil {
		t.Fatal("RegisterClient with empty credential and allow_no_credential=false: expected error, got nil")
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestClientRegister_AllowNoCredential(t *testing.T) {
	store := beads.NewMemStore()

	result, err := RegisterClient(t.Context(), store, RegisterClientInput{
		Credential:        "",
		AllowNoCredential: true,
		AllowedSessions:   []string{"mayor"},
	})
	if err != nil {
		t.Fatalf("RegisterClient with allow_no_credential=true: %v", err)
	}
	if !result.Created {
		t.Fatal("Created = false, want true")
	}
	if result.Token == "" {
		t.Fatal("Token is empty")
	}
}

func TestResolveClientToken_ValidToken(t *testing.T) {
	store := beads.NewMemStore()

	reg, err := RegisterClient(t.Context(), store, RegisterClientInput{
		Credential:        "secret-key",
		AllowedSessions:   []string{"mayor", "iris"},
		AllowNoCredential: false,
	})
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}

	clientID, sessions, err := ResolveClientToken(t.Context(), store, reg.Token)
	if err != nil {
		t.Fatalf("ResolveClientToken: %v", err)
	}
	if clientID != reg.ClientID {
		t.Fatalf("clientID = %q, want %q", clientID, reg.ClientID)
	}
	if len(sessions) != 2 {
		t.Fatalf("allowedSessions = %v, want [mayor iris]", sessions)
	}
}

func TestResolveClientToken_UnknownToken(t *testing.T) {
	store := beads.NewMemStore()

	_, _, err := ResolveClientToken(t.Context(), store, "unknown-token-value")
	if err == nil {
		t.Fatal("ResolveClientToken with unknown token: expected error, got nil")
	}
	if !errors.Is(err, ErrClientTokenNotFound) {
		t.Fatalf("expected ErrClientTokenNotFound, got %v", err)
	}
}

func TestResolveClientToken_EmptyTokenRejected(t *testing.T) {
	store := beads.NewMemStore()

	_, _, err := ResolveClientToken(t.Context(), store, "")
	if err == nil {
		t.Fatal("expected error for empty token, got nil")
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}
