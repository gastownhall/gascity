package api

import (
	"context"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/workbench"
)

// humaHandleBeadAttemptsDiff is the Huma-typed handler for
// GET /v0/city/{cityName}/bead/{id}/attempts/diff.
//
// It returns the READ-ONLY diff of the selected Bead's Execution-Attempt
// worktree. The worktree is Gas City's: this reads gc.work_dir off the Bead and
// runs `git diff` there without staging, discarding, committing, or otherwise
// mutating the worktree. States (empty / missing_worktree / unavailable) are
// explicit so the Workbench can render them without treating them as failures.
func (s *Server) humaHandleBeadAttemptsDiff(ctx context.Context, input *BeadAttemptsDiffInput) (*IndexOutput[workbench.Diff], error) {
	_, bead, err := s.resolveBeadOwner(input.ID)
	if err != nil {
		return nil, err
	}
	dir := strings.TrimSpace(bead.Metadata[beadmeta.WorkDirMetadataKey])
	if dir == "" {
		dir = strings.TrimSpace(bead.Metadata[beadmeta.LegacyWorkDirMetadataKey])
	}
	diff := workbench.WorktreeDiff(ctx, dir, workbench.DefaultMaxDiffBytes)
	return &IndexOutput[workbench.Diff]{
		Index: s.latestIndex(),
		Body:  diff,
	}, nil
}
