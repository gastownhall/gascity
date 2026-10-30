package scripts_test

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestContainerCLIToolsRebuildWithPatchedGRPC(t *testing.T) {
	const (
		ghVersion                 = "2.96.0"
		ghSourceRef               = "b300f2ec7ec9dc9addc39b2ad88c54097ded7ca0"
		doltSourceRef             = "05e6bceb1a210a7a8d770b74b8ef4825c165c0a8"
		ghSourceSHA256            = "a0c18c98c73f7333f73e19b3a0bf5bd18673f3dc226193ab6478b3ea1ea18f03"
		doltSourceSHA256          = "8bd82ec8f9693d5e69d79fb26e361438859e4af96c48f99a40f324c0f2fc6d92"
		doltToolchainRelease      = "20260611_0.0.5_trixie"
		doltOptcrossX8664SHA256   = "caf703fb1cbc0c9ff9a5b506f73da6c6f5233c04a455e638cdc50267a4d0c0c0"
		doltOptcrossAarch64SHA256 = "5635d0b38343fefb0c2b600d61c49ad9ceeaa1107bccdec8a60b1789100dc0ce"
		doltICUStaticSHA256       = "8b0234f16da73b9c8d47f86eeef98928879611149e3ee1bb560dddb0ffdd95a1"
	)

	root := repoRoot(t)
	dockerfile := readFile(t, root, "contrib/k8s/Dockerfile.base")
	requireArgsMatchGoMod(t, "contrib/k8s/Dockerfile.base", dockerfile, readFile(t, root, "go.mod"),
		goModPinnedArg{"GRPC_VERSION", "google.golang.org/grpc", "v1.83.2"})
	for _, want := range []string{
		"ARG GH_VERSION=" + ghVersion,
		"ARG GH_SOURCE_REF=" + ghSourceRef,
		"ARG GH_SOURCE_SHA256=" + ghSourceSHA256,
		"ARG DOLT_SOURCE_REF=" + doltSourceRef,
		"ARG DOLT_SOURCE_SHA256=" + doltSourceSHA256,
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
		bdSourceRef    = "9b65b9e198acbcc98e7fd3a87a5d55baa065a572"
		bdSourceSHA256 = "f176a649d688893026de5a7f4b74af51cf7737ad69fd4fcde3295b123407928f"
		bdBuild        = "9b65b9e198a"
		bdBranch       = "HEAD"
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
	// The agent image also ships gc, built straight from go.mod, so these floors keep
	// gc's own grpc and x/net at or above the patched versions as well as bd's.
	requireArgsMatchGoMod(t, "contrib/k8s/Dockerfile.agent", dockerfile, readFile(t, root, "go.mod"),
		goModPinnedArg{"GRPC_VERSION", "google.golang.org/grpc", "v1.83.2"},
		goModPinnedArg{"THRIFT_VERSION", "github.com/apache/thrift", "v0.24.0"},
		goModPinnedArg{"XNET_VERSION", "golang.org/x/net", "v0.60.0"},
		goModPinnedArg{"JOSE_VERSION", "github.com/go-jose/go-jose/v4", "v4.1.6"},
	)
	for _, want := range []string{
		"ARG BD_VERSION=" + bdVersion,
		"ARG BD_SOURCE_REF=" + bdSourceRef,
		"ARG BD_SOURCE_SHA256=" + bdSourceSHA256,
		"ARG BD_BUILD=" + bdBuild,
		"ARG BD_BRANCH=" + bdBranch,
		`https://github.com/gastownhall/beads/archive/${BD_SOURCE_REF}.tar.gz`,
		`echo "${BD_SOURCE_SHA256}  /tmp/bd-source.tar.gz" | sha256sum --check --strict`,
		`grep -Fq "Version = \"${bd_version}\"" cmd/bd/version.go`,
		`go get "google.golang.org/grpc@v${GRPC_VERSION}"`,
		`go get "github.com/apache/thrift@v${THRIFT_VERSION}"`,
		`go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep github.com/apache/thrift v${THRIFT_VERSION} "`,
		`go get "golang.org/x/net@v${XNET_VERSION}"`,
		`go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep golang.org/x/net v${XNET_VERSION} "`,
		`go get "github.com/go-jose/go-jose/v4@v${JOSE_VERSION}"`,
		`go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep github.com/go-jose/go-jose/v4 v${JOSE_VERSION} "`,
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
	if got := strings.Count(dockerfile, `go get "github.com/apache/thrift@v${THRIFT_VERSION}"`); got != 1 {
		t.Errorf("contrib/k8s/Dockerfile.agent applies the bd thrift override %d times, want exactly 1", got)
	}
	if strings.Contains(dockerfile, "COPY bd /usr/local/bin/bd") {
		t.Error("contrib/k8s/Dockerfile.agent still copies the vulnerable prebuilt bd binary")
	}
	baseImageArg := strings.Index(dockerfile, "ARG BASE_IMAGE=")
	firstStage := strings.Index(dockerfile, "FROM ")
	if baseImageArg < 0 || firstStage < 0 || baseImageArg > firstStage {
		t.Error("contrib/k8s/Dockerfile.agent must declare BASE_IMAGE globally before its first FROM")
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
		"gitpython>=3.1.62,<3.2",
		"aiohttp>=3.14.4",
		"anyio>=4.14.2",
		"pillow>=13.0.0",
		"mcp>=1.30.0",
		"urllib3>=2.8.0",
		"fsspec>=2026.6.0",
		"litellm>=1.106.3",
		"filelock>=3.29.0,<4",
		"opentelemetry-api>=1.45.1",
		"dnspython>=2.9.0",
	} {
		if !strings.Contains(input, want) {
			t.Errorf("mcp-agent-mail input requirements missing security floor %q", want)
		}
	}
	overrides := readFile(t, root, ".github/requirements/mcp-agent-mail.overrides.txt")
	for _, want := range []string{
		"cryptography>=50.0.0",
		"pyjwt>=2.15.0",
		"fastmcp>=3.4.10",
		"authlib>=1.9.1",
	} {
		if !strings.Contains(overrides, want) {
			t.Errorf("mcp-agent-mail overrides missing security floor %q", want)
		}
	}

	lock := readFile(t, root, ".github/requirements/mcp-agent-mail.txt")
	for _, want := range []string{
		"gitpython==3.1.62 \\",
		"aiohttp==3.14.4 \\",
		"anyio==4.14.2 \\",
		"cryptography==50.0.0 \\",
		"pillow==13.0.0 \\",
		"mcp==1.30.0 \\",
		"pyjwt==2.15.1 \\",
		"urllib3==2.8.0 \\",
		"fsspec==2026.6.0 \\",
		"litellm==1.106.3 \\",
		"fastmcp==3.4.10 \\",
		"fastmcp-slim==3.4.10 \\",
		"filelock==3.29.0 \\",
		"opentelemetry-api==1.45.1 \\",
		"authlib==1.9.1 \\",
		"dnspython==2.9.0 \\",
		// Lock-only CVE bumps: no input floor, so this pin is their only guard.
		"bleach==6.4.0 \\",
		"h2==4.4.1 \\",
		"hpack==4.2.0 \\",
		"multidict==6.9.1 \\",
		"pydantic-settings==2.14.2 \\",
	} {
		if !strings.Contains(lock, want) {
			t.Errorf("mcp-agent-mail hashed lock missing patched dependency %q", want)
		}
	}
}

// mcpMailLockPath is the hashed lock Dockerfile.mail installs.
const mcpMailLockPath = ".github/requirements/mcp-agent-mail.txt"

// mcpMailFilelockExitRegression is the script that reproduces filelock 4.x
// truncating the mail database at interpreter exit.
const mcpMailFilelockExitRegression = ".github/scripts/mcp-mail-filelock-exit-regression.py"

// lockPinnedVersion returns the version the hashed lock pins for dist, matching
// the "name==version \" line uv writes. Names compare case-insensitively.
func lockPinnedVersion(t *testing.T, lock, dist string) string {
	t.Helper()
	for _, line := range strings.Split(lock, "\n") {
		name, rest, ok := strings.Cut(line, "==")
		if !ok || !strings.EqualFold(name, dist) {
			continue
		}
		version, _, _ := strings.Cut(rest, " ")
		return version
	}
	t.Fatalf("%s does not pin %s", mcpMailLockPath, dist)
	return ""
}

// releaseLine returns the first n numeric components of a dotted release.
func releaseLine(t *testing.T, version string, n int) []int {
	t.Helper()
	parts := strings.Split(version, ".")
	if len(parts) < n {
		t.Fatalf("version %q has fewer than %d components", version, n)
	}
	line := make([]int, n)
	for i := range line {
		v, err := strconv.Atoi(parts[i])
		if err != nil {
			t.Fatalf("parsing version %q component %q: %v", version, parts[i], err)
		}
		line[i] = v
	}
	return line
}

// TestMCPMailImageHoldsFilelockAndGitPythonOnSafeLines keeps the two packages
// #8307 moved off their safe release lines from drifting back. The deep review of
// #8307 (https://github.com/gastownhall/gascity/pull/8307#pullrequestreview-5645840708)
// found that:
//   - filelock 4.x can truncate the mail SQLite database at shutdown. mcp-agent-mail's
//     emergency cleanup (storage.cleanup_leaked_lockfile_fds) closes lock fds behind
//     filelock's back, and 4.x's exit-time release writes "filelock/released\n" through
//     the reused fd. mcp-mail-filelock-exit-regression.py reproduces it, and the container
//     scan runs that script in the image.
//   - GitPython 3.2 makes archive commits 1.5-2.9x slower. Bursts of mail then time out
//     on the archive lock: 293-322 of 400 messages delivered, against about 395 on 3.1.x.
//
// The advisories the newer lines fix are unreachable here. Each is waived for the
// pinned release in .trivyignore.yaml (TestTrivyIgnoreMCPMailWaiversNameTheLockedRelease).
// Lift a ceiling only once the newer line has fixed its regression, and run the
// script against the new lock when you do.
func TestMCPMailImageHoldsFilelockAndGitPythonOnSafeLines(t *testing.T) {
	lock := readFile(t, repoRoot(t), mcpMailLockPath)

	if filelock := lockPinnedVersion(t, lock, "filelock"); releaseLine(t, filelock, 1)[0] >= 4 {
		t.Errorf("%s pins filelock %s; filelock 4.x truncates the mail database through a reused fd at exit, so keep it below 4", mcpMailLockPath, filelock)
	}
	if gitpython := lockPinnedVersion(t, lock, "gitpython"); !slices.Equal(releaseLine(t, gitpython, 2), []int{3, 1}) {
		t.Errorf("%s pins GitPython %s; GitPython 3.2 slows archive commits enough to drop mail under bursts, so keep it on 3.1.x", mcpMailLockPath, gitpython)
	}
}

// TestContainerScanRunsMCPMailFilelockExitRegression keeps the filelock exit
// regression running against the built gc-mcp-mail image. A text pin can say
// which filelock the lock names; only this run shows that the image's actual
// filelock and mcp-agent-mail leave the database intact. The workflow must also
// rerun when the script itself changes.
func TestContainerScanRunsMCPMailFilelockExitRegression(t *testing.T) {
	root := repoRoot(t)
	script := readFile(t, root, mcpMailFilelockExitRegression)
	for _, want := range []string{"cleanup_leaked_lockfile_fds", "AsyncFileLock", "SQLite format 3"} {
		if !strings.Contains(script, want) {
			t.Errorf("%s no longer exercises %q; it must drive mcp-agent-mail's own lock and cleanup and check the database header", mcpMailFilelockExitRegression, want)
		}
	}

	workflow := readFile(t, root, ".github/workflows/container-scan.yml")
	stepStart := strings.Index(workflow, "- name: Check MCP mail image database survives filelock exit")
	if stepStart < 0 {
		t.Fatal(".github/workflows/container-scan.yml has no step running the filelock exit regression in the gc-mcp-mail image")
	}
	step := workflow[stepStart:]
	if next := strings.Index(step[1:], "\n      - name: "); next >= 0 {
		step = step[:next+1]
	}
	for _, want := range []string{
		"docker run --rm",
		"--entrypoint python3",
		"/" + mcpMailFilelockExitRegression + ":/tmp/mcp-mail-filelock-exit-regression.py:ro",
		`"gc-mcp-mail:${IMAGE_TAG}"`,
		"/tmp/mcp-mail-filelock-exit-regression.py",
	} {
		if !strings.Contains(step, want) {
			t.Errorf("filelock exit regression step missing %q", want)
		}
	}
	if strings.Contains(step, "continue-on-error") || strings.Contains(step, "|| true") {
		t.Error("filelock exit regression step must fail the job when the database is corrupted")
	}
	if got := strings.Count(workflow, `- "`+mcpMailFilelockExitRegression+`"`); got != 2 {
		t.Errorf("container-scan.yml lists %s in %d path filters, want 2 (push and pull_request)", mcpMailFilelockExitRegression, got)
	}
}

// mcpMailSitePackages is where Dockerfile.mail's pip install puts the lock's
// packages, and the path prefix Trivy reports for them.
const mcpMailSitePackages = "usr/local/lib/python3.12/site-packages/"

// reviewedMCPMailWaivers maps each Python-package waiver for the gc-mcp-mail
// image to the distribution it covers. Both advisories are fixed only on the
// lines TestMCPMailImageHoldsFilelockAndGitPythonOnSafeLines keeps the image
// off, and neither is reachable from the image; each statement in
// .trivyignore.yaml gives the reasoning.
var reviewedMCPMailWaivers = map[string]string{
	"CVE-2026-112940": "filelock",  // GHSA-v4x8-3wr5-3wq8, ReadWriteLock only
	"CVE-2026-114145": "gitpython", // GHSA-83vg-56qc-22m7, submodule clone only
}

// TestTrivyIgnoreMCPMailWaiversNameTheLockedRelease holds every gc-mcp-mail
// package waiver to the exact dist-info path of the release the lock pins. Trivy
// matches the path literally, so moving the pin stops the waiver from applying
// and the scan re-checks the new release. This test then fails until the entry
// is dropped or re-pointed with a fresh review. It also keeps the waived set to
// the reviewed entries.
func TestTrivyIgnoreMCPMailWaiversNameTheLockedRelease(t *testing.T) {
	root := repoRoot(t)
	lock := readFile(t, root, mcpMailLockPath)

	var doc trivyIgnoreDoc
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}
	found := map[string]bool{}
	for _, v := range doc.Vulnerabilities {
		for _, p := range v.Paths {
			if !strings.HasPrefix(p, mcpMailSitePackages) {
				continue
			}
			dist, ok := reviewedMCPMailWaivers[v.ID]
			if !ok {
				t.Errorf("%s waives gc-mcp-mail package path %q but is not a reviewed mail waiver; add it to reviewedMCPMailWaivers with its reasoning, or move the pin past it", v.ID, p)
				continue
			}
			want := mcpMailSitePackages + dist + "-" + lockPinnedVersion(t, lock, dist) + ".dist-info/METADATA"
			if p != want {
				t.Errorf("%s waives %q, want %q, the release %s pins; re-check the advisory against the new release before re-pointing it", v.ID, p, want, mcpMailLockPath)
			}
			if len(v.Paths) != 1 || len(v.Purls) != 0 {
				t.Errorf("%s waives the mail package together with paths %q and purls %q; scope it to the one dist-info path", v.ID, v.Paths, v.Purls)
			}
			found[v.ID] = true
		}
	}
	for id, dist := range reviewedMCPMailWaivers {
		if !found[id] {
			t.Errorf(".trivyignore.yaml must waive %s for the locked %s dist-info path while the lock stays on the line that carries it", id, dist)
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
		// gzip CVE-2026-41992; libpcre2-8-0 CVE-2026-86145 and CVE-2026-89161;
		// libsqlite3-0 CVE-2026-11822 and CVE-2026-11824.
		"gzip", "libpcre2-8-0", "libsqlite3-0",
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

	// Each override forces the version go.mod pins, and each floor is at or above what
	// Trivy names as fixed for the findings the override clears.
	requireArgsMatchGoMod(t, "contrib/k8s/Dockerfile.base", base, readFile(t, root, "go.mod"),
		goModPinnedArg{"XCRYPTO_VERSION", "golang.org/x/crypto", "v0.57.0"},
		goModPinnedArg{"XNET_VERSION", "golang.org/x/net", "v0.60.0"},
		goModPinnedArg{"XTEXT_VERSION", "golang.org/x/text", "v0.42.0"},
		goModPinnedArg{"XMOD_VERSION", "golang.org/x/mod", "v0.41.0"},
		goModPinnedArg{"THRIFT_VERSION", "github.com/apache/thrift", "v0.24.0"},
		goModPinnedArg{"COMPRESS_VERSION", "github.com/klauspost/compress", "v1.18.7"},
		goModPinnedArg{"JOSE_VERSION", "github.com/go-jose/go-jose/v4", "v4.1.6"},
	)

	// gh needs x/text, x/net and x/mod. Its pinned source selects x/crypto v0.53.0, below
	// XCRYPTO_VERSION; the patched x/crypto reaches the binary only transitively, through
	// the x/net override's own requirement, and nothing asserts it.
	// Dolt takes no x/mod override because no x/mod package is linked into its binary.
	ghModules := map[string]string{
		"golang.org/x/text": "XTEXT_VERSION",
		"golang.org/x/net":  "XNET_VERSION",
		"golang.org/x/mod":  "XMOD_VERSION",
		// Both pinned sources select klauspost/compress below 1.18.7 (gh v1.18.6,
		// Dolt v1.18.0), which CVE-2026-63209 affects.
		"github.com/klauspost/compress": "COMPRESS_VERSION",
	}
	doltModules := map[string]string{
		"golang.org/x/crypto":           "XCRYPTO_VERSION",
		"golang.org/x/net":              "XNET_VERSION",
		"golang.org/x/text":             "XTEXT_VERSION",
		"github.com/apache/thrift":      "THRIFT_VERSION",
		"github.com/klauspost/compress": "COMPRESS_VERSION",
		// Dolt's pinned source selects go-jose/v4 4.1.4 (CVE-2026-116587).
		"github.com/go-jose/go-jose/v4": "JOSE_VERSION",
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

// trivyIgnoreDoc is the shape of .trivyignore.yaml the guards below read. purls
// is decoded because Trivy honors a purl-scoped waiver in every image, path or
// no path: an entry naming only `purls: ["pkg:golang/github.com/apache/thrift"]`
// is a waiver for bd, dolt and gh at once, and a guard that iterates `paths`
// never sees it.
type trivyIgnoreDoc struct {
	Vulnerabilities []struct {
		ID        string   `yaml:"id"`
		Paths     []string `yaml:"paths"`
		Purls     []string `yaml:"purls"`
		ExpiredAt string   `yaml:"expired_at"`
		Statement string   `yaml:"statement"`
	} `yaml:"vulnerabilities"`
}

// reviewedTrivyIgnorePurlWaivers names the purl-scoped waivers this repo has
// reviewed, as "<id> <purl>". It is empty and is meant to stay that way: a
// module-scoped waiver is exactly the "waive instead of fix" shape the rebuilt-
// tool guard forbids, and the images force the patched modules in instead
// (Dockerfile.base's GRPC_VERSION/THRIFT_VERSION and the x/* pins). Adding an
// entry here is the deliberate edit that admits one.
var reviewedTrivyIgnorePurlWaivers = map[string]bool{}

// unreviewedTrivyIgnorePurlWaivers reports every purl-scoped waiver the file
// carries that no reviewer has admitted, as a ready-to-print message.
func unreviewedTrivyIgnorePurlWaivers(doc trivyIgnoreDoc) []string {
	var found []string
	for _, v := range doc.Vulnerabilities {
		for _, purl := range v.Purls {
			if reviewedTrivyIgnorePurlWaivers[v.ID+" "+purl] {
				continue
			}
			found = append(found, fmt.Sprintf("%s waives module %q by purl, which applies to every image including the rebuilt bd, dolt and gh; move the module forward in the build instead of waiving it", v.ID, purl))
		}
	}
	return found
}

// TestTrivyIgnoreRejectsPurlScopedWaivers drives the guard itself, because the
// file it guards is (correctly) all kubectl paths today, so the assertion in
// TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools cannot demonstrate what it
// catches. The entry below is the one the council named: a thrift regression
// waived by module instead of moving Dockerfile.base's THRIFT_VERSION, on the
// file's own horizon, which both existing guards accepted.
func TestTrivyIgnoreRejectsPurlScopedWaivers(t *testing.T) {
	const waived = `vulnerabilities:
  - id: CVE-2026-43871
    purls:
      - pkg:golang/github.com/apache/thrift
    expired_at: 2026-11-07
  - id: CVE-2026-56852
    paths:
      - usr/local/bin/kubectl
    expired_at: 2026-11-07
`
	var doc trivyIgnoreDoc
	if err := yaml.Unmarshal([]byte(waived), &doc); err != nil {
		t.Fatalf("parsing the fixture: %v", err)
	}
	found := unreviewedTrivyIgnorePurlWaivers(doc)
	if len(found) != 1 {
		t.Fatalf("unreviewedTrivyIgnorePurlWaivers() = %v, want exactly the thrift purl entry", found)
	}
	if !strings.Contains(found[0], "CVE-2026-43871") || !strings.Contains(found[0], "apache/thrift") {
		t.Fatalf("finding = %q, want it to name the CVE and the module", found[0])
	}
}

// unscopedTrivyIgnorePathWaivers reports, as ready-to-print messages, every waiver
// whose reach the path-keyed guards cannot see. Trivy matches each paths entry as a
// doublestar glob (pkg/result/ignore.go), but those guards compare paths as literal
// strings, so a pattern such as usr/local/bin/* waives kubectl, bd, dolt and gc
// together while every guard sees no path it knows. An entry with neither paths nor
// purls is worse: Trivy applies it to every file in every image.
func unscopedTrivyIgnorePathWaivers(doc trivyIgnoreDoc) []string {
	var found []string
	for _, v := range doc.Vulnerabilities {
		if len(v.Paths) == 0 && len(v.Purls) == 0 {
			found = append(found, fmt.Sprintf("%s names no path and no purl, so Trivy waives it for every file in every image; scope it to the literal path of the binary it was reviewed against", v.ID))
		}
		for _, p := range v.Paths {
			if strings.ContainsAny(p, `*?[{\`) {
				found = append(found, fmt.Sprintf("%s waives path pattern %q; Trivy matches it as a glob that can reach binaries the guards here know only by literal path, so list each binary's literal path instead", v.ID, p))
			}
		}
	}
	return found
}

// TestTrivyIgnoreRejectsPathPatternAndUnscopedWaivers drives the path guard with
// entries the real file does not (and must not) contain, because every path in it is
// a literal binary path today. Each pattern below reaches usr/local/bin/kubectl under
// Trivy's doublestar matching without equalling it, so every guard keyed on that
// literal path misses it; the entry with no path and no purl is waived everywhere.
func TestTrivyIgnoreRejectsPathPatternAndUnscopedWaivers(t *testing.T) {
	const waived = `vulnerabilities:
  - id: CVE-2026-90001
    paths:
      - "usr/local/bin/*"
  - id: CVE-2026-90002
    paths:
      - "usr/local/bin/kubect?"
  - id: CVE-2026-90003
    paths:
      - "usr/local/bin/[k]ubectl"
  - id: CVE-2026-90004
    paths:
      - "usr/local/bin/{kubectl,bd}"
  - id: CVE-2026-90005
    paths:
      - 'usr/local/bin/kube\ctl'
  - id: CVE-2026-90006
    expired_at: 2026-12-07
  - id: CVE-2026-78667
    paths:
      - "usr/local/bin/kubectl"
  - id: CVE-2026-43871
    purls:
      - pkg:golang/github.com/apache/thrift
`
	var doc trivyIgnoreDoc
	if err := yaml.Unmarshal([]byte(waived), &doc); err != nil {
		t.Fatalf("parsing the fixture: %v", err)
	}
	found := unscopedTrivyIgnorePathWaivers(doc)
	for _, id := range []string{"CVE-2026-90001", "CVE-2026-90002", "CVE-2026-90003", "CVE-2026-90004", "CVE-2026-90005", "CVE-2026-90006"} {
		flagged := false
		for _, f := range found {
			if strings.HasPrefix(f, id+" ") {
				flagged = true
			}
		}
		if !flagged {
			t.Errorf("unscopedTrivyIgnorePathWaivers() did not flag %s; got %q", id, found)
		}
	}
	if len(found) != 6 {
		t.Errorf("unscopedTrivyIgnorePathWaivers() = %q, want exactly the five patterns and the unscoped entry (the literal kubectl path passes, and the purl entry is the purl guard's)", found)
	}
}

// TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools enforces that the rebuilt-from-
// source tools (bd, dolt, gh) carry no waiver at all. They are rebuilt with the Go
// 1.27.3 toolchain, which fixes every stdlib CVE listed, and Dockerfile.base forces
// x/crypto, x/net, x/text and thrift forward in the gh and Dolt builds the same way
// it forces grpc, so a waiver on those paths would let the scan gate mask a regressed
// rebuild instead of proving the fix holds. The surviving reviewed waivers are the
// five kubectl entries the v1.36.5 pin does not clear (CVE-2026-78667,
// CVE-2026-78669, CVE-2026-97031, and the owner-approved CVE-2026-56851 and
// CVE-2026-111943); kubectl is an upstream-signed prebuilt this repo installs
// rather than builds, so nothing here can move its dependencies, and a kubectl
// waiver outside that reviewed set fails here too. gc's module waivers
// are enforced separately by TestTrivyIgnoreDropsGCModuleWaiversPastThreshold.
func TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools(t *testing.T) {
	root := repoRoot(t)

	var doc trivyIgnoreDoc
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}
	for _, unreviewed := range unreviewedTrivyIgnorePurlWaivers(doc) {
		t.Error(unreviewed)
	}
	for _, unscoped := range unscopedTrivyIgnorePathWaivers(doc) {
		t.Error(unscoped)
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
		// kubectl-only stdlib CVEs, fixed in Go 1.26.6.
		"CVE-2026-33818": true, "CVE-2026-56853": true, "CVE-2026-56858": true,
		"CVE-2026-56859": true, "CVE-2026-56860": true, "CVE-2026-56862": true,
	}
	// Waivers that survive, checked as present so an entry cannot be dropped without
	// a deliberate edit here, and as the only rebuilt-path entries allowed, so the set
	// cannot grow without one either. The bd and gc paths of the grpc pair were removed
	// after both moved to 1.83.2; the gh and Dolt grpc paths (CVE-2026-84304,
	// CVE-2026-84445) were removed once Dockerfile.base's own GRPC_VERSION also reached
	// 1.83.2, clearing both CVEs for both binaries. The thrift entry (CVE-2026-43871)
	// is gone entirely: Dockerfile.agent forced bd's thrift to 0.24.0 first, and
	// Dockerfile.base's own THRIFT_VERSION has now followed for the Dolt rebuild, so
	// no rebuilt path is waived for it and a regression below 0.24.0 fails the scan.
	// Nothing rebuilt from source is waived here any more; only kubectl is, and
	// kubectl is a prebuilt.
	reviewedWaivers := map[string]map[string]bool{
		"CVE-2026-78667": {"usr/local/bin/kubectl": true},
		"CVE-2026-78669": {"usr/local/bin/kubectl": true},
		"CVE-2026-97031": {"usr/local/bin/kubectl": true},
		// Owner-approved 2026-10-25 (code-scanning alerts #481 and #490): no kubectl
		// release ships x/text >= 0.41.0 or Go >= 1.26.10 yet. Each statement in
		// .trivyignore.yaml carries the reasoning.
		"CVE-2026-56851":  {"usr/local/bin/kubectl": true},
		"CVE-2026-111943": {"usr/local/bin/kubectl": true},
	}
	foundReviewed := map[string]map[string]bool{}

	for _, v := range doc.Vulnerabilities {
		for _, p := range v.Paths {
			if stdlibCVEs[v.ID] && rebuiltPaths[p] {
				t.Errorf("%s still waives rebuilt tool %q for a Go-stdlib CVE the 1.27.3 rebuild clears; drop the path so the scan proves the fix stays effective", v.ID, p)
			}
			if allowedPaths, ok := reviewedWaivers[v.ID]; ok && allowedPaths[p] {
				if foundReviewed[v.ID] == nil {
					foundReviewed[v.ID] = map[string]bool{}
				}
				foundReviewed[v.ID][p] = true
				continue
			}
			if rebuiltPaths[p] {
				t.Errorf("%s waives rebuilt tool %q; Dockerfile.base forces the patched modules into the gh and Dolt builds and Dockerfile.agent forces bd's grpc, thrift and x/net forward, so move the module forward in that build instead of waiving the path", v.ID, p)
			}
			if p == "usr/local/bin/kubectl" {
				t.Errorf("%s waives kubectl but is not a reviewed kubectl waiver; check the finding against the pinned binary and add it to reviewedWaivers with its reasoning, or move the pin past it", v.ID)
			}
		}
	}
	for cve, paths := range reviewedWaivers {
		for path := range paths {
			if !foundReviewed[cve][path] {
				t.Errorf(".trivyignore.yaml must retain the reviewed %s waiver for %s until its pin moves past the fixed version", cve, path)
			}
		}
	}
}

// TestTrivyIgnoreKubectlWaiversNameThePinnedRelease keeps every kubectl waiver's
// statement in step with contrib/k8s/Dockerfile.controller's KUBECTL_VERSION. Each
// statement records what the pinned release ships ("kubectl vX.Y.Z ships ..."), so
// a pin bump that leaves a statement naming the old release has not re-checked that
// waiver against the new binary: either the new release clears it and the entry
// should go, or it does not and the statement must say what the new release ships.
func TestTrivyIgnoreKubectlWaiversNameThePinnedRelease(t *testing.T) {
	root := repoRoot(t)

	var pins []string
	for _, line := range strings.Split(readFile(t, root, "contrib/k8s/Dockerfile.controller"), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ARG KUBECTL_VERSION="); ok {
			pins = append(pins, strings.TrimSpace(v))
		}
	}
	switch {
	case len(pins) == 0:
		t.Fatal("contrib/k8s/Dockerfile.controller has no ARG KUBECTL_VERSION=; the kubectl pin moved and this guard must follow it")
	case len(pins) > 1:
		t.Fatalf("contrib/k8s/Dockerfile.controller declares ARG KUBECTL_VERSION= %d times %q; a later declaration overrides the earlier one for every instruction after it; keep exactly one so this guard reads the release the build installs", len(pins), pins)
	}
	pin := pins[0]

	var doc trivyIgnoreDoc
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}
	for _, stale := range staleKubectlWaiverStatements(pin, doc) {
		t.Error(stale)
	}
}

// kubectlShipsClause matches the "kubectl <release> ships" clause a kubectl
// waiver's statement uses to record what a release ships, capturing the release.
var kubectlShipsClause = regexp.MustCompile(`kubectl (v\S+) ships`)

// staleKubectlWaiverStatements reports every usr/local/bin/kubectl waiver whose
// statement does not describe the pinned release, as a ready-to-print message:
// one that never says what the pin ships, and one that names the pin but still
// says what another release ships in some other clause.
func staleKubectlWaiverStatements(pin string, doc trivyIgnoreDoc) []string {
	var found []string
	want := "kubectl " + pin + " ships"
	for _, v := range doc.Vulnerabilities {
		if !slices.Contains(v.Paths, "usr/local/bin/kubectl") {
			continue
		}
		if !strings.Contains(v.Statement, want) {
			found = append(found, fmt.Sprintf("%s waives kubectl but its statement does not say what the pinned %s ships (want %q in it); re-check the waiver against the pinned binary and drop it if the pin clears it", v.ID, pin, want))
			continue
		}
		for _, clause := range kubectlShipsClause.FindAllStringSubmatch(v.Statement, -1) {
			if clause[1] != pin {
				found = append(found, fmt.Sprintf("%s waives kubectl but its statement still says what kubectl %s ships next to the pinned %s; re-check that clause against the pinned binary and restate it for %s", v.ID, clause[1], pin, pin))
			}
		}
	}
	return found
}

// TestTrivyIgnoreRejectsKubectlStatementsNamingAnotherRelease drives the kubectl
// statement guard itself, because the file it guards is (correctly) in step with
// the pin today, so TestTrivyIgnoreKubectlWaiversNameThePinnedRelease cannot
// demonstrate what it catches. The fixture is the next pin bump done by halves:
// CVE-2026-78669's statement says what the release ships twice (x/net and Go), and
// moving only the first clause to the new pin must not pass the guard. Nor may
// CVE-2026-111943's free-form statement, which never says what any release ships
// and so would never go stale at a pin bump.
func TestTrivyIgnoreRejectsKubectlStatementsNamingAnotherRelease(t *testing.T) {
	const waived = `vulnerabilities:
  - id: CVE-2026-78667
    paths:
      - usr/local/bin/kubectl
    statement: Reached through Go stdlib (kubectl v1.36.6 ships 1.26.8; fixed in 1.26.9, 1.27.2).
  - id: CVE-2026-78669
    paths:
      - usr/local/bin/kubectl
    statement: Reached through golang.org/x/net (kubectl v1.36.6 ships v0.56.0; fixed in 0.60.0) and Go stdlib (kubectl v1.36.5 ships 1.26.8; fixed in 1.26.9, 1.27.2).
  - id: CVE-2026-97031
    paths:
      - usr/local/bin/kubectl
    statement: Reached through Go stdlib (kubectl v1.36.5 ships 1.26.8; fixed in 1.26.9, 1.27.2).
  - id: CVE-2026-56852
    paths:
      - usr/local/bin/kubectl
    statement: Reached through golang.org/x/text (kubectl v1.36.60 ships v0.33.0; fixed in 0.39.0).
  - id: CVE-2026-111943
    paths:
      - usr/local/bin/kubectl
    statement: Accepted risk; see the kubectl notes.
  - id: CVE-2026-25681
    paths:
      - usr/local/bin/bd
    statement: Not a kubectl waiver, so it may name whatever kubectl v1.36.5 ships.
`
	var doc trivyIgnoreDoc
	if err := yaml.Unmarshal([]byte(waived), &doc); err != nil {
		t.Fatalf("parsing the fixture: %v", err)
	}
	found := staleKubectlWaiverStatements("v1.36.6", doc)
	for _, id := range []string{"CVE-2026-78669", "CVE-2026-97031", "CVE-2026-56852", "CVE-2026-111943"} {
		flagged := false
		for _, f := range found {
			if strings.HasPrefix(f, id+" ") {
				flagged = true
			}
		}
		if !flagged {
			t.Errorf("staleKubectlWaiverStatements() did not flag %s; got %q", id, found)
		}
	}
	for _, f := range found {
		if strings.HasPrefix(f, "CVE-2026-78667 ") || strings.HasPrefix(f, "CVE-2026-25681 ") {
			t.Errorf("staleKubectlWaiverStatements() flagged an entry it must accept: %q", f)
		}
	}
}

// TestTrivyIgnoreKubectlWaiversStayScopedAndTimeBoxed holds every kubectl waiver to
// the kubectl binary alone and to one shared expiry. The waivers are reviewed
// against the pinned binary as a set, at each pin bump, so they expire together; a
// waiver that also named another path or a purl would hide its finding wherever
// else it turned up, including in the binaries this image rebuilds from source.
func TestTrivyIgnoreKubectlWaiversStayScopedAndTimeBoxed(t *testing.T) {
	root := repoRoot(t)

	var doc trivyIgnoreDoc
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}
	for _, problem := range kubectlWaiverScopeProblems(doc) {
		t.Error(problem)
	}
}

// kubectlWaiverScopeProblems reports, as ready-to-print messages, every
// usr/local/bin/kubectl waiver that also names another path or a purl, and the
// kubectl waivers' expiries when they are not all one date.
func kubectlWaiverScopeProblems(doc trivyIgnoreDoc) []string {
	var found []string
	expiries := map[string][]string{}
	for _, v := range doc.Vulnerabilities {
		if !slices.Contains(v.Paths, "usr/local/bin/kubectl") {
			continue
		}
		if len(v.Paths) != 1 || len(v.Purls) != 0 {
			found = append(found, fmt.Sprintf("%s waives kubectl together with paths %q and purls %q; scope a kubectl waiver to usr/local/bin/kubectl alone", v.ID, v.Paths, v.Purls))
		}
		expiries[v.ExpiredAt] = append(expiries[v.ExpiredAt], v.ID)
	}
	if len(expiries) > 1 {
		found = append(found, fmt.Sprintf("kubectl waivers expire on %d different dates %v; they are re-checked together at each kubectl pin bump, so keep them on one horizon", len(expiries), expiries))
	}
	return found
}

// TestTrivyIgnoreRejectsMisscopedKubectlWaivers drives kubectlWaiverScopeProblems
// with entries the real file does not (and must not) contain: a kubectl waiver that
// also waives a rebuilt binary, one that adds a purl, and one off the shared horizon.
func TestTrivyIgnoreRejectsMisscopedKubectlWaivers(t *testing.T) {
	const waived = `vulnerabilities:
  - id: CVE-2026-78667
    paths:
      - usr/local/bin/kubectl
    expired_at: 2026-12-07
  - id: CVE-2026-56851
    paths:
      - usr/local/bin/kubectl
      - usr/local/bin/bd
    expired_at: 2026-12-07
  - id: CVE-2026-111943
    paths:
      - usr/local/bin/kubectl
    purls:
      - pkg:golang/stdlib
    expired_at: 2026-12-07
  - id: CVE-2026-97031
    paths:
      - usr/local/bin/kubectl
    expired_at: 2027-03-07
  - id: CVE-2026-25681
    paths:
      - usr/local/bin/dolt
    expired_at: 2027-06-07
`
	var doc trivyIgnoreDoc
	if err := yaml.Unmarshal([]byte(waived), &doc); err != nil {
		t.Fatalf("parsing the fixture: %v", err)
	}
	found := kubectlWaiverScopeProblems(doc)
	for _, want := range []string{"CVE-2026-56851 ", "CVE-2026-111943 ", "kubectl waivers expire on 2 different dates"} {
		flagged := false
		for _, f := range found {
			if strings.HasPrefix(f, want) {
				flagged = true
			}
		}
		if !flagged {
			t.Errorf("kubectlWaiverScopeProblems() did not report %q; got %q", want, found)
		}
	}
	if len(found) != 3 {
		t.Errorf("kubectlWaiverScopeProblems() = %q, want exactly the two misscoped entries and the split horizon (the dolt entry is not a kubectl waiver)", found)
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

// goModPinnedArg is a Dockerfile ARG that forces module into a rebuilt tool, and the
// patched floor go.mod's pin for that module must not fall below.
type goModPinnedArg struct {
	arg, module, floor string
}

// requireArgsMatchGoMod checks that dockerfile declares each ARG at exactly the
// version go.mod pins for its module, and that the go.mod pin is at or above the
// floor. The expected value comes from go.mod rather than a literal because a
// go.mod-only bump otherwise leaves the images behind with every guard green, which
// is how #7387 left them on x/net 0.58.0; the floor keeps a coordinated downgrade of
// both from passing.
func requireArgsMatchGoMod(t *testing.T, name, dockerfile, goMod string, pins ...goModPinnedArg) {
	t.Helper()
	for _, pin := range pins {
		have := goModVersion(t, goMod, pin.module)
		version := fmt.Sprintf("%d.%d.%d", have[0], have[1], have[2])
		if !semverAtLeast(have, parseModuleSemver(t, pin.floor)) {
			t.Errorf("go.mod pins %s v%s, below the patched floor %s; raise it, and move %s's %s with it", pin.module, version, pin.floor, name, pin.arg)
		}
		want := "ARG " + pin.arg + "=" + version
		if !strings.Contains(dockerfile, "\n"+want+"\n") {
			t.Errorf("%s missing %q, the %s version go.mod pins; move the ARG with go.mod so the image cannot fall behind it", name, want, pin.module)
		}
	}
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
		// google.golang.org/grpc. CVE-2026-84445 is fixed in 1.82.2 on the 1.82 line
		// and 1.83.2 on the 1.83 line go.mod is on, so 1.83.1 clears only the first.
		"CVE-2026-84304": {"google.golang.org/grpc", "v1.83.1"},
		"CVE-2026-84445": {"google.golang.org/grpc", "v1.83.2"},
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

// TestTrivyIgnoreKeepsReviewedBridgeEntries holds main's time-boxed waiver bridge
// (#5885) retired. The bridge carried four reviewed entries on its own 2026-09-21
// horizon rather than this file's, for findings no rebuild cleared at the time; every
// one of them was closed by moving the pin its statement named, not by re-dating it.
// CVE-2026-43871 went when contrib/k8s/Dockerfile.base's THRIFT_VERSION reached
// 0.24.0 (TestRebuiltToolsForcePatchedXModules pins that), and the three GitPython
// CVEs went when .github/requirements/mcp-agent-mail.txt reached 3.1.59
// (TestMCPMailImagePinsPatchedPythonDependencies pins that).
//
// So the guard is now the absence of the bridge: no entry may carry a horizon at or
// behind the bridge's, because such an entry is either the bridge coming back under a
// new statement or a waiver Trivy already treats as expired, and an expired waiver
// fails the scan gate at the next run rather than at review time. Re-dating one of
// these forward is a deliberate decision that belongs in this file's own horizon with
// a statement to match, which is exactly the edit this test forces.
func TestTrivyIgnoreKeepsReviewedBridgeEntries(t *testing.T) {
	root := repoRoot(t)

	var doc struct {
		Vulnerabilities []struct {
			ID        string `yaml:"id"`
			ExpiredAt string `yaml:"expired_at"`
		} `yaml:"vulnerabilities"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}
	if len(doc.Vulnerabilities) == 0 {
		t.Fatal(".trivyignore.yaml parsed to no entries; the guard below would pass vacuously")
	}

	// ISO-8601 dates compare correctly as strings, so <= is "at or behind".
	const bridgeHorizon = "2026-09-21"
	for _, v := range doc.Vulnerabilities {
		if v.ExpiredAt == "" {
			t.Errorf("%s has no expired_at; every waiver in this file is time-boxed", v.ID)
			continue
		}
		if v.ExpiredAt <= bridgeHorizon {
			t.Errorf("%s expires %s, at or behind the retired bridge horizon %s; fix the finding as the bridge's own entries were, or move it to this file's horizon with a statement saying why", v.ID, v.ExpiredAt, bridgeHorizon)
		}
	}
}
