package scripts_test

import (
	"strings"
	"testing"
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
