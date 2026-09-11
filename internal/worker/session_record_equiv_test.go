package worker

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// buildEquivFactory returns a factory whose resolved-runtime hook records the
// Info, sessionKind, and metadata it is handed, so the equivalence tests can
// assert the new record-based constructors feed the t3bridge hook byte-identical
// arguments to the retired bead-based ones.
func buildEquivFactory(t *testing.T, store beads.Store, sp runtime.Provider, capture *resolverCapture) *Factory {
	t.Helper()
	factory, err := NewFactory(FactoryConfig{
		Store:    store,
		Provider: sp,
		ResolveSessionRuntime: func(info sessionpkg.Info, sessionKind string, metadata map[string]string) (*ResolvedRuntime, error) {
			capture.info = info
			capture.sessionKind = sessionKind
			capture.metadata = metadata
			return &ResolvedRuntime{
				Command:  "/bin/echo",
				WorkDir:  t.TempDir(),
				Provider: "stub",
				Resume:   sessionpkg.ProviderResume{SessionIDFlag: "--session-id"},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	return factory
}

type resolverCapture struct {
	info        sessionpkg.Info
	sessionKind string
	metadata    map[string]string
}

func seedEquivSession(t *testing.T, store beads.Store, sp runtime.Provider) sessionpkg.Info {
	t.Helper()
	manager := sessionpkg.NewManagerWithOptions(store, sp)
	info, err := manager.CreateSession(context.Background(), sessionpkg.CreateOptions{
		BeadOnly:  true,
		Template:  "worker",
		Title:     "Probe",
		Command:   "",
		WorkDir:   t.TempDir(),
		Provider:  "legacy-provider",
		Transport: "",
		Resume:    sessionpkg.ProviderResume{SessionIDFlag: "--stale-session-id"},
	})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	if err := store.SetMetadata(info.ID, "real_world_app_session_kind", "provider"); err != nil {
		t.Fatalf("SetMetadata(real_world_app_session_kind): %v", err)
	}
	if err := store.SetMetadata(info.ID, "worker_profile", string(ProfileClaudeTmuxCLI)); err != nil {
		t.Fatalf("SetMetadata(worker_profile): %v", err)
	}
	return info
}

// TestSessionByHandleCharacterizesResolverAndSpec pins the concrete
// resolved-runtime hook inputs and handle spec SessionByHandle produces: the
// persisted sessionKind, the full metadata map (including the worker_profile
// that drives the spec Profile), and the Info-derived spec identity. This is the
// characterization the retired GetWithBead-backed path satisfied (proven
// differentially in Commit A before SessionByLoadedBead was deleted).
func TestSessionByHandleCharacterizesResolverAndSpec(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	info := seedEquivSession(t, store, sp)

	var captured resolverCapture
	factory := buildEquivFactory(t, store, sp, &captured)

	handle, err := factory.SessionByHandle(info.ID)
	if err != nil {
		t.Fatalf("SessionByHandle: %v", err)
	}
	if captured.sessionKind != "provider" {
		t.Fatalf("resolver sessionKind = %q, want provider", captured.sessionKind)
	}
	if captured.metadata["real_world_app_session_kind"] != "provider" {
		t.Fatalf("resolver metadata[real_world_app_session_kind] = %q, want provider", captured.metadata["real_world_app_session_kind"])
	}
	if captured.metadata["worker_profile"] != string(ProfileClaudeTmuxCLI) {
		t.Fatalf("resolver metadata[worker_profile] = %q, want %q", captured.metadata["worker_profile"], ProfileClaudeTmuxCLI)
	}
	sh, ok := handle.(*SessionHandle)
	if !ok {
		t.Fatalf("handle is %T, not *SessionHandle", handle)
	}
	if sh.session.Profile != ProfileClaudeTmuxCLI {
		t.Fatalf("spec.Profile = %q, want %q", sh.session.Profile, ProfileClaudeTmuxCLI)
	}
	if sh.session.ID != info.ID {
		t.Fatalf("spec.ID = %q, want %q", sh.session.ID, info.ID)
	}
	// The resolver receives the PERSISTED Info (before it overlays its own
	// runtime): Provider is the stored legacy-provider, not the resolver's own
	// stub result that applyResolvedRuntimeToSessionSpec later writes onto the spec.
	if captured.info.ID != info.ID {
		t.Fatalf("resolver Info.ID = %q, want %q", captured.info.ID, info.ID)
	}
	if captured.info.Provider != "legacy-provider" {
		t.Fatalf("resolver Info.Provider = %q, want legacy-provider", captured.info.Provider)
	}
}

// TestSessionByRecordMatchesSessionByHandle pins that the two surviving worker
// construction entrypoints agree: the resolve+construct path
// (ResolveSessionRecordByExactID + SessionByRecord, used by cmd/gc/worker_handle.go)
// feeds the resolved-runtime hook the same sessionKind, metadata map, and Info
// as the by-id path (SessionByHandle), and builds the same handle spec.
func TestSessionByRecordMatchesSessionByHandle(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	info := seedEquivSession(t, store, sp)

	var byIDCap, byRecordCap resolverCapture
	byIDFactory := buildEquivFactory(t, store, sp, &byIDCap)
	byRecordFactory := buildEquivFactory(t, store, sp, &byRecordCap)

	byIDHandle, err := byIDFactory.SessionByHandle(info.ID)
	if err != nil {
		t.Fatalf("SessionByHandle: %v", err)
	}

	recInfo, pr, err := sessionpkg.ResolveSessionRecordByExactID(store, info.ID)
	if err != nil {
		t.Fatalf("ResolveSessionRecordByExactID: %v", err)
	}
	byRecordHandle, err := byRecordFactory.SessionByRecord(recInfo, pr)
	if err != nil {
		t.Fatalf("SessionByRecord: %v", err)
	}

	assertResolverCaptureEqual(t, byIDCap, byRecordCap)
	assertHandleSpecEqual(t, byIDHandle, byRecordHandle)
}

// TestResolveSessionRecordByExactIDMatchesBeadForm pins that the record resolver
// projects the SAME bead the bead resolver returns (Info + PersistedResponse) for
// a canonical typed session bead, and shares its not-found error. The empty-type
// normalize is pinned separately in
// TestResolveSessionRecordByExactIDNormalizesRepairableType.
func TestResolveSessionRecordByExactIDMatchesBeadForm(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	info := seedEquivSession(t, store, sp)

	bead, id, err := sessionpkg.ResolveSessionBeadByExactID(store, info.ID)
	if err != nil {
		t.Fatalf("ResolveSessionBeadByExactID: %v", err)
	}
	recInfo, pr, err := sessionpkg.ResolveSessionRecordByExactID(store, info.ID)
	if err != nil {
		t.Fatalf("ResolveSessionRecordByExactID: %v", err)
	}

	// SeedBead(t, bead) verbatim-seeds the resolved bead through the session front
	// door and reads it back — byte-identical to the raw-codec projection of bead,
	// but keeping bead serialization at the store edge. The record resolver must
	// match it.
	wantInfo := sessiontest.SeedBead(t, bead)
	if !reflect.DeepEqual(recInfo, wantInfo) {
		t.Fatalf("record Info = %#v, want the front-door projection of the resolved bead %#v", recInfo, wantInfo)
	}
	if id != info.ID || recInfo.ID != info.ID {
		t.Fatalf("id mismatch: bead=%q record=%q want %q", id, recInfo.ID, info.ID)
	}
	if pr.Status != bead.Status {
		t.Fatalf("record Status = %q, want %q", pr.Status, bead.Status)
	}
	for k, v := range bead.Metadata {
		if pr.Metadata[k] != v {
			t.Fatalf("record Metadata[%q] = %q, want %q", k, pr.Metadata[k], v)
		}
	}

	if _, _, err := sessionpkg.ResolveSessionRecordByExactID(store, "does-not-exist"); err == nil {
		t.Fatal("ResolveSessionRecordByExactID(absent) = nil error, want not-found")
	}
}

// TestResolveSessionRecordByExactIDNormalizesRepairableType pins the in-memory
// empty-type normalize: a label-only repairable bead (empty Type, gc:session
// label) resolves to a record whose Info.Type is the canonical session type,
// WITHOUT writing the repair back to the store (read-only resolution normalizes
// in memory; RepairEmptyType is the mutating path). Deleting normalizeEmptyType
// from ResolveSessionRecordByExactID makes this test red.
func TestResolveSessionRecordByExactIDNormalizesRepairableType(t *testing.T) {
	store := beads.NewMemStore()

	created, err := store.Create(beads.Bead{
		Title:    "repairable",
		Labels:   []string{sessionpkg.LabelSession},
		Metadata: map[string]string{"session_name": "repairable"},
	})
	if err != nil {
		t.Fatalf("create repairable bead: %v", err)
	}
	// MemStore.Create defaults an empty Type to "task"; rewrite to empty so the
	// crash/migration-damaged repairable shape (empty Type + gc:session label) is
	// preserved for the normalize path.
	empty := ""
	if err := store.Update(created.ID, beads.UpdateOpts{Type: &empty}); err != nil {
		t.Fatalf("clear type on repairable bead: %v", err)
	}

	recInfo, _, err := sessionpkg.ResolveSessionRecordByExactID(store, created.ID)
	if err != nil {
		t.Fatalf("ResolveSessionRecordByExactID: %v", err)
	}
	if recInfo.Type != sessionpkg.BeadType {
		t.Fatalf("record Info.Type = %q, want %q (in-memory normalize)", recInfo.Type, sessionpkg.BeadType)
	}

	// The normalize is in-memory only: the persisted bead type stays empty.
	persisted, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if persisted.Type != "" {
		t.Fatalf("persisted Type = %q, want empty (resolution must not write the repair)", persisted.Type)
	}
}

// TestSessionRecordViaManagerBridgesErrorContract pins that the worker read
// helper bridges the session.Store error contract back to the retired
// Manager.GetWithBead one so the API factory-lane mappers keep their status
// codes: a present-but-non-session bead surfaces session.ErrNotSession (mapped to
// 400), and an absent id keeps the beads.ErrNotFound chain (mapped to 404).
// Without the bridge the first case is session.ErrSessionNotFound, which those
// mappers do not recognize and fall through to a 500.
func TestSessionRecordViaManagerBridgesErrorContract(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp)

	// A present bead that is NOT a session bead (no session type, no gc:session
	// label) — the resolve-then-Get race the factory lane can hit.
	nonSession, err := store.Create(beads.Bead{
		Title:    "task",
		Type:     "task",
		Metadata: map[string]string{},
	})
	if err != nil {
		t.Fatalf("create non-session bead: %v", err)
	}

	_, _, err = sessionRecordViaManager(manager, nonSession.ID)
	if err == nil {
		t.Fatal("sessionRecordViaManager(present non-session) = nil error, want ErrNotSession")
	}
	if !errors.Is(err, sessionpkg.ErrNotSession) {
		t.Fatalf("present non-session error = %v, want errors.Is ErrNotSession (400 preservation)", err)
	}
	if errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("present non-session error must NOT be on the beads.ErrNotFound chain: %v", err)
	}

	_, _, err = sessionRecordViaManager(manager, "does-not-exist")
	if err == nil {
		t.Fatal("sessionRecordViaManager(absent) = nil error, want beads.ErrNotFound")
	}
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("absent-id error = %v, want errors.Is beads.ErrNotFound (404 preservation)", err)
	}
}

func assertResolverCaptureEqual(t *testing.T, want, got resolverCapture) {
	t.Helper()
	if got.sessionKind != want.sessionKind {
		t.Fatalf("sessionKind = %q, want %q", got.sessionKind, want.sessionKind)
	}
	if !reflect.DeepEqual(got.info, want.info) {
		t.Fatalf("resolver Info = %#v, want %#v", got.info, want.info)
	}
	if len(got.metadata) != len(want.metadata) {
		t.Fatalf("resolver metadata len = %d, want %d", len(got.metadata), len(want.metadata))
	}
	for k, v := range want.metadata {
		if got.metadata[k] != v {
			t.Fatalf("resolver metadata[%q] = %q, want %q", k, got.metadata[k], v)
		}
	}
}

func assertHandleSpecEqual(t *testing.T, want, got Handle) {
	t.Helper()
	wantSH, ok := want.(*SessionHandle)
	if !ok {
		t.Fatalf("want handle is %T, not *SessionHandle", want)
	}
	gotSH, ok := got.(*SessionHandle)
	if !ok {
		t.Fatalf("got handle is %T, not *SessionHandle", got)
	}
	if gotSH.session.Profile != wantSH.session.Profile {
		t.Fatalf("spec.Profile = %q, want %q", gotSH.session.Profile, wantSH.session.Profile)
	}
	if gotSH.session.ID != wantSH.session.ID ||
		gotSH.session.Template != wantSH.session.Template ||
		gotSH.session.Command != wantSH.session.Command ||
		gotSH.session.Provider != wantSH.session.Provider {
		t.Fatalf("spec identity mismatch: got %#v want %#v", gotSH.session, wantSH.session)
	}
}
