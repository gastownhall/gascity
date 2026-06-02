package main

import (
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Provider-health is a generic, bead-backed signal that lets the reconciler
// avoid respawning sessions onto a provider that is currently unable to serve
// requests (for example, a provider whose credentials have been revoked). The
// SDK does NOT detect provider health itself — detection is provider-specific
// and belongs to the consumer layer, which publishes health by writing a
// provider-health bead through the normal bead API. The reconciler only reads
// that bead and gates fresh respawns on it. This keeps all provider-specific
// behavior out of the SDK while still letting the controller protect the fleet
// from a thundering respawn into a dead provider.
//
// Contract (consumer-written, SDK-read):
//   - Label:    providerHealthLabel on every provider-health bead.
//   - Metadata: providerHealthProviderKey = the provider name (matching the
//     agent's configured provider), providerHealthStatusKey = one of
//     providerHealthStatusHealthy / providerHealthStatusUnhealthy.
//   - A bead whose status is closed is treated as resolved (healthy again).
//
// The signal is fail-open: a provider is considered unhealthy ONLY when an open
// bead positively marks it so. Unknown providers, an empty store, or a read
// error all resolve to healthy, so a missing or misconfigured signal can never
// wedge respawns.
const (
	providerHealthLabel           = "gc.provider-health"
	providerHealthProviderKey     = "gc.provider"
	providerHealthStatusKey       = "gc.health"
	providerHealthStatusHealthy   = "healthy"
	providerHealthStatusUnhealthy = "unhealthy"
)

// providerHealthLister is the narrow slice of beads.Store that the
// provider-health snapshot needs. Narrowing the dependency keeps the loader
// trivially testable with a stub and documents that it only reads.
type providerHealthLister interface {
	ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error)
}

// providerHealthSnapshot maps a provider name to its current health. A provider
// absent from the map is healthy (fail-open); see loadProviderHealthSnapshot.
type providerHealthSnapshot map[string]bool

// healthy reports whether sessions may be respawned onto the named provider.
// The empty provider name and any provider with no recorded signal are healthy,
// so the gate only ever blocks providers positively marked unhealthy.
func (s providerHealthSnapshot) healthy(provider string) bool {
	if provider == "" {
		return true
	}
	h, ok := s[provider]
	if !ok {
		return true
	}
	return h
}

// loadProviderHealthSnapshot reads every provider-health bead once and projects
// the latest state per provider. When multiple beads exist for one provider the
// most recently updated wins (falling back to creation time), matching the
// consumer overwriting a single per-provider bead as health flips. A nil lister
// or a read error yields an all-healthy snapshot so respawns are never wedged by
// a missing or failing signal.
func loadProviderHealthSnapshot(lister providerHealthLister) (providerHealthSnapshot, error) {
	snap := providerHealthSnapshot{}
	if lister == nil {
		return snap, nil
	}
	healthBeads, err := lister.ListByLabel(providerHealthLabel, 0)
	if err != nil {
		return snap, err
	}
	// Newest-first so the first bead seen for each provider is authoritative.
	sort.SliceStable(healthBeads, func(i, j int) bool {
		return providerHealthBeadTime(healthBeads[i]).After(providerHealthBeadTime(healthBeads[j]))
	})
	for i := range healthBeads {
		bead := healthBeads[i]
		provider := bead.Metadata[providerHealthProviderKey]
		if provider == "" {
			continue
		}
		if _, seen := snap[provider]; seen {
			continue
		}
		// A closed bead means the consumer resolved the condition: healthy.
		unhealthy := bead.Status != "closed" &&
			bead.Metadata[providerHealthStatusKey] == providerHealthStatusUnhealthy
		snap[provider] = !unhealthy
	}
	return snap, nil
}

// providerHealthBeadTime returns the bead's most recent timestamp for ordering,
// falling back to CreatedAt when UpdatedAt is zero (legacy beads).
func providerHealthBeadTime(b beads.Bead) time.Time {
	if !b.UpdatedAt.IsZero() {
		return b.UpdatedAt
	}
	return b.CreatedAt
}

// agentProviderName returns the provider preset an agent resolves to,
// following config precedence: the agent's own provider, then its pack-inherited
// default, then the city-level [agent_defaults] and [workspace] defaults. The
// result is the key the provider-health snapshot is consulted with. Empty when
// no provider is configured anywhere, in which case the health gate is a no-op
// (fail-open).
func agentProviderName(cfg *config.City, a *config.Agent) string {
	if a != nil {
		if a.Provider != "" {
			return a.Provider
		}
		if a.InheritedProvider != "" {
			return a.InheritedProvider
		}
	}
	if cfg != nil {
		if cfg.AgentDefaults.Provider != "" {
			return cfg.AgentDefaults.Provider
		}
		return cfg.Workspace.Provider
	}
	return ""
}
