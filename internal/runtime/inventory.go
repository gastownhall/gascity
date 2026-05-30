package runtime

import (
	"context"
	"strings"
	"time"
)

// InventoryObservation is a request-scoped read-model fact about a
// provider-owned runtime. Implementations should only populate fields that are
// cheap to learn while building the inventory.
type InventoryObservation struct {
	SessionName       string
	Running           bool
	Alive             bool
	AliveKnown        bool
	Attached          bool
	AttachedKnown     bool
	LastActivity      time.Time
	LastActivityKnown bool
	Source            string
}

// Inventory is a batched provider snapshot for list/status read paths.
// When Complete is true, a missing session name is known not to be running.
// When Complete is false, callers should treat missing names as unknown.
type Inventory struct {
	Complete     bool
	Source       string
	Observations map[string]InventoryObservation
}

// InventoryProvider is an optional provider extension for efficient
// batched runtime observation.
type InventoryProvider interface {
	Inventory(ctx context.Context, prefix string) (Inventory, error)
}

// ObserveInventory returns a provider inventory when the runtime
// implements the optional batched observation contract.
func ObserveInventory(ctx context.Context, sp Provider, prefix string) (Inventory, bool, error) {
	inventoryProvider, ok := sp.(InventoryProvider)
	if !ok || inventoryProvider == nil {
		return Inventory{}, false, nil
	}
	inventory, err := inventoryProvider.Inventory(ctx, prefix)
	if inventory.Observations == nil {
		inventory.Observations = map[string]InventoryObservation{}
	}
	return inventory, true, err
}

// Observe returns the named runtime observation. If the inventory is complete,
// a missing name returns a known stopped observation.
func (i Inventory) Observe(name string) (InventoryObservation, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return InventoryObservation{}, false
	}
	if i.Observations != nil {
		if obs, ok := i.Observations[name]; ok {
			return obs, true
		}
	}
	if i.Complete {
		return InventoryObservation{SessionName: name, Source: i.Source}, true
	}
	return InventoryObservation{}, false
}

// RunningKnown reports whether the inventory can answer liveness for name.
func (i Inventory) RunningKnown(name string) (bool, bool) {
	obs, ok := i.Observe(name)
	if !ok {
		return false, false
	}
	return obs.Running, true
}
