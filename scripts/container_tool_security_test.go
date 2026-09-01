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
		bdSourceRef    = "bf97b73749ac3ef2fca2365b54537ac041ad4293"
		bdSourceSHA256 = "a8b1d8dd85b2c008093615cb85937067a9597e760e8d39f93fe55f5c1cbb4d37"
		bdBuild        = "bf97b73749"
		bdBranch       = "HEAD"
		grpcVersion    = "1.82.1"
	)

	root := repoRoot(t)
	bdVersion := readDotenv(t, root+"/deps.env")["BD_VERSION"]
	if bdVersion != "v1.1.0" {
		t.Fatalf("deps.env BD_VERSION = %q, want v1.1.0 for the pinned source build", bdVersion)
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

// TestMCPMailImageUpgradesPatchedOSPackages guards the --only-upgrade list in
// Dockerfile.mail. The base image is pinned by digest, so an OS-package CVE is only
// cleared by naming the package here, and Trivy reports every binary package of a
// source separately: dropping one name leaves that package on the vulnerable version
// and the scan red, with the other eight looking like the whole fix.
func TestMCPMailImageUpgradesPatchedOSPackages(t *testing.T) {
	dockerfile := readFile(t, repoRoot(t), "contrib/k8s/Dockerfile.mail")

	upgrade, _, ok := strings.Cut(dockerfile, "&& apt-get install -y --no-install-recommends \\")
	if !ok {
		t.Fatal("contrib/k8s/Dockerfile.mail has no plain apt-get install stanza to bound the --only-upgrade list")
	}
	if !strings.Contains(upgrade, "--only-upgrade") {
		t.Fatal("contrib/k8s/Dockerfile.mail no longer upgrades any pinned-base OS package")
	}

	for _, pkg := range []string{
		// openssl / systemd set, already present.
		"libcap2", "libssl3t64", "libsystemd0", "libudev1", "openssl", "openssl-provider-legacy",
		// util-linux set, CVE-2026-53615, fixed in 2.41.5-0+deb13u1.
		"bsdutils", "libblkid1", "liblastlog2-2", "libmount1", "libsmartcols1",
		"libuuid1", "login", "mount", "util-linux",
	} {
		if !strings.Contains(upgrade, "\n    "+pkg+" \\") {
			t.Errorf("contrib/k8s/Dockerfile.mail --only-upgrade list missing %q", pkg)
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

// TestRebuiltToolsForcePatchedXModules guards the module overrides that replaced the
// gh and Dolt waivers in .trivyignore.yaml. The pinned gh and Dolt sources select
// x/crypto, x/net, x/text and thrift versions Trivy flags, and the grpc-only override
// left them there, which is what kept those paths waived. Dropping either the `go get`
// or its `go version -m` proof would put the vulnerable module back with nothing
// failing, so both halves are asserted here.
func TestRebuiltToolsForcePatchedXModules(t *testing.T) {
	root := repoRoot(t)
	base := readFile(t, root, "contrib/k8s/Dockerfile.base")

	// Versions each override forces, at or above what Trivy names as fixed for the findings it clears.
	for _, arg := range []string{
		"ARG XCRYPTO_VERSION=0.55.0",
		"ARG XNET_VERSION=0.58.0",
		"ARG XTEXT_VERSION=0.41.0",
		"ARG XMOD_VERSION=0.40.0",
		"ARG THRIFT_VERSION=0.23.0",
	} {
		if !strings.Contains(base, arg) {
			t.Errorf("contrib/k8s/Dockerfile.base missing %q", arg)
		}
	}

	// gh needs x/text and x/mod; its pinned source already selects patched x/crypto and x/net.
	// Dolt takes no x/mod override because no x/mod package is linked into its binary.
	ghModules := map[string]string{
		"golang.org/x/text": "XTEXT_VERSION",
		"golang.org/x/mod":  "XMOD_VERSION",
	}
	doltModules := map[string]string{
		"golang.org/x/crypto":      "XCRYPTO_VERSION",
		"golang.org/x/net":         "XNET_VERSION",
		"golang.org/x/text":        "XTEXT_VERSION",
		"github.com/apache/thrift": "THRIFT_VERSION",
	}
	// Each `go get` is looked for inside its own stanza. gh and Dolt share the x/text
	// override verbatim, so a file-wide search lets one stand in for the other and a
	// dropped override reads as present here, failing only in the image build.
	ghStart := strings.Index(base, "WORKDIR /src/gh")
	doltStart := strings.Index(base, "WORKDIR /src/dolt")
	if ghStart < 0 || doltStart <= ghStart {
		t.Fatal("contrib/k8s/Dockerfile.base has no WORKDIR /src/gh stanza ahead of the Dolt one")
	}
	ghStanza := base[ghStart:doltStart]
	doltStanza := base[doltStart:]
	if next := strings.Index(doltStanza, "\nFROM "); next > 0 {
		doltStanza = doltStanza[:next]
	}
	stanzas := map[string]string{"/out/gh": ghStanza, "/out/dolt": doltStanza}

	for bin, modules := range map[string]map[string]string{"/out/gh": ghModules, "/out/dolt": doltModules} {
		for module, arg := range modules {
			get := `"` + module + `@v${` + arg + `}"`
			if !strings.Contains(stanzas[bin], get) {
				t.Errorf("contrib/k8s/Dockerfile.base must override %s inside the %s build stanza; missing %q", module, bin, get)
			}
			assert := `go version -m ` + bin + ` | tr '\t' ' ' | grep -Fq "dep ` + module + ` v${` + arg + `} "`
			if !strings.Contains(base, assert) {
				t.Errorf("contrib/k8s/Dockerfile.base must assert %s embeds patched %s; missing %q", bin, module, assert)
			}
		}
	}

	// Inside the gh stanza the x/mod get has to run after the x/text one. A later
	// `go get` naming a version below what an earlier one dragged in is a downgrade,
	// and it takes the earlier module with it: measured against the pinned source,
	// with XTEXT_VERSION at 0.39.0 the reversed order selects x/mod v0.38.0, under
	// the fixed version. The two orders agree at the versions pinned today, so this
	// is what keeps the next bump from recreating that shape.
	xtextGet := strings.Index(ghStanza, `"golang.org/x/text@v${XTEXT_VERSION}"`)
	xmodGet := strings.Index(ghStanza, `"golang.org/x/mod@v${XMOD_VERSION}"`)
	if xtextGet < 0 || xmodGet < 0 {
		t.Fatal("gh stanza is missing the x/text or the x/mod override")
	}
	if xmodGet < xtextGet {
		t.Error("gh stanza runs the x/mod override ahead of x/text; the newest constraint goes last, so a lower x/text pin cannot downgrade x/mod out from under its assertion")
	}
}

// TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools enforces that the rebuilt-from-
// source tools (bd, dolt, gh) carry no waiver at all. They are rebuilt with the Go
// 1.26.5 toolchain, which fixes every stdlib CVE listed, and Dockerfile.base now
// forces x/crypto, x/net, x/text and thrift forward in the gh and Dolt builds the
// same way it forces grpc, so a waiver on those paths would let the scan gate mask a
// regressed rebuild instead of proving the fix holds. kubectl keeps the x/text waiver
// because it is an upstream-signed prebuilt this repo installs rather than builds.
// gc's x/net / x/crypto module waivers are enforced separately by
// TestTrivyIgnoreDropsGCModuleWaiversPastThreshold.
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
	}
	// Waivers that survive on a non-rebuilt path, checked as present so the entry
	// cannot be dropped without a deliberate edit here.
	reviewedWaivers := map[string]map[string]bool{
		"CVE-2026-56852": {
			"usr/local/bin/kubectl": true,
		},
	}
	foundReviewed := map[string]map[string]bool{}

	for _, v := range doc.Vulnerabilities {
		for _, p := range v.Paths {
			if stdlibCVEs[v.ID] && rebuiltPaths[p] {
				t.Errorf("%s still waives rebuilt tool %q for a Go-stdlib CVE the 1.26.5 rebuild clears; drop the path so the scan proves the fix stays effective", v.ID, p)
			}
			if allowedPaths, ok := reviewedWaivers[v.ID]; ok && allowedPaths[p] {
				if foundReviewed[v.ID] == nil {
					foundReviewed[v.ID] = map[string]bool{}
				}
				foundReviewed[v.ID][p] = true
				continue
			}
			if rebuiltPaths[p] {
				t.Errorf("%s waives rebuilt tool %q; Dockerfile.base forces the patched modules into the gh and Dolt builds and bd's pinned source already selects them, so move the module forward in that build instead of waiving the path", v.ID, p)
			}
		}
	}
	for cve, paths := range reviewedWaivers {
		for path := range paths {
			if !foundReviewed[cve][path] {
				t.Errorf(".trivyignore.yaml must retain the reviewed %s waiver for %s until that source updates its dependency", cve, path)
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
// x/net or x/crypto CVE waiver outlives the go.mod bump that fixes it. Unlike the
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
	}

	goMod := readFile(t, root, "go.mod")
	have := map[string][3]int{
		"golang.org/x/net":    goModVersion(t, goMod, "golang.org/x/net"),
		"golang.org/x/crypto": goModVersion(t, goMod, "golang.org/x/crypto"),
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

// TestGoModPinsXModPastGCFinding guards the gc half of the same container-scan
// finding the Dockerfile overrides cover for gh. gc is built straight from this
// module rather than from pinned third-party source, so no `go get` in a Dockerfile
// can move it: the only floor is go.mod's own pin, and dropping that pin back below
// the fixed version would put the vulnerable module into usr/local/bin/gc with
// nothing in the build failing.
func TestGoModPinsXModPastGCFinding(t *testing.T) {
	// The first version Trivy names as fixed for the x/mod findings on usr/local/bin/gc.
	const xmodFixVersion = "v0.40.0"

	goMod := readFile(t, repoRoot(t), "go.mod")
	have := goModVersion(t, goMod, "golang.org/x/mod")
	if !semverAtLeast(have, parseModuleSemver(t, xmodFixVersion)) {
		t.Errorf("go.mod pins golang.org/x/mod below %s, so the gc binary carries the flagged module; raise the pin rather than waiving the gc path", xmodFixVersion)
	}
}
