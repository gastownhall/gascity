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
	// Database is the resolved file the binding's engine opens, and is empty
	// when no such file was resolved: a city with no split named no binding at
	// all, and a born-split city serves from a provider this build does not open
	// — its location is whatever that provider means by one, which is not
	// something this field can promise is a path. Binding is the field that is
	// always populated once a binding exists.
	Database string `json:"database"`
	// Outcome names what was concluded, and it is FINER than the event type.
	// Five types carry eight outcomes: not-configured, converged, genesis and
	// uncheckable each have a type to themselves, while unconverged, stranded,
	// born-split-blocked and genesis-blocked all arrive as
	// storage.binding.unconverged.
	//
	// They share a type because a subscriber branches on "is this city serving"
	// and all four answer no; they keep distinct outcomes because an operator
	// reading one event needs to know which no it was. A consumer switching on
	// this field must handle all eight — matching only the five type names would
	// silently drop three real values — and Invariant is the sentence that
	// explains whichever one arrived.
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
	// established — NOT that the copy is empty.
	//
	// Two serving outcomes report a real zero, and they mean different things. A
	// genesis city converged on a copy that carried nothing. A born-split city —
	// converged, on a binding this build does not migrate onto — never had a
	// proven copy to size, because nothing was ever copied into it; its zero says
	// the work store holds no infrastructure bead, which is the whole of what
	// that discipline proves. Read them apart by Database, which a born-split
	// event leaves empty.
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
