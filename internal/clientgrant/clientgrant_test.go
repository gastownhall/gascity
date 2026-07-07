package clientgrant

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
)

// fakeToken builds a shape-valid token: base64url(payload) "." base64url(sig)
// with a 64-byte signature. The bytes are meaningless — clientgrant cannot (and
// must not) verify the signature, only its shape.
func fakeToken(payload string) string {
	sig := make([]byte, ed25519.SignatureSize)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

type stubRunner struct {
	calls    int
	tokens   []string
	err      error
	lastInfo GrantInfo
}

func (r *stubRunner) run(_ context.Context, _ string, info GrantInfo) (string, error) {
	r.calls++
	r.lastInfo = info
	if r.err != nil {
		return "", r.err
	}
	tok := r.tokens[0]
	if len(r.tokens) > 1 {
		r.tokens = r.tokens[1:]
	}
	return tok, nil
}

func newTestSource(t *testing.T, r *stubRunner) *GrantSource {
	t.Helper()
	s, err := NewGrantSource("gc-write-mint --key k.ed25519")
	if err != nil {
		t.Fatal(err)
	}
	s.runner = r.run
	return s
}

func sampleInfo() GrantInfo {
	return GrantInfo{
		Aud:        "gc-city-write",
		City:       "mc",
		Method:     "POST",
		Path:       "/v0/city/mc/sling",
		BodySHA256: "abc123",
		ReqDigest:  "deadbeef",
	}
}

func TestGrantSource_MintReturnsToken(t *testing.T) {
	r := &stubRunner{tokens: []string{fakeToken(`{"kid":"k1"}`)}}
	s := newTestSource(t, r)

	tok, err := s.Mint(sampleInfo())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.Contains(tok, ".") {
		t.Fatalf("token missing separator: %q", tok)
	}
	// The exec info carries the versioned contract stamped by Mint, plus the
	// request binding the caller supplied.
	if r.lastInfo.Version != Version {
		t.Errorf("Mint must stamp Version, got %q", r.lastInfo.Version)
	}
	if r.lastInfo.Aud != "gc-city-write" || r.lastInfo.City != "mc" || r.lastInfo.ReqDigest != "deadbeef" {
		t.Errorf("exec info lost fields: %+v", r.lastInfo)
	}
}

func TestGrantSource_NoCacheMintsFreshEveryTime(t *testing.T) {
	// A grant is single-use + request-bound: a retry MUST mint a fresh grant, so
	// two Mints exec the helper twice even with identical inputs.
	r := &stubRunner{tokens: []string{fakeToken(`{"jti":"a"}`), fakeToken(`{"jti":"b"}`)}}
	s := newTestSource(t, r)

	if _, err := s.Mint(sampleInfo()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mint(sampleInfo()); err != nil {
		t.Fatal(err)
	}
	if r.calls != 2 {
		t.Fatalf("expected 2 execs (no cache), got %d", r.calls)
	}
}

func TestGrantSource_RejectsMalformedToken(t *testing.T) {
	cases := map[string]string{
		"no-separator":   "not-a-token",
		"empty-payload":  "." + base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
		"empty-sig":      base64.RawURLEncoding.EncodeToString([]byte("p")) + ".",
		"short-sig":      base64.RawURLEncoding.EncodeToString([]byte("p")) + "." + base64.RawURLEncoding.EncodeToString([]byte("tooshort")),
		"non-b64-sig":    base64.RawURLEncoding.EncodeToString([]byte("p")) + ".!!!not-base64!!!",
		"empty-token":    "",
		"whitespace-tok": "   ",
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			r := &stubRunner{tokens: []string{tok}}
			s := newTestSource(t, r)
			if _, err := s.Mint(sampleInfo()); err == nil {
				t.Fatalf("malformed token %q must error", tok)
			}
		})
	}
}

func TestGrantSource_TrimsTrailingNewline(t *testing.T) {
	// The reference minter prints the token followed by a newline; Mint must
	// tolerate surrounding whitespace rather than reject a valid token.
	r := &stubRunner{tokens: []string{"\n" + fakeToken(`{"kid":"k1"}`) + "\n"}}
	s := newTestSource(t, r)
	if _, err := s.Mint(sampleInfo()); err != nil {
		t.Fatalf("Mint must trim surrounding whitespace: %v", err)
	}
}

func TestGrantSource_PropagatesExecError(t *testing.T) {
	r := &stubRunner{err: context.DeadlineExceeded}
	s := newTestSource(t, r)
	if _, err := s.Mint(sampleInfo()); err == nil {
		t.Fatal("exec error must propagate")
	}
}

func TestNewGrantSource_Validation(t *testing.T) {
	if _, err := NewGrantSource(""); err == nil {
		t.Error("empty command must error")
	}
	if _, err := NewGrantSource("   "); err == nil {
		t.Error("whitespace command must error")
	}
}

// Real exec path: the helper receives GrantInfo via GC_GRANT_INFO (env only,
// never argv) and its stdout is the bare token. Inherited GC_*_INFO is stripped.
func TestRunGrantCommand_RealExec(t *testing.T) {
	t.Setenv("GC_EXEC_INFO", "stale-should-be-stripped")
	valid := fakeToken(`{"city":"mc"}`)
	// The command proves env delivery (echoes the token only when GC_GRANT_INFO
	// carries the city) and asserts the sibling GC_EXEC_INFO was stripped.
	script := `test -z "$GC_EXEC_INFO" && echo "$GC_GRANT_INFO" | grep -q '"city":"mc"' && printf '%s\n' '` + valid + `'`
	// runGrantCommand returns raw stdout (Mint owns the trim), so compare trimmed.
	tok, err := runGrantCommand(context.Background(), script, GrantInfo{Version: Version, City: "mc"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if strings.TrimSpace(tok) != valid {
		t.Errorf("token = %q, want %q (GC_GRANT_INFO not delivered or GC_EXEC_INFO not stripped)", tok, valid)
	}
}

func TestRunGrantCommand_NonZeroExit(t *testing.T) {
	_, err := runGrantCommand(context.Background(), "echo boom >&2; exit 3", GrantInfo{Version: Version})
	if err == nil {
		t.Fatal("non-zero exit must error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error must carry stderr: %v", err)
	}
}
