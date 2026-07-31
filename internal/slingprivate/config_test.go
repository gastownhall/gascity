package slingprivate

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/slingassert"
)

func writeCAFile(t *testing.T) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(nil, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func envKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return testKid + ":" + base64.StdEncoding.EncodeToString(pub)
}

func TestResolveConfigUnsetServesNothing(t *testing.T) {
	t.Setenv(EnvPubkey, "")
	cfg, err := ResolveConfig()
	if err != nil {
		t.Fatalf("ResolveConfig: %v", err)
	}
	if cfg != nil {
		t.Fatalf("no key configured must yield no boundary, got %+v", cfg)
	}
}

// TestResolveConfigFailsClosedOnPartialConfig is the boot gate: a minting key
// without the transport bindings that pin it to one workload is refused, not
// warned about. Booting that way would accept a captured assertion from any
// peer the client CA trusts.
func TestResolveConfigFailsClosedOnPartialConfig(t *testing.T) {
	key := envKey(t)
	ca := writeCAFile(t)

	cases := []struct {
		name     string
		key      string
		workload string
		ca       string
	}{
		{"key without workload or ca", key, "", ""},
		{"key and workload without ca", key, testWorkload, ""},
		{"key and ca without workload", key, "", ca},
		{"non-spiffe workload", key, "cn=bts-gateway", ca},
		{"unreadable ca", key, testWorkload, filepath.Join(t.TempDir(), "missing.pem")},
		{"malformed key", "not-a-key-spec", testWorkload, ca},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvPubkey, tc.key)
			t.Setenv(EnvWorkload, tc.workload)
			t.Setenv(EnvClientCA, tc.ca)
			if _, err := ResolveConfig(); err == nil {
				t.Fatal("ResolveConfig should have refused a partial configuration")
			}
		})
	}
}

func TestResolveConfigComplete(t *testing.T) {
	t.Setenv(EnvPubkey, envKey(t))
	t.Setenv(EnvWorkload, testWorkload)
	t.Setenv(EnvClientCA, writeCAFile(t))
	t.Setenv(EnvDisabled, "")

	cfg, err := ResolveConfig()
	if err != nil {
		t.Fatalf("ResolveConfig: %v", err)
	}
	if cfg == nil || cfg.Workload != testWorkload || cfg.ClientCA == nil || cfg.Disabled {
		t.Fatalf("unexpected config %+v", cfg)
	}

	// The listener config must require and verify a client certificate; a
	// boundary reachable without one is not a private boundary.
	tc := cfg.TLSConfig(tls.Certificate{})
	if tc.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", tc.ClientAuth)
	}
	if tc.ClientCAs == nil {
		t.Fatal("ClientCAs must be pinned to the configured bundle")
	}
	if tc.MinVersion < tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want TLS 1.2 or better", tc.MinVersion)
	}
}

// TestNewBoundaryRollbackDrill is the AC3 runtime-registry drill at the
// configuration seam: flipping the rollback switch closes sling, the port keeps
// answering with the same uniform rejection, and nothing dispatches.
func TestNewBoundaryRollbackDrill(t *testing.T) {
	t.Setenv(EnvPubkey, envKey(t))
	t.Setenv(EnvWorkload, testWorkload)
	t.Setenv(EnvClientCA, writeCAFile(t))
	t.Setenv(EnvDisabled, "1")

	cfg, err := ResolveConfig()
	if err != nil {
		t.Fatalf("ResolveConfig: %v", err)
	}
	if !cfg.Disabled {
		t.Fatal("rollback switch was not read")
	}
	spy := &spyDispatcher{result: Result{Status: http.StatusAccepted}}
	b, err := NewBoundary(cfg, spy, nil)
	if err != nil {
		t.Fatalf("NewBoundary: %v", err)
	}

	w := httptest.NewRecorder()
	b.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost,
		privatePath(slingassert.CommandSlingCityWork), strings.NewReader("{}")))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if w.Body.String() != string(problemRejected.body) {
		t.Fatalf("rolled-back rejection is distinguishable: %s", w.Body)
	}
	if len(spy.calls) != 0 {
		t.Fatalf("a rolled-back boundary dispatched: %+v", spy.calls)
	}
}

// TestNewBoundaryPinsTheWorkload proves the configured identity is enforced by
// the transport check: a peer the client CA trusts, but carrying a different
// SPIFFE ID, gets no identity at all.
func TestNewBoundaryPinsTheWorkload(t *testing.T) {
	t.Setenv(EnvPubkey, envKey(t))
	t.Setenv(EnvWorkload, testWorkload)
	t.Setenv(EnvClientCA, writeCAFile(t))
	t.Setenv(EnvDisabled, "")

	cfg, err := ResolveConfig()
	if err != nil {
		t.Fatalf("ResolveConfig: %v", err)
	}
	match := cfg.workloadMatcher()

	if _, ok := match(spiffeRequest(t, testWorkload)); !ok {
		t.Fatal("the configured workload was not accepted")
	}
	if _, ok := match(spiffeRequest(t, "spiffe://gasworks/ns/other/sa/broker")); ok {
		t.Fatal("an unconfigured workload was accepted")
	}
}

func spiffeRequest(t *testing.T, id string) *http.Request {
	t.Helper()
	u, err := url.Parse(id)
	if err != nil {
		t.Fatalf("parse %q: %v", id, err)
	}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.TLS = &tls.ConnectionState{
		HandshakeComplete: true,
		VerifiedChains:    [][]*x509.Certificate{{{URIs: []*url.URL{u}}}},
	}
	return r
}
