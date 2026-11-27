package beads

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/deps"
)

// BdStore's pinned create runs `bd create --id <id>`. Whether that verb
// refuses a duplicate is a property of the bd release: bd v1.0.4 through
// v1.2.2 ran it through INSERT ... ON DUPLICATE KEY UPDATE, so a pinned
// create over an existing row silently reset its status, assignee and
// metadata and reported a fresh create — the overwrite the pinned-create
// contract exists to refuse. v1.3.0 made the create insert-only.
//
// Because the hazard is an overwrite, both gates run before the create: a bd
// this store cannot prove insert-only gets ErrPinnedCreateUnsupported with
// nothing executed.

// BdPinnedCreateMinVersion is the first bd release whose `bd create --id`
// refuses a duplicate id instead of upserting over the stored row.
const BdPinnedCreateMinVersion = "1.3.0"

// bdPinnedCreateGate reports, memoized per store, whether this store's bd
// release serves an insert-only `bd create --id`. A version probe failure is
// a refusal: a bd whose version cannot be read cannot be proven insert-only.
func (s *BdStore) bdPinnedCreateGate() error {
	s.pinnedCreateMu.Lock()
	defer s.pinnedCreateMu.Unlock()
	if s.pinnedCreateChecked {
		return s.pinnedCreateErr
	}
	s.pinnedCreateChecked = true
	version, err := s.BDVersion()
	if err != nil {
		s.pinnedCreateErr = fmt.Errorf("%w: cannot prove --id create insert-only: %w",
			ErrPinnedCreateUnsupported, err)
		return s.pinnedCreateErr
	}
	if deps.CompareVersions(version, BdPinnedCreateMinVersion) < 0 {
		s.pinnedCreateErr = fmt.Errorf("%w: bd %s predates %s, which made --id create insert-only; upgrade bd",
			ErrPinnedCreateUnsupported, version, BdPinnedCreateMinVersion)
		return s.pinnedCreateErr
	}
	return nil
}

// CreatePinned persists b at exactly b.ID via `bd create --id`. It admits only
// a bd at or above BdPinnedCreateMinVersion that also passes the
// conditional-write probe (conditionalWritesCapable); any other bd is refused
// with ErrPinnedCreateUnsupported before bd create runs, and a failed
// conditional-write probe is returned as the error. It satisfies
// PinnedIDCreator.
func (s *BdStore) CreatePinned(b Bead) (Bead, error) {
	if err := checkPinnedCreateID("bd pinned create", b.ID); err != nil {
		return Bead{}, err
	}
	if err := s.bdPinnedCreateGate(); err != nil {
		return Bead{}, fmt.Errorf("bd pinned create %q: %w", b.ID, err)
	}
	capable, err := s.conditionalWritesCapable()
	if err != nil {
		return Bead{}, fmt.Errorf("bd pinned create %q: probing conditional writes: %w", b.ID, err)
	}
	if !capable {
		return Bead{}, fmt.Errorf("bd pinned create %q: bd lacks conditional writes, so it would overwrite an existing id: %w", b.ID, ErrPinnedCreateUnsupported)
	}
	noteSessionCreate(b)
	created, err := s.CreateWithStorage(b, StorageDefault)
	if err != nil {
		return Bead{}, err
	}
	return checkPinnedCreateResult("bd pinned create", b.ID, created)
}
