package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	beadslib "github.com/steveyegge/beads"
)

const nativeDoltStoreActor = "gascity"

// NativeDoltStore is a Store implementation backed by the upstream beads
// library over Dolt. It is constructed by the store factory after native-store
// preflight gates pass.
type NativeDoltStore struct {
	storage beadslib.Storage
	actor   string
}

var _ Store = (*NativeDoltStore)(nil)

func newNativeDoltStoreWithStorage(storage beadslib.Storage, actor string) *NativeDoltStore {
	if actor == "" {
		actor = nativeDoltStoreActor
	}
	return &NativeDoltStore{storage: storage, actor: actor}
}

func newNativeDoltStoreForTest(storage beadslib.Storage) *NativeDoltStore {
	return newNativeDoltStoreWithStorage(storage, "native-test")
}

// Create persists a new bead through the upstream beads storage layer.
func (s *NativeDoltStore) Create(b Bead) (Bead, error) {
	issue, err := nativeIssueFromBead(b)
	if err != nil {
		return Bead{}, err
	}
	if err := s.storage.CreateIssue(context.Background(), issue, s.actor); err != nil {
		return Bead{}, err
	}
	return beadFromNativeIssue(issue), nil
}

// Get retrieves a bead by ID from the upstream beads storage layer.
func (s *NativeDoltStore) Get(id string) (Bead, error) {
	issue, err := s.storage.GetIssue(context.Background(), id)
	if err != nil {
		return Bead{}, err
	}
	return beadFromNativeIssue(issue), nil
}

// Update modifies an existing bead through the upstream beads storage layer.
func (s *NativeDoltStore) Update(id string, opts UpdateOpts) error {
	ctx := context.Background()
	updates, err := s.nativeUpdates(ctx, id, opts)
	if err != nil {
		return err
	}
	if len(updates) > 0 {
		if err := s.storage.UpdateIssue(ctx, id, updates, s.actor); err != nil {
			return err
		}
	}
	for _, label := range opts.Labels {
		if err := s.storage.AddLabel(ctx, id, label, s.actor); err != nil {
			return err
		}
	}
	for _, label := range opts.RemoveLabels {
		if err := s.storage.RemoveLabel(ctx, id, label, s.actor); err != nil {
			return err
		}
	}
	if opts.ParentID != nil {
		return s.updateParent(ctx, id, *opts.ParentID)
	}
	return nil
}

// Close sets a bead's status to closed through the upstream beads storage layer.
func (s *NativeDoltStore) Close(id string) error {
	ctx := context.Background()
	current, err := s.storage.GetIssue(ctx, id)
	if err != nil {
		return err
	}
	if current.Status == beadslib.StatusClosed {
		return nil
	}
	return s.storage.CloseIssue(ctx, id, "", s.actor, "")
}

// Reopen sets a closed bead's status back to open.
func (s *NativeDoltStore) Reopen(id string) error {
	return s.storage.ReopenIssue(context.Background(), id, "", s.actor)
}

// CloseAll closes multiple beads and sets metadata on each newly closed bead.
func (s *NativeDoltStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	closed := 0
	for _, id := range ids {
		current, err := s.Get(id)
		if err != nil {
			return closed, err
		}
		if current.Status == "closed" {
			continue
		}
		if len(metadata) > 0 {
			if err := s.SetMetadataBatch(id, metadata); err != nil {
				return closed, err
			}
		}
		if err := s.Close(id); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}

// List returns beads matching the query.
func (s *NativeDoltStore) List(query ListQuery) ([]Bead, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("listing beads: %w", ErrQueryRequiresScan)
	}
	filter := nativeIssueFilterFromListQuery(query)
	issues, err := s.storage.SearchIssues(context.Background(), "", filter)
	if err != nil {
		return nil, err
	}
	beads := make([]Bead, 0, len(issues))
	for _, issue := range issues {
		beads = append(beads, beadFromNativeIssue(issue))
	}
	return ApplyListQuery(beads, query), nil
}

// ListOpen returns non-closed beads by default, or beads with the given status.
func (s *NativeDoltStore) ListOpen(status ...string) ([]Bead, error) {
	query := ListQuery{AllowScan: true}
	if len(status) > 0 {
		query.Status = status[0]
		if status[0] == "closed" {
			query.IncludeClosed = true
		}
	}
	return s.List(query)
}

// Ready returns open, unblocked actionable beads.
func (s *NativeDoltStore) Ready(queries ...ReadyQuery) ([]Bead, error) {
	q := readyQueryFromArgs(queries)
	filter := beadslib.WorkFilter{
		Status: beadslib.StatusOpen,
		Limit:  q.Limit,
	}
	if q.Assignee != "" {
		filter.Assignee = &q.Assignee
	}
	issues, err := s.storage.GetReadyWork(context.Background(), filter)
	if err != nil {
		return nil, err
	}
	beads := make([]Bead, 0, len(issues))
	for _, issue := range issues {
		beads = append(beads, beadFromNativeIssue(issue))
	}
	return beads, nil
}

// Children returns all beads whose parent-child dependency points at parentID.
func (s *NativeDoltStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		ParentID:      parentID,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByLabel returns beads with an exact label match.
func (s *NativeDoltStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByAssignee returns beads assigned to assignee with the requested status.
func (s *NativeDoltStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return s.List(ListQuery{Assignee: assignee, Status: status, Limit: limit, AllowScan: true})
}

// ListByMetadata returns beads whose metadata contains all filters.
func (s *NativeDoltStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	})
}

// SetMetadata sets a single metadata key on a bead.
func (s *NativeDoltStore) SetMetadata(id, key, value string) error {
	return s.SetMetadataBatch(id, map[string]string{key: value})
}

// SetMetadataBatch sets multiple metadata keys on a bead.
func (s *NativeDoltStore) SetMetadataBatch(id string, kvs map[string]string) error {
	ctx := context.Background()
	issue, err := s.storage.GetIssue(ctx, id)
	if err != nil {
		return err
	}
	metadata := metadataMapFromNative(issue.Metadata)
	if metadata == nil {
		metadata = make(map[string]string, len(kvs))
	}
	for k, v := range kvs {
		metadata[k] = v
	}
	raw, err := metadataRawFromMap(metadata)
	if err != nil {
		return err
	}
	return s.storage.UpdateIssue(ctx, id, map[string]interface{}{"metadata": raw}, s.actor)
}

// Delete permanently removes a bead from the upstream beads storage layer.
func (s *NativeDoltStore) Delete(id string) error {
	return s.storage.DeleteIssue(context.Background(), id)
}

// Ping verifies that the upstream storage is reachable.
func (s *NativeDoltStore) Ping() error {
	_, err := s.storage.GetStatistics(context.Background())
	return err
}

// DepAdd records a dependency between two beads.
func (s *NativeDoltStore) DepAdd(issueID, dependsOnID, depType string) error {
	return s.storage.AddDependency(context.Background(), &beadslib.Dependency{
		IssueID:     issueID,
		DependsOnID: dependsOnID,
		Type:        beadslib.DependencyType(depType),
	}, s.actor)
}

// DepRemove removes a dependency between two beads.
func (s *NativeDoltStore) DepRemove(issueID, dependsOnID string) error {
	return s.storage.RemoveDependency(context.Background(), issueID, dependsOnID, s.actor)
}

// DepList returns dependencies for a bead.
func (s *NativeDoltStore) DepList(id, direction string) ([]Dep, error) {
	ctx := context.Background()
	if direction == "up" {
		issues, err := s.storage.GetDependentsWithMetadata(ctx, id)
		if err != nil {
			return nil, err
		}
		deps := make([]Dep, 0, len(issues))
		for _, issue := range issues {
			deps = append(deps, Dep{
				IssueID:     issue.ID,
				DependsOnID: id,
				Type:        string(issue.DependencyType),
			})
		}
		return deps, nil
	}
	issues, err := s.storage.GetDependenciesWithMetadata(ctx, id)
	if err != nil {
		return nil, err
	}
	deps := make([]Dep, 0, len(issues))
	for _, issue := range issues {
		deps = append(deps, Dep{
			IssueID:     id,
			DependsOnID: issue.ID,
			Type:        string(issue.DependencyType),
		})
	}
	return deps, nil
}

func (s *NativeDoltStore) nativeUpdates(ctx context.Context, id string, opts UpdateOpts) (map[string]interface{}, error) {
	updates := make(map[string]interface{})
	if opts.Title != nil {
		updates["title"] = *opts.Title
	}
	if opts.Status != nil {
		updates["status"] = *opts.Status
	}
	if opts.Type != nil {
		updates["issue_type"] = *opts.Type
	}
	if opts.Priority != nil {
		updates["priority"] = *opts.Priority
	}
	if opts.Description != nil {
		updates["description"] = *opts.Description
	}
	if opts.Assignee != nil {
		updates["assignee"] = *opts.Assignee
	}
	if len(opts.Metadata) > 0 {
		issue, err := s.storage.GetIssue(ctx, id)
		if err != nil {
			return nil, err
		}
		metadata := metadataMapFromNative(issue.Metadata)
		if metadata == nil {
			metadata = make(map[string]string, len(opts.Metadata))
		}
		for k, v := range opts.Metadata {
			metadata[k] = v
		}
		raw, err := metadataRawFromMap(metadata)
		if err != nil {
			return nil, err
		}
		updates["metadata"] = raw
	}
	return updates, nil
}

func (s *NativeDoltStore) updateParent(ctx context.Context, id, parentID string) error {
	deps, err := s.DepList(id, "down")
	if err != nil {
		return err
	}
	for _, dep := range deps {
		if dep.Type != string(beadslib.DepParentChild) {
			continue
		}
		if err := s.storage.RemoveDependency(ctx, id, dep.DependsOnID, s.actor); err != nil {
			return err
		}
	}
	if parentID == "" {
		return nil
	}
	return s.storage.AddDependency(ctx, &beadslib.Dependency{
		IssueID:     id,
		DependsOnID: parentID,
		Type:        beadslib.DepParentChild,
	}, s.actor)
}

func nativeIssueFromBead(b Bead) (*beadslib.Issue, error) {
	status := b.Status
	if status == "" {
		status = "open"
	}
	issueType := b.Type
	if issueType == "" {
		issueType = "task"
	}
	issue := &beadslib.Issue{
		ID:          b.ID,
		Title:       b.Title,
		Description: b.Description,
		Status:      beadslib.Status(status),
		IssueType:   beadslib.IssueType(issueType),
		Assignee:    b.Assignee,
		Sender:      b.From,
		CreatedAt:   b.CreatedAt,
		Labels:      append([]string(nil), b.Labels...),
		Ephemeral:   b.Ephemeral,
	}
	if b.Priority != nil {
		issue.Priority = *b.Priority
	} else {
		issue.Priority = 2
	}
	raw, err := metadataRawFromMap(b.Metadata)
	if err != nil {
		return nil, err
	}
	issue.Metadata = raw
	for _, dep := range b.Dependencies {
		issue.Dependencies = append(issue.Dependencies, &beadslib.Dependency{
			IssueID:     dep.IssueID,
			DependsOnID: dep.DependsOnID,
			Type:        beadslib.DependencyType(dep.Type),
		})
	}
	if b.ParentID != "" {
		issue.Dependencies = append(issue.Dependencies, &beadslib.Dependency{
			IssueID:     b.ID,
			DependsOnID: b.ParentID,
			Type:        beadslib.DepParentChild,
		})
	}
	for _, need := range b.Needs {
		depType := "blocks"
		dependsOnID := need
		if before, after, ok := strings.Cut(need, ":"); ok && before != "" && after != "" {
			depType = before
			dependsOnID = after
		}
		issue.Dependencies = append(issue.Dependencies, &beadslib.Dependency{
			IssueID:     b.ID,
			DependsOnID: dependsOnID,
			Type:        beadslib.DependencyType(depType),
		})
	}
	return issue, nil
}

func beadFromNativeIssue(issue *beadslib.Issue) Bead {
	if issue == nil {
		return Bead{}
	}
	priority := issue.Priority
	b := Bead{
		ID:          issue.ID,
		Title:       issue.Title,
		Status:      string(issue.Status),
		Type:        string(issue.IssueType),
		Priority:    &priority,
		CreatedAt:   issue.CreatedAt,
		Assignee:    issue.Assignee,
		From:        issue.Sender,
		Description: issue.Description,
		Labels:      append([]string(nil), issue.Labels...),
		Metadata:    metadataMapFromNative(issue.Metadata),
		Ephemeral:   issue.Ephemeral,
	}
	for _, dep := range issue.Dependencies {
		if dep == nil {
			continue
		}
		converted := Dep{
			IssueID:     dep.IssueID,
			DependsOnID: dep.DependsOnID,
			Type:        string(dep.Type),
		}
		b.Dependencies = append(b.Dependencies, converted)
		if dep.Type == beadslib.DepParentChild && b.ParentID == "" {
			b.ParentID = dep.DependsOnID
		}
	}
	return b
}

func nativeIssueFilterFromListQuery(query ListQuery) beadslib.IssueFilter {
	filter := beadslib.IssueFilter{
		Limit:               query.Limit,
		MetadataFields:      query.Metadata,
		CreatedBefore:       zeroTimePtr(query.CreatedBefore),
		IncludeDependencies: true,
	}
	switch query.TierMode {
	case TierWisps:
		ephemeral := true
		filter.Ephemeral = &ephemeral
	case TierBoth:
		// no tier filter
	default:
		ephemeral := false
		filter.Ephemeral = &ephemeral
	}
	if query.Status != "" {
		status := beadslib.Status(query.Status)
		filter.Status = &status
	} else if !query.IncludeClosed {
		filter.ExcludeStatus = []beadslib.Status{beadslib.StatusClosed}
	}
	if query.Type != "" {
		issueType := beadslib.IssueType(query.Type)
		filter.IssueType = &issueType
	}
	if query.Label != "" {
		filter.Labels = []string{query.Label}
	}
	if query.Assignee != "" {
		filter.Assignee = &query.Assignee
	}
	if query.ParentID != "" {
		filter.ParentID = &query.ParentID
	}
	return filter
}

func zeroTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func metadataRawFromMap(metadata map[string]string) (json.RawMessage, error) {
	if len(metadata) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("marshaling metadata: %w", err)
	}
	return raw, nil
}

func metadataMapFromNative(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var values map[string]interface{}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil
	}
	metadata := make(map[string]string, len(values))
	for k, v := range values {
		if s, ok := v.(string); ok {
			metadata[k] = s
			continue
		}
		metadata[k] = fmt.Sprint(v)
	}
	return metadata
}
