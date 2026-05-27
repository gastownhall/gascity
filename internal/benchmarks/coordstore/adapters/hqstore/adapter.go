// Package hqstore adapts the dormant beads.HQStore backend to the coordination
// store benchmark harness.
package hqstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
)

const (
	expiresAtMetadataKey = "expires_at"
	updatedAtMetadataKey = "coordstore.updated_at"
)

// Adapter implements coordstore.StoreAdapter using the dormant HQStore backend.
type Adapter struct {
	dir   string
	store *beads.HQStore
}

// New returns an uninitialized HQStore benchmark adapter.
func New() *Adapter {
	return &Adapter{}
}

// Open initializes HQStore under cfg.DataDir. Durability is provided by the
// async background snapshotter (no per-write fsync), so latency reflects the
// in-memory hot core.
func (a *Adapter) Open(_ context.Context, cfg coordstore.Config) error {
	dir := filepath.Join(cfg.DataDir, "hqstore")
	store, err := beads.OpenHQStore(dir)
	if err != nil {
		return err
	}
	a.dir = dir
	a.store = store
	return nil
}

// Close releases the HQStore handle.
func (a *Adapter) Close() error {
	if a.store == nil {
		return nil
	}
	err := a.store.Shutdown()
	a.store = nil
	return err
}

// Reset wipes all HQStore data and reopens the adapter.
func (a *Adapter) Reset(ctx context.Context) error {
	if a.store != nil {
		if err := a.store.Shutdown(); err != nil {
			return err
		}
		a.store = nil
	}
	if err := os.RemoveAll(a.dir); err != nil {
		return fmt.Errorf("hqstore reset: %w", err)
	}
	return a.Open(ctx, coordstore.Config{DataDir: filepath.Dir(a.dir)})
}

// Create persists a new record.
func (a *Adapter) Create(_ context.Context, r coordstore.Record) (coordstore.Record, error) {
	created, err := a.store.Create(recordToBead(r))
	if err != nil {
		return coordstore.Record{}, err
	}
	return beadToRecord(created), nil
}

// Get retrieves a record by ID.
func (a *Adapter) Get(_ context.Context, id string) (coordstore.Record, error) {
	got, err := a.store.Get(id)
	if err != nil {
		return coordstore.Record{}, mapNotFound(err)
	}
	return beadToRecord(got), nil
}

// Update modifies an existing record.
func (a *Adapter) Update(_ context.Context, id string, u coordstore.Update) error {
	opts := beads.UpdateOpts{}
	if u.Status != "" {
		opts.Status = &u.Status
	}
	if u.Assignee != "" {
		opts.Assignee = &u.Assignee
	}
	metadata := cloneMetadata(u.Metadata)
	if metadata == nil {
		metadata = make(map[string]string, 1)
	}
	metadata[updatedAtMetadataKey] = time.Now().Format(time.RFC3339Nano)
	opts.Metadata = metadata
	if err := a.store.Update(id, opts); err != nil {
		return mapNotFound(err)
	}
	return nil
}

// Delete permanently removes a record.
func (a *Adapter) Delete(_ context.Context, id string) error {
	if err := a.store.Delete(id); err != nil {
		return mapNotFound(err)
	}
	return nil
}

// FilterScan returns records matching q.
func (a *Adapter) FilterScan(_ context.Context, q coordstore.Query) ([]coordstore.Record, error) {
	items, err := a.store.List(queryToListQuery(q))
	if err != nil {
		return nil, err
	}
	return beadsToRecords(items), nil
}

// BatchGet retrieves multiple records by ID.
func (a *Adapter) BatchGet(_ context.Context, ids []string) ([]coordstore.Record, error) {
	out := make([]coordstore.Record, 0, len(ids))
	for _, id := range ids {
		got, err := a.store.Get(id)
		if errors.Is(err, beads.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, beadToRecord(got))
	}
	return out, nil
}

// SetMetadataBatch atomically merges metadata into a record.
func (a *Adapter) SetMetadataBatch(_ context.Context, id string, kvs map[string]string) error {
	metadata := cloneMetadata(kvs)
	if metadata == nil {
		metadata = make(map[string]string, 1)
	}
	metadata[updatedAtMetadataKey] = time.Now().Format(time.RFC3339Nano)
	if err := a.store.SetMetadataBatch(id, metadata); err != nil {
		return mapNotFound(err)
	}
	return nil
}

// Ready returns open, unblocked actionable records.
func (a *Adapter) Ready(_ context.Context, q coordstore.ReadyQuery) ([]coordstore.Record, error) {
	items, err := a.store.Ready(beads.ReadyQuery{Assignee: q.Assignee, Limit: q.Limit})
	if err != nil {
		return nil, err
	}
	return beadsToRecords(items), nil
}

// DepAdd records a dependency edge.
func (a *Adapter) DepAdd(_ context.Context, fromID, toID, depType string) error {
	return a.store.DepAdd(fromID, toID, depType)
}

// DepRemove removes a dependency edge.
func (a *Adapter) DepRemove(_ context.Context, fromID, toID string) error {
	return a.store.DepRemove(fromID, toID)
}

// DepList returns dependencies for a record.
func (a *Adapter) DepList(_ context.Context, id, direction string) ([]coordstore.Dep, error) {
	deps, err := a.store.DepList(id, direction)
	if err != nil {
		return nil, err
	}
	out := make([]coordstore.Dep, 0, len(deps))
	for _, dep := range deps {
		out = append(out, coordstore.Dep{
			FromID:  dep.IssueID,
			ToID:    dep.DependsOnID,
			DepType: dep.Type,
		})
	}
	return out, nil
}

// PurgeExpired removes expired ephemeral records.
func (a *Adapter) PurgeExpired(context.Context) (int, error) {
	return a.store.PurgeExpired()
}

// PurgeTerminal removes old terminal main-tier records.
func (a *Adapter) PurgeTerminal(_ context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	items, err := a.store.List(beads.ListQuery{
		AllowScan:     true,
		IncludeClosed: true,
		TierMode:      beads.TierIssues,
	})
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	purged := 0
	for _, item := range items {
		r := beadToRecord(item)
		if !coordstore.IsTerminalStatus(r.Status) {
			continue
		}
		ref := r.UpdatedAt
		if ref.IsZero() {
			ref = r.CreatedAt
		}
		if ref.IsZero() || !ref.Before(cutoff) {
			continue
		}
		if err := a.store.Delete(r.ID); err != nil {
			return purged, mapNotFound(err)
		}
		purged++
		if purged >= coordstore.TerminalPurgeBatchSize {
			break
		}
	}
	return purged, nil
}

// PrimeScan loads open records through the HQStore list path.
func (a *Adapter) PrimeScan(context.Context) (int, error) {
	items, err := a.store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		return 0, err
	}
	return len(items), nil
}

// RecentScan returns recently-created records across both tiers.
func (a *Adapter) RecentScan(_ context.Context, limit int) ([]coordstore.Record, error) {
	items, err := a.store.List(beads.ListQuery{
		AllowScan:     true,
		IncludeClosed: true,
		Limit:         limit,
		Sort:          beads.SortCreatedDesc,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return nil, err
	}
	return beadsToRecords(items), nil
}

// Stats returns optional diagnostics.
func (a *Adapter) Stats(context.Context) map[string]int64 {
	return nil
}

func recordToBead(r coordstore.Record) beads.Bead {
	metadata := cloneMetadata(r.Metadata)
	if !r.ExpiresAt.IsZero() || !r.UpdatedAt.IsZero() {
		if metadata == nil {
			metadata = make(map[string]string, 2)
		}
	}
	if !r.ExpiresAt.IsZero() {
		metadata[expiresAtMetadataKey] = r.ExpiresAt.Format(time.RFC3339Nano)
	}
	if !r.UpdatedAt.IsZero() {
		metadata[updatedAtMetadataKey] = r.UpdatedAt.Format(time.RFC3339Nano)
	}

	var priority *int
	if r.Priority != 0 {
		p := r.Priority
		priority = &p
	}
	return beads.Bead{
		ID:        r.ID,
		Title:     r.Title,
		Status:    r.Status,
		Type:      r.Type,
		Priority:  priority,
		CreatedAt: r.CreatedAt,
		Assignee:  r.Assignee,
		ParentID:  r.ParentID,
		Labels:    r.Labels,
		Metadata:  metadata,
		Ephemeral: r.Ephemeral,
	}
}

func beadToRecord(b beads.Bead) coordstore.Record {
	var priority int
	if b.Priority != nil {
		priority = *b.Priority
	}
	r := coordstore.Record{
		ID:        b.ID,
		Title:     b.Title,
		Status:    b.Status,
		Type:      b.Type,
		Priority:  priority,
		CreatedAt: b.CreatedAt,
		Assignee:  b.Assignee,
		ParentID:  b.ParentID,
		Labels:    b.Labels,
		Metadata:  b.Metadata,
		Ephemeral: b.Ephemeral,
	}
	if raw := r.Metadata[expiresAtMetadataKey]; raw != "" {
		if expiresAt, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			r.ExpiresAt = expiresAt
		}
	}
	if raw := r.Metadata[updatedAtMetadataKey]; raw != "" {
		if updatedAt, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			r.UpdatedAt = updatedAt
		}
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	return r
}

func beadsToRecords(items []beads.Bead) []coordstore.Record {
	out := make([]coordstore.Record, 0, len(items))
	for _, item := range items {
		out = append(out, beadToRecord(item))
	}
	return out
}

func queryToListQuery(q coordstore.Query) beads.ListQuery {
	query := beads.ListQuery{
		Status:    q.Status,
		Type:      q.Type,
		Label:     q.Label,
		Assignee:  q.Assignee,
		ParentID:  q.ParentID,
		Metadata:  q.Metadata,
		Limit:     q.Limit,
		AllowScan: q.AllowScan,
		TierMode:  tierMode(q.Tier),
	}
	if !query.HasFilter() {
		query.AllowScan = true
	}
	return query
}

func tierMode(t coordstore.Tier) beads.TierMode {
	switch t {
	case coordstore.TierEphemeral:
		return beads.TierWisps
	case coordstore.TierBoth:
		return beads.TierBoth
	default:
		return beads.TierIssues
	}
}

func mapNotFound(err error) error {
	if errors.Is(err, beads.ErrNotFound) {
		return coordstore.ErrNotFound
	}
	return err
}

func cloneMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
