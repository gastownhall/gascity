package mail

import "testing"

func TestResolveRecipientHuman(t *testing.T) {
	got, err := ResolveRecipient("human", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "human" {
		t.Errorf("got %q, want %q", got, "human")
	}
}

func TestResolveRecipientEmpty(t *testing.T) {
	_, err := ResolveRecipient("", nil)
	if err == nil {
		t.Fatal("expected error for empty recipient")
	}
}

func TestResolveRecipientQualifiedMatch(t *testing.T) {
	agents := []AgentEntry{
		{Dir: "corp", Name: "maya"},
		{Dir: "corp", Name: "sky"},
	}
	got, err := ResolveRecipient("corp/maya", agents)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "corp/maya" {
		t.Errorf("got %q, want %q", got, "corp/maya")
	}
}

func TestResolveRecipientQualifiedNotFound(t *testing.T) {
	agents := []AgentEntry{
		{Dir: "corp", Name: "maya"},
	}
	_, err := ResolveRecipient("ops/maya", agents)
	if err == nil {
		t.Fatal("expected error for unknown qualified name")
	}
}

func TestResolveRecipientBareUnambiguous(t *testing.T) {
	agents := []AgentEntry{
		{Dir: "corp", Name: "maya"},
		{Dir: "corp", Name: "sky"},
	}
	got, err := ResolveRecipient("maya", agents)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "corp/maya" {
		t.Errorf("got %q, want %q", got, "corp/maya")
	}
}

func TestResolveRecipientBareCityScoped(t *testing.T) {
	agents := []AgentEntry{
		{Dir: "", Name: "mayor"},
		{Dir: "corp", Name: "sky"},
	}
	got, err := ResolveRecipient("mayor", agents)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "mayor" {
		t.Errorf("got %q, want %q", got, "mayor")
	}
}

func TestResolveRecipientBareAmbiguous(t *testing.T) {
	agents := []AgentEntry{
		{Dir: "corp", Name: "maya"},
		{Dir: "ops", Name: "maya"},
	}
	_, err := ResolveRecipient("maya", agents)
	if err == nil {
		t.Fatal("expected error for ambiguous bare name")
	}
	if got := err.Error(); got != `ambiguous recipient "maya": matches corp/maya, ops/maya` {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveRecipientBareNotFound(t *testing.T) {
	agents := []AgentEntry{
		{Dir: "corp", Name: "sky"},
	}
	_, err := ResolveRecipient("maya", agents)
	if err == nil {
		t.Fatal("expected error for unknown bare name")
	}
}
