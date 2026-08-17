package beads

import (
	"context"
	"fmt"

	beadslib "github.com/steveyegge/beads"
)

var _ CompleteIDScanner = (*NativeDoltStore)(nil)

// ScanAllIDs enumerates native rows without decoding metadata. Native List
// intentionally omits malformed-metadata rows so ordinary list views remain
// usable; a recovery census cannot tolerate that omission because it would let
// other rows emit facts while corrupt work stays invisible. The subsequent
// authoritative GetBatch decides whether every censused row is usable.
func (s *NativeDoltStore) ScanAllIDs() ([]string, error) {
	var out []string
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		filter := nativeIssueFilterFromListQuery(ListQuery{
			AllowScan:     true,
			IncludeClosed: true,
			TierMode:      TierBoth,
		})
		filter.IncludeDependencies = false
		ids, searchErr := storage.SearchIssueIDs(ctx, "", filter)
		if searchErr != nil {
			return nativeStoreError("", searchErr)
		}
		var validateErr error
		out, validateErr = stableUniqueCensusIDs(ids)
		return validateErr
	})
	if err != nil {
		return nil, fmt.Errorf("scanning native bead IDs: %w", err)
	}
	return out, nil
}
