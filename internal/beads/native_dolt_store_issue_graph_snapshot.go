package beads

import (
	"context"
	"fmt"

	beadslib "github.com/steveyegge/beads"
)

// IssueGraphSnapshot reads issues and their dependency rows through one
// upstream storage transaction. Results are published only after the complete
// transaction succeeds, so a retry or dependency error cannot expose a partial
// graph to the caller.
func (s *NativeDoltStore) IssueGraphSnapshot(query ListQuery) ([]Bead, map[string][]Dep, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, nil, fmt.Errorf("reading native issue graph snapshot: %w", ErrQueryRequiresScan)
	}
	filter := nativeIssueFilterFromListQuery(query)
	if query.Status != "" {
		// Snapshot statuses are exact. In particular, open must not include
		// scheduler-normalized blocked rows before a pushed-down limit.
		status := beadslib.Status(query.Status)
		filter.Status = &status
		filter.ExcludeStatus = nil
	}

	var rows []Bead
	var depsByID map[string][]Dep
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		var candidateRows []Bead
		var candidateDeps map[string][]Dep
		if err := storage.RunInTransaction(ctx, "", func(tx beadslib.Transaction) error {
			issues, err := tx.SearchIssues(ctx, "", filter)
			if err != nil {
				return err
			}
			converted := make([]Bead, 0, len(issues))
			for _, issue := range issues {
				bead, err := beadFromNativeIssue(issue)
				if err != nil {
					return err
				}
				// This is an exact synchronization snapshot, not the scheduler
				// view that folds blocked/deferred/hooked into open.
				bead.Status = string(issue.Status)
				converted = append(converted, bead)
			}
			candidateRows = ApplyListQuery(converted, query)
			candidateDeps = make(map[string][]Dep, len(candidateRows))
			for _, bead := range candidateRows {
				records, err := tx.GetDependencyRecords(ctx, bead.ID)
				if err != nil {
					return fmt.Errorf("reading native dependencies for %s: %w", bead.ID, err)
				}
				deps := make([]Dep, 0, len(records))
				for _, record := range records {
					if record == nil {
						return fmt.Errorf("reading native dependencies for %s: nil dependency row", bead.ID)
					}
					deps = append(deps, Dep{
						IssueID:     record.IssueID,
						DependsOnID: record.DependsOnID,
						Type:        string(record.Type),
					})
				}
				candidateDeps[bead.ID] = deps
			}
			return nil
		}); err != nil {
			return err
		}
		rows = candidateRows
		depsByID = candidateDeps
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return rows, depsByID, nil
}
