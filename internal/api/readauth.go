package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

// Read-auth gates per-city reads on a signed, single-use, request-bound grant
// when a verifying key is configured. It is the read-side twin of write-auth: it
// covers every GET/HEAD to an already-registered city on the typed per-city API
// (the routes under /v0/city/{cityName}), so a deployment can require an
// authenticated grant to read a city's beads, mail, sessions, and agent
// transcripts rather than trusting network position. It is opt-in hardening: with
// no key configured the middleware is not installed and reads follow the prior
// behavior; with a key configured it is fail-closed — every city-scoped read must
// present a valid grant minted by the configured trusted authority.
//
// Scope boundary: this covers ONLY the typed /v0/city/{cityName} read routes. It
// deliberately does NOT gate the supervisor-scope aggregate event feed
// (/v0/events, /v0/events/stream — which multiplex per-city events across all
// running cities) nor the default-on dashboard host plane (/api/*, including its
// /api/city/{cityName}/* per-city samplers, run detail/diff, and config reads),
// which expose per-city data on the same listener. Those surfaces are covered by
// the grant-minting authority/edge when it fronts the whole listener; gating them
// in-process is follow-up work under the supervisor-scope grant. See the
// ReadAuthVerifyKey config doc for the operator guidance.
//
// The bundled first-party callers (the gc API client and dashboard SPA) mint no
// grant, so enabling the gate turns their direct /v0/city reads away with a clear
// 401; an authority-fronted deployment supplies grants out of band rather than
// minting them in this process.
const (
	readAuthHeader   = "X-GC-City-Read"
	readAuthAudience = "gc-city-read"

	// readAuthMaxTTL and readAuthSkew bound grant lifetime and clock drift.
	// Kept as independent consts from the write-auth pair so the tiers can
	// diverge later; the minter and verifier share a pod, so drift is small.
	readAuthMaxTTL = 2 * time.Minute
	readAuthSkew   = 30 * time.Second
)

// readAuthMiddleware enforces a valid X-GC-City-Read grant on every city-scoped
// read (GET/HEAD). Mutations and non-city-scoped routes pass through untouched.
//
// Unlike the write gate it deliberately has no CSRF or read-only front-door
// checks — a read changes no state (so CSRF is moot and the browser same-origin
// policy already blocks a cross-site attacker from reading the response) and
// reads must keep working in read-only mode — and it buffers no request body,
// because a GET/HEAD carries none. The grant is therefore bound to
// method+path+query over an empty body and consumed exactly at admission; there
// are no cheap pre-checks between token presence and verification, so the
// don't-burn-the-jti ordering the write path needs does not apply here. The
// single-use grant is consumed even when the downstream handler later 404s or
// 500s, which is harmless.
//
// For streaming reads (SSE feeds under a city) the gate runs at connect only and
// wraps nothing around the ResponseWriter, so flushing/streaming pass through
// untouched. Each reconnect (including Last-Event-ID resumes) is a fresh request
// needing a fresh grant.
func readAuthMiddleware(v *citywriteauth.Verifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		city, ok := cityScopedObjectPath(r.URL.Path)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		// Fail closed on control characters in a gated path: the digest preimage
		// is newline-delimited and r.URL.Path can carry a decoded \n/\r/NUL from
		// %0A/%0D/%00, so reject before digesting. Such paths also fail exact-match
		// routing, so this rejects nothing a handler would otherwise serve.
		if strings.ContainsAny(r.URL.Path, "\n\r\x00") {
			problemReadAuthBadPath.writeTo(w)
			return
		}

		token := r.Header.Get(readAuthHeader)
		if token == "" {
			problemReadAuthMissingGrant.writeTo(w)
			return
		}

		expect := citywriteauth.Expect{
			City:      city,
			ReqDigest: citywriteauth.ReqDigest(r.Method, r.URL.Path, r.URL.RawQuery, nil),
		}
		if _, err := v.Verify(token, expect); err != nil {
			// Deliberately generic to the client (no verification oracle); the
			// specific reason is for server-side audit, not the response.
			problemReadAuthRejected.writeTo(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Pre-serialized RFC 9457 problem responses for the read-auth gate. Like the
// other mux-level problemBody values, pre-serialization keeps json.Marshal off
// the rejection path (Principle 8) and matches the typed-wire convention instead
// of hand-encoding a map[string]any.
var (
	problemReadAuthMissingGrant = problemBody{
		status: http.StatusUnauthorized,
		body:   []byte(`{"status":401,"title":"Unauthorized","detail":"missing ` + readAuthHeader + ` grant"}`),
	}
	problemReadAuthRejected = problemBody{
		status: http.StatusForbidden,
		body:   []byte(`{"status":403,"title":"Forbidden","detail":"read grant rejected"}`),
	}
	problemReadAuthBadPath = problemBody{
		status: http.StatusBadRequest,
		body:   []byte(`{"status":400,"title":"Bad Request","detail":"invalid characters in request path"}`),
	}
)

// ResolveReadAuthVerifier builds a read-auth verifier from the configured key
// material, preferring the GC_CITY_READ_PUBKEY env over the supplied config
// value. It returns (nil, nil) when no key is configured and read-auth is not
// required. When read-auth is required (configRequired, or
// GC_CITY_READ_REQUIRED=1) but no key is present it returns an error so the
// caller can fail closed at boot rather than serve reads unguarded.
func ResolveReadAuthVerifier(configKey string, configRequired bool) (*citywriteauth.Verifier, error) {
	raw := strings.TrimSpace(os.Getenv("GC_CITY_READ_PUBKEY"))
	if raw == "" {
		raw = strings.TrimSpace(configKey)
	}
	required := configRequired || os.Getenv("GC_CITY_READ_REQUIRED") == "1"
	if raw == "" {
		if required {
			return nil, errors.New("read-auth required but no verifying key configured")
		}
		return nil, nil // not enabled
	}
	keys, err := parseVerifyKeys(raw)
	if err != nil {
		return nil, err
	}
	var epochFloor int64
	if e := strings.TrimSpace(os.Getenv("GC_CITY_READ_EPOCH_FLOOR")); e != "" {
		epochFloor, err = strconv.ParseInt(e, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("GC_CITY_READ_EPOCH_FLOOR: %w", err)
		}
	}
	return citywriteauth.New(citywriteauth.Options{
		Aud:        readAuthAudience,
		Keys:       keys,
		EpochFloor: epochFloor,
		MaxTTL:     readAuthMaxTTL,
		Skew:       readAuthSkew,
	})
}

// InstallReadAuth resolves the read-auth verifier from config + env and, when
// configured, installs it on sm — the single seam every serve path uses so none
// can forget to gate reads. It fails closed: if read-auth is required
// (configRequired or GC_CITY_READ_REQUIRED=1) but no usable key is configured,
// it returns an error so the caller can refuse to start.
func InstallReadAuth(sm *SupervisorMux, configKey string, configRequired bool) error {
	v, err := ResolveReadAuthVerifier(configKey, configRequired)
	if err != nil {
		return err
	}
	if v != nil {
		sm.WithReadAuth(v)
	}
	return nil
}
