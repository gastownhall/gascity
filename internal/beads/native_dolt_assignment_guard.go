package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/issueops"
)

var _ AssignmentGuardedUpdater = (*NativeDoltStore)(nil)
var _ BlockedAssignmentReleaser = (*NativeDoltStore)(nil)

// UpdateIfAssignment applies opts only while id has the expected raw status
// and assignee. The raw reader avoids the Bead projection's blocked-to-open
// normalization. Its version fences the writing transaction as well: the SDK's
// assignee comparison accepts identity aliases, whereas this port requires the
// exact owner spelling. Retries retain that version and cannot acquire a newer
// claim. Missing roles and operational failures never fall back to raw writes.
func (s *NativeDoltStore) UpdateIfAssignment(id, expectedStatus, expectedAssignee string, opts UpdateOpts) (bool, error) {
	updated, _, err := s.updateIfAssignmentRaw(id, expectedStatus, expectedAssignee, opts)
	return updated, err
}

// ReleaseBlockedIfCurrent distinguishes an ordinary raw-open assignment from a
// blocked release that lost its guard. A lost blocked guard is handled even
// when another writer reopened the row, so the caller cannot release it again.
// The assignee clear and metadata patch share the raw status, holder and detail
// revision fence; the blocked status is never rewritten.
func (s *NativeDoltStore) ReleaseBlockedIfCurrent(id, expectedAssignee string, metadata map[string]string) (released, handled bool, err error) {
	if err := s.readOnlyGuard(); err != nil {
		return false, true, err
	}
	if strings.TrimSpace(expectedAssignee) == "" {
		return false, true, fmt.Errorf("native blocked release %q: a holder is required", id)
	}
	empty := ""
	released, rawStatus, err := s.updateIfAssignmentRaw(id, "blocked", expectedAssignee, UpdateOpts{Assignee: &empty, Metadata: metadata})
	return released, rawStatus != "open", err
}

func (s *NativeDoltStore) updateIfAssignmentRaw(id, expectedStatus, expectedAssignee string, opts UpdateOpts) (bool, string, error) {
	if err := s.readOnlyGuard(); err != nil {
		return false, "", err
	}
	if err := validateConditionalUpdateOpts(opts, false); err != nil {
		return false, "", fmt.Errorf("native update-if-assignment %s: %w", id, err)
	}
	expectedStatus = strings.TrimSpace(expectedStatus)
	expectedAssignee = strings.TrimSpace(expectedAssignee)
	if expectedStatus == "" {
		return false, "", fmt.Errorf("native update-if-assignment %s: an expected status is required", id)
	}
	patch, err := nativeIssuePatchFromUpdateOpts(opts)
	if err != nil {
		return false, "", err
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return false, "", err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	reader, err := storage.IssueReader()
	if err != nil {
		return false, "", fmt.Errorf("native update-if-assignment %s: resolving reader: %w", id, err)
	}
	current, err := reader.Get(ctx, issueops.GetRequest{ID: id})
	if errors.Is(err, issueops.ErrNotFound) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("native update-if-assignment %s: reading raw assignment: %w", id, err)
	}
	if current == nil || current.ID != id || strings.TrimSpace(current.Assignee) != expectedAssignee {
		return false, "", nil
	}
	if string(current.Status) != expectedStatus {
		return false, string(current.Status), nil
	}
	// Reuse the detail projection's revision decoder. Served rows publish
	// Revision and leave their embedded RowVersion at zero.
	projected, err := beadFromNativeIssueDetails(current, issueops.AnchorEdges{})
	if err != nil {
		return false, "", fmt.Errorf("native update-if-assignment %s: %w", id, err)
	}
	ops, err := storage.IssueLifecycle()
	if err != nil {
		return false, "", fmt.Errorf("native update-if-assignment %s: resolving lifecycle: %w", id, err)
	}
	status := issueops.Status(expectedStatus)
	version := projected.Revision
	err = retryOnNativeDoltSerializationConflict(func() error {
		_, err := ops.Update(ctx, issueops.UpdateRequest{
			Actor: s.actor, IssueID: id, Patch: patch,
			ExpectedVersion:  &version,
			ExpectedStatus:   &status,
			ExpectedAssignee: &expectedAssignee,
			Provenance:       fmt.Sprintf("gc: update-if-assignment bead %s", id),
		})
		return err
	})
	if errors.Is(err, issueops.ErrVersionMismatch) || errors.Is(err, issueops.ErrStatusMismatch) || errors.Is(err, issueops.ErrAssigneeMismatch) || errors.Is(err, issueops.ErrNotFound) {
		return false, expectedStatus, nil
	}
	if err != nil {
		return false, expectedStatus, fmt.Errorf("native update-if-assignment %s: %w", id, err)
	}
	return true, expectedStatus, nil
}
