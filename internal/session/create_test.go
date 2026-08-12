package session

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// failCreateStore wraps a store but errors on Create, to exercise the
// CreateSessionInfo error contract (no silent half-create).
type failCreateStore struct {
	beads.Store
	err error
}

func (f failCreateStore) Create(beads.Bead) (beads.Bead, error) {
	return beads.Bead{}, f.err
}

// TestCreateSessionInfoReturnsProjectionOfCreatedBead is the write-returns-Info
// oracle for the pool-create front door (W-pool §4): CreateSessionInfo returns
// the projected Info of the just-created bead, and that Info is byte-identical to
// projecting the bead a subsequent Get returns — proving the local projection on
// the store's Create result needs no post-create store.Get. It also pins that the
// returned Info.ID matches the id-only CreateSession sibling for the same spec.
func TestCreateSessionInfoReturnsProjectionOfCreatedBead(t *testing.T) {
	meta := map[string]string{
		"template":                  "tower/polecat",
		"agent_name":                "tower/polecat",
		"state":                     string(StateStartPending),
		"pending_create_claim":      "true",
		"pending_create_started_at": "2026-06-01T12:00:00Z",
		"session_origin":            "ephemeral",
		"generation":                "1",
		"continuation_epoch":        "1",
		"instance_token":            "tok-info",
		"session_name":              "polecat-pending-tok-info",
		"alias":                     "pc-1",
		"pool_slot":                 "3",
		"pool_alias_conflict":       "tower/polecat",
		"pool_alias_conflict_count": "2",
	}
	spec := CreateSpec{
		ID:        "explicit-info-id",
		Title:     "polecat",
		AgentName: "tower/polecat",
		Metadata:  meta,
	}

	is := NewStore(beads.SessionStore{Store: beads.NewMemStore()})
	info, err := is.CreateSessionInfo(spec)
	if err != nil {
		t.Fatalf("CreateSessionInfo: %v", err)
	}
	if info.ID == "" {
		t.Fatal("CreateSessionInfo returned an empty-id Info")
	}

	// The returned Info must equal a full projection of what a Get returns — the
	// property that lets the caller drop its post-create store.Get.
	want, err := is.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%q) after CreateSessionInfo: %v", info.ID, err)
	}
	if !reflect.DeepEqual(info, want) {
		t.Errorf("CreateSessionInfo Info diverged from Get projection\n got=%+v\nwant=%+v", info, want)
	}
	// The new under-reach fields must round-trip through the create projection.
	if info.PoolAliasConflict != "tower/polecat" || info.PoolAliasConflictCount != "2" {
		t.Errorf("pool-alias-conflict mirrors not projected: conflict=%q count=%q", info.PoolAliasConflict, info.PoolAliasConflictCount)
	}

	// Info.ID matches the id-only sibling for the same spec (fresh store so the
	// store-assigned ids line up deterministically for memstore's explicit-id echo).
	is2 := NewStore(beads.SessionStore{Store: beads.NewMemStore()})
	id, err := is2.CreateSession(spec)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if id != info.ID {
		t.Errorf("CreateSession id = %q, CreateSessionInfo Info.ID = %q; want equal", id, info.ID)
	}
}

// TestCreateSessionInfoErrorContract pins the "no silent half-create" contract:
// when the store's Create fails, CreateSessionInfo returns a zero Info and the
// wrapped error, and no session bead is persisted.
func TestCreateSessionInfoErrorContract(t *testing.T) {
	boom := errors.New("boom")
	mem := beads.NewMemStore()
	is := NewStore(beads.SessionStore{Store: failCreateStore{Store: mem, err: boom}})

	info, err := is.CreateSessionInfo(CreateSpec{Title: "polecat", AgentName: "tower/polecat"})
	if !errors.Is(err, boom) {
		t.Fatalf("CreateSessionInfo err = %v, want wrap of boom", err)
	}
	if !reflect.DeepEqual(info, Info{}) {
		t.Errorf("CreateSessionInfo returned non-zero Info on create error: %+v", info)
	}
	// No bead was persisted (the failing store never delegated to the memstore).
	all, err := ListAllSessionBeads(mem, beads.ListQuery{})
	if err != nil {
		t.Fatalf("ListAllSessionBeads: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("persisted %d beads after a failed create, want 0", len(all))
	}
}

// TestCreateSessionByteIdenticalConfiguredNamed proves CreateSession emits a
// single Create whose bead is byte-identical to the raw store.Create the
// configured-named create site in cmd/gc/session_beads.go performed: the same
// Title, the session Type, the [gc:session, agent:<name>] label pair, and the
// caller-assembled metadata map verbatim, with no explicit ID.
func TestCreateSessionByteIdenticalConfiguredNamed(t *testing.T) {
	mem := beads.NewMemStore()
	rec := beadstest.NewRecordingStore(mem)
	is := NewStore(beads.SessionStore{Store: rec})

	// The metadata vocabulary the session_beads.go create site assembles inline
	// for a configured-named (non-pool) session.
	meta := map[string]string{
		"agent_name":                "tower/polecat",
		"live_hash":                 "abc123",
		"session_origin":            "configured",
		"generation":                "1",
		"continuation_epoch":        "1",
		"instance_token":            "tok-1",
		"state":                     string(StateStartPending),
		"synced_at":                 "2026-06-01T12:00:00Z",
		"session_name":              "polecat",
		"pending_create_claim":      "true",
		"pending_create_started_at": "2026-06-01T12:00:00Z",
		"template":                  "tower/polecat",
		NamedSessionMetadataKey:     "true",
	}

	id, err := is.CreateSession(CreateSpec{
		Title:     "tower/polecat",
		AgentName: "tower/polecat",
		Metadata:  meta,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if id == "" {
		t.Fatal("CreateSession returned empty id")
	}

	calls := rec.CallsForOp("Create")
	if len(calls) != 1 {
		t.Fatalf("want 1 Create, got %d", len(calls))
	}
	got := calls[0].Bead

	wantBead := beads.Bead{
		Title:    "tower/polecat",
		Type:     BeadType,
		Labels:   []string{LabelSession, "agent:tower/polecat"},
		Metadata: meta,
	}
	if got.ID != "" {
		t.Errorf("Create bead ID = %q, want empty (no explicit id)", got.ID)
	}
	if got.Title != wantBead.Title {
		t.Errorf("Create bead Title = %q, want %q", got.Title, wantBead.Title)
	}
	if got.Type != wantBead.Type {
		t.Errorf("Create bead Type = %q, want %q", got.Type, wantBead.Type)
	}
	if !reflect.DeepEqual(got.Labels, wantBead.Labels) {
		t.Errorf("Create bead Labels = %#v, want %#v", got.Labels, wantBead.Labels)
	}
	if !reflect.DeepEqual(got.Metadata, wantBead.Metadata) {
		t.Errorf("Create bead Metadata = %#v, want %#v", got.Metadata, wantBead.Metadata)
	}
}

// TestCreateSessionByteIdenticalPoolWithExplicitID proves CreateSession with an
// explicit ID emits a Create byte-identical to the createPoolSessionBeadWithAlias
// raw site: the explicit ID, the pool title, the session Type, the
// [gc:session, agent:<name>] label pair, and the assembled pool metadata.
func TestCreateSessionByteIdenticalPoolWithExplicitID(t *testing.T) {
	mem := beads.NewMemStore()
	rec := beadstest.NewRecordingStore(mem)
	is := NewStore(beads.SessionStore{Store: rec})

	meta := map[string]string{
		"template":                  "tower/polecat",
		"agent_name":                "tower/polecat",
		"state":                     string(StateStartPending),
		"pending_create_claim":      "true",
		"pending_create_started_at": "2026-06-01T12:00:00Z",
		"session_origin":            "ephemeral",
		"generation":                "1",
		"continuation_epoch":        "1",
		"instance_token":            "tok-2",
		"session_name":              "polecat-pending-tok-2",
		"alias":                     "pc-1",
		"pool_slot":                 "3",
	}

	id, err := is.CreateSession(CreateSpec{
		ID:        "explicit-bead-id",
		Title:     "polecat",
		AgentName: "tower/polecat",
		Metadata:  meta,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// CreateSession returns the id the store assigned. The explicit ID is a hint
	// the create site supplies to the bead envelope (asserted on the recorded
	// Create below); whether the backend honors it is a store concern, so the
	// returned id is whatever Create handed back, not necessarily the hint.
	if id == "" {
		t.Error("CreateSession returned empty id")
	}

	calls := rec.CallsForOp("Create")
	if len(calls) != 1 {
		t.Fatalf("want 1 Create, got %d", len(calls))
	}
	got := calls[0].Bead

	if got.ID != "explicit-bead-id" {
		t.Errorf("Create bead ID = %q, want explicit-bead-id", got.ID)
	}
	if got.Title != "polecat" {
		t.Errorf("Create bead Title = %q, want polecat", got.Title)
	}
	if got.Type != BeadType {
		t.Errorf("Create bead Type = %q, want %q", got.Type, BeadType)
	}
	wantLabels := []string{LabelSession, "agent:tower/polecat"}
	if !reflect.DeepEqual(got.Labels, wantLabels) {
		t.Errorf("Create bead Labels = %#v, want %#v", got.Labels, wantLabels)
	}
	if !reflect.DeepEqual(got.Metadata, beads.StringMap(meta)) {
		t.Errorf("Create bead Metadata = %#v, want %#v", got.Metadata, beads.StringMap(meta))
	}
}

// TestCreateSessionByteIdenticalAdoptionBarrier proves CreateSession emits a
// Create byte-identical to the raw store.Create the adoption barrier performed
// in cmd/gc/adoption_barrier.go: no explicit ID (store-assigned), Title and
// AgentName both the adopted agent name, the session Type, the
// [gc:session, agent:<name>] label pair, and the barrier-assembled metadata
// (state:"active", instance_token, synced_at, no template/pending_create_claim)
// passed verbatim.
func TestCreateSessionByteIdenticalAdoptionBarrier(t *testing.T) {
	mem := beads.NewMemStore()
	rec := beadstest.NewRecordingStore(mem)
	is := NewStore(beads.SessionStore{Store: rec})

	// The metadata vocabulary runAdoptionBarrier assembles inline for an
	// adopted running session (no template/pending_create_claim — the barrier
	// adopts an already-live session; syncSessionBeads backfills hashes).
	meta := map[string]string{
		"session_name":       "tower-worker-3",
		"state":              "active",
		"generation":         "1",
		"continuation_epoch": "1",
		"instance_token":     "tok-adopt",
		"agent_name":         "tower/worker-3",
		"pool_slot":          "3",
		"synced_at":          "2026-06-29T00:00:00Z",
	}

	id, err := is.CreateSession(CreateSpec{
		Title:     "tower/worker-3",
		AgentName: "tower/worker-3",
		Metadata:  meta,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if id == "" {
		t.Fatal("CreateSession returned empty id")
	}

	calls := rec.CallsForOp("Create")
	if len(calls) != 1 {
		t.Fatalf("want 1 Create, got %d", len(calls))
	}
	got := calls[0].Bead

	if got.ID != "" {
		t.Errorf("Create bead ID = %q, want empty (no explicit id)", got.ID)
	}
	if got.Title != "tower/worker-3" {
		t.Errorf("Create bead Title = %q, want tower/worker-3", got.Title)
	}
	if got.Type != BeadType {
		t.Errorf("Create bead Type = %q, want %q", got.Type, BeadType)
	}
	wantLabels := []string{LabelSession, "agent:tower/worker-3"}
	if !reflect.DeepEqual(got.Labels, wantLabels) {
		t.Errorf("Create bead Labels = %#v, want %#v", got.Labels, wantLabels)
	}
	if !reflect.DeepEqual(got.Metadata, beads.StringMap(meta)) {
		t.Errorf("Create bead Metadata = %#v, want %#v", got.Metadata, beads.StringMap(meta))
	}
}

// TestCreateSessionReturnsAssignedID proves CreateSession returns the id the
// underlying store assigned when no explicit ID is supplied (the create sites
// read newBead.ID after the raw Create).
func TestCreateSessionReturnsAssignedID(t *testing.T) {
	mem := beads.NewMemStore()
	is := NewStore(beads.SessionStore{Store: mem})

	id, err := is.CreateSession(CreateSpec{
		Title:     "polecat",
		AgentName: "tower/polecat",
		Metadata:  map[string]string{"state": string(StateStartPending)},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if id == "" {
		t.Fatal("CreateSession returned empty id for store-assigned create")
	}
	// The returned id must address a real, readable session bead.
	if _, err := is.Get(id); err != nil {
		t.Errorf("Get(%q) after CreateSession: %v", id, err)
	}
}
