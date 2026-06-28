package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
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

// Write-auth gates city-config mutations on a signed, single-use, request-bound
// grant when a verifying key is configured. It is an opt-in hardening on top of
// the existing CSRF/read-only checks: with no key configured the middleware is
// not installed and mutations follow the prior guards; with a key configured it
// is fail-closed — every city-scoped mutation must present a valid grant minted
// by the configured trusted authority.
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

// cityScopedObjectMutation reports whether path targets a city the write-auth
// gate must cover, returning the city name. It matches the per-city typed gc
// routes: /v0/city/{cityName} (the suspend/resume PATCH) and
// /v0/city/{cityName}/<sub-resource>. It excludes:
//   - the bare /v0/city/ (empty name), /v0/cities, and any non-city path,
//   - an empty sub-resource (/v0/city/{name}/),
//   - the /svc/ workspace-service pass-through, which cannot mutate gc config
//     objects and applies its own publication rules.
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
// parses it, and binds the grant to this exact method+path+body.
func writeAuthMiddleware(v *citywriteauth.Verifier, next http.Handler) http.Handler {
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

		token := r.Header.Get(writeAuthHeader)
		if token == "" {
			writeAuthError(w, http.StatusUnauthorized, "missing "+writeAuthHeader+" grant")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWriteBodyBytes))
		if err != nil {
			writeAuthError(w, http.StatusRequestEntityTooLarge, "request body exceeds limit")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		expect := citywriteauth.Expect{
			City:      city,
			ReqDigest: citywriteauth.ReqDigest(r.Method, r.URL.Path, body),
		}
		if _, err := v.Verify(token, expect); err != nil {
			// Deliberately generic to the client (no verification oracle); the
			// specific reason is for server-side audit, not the response.
			writeAuthError(w, http.StatusForbidden, "write grant rejected")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeAuthError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"title":  http.StatusText(status),
		"status": status,
		"detail": detail,
	})
}

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
