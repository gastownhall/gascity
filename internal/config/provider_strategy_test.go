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

func TestRandomStrategyImplementsInterface(t *testing.T) {
	var _ ProviderStrategy = RandomStrategy{}
}
