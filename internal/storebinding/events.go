package storebinding

import "github.com/gastownhall/gascity/internal/events"

// StorageBindingOutcomePayload is the shape of every storage.binding.* event.
//
// One payload for four event types, because they report the same fact from the
// same place: what a process concluded about one binding, and what it did about
// it. A separate shape per outcome would make a consumer switch on the type to
// read the same three fields.
//
// It carries no bead counts. The gate that emits it at boot reads the marker,
// the manifest and — only when there is no marker — the source census; adding
// counts would make every boot pay for a full scan of two stores to decorate a
// notification. `gc storage status` reports the counts on demand, which is what
// a read-only surface is for.
type StorageBindingOutcomePayload struct {
	// Binding is the [storage.bindings.<name>] key the infrastructure classes
	// are assigned to.
	Binding string `json:"binding"`
	// Database is the resolved file the binding's engine opens.
	Database string `json:"database"`
	// Outcome names what was concluded: converged, genesis, unconverged, or
	// uncheckable.
	Outcome string `json:"outcome"`
	// Invariant is the operator-facing sentence a non-serving outcome carries,
	// empty when the binding is being served. It is the same text the refusal
	// printed, so a subscriber and a terminal never disagree about why a city
	// did not start.
	Invariant string `json:"invariant"`
}

// IsEventPayload marks StorageBindingOutcomePayload as an events.Payload
// variant.
func (StorageBindingOutcomePayload) IsEventPayload() {}

func init() {
	for _, eventType := range []string{
		events.StorageBindingConverged,
		events.StorageBindingGenesis,
		events.StorageBindingUnconverged,
		events.StorageBindingUncheckable,
	} {
		events.RegisterPayload(eventType, StorageBindingOutcomePayload{})
	}
}
