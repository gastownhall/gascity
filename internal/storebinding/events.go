package storebinding

import "github.com/gastownhall/gascity/internal/events"

// StorageBindingOutcomePayload is the shape of every storage.binding.* event.
//
// One payload for every storage.binding.* type, because they report the same
// fact from the same place: what a process concluded about one binding, and what
// it did about it. A separate shape per outcome would make a consumer switch on
// the type to read the same fields.
//
// It carries no census. The gate that emits it at boot reads the marker, the
// manifest and — only when there is no marker — the source rows; making it
// report what the source and the binding hold right now would put a full scan of
// two stores on every boot to decorate a notification. `gc storage status`
// reports those counts on demand, which is what a read-only surface is for.
//
// ProvenBeads is the one number that is free, and it is here for that reason.
type StorageBindingOutcomePayload struct {
	// Binding is the [storage.bindings.<name>] key the infrastructure classes
	// are assigned to.
	Binding string `json:"binding"`
	// Database is the resolved file the binding's engine opens.
	Database string `json:"database"`
	// Outcome names what was concluded: not-configured, converged, genesis,
	// unconverged, or uncheckable.
	Outcome string `json:"outcome"`
	// ProvenBeads is the size of the proven-copy manifest a serving verdict
	// rests on. "Converged" alone does not distinguish a city serving its whole
	// infrastructure slice from the binding from one whose copy carried nothing,
	// and those are the two situations an operator watching a cutover most needs
	// to tell apart.
	//
	// It costs nothing: every path that reaches a serving verdict has already
	// read the manifest to reach it. Every other outcome leaves it zero, and
	// zero there means the copy's size is not something this verdict
	// established — NOT that the copy is empty. On a serving outcome zero is the
	// real answer, which is what a genesis city reports.
	ProvenBeads int `json:"proven_beads"`
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
		events.StorageBindingNotConfigured,
	} {
		events.RegisterPayload(eventType, StorageBindingOutcomePayload{})
	}
}
