package config

import (
	"fmt"
	"math/rand"
)

// ProviderStrategy selects a provider name from a list of candidates.
// Implementations define the selection policy (random, round-robin, etc.).
type ProviderStrategy interface {
	// Select picks one provider name from the given list.
	// The list is guaranteed to be non-empty by the caller.
	Select(providers []string) string
}

// ErrUnknownStrategy indicates the strategy name is not recognized.
var ErrUnknownStrategy = fmt.Errorf("unknown provider strategy")

// LookupStrategy returns the ProviderStrategy for the given name.
// Currently supported: "random" (default when name is empty).
func LookupStrategy(name string) (ProviderStrategy, error) {
	switch name {
	case "", "random":
		return RandomStrategy{}, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownStrategy, name)
	}
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
