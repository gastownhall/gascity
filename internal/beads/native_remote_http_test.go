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

// The http leg: the real beads http client, registered the way gc's
// composition root registers it, against an in-process stand-in for `bd serve`
// that answers the v0 handshake plus the two reads below. beads exposes no
// public httpapi harness, so the stand-in implements just what the leg needs.
// It is served through an in-memory transport handed to the client by the
// per-open options seam (nativeOpenOptions), so no listener is opened.

const remoteHTTPProjectID = "gc-remote"

// remoteHTTPServer records every request and answers the handshake, the issue
// prefix setting and a not-found issue read. Anything else is answered 404
// and recorded, and the test's allowlist check fails on it.
type remoteHTTPServer struct {
	URL          string
	mu           sync.Mutex
	paths        []string
	capabilities []string
}

// RoundTrip serves one request in memory.
func (s *remoteHTTPServer) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

func (s *remoteHTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
		body, _ := json.Marshal(map[string]any{
			"api_version": "v0", "backend": "dolt", "bd_version": "1.3.1",
			"beads_dir": "/srv/.beads", "capabilities": s.capabilities,
			"database": "beads", "dolt_mode": "server", "project_id": remoteHTTPProjectID,
			"repo_root": "/srv", "schema_version": 1, "wire_revision": 2, "min_client_wire_revision": 0,
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/config/issue_prefix":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key":"issue_prefix","value":"gr"}`))
	default:
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":404,"code":"not_found","detail":"no issue or wisp with that id"}`))
	}
}

// newRemoteHTTPServer builds the stand-in and routes every remote open and
// handshake of this test to it through the per-open options seam.
func newRemoteHTTPServer(t *testing.T, capabilities []string) *remoteHTTPServer {
	t.Helper()
	s := &remoteHTTPServer{URL: "http://127.0.0.1:9", capabilities: capabilities}
	client := &http.Client{Transport: s}
	previous := nativeOpenOptions
	nativeOpenOptions = func(string) beadslib.OpenOptions { return beadslib.OpenOptions{HTTPClient: client} }
	t.Cleanup(func() { nativeOpenOptions = previous })
	return s
}

func (s *remoteHTTPServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

// remoteHTTPAllowed is the role surface this leg may reach. A raw path (SQL,
// a transaction, the blocked projection) has no route on the wire, so any
// request outside this list is the tripwire firing.
func remoteHTTPAllowed(request string) bool {
	switch request {
	case "GET /v0/beads/context", "GET /v0/beads/config/issue_prefix", "GET /v0/beads/issues/gr-missing":
		return true
	}
	return false
}

// remoteHTTPScope writes a scope activated for the http backend against
// server: metadata.json selects it, the per-user sidecar pins the server and
// project.
func remoteHTTPScope(t *testing.T, server *remoteHTTPServer) string {
	t.Helper()
	scope := writeRemoteTestScope(t, `{"backend": "http"}`)
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := bdhttp.SaveTarget(filepath.Join(scope, ".beads"), bdhttp.Target{BaseURL: base, ExpectProjectID: remoteHTTPProjectID}); err != nil {
		t.Fatalf("SaveTarget: %v", err)
	}
	return scope
}

// hermeticRemoteHTTPEnv keeps every rung of bd's credential ladder empty, so
// the loopback test server is reached with no credential at all.
func hermeticRemoteHTTPEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv(bdhttp.TokenEnv, "")
	t.Setenv(bdhttp.TokenCommandEnv, "")
	t.Setenv("BEADS_CREDENTIALS_FILE", filepath.Join(home, "no-credentials"))
	t.Setenv(nativeForceFallbackEnv, "")
	remoteHandshakeMu.Lock()
	remoteHandshakeCache = map[string]remoteHandshakeEntry{}
	remoteHandshakeMu.Unlock()
}

// noDoltPreflight is the production-shaped checker for the http leg with every
// Dolt reader replaced by a spy that fails the test. WireHandshake stays nil,
// so the factory's real (cached) bdhttp handshake runs.
func noDoltPreflight(t *testing.T) contract.PreflightChecker {
	t.Helper()
	return contract.PreflightChecker{
		Provider: "bd",
		BDContext: func(string) (contract.PreflightBDContext, error) {
			t.Fatal("bd context consulted on the http leg")
			return contract.PreflightBDContext{}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			t.Fatal("database identity probed on the http leg")
			return "", false, nil
		},
		DatabaseSchemaCursors: func(string) (contract.PreflightSchemaCursors, bool, error) {
			t.Fatal("schema cursors probed on the http leg")
			return contract.PreflightSchemaCursors{}, false, nil
		},
	}
}

// TestNativeStoreOpensRemoteHTTPBackendThroughLibraryDispatch is the http leg
// end to end: registration, routing, the real handshake's wire_compat, the
// library open, a role read, and the raw-method tripwire.
func TestNativeStoreOpensRemoteHTTPBackendThroughLibraryDispatch(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	scope := remoteHTTPScope(t, server)

	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		NativeTransport:  NativeTransportAuto,
		PreflightChecker: noDoltPreflight(t),
		OpenBdStore: func() (Store, error) {
			t.Fatal("OpenBdStore called for an http scope under auto")
			return nil, nil
		},
		OpenNativeStore: func() (Store, error) {
			t.Fatal("the Dolt native opener was used for an http scope")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v (requests %v)", err, server.requests())
	}
	native, ok := result.Store.(*NativeDoltStore)
	if !ok || result.Diagnostic.Store != storeNameNativeDoltStore {
		t.Fatalf("store = %T diag %+v, want a NativeDoltStore", result.Store, result.Diagnostic)
	}
	t.Cleanup(func() { _ = native.CloseStore() })
	if native.IDPrefix() != "gr" {
		t.Fatalf("issue prefix = %q, want the server's setting", native.IDPrefix())
	}

	if _, err := native.Get("gr-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}

	// The raw-method tripwire. CountIssues is a raw primitive the http client
	// refuses before it dials; the store must classify that refusal and the
	// wire must never see a request for it.
	before := len(server.requests())
	if _, err := native.Count(context.Background(), ListQuery{Type: "task", TierMode: TierIssues}); !errors.Is(err, ErrCountUnsupported) {
		t.Fatalf("Count error = %v, want ErrCountUnsupported (the raw CountIssues refusal)", err)
	}
	if after := len(server.requests()); after != before {
		t.Fatalf("raw CountIssues reached the wire: %v", server.requests()[before:])
	}

	for _, request := range server.requests() {
		if !remoteHTTPAllowed(request) {
			t.Errorf("request %q is outside the role surface (raw path reached the wire)", request)
		}
		lower := strings.ToLower(request)
		if strings.Contains(lower, "sql") || strings.Contains(lower, "blocked") || strings.Contains(lower, "transaction") {
			t.Errorf("request %q is a raw path", request)
		}
	}
}

// TestNativeStoreRemoteHTTPWireCompatFailureIsTerminal: the real handshake
// against a server missing a required capability refuses the open, names the
// token and the off escape hatch, and never opens the store.
func TestNativeStoreRemoteHTTPWireCompatFailureIsTerminal(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	var caps []string
	for _, token := range fullRemoteCapabilities() {
		if token != "issues.batchApply" {
			caps = append(caps, token)
		}
	}
	server := newRemoteHTTPServer(t, caps)
	scope := remoteHTTPScope(t, server)

	_, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		NativeTransport:  NativeTransportAuto,
		PreflightChecker: noDoltPreflight(t),
		OpenBdStore: func() (Store, error) {
			t.Fatal("OpenBdStore called for an http scope under auto")
			return nil, nil
		},
	})
	if !IsHTTPNativeOpenRequired(err) {
		t.Fatalf("error = %v, want HTTPNativeOpenRequiredError", err)
	}
	if !strings.Contains(err.Error(), "issues.batchApply") || !strings.Contains(err.Error(), `native_transport = "off"`) {
		t.Fatalf("error %q must name the missing token and the off escape hatch", err)
	}
	for _, request := range server.requests() {
		if request != "GET /v0/beads/context" {
			t.Fatalf("request %q after a failed wire_compat: the store must not open", request)
		}
	}
}

// TestRemoteWireHandshakeIsCachedPerProcess: a successful handshake answers
// later opens of the same scope without a dial; a failed one is not cached.
func TestRemoteWireHandshakeIsCachedPerProcess(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	scope := remoteHTTPScope(t, server)
	for i := 0; i < 3; i++ {
		if err := remoteWireHandshake(scope); err != nil {
			t.Fatalf("handshake %d: %v", i, err)
		}
	}
	if got := len(server.requests()); got != 1 {
		t.Fatalf("handshake dials = %d, want 1 (cached)", got)
	}
	if err := os.Remove(bdhttp.TargetPath(filepath.Join(scope, ".beads"))); err != nil {
		t.Fatal(err)
	}
	err := remoteWireHandshake(scope)
	var wireErr *contract.PreflightWireError
	if !errors.As(err, &wireErr) || wireErr.Reason != contract.PreflightWireNotConnected {
		t.Fatalf("handshake without an activation = %v, want not_connected", err)
	}
}

// remoteWireHandshake runs the cached handshake over the scope's own
// single-city plan (no city, so no per-city credential).
func remoteWireHandshake(scope string) error {
	plan, err := remoteOpenPlanFor(context.Background(), "", scope, scope)
	if err != nil {
		return err
	}
	_, err = remoteWireHandshakeWith(scope, plan)
	return err
}
