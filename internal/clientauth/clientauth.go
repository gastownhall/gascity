// Package clientauth implements the client-side credential exec contract used to
// operate a REMOTE city over the control plane. It runs a user-configured
// credential command to mint a transport bearer, caches the token until its
// expiry, and re-mints on demand (a per-attempt 401 re-invoke). The request is
// handed to the command as JSON in the GC_EXEC_INFO environment variable — never
// on argv — and any inherited GC_*_INFO is stripped so a nested exec cannot see
// a stale request. The contract is versioned so a helper can evolve.
package clientauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Version identifies the exec contract. It is echoed to the credential command
// in the GC_EXEC_INFO payload so a helper can branch on the schema it expects.
const Version = "gascity.dev/client-auth/v1"

// ExecInfoEnv is the environment variable carrying the JSON request to the
// credential command.
const ExecInfoEnv = "GC_EXEC_INFO"

// expirySkew re-mints a token slightly before it actually expires, so a token
// handed to an in-flight request cannot expire mid-flight.
const expirySkew = 30 * time.Second

// ExecInfo is the JSON handed to the credential command via GC_EXEC_INFO.
type ExecInfo struct {
	Version string   `json:"version"`
	Spec    ExecSpec `json:"spec"`
}

// ExecSpec is the request detail the command needs to mint a scoped token.
type ExecSpec struct {
	ServerURL   string `json:"server_url"`
	City        string `json:"city"`
	Interactive bool   `json:"interactive"`
}

// ExecResult is the JSON the credential command writes to stdout. The
// expiration is REQUIRED: without it the client cannot cache safely, so a
// missing expiration is a hard error rather than a "never expires" assumption.
type ExecResult struct {
	Token               string `json:"token"`
	ExpirationTimestamp string `json:"expiration_timestamp"` // RFC3339
}

// runFunc executes a credential command; injectable so tests need no real exec.
type runFunc func(ctx context.Context, command string, info ExecInfo) (ExecResult, error)

// CredentialSource execs a credential command and caches the minted token until
// expiry. Token returns a live cached token or mints a fresh one; Refresh forces
// a re-mint (the per-attempt 401 re-invoke). It is safe for concurrent use by a
// REST client and an SSE stream sharing one source.
type CredentialSource struct {
	command     string
	serverURL   string
	city        string
	interactive bool

	now    func() time.Time
	runner runFunc

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewCredentialSource builds a source for command, scoped to serverURL/city.
// serverURL should already be resolved and validated (https for non-loopback)
// by the caller.
func NewCredentialSource(command, serverURL, city string, interactive bool) (*CredentialSource, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, errors.New("clientauth: credential command is empty")
	}
	if strings.TrimSpace(serverURL) == "" {
		return nil, errors.New("clientauth: server URL is required")
	}
	return &CredentialSource{
		command:     command,
		serverURL:   serverURL,
		city:        city,
		interactive: interactive,
		now:         time.Now,
		runner:      runCredentialCommand,
	}, nil
}

// Token returns a cached token when it is live (before expiry minus skew),
// otherwise mints a fresh one. Call it before every request and every SSE
// (re)connect.
func (s *CredentialSource) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.now().Add(expirySkew).Before(s.expiresAt) {
		return s.token, nil
	}
	return s.mintLocked()
}

// Refresh mints a fresh token regardless of cache state. Use it to re-invoke the
// credential command after a 401 (the server rejected the presented token).
func (s *CredentialSource) Refresh() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mintLocked()
}

func (s *CredentialSource) mintLocked() (string, error) {
	info := ExecInfo{
		Version: Version,
		Spec:    ExecSpec{ServerURL: s.serverURL, City: s.city, Interactive: s.interactive},
	}
	res, err := s.runner(context.Background(), s.command, info)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(res.Token) == "" {
		return "", errors.New("clientauth: credential command returned an empty token")
	}
	if strings.TrimSpace(res.ExpirationTimestamp) == "" {
		return "", errors.New("clientauth: credential command result is missing the required expiration_timestamp")
	}
	exp, err := time.Parse(time.RFC3339, strings.TrimSpace(res.ExpirationTimestamp))
	if err != nil {
		return "", fmt.Errorf("clientauth: invalid expiration_timestamp %q: %w", res.ExpirationTimestamp, err)
	}
	s.token = res.Token
	s.expiresAt = exp
	return s.token, nil
}

// runCredentialCommand runs command via "sh -c" with the request JSON in
// GC_EXEC_INFO (env only — never argv, so the request is not visible in `ps`).
// Inherited GC_*_INFO variables are stripped so a nested exec cannot read a
// stale request. cmd.Output captures stderr into the returned error on failure.
func runCredentialCommand(ctx context.Context, command string, info ExecInfo) (ExecResult, error) {
	payload, err := json.Marshal(info)
	if err != nil {
		return ExecResult{}, fmt.Errorf("clientauth: encoding exec info: %w", err)
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = append(strippedEnv(), ExecInfoEnv+"="+string(payload))
	out, err := cmd.Output()
	if err != nil {
		return ExecResult{}, fmt.Errorf("clientauth: running credential command: %w", withStderr(err))
	}
	var res ExecResult
	if err := json.Unmarshal(out, &res); err != nil {
		return ExecResult{}, fmt.Errorf("clientauth: parsing credential command output: %w", err)
	}
	return res, nil
}

// strippedEnv returns the current environment minus any GC_*_INFO variable, so a
// credential command (and anything it spawns) never inherits a stale exec/grant
// request that would let it impersonate a different call.
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
// failing credential command produces an actionable diagnostic.
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
