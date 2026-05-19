//go:build cgo && gascity_native_beads

package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	gcbeads "github.com/steveyegge/beads/gascitystore"
)

// DoltliteNativeStore keeps Gas City's existing fast Doltlite read path and
// sends writes through Beads' in-process Doltlite API instead of spawning bd.
type DoltliteNativeStore struct {
	*DoltliteReadStore
	native *gcbeads.Store
}

var doltliteNativeWriteMu sync.Mutex

func NewDoltliteNativeStore(dir string, backing *BdStore) (*DoltliteNativeStore, error) {
	readStore, err := NewDoltliteReadStore(dir, backing)
	if err != nil {
		return nil, err
	}
	native, err := gcbeads.Open(context.Background(), dir, "gascity")
	if err != nil {
		_ = readStore.CloseStore()
		return nil, err
	}
	return &DoltliteNativeStore{DoltliteReadStore: readStore, native: native}, nil
}

func (s *DoltliteNativeStore) CloseStore() error {
	var err error
	if s.DoltliteReadStore != nil {
		err = errors.Join(err, s.DoltliteReadStore.CloseStore())
	}
	if s.native != nil {
		err = errors.Join(err, s.native.Close())
	}
	return err
}

func (s *DoltliteNativeStore) Create(b Bead) (Bead, error) {
	var created gcbeads.Issue
	err := s.withNativeWriteRetry(func() error {
		var err error
		created, err = s.native.Create(context.Background(), nativeIssueFromBead(b))
		return err
	})
	if err != nil {
		return Bead{}, fmt.Errorf("doltlite create: %w", mapNativeErr(err))
	}
	return beadFromNativeIssue(created, b), nil
}

func (s *DoltliteNativeStore) Update(id string, opts UpdateOpts) error {
	err := s.withNativeWriteRetry(func() error {
		return s.native.Update(context.Background(), id, gcbeads.Update{
			Title:        opts.Title,
			Status:       opts.Status,
			IssueType:    opts.Type,
			Priority:     opts.Priority,
			Description:  opts.Description,
			ParentID:     opts.ParentID,
			Assignee:     opts.Assignee,
			Labels:       opts.Labels,
			RemoveLabels: opts.RemoveLabels,
			Metadata:     opts.Metadata,
		})
	})
	if err != nil {
		return fmt.Errorf("updating bead %q: %w", id, mapNativeErr(err))
	}
	return nil
}

func (s *DoltliteNativeStore) SetMetadata(id, key, value string) error {
	return s.SetMetadataBatch(id, map[string]string{key: value})
}

func (s *DoltliteNativeStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}
	err := s.withNativeWriteRetry(func() error {
		return s.native.Update(context.Background(), id, gcbeads.Update{Metadata: kvs})
	})
	if err != nil {
		return fmt.Errorf("setting metadata on %q: %w", id, mapNativeErr(err))
	}
	return nil
}

func (s *DoltliteNativeStore) Close(id string) error {
	// CloseIssue currently exercises a libdoltlite connection-open path that
	// can abort the gc process under concurrent controller order dispatch.
	// Keep closes on the bd CLI fallback until that native path is stable.
	if s.DoltliteReadStore != nil && s.DoltliteReadStore.BdStore != nil {
		return s.DoltliteReadStore.BdStore.Close(id)
	}
	if err := s.withNativeWriteRetry(func() error {
		return s.native.CloseIssue(context.Background(), id)
	}); err != nil {
		return fmt.Errorf("closing bead %q: %w", id, mapNativeErr(err))
	}
	return nil
}

func (s *DoltliteNativeStore) Reopen(id string) error {
	if err := s.withNativeWriteRetry(func() error {
		return s.native.Reopen(context.Background(), id)
	}); err != nil {
		return fmt.Errorf("reopening bead %q: %w", id, mapNativeErr(err))
	}
	return nil
}

func (s *DoltliteNativeStore) Delete(id string) error {
	if err := s.withNativeWriteRetry(func() error {
		return s.native.Delete(context.Background(), id)
	}); err != nil {
		return fmt.Errorf("deleting bead %q: %w", id, mapNativeErr(err))
	}
	return nil
}

func (s *DoltliteNativeStore) DepAdd(issueID, dependsOnID, depType string) error {
	if depType == "parent-child" {
		bead, err := s.Get(issueID)
		if err == nil && bead.ParentID == dependsOnID {
			return nil
		}
	}
	if err := s.withNativeWriteRetry(func() error {
		return s.native.AddDependency(context.Background(), issueID, dependsOnID, depType)
	}); err != nil {
		return fmt.Errorf("adding dep %s->%s: %w", issueID, dependsOnID, mapNativeErr(err))
	}
	return nil
}

func (s *DoltliteNativeStore) DepRemove(issueID, dependsOnID string) error {
	if err := s.withNativeWriteRetry(func() error {
		return s.native.RemoveDependency(context.Background(), issueID, dependsOnID)
	}); err != nil {
		return fmt.Errorf("removing dep %s->%s: %w", issueID, dependsOnID, mapNativeErr(err))
	}
	return nil
}

func (s *DoltliteNativeStore) withNativeWriteRetry(fn func() error) error {
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		doltliteNativeWriteMu.Lock()
		err = fn()
		doltliteNativeWriteMu.Unlock()
		if err == nil || !isDoltliteBusyErr(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 75 * time.Millisecond)
	}
	return err
}

func isDoltliteBusyErr(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "database is locked") || strings.Contains(text, "database busy")
}

func (s *DoltliteNativeStore) Ping() error {
	if _, err := s.native.Get(context.Background(), "__gascity_ping__"); err != nil && !errors.Is(mapNativeErr(err), ErrNotFound) {
		return fmt.Errorf("doltlite native store ping: %w", err)
	}
	return nil
}

func nativeIssueFromBead(b Bead) gcbeads.Issue {
	metadata := cloneStringMap(b.Metadata)
	if b.From != "" {
		if metadata == nil {
			metadata = map[string]string{}
		}
		if metadata["from"] == "" {
			metadata["from"] = b.From
		}
	}
	typ := b.Type
	if typ == "" {
		typ = "task"
	}
	status := b.Status
	if status == "" {
		status = "open"
	}
	deps := make([]gcbeads.Dependency, 0, len(b.Needs))
	for _, need := range b.Needs {
		need = strings.TrimSpace(need)
		if need == "" {
			continue
		}
		deps = append(deps, gcbeads.Dependency{
			IssueID:     b.ID,
			DependsOnID: need,
			Type:        "blocks",
		})
	}
	return gcbeads.Issue{
		ID:           b.ID,
		Title:        b.Title,
		Status:       status,
		IssueType:    typ,
		Priority:     cloneIntPtr(b.Priority),
		CreatedAt:    b.CreatedAt,
		Assignee:     b.Assignee,
		ParentID:     b.ParentID,
		Description:  b.Description,
		Labels:       append([]string(nil), b.Labels...),
		Metadata:     metadata,
		Dependencies: deps,
	}
}

func beadFromNativeIssue(issue gcbeads.Issue, fallback Bead) Bead {
	bead := Bead{
		ID:           issue.ID,
		Title:        issue.Title,
		Status:       mapBdStatus(issue.Status),
		Type:         issue.IssueType,
		Priority:     cloneIntPtr(issue.Priority),
		CreatedAt:    issue.CreatedAt,
		Assignee:     issue.Assignee,
		ParentID:     issue.ParentID,
		Description:  issue.Description,
		Labels:       append([]string(nil), issue.Labels...),
		Metadata:     cloneStringMap(issue.Metadata),
		Dependencies: make([]Dep, 0, len(issue.Dependencies)),
	}
	for _, dep := range issue.Dependencies {
		bead.Dependencies = append(bead.Dependencies, Dep{
			IssueID:     dep.IssueID,
			DependsOnID: dep.DependsOnID,
			Type:        dep.Type,
		})
	}
	if bead.Assignee == "" {
		bead.Assignee = fallback.Assignee
	}
	if bead.Priority == nil {
		bead.Priority = cloneIntPtr(fallback.Priority)
	}
	if bead.Metadata == nil {
		bead.Metadata = cloneStringMap(fallback.Metadata)
	}
	return bead
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	return out
}

func mapNativeErr(err error) error {
	if err == nil {
		return nil
	}
	if gcbeads.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}
