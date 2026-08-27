package session

import "github.com/gastownhall/gascity/internal/beads"

// PersistedResponse carries fields projected from one persisted session bead.
// Status and Metadata form the backend-invariant response projection that is
// not part of scalar session.Info. Revision is internal, backend-dependent
// provenance from that same read. Keeping them together lets response and
// reconciliation paths use domain types without a *beads.Bead crossing their
// boundary.
//
// Bead serialization is confined here: PersistedResponseFromBead is the only
// place callers learn these facts come from a bead. Callers decode specific
// metadata keys through the existing session codecs (ParseTemplateOverrides,
// SubmissionCapabilitiesForMetadata, LifecycleDisplayReasonWithLiveness, the
// NamedSessionMetadataKey lookup), never by re-reading a bead.
type PersistedResponse struct {
	// Status is the persisted bead status ("open"/"closed"), used to derive the
	// lifecycle reason and to gate the metadata-derived fields on closed beads.
	Status string
	// Metadata is the full persisted session metadata map.
	Metadata map[string]string
	// Revision is copied from the same loaded Bead under the strong whole-row
	// beads.Bead.Revision contract. It is internal and off-wire; zero means the
	// revision is unavailable. It is not capability authorization and is not the
	// schema59 partial RowVersion.
	Revision int64 `json:"-"`
}

// PersistedResponseFromBead projects one loaded persisted session bead without
// additional I/O. Status and Metadata are backend-invariant response fields;
// Revision intentionally preserves the backend-dependent whole-row provenance
// supplied on that same loaded bead.
func PersistedResponseFromBead(b beads.Bead) PersistedResponse {
	return PersistedResponse{
		Status:   b.Status,
		Metadata: b.Metadata,
		Revision: b.Revision,
	}
}
