package beads

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	bdhttp "github.com/steveyegge/beads/backend/http"

	"github.com/gastownhall/gascity/internal/beads/credsource"
)

const leakedSecret = "LEAKED-s3cr3t-value"

// installRemoteCredentialLookup installs a per-scope lookup answering from
// byScope (keyed by scope root), the way gc's composition root installs its
// city.toml reader.
func installRemoteCredentialLookup(t *testing.T, byScope map[string]RemoteCredentialConfig) {
	t.Helper()
	SetRemoteCredentialLookup(func(_, scopeRoot string) (RemoteCredentialConfig, bool, error) {
		cfg, ok := byScope[scopeRoot]
		return cfg, ok, nil
	})
	t.Cleanup(func() { SetRemoteCredentialLookup(nil) })
}

func mustCredentialSource(t *testing.T, raw string) credsource.Source {
	t.Helper()
	src, err := credsource.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// poisonAmbientCredentialLadder configures every rung of bd's ambient ladder
// and the ambient plaintext grant for the test server, so any request that
// fell back to it would carry "ambient-*".
func poisonAmbientCredentialLadder(t *testing.T) {
	t.Helper()
	t.Setenv(bdhttp.TokenEnv, "127.0.0.1:9=ambient-env-token")
	t.Setenv(bdhttp.TokenCommandEnv, "127.0.0.1:9=echo ambient-command-token")
	credentials := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(credentials, []byte("[127.0.0.1:9]\npassword=ambient-file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", credentials)
	t.Setenv("BEADS_HTTP_ALLOW_INSECURE", "1")
}

// scopeAt writes a scope activated for the http backend at base.
func scopeAt(t *testing.T, base string, allowPlaintext bool) string {
	t.Helper()
	scope := writeRemoteTestScope(t, `{"backend": "http"}`)
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := bdhttp.SaveTarget(filepath.Join(scope, ".beads"), bdhttp.Target{BaseURL: u, ExpectProjectID: remoteHTTPProjectID, AllowInsecureCredential: allowPlaintext}); err != nil {
		t.Fatal(err)
	}
	return scope
}

// since returns the requests and Authorization headers recorded from index from.
func (t *authRecordingTransport) since(from int) (requests, auth []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.requestSeen[from:]...), append([]string(nil), t.auth[from:]...)
}

func (t *authRecordingTransport) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.requestSeen)
}

// openAndReadAs opens scope natively under city and performs one role read,
// returning every request (and its Authorization header) the open made: the
// wire_compat handshake and the store's own requests.
func openAndReadAs(t *testing.T, recorder *authRecordingTransport, scope, city string) ([]string, []string) {
	t.Helper()
	from := recorder.count()
	native := openRemoteForTest(t, scope, city)
	if _, err := native.Get("gr-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}
	return recorder.since(from)
}

func assertAllAuth(t *testing.T, who string, requests, auth []string, want string) {
	t.Helper()
	sawHandshake := false
	for i, request := range requests {
		if request == "GET /v0/beads/context" {
			sawHandshake = true
		}
		if auth[i] != want {
			t.Errorf("%s: request %q authorized with %q, want %q", who, request, auth[i], want)
		}
	}
	if !sawHandshake || len(requests) < 3 {
		t.Errorf("%s: requests = %v, want the wire_compat handshake and the store's own requests", who, requests)
	}
}

// TestTwoCitiesOnOneServerEachSendTheirOwnCredential is the G7 headline: two
// cities (and a rig with its own credential) in one process, on ONE server,
// each present their own bearer on every request, the wire_compat handshake
// included, and none of them reads the ambient ladder that is configured for
// that very server.
func TestTwoCitiesOnOneServerEachSendTheirOwnCredential(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	recorder := &authRecordingTransport{inner: server}
	routeRemoteOpensThrough(t, recorder)
	poisonAmbientCredentialLadder(t)

	cityA := remoteHTTPScope(t, server)
	cityB := remoteHTTPScope(t, server)
	rig := remoteHTTPScope(t, server)
	inheritingRig := remoteHTTPScope(t, server)
	tokenFile := filepath.Join(t.TempDir(), "city-b-token")
	if err := os.WriteFile(tokenFile, []byte("token-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CITY_A_TOKEN", "token-a")
	t.Setenv("RIG_TOKEN", "token-rig")
	installRemoteCredentialLookup(t, map[string]RemoteCredentialConfig{
		cityA:         {Source: mustCredentialSource(t, "env:CITY_A_TOKEN"), Scope: "[beads]", FromCity: true},
		cityB:         {Source: mustCredentialSource(t, "file:"+tokenFile), Scope: "[beads]", FromCity: true},
		rig:           {Source: mustCredentialSource(t, "env:RIG_TOKEN"), Scope: `rig "r"`},
		inheritingRig: {Source: mustCredentialSource(t, "env:CITY_A_TOKEN"), Scope: "[beads]", FromCity: true},
	})

	requests, auth := openAndReadAs(t, recorder, cityA, cityA)
	assertAllAuth(t, "city A", requests, auth, "Bearer token-a")
	requests, auth = openAndReadAs(t, recorder, cityB, cityB)
	assertAllAuth(t, "city B", requests, auth, "Bearer token-b")
	requests, auth = openAndReadAs(t, recorder, rig, cityA)
	assertAllAuth(t, "rig with its own credential", requests, auth, "Bearer token-rig")
	// A rig on the city's server (same scheme, host and port) takes the
	// city's credential.
	requests, auth = openAndReadAs(t, recorder, inheritingRig, cityA)
	assertAllAuth(t, "rig inheriting city A's credential", requests, auth, "Bearer token-a")
}

// TestPerCityCredentialRefusesAnotherServer: the city's credential is never
// sent to a rig attached to a different server; the open is refused before
// any request.
func TestPerCityCredentialRefusesAnotherServer(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	city := remoteHTTPScope(t, server)
	rig := scopeAt(t, "http://127.0.0.1:10", false)
	t.Setenv("CITY_TOKEN", "token-city")
	installRemoteCredentialLookup(t, map[string]RemoteCredentialConfig{
		rig: {Source: mustCredentialSource(t, "env:CITY_TOKEN"), Scope: "[beads]", FromCity: true},
	})
	_, _, err := ResolveScopeRemoteCredential(context.Background(), city, rig)
	var credErr *RemoteCredentialError
	if !errors.As(err, &credErr) || credErr.Reason != RemoteCredentialReasonServerMismatch || !errors.Is(err, ErrRemoteCredentialServerMismatch) {
		t.Fatalf("error = %v, want a server_mismatch *RemoteCredentialError", err)
	}
	_ = assertRemoteOpenRefusedBeforeAnyRequest(t, server, rig, city)
}

func assertRemoteOpenRefusedBeforeAnyRequest(t *testing.T, server *remoteHTTPServer, scope, city string) error {
	t.Helper()
	before := len(server.requests())
	_, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		CityPath:         city,
		Provider:         "bd",
		NativeTransport:  NativeTransportAuto,
		PreflightChecker: noDoltPreflight(t),
		OpenBdStore: func() (Store, error) {
			t.Fatal("OpenBdStore called for a remote scope under auto")
			return nil, nil
		},
	})
	var required *HTTPNativeOpenRequiredError
	if !errors.As(err, &required) || required.Gate != remoteCredentialGate {
		t.Fatalf("open error = %v, want HTTPNativeOpenRequiredError at gate %s", err, remoteCredentialGate)
	}
	if got := server.requests()[before:]; len(got) != 0 {
		t.Fatalf("requests %v sent after a credential refusal", got)
	}
	return err
}

// TestPerCityCredentialResolutionErrorsAreTypedAndNeverLeak: a configured
// source that fails is a typed, terminal error naming the source (never the
// value, never a helper's output), and it never falls back to the ambient
// ladder.
func TestPerCityCredentialResolutionErrorsAreTypedAndNeverLeak(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	poisonAmbientCredentialLadder(t)
	dir := t.TempDir()
	helper := filepath.Join(dir, "token-helper.sh")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho "+leakedSecret+"\necho "+leakedSecret+" >&2\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	unsetCity := remoteHTTPScope(t, server)
	failingCity := remoteHTTPScope(t, server)
	installRemoteCredentialLookup(t, map[string]RemoteCredentialConfig{
		unsetCity:   {Source: mustCredentialSource(t, "env:UNSET_CITY_TOKEN"), Scope: "[beads]", FromCity: true},
		failingCity: {Source: mustCredentialSource(t, "command:"+helper+" --secret "+leakedSecret), Scope: "[beads]", FromCity: true},
	})
	for _, tc := range []struct {
		scope   string
		cause   error
		naming  string
		errName string
	}{
		{unsetCity, credsource.ErrEmpty, "env:UNSET_CITY_TOKEN", "unset env"},
		{failingCity, credsource.ErrCommandFailed, "command:" + helper, "failing command"},
	} {
		_, _, err := ResolveScopeRemoteCredential(context.Background(), tc.scope, tc.scope)
		var credErr *RemoteCredentialError
		if !errors.As(err, &credErr) || credErr.Reason != RemoteCredentialReasonResolve || !errors.Is(err, tc.cause) {
			t.Fatalf("%s: error = %v, want a resolve *RemoteCredentialError wrapping %v", tc.errName, err, tc.cause)
		}
		if !strings.Contains(err.Error(), tc.naming) || !strings.Contains(err.Error(), "[beads]") {
			t.Errorf("%s: error %q does not name the source %q and its scope", tc.errName, err, tc.naming)
		}
		openErr := assertRemoteOpenRefusedBeforeAnyRequest(t, server, tc.scope, tc.scope)
		for _, text := range []string{err.Error(), openErr.Error()} {
			if strings.Contains(text, leakedSecret) || strings.Contains(text, "ambient") {
				t.Errorf("%s: error %q leaks a credential", tc.errName, text)
			}
		}
	}
}

// TestPerCityCredentialPlaintextRefusal: a bearer over plain http to a
// non-loopback server is refused before any request unless THIS scope's
// configuration allows it; the sidecar's own grant and
// BEADS_HTTP_ALLOW_INSECURE do not count. With allow_insecure_credential the
// open goes through, with the scope's own token.
func TestPerCityCredentialPlaintextRefusal(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	recorder := &authRecordingTransport{inner: server}
	routeRemoteOpensThrough(t, recorder)
	poisonAmbientCredentialLadder(t)
	t.Setenv("CITY_TOKEN", "token-city")
	refused := scopeAt(t, "http://10.20.30.40:9", true) // sidecar grants plaintext; config does not
	allowed := scopeAt(t, "http://10.20.30.41:9", false)
	installRemoteCredentialLookup(t, map[string]RemoteCredentialConfig{
		refused: {Source: mustCredentialSource(t, "env:CITY_TOKEN"), Scope: "[beads]", FromCity: true},
		allowed: {Source: mustCredentialSource(t, "env:CITY_TOKEN"), Scope: "[beads]", FromCity: true, AllowInsecure: true},
	})

	_, _, err := ResolveScopeRemoteCredential(context.Background(), refused, refused)
	var credErr *RemoteCredentialError
	if !errors.As(err, &credErr) || credErr.Reason != RemoteCredentialReasonInsecure || !errors.Is(err, ErrRemoteCredentialInsecure) {
		t.Fatalf("error = %v, want an insecure_transport *RemoteCredentialError", err)
	}
	_ = assertRemoteOpenRefusedBeforeAnyRequest(t, server, refused, refused)

	scoped, ok, err := ResolveScopeRemoteCredential(context.Background(), allowed, allowed)
	if err != nil || !ok || !scoped.Target().AllowInsecureCredential {
		t.Fatalf("allowed scope: ok=%v err=%v; want a credential whose target carries the scope's plaintext grant", ok, err)
	}
	requests, auth := openAndReadAs(t, recorder, allowed, allowed)
	assertAllAuth(t, "allowed plaintext scope", requests, auth, "Bearer token-city")
}

// rotatingAuthServer refuses one retired bearer with a 401, as a server does
// after a token rotation, and serves everything else from inner.
type rotatingAuthServer struct {
	inner   *remoteHTTPServer
	mu      sync.Mutex
	retired string
}

func (s *rotatingAuthServer) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	retired := s.retired
	s.mu.Unlock()
	if retired != "" && r.Header.Get("Authorization") == "Bearer "+retired {
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/problem+json")
		rec.WriteHeader(http.StatusUnauthorized)
		_, _ = rec.Write([]byte(`{"status":401,"code":"unauthenticated","detail":"token not accepted"}`))
		resp := rec.Result()
		resp.Request = r
		return resp, nil
	}
	return s.inner.RoundTrip(r)
}

// TestPerCityCredentialRefreshRereadsTheSource: after a 401 the store re-reads
// its own source (a rotated token) and retries once with the new token.
func TestPerCityCredentialRefreshRereadsTheSource(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	rotating := &rotatingAuthServer{inner: server}
	routeRemoteOpensThrough(t, rotating)
	// A command source: its token is cached between reads, so only the 401
	// refresh can pick the rotated one up inside the cache interval.
	_ = fakeCredentialClock(t)
	city := remoteHTTPScope(t, server)
	source, tokenFile, _ := tokenHelper(t, t.TempDir(), "token-old")
	installRemoteCredentialLookup(t, map[string]RemoteCredentialConfig{
		city: {Source: mustCredentialSource(t, source), Scope: "[beads]", FromCity: true},
	})
	native := openRemoteForTest(t, city, city)

	if err := os.WriteFile(tokenFile, []byte("token-new"), 0o600); err != nil {
		t.Fatal(err)
	}
	rotating.mu.Lock()
	rotating.retired = "token-old"
	rotating.mu.Unlock()
	if _, err := native.Get("gr-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after rotation error = %v, want ErrNotFound via the refreshed token", err)
	}
}

// TestRemoteCredentialSubprocessEnvCarriesOnlyItsOwnScope: the bd child env
// for a scope carries that scope's token in bd's highest ladder rung, scoped
// to the server's host:port, blanks the ambient token command, and is never
// another scope's.
func TestRemoteCredentialSubprocessEnvCarriesOnlyItsOwnScope(t *testing.T) {
	hermeticRemoteHTTPEnv(t)
	RegisterRemoteBackends("gc/test")
	server := newRemoteHTTPServer(t, fullRemoteCapabilities())
	poisonAmbientCredentialLadder(t)
	cityA := remoteHTTPScope(t, server)
	cityB := remoteHTTPScope(t, server)
	ambientOnly := remoteHTTPScope(t, server)
	t.Setenv("CITY_A_TOKEN", "token-a")
	t.Setenv("CITY_B_TOKEN", "token-b")
	installRemoteCredentialLookup(t, map[string]RemoteCredentialConfig{
		cityA: {Source: mustCredentialSource(t, "env:CITY_A_TOKEN"), Scope: "[beads]", FromCity: true},
		cityB: {Source: mustCredentialSource(t, "env:CITY_B_TOKEN"), Scope: "[beads]", FromCity: true},
	})
	for scope, want := range map[string]string{cityA: "token-a", cityB: "token-b"} {
		env := map[string]string{}
		applied, err := NewRemoteCredentialSubprocessEnv(scope, scope).Apply(context.Background(), env)
		if err != nil || !applied {
			t.Fatalf("Apply(%s) = %v, %v", scope, applied, err)
		}
		if got := env["BEADS_HTTP_TOKEN"]; got != "127.0.0.1:9="+want {
			t.Errorf("BEADS_HTTP_TOKEN = %q, want the scope's own token scoped to 127.0.0.1:9", got)
		}
		if got, ok := env["BEADS_HTTP_TOKEN_COMMAND"]; !ok || got != "" {
			t.Errorf("BEADS_HTTP_TOKEN_COMMAND = %q (set %v), want blanked", got, ok)
		}
		if got := env["BEADS_HTTP_ALLOW_INSECURE"]; got != "" {
			t.Errorf("BEADS_HTTP_ALLOW_INSECURE = %q, want blanked (no scope grant)", got)
		}
		if got, ok := env["BEADS_HTTP_CA_FILE"]; !ok || got != "" {
			t.Errorf("BEADS_HTTP_CA_FILE = %q (set %v), want blanked (the sidecar's ca_file alone)", got, ok)
		}
	}
	env := map[string]string{}
	applied, err := NewRemoteCredentialSubprocessEnv(ambientOnly, ambientOnly).Apply(context.Background(), env)
	if err != nil || applied || len(env) != 0 {
		t.Fatalf("scope without a per-scope credential: applied=%v err=%v env=%v; want bd's ambient ladder untouched", applied, err, env)
	}
}
