package beads

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/deps"
)

// This file implements BdStore's pinned-create capability on `bd create --id`.
//
// The verb is the only path: a pinned create runs `bd create --id <id>`, and
// whether that verb is safe to call is a property of the bd release. bd
// v1.0.4 through v1.2.2 ran `bd create --id` through INSERT ... ON DUPLICATE
// KEY UPDATE — a pinned create over an existing row silently reset its
// status, assignee and metadata and reported a fresh create, which is the
// exact overwrite the pinned-create contract exists to refuse. v1.3.0 made
// the create insert-only (the same release as BdConditionalReleaseMinVersion,
// and for the same reason: 1.2.2 re-released the 1.1.2 code).
//
// Because the hazard is an OVERWRITE, the version gate runs BEFORE the
// create: a bd this store cannot prove insert-only gets
// ErrPinnedCreateUnsupported with nothing executed, not a create whose
// refusal happens after bd already wrote the row.

// BdPinnedCreateMinVersion is the first bd release whose `bd create --id`
// refuses a duplicate id instead of upserting over the stored row. cmd/gc's
// bdMinVersion may not drop below it (scripts/bd_version_pin_test.go).
const BdPinnedCreateMinVersion = "1.3.0"

// bdPinnedCreateGate answers, memoized per store, whether this store's bd can
// serve an insert-only `bd create --id`. A version probe failure is a refusal,
// not a pass: a bd whose version cannot be read cannot be proven insert-only,
// and the pinned-create contract's safe side is refusing before any mutation.
func (s *BdStore) bdPinnedCreateGate() error {
	s.pinnedCreateMu.Lock()
	defer s.pinnedCreateMu.Unlock()
	// Probe once per store instance: a long-lived store pays `bd version` one
	// subprocess on its first pinned create, and a store rebuilt per request
	// re-probes rather than inheriting a verdict from a bd that may have been
	// upgraded in between (mirroring the ready-projection gate's reasoning).
	if s.pinnedCreateChecked {
		return s.pinnedCreateErr
	}
	s.pinnedCreateChecked = true
	version, err := s.BDVersion()
	if err != nil {
		s.pinnedCreateErr = fmt.Errorf("bd pinned create: %w: cannot prove --id create insert-only: %w",
			ErrPinnedCreateUnsupported, err)
		return s.pinnedCreateErr
	}
	if deps.CompareVersions(version, BdPinnedCreateMinVersion) < 0 {
		s.pinnedCreateErr = fmt.Errorf("bd pinned create: %w: bd %s predates %s, which made --id create insert-only; upgrade bd",
			ErrPinnedCreateUnsupported, version, BdPinnedCreateMinVersion)
		return s.pinnedCreateErr
	}
	return nil
}

// CreatePinned persists b with exactly b.ID via `bd create --id`, refusing a
// duplicate ID without modifying the existing occupant. A bd release this
// store cannot prove insert-only refuses ErrPinnedCreateUnsupported before
// bd is ever invoked (PinnedIDCreator).
func (s *BdStore) CreatePinned(b Bead) (Bead, error) {
	if _, err := validatePinnedID(b); err != nil {
		return Bead{}, err
	}
	if err := s.bdPinnedCreateGate(); err != nil {
		return Bead{}, err
	}
	noteSessionCreate(b)
	return s.CreateWithStorage(b, StorageDefault)
}
