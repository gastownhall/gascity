package beads

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	beadslib "github.com/steveyegge/beads"
	bdhttp "github.com/steveyegge/beads/backend/http"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// authRecordingTransport wraps the in-memory `bd serve` stand-in and records,
// for every request, the Authorization header it carried and whether the
// global native-open env mutex was held while it was served.
type authRecordingTransport struct {
	inner *remoteHTTPServer

	mu          sync.Mutex
	auth        []string
	underEnvMu  []string
	requestSeen []string
}

func (t *authRecordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	held := !nativeDoltOpenEnvMu.TryLock()
	if !held {
		nativeDoltOpenEnvMu.Unlock()
	}
	t.mu.Lock()
	request := r.Method + " " + r.URL.Path
	t.requestSeen = append(t.requestSeen, request)
	t.auth = append(t.auth, r.Header.Get("Authorization"))
	if held {
		t.underEnvMu = append(t.underEnvMu, request)
	}
	t.mu.Unlock()
	return t.inner.RoundTrip(r)
}

// routeRemoteOpensThrough points every remote open and handshake of this test
// at transport, through the per-open options seam.
func routeRemoteOpensThrough(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	client := &http.Client{Transport: transport}
	previous := nativeOpenOptions
	nativeOpenOptions = func(string) beadslib.OpenOptions { return beadslib.OpenOptions{HTTPClient: client} }
	t.Cleanup(func() { nativeOpenOptions = previous })
}

func openRemoteForTest(t *testing.T, scope, city string) *NativeDoltStore {
	t.Helper()
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		CityPath:         city,
		Provider:         "bd",
		NativeTransport:  NativeTransportAuto,
		PreflightChecker: noDoltPreflight(t),
		OpenBdStore: func() (Store, error) {
			t.Fatal("OpenBdStore called for a remote scope under auto")
			return nil, nil
		},
		OpenNativeStore: func() (Store, error) {
			t.Fatal("the Dolt native opener was used for a remote scope")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	native, ok := result.Store.(*NativeDoltStore)
	if !ok {
		t.Fatalf("store = %T, want a NativeDoltStore", result.Store)
	}
	t.Cleanup(func() { _ = native.CloseStore() })
	return native
}

// TestRemoteOpenAuthorizesProbeAndStoreWithTheConfiguredCredentialsFile is
// MED 3: the operator's BEADS_CREDENTIALS_FILE override reaches EVERY request
// of the open, the wire_compat handshake and the store's own requests alike.
// The remote open used to run under the Dolt open's env projection, which
// withholds BEADS_CREDENTIALS_FILE, so the store authorized from the default
// file while the probe had honored the override. No request may be served
// while the global env mutex is held: it fences environment reads, never
// network I/O.
func TestRemoteOpenAuthorizesProbeAndStoreWithTheConfiguredCredentialsFile(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	recorder := &authRecordingTransport{inner: server}
	routeRemoteOpensThrough(t, recorder)
	scope := remoteHTTPScope(t, server)

	credentials := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(credentials, []byte("[127.0.0.1:9]\npassword=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", credentials)

	native := openRemoteForTest(t, scope, "")
	if _, err := native.Get("gr-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.requestSeen) < 3 {
		t.Fatalf("requests = %v, want the handshake and the store's own requests", recorder.requestSeen)
	}
	for i, request := range recorder.requestSeen {
		if recorder.auth[i] != "Bearer from-file" {
			t.Errorf("request %q authorized with %q, want the credentials-file token", request, recorder.auth[i])
		}
	}
	if len(recorder.underEnvMu) != 0 {
		t.Errorf("requests served while the global env mutex was held: %v", recorder.underEnvMu)
	}
}

// TestRemoteOpenCarriesAnExplicitCredential: the store open receives the
// credential this open resolved as bdhttp.Options.Credential (never the nil
// "ambient, read whenever" default), beside the seam's own transport.
func TestRemoteOpenCarriesAnExplicitCredential(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	scope := remoteHTTPScope(t, server)
	seam := nativeOpenOptions(filepath.Join(scope, ".beads"))

	var seen []bdhttp.Options
	previous := remoteNativeOpen
	remoteNativeOpen = func(ctx context.Context, target bdhttp.Target, opts bdhttp.Options) (beadslib.Storage, error) {
		seen = append(seen, opts)
		return previous(ctx, target, opts)
	}
	t.Cleanup(func() { remoteNativeOpen = previous })

	openRemoteForTest(t, scope, "")
	if len(seen) != 1 {
		t.Fatalf("remote opens = %d, want 1", len(seen))
	}
	if seen[0].Credential == nil {
		t.Fatal("bdhttp.Options.Credential is nil, want the resolved ambient ladder")
	}
	if seen[0].HTTPClient != seam.HTTPClient {
		t.Fatal("the store open did not take the seam's transport")
	}
}

// contextOnlyServer answers the v0 handshake with a chosen wire_revision.
type contextOnlyServer struct{ wireRevision int }

func (s contextOnlyServer) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	if r.URL.Path != "/v0/beads/context" {
		rec.WriteHeader(http.StatusNotFound)
	} else {
		body, _ := json.Marshal(map[string]any{
			"api_version": "v0", "backend": "dolt", "bd_version": "1.3.1",
			"beads_dir": "/srv/.beads", "capabilities": fullRemoteCapabilities(),
			"database": "beads", "dolt_mode": "server", "project_id": remoteHTTPProjectID,
			"repo_root": "/srv", "schema_version": 1, "wire_revision": s.wireRevision, "min_client_wire_revision": 0,
		})
		rec.Header().Set("Content-Type", "application/json")
		_, _ = rec.Write(body)
	}
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// TestRemoteWireRevisionRangeMatchesTheLinkedClient pins
// contract.RemoteWireRevisionMin/Max to the linked beads http client's own
// compiled range (which beads does not export): the real bdhttp.Handshake
// accepts exactly the revisions wire_compat accepts at both edges, and
// refuses the first revision past RemoteWireRevisionMax. A beads pin bump that
// moves the client's revision fails here until the constants move with it.
func TestRemoteWireRevisionRangeMatchesTheLinkedClient(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	base, err := url.Parse("http://127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		revision int
		accept   bool
	}{
		{contract.RemoteWireRevisionMin, true},
		{contract.RemoteWireRevisionMax, true},
		{contract.RemoteWireRevisionMax + 1, false},
	} {
		client := &http.Client{Transport: contextOnlyServer{wireRevision: tc.revision}}
		snapshot, err := bdhttp.Handshake(context.Background(), bdhttp.Target{BaseURL: base, ExpectProjectID: remoteHTTPProjectID}, bdhttp.Options{HTTPClient: client})
		if clientAccepts := err == nil; clientAccepts != tc.accept {
			t.Errorf("linked client at wire_revision %d: accepted = %v (err %v), want %v", tc.revision, clientAccepts, err, tc.accept)
			continue
		}
		if err != nil {
			continue
		}
		check := contract.EvaluateWireCompat("http", "", contract.PreflightWireHandshake{
			Snapshot: contract.PreflightWireSnapshot{
				APIVersion: snapshot.APIVersion, BdVersion: snapshot.BdVersion, WireRevision: snapshot.WireRevision,
				ProjectID: snapshot.ProjectID, Capabilities: snapshot.Capabilities,
			},
			TargetProjectID: remoteHTTPProjectID,
		}, nil)
		if check.State != contract.PreflightCheckPass {
			t.Errorf("wire_compat at wire_revision %d = %s (%s), but the linked client accepts it", tc.revision, check.State, check.Summary)
		}
	}
	past := contract.EvaluateWireCompat("http", "", contract.PreflightWireHandshake{
		Snapshot:        contract.PreflightWireSnapshot{APIVersion: "v0", WireRevision: contract.RemoteWireRevisionMax + 1, ProjectID: remoteHTTPProjectID, Capabilities: fullRemoteCapabilities()},
		TargetProjectID: remoteHTTPProjectID,
	}, nil)
	if past.State != contract.PreflightCheckFail {
		t.Errorf("wire_compat at wire_revision %d = %s, want FAIL like the linked client", contract.RemoteWireRevisionMax+1, past.State)
	}
}

// TestDecideMetadataBackendRigInheritsTheCityRemoteBackend is MED 6's route
// table: a rig with no metadata.json of its own under a city whose metadata
// names a registered remote backend takes the remote route against the
// CITY's activation; a rig that has its own metadata, or a city that is not
// remote, keeps today's route.
func TestDecideMetadataBackendRigInheritsTheCityRemoteBackend(t *testing.T) {
	registerFakeRemoteBackend(t)
	city := writeRemoteTestScope(t, remoteMetadata())
	bare := filepath.Join(city, "rigs", "bare")
	if err := os.MkdirAll(filepath.Join(bare, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(city, "rigs", "own")
	if err := os.MkdirAll(filepath.Join(own, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, ".beads", "metadata.json"), []byte(`{"backend": "dolt", "project_id": "p"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	localCity := writeRemoteTestScope(t, `{"backend": "dolt", "project_id": "p"}`)
	localRig := filepath.Join(localCity, "rigs", "bare")
	if err := os.MkdirAll(localRig, 0o755); err != nil {
		t.Fatal(err)
	}

	got := decideMetadataBackend(bare, city)
	if got.Route != metadataBackendRouteRemote || got.ActivationRoot != city || got.Backend != fakeRemoteBackendName {
		t.Fatalf("bare rig under a remote city = %+v, want the remote route on the city's activation", got)
	}
	if root, ok := RemoteBackendActivationRoot(bare, city); !ok || root != city {
		t.Fatalf("RemoteBackendActivationRoot(bare rig) = (%q, %v), want the city", root, ok)
	}
	if got := decideMetadataBackend(own, city); got.Route != metadataBackendRoutePreflight {
		t.Fatalf("rig with its own dolt metadata = %+v, want the preflight route", got)
	}
	if got := decideMetadataBackend(localRig, localCity); got.Route != metadataBackendRoutePreflight {
		t.Fatalf("bare rig under a dolt city = %+v, want the preflight route", got)
	}
	if got := decideMetadataBackend(bare, ""); got.Route != metadataBackendRoutePreflight {
		t.Fatalf("bare rig with no city = %+v, want the preflight route", got)
	}
	if got := decideMetadataBackend(city, city); got.Route != metadataBackendRouteRemote || got.ActivationRoot != city {
		t.Fatalf("the remote city itself = %+v, want the remote route on its own activation", got)
	}
}

// TestOpenStoreAtForCityRigInheritingRemoteCityOpensTheCityActivation is the
// same rule end to end with the real http client: a rig with no metadata of
// its own under an http city opens natively through the city's activation
// (the handshake and the store reach the city's server), never BdStore.
func TestOpenStoreAtForCityRigInheritingRemoteCityOpensTheCityActivation(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	city := remoteHTTPScope(t, server)
	rig := filepath.Join(city, "rigs", "inheriting")
	if err := os.MkdirAll(filepath.Join(rig, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	native := openRemoteForTest(t, rig, city)
	if native.IDPrefix() != "gr" {
		t.Fatalf("issue prefix = %q, want the city server's setting", native.IDPrefix())
	}
	if !strings.Contains(strings.Join(server.requests(), ","), "GET /v0/beads/context") {
		t.Fatalf("requests = %v, want the city server's handshake", server.requests())
	}
}

// TestLabeledUpdateIfMatchNeedsNoTransaction is MED 5: a conditional update
// carrying labels goes through the issueops roles (one fenced single-item
// batch), so it works on a backend whose RunInTransaction refuses, as the
// http backend's does. It still moves the revision and still refuses a stale
// one.
func TestLabeledUpdateIfMatchNeedsNoTransaction(t *testing.T) {
	store := newNativeDoltStoreForTest(&batchOnlyMemStorage{nativeDoltMemStorage: newNativeDoltMemStorage()})
	created, err := store.Create(Bead{Title: "labels over the wire"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := store.UpdateIfMatch(created.ID, before.Revision, UpdateOpts{Labels: []string{"gc:claimed"}}); err != nil {
		t.Fatalf("labeled UpdateIfMatch on a transaction-less backend: %v", err)
	}
	after, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	found := false
	for _, label := range after.Labels {
		found = found || label == "gc:claimed"
	}
	if !found {
		t.Fatalf("labels = %v, want gc:claimed", after.Labels)
	}
	if after.Revision == before.Revision {
		t.Fatalf("revision stayed %d: a label CAS must move the row version", after.Revision)
	}
	err = store.UpdateIfMatch(created.ID, before.Revision, UpdateOpts{RemoveLabels: []string{"gc:claimed"}})
	var precondition *PreconditionFailedError
	if !errors.As(err, &precondition) {
		t.Fatalf("stale labeled UpdateIfMatch error = %v, want *PreconditionFailedError", err)
	}
}
