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
	// Six types carry the outcomes: not-configured, converged, genesis,
	// uncheckable and stranded each have a type to themselves, while
	// unconverged, born-split-blocked, genesis-blocked, retained-copies and
	// identity-mismatch all arrive as storage.binding.unconverged.
	//
	// Those share a type because a subscriber branches on "is this city
	// serving" and all of them answer no; they keep distinct outcomes because an
	// operator reading one event needs to know which no it was. Stranded has a
	// type of its own because it answers both ways (Serving): a stranded city
	// serves degraded unless a stranded id is held in the binding by a
	// different bead. A consumer switching on this field must handle every
	// outcome, and Invariant is the sentence that explains whichever one
	// arrived.
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
	// LostCrossEdges names the edges from work beads into beads the migration
	// moved that the work store could not keep once those beads were cleared
	// from it ("issue -[type]-> depends_on" with the store's reason). bd and
	// native-Dolt work stores refuse an edge to a bead they no longer hold, so
	// such an edge is dropped: a blocking one stops holding its work bead
	// back, and a tracks or related one stops linking the two. It is carried
	// on every verdict that reads the cleared note, so the record outlives the
	// migrate run that dropped them.
	LostCrossEdges []string `json:"lost_cross_edges,omitempty"`
	// Serving is set only by the boot gate (a controller boot or a gc
	// process resolving its routes) and says what that gate did: true when it
	// serves the city — converged, genesis, not-configured, or a stranded city
	// served degraded — false when it refuses. It is absent (null) on an
	// operator command's event and on a recheck's, which serve nothing, and
	// on every event an older build recorded; absent means "not stated", never
	// false.
	Serving *bool `json:"serving,omitempty"`
	// Recheck marks an event the controller's periodic, read-only containment
	// recheck published, rather than a boot or an operator command. The
	// running controller keeps serving whatever a recheck finds.
	Recheck bool `json:"recheck,omitempty"`
	// Stranded names the stranded ids a storage.binding.stranded event is
	// about, capped at the first StrandedCap in id order; StrandedCount is the
	// whole count. HeldDifferent (the ids the binding holds for a different
	// bead, any one of which makes the city refuse), PresumedReused (ids a
	// legacy manifest lists by a bare id they are younger than) and Retained
	// (retained copies of binding-owned beads beside the strand, which also
	// make it refuse) are capped the same way, each with its whole count. All
	// are empty on every other type.
	Stranded            []string `json:"stranded,omitempty"`
	StrandedCount       int      `json:"stranded_count,omitempty"`
	HeldDifferent       []string `json:"held_different,omitempty"`
	HeldDifferentCount  int      `json:"held_different_count,omitempty"`
	PresumedReused      []string `json:"presumed_reused,omitempty"`
	PresumedReusedCount int      `json:"presumed_reused_count,omitempty"`
	Retained            []string `json:"retained,omitempty"`
	RetainedCount       int      `json:"retained_count,omitempty"`
}

// StrandedCap bounds the ids a storage.binding.stranded payload lists.
const StrandedCap = 50

// IsEventPayload marks StorageBindingOutcomePayload as an events.Payload
// variant.
func (StorageBindingOutcomePayload) IsEventPayload() {}

// StorageRigPayload is the shape of the storage.rig_import and
// storage.rig_residue events: one rig's import outcome or residue verdict.
type StorageRigPayload struct {
	// Rig is the rig's name.
	Rig string `json:"rig"`
	// Binding is the city binding the rig's rows move into.
	Binding string `json:"binding"`
	// Outcome is what was concluded: for an import, imported, clean,
	// "nothing to import", skipped, refused or failed; for residue,
	// awaiting-import, retained or stranded.
	Outcome string `json:"outcome"`
	// Beads is the number of rows the import copied, or the residue counted.
	Beads int `json:"beads"`
	// Gates is the number of cross-store gates the import minted.
	Gates int `json:"gates,omitempty"`
	// Cleared is the number of rig copies the import removed.
	Cleared int `json:"cleared,omitempty"`
	// Detail is the operator-facing sentence: the refusal, failure or remedy.
	Detail string `json:"detail,omitempty"`
}

// IsEventPayload marks StorageRigPayload as an events.Payload variant.
func (StorageRigPayload) IsEventPayload() {}

func init() {
	events.RegisterPayload(events.StorageRigImport, StorageRigPayload{})
	events.RegisterPayload(events.StorageRigResidue, StorageRigPayload{})
	for _, eventType := range []string{
		events.StorageBindingConverged,
		events.StorageBindingGenesis,
		events.StorageBindingUnconverged,
		events.StorageBindingUncheckable,
		events.StorageBindingNotConfigured,
		events.StorageBindingStranded,
	} {
		events.RegisterPayload(eventType, StorageBindingOutcomePayload{})
	}
}
