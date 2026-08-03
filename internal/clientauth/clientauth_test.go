package clientauth

import (
	"context"
	"strings"
	"testing"
	"time"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// stubRunner records invocations and returns scripted results.
type stubRunner struct {
	calls   int
	results []ExecResult
	err     error
	lastEnv ExecInfo
}

func (r *stubRunner) run(_ context.Context, _ string, info ExecInfo) (ExecResult, error) {
	r.calls++
	r.lastEnv = info
	if r.err != nil {
		return ExecResult{}, r.err
	}
	res := r.results[0]
	if len(r.results) > 1 {
		r.results = r.results[1:]
	}
	return res, nil
}

func newTestSource(t *testing.T, r *stubRunner, clock func() time.Time) *CredentialSource {
	t.Helper()
	s, err := NewCredentialSource("cred-helper --aud gc", "https://box:9443", "mc", false)
	if err != nil {
		t.Fatal(err)
	}
	s.runner = r.run
	s.now = clock
	return s
}

func TestCredentialSource_CachesUntilExpiry(t *testing.T) {
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	exp := base.Add(10 * time.Minute).Format(time.RFC3339)
	r := &stubRunner{results: []ExecResult{{Token: "tok1", ExpirationTimestamp: exp}}}
	s := newTestSource(t, r, fixedClock(base))

	tok, err := s.Token()
	if err != nil || tok != "tok1" {
		t.Fatalf("first Token = %q, %v", tok, err)
	}
	if _, err := s.Token(); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 {
		t.Errorf("expected 1 exec (cached), got %d", r.calls)
	}
	// The exec info carries the versioned contract + spec.
	if r.lastEnv.Version != Version || r.lastEnv.Spec.ServerURL != "https://box:9443" || r.lastEnv.Spec.City != "mc" {
		t.Errorf("exec info = %+v", r.lastEnv)
	}
}

func TestCredentialSource_ReMintsAfterExpiry(t *testing.T) {
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	exp1 := base.Add(1 * time.Minute).Format(time.RFC3339)
	exp2 := base.Add(30 * time.Minute).Format(time.RFC3339)
	r := &stubRunner{results: []ExecResult{
		{Token: "tok1", ExpirationTimestamp: exp1},
		{Token: "tok2", ExpirationTimestamp: exp2},
	}}
	now := base
	s := newTestSource(t, r, func() time.Time { return now })

	if tok, _ := s.Token(); tok != "tok1" {
		t.Fatalf("tok1 expected, got %q", tok)
	}
	// Advance past expiry (accounting for skew).
	now = base.Add(2 * time.Minute)
	if tok, _ := s.Token(); tok != "tok2" {
		t.Fatalf("tok2 expected after expiry, got %q", tok)
	}
	if r.calls != 2 {
		t.Errorf("expected 2 execs, got %d", r.calls)
	}
}

func TestCredentialSource_RefreshForcesReMint(t *testing.T) {
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	exp := base.Add(1 * time.Hour).Format(time.RFC3339)
	r := &stubRunner{results: []ExecResult{
		{Token: "tok1", ExpirationTimestamp: exp},
		{Token: "tok2", ExpirationTimestamp: exp},
	}}
	s := newTestSource(t, r, fixedClock(base))

	_, _ = s.Token()
	tok, err := s.Refresh() // 401 re-invoke: fresh mint even though cache is live
	if err != nil || tok != "tok2" {
		t.Fatalf("Refresh = %q, %v", tok, err)
	}
	if r.calls != 2 {
		t.Errorf("Refresh must re-exec: calls=%d", r.calls)
	}
}

func TestCredentialSource_RequiresExpiration(t *testing.T) {
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	r := &stubRunner{results: []ExecResult{{Token: "tok1", ExpirationTimestamp: ""}}}
	s := newTestSource(t, r, fixedClock(base))
	if _, err := s.Token(); err == nil || !strings.Contains(err.Error(), "expiration_timestamp") {
		t.Fatalf("missing expiration must error, got %v", err)
	}
}

func TestCredentialSource_RejectsEmptyToken(t *testing.T) {
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	exp := base.Add(time.Hour).Format(time.RFC3339)
	r := &stubRunner{results: []ExecResult{{Token: "  ", ExpirationTimestamp: exp}}}
	s := newTestSource(t, r, fixedClock(base))
	if _, err := s.Token(); err == nil || !strings.Contains(err.Error(), "empty token") {
		t.Fatalf("empty token must error, got %v", err)
	}
}

func TestCredentialSource_InvalidExpirationFormat(t *testing.T) {
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	r := &stubRunner{results: []ExecResult{{Token: "tok", ExpirationTimestamp: "not-a-timestamp"}}}
	s := newTestSource(t, r, fixedClock(base))
	if _, err := s.Token(); err == nil || !strings.Contains(err.Error(), "expiration_timestamp") {
		t.Fatalf("invalid expiration must error, got %v", err)
	}
}

func TestNewCredentialSource_Validation(t *testing.T) {
	if _, err := NewCredentialSource("", "https://box", "mc", false); err == nil {
		t.Error("empty command must error")
	}
	if _, err := NewCredentialSource("cmd", "", "mc", false); err == nil {
		t.Error("empty server URL must error")
	}
}

// Real exec path: the helper receives the request via GC_EXEC_INFO (env only,
// never argv) and its stdout JSON is parsed. Inherited GC_*_INFO is stripped.
func TestRunCredentialCommand_RealExec(t *testing.T) {
	t.Setenv("GC_GRANT_INFO", "stale-should-be-stripped")
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	// The command echoes a token built from GC_EXEC_INFO to prove env delivery,
	// and asserts GC_GRANT_INFO was stripped.
	script := `test -z "$GC_GRANT_INFO" && printf '{"token":"t-%s","expiration_timestamp":"` + exp + `"}' "$(echo "$GC_EXEC_INFO" | grep -o mc)"`
	res, err := runCredentialCommand(context.Background(), script, ExecInfo{Version: Version, Spec: ExecSpec{ServerURL: "https://box:9443", City: "mc"}})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.Token != "t-mc" {
		t.Errorf("token = %q, want t-mc (GC_EXEC_INFO not delivered or GC_GRANT_INFO not stripped)", res.Token)
	}
	if res.ExpirationTimestamp != exp {
		t.Errorf("expiration = %q", res.ExpirationTimestamp)
	}
}

func TestRunCredentialCommand_NonZeroExit(t *testing.T) {
	_, err := runCredentialCommand(context.Background(), "echo oops >&2; exit 7", ExecInfo{Version: Version})
	if err == nil {
		t.Fatal("non-zero exit must error")
	}
}

// TestCredentialSource_BoundsHelperContext proves a non-interactive credential
// command runs under a bounded, cancellable context so a hung helper cannot block
// the caller (and every remote request behind the mint lock) forever. The stub
// runner records whether it got a deadline and blocks until cancellation; with the
// helper timeout shrunk, Token must return the canceled-helper error promptly
// rather than hang. Regression for the context.Background() unbounded-exec finding.
func TestCredentialSource_BoundsHelperContext(t *testing.T) {
	s, err := NewCredentialSource("cred-helper", "https://box:9443", "mc", false)
	if err != nil {
		t.Fatal(err)
	}
	if s.helperTimeout <= 0 {
		t.Fatalf("non-interactive source must bound the helper exec, got timeout %v", s.helperTimeout)
	}
	s.helperTimeout = 20 * time.Millisecond

	var sawDeadline bool
	s.runner = func(ctx context.Context, _ string, _ ExecInfo) (ExecResult, error) {
		_, sawDeadline = ctx.Deadline()
		<-ctx.Done() // simulate a hung credential command
		return ExecResult{}, ctx.Err()
	}

	done := make(chan struct{})
	var tokErr error
	go func() {
		_, tokErr = s.Token()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Token did not return: the credential helper exec is unbounded")
	}
	if !sawDeadline {
		t.Error("credential helper ran without a context deadline")
	}
	if tokErr == nil {
		t.Error("Token = nil error, want the canceled-helper error")
	}
}

// TestCredentialSource_InteractiveBoundedGenerously documents that an interactive
// credential command is still bounded (nothing runs under an unbounded context),
// but with a far more generous deadline than the machine path so a human
// completing a login is not killed mid-flow.
func TestCredentialSource_InteractiveBoundedGenerously(t *testing.T) {
	s, err := NewCredentialSource("cred-helper", "https://box:9443", "mc", true /*interactive*/)
	if err != nil {
		t.Fatal(err)
	}
	if s.helperTimeout != interactiveCredentialHelperTimeout {
		t.Fatalf("interactive helper timeout = %v, want %v", s.helperTimeout, interactiveCredentialHelperTimeout)
	}
	if interactiveCredentialHelperTimeout <= credentialHelperTimeout {
		t.Fatalf("interactive timeout %v must be more generous than the machine timeout %v", interactiveCredentialHelperTimeout, credentialHelperTimeout)
	}
}
