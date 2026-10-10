package beads

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	beadslib "github.com/steveyegge/beads"
	bdhttp "github.com/steveyegge/beads/backend/http"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// The G10 pieces against the in-process bd serve stand-in (remoteHTTPServer):
// gc storage connect's library half (ConnectRemoteScope) and the boot
// capability gate (CheckRemoteScopeBootGate).

// authCapture wraps the stand-in and records each request's Authorization.
type authCapture struct {
	server *remoteHTTPServer
	mu     sync.Mutex
	auth   []string
}

func (a *authCapture) RoundTrip(r *http.Request) (*http.Response, error) {
	a.mu.Lock()
	a.auth = append(a.auth, r.Header.Get("Authorization"))
	a.mu.Unlock()
	return a.server.RoundTrip(r)
}

func (a *authCapture) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.auth...)
}

// routeOpensThrough points every remote dial of this test at transport.
func routeOpensThrough(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	client := &http.Client{Transport: transport}
	previous := nativeOpenOptions
	nativeOpenOptions = func(string) beadslib.OpenOptions { return beadslib.OpenOptions{HTTPClient: client} }
	t.Cleanup(func() { nativeOpenOptions = previous })
}

func capabilitiesWithout(drop ...string) []string {
	skip := map[string]bool{}
	for _, token := range drop {
		skip[token] = true
	}
	var out []string
	for _, token := range fullRemoteCapabilities() {
		if !skip[token] {
			out = append(out, token)
		}
	}
	return out
}

// TestConnectRemoteScopeWritesTheAttachShapeGCOpensNatively is the whole G10
// connect path: a fresh scope attached through bdhttp.Attach, pinned to the
// project the server reported, then opened by the ordinary factory under
// native_transport=auto as a NativeDoltStore, with no Dolt probe.
func TestConnectRemoteScopeWritesTheAttachShapeGCOpensNatively(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	scope := t.TempDir()

	result, err := ConnectRemoteScope(context.Background(), RemoteConnectRequest{ScopeRoot: scope, URL: server.URL})
	if err != nil {
		t.Fatalf("ConnectRemoteScope: %v", err)
	}
	if !contract.BackendIsRemote(result.Backend) || !result.Changed {
		t.Fatalf("result = %+v, want a changed scope selecting a registered remote backend", result)
	}
	if result.Server.ProjectID != remoteHTTPProjectID || result.WireCompat.State == contract.PreflightCheckFail {
		t.Fatalf("result server/verdict = %+v / %+v", result.Server, result.WireCompat)
	}
	target, err := bdhttp.LoadTarget(filepath.Join(scope, ".beads"))
	if err != nil || target.BaseURL == nil || target.BaseURL.String() != server.URL || target.ExpectProjectID != remoteHTTPProjectID {
		t.Fatalf("sidecar = %+v (%v), want the server pinned to %q", target, err, remoteHTTPProjectID)
	}

	opened, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		NativeTransport:  NativeTransportAuto,
		PreflightChecker: noDoltPreflight(t),
		OpenBdStore: func() (Store, error) {
			t.Fatal("a connected scope fell back to BdStore under auto")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("opening the connected scope: %v (requests %v)", err, server.requests())
	}
	native, ok := opened.Store.(*NativeDoltStore)
	if !ok {
		t.Fatalf("store = %T, want *NativeDoltStore", opened.Store)
	}
	t.Cleanup(func() { _ = native.CloseStore() })
	if native.IDPrefix() != "gr" {
		t.Fatalf("prefix = %q, want the server's", native.IDPrefix())
	}
}

// TestConnectRemoteScopeRefusesAServerTheBootGateWouldRefuse: a server
// missing a required capability is refused before anything is written.
func TestConnectRemoteScopeRefusesAServerTheBootGateWouldRefuse(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, capabilitiesWithout("issues.casMetadata"))
	scope := t.TempDir()

	_, err := ConnectRemoteScope(context.Background(), RemoteConnectRequest{ScopeRoot: scope, URL: server.URL})
	var gate *RemoteCapabilityGateError
	if !errors.As(err, &gate) || len(gate.MissingRequired) != 1 || gate.MissingRequired[0] != "issues.casMetadata" {
		t.Fatalf("err = %v, want a gate refusal naming issues.casMetadata", err)
	}
	if _, statErr := os.Stat(filepath.Join(scope, ".beads")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a refused connect wrote %s (stat err %v)", filepath.Join(scope, ".beads"), statErr)
	}
}

// TestConnectRemoteScopeNeedsConsentToSwitchALocalWorkspace: a scope that
// already holds a local workspace is not switched without --convert-workspace,
// and is with it.
func TestConnectRemoteScopeNeedsConsentToSwitchALocalWorkspace(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	scope := writeRemoteTestScope(t, `{"backend": "dolt", "database": "beads"}`)

	if _, err := ConnectRemoteScope(context.Background(), RemoteConnectRequest{ScopeRoot: scope, URL: server.URL}); !errors.Is(err, ErrRemoteConnectRefused) || !strings.Contains(err.Error(), "--convert-workspace") {
		t.Fatalf("err = %v, want a refusal naming --convert-workspace", err)
	}
	if len(server.requests()) != 0 {
		t.Fatalf("a refused switch dialed the server: %v", server.requests())
	}
	if _, err := ConnectRemoteScope(context.Background(), RemoteConnectRequest{ScopeRoot: scope, URL: server.URL, ConvertWorkspace: true}); err != nil {
		t.Fatalf("connect with consent: %v", err)
	}
	target, err := bdhttp.LoadTarget(filepath.Join(scope, ".beads"))
	if err != nil || target.PreviousBackend != "dolt" {
		t.Fatalf("sidecar = %+v (%v), want previous backend dolt recorded for bd connect --clear", target, err)
	}
}

// TestConnectRemoteScopeSendsTheScopesConfiguredCredential: the verifying
// handshake carries the scope's configured credential and nothing ambient.
func TestConnectRemoteScopeSendsTheScopesConfiguredCredential(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	poisonAmbientCredentialLadder(t)
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	capture := &authCapture{server: server}
	routeOpensThrough(t, capture)
	scope := t.TempDir()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("scope-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	installRemoteCredentialLookup(t, map[string]RemoteCredentialConfig{
		filepath.Clean(scope): {Source: mustCredentialSource(t, "file:"+tokenFile), Scope: "[beads]", FromCity: true},
	})

	result, err := ConnectRemoteScope(context.Background(), RemoteConnectRequest{CityPath: scope, ScopeRoot: scope, URL: server.URL})
	if err != nil {
		t.Fatalf("ConnectRemoteScope: %v", err)
	}
	if !strings.Contains(result.Credential, "[beads]") {
		t.Fatalf("credential label = %q, want the configured [beads] source", result.Credential)
	}
	auth := capture.seen()
	if len(auth) == 0 {
		t.Fatal("no handshake reached the server")
	}
	for _, header := range auth {
		if header != "Bearer scope-token" {
			t.Fatalf("handshake Authorization = %q, want the scope's configured token only", header)
		}
	}
}

// TestRemoteBootGate covers the gate's three answers: a local scope is not its
// business, a capable server passes, and a server missing a required token
// refuses with a typed error naming the scope, wire_compat, the token and the
// native_transport escape hatch.
func TestRemoteBootGate(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")

	t.Run("local scope", func(t *testing.T) {
		scope := writeRemoteTestScope(t, `{"backend": "dolt"}`)
		if _, remote, err := CheckRemoteScopeBootGate(context.Background(), "", scope); remote || err != nil {
			t.Fatalf("gate on a local scope = remote %v err %v, want no verdict", remote, err)
		}
	})
	t.Run("capable server", func(t *testing.T) {
		server := newRemoteHTTPServer(t, fullRemoteCapabilities())
		scope := remoteHTTPScope(t, server)
		result, remote, err := CheckRemoteScopeBootGate(context.Background(), "", scope)
		if !remote || err != nil || result.ID != contract.PreflightCheckWireCompat || result.State != contract.PreflightCheckPass {
			t.Fatalf("gate = %+v remote %v err %v, want a wire_compat PASS", result, remote, err)
		}
	})
	t.Run("optional gap starts", func(t *testing.T) {
		server := newRemoteHTTPServer(t, capabilitiesWithout("issues.update.claim"))
		scope := remoteHTTPScope(t, server)
		result, _, err := CheckRemoteScopeBootGate(context.Background(), "", scope)
		if err != nil || result.State != contract.PreflightCheckWarn || !strings.Contains(result.Summary, "issues.update.claim") {
			t.Fatalf("gate = %+v err %v, want a WARN naming issues.update.claim", result, err)
		}
	})
	t.Run("required gap refuses", func(t *testing.T) {
		server := newRemoteHTTPServer(t, capabilitiesWithout("ready.list", "issues.batchGet"))
		scope := remoteHTTPScope(t, server)
		result, remote, err := CheckRemoteScopeBootGate(context.Background(), "", scope)
		var gate *RemoteCapabilityGateError
		if !remote || result.State != contract.PreflightCheckFail || !errors.As(err, &gate) {
			t.Fatalf("gate = %+v remote %v err %v, want a typed refusal", result, remote, err)
		}
		if gate.ScopeRoot != scope || gate.Check != string(contract.PreflightCheckWireCompat) || len(gate.MissingRequired) != 1 || gate.MissingRequired[0] != "ready.list" {
			t.Fatalf("gate error = %+v, want scope, wire_compat and only the REQUIRED ready.list", gate)
		}
		for _, want := range []string{"wire_compat", "ready.list", scope, `native_transport = "off"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("gate error %q does not name %q", err, want)
			}
		}
	})
	t.Run("unconnected scope refuses", func(t *testing.T) {
		scope := writeRemoteTestScope(t, `{"backend": "http"}`)
		_, remote, err := CheckRemoteScopeBootGate(context.Background(), "", scope)
		var gate *RemoteCapabilityGateError
		if !remote || !errors.As(err, &gate) || !strings.Contains(err.Error(), "not_connected") {
			t.Fatalf("gate on an unattached http scope = remote %v err %v, want a not_connected refusal", remote, err)
		}
	})
}
