package beads

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/beads/issueops"
)

// The reverse migration's copy into a native Dolt work store (S5 §3.2, §4):
// rows keep their ids, many to a commit, and the verify compares each row with
// the form the store reads it back in.

var (
	_ ForeignIDBatchCreator = (*NativeDoltStore)(nil)
	_ CreatedFormer         = (*NativeDoltStore)(nil)
	_ DepBatchWriter        = (*NativeDoltStore)(nil)
	_ DanglingDepJudge      = (*NativeDoltStore)(nil)
)

// DanglingDepJudge reports whether a store keeps an edge whose target it does
// not hold. A copy that has to put such an edge back after deleting the
// target asks first; a store that does not implement it keeps every edge.
type DanglingDepJudge interface {
	AcceptsDanglingDep(issueID, dependsOnID string) bool
}

// ForeignIDCreateFidelity reports whether a store's ForeignIDCreator really
// keeps the row, or falls back to a plain create that mints its own status
// and creation time. A store that does not implement it keeps the row.
type ForeignIDCreateFidelity interface {
	ForeignIDCreateKeepsRows() bool
}

// CreatedFormer reports the form a store reads a bead back in after it has
// created it with its id preserved (ForeignIDCreator, ForeignIDBatchCreator):
// the fields its schema does not hold are gone, and those it normalizes are
// normalized. A copy proves itself against that form, so a field the store
// cannot hold is a property of the store, not a lost write. A store that
// round-trips every field a bead carries does not implement it.
type CreatedFormer interface {
	CreatedForm(b Bead) (Bead, error)
}

// CreateWithForeignIDBatch creates rows keeping their ids, whatever prefix
// they carry, in ONE upstream batch create: one transaction and one Dolt
// commit for the durable rows of the call, all or nothing. Every id must be
// new to the store. Edges are not created here; the caller adds them once
// every row exists. It satisfies ForeignIDBatchCreator.
func (s *NativeDoltStore) CreateWithForeignIDBatch(rows []Bead) error {
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	items := make([]issueops.BatchCreateItem, 0, len(rows))
	for _, b := range rows {
		if b.ID == "" {
			return fmt.Errorf("creating %d beads with foreign ids: a row has no id", len(rows))
		}
		b.Dependencies, b.Needs, b.ParentID = nil, nil, ""
		issue, err := nativeIssueFromBead(b)
		if err != nil {
			return err
		}
		items = append(items, issueops.BatchCreateItem{Issue: issue})
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), nativeCreateBatchDeadline(len(rows)))
	defer cancel()
	creator, err := storage.BatchCreator()
	if err != nil {
		return nativeStoreError(rows[0].ID, err)
	}
	_, err = creator.CreateBatch(ctx, issueops.CreateBatchRequest{
		Actor:         s.actor,
		Items:         items,
		Provenance:    "gc: copy rows with their ids",
		ForceIDPrefix: true,
	})
	if err != nil {
		return fmt.Errorf("creating beads %s..%s: %w", rows[0].ID, rows[len(rows)-1].ID, nativeStoreError(rows[0].ID, err))
	}
	return nil
}

// DepAddBatch adds edges in ONE dependency-editor request: one transaction
// and one Dolt commit, all or nothing. The native store has no edge-payload
// writer, so a batch with a payload is refused before anything is written,
// wrapping ErrDepMetadataUnsupported. It satisfies DepBatchWriter.
func (s *NativeDoltStore) DepAddBatch(edges []DepWithPayload) error {
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
	if len(edges) == 0 {
		return nil
	}
	request := issueops.AddDependenciesRequest{Actor: s.actor, SkipPerEdgeCycleCheck: true}
	for _, e := range edges {
		if e.Payload != "" {
			return fmt.Errorf("adding dep %s -> %s with a payload: %w", e.IssueID, e.DependsOnID, ErrDepMetadataUnsupported)
		}
		request.Edges = append(request.Edges, issueops.DependencyEdge{IssueID: e.IssueID, DependsOnID: e.DependsOnID, Type: nativeGraphApplyDependencyType(e.Type)})
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), nativeDepRemoveBatchDeadline(len(edges)))
	defer cancel()
	editor, err := storage.DependencyEditor()
	if err != nil {
		return nativeStoreError(edges[0].IssueID, err)
	}
	if _, err := editor.AddDependencies(ctx, request); err != nil {
		return fmt.Errorf("adding deps %s -> %s .. %s -> %s: %w", edges[0].IssueID, edges[0].DependsOnID, edges[len(edges)-1].IssueID, edges[len(edges)-1].DependsOnID, nativeStoreError(edges[0].IssueID, err))
	}
	return nil
}

// AcceptsDanglingDep reports bd's rule: an edge onto an id of the source's
// own prefix must name a row the database holds, while an external: reference
// or an id of another prefix is stored as an external reference. It
// satisfies DanglingDepJudge.
func (s *NativeDoltStore) AcceptsDanglingDep(issueID, dependsOnID string) bool {
	return strings.HasPrefix(dependsOnID, "external:") || nativeIDPrefix(issueID) != nativeIDPrefix(dependsOnID)
}

// nativeIDPrefix is bd's prefix of an id: everything through its first '-'.
func nativeIDPrefix(id string) string {
	if i := strings.Index(id, "-"); i >= 0 {
		return id[:i+1]
	}
	return ""
}

// nativeDefaultInfraTypes are the types bd routes to the wisp tables when the
// scope's types.infra setting is unset (bd's built-in default).
var nativeDefaultInfraTypes = map[string]bool{"agent": true, "role": true, "message": true}

// createdFormInfraTypes is the scope's types.infra setting, read once through
// the workspace-config role, or bd's default when it is unset or unreadable.
func (s *NativeDoltStore) createdFormInfraTypes() map[string]bool {
	s.infraTypesOnce.Do(func() {
		s.infraTypes = nativeDefaultInfraTypes
		storage, release, err := s.acquireStorage()
		if err != nil {
			return
		}
		defer release()
		settings, err := storage.WorkspaceConfig()
		if err != nil {
			return
		}
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		result, err := settings.GetSetting(ctx, issueops.GetSettingRequest{Key: nativeTypesInfraConfigKey})
		if err != nil || strings.TrimSpace(result.Value) == "" {
			return
		}
		configured := map[string]bool{}
		for _, t := range strings.Split(result.Value, ",") {
			if t = strings.TrimSpace(t); t != "" {
				configured[t] = true
			}
		}
		s.infraTypes = configured
	})
	return s.infraTypes
}

// nativeCreateBatchDeadline scales a batch create's budget with its rows: the
// flat operation budget plus 100 ms a row, far above the few milliseconds a
// row costs, so a slow host does not hit the wall inside one batch.
func nativeCreateBatchDeadline(rows int) time.Duration {
	return bdCommandTimeout + time.Duration(rows)*100*time.Millisecond
}

// CreatedForm is b as this store reads it back after a create that keeps its
// id: the conversion to the upstream issue and back. It drops what the schema
// does not hold (the step ref, updated_at, which the store stamps itself) and
// normalizes what it does (an unset priority reads as the default, the close
// reason as the store derives it, times as whole seconds, rounded, and a row of
// one of bd's infra types lands in the wisp tables). The parent is the parent-child edge the copy adds, not
// a field. It satisfies CreatedFormer.
func (s *NativeDoltStore) CreatedForm(b Bead) (Bead, error) {
	parent := b.ParentID
	b.Dependencies, b.Needs, b.ParentID = nil, nil, ""
	issue, err := nativeIssueFromBead(b)
	if err != nil {
		return Bead{}, err
	}
	issue.RowVersion = 0
	if !issue.Ephemeral && !issue.NoHistory && s.createdFormInfraTypes()[string(issue.IssueType)] {
		// bd routes its configured infra types to the wisp tables whatever
		// the create asked (issueops ExecuteCreate and the batch create).
		issue.Ephemeral = true
	}
	got, err := beadFromNativeIssue(issue)
	if err != nil {
		return Bead{}, err
	}
	got.ParentID = parent
	got.SourceStatus = ""
	// Dolt keeps whole seconds and rounds what it is handed to the nearest.
	got.CreatedAt = got.CreatedAt.Round(time.Second)
	if got.DeferUntil != nil {
		rounded := got.DeferUntil.Round(time.Second)
		got.DeferUntil = &rounded
	}
	return got, nil
}
