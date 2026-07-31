package slingprivate

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/slingassert"
)

// Env vars. They mirror the GC_CITY_WRITE_* naming the city-write gate uses, so
// an operator configuring both planes reads one convention.
const (
	// EnvPubkey is the BTS minting key set, "kid:base64[,kid2:base64]", each
	// base64 the standard-encoded 32-byte ed25519 public key.
	EnvPubkey = "GC_CITY_SLING_PUBKEY"
	// EnvWorkload is the exact SPIFFE ID the BTS sling gateway presents.
	// Required alongside a key: without it the boundary would accept an
	// assertion from any peer the client CA trusts.
	EnvWorkload = "GC_CITY_SLING_WORKLOAD"
	// EnvClientCA is a path to the PEM bundle that must sign the gateway's
	// client certificate. Required alongside a key.
	EnvClientCA = "GC_CITY_SLING_CLIENT_CA"
	// EnvDisabled is the rollback switch. Set to 1 to withdraw the sling
	// command from the registry: the private boundary then refuses every sling
	// assertion and opens nothing in its place.
	EnvDisabled = "GC_CITY_SLING_DISABLED"
)

// maxAssertionTTL and assertionSkew bound assertion lifetime and clock drift.
// They are deliberately tighter than the city-write grant's: an assertion
// crosses one service hop between two processes an operator runs together, so
// there is no reason for a long window.
const (
	maxAssertionTTL = 60 * time.Second
	assertionSkew   = 15 * time.Second
)

// Config is the resolved private-boundary configuration.
type Config struct {
	Keys     map[string]ed25519.PublicKey
	Workload string
	ClientCA *x509.CertPool
	// Disabled reports that the rollback switch is on. The boundary is still
	// constructed — so the port keeps answering with the same uniform rejection
	// rather than vanishing — but no command is registered.
	Disabled bool
}

// ResolveConfig reads the private-boundary configuration from the environment.
// It returns (nil, nil) when no minting key is configured, which is the normal
// state for a City that fronts no BTS gateway: the caller then serves no
// private boundary at all rather than serving an unauthenticated one.
//
// A key WITHOUT a workload identity or a client CA is an error, not a warning.
// Both are what bind an assertion to the one workload allowed to present it; a
// deployment that configures a key and omits either would accept a captured
// assertion from any peer the transport trusts, which is precisely the
// unknown-workload case this boundary exists to refuse.
func ResolveConfig() (*Config, error) {
	raw := strings.TrimSpace(os.Getenv(EnvPubkey))
	if raw == "" {
		return nil, nil
	}
	keys, err := parseVerifyKeys(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvPubkey, err)
	}
	workload := strings.TrimSpace(os.Getenv(EnvWorkload))
	if workload == "" {
		return nil, fmt.Errorf("%s is required when %s is set", EnvWorkload, EnvPubkey)
	}
	if !strings.HasPrefix(workload, "spiffe://") {
		return nil, fmt.Errorf("%s must be a spiffe:// id", EnvWorkload)
	}
	caPath := strings.TrimSpace(os.Getenv(EnvClientCA))
	if caPath == "" {
		return nil, fmt.Errorf("%s is required when %s is set", EnvClientCA, EnvPubkey)
	}
	pem, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvClientCA, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: no certificates in %s", EnvClientCA, caPath)
	}
	return &Config{
		Keys:     keys,
		Workload: workload,
		ClientCA: pool,
		Disabled: os.Getenv(EnvDisabled) == "1",
	}, nil
}

// NewBoundary builds the private boundary a resolved configuration describes,
// wiring the workload matcher, the assertion verifier and the command registry
// so a caller cannot assemble a weaker combination by hand.
//
// A configuration with the rollback switch on yields a CLOSED boundary: the
// port keeps answering, with the same uniform rejection every other refusal
// gets, and no command is registered. Closing this way rather than unmounting
// the route means rollback changes what the boundary decides, not what exists —
// a caller cannot tell a withdrawn command from any other rejection, and
// nothing else becomes reachable in sling's place.
func NewBoundary(cfg *Config, dispatcher Dispatcher, results ResultStore) (*Boundary, error) {
	if cfg == nil {
		return nil, errors.New("slingprivate: no configuration")
	}
	if cfg.Disabled {
		return &Boundary{
			closed:     true,
			dispatcher: dispatcher,
			auditf:     log.Printf,
		}, nil
	}
	verifier, err := slingassert.New(slingassert.Options{
		Keys: cfg.Keys,
		Commands: map[string]slingassert.Command{
			slingassert.CommandSlingCityWork: {
				PolicyID:    slingassert.PolicySlingCityWork,
				OperationID: slingassert.CommandSlingCityWork,
			},
		},
		MaxTTL: maxAssertionTTL,
		Skew:   assertionSkew,
	})
	if err != nil {
		return nil, err
	}
	return New(Options{
		Verifier:   verifier,
		Dispatcher: dispatcher,
		Workload:   cfg.workloadMatcher(),
		Results:    results,
	})
}

// TLSConfig returns the server TLS configuration the private listener must use:
// a client certificate is required and verified against the configured CA. It
// is returned rather than applied so the caller supplies its own server
// certificate; every other field here is the part that must not be weakened.
func (c *Config) TLSConfig(serverCert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    c.ClientCA,
		MinVersion:   tls.VersionTLS13,
	}
}

// workloadMatcher yields an identity only for the one configured workload.
// SPIFFEWorkload alone would accept any SPIFFE ID the client CA signs, leaving
// the assertion's workload claim compared against whatever the peer chose to
// present; pinning it here means an unexpected workload is refused by the
// transport check, before the assertion is even parsed.
func (c *Config) workloadMatcher() func(*http.Request) (string, bool) {
	want := c.Workload
	return func(r *http.Request) (string, bool) {
		got, ok := SPIFFEWorkload(r)
		if !ok || got != want {
			return "", false
		}
		return got, true
	}
}

// parseVerifyKeys parses "kid:base64,kid2:base64" where each base64 is the
// standard-encoded 32-byte ed25519 public key. At least one entry is required.
// It mirrors the city-write gate's key grammar so one operator convention
// covers both planes.
func parseVerifyKeys(s string) (map[string]ed25519.PublicKey, error) {
	keys := make(map[string]ed25519.PublicKey)
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kid, b64, ok := strings.Cut(part, ":")
		kid = strings.TrimSpace(kid)
		if !ok || kid == "" {
			return nil, fmt.Errorf("verify key %q: want kid:base64", part)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return nil, fmt.Errorf("verify key %q: %w", kid, err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("verify key %q: want %d bytes, got %d", kid, ed25519.PublicKeySize, len(raw))
		}
		keys[kid] = ed25519.PublicKey(raw)
	}
	if len(keys) == 0 {
		return nil, errors.New("no verifying keys parsed")
	}
	return keys, nil
}
