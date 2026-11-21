package bazelhermetic

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// nonHermeticTest is a deliberately non-hermetic test: every function reads
// state a Bazel cache key does not cover.
const nonHermeticTest = `package leaky

import (
	"net"
	nethttp "net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestFetchesRelease(t *testing.T) {
	resp, err := nethttp.Get("https://api.github.com/repos/gastownhall/gascity/releases/latest")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestResolves(t *testing.T) {
	if _, err := net.LookupHost("proxy.golang.org"); err != nil {
		t.Skip("offline")
	}
}

func TestDialsRegistry(t *testing.T) {
	conn, _ := net.Dial("tcp", "registry.npmjs.org:443")
	_ = conn
}

func TestShellsOut(t *testing.T) {
	_ = exec.Command("curl", "-fsSL", "https://example.com/x").Run()
	_ = exec.Command("go", "mod", "download").Run()
	_ = exec.Command("git", "clone", "https://github.com/gastownhall/gascity-packs").Run()
}

func TestExpiry(t *testing.T) {
	expires := time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)
	if expires.Before(time.Now()) {
		t.Fatal("waiver expired")
	}
	if time.Since(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) > 365*24*time.Hour {
		t.Fatal("too old")
	}
}

func TestSharedTmp(t *testing.T) {
	_ = os.WriteFile("/tmp/leaky-state", nil, 0o600)
	_ = os.MkdirAll(filepath.Join("/tmp", "leaky"), 0o700)
	_, _ = net.Listen("unix", "/tmp/leaky.sock")
}
`

// hermeticTest uses every API above in ways that read only declared state.
const hermeticTest = `package tidy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"
)

var fixtureURL = "https://github.com/gastownhall/gascity-packs" // data only, never dialed

func TestLoopback(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, _ = http.Get(srv.URL)
	_, _ = http.Get("http://127.0.0.1:1/health")
	_, _ = net.Dial("tcp", "localhost:0")
	_, _ = http.NewRequest("GET", "https://api.example.com/v1", nil)
	_ = exec.Command("git", "clone", "file:///tmp/repo").Run()
	_ = exec.Command("ssh-keygen", "-t", "ed25519").Run()
}

func TestFixedClock(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	deadline := time.Now().Add(time.Second)
	if now.Add(time.Hour).Before(now) || time.Now().After(deadline) {
		t.Fatal("clock")
	}
}

func TestUniqueTmp(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "tidy-")
	defer os.RemoveAll(dir)
	_ = os.WriteFile(t.TempDir()+"/x", nil, 0o600)
}
`

const datedLedgerSource = `package dated

import "github.com/gastownhall/gascity/internal/testpolicy/waiverclock"

var _ = waiverclock.Check
`

func fixtureFS() fstest.MapFS {
	return fstest.MapFS{
		"leaky/leaky_test.go":            {Data: []byte(nonHermeticTest)},
		"tidy/tidy_test.go":              {Data: []byte(hermeticTest)},
		"dated/dated.go":                 {Data: []byte(datedLedgerSource)},
		"dated/dated_test.go":            {Data: []byte(datedLedgerSource)},
		"leaky/testdata/ignored_test.go": {Data: []byte(nonHermeticTest)},
		"bazel-out/x/ignored_test.go":    {Data: []byte(nonHermeticTest)},
	}
}

func TestScanFlagsDeliberatelyNonHermeticTest(t *testing.T) {
	findings, err := Scan(fixtureFS())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range findings {
		got = append(got, f.String())
	}
	want := []string{
		`dated/dated_test.go:3: wall-clock: imports waiverclock: dated waivers expire with the calendar`,
		`leaky/leaky_test.go:14: external-url: http.Get("https://api.github.com/repos/gastownhall/gascity/releases/latest") reaches api.github.com`,
		`leaky/leaky_test.go:22: dns: net.LookupHost`,
		`leaky/leaky_test.go:28: external-url: net.Dial("registry.npmjs.org:443") reaches registry.npmjs.org`,
		`leaky/leaky_test.go:33: network-cli: exec.Command("curl")`,
		`leaky/leaky_test.go:34: network-cli: exec.Command("go", "mod download") downloads modules`,
		`leaky/leaky_test.go:35: external-url: exec.Command("https://github.com/gastownhall/gascity-packs") reaches github.com`,
		`leaky/leaky_test.go:40: wall-clock: time.Date(2026, ...).Before(now) depends on the date the test runs`,
		`leaky/leaky_test.go:43: wall-clock: time.Since(time.Date(2026, ...)) depends on the date the test runs`,
		`leaky/leaky_test.go:49: shared-tmp: os.WriteFile("/tmp/leaky-state") writes a host path shared between actions`,
		`leaky/leaky_test.go:50: shared-tmp: os.MkdirAll("/tmp") writes a host path shared between actions`,
		`leaky/leaky_test.go:51: shared-tmp: net.Listen("/tmp/leaky.sock") writes a host path shared between actions`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("findings mismatch\n got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestCheckFailsUntilLeakyPackageIsLedgered(t *testing.T) {
	sourceFS := fixtureFS()
	findings, err := Scan(sourceFS)
	if err != nil {
		t.Fatal(err)
	}
	packages, err := TestPackages(sourceFS)
	if err != nil {
		t.Fatal(err)
	}

	problems := Check(findings, Ledger{}, packages)
	if len(problems) != len(findings) {
		t.Fatalf("empty ledger: %d problems for %d findings:\n%s", len(problems), len(findings), strings.Join(problems, "\n"))
	}
	for _, p := range problems {
		if !strings.Contains(p, "undeclared test input") {
			t.Errorf("problem lacks remediation text: %s", p)
		}
	}

	ledger := Ledger{
		Targets:  []LedgerTarget{{Package: "leaky", Tags: []string{"external", "requires-network"}, Reason: "fixture"}},
		Reviewed: []LedgerReview{{Package: "dated", Signal: SignalWallClock, Reason: "fixture"}},
	}
	if err := ledger.Validate(); err != nil {
		t.Fatal(err)
	}
	if problems := Check(findings, ledger, packages); len(problems) != 0 {
		t.Fatalf("ledgered fixture still fails:\n%s", strings.Join(problems, "\n"))
	}
}

func TestCheckRejectsStaleAndPhantomEntries(t *testing.T) {
	ledger := Ledger{
		Targets:  []LedgerTarget{{Package: "gone", Tags: []string{"external"}, Reason: "fixture"}},
		Reviewed: []LedgerReview{{Package: "tidy", Signal: SignalDNS, Reason: "fixture"}},
	}
	problems := Check(nil, ledger, map[string]bool{"tidy": true})
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"reviewed tidy dns matches no finding", "target gone has no Go test package"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestLedgerValidateRequiresCacheExemptTagsAndReasons(t *testing.T) {
	cases := map[string]Ledger{
		"cacheable target":  {Targets: []LedgerTarget{{Package: "a", Tags: []string{"requires-network"}, Reason: "r"}}},
		"unknown tag":       {Targets: []LedgerTarget{{Package: "a", Tags: []string{"external", "flaky"}, Reason: "r"}}},
		"missing reason":    {Targets: []LedgerTarget{{Package: "a", Tags: []string{"external"}}}},
		"duplicate target":  {Targets: []LedgerTarget{{Package: "a", Tags: []string{"external"}, Reason: "r"}, {Package: "a", Tags: []string{"external"}, Reason: "r"}}},
		"unknown signal":    {Reviewed: []LedgerReview{{Package: "a", Signal: "vibes", Reason: "r"}}},
		"redundant review":  {Targets: []LedgerTarget{{Package: "a", Tags: []string{"external"}, Reason: "r"}}, Reviewed: []LedgerReview{{Package: "a", Signal: SignalDNS, Reason: "r"}}},
		"review w/o reason": {Reviewed: []LedgerReview{{Package: "a", Signal: SignalDNS}}},
	}
	for name, ledger := range cases {
		if err := ledger.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, ledger)
		}
	}
}

// TestRepositoryTestsAreHermeticOrLedgered is the enforcement: every signal
// in the repository's tests is either fixed, cache-exempt via the ledger's
// tags, or reviewed.
func TestRepositoryTestsAreHermeticOrLedgered(t *testing.T) {
	sourceFS := os.DirFS(bazeltest.RepoRoot(t))
	ledger, err := LoadLedger(sourceFS)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := Scan(sourceFS)
	if err != nil {
		t.Fatal(err)
	}
	packages, err := TestPackages(sourceFS)
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) < 100 {
		t.Fatalf("scanned only %d test packages; under bazel the test needs //:repo_go_test_srcs in data", len(packages))
	}
	if problems := Check(findings, ledger, packages); len(problems) > 0 {
		t.Fatalf("%d Bazel hermeticity problem(s):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
}
