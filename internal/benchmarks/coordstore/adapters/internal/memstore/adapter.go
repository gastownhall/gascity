// Package memstore provides a small indexed in-process StoreAdapter core for
// benchmark adapters that need a hot coordination-store model.
package memstore

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
)

// Persister receives write-through notifications from Adapter. Implementations
// may persist to disk or a remote service. Nil is valid for pure in-memory use.
type Persister interface {
	SaveRecord(context.Context, coordstore.Record) error
	DeleteRecord(context.Context, string, bool) error
	SaveDep(context.Context, coordstore.Dep) error
	DeleteDep(context.Context, string, string) error
	ResetBacking(context.Context) error
}

// Adapter is an in-process StoreAdapter with two physical tiers and a dep map.
type Adapter struct {
	prefix    string
	persister Persister
	seq       atomic.Int64

	mu        sync.RWMutex
	main      map[string]coordstore.Record
	ephemeral map[string]coordstore.Record
	deps      map[string]coordstore.Dep
	mainIdx   tierIndex
	ephIdx    tierIndex
}

// New returns an initialized in-process adapter.
func New(prefix string, persister Persister) *Adapter {
	a := &Adapter{prefix: prefix, persister: persister}
	a.resetLocked()
	return a
}

// ReplaceState installs records and deps loaded from a backing store.
func (a *Adapter) ReplaceState(records []coordstore.Record, deps []coordstore.Dep) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.resetLocked()
	var maxSeq int64
	for _, r := range records {
		r = cloneRecord(r)
		if r.Ephemeral {
			a.ephemeral[r.ID] = r
			a.ephIdx.add(r)
		} else {
			a.main[r.ID] = r
			a.mainIdx.add(r)
		}
		if n := numericSuffix(r.ID); n > maxSeq {
			maxSeq = n
		}
	}
	for _, d := range deps {
		a.deps[depKey(d.FromID, d.ToID)] = d
	}
	a.seq.Store(maxSeq)
}

// Open initializes the adapter.
func (a *Adapter) Open(context.Context, coordstore.Config) error { return nil }

// Close releases resources held by the adapter.
func (a *Adapter) Close() error { return nil }

// Reset wipes all records and dependencies.
func (a *Adapter) Reset(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.persister != nil {
		if err := a.persister.ResetBacking(ctx); err != nil {
			return err
		}
	}
	a.resetLocked()
	a.seq.Store(0)
	return nil
}

func (a *Adapter) resetLocked() {
	a.main = make(map[string]coordstore.Record)
	a.ephemeral = make(map[string]coordstore.Record)
	a.deps = make(map[string]coordstore.Dep)
	a.mainIdx = newTierIndex()
	a.ephIdx = newTierIndex()
}

// Create persists a record into the selected tier.
func (a *Adapter) Create(ctx context.Context, r coordstore.Record) (coordstore.Record, error) {
	r = normalizeRecord(r, a.nextID)

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.main[r.ID]; ok {
		return coordstore.Record{}, fmt.Errorf("memstore Create: duplicate id %q", r.ID)
	}
	if _, ok := a.ephemeral[r.ID]; ok {
		return coordstore.Record{}, fmt.Errorf("memstore Create: duplicate id %q", r.ID)
	}
	if a.persister != nil {
		if err := a.persister.SaveRecord(ctx, r); err != nil {
			return coordstore.Record{}, err
		}
	}
	if r.Ephemeral {
		a.ephemeral[r.ID] = cloneRecord(r)
		a.ephIdx.add(r)
	} else {
		a.main[r.ID] = cloneRecord(r)
		a.mainIdx.add(r)
	}
	return cloneRecord(r), nil
}

// Get retrieves a record by ID from either tier.
func (a *Adapter) Get(_ context.Context, id string) (coordstore.Record, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if r, ok := a.main[id]; ok {
		return cloneRecord(r), nil
	}
	if r, ok := a.ephemeral[id]; ok {
		return cloneRecord(r), nil
	}
	return coordstore.Record{}, coordstore.ErrNotFound
}

// Update modifies an existing record.
func (a *Adapter) Update(ctx context.Context, id string, u coordstore.Update) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	r, tier, ok := a.findLocked(id)
	if !ok {
		return coordstore.ErrNotFound
	}
	applyUpdate(&r, u)
	if a.persister != nil {
		if err := a.persister.SaveRecord(ctx, r); err != nil {
			return err
		}
	}
	a.storeLocked(r, tier)
	return nil
}

// Delete removes a record and all dependency edges touching it.
func (a *Adapter) Delete(ctx context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	r, tier, ok := a.findLocked(id)
	if !ok {
		return coordstore.ErrNotFound
	}
	if a.persister != nil {
		if err := a.persister.DeleteRecord(ctx, id, r.Ephemeral); err != nil {
			return err
		}
	}
	delete(a.main, id)
	delete(a.ephemeral, id)
	if tier == tierEphemeral {
		a.ephIdx.remove(r)
	} else {
		a.mainIdx.remove(r)
	}
	for k, d := range a.deps {
		if d.FromID == id || d.ToID == id {
			if a.persister != nil {
				if err := a.persister.DeleteDep(ctx, d.FromID, d.ToID); err != nil {
					return err
				}
			}
			delete(a.deps, k)
		}
	}
	_ = tier
	return nil
}

// FilterScan returns records matching q from the selected tier.
func (a *Adapter) FilterScan(_ context.Context, q coordstore.Query) ([]coordstore.Record, error) {
	a.mu.RLock()

	var candidates []coordstore.Record
	if q.Tier == coordstore.TierMain || q.Tier == coordstore.TierBoth {
		candidates = append(candidates, a.candidatesLocked(a.main, a.mainIdx, q)...)
	}
	if q.Tier == coordstore.TierEphemeral || q.Tier == coordstore.TierBoth {
		candidates = append(candidates, a.candidatesLocked(a.ephemeral, a.ephIdx, q)...)
	}
	if q.Tier == 0 {
		candidates = a.candidatesLocked(a.main, a.mainIdx, q)
	}
	a.mu.RUnlock()

	var out []coordstore.Record
	for _, r := range candidates {
		if !matches(r, q) {
			continue
		}
		out = append(out, r)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// BatchGet retrieves multiple records by ID.
func (a *Adapter) BatchGet(_ context.Context, ids []string) ([]coordstore.Record, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]coordstore.Record, 0, len(ids))
	for _, id := range ids {
		if r, ok := a.main[id]; ok {
			out = append(out, cloneRecord(r))
			continue
		}
		if r, ok := a.ephemeral[id]; ok {
			out = append(out, cloneRecord(r))
		}
	}
	return out, nil
}

// SetMetadataBatch atomically merges metadata into a record.
func (a *Adapter) SetMetadataBatch(ctx context.Context, id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	r, tier, ok := a.findLocked(id)
	if !ok {
		return coordstore.ErrNotFound
	}
	if r.Metadata == nil {
		r.Metadata = make(map[string]string, len(kvs))
	}
	for k, v := range kvs {
		r.Metadata[k] = v
	}
	r.UpdatedAt = time.Now()
	if a.persister != nil {
		if err := a.persister.SaveRecord(ctx, r); err != nil {
			return err
		}
	}
	a.storeLocked(r, tier)
	return nil
}

// Ready returns open records without unresolved blocking dependencies.
func (a *Adapter) Ready(_ context.Context, q coordstore.ReadyQuery) ([]coordstore.Record, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []coordstore.Record
	for id := range a.mainIdx.nonClosedIDs() {
		r, ok := a.main[id]
		if !ok {
			continue
		}
		if len(out) == q.Limit && q.Limit > 0 {
			break
		}
		if r.Status != "open" && r.Status != "in_progress" {
			continue
		}
		if q.Assignee != "" && r.Assignee != q.Assignee {
			continue
		}
		if readyExcludedType(r.Type) || a.blockedLocked(r.ID) {
			continue
		}
		out = append(out, cloneRecord(r))
	}
	return out, nil
}

// DepAdd records a dependency edge.
func (a *Adapter) DepAdd(ctx context.Context, fromID, toID, depType string) error {
	if depType == "" {
		depType = "blocks"
	}
	d := coordstore.Dep{FromID: fromID, ToID: toID, DepType: depType}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.persister != nil {
		if err := a.persister.SaveDep(ctx, d); err != nil {
			return err
		}
	}
	a.deps[depKey(fromID, toID)] = d
	return nil
}

// DepRemove removes a dependency edge.
func (a *Adapter) DepRemove(ctx context.Context, fromID, toID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.persister != nil {
		if err := a.persister.DeleteDep(ctx, fromID, toID); err != nil {
			return err
		}
	}
	delete(a.deps, depKey(fromID, toID))
	return nil
}

// DepList returns dependencies in the requested direction.
func (a *Adapter) DepList(_ context.Context, id, direction string) ([]coordstore.Dep, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []coordstore.Dep
	for _, d := range a.deps {
		if direction == "up" {
			if d.ToID == id {
				out = append(out, d)
			}
			continue
		}
		if d.FromID == id {
			out = append(out, d)
		}
	}
	return out, nil
}

// PurgeExpired removes expired ephemeral records.
func (a *Adapter) PurgeExpired(ctx context.Context) (int, error) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()

	var ids []string
	for id, r := range a.ephemeral {
		if !r.ExpiresAt.IsZero() && r.ExpiresAt.Before(now) {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		r := a.ephemeral[id]
		if a.persister != nil {
			if err := a.persister.DeleteRecord(ctx, id, true); err != nil {
				return 0, err
			}
		}
		a.ephIdx.remove(r)
		delete(a.ephemeral, id)
	}
	return len(ids), nil
}

// PurgeTerminal removes old terminal main-tier records.
func (a *Adapter) PurgeTerminal(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-olderThan)
	a.mu.Lock()
	defer a.mu.Unlock()

	var ids []string
	for id, r := range a.main {
		if !coordstore.IsTerminalStatus(r.Status) {
			continue
		}
		ref := r.UpdatedAt
		if ref.IsZero() {
			ref = r.CreatedAt
		}
		if !ref.IsZero() && ref.Before(cutoff) {
			ids = append(ids, id)
			if len(ids) >= coordstore.TerminalPurgeBatchSize {
				break
			}
		}
	}
	for _, id := range ids {
		r := a.main[id]
		if a.persister != nil {
			if err := a.persister.DeleteRecord(ctx, id, false); err != nil {
				return 0, err
			}
		}
		a.mainIdx.remove(r)
		delete(a.main, id)
		for k, d := range a.deps {
			if d.FromID != id && d.ToID != id {
				continue
			}
			if a.persister != nil {
				if err := a.persister.DeleteDep(ctx, d.FromID, d.ToID); err != nil {
					return 0, err
				}
			}
			delete(a.deps, k)
		}
	}
	return len(ids), nil
}

// PrimeScan loads all non-closed main records into the hot path.
func (a *Adapter) PrimeScan(_ context.Context) (int, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	n := 0
	for _, r := range a.main {
		if r.Status != "closed" {
			n++
		}
	}
	return n, nil
}

// RecentScan returns recently-created records from both tiers.
func (a *Adapter) RecentScan(_ context.Context, limit int) ([]coordstore.Record, error) {
	if limit <= 0 {
		limit = 100
	}
	a.mu.RLock()
	out := make([]coordstore.Record, 0, len(a.main)+len(a.ephemeral))
	for _, r := range a.main {
		out = append(out, cloneRecord(r))
	}
	for _, r := range a.ephemeral {
		out = append(out, cloneRecord(r))
	}
	a.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Stats returns store counts.
func (a *Adapter) Stats(context.Context) map[string]int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return map[string]int64{
		"main_records":      int64(len(a.main)),
		"ephemeral_records": int64(len(a.ephemeral)),
		"deps":              int64(len(a.deps)),
	}
}

func (a *Adapter) nextID() string {
	return fmt.Sprintf("%s-%d", a.prefix, a.seq.Add(1))
}

type tier int

const (
	tierMain tier = iota
	tierEphemeral
)

func (a *Adapter) findLocked(id string) (coordstore.Record, tier, bool) {
	if r, ok := a.main[id]; ok {
		return cloneRecord(r), tierMain, true
	}
	if r, ok := a.ephemeral[id]; ok {
		return cloneRecord(r), tierEphemeral, true
	}
	return coordstore.Record{}, tierMain, false
}

func (a *Adapter) storeLocked(r coordstore.Record, t tier) {
	r = cloneRecord(r)
	if t == tierEphemeral {
		r.Ephemeral = true
		if old, ok := a.ephemeral[r.ID]; ok {
			a.ephIdx.remove(old)
		}
		a.ephemeral[r.ID] = r
		a.ephIdx.add(r)
		return
	}
	r.Ephemeral = false
	if old, ok := a.main[r.ID]; ok {
		a.mainIdx.remove(old)
	}
	a.main[r.ID] = r
	a.mainIdx.add(r)
}

func (a *Adapter) candidatesLocked(records map[string]coordstore.Record, idx tierIndex, q coordstore.Query) []coordstore.Record {
	var out []coordstore.Record
	for id := range idx.candidateIDs(q) {
		r, ok := records[id]
		if !ok {
			continue
		}
		out = append(out, cloneRecord(r))
	}
	return out
}

func (a *Adapter) blockedLocked(id string) bool {
	for _, d := range a.deps {
		if d.FromID != id {
			continue
		}
		blocker, ok := a.main[d.ToID]
		if ok && (blocker.Status == "open" || blocker.Status == "in_progress") {
			return true
		}
	}
	return false
}

func normalizeRecord(r coordstore.Record, nextID func() string) coordstore.Record {
	if r.ID == "" {
		r.ID = nextID()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	if r.Status == "" {
		r.Status = "open"
	}
	if r.Type == "" {
		if r.Ephemeral {
			r.Type = "message"
		} else {
			r.Type = "task"
		}
	}
	return cloneRecord(r)
}

func applyUpdate(r *coordstore.Record, u coordstore.Update) {
	if u.Status != "" {
		r.Status = u.Status
	}
	if u.Assignee != "" {
		r.Assignee = u.Assignee
	}
	if len(u.Metadata) > 0 {
		if r.Metadata == nil {
			r.Metadata = make(map[string]string, len(u.Metadata))
		}
		for k, v := range u.Metadata {
			r.Metadata[k] = v
		}
	}
	r.UpdatedAt = time.Now()
}

func matches(r coordstore.Record, q coordstore.Query) bool {
	if q.Status != "" {
		if r.Status != q.Status {
			return false
		}
	} else if r.Status == "closed" {
		return false
	}
	if q.Type != "" && r.Type != q.Type {
		return false
	}
	if q.Assignee != "" && r.Assignee != q.Assignee {
		return false
	}
	if q.ParentID != "" && r.ParentID != q.ParentID {
		return false
	}
	if q.Label != "" && !contains(r.Labels, q.Label) {
		return false
	}
	for k, v := range q.Metadata {
		if r.Metadata[k] != v {
			return false
		}
	}
	return true
}

func cloneRecord(r coordstore.Record) coordstore.Record {
	if len(r.Labels) > 0 {
		r.Labels = append([]string(nil), r.Labels...)
	}
	if len(r.Metadata) > 0 {
		m := make(map[string]string, len(r.Metadata))
		for k, v := range r.Metadata {
			m[k] = v
		}
		r.Metadata = m
	}
	return r
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func depKey(fromID, toID string) string { return fromID + "\x00" + toID }

func readyExcludedType(t string) bool {
	switch t {
	case "merge-request", "gate", "molecule", "step", "message", "session", "agent", "role", "rig":
		return true
	default:
		return false
	}
}

type idSet map[string]struct{}

type tierIndex struct {
	status   map[string]idSet
	assignee map[string]idSet
	typ      map[string]idSet
	parent   map[string]idSet
}

func newTierIndex() tierIndex {
	return tierIndex{
		status:   make(map[string]idSet),
		assignee: make(map[string]idSet),
		typ:      make(map[string]idSet),
		parent:   make(map[string]idSet),
	}
}

func (i tierIndex) add(r coordstore.Record) {
	addToIndex(i.status, r.Status, r.ID)
	addToIndex(i.assignee, r.Assignee, r.ID)
	addToIndex(i.typ, r.Type, r.ID)
	addToIndex(i.parent, r.ParentID, r.ID)
}

func (i tierIndex) remove(r coordstore.Record) {
	removeFromIndex(i.status, r.Status, r.ID)
	removeFromIndex(i.assignee, r.Assignee, r.ID)
	removeFromIndex(i.typ, r.Type, r.ID)
	removeFromIndex(i.parent, r.ParentID, r.ID)
}

func (i tierIndex) nonClosedIDs() idSet {
	out := make(idSet)
	for id := range i.status["open"] {
		out[id] = struct{}{}
	}
	for id := range i.status["in_progress"] {
		out[id] = struct{}{}
	}
	return out
}

func (i tierIndex) candidateIDs(q coordstore.Query) idSet {
	var candidates []idSet
	if q.Status != "" {
		candidates = append(candidates, i.status[q.Status])
	} else {
		candidates = append(candidates, i.nonClosedIDs())
	}
	if q.Type != "" {
		candidates = append(candidates, i.typ[q.Type])
	}
	if q.Assignee != "" {
		candidates = append(candidates, i.assignee[q.Assignee])
	}
	if q.ParentID != "" {
		candidates = append(candidates, i.parent[q.ParentID])
	}
	if len(candidates) == 0 {
		return i.allIDs()
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if len(c) < len(best) {
			best = c
		}
	}
	out := make(idSet, len(best))
	for id := range best {
		out[id] = struct{}{}
	}
	return out
}

func (i tierIndex) allIDs() idSet {
	out := make(idSet)
	for _, ids := range i.status {
		for id := range ids {
			out[id] = struct{}{}
		}
	}
	return out
}

func addToIndex(index map[string]idSet, key, id string) {
	ids := index[key]
	if ids == nil {
		ids = make(idSet)
		index[key] = ids
	}
	ids[id] = struct{}{}
}

func removeFromIndex(index map[string]idSet, key, id string) {
	ids := index[key]
	if ids == nil {
		return
	}
	delete(ids, id)
	if len(ids) == 0 {
		delete(index, key)
	}
}

func numericSuffix(id string) int64 {
	var n int64
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] < '0' || id[i] > '9' {
			if i == len(id)-1 {
				return 0
			}
			_, _ = fmt.Sscanf(id[i+1:], "%d", &n)
			return n
		}
	}
	_, _ = fmt.Sscanf(id, "%d", &n)
	return n
}
