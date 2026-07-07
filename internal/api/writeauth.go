package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

// Write-auth gates per-city write mutations on a signed, single-use,
// request-bound grant when a verifying key is configured. It covers every
// mutating request to an already-registered city (the routes under
// /v0/city/{cityName}), not only config edits. It is an opt-in hardening on top
// of the existing CSRF/read-only checks: with no key configured the middleware
// is not installed and mutations follow the prior guards; with a key configured
// it is fail-closed — every city-scoped mutation must present a valid grant
// minted by the configured trusted authority.
//
// The bundled first-party callers (the gc API client and dashboard SPA) send
// only the CSRF header and mint no grant, so enabling the gate turns their
// direct city mutations away with a clear 401; an authority-fronted deployment
// supplies grants out of band rather than minting them in this process.
const (
	writeAuthHeader   = "X-GC-City-Write"
	writeAuthAudience = "gc-city-write"

	// maxWriteBodyBytes caps the request body the middleware buffers to compute
	// the request digest, so an unauthenticated caller cannot exhaust memory by
	// streaming a huge body before verification.
	maxWriteBodyBytes = 1 << 20 // 1 MiB

	// writeAuthMaxTTL and writeAuthSkew bound grant lifetime and clock drift.
	// The minter and verifier share a pod, so drift is small.
	writeAuthMaxTTL = 2 * time.Minute
	writeAuthSkew   = 30 * time.Second
)

// cityScopedObjectMutation reports whether path targets an existing city whose
// config the write-auth gate must cover, returning the city name. It matches the
// per-city typed gc routes: /v0/city/{cityName} (the suspend/resume PATCH) and
// /v0/city/{cityName}/<sub-resource>. It excludes:
//   - registry creation (POST /v0/city) and the bare /v0/city/ (empty name): a
//     grant binds a path-resident city name, so creating a city — which carries
//     no city in its path yet — stays governed by the prior supervisor-registry
//     guards, not this gate. Write-auth covers mutations of cities that already
//     exist (including unregister, which does carry the city in its path).
//   - any other non-city path,
//   - an empty sub-resource (/v0/city/{name}/),
//   - the /svc/ workspace-service pass-through, which cannot mutate gc config
//     objects and applies its own publication rules.
//
// The /hook/ webhook receiver is deliberately NOT exempted (the H2 reversal): a
// /hook/{name} POST dispatches order → sh -c authenticated by a verifier a pack
// may author, so when write-auth is configured it stays gated on the operator's
// signed grant. Signature verification (E4) is an ADDITIONAL gate for public
// webhooks, never a replacement for this one. Do not add a /hook/ exemption here.
func cityScopedObjectMutation(path string) (city string, ok bool) {
	const prefix = "/v0/city/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rest := path[len(prefix):]
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		// /v0/city/{name} with no sub-resource — the suspend/resume PATCH is a
		// real city mutation and must be gated.
		if rest == "" {
			return "", false // "/v0/city/"
		}
		return rest, true
	}
	if slash == 0 {
		return "", false // "/v0/city//..." — empty city name
	}
	city = rest[:slash]
	sub := rest[slash:] // begins with "/"
	if sub == "/" {
		return "", false // empty sub-resource ("/v0/city/{name}/")
	}
	if sub == "/svc" || strings.HasPrefix(sub, "/svc/") {
		return "", false // workspace-service pass-through is exempt
	}
	return city, true
}

// writeAuthMiddleware enforces a valid X-GC-City-Write grant on every
// city-scoped mutation. Non-mutations and non-city-scoped routes pass through
// untouched. It buffers and resets the body so the downstream handler still
// parses it, and binds the grant to this exact method+path+query+body.
//
// The single-use grant is verified — and its jti consumed — only after the
// front-door mutation guards (CSRF token presence and read-only mode) accept
// the request. Those guards live downstream in the Huma stack, but the
// consuming Verify call is here at the mux layer, so a request they would
// reject must be turned away here first; otherwise a valid grant is spent on a
// request that never mutates and the legitimate retry is misread as a replay.
func writeAuthMiddleware(v *citywriteauth.Verifier, readOnly bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isMutationMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		city, ok := cityScopedObjectMutation(r.URL.Path)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		// Fail closed on control characters in a gated path. The digest preimage
		// is newline-delimited and r.URL.Path can carry a decoded \n/\r/NUL from
		// %0A/%0D/%00, so reject before digesting — otherwise a query-bearing
		// grant could share a preimage with a newline-bearing, query-less path.
		// Such paths also fail exact-match routing, so this rejects nothing a
		// handler would otherwise serve.
		if strings.ContainsAny(r.URL.Path, "\n\r\x00") {
			problemWriteAuthBadPath.writeTo(w)
			return
		}

		token := r.Header.Get(writeAuthHeader)
		if token == "" {
			problemWriteAuthMissingGrant.writeTo(w)
			return
		}

		// Front-door mutation guards run before the grant is verified so a
		// request the server will reject anyway never consumes the single-use
		// grant. These mirror the downstream Huma CSRF and read-only guards, but
		// the consuming Verify call sits here at the mux layer ahead of them, so
		// the cheaper rejections must be evaluated here too — otherwise the jti is
		// spent and the caller's legitimate retry is misclassified as replay.
		if r.Header.Get(csrfHeaderName) == "" {
			problemWriteAuthCSRF.writeTo(w)
			return
		}
		if readOnly {
			problemWriteAuthReadOnly.writeTo(w)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWriteBodyBytes))
		if err != nil {
			// Fail closed either way, but report the genuine cause: a 413 only
			// for an oversize body, otherwise a 400 for a transport-level read
			// failure (client disconnect, reset, timeout) so audit logs are not
			// all mislabelled "body too large".
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				problemWriteAuthBodyTooLarge.writeTo(w)
			} else {
				problemWriteAuthBadBody.writeTo(w)
			}
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		expect := citywriteauth.Expect{
			City:      city,
			ReqDigest: citywriteauth.ReqDigest(r.Method, r.URL.Path, r.URL.RawQuery, body),
		}
		if _, err := v.Verify(token, expect); err != nil {
			// Deliberately generic to the client (no verification oracle); the
			// specific reason is for server-side audit, not the response.
			problemWriteAuthRejected.writeTo(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Pre-serialized RFC 9457 problem responses for the write-auth gate. Like the
// other mux-level problemBody values, pre-serialization keeps json.Marshal off
// the rejection path (Principle 8) and matches the typed-wire convention instead
// of hand-encoding a map[string]any.
var (
	problemWriteAuthMissingGrant = problemBody{
		status: http.StatusUnauthorized,
		body:   []byte(`{"status":401,"title":"Unauthorized","detail":"missing ` + writeAuthHeader + ` grant"}`),
	}
	problemWriteAuthRejected = problemBody{
		status: http.StatusForbidden,
		body:   []byte(`{"status":403,"title":"Forbidden","detail":"write grant rejected"}`),
	}
	problemWriteAuthBodyTooLarge = problemBody{
		status: http.StatusRequestEntityTooLarge,
		body:   []byte(`{"status":413,"title":"Request Entity Too Large","detail":"request body exceeds limit"}`),
	}
	problemWriteAuthBadBody = problemBody{
		status: http.StatusBadRequest,
		body:   []byte(`{"status":400,"title":"Bad Request","detail":"could not read request body"}`),
	}
	problemWriteAuthBadPath = problemBody{
		status: http.StatusBadRequest,
		body:   []byte(`{"status":400,"title":"Bad Request","detail":"invalid characters in request path"}`),
	}
	// problemWriteAuthCSRF and problemWriteAuthReadOnly are emitted by the
	// write-auth gate for the front-door checks it evaluates ahead of grant
	// consumption. Their detail text matches the downstream Huma CSRF/read-only
	// guards so a client sees the same rejection whether or not write-auth is on.
	problemWriteAuthCSRF = problemBody{
		status: http.StatusForbidden,
		body:   []byte(`{"status":403,"title":"Forbidden","detail":"csrf: X-GC-Request header required on mutation endpoints"}`),
	}
	problemWriteAuthReadOnly = problemBody{
		status: http.StatusForbidden,
		body:   []byte(`{"status":403,"title":"Forbidden","detail":"read_only: mutations disabled: server bound to non-localhost address"}`),
	}
)

// parseVerifyKeys parses a verifying-key set of the form
// "kid:base64,kid2:base64" where each base64 is the standard-encoded 32-byte
// ed25519 public key. At least one well-formed entry is required.
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
			return nil, fmt.Errorf("write-auth key %q: want kid:base64", part)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return nil, fmt.Errorf("write-auth key %q: %w", kid, err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("write-auth key %q: wrong public-key size %d", kid, len(raw))
		}
		keys[kid] = ed25519.PublicKey(raw)
	}
	if len(keys) == 0 {
		return nil, errors.New("write-auth: no verifying keys parsed")
	}
	return keys, nil
}

// ResolveWriteAuthVerifier builds a write-auth verifier from the configured key
// material, preferring the GC_CITY_WRITE_PUBKEY env over the supplied config
// value. It returns (nil, nil) when no key is configured and write-auth is not
// required. When write-auth is required (configRequired, or
// GC_CITY_WRITE_REQUIRED=1) but no key is present it returns an error so the
// caller can fail closed at boot rather than serve mutations unguarded.
func ResolveWriteAuthVerifier(configKey string, configRequired bool) (*citywriteauth.Verifier, error) {
	raw := strings.TrimSpace(os.Getenv("GC_CITY_WRITE_PUBKEY"))
	if raw == "" {
		raw = strings.TrimSpace(configKey)
	}
	required := configRequired || os.Getenv("GC_CITY_WRITE_REQUIRED") == "1"
	if raw == "" {
		if required {
			return nil, errors.New("write-auth required but no verifying key configured")
		}
		return nil, nil // not enabled
	}
	keys, err := parseVerifyKeys(raw)
	if err != nil {
		return nil, err
	}
	var epochFloor int64
	if e := strings.TrimSpace(os.Getenv("GC_CITY_WRITE_EPOCH_FLOOR")); e != "" {
		epochFloor, err = strconv.ParseInt(e, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("GC_CITY_WRITE_EPOCH_FLOOR: %w", err)
		}
	}
	return citywriteauth.New(citywriteauth.Options{
		Aud:        writeAuthAudience,
		Keys:       keys,
		EpochFloor: epochFloor,
		MaxTTL:     writeAuthMaxTTL,
		Skew:       writeAuthSkew,
	})
}

// InstallWriteAuth resolves the write-auth verifier from config + env and, when
// configured, installs it on sm — the single seam every serve path uses so none
// can forget to gate writes. It fails closed: if write-auth is required
// (configRequired or GC_CITY_WRITE_REQUIRED=1) but no usable key is configured,
// it returns an error so the caller can refuse to start.
func InstallWriteAuth(sm *SupervisorMux, configKey string, configRequired bool) error {
	v, err := ResolveWriteAuthVerifier(configKey, configRequired)
	if err != nil {
		return err
	}
	if v != nil {
		sm.WithWriteAuth(v)
	}
	return nil
}
