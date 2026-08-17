package beads

import (
	"context"
	"fmt"
	"strings"

	beadslib "github.com/steveyegge/beads"
)

var _ BatchGetter = (*NativeDoltStore)(nil)

// GetBatch retrieves the same fully hydrated rows as repeated Get calls using
// one upstream SearchIssues operation. The upstream search owns NativeDolt's
// cross-tier authority; this method only validates its response and restores
// stable first-request order.
func (s *NativeDoltStore) GetBatch(ids []string) ([]Bead, error) {
	unique, err := uniqueBatchGetIDs(ids)
	if err != nil {
		return nil, fmt.Errorf("getting native beads batch: %w", err)
	}
	if len(unique) == 0 {
		return nil, nil
	}

	var out []Bead
	err = s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		issues, searchErr := storage.SearchIssues(ctx, "", beadslib.IssueFilter{
			IDs:                 unique,
			IncludeDependencies: true,
		})
		if searchErr != nil {
			return nativeStoreError("", searchErr)
		}
		rows, validateErr := nativeBatchGetRows(unique, issues)
		if validateErr != nil {
			return validateErr
		}
		out = rows
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("getting native beads batch: %w", err)
	}
	return out, nil
}

func nativeBatchGetRows(ids []string, issues []*beadslib.Issue) ([]Bead, error) {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(issues))
	rows := make([]Bead, 0, len(issues))
	for i, issue := range issues {
		if issue == nil || strings.TrimSpace(issue.ID) == "" {
			return nil, fmt.Errorf("malformed native batch result at index %d: empty bead ID", i)
		}
		if _, ok := wanted[issue.ID]; !ok {
			return nil, fmt.Errorf("unexpected bead %q in native batch result: %w", issue.ID, ErrIDCollision)
		}
		if _, duplicate := seen[issue.ID]; duplicate {
			return nil, fmt.Errorf("duplicate bead %q in native batch result", issue.ID)
		}
		seen[issue.ID] = struct{}{}
		bead, err := beadFromNativeIssue(issue)
		if err != nil {
			return nil, err
		}
		rows = append(rows, bead)
	}
	return validateBatchGetRows(ids, rows)
}
