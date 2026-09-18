package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ImportedCommentIDsMetadataKey is the durable idempotency receipt shared by
// the GitHub work-sync planner and the exact-store atomic comment importer.
// Its value is a JSON array encoded as a metadata string so it round-trips
// through Gas City's existing map[string]string Store contract.
const ImportedCommentIDsMetadataKey = "github.imported_comment_ids"

// ErrAtomicCommentImportUnsupported reports that a store cannot prove the
// comment rows and their idempotency receipt commit in the same transaction.
var ErrAtomicCommentImportUnsupported = errors.New("atomic comment import unsupported")

// ImportedComment is one externally identified comment to preserve in Beads.
// ExternalID is not used as the Beads comment primary key; it is committed in
// ImportedCommentIDsMetadataKey alongside the imported native comment row.
type ImportedComment struct {
	ExternalID string
	Author     string
	Text       string
	CreatedAt  time.Time
}

// AtomicCommentImporter atomically applies row-backed fields, imports missing
// comments, and advances ImportedCommentIDsMetadataKey under one row revision
// fence. Implementations either commit all three effects or none of them.
type AtomicCommentImporter interface {
	UpdateWithCommentsIfMatch(id string, expectedRevision int64, opts UpdateOpts, comments []ImportedComment) error
}

// AtomicCommentImporterHandleProvider lets wrappers report the capability of
// their backing store without advertising support that backing cannot honor.
type AtomicCommentImporterHandleProvider interface {
	AtomicCommentImporterHandle() (AtomicCommentImporter, bool)
}

// AtomicCommentImporterFor resolves wrapper-declared exact-store targets and
// returns only a capability that can prove transactional comment import.
func AtomicCommentImporterFor(store Store) (AtomicCommentImporter, bool) {
	if store == nil {
		return nil, false
	}
	store = followConditionalWritesResolveTarget(store)
	if provider, ok := store.(AtomicCommentImporterHandleProvider); ok {
		return provider.AtomicCommentImporterHandle()
	}
	importer, ok := store.(AtomicCommentImporter)
	return importer, ok
}

// ApplyUpdateCASWithComments is ApplyUpdateCAS plus an atomic external-comment
// import. It preserves the same updated/already_applied/conflict outcomes and
// authoritative post-write readback. A retry after an ambiguous commit is
// classified already_applied from the durable external comment IDs and never
// duplicates native comment rows.
func ApplyUpdateCASWithComments(
	store Store,
	id string,
	expectedRevision int64,
	opts UpdateOpts,
	comments []ImportedComment,
) (UpdateCASResult, error) {
	if expectedRevision == 0 {
		return UpdateCASResult{}, fmt.Errorf("conditional comment import %s: expected revision must be a nonzero opaque token", id)
	}
	if err := validateImportedComments(comments); err != nil {
		return UpdateCASResult{}, fmt.Errorf("conditional comment import %s: %w", id, err)
	}
	if _, collision := opts.Metadata[ImportedCommentIDsMetadataKey]; collision {
		return UpdateCASResult{}, fmt.Errorf("conditional comment import %s: metadata %q is managed by comments", id, ImportedCommentIDsMetadataKey)
	}
	if !isEmptyUpdateOpts(opts) {
		if err := validateConditionalUpdateOpts(opts); err != nil {
			return UpdateCASResult{}, fmt.Errorf("conditional comment import %s: %w", id, err)
		}
	}

	target := followConditionalWritesResolveTarget(store)
	importer, ok := AtomicCommentImporterFor(target)
	if !ok {
		return UpdateCASResult{}, fmt.Errorf("conditional comment import for %q: %w", id, ErrAtomicCommentImportUnsupported)
	}
	current, err := ReadUpdateCASBead(target, id)
	if err != nil {
		return UpdateCASResult{}, fmt.Errorf("read conditional comment import base for %q: %w", id, err)
	}
	if current.Revision != expectedRevision || updateAndCommentsMatchBead(current, opts, comments) {
		return classifyCommentCASReadback(current, expectedRevision, opts, comments), nil
	}

	if err := importer.UpdateWithCommentsIfMatch(id, expectedRevision, opts, comments); err != nil {
		if !IsPreconditionFailed(err) {
			return UpdateCASResult{}, fmt.Errorf("conditional comment import for %q: %w", id, err)
		}
		current, readErr := ReadUpdateCASBead(target, id)
		if readErr != nil {
			return UpdateCASResult{}, fmt.Errorf("read conditional comment import conflict for %q: %w", id, readErr)
		}
		return classifyCommentCASReadback(current, expectedRevision, opts, comments), nil
	}

	updated, err := ReadUpdateCASBead(target, id)
	if err != nil {
		return UpdateCASResult{}, fmt.Errorf("read conditional comment import result for %q: %w", id, err)
	}
	if !updateAndCommentsMatchBead(updated, opts, comments) {
		return UpdateCASResult{}, fmt.Errorf("conditional comment import for %q reported success but authoritative readback does not match the request", id)
	}
	if updated.Revision == current.Revision {
		return UpdateCASResult{}, fmt.Errorf("conditional comment import for %q changed durable state without advancing revision %d", id, current.Revision)
	}
	return UpdateCASResult{
		Outcome:          UpdateCASUpdated,
		PreviousRevision: expectedRevision,
		Revision:         updated.Revision,
	}, nil
}

func validateImportedComments(comments []ImportedComment) error {
	if len(comments) == 0 {
		return errors.New("comments must not be empty")
	}
	seen := make(map[string]struct{}, len(comments))
	for i, comment := range comments {
		if strings.TrimSpace(comment.ExternalID) == "" {
			return fmt.Errorf("comment %d external id is empty", i)
		}
		if _, duplicate := seen[comment.ExternalID]; duplicate {
			return fmt.Errorf("duplicate comment external id %q", comment.ExternalID)
		}
		seen[comment.ExternalID] = struct{}{}
		if strings.TrimSpace(comment.Author) == "" {
			return fmt.Errorf("comment %q author is empty", comment.ExternalID)
		}
		if strings.TrimSpace(comment.Text) == "" {
			return fmt.Errorf("comment %q text is empty", comment.ExternalID)
		}
		if comment.CreatedAt.IsZero() {
			return fmt.Errorf("comment %q created_at is zero", comment.ExternalID)
		}
	}
	return nil
}

func classifyCommentCASReadback(
	current Bead,
	expectedRevision int64,
	opts UpdateOpts,
	comments []ImportedComment,
) UpdateCASResult {
	outcome := UpdateCASConflict
	if updateAndCommentsMatchBead(current, opts, comments) {
		outcome = UpdateCASAlreadyApplied
	}
	return UpdateCASResult{
		Outcome:          outcome,
		PreviousRevision: expectedRevision,
		Revision:         current.Revision,
	}
}

func updateAndCommentsMatchBead(bead Bead, opts UpdateOpts, comments []ImportedComment) bool {
	if !updateOptsMatchBead(bead, opts) {
		return false
	}
	ids, err := importedCommentIDSet(bead.Metadata[ImportedCommentIDsMetadataKey])
	if err != nil {
		return false
	}
	for _, comment := range comments {
		if _, ok := ids[comment.ExternalID]; !ok {
			return false
		}
	}
	return true
}

func importedCommentIDSet(raw string) (map[string]struct{}, error) {
	ids := []string{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &ids); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", ImportedCommentIDsMetadataKey, err)
		}
	}
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("decoding %s: empty comment id", ImportedCommentIDsMetadataKey)
		}
		if _, duplicate := set[id]; duplicate {
			return nil, fmt.Errorf("decoding %s: duplicate comment id %q", ImportedCommentIDsMetadataKey, id)
		}
		set[id] = struct{}{}
	}
	return set, nil
}
