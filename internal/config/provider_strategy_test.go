package config

import "testing"

func TestRandomStrategySelectSingle(t *testing.T) {
	s := RandomStrategy{}
	got := s.Select([]string{"claude"})
	if got != "claude" {
		t.Errorf("Select([claude]) = %q, want %q", got, "claude")
	}
}

func TestRandomStrategySelectMultiple(t *testing.T) {
	s := RandomStrategy{}
	providers := []string{"claude", "gemini", "gpt"}
	seen := make(map[string]bool)
	// Run enough iterations that all providers should appear.
	for i := 0; i < 300; i++ {
		got := s.Select(providers)
		seen[got] = true
	}
	for _, p := range providers {
		if !seen[p] {
			t.Errorf("provider %q was never selected in 300 iterations", p)
		}
	}
}

func TestRandomStrategyImplementsInterface(_ *testing.T) {
	var _ ProviderStrategy = RandomStrategy{}
}

func TestLookupStrategyRandom(t *testing.T) {
	s, err := LookupStrategy("random")
	if err != nil {
		t.Fatalf("LookupStrategy(random): %v", err)
	}
	if _, ok := s.(RandomStrategy); !ok {
		t.Errorf("expected RandomStrategy, got %T", s)
	}
}

func TestLookupStrategyEmpty(t *testing.T) {
	s, err := LookupStrategy("")
	if err != nil {
		t.Fatalf("LookupStrategy(empty): %v", err)
	}
	if _, ok := s.(RandomStrategy); !ok {
		t.Errorf("expected RandomStrategy for empty name, got %T", s)
	}
}

func TestLookupStrategyUnknown(t *testing.T) {
	_, err := LookupStrategy("round-robin")
	if err == nil {
		t.Fatal("expected error for unknown strategy")
	}
}
