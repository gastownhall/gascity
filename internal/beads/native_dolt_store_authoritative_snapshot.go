package beads

import (
	"context"
	"fmt"
)

var _ AuthoritativeSnapshotter = (*NativeDoltStore)(nil)

// AuthoritativeSnapshot performs one upstream all-status/all-tier search and
// strictly converts every returned row. NativeDoltStore.Get is itself an
// exact-ID SearchIssues call, so the upstream search's tier precedence is the
// store's point-Get authority. Unlike List, this recovery snapshot must not
// skip a malformed-metadata row.
func (s *NativeDoltStore) AuthoritativeSnapshot() ([]Bead, error) {
	var out []Bead
	err := s.withReadRetry(func(ctx context.Context, storage NativeStorage) error {
		filter := nativeIssueFilterFromListQuery(ListQuery{
			AllowScan:     true,
			IncludeClosed: true,
			TierMode:      TierBoth,
		})
		issues, searchErr := storage.SearchIssues(ctx, "", filter)
		if searchErr != nil {
			return searchErr
		}
		rows := make([]Bead, 0, len(issues))
		for i, issue := range issues {
			if issue == nil {
				return fmt.Errorf("malformed native snapshot row at index %d: nil issue", i)
			}
			row, convertErr := beadFromNativeIssue(issue)
			if convertErr != nil {
				return fmt.Errorf("converting native snapshot row %q: %w", issue.ID, convertErr)
			}
			rows = append(rows, row)
		}
		out = rows
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading native authoritative snapshot: %w", err)
	}
	return validateAuthoritativeSnapshotRows(out)
}
