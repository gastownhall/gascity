package worker

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
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
	manager := sessionpkg.NewManager(store, sp)
	info, err := manager.CreateBeadOnly(
		"worker",
		"Probe",
		"",
		t.TempDir(),
		"legacy-provider",
		"",
		nil,
		sessionpkg.ProviderResume{SessionIDFlag: "--stale-session-id"},
	)
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

// TestSessionByHandleMatchesSessionByID pins that the new SessionByHandle feeds
// the resolved-runtime hook the same sessionKind, metadata map, and Info-derived
// spec the retired GetWithBead-backed SessionByID did.
func TestSessionByHandleMatchesSessionByID(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	info := seedEquivSession(t, store, sp)

	var oldCap, newCap resolverCapture
	oldFactory := buildEquivFactory(t, store, sp, &oldCap)
	newFactory := buildEquivFactory(t, store, sp, &newCap)

	oldHandle, err := oldFactory.SessionByID(info.ID)
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	newHandle, err := newFactory.SessionByHandle(info.ID)
	if err != nil {
		t.Fatalf("SessionByHandle: %v", err)
	}

	assertResolverCaptureEqual(t, oldCap, newCap)
	assertHandleSpecEqual(t, oldHandle, newHandle)
}

// TestSessionByRecordMatchesSessionByLoadedBead pins that resolving via
// ResolveSessionRecordByExactID + SessionByRecord feeds the hook the same
// arguments as ResolveSessionBeadByExactID + SessionByLoadedBead.
func TestSessionByRecordMatchesSessionByLoadedBead(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	info := seedEquivSession(t, store, sp)

	var oldCap, newCap resolverCapture
	oldFactory := buildEquivFactory(t, store, sp, &oldCap)
	newFactory := buildEquivFactory(t, store, sp, &newCap)

	bead, _, err := sessionpkg.ResolveSessionBeadByExactID(store, info.ID)
	if err != nil {
		t.Fatalf("ResolveSessionBeadByExactID: %v", err)
	}
	oldHandle, err := oldFactory.SessionByLoadedBead(bead)
	if err != nil {
		t.Fatalf("SessionByLoadedBead: %v", err)
	}

	recInfo, pr, err := sessionpkg.ResolveSessionRecordByExactID(store, info.ID)
	if err != nil {
		t.Fatalf("ResolveSessionRecordByExactID: %v", err)
	}
	newHandle, err := newFactory.SessionByRecord(recInfo, pr)
	if err != nil {
		t.Fatalf("SessionByRecord: %v", err)
	}

	assertResolverCaptureEqual(t, oldCap, newCap)
	assertHandleSpecEqual(t, oldHandle, newHandle)
}

// TestResolveSessionRecordByExactIDMatchesBeadForm pins that the record resolver
// projects the SAME bead the bead resolver returns (Info + PersistedResponse),
// including the in-memory empty-type normalize, and shares its not-found error.
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

	wantInfo := sessionpkg.InfoFromPersistedBead(bead)
	if !reflect.DeepEqual(recInfo, wantInfo) {
		t.Fatalf("record Info = %#v, want InfoFromPersistedBead(bead) %#v", recInfo, wantInfo)
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
