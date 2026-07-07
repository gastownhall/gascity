// Package clientgrant implements the client-side grant exec contract used to
// authorize a MUTATION against a direct hardened city over the control plane. It
// runs a user-configured grant command (a per-request signer that re-validates)
// to mint a single-use X-GC-City-Write token bound to exactly one request, and
// returns it for attachment to that request.
//
// Unlike a transport bearer (see internal/clientauth), a grant is NOT cached: it
// is single-use and request-bound, so every mutation — including a retry of an
// identical one — mints a fresh grant. The request binding is handed to the
// command as JSON in the GC_GRANT_INFO environment variable — never on argv, so
// it is not visible in `ps` — and any inherited GC_*_INFO is stripped so a
// nested exec cannot see a stale request. The key never enters gc; the command
// re-validates the audience/city, recomputes the request digest, stamps the
// remaining claims, and ed25519-signs out of tree. The contract is versioned so
// a helper can evolve.
package clientgrant

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Version identifies the grant exec contract. It is echoed to the grant command
// in the GC_GRANT_INFO payload so a helper can branch on the schema it expects.
const Version = "gascity.dev/city-write-grant/v1"

// GrantInfoEnv is the environment variable carrying the JSON request binding to
// the grant command.
const GrantInfoEnv = "GC_GRANT_INFO"

// GrantInfo is the JSON handed to the grant command via GC_GRANT_INFO. It
// carries the request binding as separate fields so the command can re-validate
// the audience and city and independently recompute the request digest rather
// than blindly signing whatever it is handed. BodySHA256 is the lowercase hex
// SHA-256 of the request body; the command never receives the body itself.
type GrantInfo struct {
	Version string `json:"version"`
	Aud     string `json:"aud"`
	City    string `json:"city"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	// CanonicalQuery MUST be the request's RAW URL query (r.URL.RawQuery) — the
	// exact bytes the server holds — NOT a pre-encoded form. The minter and the
	// server both fold it through the identical canonicalization
	// (citywriteauth.ReqDigest), so passing the raw query round-trips. Passing a
	// url.Values.Encode() form instead can diverge from the raw query for a
	// malformed query (bare semicolons, invalid %XX), yielding a digest the
	// server will not match.
	CanonicalQuery string `json:"canonical_query"`
	BodySHA256     string `json:"body_sha256"`
	ReqDigest      string `json:"req_digest"`
}

// runFunc executes a grant command; injectable so tests need no real exec.
type runFunc func(ctx context.Context, command string, info GrantInfo) (token string, err error)

// GrantSource execs a grant command to mint a single-use, request-bound grant
// token. It holds NO cache: a grant authorizes exactly one request, so a caller
// mints one per mutation and a retry mints a fresh grant. It is safe for
// concurrent use — it owns no mutable state.
type GrantSource struct {
	command string
	runner  runFunc
}

// NewGrantSource builds a source for command. The command is a per-request
// signer (e.g. the reference gc-write-mint) that receives the request binding
// via GC_GRANT_INFO and prints a token to stdout.
func NewGrantSource(command string) (*GrantSource, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, errors.New("clientgrant: grant command is empty")
	}
	return &GrantSource{command: command, runner: runGrantCommand}, nil
}

// Mint runs the grant command for one request binding and returns its token. It
// stamps the contract Version onto info so the caller cannot get it wrong, execs
// the command fresh (no cache), and shape-validates the returned token before
// handing it back. It is the caller's job to attach the token as the
// X-GC-City-Write header on the exact request the binding describes.
func (s *GrantSource) Mint(info GrantInfo) (string, error) {
	info.Version = Version
	token, err := s.runner(context.Background(), s.command, info)
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if err := validateTokenShape(token); err != nil {
		return "", err
	}
	return token, nil
}

// validateTokenShape rejects a token that is not a well-formed
// base64url(payload) "." base64url(signature) pair with an ed25519-sized
// signature. gc holds no verifying key, so this cannot authenticate the grant —
// it only catches a helper that printed an error, a truncated token, or a
// wrong-algorithm signature, failing loudly at mint time rather than letting the
// server reject a garbage header with an opaque 401.
func validateTokenShape(token string) error {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok || payload == "" || sig == "" {
		return fmt.Errorf("clientgrant: grant command returned a malformed token")
	}
	if _, err := base64.RawURLEncoding.DecodeString(payload); err != nil {
		return fmt.Errorf("clientgrant: grant token payload is not base64url: %w", err)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("clientgrant: grant token signature is not base64url: %w", err)
	}
	if len(sigBytes) != ed25519.SignatureSize {
		return fmt.Errorf("clientgrant: grant token signature is %d bytes, want %d", len(sigBytes), ed25519.SignatureSize)
	}
	return nil
}

// runGrantCommand runs command via "sh -c" with the request binding JSON in
// GC_GRANT_INFO (env only — never argv, so the request is not visible in `ps`).
// Inherited GC_*_INFO variables are stripped so a nested exec cannot read a
// stale request. cmd.Output captures stderr into the returned error on failure.
func runGrantCommand(ctx context.Context, command string, info GrantInfo) (string, error) {
	payload, err := json.Marshal(info)
	if err != nil {
		return "", fmt.Errorf("clientgrant: encoding grant info: %w", err)
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = append(strippedEnv(), GrantInfoEnv+"="+string(payload))
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("clientgrant: running grant command: %w", withStderr(err))
	}
	return string(out), nil
}

// strippedEnv returns the current environment minus any GC_*_INFO variable, so a
// grant command (and anything it spawns) never inherits a stale exec/grant
// request that would let it mint against a different call.
func strippedEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "GC_") && strings.HasSuffix(key, "_INFO") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// withStderr enriches an *exec.ExitError with the (bounded) captured stderr so a
// failing grant command produces an actionable diagnostic.
func withStderr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		msg := strings.TrimSpace(string(ee.Stderr))
		if len(msg) > 512 {
			msg = msg[:512] + "…"
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return err
}
