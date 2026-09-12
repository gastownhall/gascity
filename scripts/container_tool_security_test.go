package scripts_test

import (
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestContainerCLIToolsRebuildWithPatchedGRPC(t *testing.T) {
	const (
		ghVersion                 = "2.96.0"
		ghSourceRef               = "b300f2ec7ec9dc9addc39b2ad88c54097ded7ca0"
		doltSourceRef             = "781cbb730221ea7df4fc7995255bb336df9c3864"
		grpcVersion               = "1.82.1"
		ghSourceSHA256            = "a0c18c98c73f7333f73e19b3a0bf5bd18673f3dc226193ab6478b3ea1ea18f03"
		doltSourceSHA256          = "0b0c9bce8baef26baa7e0e5825cd2d7d6101daf6fc9673f38dac9670afb66847"
		doltToolchainRelease      = "20260611_0.0.5_trixie"
		doltOptcrossX8664SHA256   = "caf703fb1cbc0c9ff9a5b506f73da6c6f5233c04a455e638cdc50267a4d0c0c0"
		doltOptcrossAarch64SHA256 = "5635d0b38343fefb0c2b600d61c49ad9ceeaa1107bccdec8a60b1789100dc0ce"
		doltICUStaticSHA256       = "8b0234f16da73b9c8d47f86eeef98928879611149e3ee1bb560dddb0ffdd95a1"
	)

	dockerfile := readFile(t, repoRoot(t), "contrib/k8s/Dockerfile.base")
	for _, want := range []string{
		"ARG GH_VERSION=" + ghVersion,
		"ARG GH_SOURCE_REF=" + ghSourceRef,
		"ARG GH_SOURCE_SHA256=" + ghSourceSHA256,
		"ARG DOLT_SOURCE_REF=" + doltSourceRef,
		"ARG DOLT_SOURCE_SHA256=" + doltSourceSHA256,
		"ARG GRPC_VERSION=" + grpcVersion,
		"ARG DOLT_TOOLCHAIN_RELEASE=" + doltToolchainRelease,
		"ARG DOLT_OPTCROSS_X86_64_SHA256=" + doltOptcrossX8664SHA256,
		"ARG DOLT_OPTCROSS_AARCH64_SHA256=" + doltOptcrossAarch64SHA256,
		"ARG DOLT_ICU_STATIC_SHA256=" + doltICUStaticSHA256,
		`grep -Fq "Version = \"${DOLT_VERSION}\"" cmd/dolt/doltversion/version.go`,
		`CGO_LDFLAGS="-static -s"`,
		`-tags="icu_static,timetzdata"`,
		"x86_64-linux-musl-gcc",
		"aarch64-linux-musl-gcc",
		`file /out/dolt | grep -Fq "statically linked"`,
		"COPY --from=tool-builder /out/gh /usr/bin/gh",
		"COPY --from=tool-builder /out/dolt /usr/local/bin/dolt",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("contrib/k8s/Dockerfile.base missing %q", want)
		}
	}
	if got := strings.Count(dockerfile, `go get "google.golang.org/grpc@v${GRPC_VERSION}"`); got != 2 {
		t.Errorf("contrib/k8s/Dockerfile.base applies the grpc override %d times, want exactly 2 (gh and Dolt)", got)
	}

	for _, forbidden := range []string{
		"apt-get install -y --no-install-recommends gh",
		`/tmp/install-dolt-archive.sh "${DOLT_VERSION}"`,
		"libicu74",
		"-tags=timetzdata",
	} {
		if strings.Contains(dockerfile, forbidden) {
			t.Errorf("contrib/k8s/Dockerfile.base still installs vulnerable prebuilt tool via %q", forbidden)
		}
	}
}

func TestAgentImageRebuildsBDAndGCWithPatchedGRPC(t *testing.T) {
	const (
		bdSourceRef    = "c185735c38e25569277eae798ce363ecba9859e8"
		bdSourceSHA256 = "3e256519a683b413f7baa9f4d1071084bb2646478faabad9bf3ac7bd05952f43"
		bdBuild        = "c185735c38"
		bdBranch       = "HEAD"
		grpcVersion    = "1.83.0"
	)

	root := repoRoot(t)
	env := readDotenv(t, root+"/deps.env")
	// The image stamps -X main.Version=${BD_VERSION} onto source fetched at
	// BD_SOURCE_REF, and the Dockerfile's own `grep Version = "${bd_version}"
	// cmd/bd/version.go` fails the build if those two name different releases.
	// Assert it here rather than discovering it in a docker build CI may not run.
	//
	// The anchor is BD_CURRENT_VERSION, not BD_VERSION: BD_SOURCE_REF tracks
	// BD_CURRENT_REF (TestBDVersionPins), so the version that source declares is
	// BD_CURRENT_VERSION. deps.env BD_VERSION is a different role -- the
	// published tarball CI installs -- and it legitimately lags whenever the
	// current cell is pinned to a commit upstream never cut a release for, which
	// is the normal state of a bleeding-edge cell. Tying this to BD_VERSION
	// would forbid that lag and collapse two anchors the matrix keeps distinct.
	bdVersion := env["BD_CURRENT_VERSION"]
	if bdVersion == "" {
		t.Fatal("deps.env missing BD_CURRENT_VERSION")
	}

	dockerfile := readFile(t, root, "contrib/k8s/Dockerfile.agent")
	for _, want := range []string{
		"ARG BD_VERSION=" + bdVersion,
		"ARG BD_SOURCE_REF=" + bdSourceRef,
		"ARG BD_SOURCE_SHA256=" + bdSourceSHA256,
		"ARG BD_BUILD=" + bdBuild,
		"ARG BD_BRANCH=" + bdBranch,
		"ARG GRPC_VERSION=" + grpcVersion,
		`https://github.com/gastownhall/beads/archive/${BD_SOURCE_REF}.tar.gz`,
		`echo "${BD_SOURCE_SHA256}  /tmp/bd-source.tar.gz" | sha256sum --check --strict`,
		`grep -Fq "Version = \"${bd_version}\"" cmd/bd/version.go`,
		`go get "google.golang.org/grpc@v${GRPC_VERSION}"`,
		`CGO_ENABLED=1 go build`,
		`-tags="gms_pure_go"`,
		`-X main.Version=${bd_version}`,
		`-X main.Build=${BD_BUILD}`,
		`-X main.Commit=${BD_SOURCE_REF}`,
		`-X main.Branch=${BD_BRANCH}`,
		`COPY --from=bd-builder /out/bd /usr/local/bin/bd`,
		`CGO_ENABLED=0 go build -o gc ./cmd/gc`,
		`RUN gc version`,
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("contrib/k8s/Dockerfile.agent missing %q", want)
		}
	}
	if got := strings.Count(dockerfile, `go get "google.golang.org/grpc@v${GRPC_VERSION}"`); got != 1 {
		t.Errorf("contrib/k8s/Dockerfile.agent applies the bd grpc override %d times, want exactly 1", got)
	}
	if strings.Contains(dockerfile, "COPY bd /usr/local/bin/bd") {
		t.Error("contrib/k8s/Dockerfile.agent still copies the vulnerable prebuilt bd binary")
	}
	baseImageArg := strings.Index(dockerfile, "ARG BASE_IMAGE=")
	firstStage := strings.Index(dockerfile, "FROM ")
	if baseImageArg < 0 || firstStage < 0 || baseImageArg > firstStage {
		t.Error("contrib/k8s/Dockerfile.agent must declare BASE_IMAGE globally before its first FROM")
	}

	goMod := readFile(t, root, "go.mod")
	wantGRPCModule := "google.golang.org/grpc v" + grpcVersion
	if got := strings.Count(goMod, wantGRPCModule); got != 1 {
		t.Errorf("go.mod contains %q %d times, want exactly 1 so the gc binary embeds the patched grpc", wantGRPCModule, got)
	}

	workflow := readFile(t, root, ".github/workflows/container-scan.yml")
	if !strings.Contains(workflow, "CGO_ENABLED=0 go build -o gc ./cmd/gc") {
		t.Error("container scan must build gc with the release's portable CGO_ENABLED=0 configuration")
	}
}

func TestMCPMailImagePinsPatchedPythonDependencies(t *testing.T) {
	root := repoRoot(t)
	input := readFile(t, root, ".github/requirements/mcp-agent-mail.in")
	for _, want := range []string{
		"gitpython>=3.1.57",
		"aiohttp>=3.14.3",
		"pillow>=12.3.0",
	} {
		if !strings.Contains(input, want) {
			t.Errorf("mcp-agent-mail input requirements missing security floor %q", want)
		}
	}
	overrides := readFile(t, root, ".github/requirements/mcp-agent-mail.overrides.txt")
	if !strings.Contains(overrides, "cryptography>=50.0.0") {
		t.Error("mcp-agent-mail overrides missing cryptography security floor >=50.0.0")
	}

	lock := readFile(t, root, ".github/requirements/mcp-agent-mail.txt")
	for _, want := range []string{
		"gitpython==3.1.58 \\",
		"aiohttp==3.14.3 \\",
		"cryptography==50.0.0 \\",
		"pillow==12.3.0 \\",
	} {
		if !strings.Contains(lock, want) {
			t.Errorf("mcp-agent-mail hashed lock missing patched dependency %q", want)
		}
	}
}

// TestRebuiltToolsAssertPatchedGRPCArtifact guards the artifact-level proof that
// each rebuilt CLI actually embeds the patched grpc module. Text-level ARG/recipe
// checks confirm the build inputs; these `go version -m` assertions are the only
// evidence the produced binary links grpc v${GRPC_VERSION}, so they must not be
// silently removable. bd already had one; gh and dolt now mirror it.
func TestRebuiltToolsAssertPatchedGRPCArtifact(t *testing.T) {
	root := repoRoot(t)

	base := readFile(t, root, "contrib/k8s/Dockerfile.base")
	for _, bin := range []string{"/out/gh", "/out/dolt"} {
		want := `go version -m ` + bin + ` | tr '\t' ' ' | grep -Fq "dep google.golang.org/grpc v${GRPC_VERSION} "`
		if !strings.Contains(base, want) {
			t.Errorf("contrib/k8s/Dockerfile.base must assert %s embeds patched grpc; missing %q", bin, want)
		}
	}

	agent := readFile(t, root, "contrib/k8s/Dockerfile.agent")
	want := `go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep google.golang.org/grpc v${GRPC_VERSION} "`
	if !strings.Contains(agent, want) {
		t.Errorf("contrib/k8s/Dockerfile.agent must assert /out/bd embeds patched grpc; missing %q", want)
	}
}

// TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools enforces that the rebuilt-from-
// source tools (bd, dolt, gh) carry no Go-stdlib CVE waiver. The image build rebuilds
// them with the Go 1.26.5 toolchain, which fixes every stdlib CVE listed, so a waiver
// on those paths would let the scan gate keep masking a regressed rebuild instead of
// proving the fix holds. CVE-2026-56852 (x/text) and CVE-2026-46600 (x/net
// dns/dnsmessage) are the explicit non-stdlib exceptions: the pinned gh and Dolt
// sources, plus (for CVE-2026-56852) external kubectl, still select vulnerable module
// versions. The residual
// x/net / x/crypto module waivers that bd and dolt legitimately keep (external binaries
// the grpc-only rebuild does not touch) are out of scope here; gc's x/net / x/crypto
// module waivers are enforced separately by TestTrivyIgnoreDropsGCModuleWaiversPastThreshold.
func TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools(t *testing.T) {
	root := repoRoot(t)

	var doc struct {
		Vulnerabilities []struct {
			ID    string   `yaml:"id"`
			Paths []string `yaml:"paths"`
		} `yaml:"vulnerabilities"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}

	rebuiltPaths := map[string]bool{
		"usr/local/bin/bd":   true,
		"usr/local/bin/dolt": true,
		"usr/bin/gh":         true,
	}
	stdlibCVEs := map[string]bool{
		"CVE-2026-33811": true, "CVE-2026-33814": true, "CVE-2026-39820": true,
		"CVE-2026-39822": true, "CVE-2026-39823": true, "CVE-2026-39825": true,
		"CVE-2026-39826": true, "CVE-2026-39836": true, "CVE-2026-42499": true,
		"CVE-2026-42504": true, "CVE-2026-27145": true,
		// docket E9 (2026-09-11): kubectl-only stdlib CVEs, fixed in Go 1.26.6.
		"CVE-2026-33818": true, "CVE-2026-56853": true, "CVE-2026-56858": true,
		"CVE-2026-56859": true, "CVE-2026-56860": true, "CVE-2026-56862": true,
	}
	allowedModuleWaivers := map[string]map[string]bool{
		"CVE-2026-56852": {
			"usr/bin/gh":            true,
			"usr/local/bin/dolt":    true,
			"usr/local/bin/kubectl": true,
		},
		"CVE-2026-46600": {
			"usr/bin/gh":            true,
			"usr/local/bin/dolt":    true,
			"usr/local/bin/kubectl": true,
		},
		"CVE-2026-56854": {
			"usr/bin/gh":         true,
			"usr/local/bin/dolt": true,
			"usr/local/bin/bd":   true,
		},
		"CVE-2026-56864": {
			"usr/bin/gh": true,
		},
		"CVE-2026-56865": {
			"usr/bin/gh": true,
		},
		"CVE-2026-84304": {
			"usr/bin/gh":         true,
			"usr/local/bin/dolt": true,
			"usr/local/bin/bd":   true,
			"usr/local/bin/gc":   true,
		},
		"CVE-2026-84445": {
			"usr/bin/gh":         true,
			"usr/local/bin/dolt": true,
			"usr/local/bin/bd":   true,
			"usr/local/bin/gc":   true,
		},
	}
	foundAllowed := map[string]map[string]bool{}

	for _, v := range doc.Vulnerabilities {
		for _, p := range v.Paths {
			if stdlibCVEs[v.ID] && rebuiltPaths[p] {
				t.Errorf("%s still waives rebuilt tool %q for a Go-stdlib CVE the 1.26.5 rebuild clears; drop the path so the scan proves the fix stays effective", v.ID, p)
			}
			if allowedPaths, ok := allowedModuleWaivers[v.ID]; ok && allowedPaths[p] {
				if foundAllowed[v.ID] == nil {
					foundAllowed[v.ID] = map[string]bool{}
				}
				foundAllowed[v.ID][p] = true
				continue
			}
			if p == "usr/bin/gh" {
				t.Errorf("%s waives rebuilt gh without a reviewed module-specific exception", v.ID)
			}
		}
	}
	for cve, paths := range allowedModuleWaivers {
		for path := range paths {
			if !foundAllowed[cve][path] {
				t.Errorf(".trivyignore.yaml must retain the reviewed %s waiver for %s until that module version is fixed upstream", cve, path)
			}
		}
	}
}

// goModVersion returns the [major, minor, patch] version go.mod pins for module,
// reading the require directive directly so the guard tests never drift from the
// tree's actual module graph. Replace directives are ignored.
func goModVersion(t *testing.T, goMod, module string) [3]int {
	t.Helper()
	for _, line := range strings.Split(goMod, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "replace ") || strings.Contains(line, "=>") {
			continue
		}
		line = strings.TrimPrefix(line, "require ")
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == module && strings.HasPrefix(fields[1], "v") {
			return parseModuleSemver(t, fields[1])
		}
	}
	t.Fatalf("go.mod does not pin %s", module)
	return [3]int{}
}

// parseModuleSemver parses a "vMAJOR.MINOR.PATCH" module version into comparable parts.
func parseModuleSemver(t *testing.T, v string) [3]int {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		t.Fatalf("version %q is not vMAJOR.MINOR.PATCH", v)
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			t.Fatalf("parsing %q component %q: %v", v, p, err)
		}
		out[i] = n
	}
	return out
}

// semverAtLeast reports whether have is greater than or equal to want.
func semverAtLeast(have, want [3]int) bool {
	for i := range have {
		if have[i] != want[i] {
			return have[i] > want[i]
		}
	}
	return true
}

// TestTrivyIgnoreDropsGCModuleWaiversPastThreshold enforces that no usr/local/bin/gc
// x/net, x/crypto, or grpc CVE waiver outlives the go.mod bump that fixes it. Unlike the
// rebuilt tools (bd, dolt, gh), gc is built straight from this module, so a waiver on a
// gc path is only honest while go.mod still pins a vulnerable version. Each CVE records
// the module and the first version that fixes it (taken from the waiver's own removal
// text); once go.mod reaches that version the gc path must be dropped, or the container
// scan would stay green without proving the gc binary is clean.
func TestTrivyIgnoreDropsGCModuleWaiversPastThreshold(t *testing.T) {
	root := repoRoot(t)

	type modFix struct {
		module     string
		fixVersion string
	}
	gcModuleCVEs := map[string]modFix{
		// golang.org/x/net http2, fixed in 0.53.0.
		"CVE-2026-33814": {"golang.org/x/net", "v0.53.0"},
		// golang.org/x/net HTML/idna, fixed only in 0.55.0.
		"CVE-2026-25680": {"golang.org/x/net", "v0.55.0"},
		"CVE-2026-25681": {"golang.org/x/net", "v0.55.0"},
		"CVE-2026-27136": {"golang.org/x/net", "v0.55.0"},
		"CVE-2026-39821": {"golang.org/x/net", "v0.55.0"},
		"CVE-2026-42502": {"golang.org/x/net", "v0.55.0"},
		"CVE-2026-42506": {"golang.org/x/net", "v0.55.0"},
		// golang.org/x/crypto/ssh*, fixed in 0.52.0.
		"CVE-2026-39827": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39828": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39829": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39830": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39831": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39832": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39835": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-42508": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-46595": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-46597": {"golang.org/x/crypto", "v0.52.0"},
		// google.golang.org/grpc, fixed in 1.83.1.
		"CVE-2026-84304": {"google.golang.org/grpc", "v1.83.1"},
		"CVE-2026-84445": {"google.golang.org/grpc", "v1.83.1"},
	}

	goMod := readFile(t, root, "go.mod")
	have := map[string][3]int{
		"golang.org/x/net":       goModVersion(t, goMod, "golang.org/x/net"),
		"golang.org/x/crypto":    goModVersion(t, goMod, "golang.org/x/crypto"),
		"google.golang.org/grpc": goModVersion(t, goMod, "google.golang.org/grpc"),
	}

	var doc struct {
		Vulnerabilities []struct {
			ID    string   `yaml:"id"`
			Paths []string `yaml:"paths"`
		} `yaml:"vulnerabilities"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}

	for _, v := range doc.Vulnerabilities {
		fix, tracked := gcModuleCVEs[v.ID]
		if !tracked {
			continue
		}
		waivesGC := false
		for _, p := range v.Paths {
			if p == "usr/local/bin/gc" {
				waivesGC = true
			}
		}
		if !waivesGC {
			continue
		}
		if semverAtLeast(have[fix.module], parseModuleSemver(t, fix.fixVersion)) {
			t.Errorf("%s still waives usr/local/bin/gc but go.mod pins %s >= %s, which fixes it; drop the gc path so the container scan proves the gc binary is clean", v.ID, fix.module, fix.fixVersion)
		}
	}
}

// TestTrivyIgnoreRefreshesBridgeHorizonAndWaivesXNetDNSMessageCVE enforces the
// 2026-09-01 mayor-ruled bridge (ga-wb9e3a): every existing waiver's expired_at
// moves from the lapsed 2026-08-07 horizon to the short 2026-09-21 bridge, and
// exactly one new entry waives CVE-2026-46600 (golang.org/x/net dns/dnsmessage,
// DoS via invalid DNS record parsing, fixed upstream in x/net 0.56.0) on the
// vendored gh and dolt binaries. The ruling explicitly forbids trimming or
// removing any existing entry, so the total entry count must grow by exactly one.
// Widened again 2026-09-11 (operator ruling docket E9) to add
// usr/local/bin/kubectl to this same entry rather than a duplicate — see
// TestTrivyIgnoreWidensDocketE9WaiverForRemainingHighCriticalFindings.
func TestTrivyIgnoreRefreshesBridgeHorizonAndWaivesXNetDNSMessageCVE(t *testing.T) {
	root := repoRoot(t)

	var doc struct {
		Vulnerabilities []struct {
			ID        string   `yaml:"id"`
			Paths     []string `yaml:"paths"`
			ExpiredAt string   `yaml:"expired_at"`
			Statement string   `yaml:"statement"`
		} `yaml:"vulnerabilities"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}

	const bridgeHorizon = "2026-09-21"
	const newCVE = "CVE-2026-46600"
	wantNewPaths := map[string]bool{
		"usr/bin/gh":            true,
		"usr/local/bin/dolt":    true,
		"usr/local/bin/kubectl": true,
	}

	if got, want := len(doc.Vulnerabilities), 64; got != want {
		t.Errorf(".trivyignore.yaml has %d entries, want %d (63 existing + exactly 1 new); the bridge must not trim, remove, or duplicate entries", got, want)
	}

	newCVECount := 0
	for _, v := range doc.Vulnerabilities {
		if v.ExpiredAt != bridgeHorizon {
			t.Errorf("%s expired_at = %q, want the refreshed bridge horizon %q", v.ID, v.ExpiredAt, bridgeHorizon)
		}
		if strings.TrimSpace(v.Statement) == "" {
			t.Errorf("%s has no statement; every waived entry must keep or gain a comment naming its durable fix path", v.ID)
		}
		if v.ID != newCVE {
			continue
		}
		newCVECount++
		gotPaths := map[string]bool{}
		for _, p := range v.Paths {
			gotPaths[p] = true
		}
		if len(gotPaths) != len(wantNewPaths) {
			t.Errorf("%s paths = %v, want exactly %v", v.ID, v.Paths, wantNewPaths)
		}
		for p := range wantNewPaths {
			if !gotPaths[p] {
				t.Errorf("%s missing required path %q", v.ID, p)
			}
		}
		for p := range gotPaths {
			if !wantNewPaths[p] {
				t.Errorf("%s waives unexpected path %q; scope is gh, dolt, and kubectl only", v.ID, p)
			}
		}
		statement := strings.ToLower(v.Statement)
		if !strings.Contains(statement, "x/net") {
			t.Errorf("%s statement %q does not name golang.org/x/net as the durable fix path", v.ID, v.Statement)
		}
		if !strings.Contains(statement, "0.56.0") {
			t.Errorf("%s statement %q does not name the fixed x/net version 0.56.0", v.ID, v.Statement)
		}
	}
	if newCVECount != 1 {
		t.Errorf(".trivyignore.yaml has %d entries for %s, want exactly 1", newCVECount, newCVE)
	}
}

// TestTrivyIgnoreWaivesXCryptoSSHCVEForGHDoltBD enforces the 2026-09-10
// operator-ruled widening (docket D6, "Widen the waiver"): exactly one new
// entry waives CVE-2026-56854 (golang.org/x/crypto/ssh, CRITICAL) scoped to
// the three bundled binaries that carry it at this bridge's horizon —
// usr/bin/gh (x/crypto v0.53.0), usr/local/bin/dolt (v0.50.0), and
// usr/local/bin/bd (v0.53.0) — on the same expiry as the rest of the bridge.
// kubectl is untouched: it does not bundle these binaries. The durable fix is
// austinborn's #5353 (gh/dolt rebuild from patched modules) plus rebuilding
// bd from beads main, whose go.mod already carries golang.org/x/crypto >=
// v0.54.0. The ruling forbids trimming or removing any existing entry, so the
// total entry count must grow by exactly one.
func TestTrivyIgnoreWaivesXCryptoSSHCVEForGHDoltBD(t *testing.T) {
	root := repoRoot(t)

	var doc struct {
		Vulnerabilities []struct {
			ID        string   `yaml:"id"`
			Paths     []string `yaml:"paths"`
			ExpiredAt string   `yaml:"expired_at"`
			Statement string   `yaml:"statement"`
		} `yaml:"vulnerabilities"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}

	const bridgeHorizon = "2026-09-21"
	const newCVE = "CVE-2026-56854"
	wantPaths := map[string]bool{
		"usr/bin/gh":         true,
		"usr/local/bin/dolt": true,
		"usr/local/bin/bd":   true,
	}

	if got, want := len(doc.Vulnerabilities), 64; got != want {
		t.Errorf(".trivyignore.yaml has %d entries, want %d (63 existing + exactly 1 new); the widening must not trim, remove, or duplicate entries", got, want)
	}

	newCVECount := 0
	for _, v := range doc.Vulnerabilities {
		if v.ID != newCVE {
			continue
		}
		newCVECount++
		if v.ExpiredAt != bridgeHorizon {
			t.Errorf("%s expired_at = %q, want the same bridge horizon %q as the rest of the waiver", v.ID, v.ExpiredAt, bridgeHorizon)
		}
		gotPaths := map[string]bool{}
		for _, p := range v.Paths {
			gotPaths[p] = true
		}
		if len(gotPaths) != len(wantPaths) {
			t.Errorf("%s paths = %v, want exactly %v", v.ID, v.Paths, wantPaths)
		}
		for p := range wantPaths {
			if !gotPaths[p] {
				t.Errorf("%s missing required path %q", v.ID, p)
			}
		}
		for p := range gotPaths {
			if !wantPaths[p] {
				t.Errorf("%s waives unexpected path %q; scope is gh, dolt, and bd only (no kubectl)", v.ID, p)
			}
		}
		statement := strings.ToLower(v.Statement)
		for _, want := range []string{"x/crypto", "ssh", "5353", "0.54.0"} {
			if !strings.Contains(statement, want) {
				t.Errorf("%s statement %q does not name %q (durable fix path: PR #5353 and bd rebuilt from beads main with x/crypto >= v0.54.0)", v.ID, v.Statement, want)
			}
		}
	}
	if newCVECount != 1 {
		t.Errorf(".trivyignore.yaml has %d entries for %s, want exactly 1", newCVECount, newCVE)
	}
}

// TestTrivyIgnoreWidensDocketE9WaiverForRemainingHighCriticalFindings enforces
// the 2026-09-11 operator-ruled widening (docket E9): PR #5885's own Container
// Scan run (34553600725 / job 103121488224, "Image vulnerabilities") still
// fails on findings this bridge does not yet cover. This widens the waiver to
// every unwaived HIGH/CRITICAL finding on that run, scoped by path or purl:
//   - usr/bin/gh: x/mod (CVE-2026-56864, CVE-2026-56865) and grpc
//     (CVE-2026-84304, CVE-2026-84445); durable fix is contributor PR #5353.
//   - usr/local/bin/dolt: the same grpc pair plus thrift (CVE-2026-43871);
//     durable fix is contributor PR #5353.
//   - usr/local/bin/bd and usr/local/bin/gc: the same grpc pair (bd also
//     carries the thrift CVE); durable fix is ga-rl1l10.
//   - usr/local/bin/kubectl (gc-controller only): six Go-stdlib CVEs fixed in
//     Go 1.26.6, plus CVE-2026-46600 added to the existing gh/dolt x/net
//     dns/dnsmessage entry rather than duplicated; durable fix is ga-rl1l10.
//   - gc-mcp-mail, purl-scoped (no paths — these purls exist only in that
//     image's SBOM): Debian 13.5 util-linux family (CVE-2026-53612,
//     CVE-2026-53613, CVE-2026-53614) and Python GitPython 3.1.58
//     (CVE-2026-78675, CVE-2026-78676, CVE-2026-78677); durable fix is
//     ga-dkgeoi.
//
// The ruling forbids trimming, removing, or duplicating any existing entry,
// and forbids new Go-stdlib waivers on the rebuilt gh/dolt/bd paths (they
// clear those CVEs via the Go 1.26.5 rebuild — see
// TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools), so the total entry count
// must grow by exactly 17: CVE-2026-46600's widening reuses its existing
// entry and does not add to the count.
func TestTrivyIgnoreWidensDocketE9WaiverForRemainingHighCriticalFindings(t *testing.T) {
	root := repoRoot(t)

	var doc struct {
		Vulnerabilities []struct {
			ID        string   `yaml:"id"`
			Paths     []string `yaml:"paths"`
			Purls     []string `yaml:"purls"`
			ExpiredAt string   `yaml:"expired_at"`
			Statement string   `yaml:"statement"`
		} `yaml:"vulnerabilities"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}

	const bridgeHorizon = "2026-09-21"

	if got, want := len(doc.Vulnerabilities), 64; got != want {
		t.Errorf(".trivyignore.yaml has %d entries, want %d (47 existing + exactly 17 new); the E9 widening must not trim, remove, or duplicate entries", got, want)
	}

	toSet := func(vals ...string) map[string]bool {
		m := make(map[string]bool, len(vals))
		for _, v := range vals {
			m[v] = true
		}
		return m
	}

	type wantEntry struct {
		id         string
		paths      map[string]bool
		purls      map[string]bool
		substrings []string
	}
	wantEntries := []wantEntry{
		{id: "CVE-2026-56864", paths: toSet("usr/bin/gh"), substrings: []string{"x/mod", "0.40.0", "5353"}},
		{id: "CVE-2026-56865", paths: toSet("usr/bin/gh"), substrings: []string{"x/mod", "0.40.0", "5353"}},
		{
			id:         "CVE-2026-84304",
			paths:      toSet("usr/bin/gh", "usr/local/bin/dolt", "usr/local/bin/bd", "usr/local/bin/gc"),
			substrings: []string{"grpc", "5353", "rl1l10", "1.83.1"},
		},
		{
			id:         "CVE-2026-84445",
			paths:      toSet("usr/bin/gh", "usr/local/bin/dolt", "usr/local/bin/bd", "usr/local/bin/gc"),
			substrings: []string{"grpc", "5353", "rl1l10", "1.83.1"},
		},
		{
			id:         "CVE-2026-43871",
			paths:      toSet("usr/local/bin/dolt", "usr/local/bin/bd"),
			substrings: []string{"thrift", "0.24.0", "5353", "rl1l10"},
		},
		{id: "CVE-2026-33818", paths: toSet("usr/local/bin/kubectl"), substrings: []string{"1.26.6", "rl1l10"}},
		{id: "CVE-2026-56853", paths: toSet("usr/local/bin/kubectl"), substrings: []string{"1.26.6", "rl1l10"}},
		{id: "CVE-2026-56858", paths: toSet("usr/local/bin/kubectl"), substrings: []string{"1.26.6", "rl1l10"}},
		{id: "CVE-2026-56859", paths: toSet("usr/local/bin/kubectl"), substrings: []string{"1.26.6", "rl1l10"}},
		{id: "CVE-2026-56860", paths: toSet("usr/local/bin/kubectl"), substrings: []string{"1.26.6", "rl1l10"}},
		{id: "CVE-2026-56862", paths: toSet("usr/local/bin/kubectl"), substrings: []string{"1.26.6", "rl1l10"}},
		{
			id: "CVE-2026-53612",
			purls: toSet(
				"pkg:deb/debian/bsdutils", "pkg:deb/debian/libblkid1", "pkg:deb/debian/liblastlog2-2",
				"pkg:deb/debian/libmount1", "pkg:deb/debian/libsmartcols1", "pkg:deb/debian/libuuid1",
				"pkg:deb/debian/login", "pkg:deb/debian/mount", "pkg:deb/debian/util-linux",
			),
			substrings: []string{"util-linux", "2.41.5-0+deb13u1", "dkgeoi"},
		},
		{
			id: "CVE-2026-53613",
			purls: toSet(
				"pkg:deb/debian/bsdutils", "pkg:deb/debian/libblkid1", "pkg:deb/debian/liblastlog2-2",
				"pkg:deb/debian/libmount1", "pkg:deb/debian/libsmartcols1", "pkg:deb/debian/libuuid1",
				"pkg:deb/debian/login", "pkg:deb/debian/mount", "pkg:deb/debian/util-linux",
			),
			substrings: []string{"util-linux", "2.41.5-0+deb13u1", "dkgeoi"},
		},
		{
			id: "CVE-2026-53614",
			purls: toSet(
				"pkg:deb/debian/bsdutils", "pkg:deb/debian/libblkid1", "pkg:deb/debian/liblastlog2-2",
				"pkg:deb/debian/libmount1", "pkg:deb/debian/libsmartcols1", "pkg:deb/debian/libuuid1",
				"pkg:deb/debian/login", "pkg:deb/debian/mount", "pkg:deb/debian/util-linux",
			),
			substrings: []string{"util-linux", "2.41.5-0+deb13u1", "dkgeoi"},
		},
		{
			id:         "CVE-2026-78676",
			purls:      toSet("pkg:pypi/gitpython"),
			substrings: []string{"gitpython", "3.1.59", "dkgeoi", "critical"},
		},
		{
			id:         "CVE-2026-78675",
			purls:      toSet("pkg:pypi/gitpython"),
			substrings: []string{"gitpython", "3.1.59", "dkgeoi"},
		},
		{
			id:         "CVE-2026-78677",
			purls:      toSet("pkg:pypi/gitpython"),
			substrings: []string{"gitpython", "3.1.59", "dkgeoi"},
		},
	}

	byID := map[string][]int{}
	for i, v := range doc.Vulnerabilities {
		byID[v.ID] = append(byID[v.ID], i)
	}

	for _, want := range wantEntries {
		idxs := byID[want.id]
		if len(idxs) != 1 {
			t.Errorf("%s appears in %d entries, want exactly 1 new entry", want.id, len(idxs))
			continue
		}
		v := doc.Vulnerabilities[idxs[0]]
		if v.ExpiredAt != bridgeHorizon {
			t.Errorf("%s expired_at = %q, want the bridge horizon %q", v.ID, v.ExpiredAt, bridgeHorizon)
		}
		if want.paths != nil {
			gotPaths := toSet(v.Paths...)
			if len(gotPaths) != len(want.paths) {
				t.Errorf("%s paths = %v, want exactly %v", v.ID, v.Paths, want.paths)
			}
			for p := range want.paths {
				if !gotPaths[p] {
					t.Errorf("%s missing required path %q", v.ID, p)
				}
			}
			for p := range gotPaths {
				if !want.paths[p] {
					t.Errorf("%s waives unexpected path %q", v.ID, p)
				}
			}
			if len(v.Purls) != 0 {
				t.Errorf("%s sets purls %v on a path-scoped binary finding; want no purls", v.ID, v.Purls)
			}
		}
		if want.purls != nil {
			gotPurls := toSet(v.Purls...)
			if len(gotPurls) != len(want.purls) {
				t.Errorf("%s purls = %v, want exactly %v", v.ID, v.Purls, want.purls)
			}
			for p := range want.purls {
				if !gotPurls[p] {
					t.Errorf("%s missing required purl %q", v.ID, p)
				}
			}
			for p := range gotPurls {
				if !want.purls[p] {
					t.Errorf("%s waives unexpected purl %q", v.ID, p)
				}
			}
			if len(v.Paths) != 0 {
				t.Errorf("%s sets paths %v on a purl-scoped package finding; want no paths (confine to gc-mcp-mail via the purl match alone)", v.ID, v.Paths)
			}
		}
		statement := strings.ToLower(v.Statement)
		for _, sub := range want.substrings {
			if !strings.Contains(statement, strings.ToLower(sub)) {
				t.Errorf("%s statement %q does not name %q", v.ID, v.Statement, sub)
			}
		}
	}

	// CVE-2026-46600 (golang.org/x/net dns/dnsmessage) is widened in place to
	// add usr/local/bin/kubectl; it must not be duplicated into a second entry.
	const dnsCVE = "CVE-2026-46600"
	dnsIdxs := byID[dnsCVE]
	if len(dnsIdxs) != 1 {
		t.Fatalf("%s appears in %d entries, want exactly 1 (widened in place, not duplicated)", dnsCVE, len(dnsIdxs))
	}
	dnsEntry := doc.Vulnerabilities[dnsIdxs[0]]
	wantDNSPaths := toSet("usr/bin/gh", "usr/local/bin/dolt", "usr/local/bin/kubectl")
	gotDNSPaths := toSet(dnsEntry.Paths...)
	if len(gotDNSPaths) != len(wantDNSPaths) {
		t.Errorf("%s paths = %v, want exactly %v (gh, dolt, and now kubectl)", dnsCVE, dnsEntry.Paths, wantDNSPaths)
	}
	for p := range wantDNSPaths {
		if !gotDNSPaths[p] {
			t.Errorf("%s missing required path %q", dnsCVE, p)
		}
	}
	for p := range gotDNSPaths {
		if !wantDNSPaths[p] {
			t.Errorf("%s waives unexpected path %q", dnsCVE, p)
		}
	}
}

// extractStatedTrivyIgnoreEntryCounts parses the release-gate doc's stated
// .trivyignore.yaml entry count (total, pre-existing, and newly added) out
// of its "Acceptance evidence" prose. This lets a test cross-check that
// stated count against the file's actual parsed entry count, instead of
// trusting the doc's prose to stay accurate by manual review alone.
//
// Not yet implemented: the parser is wired up in the next commit. For now
// this always reports zero, which is deliberately wrong so the caller's
// cross-check fails until the real parsing lands.
func extractStatedTrivyIgnoreEntryCounts(t *testing.T, _ string) (total, preExisting, added int) {
	t.Helper()
	return 0, 0, 0
}

// TestReleaseGateWaiverBridgeDocEntryCountMatchesTrivyIgnore cross-verifies
// release-gates/ga-2yq3p5-container-scan-waiver-bridge-gate.md's stated
// .trivyignore.yaml waiver-entry count against the file's actual parsed
// entry count. Round 1 review (2026-09-11) found this criterion was
// manually-verified-only: the doc's "Acceptance evidence" section states a
// specific count in prose, and the tests above each independently hardcode
// that same total as a Go literal, but nothing ties either to the doc's own
// stated figure -- so an edit to .trivyignore.yaml that updated every
// hardcoded literal could still leave the doc's prose wrong, or vice versa.
// This test closes that gap.
func TestReleaseGateWaiverBridgeDocEntryCountMatchesTrivyIgnore(t *testing.T) {
	root := repoRoot(t)

	doc := readFile(t, root, "release-gates/ga-2yq3p5-container-scan-waiver-bridge-gate.md")
	statedTotal, statedPreExisting, statedAdded := extractStatedTrivyIgnoreEntryCounts(t, doc)

	if statedPreExisting+statedAdded != statedTotal {
		t.Errorf("release-gates/ga-2yq3p5-container-scan-waiver-bridge-gate.md is internally inconsistent: %d pre-existing + %d new = %d, not its own stated total %d", statedPreExisting, statedAdded, statedPreExisting+statedAdded, statedTotal)
	}

	var trivyDoc struct {
		Vulnerabilities []struct {
			ID string `yaml:"id"`
		} `yaml:"vulnerabilities"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &trivyDoc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}

	if got := len(trivyDoc.Vulnerabilities); got != statedTotal {
		t.Errorf(".trivyignore.yaml has %d entries, but release-gates/ga-2yq3p5-container-scan-waiver-bridge-gate.md states %d; keep the release-gate doc's stated waiver-entry count in sync with the actual file", got, statedTotal)
	}
}
