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
		bdSourceRef    = "8e4e59d39f3459a43cf21a3236a13eca4dd874f7"
		bdSourceSHA256 = "63597b6b368d7d26ba3fc570ae3b2fa4cd8a5155d4716cae13d178a560808d5a"
		bdBuild        = "8e4e59d39"
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

func TestMCPMailImagePinsPatchedGitPythonAndPillow(t *testing.T) {
	root := repoRoot(t)
	input := readFile(t, root, ".github/requirements/mcp-agent-mail.in")
	for _, want := range []string{
		"gitpython>=3.1.52",
		"pillow>=12.3.0",
	} {
		if !strings.Contains(input, want) {
			t.Errorf("mcp-agent-mail input requirements missing security floor %q", want)
		}
	}

	lock := readFile(t, root, ".github/requirements/mcp-agent-mail.txt")
	for _, want := range []string{
		"gitpython==3.1.54 \\",
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
// proving the fix holds. The residual x/net / x/crypto module waivers that bd and dolt
// legitimately keep (external binaries the grpc-only rebuild does not touch) are out of
// scope here; gc's x/net / x/crypto module waivers are enforced separately by
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

	ghWaived := false
	for _, v := range doc.Vulnerabilities {
		for _, p := range v.Paths {
			if p == "usr/bin/gh" {
				ghWaived = true
			}
			if stdlibCVEs[v.ID] && rebuiltPaths[p] {
				t.Errorf("%s still waives rebuilt tool %q for a Go-stdlib CVE the 1.26.5 rebuild clears; drop the path so the scan proves the fix stays effective", v.ID, p)
			}
		}
	}
	if ghWaived {
		t.Error(".trivyignore.yaml still waives usr/bin/gh; gh is rebuilt with Go 1.26.5 + patched grpc and must carry no residual waiver")
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
