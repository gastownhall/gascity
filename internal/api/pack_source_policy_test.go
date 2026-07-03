package api

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/importsvc"
)

func TestValidateHTTPPackSource_AllowsPublicRemotes(t *testing.T) {
	// A public host must resolve to a public address; stub DNS so the unit test
	// never touches the network.
	restore := stubPackSourceResolver(t, map[string][]net.IP{
		"github.com":     {net.ParseIP("140.82.112.3")},
		"gitlab.example": {net.ParseIP("203.0.113.10")},
	})
	defer restore()

	for _, src := range []string{
		"https://github.com/org/repo/tree/main/packs/review",
		"http://github.com/org/repo",
		"ssh://git@gitlab.example/org/repo.git",
		"git@github.com:org/repo.git",
		"github.com/org/repo",
	} {
		if err := validateHTTPPackSource(src); err != nil {
			t.Errorf("validateHTTPPackSource(%q) = %v, want nil", src, err)
		}
	}
}

func TestValidateHTTPPackSource_RejectsLocalAndFileSources(t *testing.T) {
	for _, src := range []string{
		"file:///etc/passwd",
		"//packs/local",
		"~/secret",
		"/abs/path",
		"relative/path",
		"../packs/local",
	} {
		err := validateHTTPPackSource(src)
		if err == nil {
			t.Errorf("validateHTTPPackSource(%q) = nil, want rejection", src)
			continue
		}
		if !errors.Is(err, importsvc.ErrInvalidSource) {
			t.Errorf("validateHTTPPackSource(%q) error = %v, want ErrInvalidSource", src, err)
		}
	}
}

func TestValidateHTTPPackSource_RejectsInternalIPLiterals(t *testing.T) {
	for _, src := range []string{
		"https://127.0.0.1/repo.git",         // loopback
		"http://10.0.0.5/repo.git",           // RFC1918
		"http://192.168.1.9/repo.git",        // RFC1918
		"http://172.16.4.4/repo.git",         // RFC1918
		"http://169.254.169.254/latest/meta", // link-local / cloud metadata
		"ssh://git@[::1]/repo.git",           // IPv6 loopback
		"http://[fd00::1]/repo.git",          // IPv6 unique-local
		"git@10.0.0.5:repo.git",              // scp-like, private
		"http://0.0.0.0/repo.git",            // unspecified
	} {
		err := validateHTTPPackSource(src)
		if err == nil {
			t.Errorf("validateHTTPPackSource(%q) = nil, want rejection", src)
			continue
		}
		if !errors.Is(err, importsvc.ErrInvalidSource) {
			t.Errorf("validateHTTPPackSource(%q) error = %v, want ErrInvalidSource", src, err)
		}
	}
}

func TestValidateHTTPPackSource_RejectsEncodedInternalIPLiterals(t *testing.T) {
	// Encoded-integer IP literals that net.ParseIP does not recognize but git's C
	// resolver (getaddrinfo/inet_aton) decodes to an internal address must be
	// blocked. The fence decodes them the same way (hex, octal, dotless integer)
	// and classifies the decoded destination, so these cannot slip past on a
	// resolver that merely errors for them.
	for _, src := range []string{
		"http://0x7f000001/repo.git",         // hex -> 127.0.0.1 (loopback)
		"http://2130706433/repo.git",         // dotless decimal -> 127.0.0.1
		"http://0177.0.0.1/repo.git",         // octal octet -> 127.0.0.1
		"http://0xA9FEA9FE/latest/meta-data", // hex -> 169.254.169.254 (metadata)
		"http://0xa9fea9fe/repo.git",         // lowercase hex -> 169.254.169.254
		"http://3232235521/repo.git",         // dotless decimal -> 192.168.0.1
	} {
		err := validateHTTPPackSource(src)
		if err == nil {
			t.Errorf("validateHTTPPackSource(%q) = nil, want rejection", src)
			continue
		}
		if !errors.Is(err, importsvc.ErrInvalidSource) {
			t.Errorf("validateHTTPPackSource(%q) error = %v, want ErrInvalidSource", src, err)
		}
	}
}

func TestValidateHTTPPackSource_AllowsEncodedPublicIPLiterals(t *testing.T) {
	// Decoding must not over-block: an encoded literal that decodes to a public
	// address is allowed, matching the plain dotted-decimal behavior. No DNS is
	// consulted, so no resolver stub is needed.
	for _, src := range []string{
		"http://0x08080808/repo.git", // hex -> 8.8.8.8 (public)
		"http://134744072/repo.git",  // dotless decimal -> 8.8.8.8 (public)
	} {
		if err := validateHTTPPackSource(src); err != nil {
			t.Errorf("validateHTTPPackSource(%q) = %v, want nil", src, err)
		}
	}
}

func TestValidateHTTPPackSource_RejectsBracketedIPv6ScpForm(t *testing.T) {
	// scp-like SSH syntax with a bracketed IPv6 host (git@[::1]:repo) must extract
	// the address between the brackets, not stop at the first ':' inside the
	// literal and yield the bogus host "[" that slips past the fence.
	for _, src := range []string{
		"git@[::1]:repo.git",     // IPv6 loopback
		"git@[fd00::1]:repo.git", // IPv6 unique-local
	} {
		if err := validateHTTPPackSource(src); !errors.Is(err, importsvc.ErrInvalidSource) {
			t.Errorf("validateHTTPPackSource(%q) error = %v, want ErrInvalidSource", src, err)
		}
	}
}

func TestValidateHTTPPackSource_RejectsLoopbackHostname(t *testing.T) {
	for _, src := range []string{
		"https://localhost/repo.git",
		"http://LOCALHOST:8080/repo.git",
		"https://api.localhost/repo.git",
	} {
		if err := validateHTTPPackSource(src); !errors.Is(err, importsvc.ErrInvalidSource) {
			t.Errorf("validateHTTPPackSource(%q) error = %v, want ErrInvalidSource", src, err)
		}
	}
}

func TestValidateHTTPPackSource_RejectsHostResolvingToInternal(t *testing.T) {
	restore := stubPackSourceResolver(t, map[string][]net.IP{
		// A public-looking name that resolves to an internal address (DNS-based
		// SSRF) must be blocked.
		"evil.example": {net.ParseIP("10.1.2.3")},
	})
	defer restore()

	if err := validateHTTPPackSource("https://evil.example/repo.git"); !errors.Is(err, importsvc.ErrInvalidSource) {
		t.Errorf("validateHTTPPackSource(evil.example) error = %v, want ErrInvalidSource", err)
	}
}

func TestValidateHTTPPackSource_ResolutionErrorDoesNotBlock(t *testing.T) {
	// A transient DNS failure must not block: the git fetch performs its own
	// resolution and surfaces the failure there. The fence only blocks on a
	// positively-internal address.
	restore := stubPackSourceResolver(t, nil)
	defer restore()

	if err := validateHTTPPackSource("https://unresolvable.example/repo.git"); err != nil {
		t.Errorf("validateHTTPPackSource on resolution error = %v, want nil", err)
	}
}

// The add handler must fence the source BEFORE the importsvc seam runs, so a
// blocked source can never drive a git fetch.
func TestHandlePackAdd_BlocksSSRFSourceBeforeSeam(t *testing.T) {
	restore := stubPackSourceResolver(t, map[string][]net.IP{
		"internal.example": {net.ParseIP("169.254.169.254")},
	})
	defer restore()

	orig := packAddImport
	var seamCalled bool
	packAddImport = func(fsys.FS, string, string, string, string) (*importsvc.AddResult, error) {
		seamCalled = true
		return &importsvc.AddResult{Name: "review"}, nil
	}
	defer func() { packAddImport = orig }()

	for _, body := range []string{
		`{"source":"http://169.254.169.254/latest/meta-data"}`,
		`{"source":"file:///etc/passwd"}`,
		`{"source":"//packs/local"}`,
		`{"source":"https://internal.example/repo.git"}`,
	} {
		fs := newFakeMutatorState(t)
		h := newTestCityHandler(t, fs)
		req := httptest.NewRequest("POST", cityURL(fs, "/packs"), strings.NewReader(body))
		req.Header.Set("X-GC-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want %d; body=%s", body, w.Code, http.StatusBadRequest, w.Body.String())
		}
	}
	if seamCalled {
		t.Fatal("packAddImport seam was reached for a blocked source; the SSRF fence must run first")
	}
}

// TestPackAddImportFenced_ThreadsSSRFPolicyIntoImportsvc proves the default add
// seam threads validateHTTPPackSource into importsvc as the SourcePolicy, so the
// fence reaches the direct git probe and (via SyncLockWithPolicy) every
// transitive import, not just the handler's top-level pre-check. An internal
// direct source is rejected before any git seam, with no network.
func TestPackAddImportFenced_ThreadsSSRFPolicyIntoImportsvc(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	_, err := packAddImportFenced(fsys.OSFS{}, dir, "http://10.0.0.5/repo.git", "", "")
	if !errors.Is(err, importsvc.ErrInvalidSource) {
		t.Fatalf("default add seam did not thread the SSRF fence into importsvc: err = %v", err)
	}
}

// stubPackSourceResolver swaps the DNS seam for the test and returns a restore
// func. Hosts absent from table resolve with an error (no address).
func stubPackSourceResolver(t *testing.T, table map[string][]net.IP) func() {
	t.Helper()
	orig := packSourceHostResolver
	packSourceHostResolver = func(host string) ([]net.IP, error) {
		if ips, ok := table[strings.ToLower(host)]; ok {
			return ips, nil
		}
		return nil, errors.New("no such host")
	}
	return func() { packSourceHostResolver = orig }
}
