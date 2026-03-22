package config

import "math/rand"

// ProviderStrategy selects a provider name from a list of candidates.
// Implementations define the selection policy (random, round-robin, etc.).
type ProviderStrategy interface {
	// Select picks one provider name from the given list.
	// The list is guaranteed to be non-empty by the caller.
	Select(providers []string) string
}

// RandomStrategy selects a provider uniformly at random.
type RandomStrategy struct{}

// Select picks a random provider from the list.
func (s RandomStrategy) Select(providers []string) string {
	if len(providers) == 1 {
		return providers[0]
	}
	return providers[rand.Intn(len(providers))]
}
