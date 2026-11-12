package scripts_test

import (
	"bufio"
	"crypto/sha1" //nolint:gosec // An OpenPGP v4 fingerprint is defined as SHA-1 (RFC 4880 sec 12.2).
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// CVE-2026-105437 is a heap overflow in git's xdiff three-way merge: a
// committed .gitattributes with a huge conflict-marker-size wraps the 32-bit
// merge-buffer size. Upstream fixed it on each maintained branch; 2.49.x and
// every .0 release on a fixed branch (2.55.0, 2.56.0) are still vulnerable.
var gitCVE2026105437FixedPatch = map[int]int{50: 2, 51: 3, 52: 1, 53: 1, 54: 1, 55: 1, 56: 1}

// gitCorePPAFingerprint is the Launchpad signing key of ppa:git-core/ppa
// ("Ubuntu Git Maintainers"), as published by the Launchpad API's
// signing_key_fingerprint for ~git-core/+archive/ubuntu/ppa.
const gitCorePPAFingerprint = "F911AB184317630C59970973E363C90F8F1B6217"

// knownGitTarballSHA256 maps an upstream git release to the sha256 of its
// .tar.gz, from kernel.org's signed sha256sums.asc. A GIT_VERSION bump must add
// its checksum here, so the pin cannot drift from a reviewed value.
var knownGitTarballSHA256 = map[string]string{
	"2.56.1": "fd0c0495427dc9ac445fdced8e36bb5eb6f233a14da5db3c6dfd8dcfa7f85761",
}

func gitReleaseFixesCVE2026105437(version string) (bool, error) {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false, fmt.Errorf("git version %q is not MAJOR.MINOR.PATCH", version)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return false, fmt.Errorf("git version %q: %w", version, err)
		}
		nums[i] = n
	}
	major, minor, patch := nums[0], nums[1], nums[2]
	switch {
	case major != 2:
		return major > 2, nil
	case minor < 50:
		return false, nil
	case minor > 56:
		return true, nil
	default:
		return patch >= gitCVE2026105437FixedPatch[minor], nil
	}
}

func TestGitReleaseFixesCVE2026105437(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"2.43.0", false},
		{"2.49.1", false},
		{"2.50.1", false},
		{"2.50.2", true},
		{"2.51.2", false},
		{"2.51.3", true},
		{"2.54.1", true},
		{"2.55.0", false},
		{"2.55.1", true},
		{"2.56.0", false},
		{"2.56.1", true},
		{"2.57.0", true},
		{"3.0.0", true},
		{"1.99.9", false},
	} {
		got, err := gitReleaseFixesCVE2026105437(tc.version)
		if err != nil {
			t.Fatalf("gitReleaseFixesCVE2026105437(%q): %v", tc.version, err)
		}
		if got != tc.want {
			t.Errorf("gitReleaseFixesCVE2026105437(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
	for _, bad := range []string{"2.56", "2.56.1.1", "v2.56.1", "2.x.1"} {
		if _, err := gitReleaseFixesCVE2026105437(bad); err == nil {
			t.Errorf("gitReleaseFixesCVE2026105437(%q) accepted a malformed version", bad)
		}
	}
}

// requireFixedGitArg returns the value of `ARG name=<prefix>X.Y.Z` and fails
// unless X.Y.Z is a release that fixes CVE-2026-105437.
func requireFixedGitArg(t *testing.T, path, dockerfile, name, prefix string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^ARG ` + regexp.QuoteMeta(name) + `=` + regexp.QuoteMeta(prefix) + `(\S+)$`)
	m := re.FindStringSubmatch(dockerfile)
	if m == nil {
		t.Fatalf("%s must declare ARG %s=%sX.Y.Z", path, name, prefix)
	}
	fixed, err := gitReleaseFixesCVE2026105437(m[1])
	if err != nil {
		t.Fatalf("%s ARG %s: %v", path, name, err)
	}
	if !fixed {
		t.Errorf("%s ARG %s=%s%s predates the CVE-2026-105437 fix (2.50.2, 2.51.3, 2.52.1, 2.53.1, 2.54.1, 2.55.1, 2.56.1 or later)", path, name, prefix, m[1])
	}
	return m[1]
}

// requireNoDistroGitInstall fails if an apt-get install list names git: every
// Debian and Ubuntu archive git is still vulnerable to CVE-2026-105437.
func requireNoDistroGitInstall(t *testing.T, path, text string) {
	t.Helper()
	if regexp.MustCompile(`(?m)^\s+git \\$`).MatchString(text) {
		t.Errorf("%s installs the distro git package, which no Debian/Ubuntu archive has fixed for CVE-2026-105437", path)
	}
}

// TestAgentBaseImageInstallsPatchedGit guards the runtime stage of
// Dockerfile.base (inherited by gc-agent and gc-controller): git comes from the
// signed git-core PPA, and the build fails unless the installed package is a
// release that fixes CVE-2026-105437. The PPA keeps only its newest build, so
// the version is asserted as a floor rather than pinned exactly.
func TestAgentBaseImageInstallsPatchedGit(t *testing.T) {
	const path = "contrib/k8s/Dockerfile.base"
	dockerfile := readFile(t, repoRoot(t), path)
	idx := strings.LastIndex(dockerfile, "\nFROM ")
	if idx < 0 {
		t.Fatalf("%s has no FROM line", path)
	}
	runtime := dockerfile[idx:]

	requireNoDistroGitInstall(t, path+" (runtime stage)", runtime)
	requireFixedGitArg(t, path, runtime, "GIT_MIN_VERSION", "1:")
	for _, want := range []string{
		"COPY contrib/k8s/git-core-ppa.asc /etc/apt/keyrings/git-core-ppa.asc",
		"URIs: https://ppa.launchpadcontent.net/git-core/ppa/ubuntu",
		"Signed-By: /etc/apt/keyrings/git-core-ppa.asc",
		"apt-get install -y --no-install-recommends git",
		`dpkg --compare-versions "$(dpkg-query -W -f='${Version}' git)" ge "${GIT_MIN_VERSION}"`,
	} {
		if !strings.Contains(runtime, want) {
			t.Errorf("%s runtime stage missing %q", path, want)
		}
	}
}

// TestGitCorePPAKeyIsLaunchpadSigningKey pins the committed PPA key to the
// fingerprint Launchpad publishes, so a swapped key file fails here rather than
// being trusted by apt.
func TestGitCorePPAKeyIsLaunchpadSigningKey(t *testing.T) {
	armored := readFile(t, repoRoot(t), "contrib/k8s/git-core-ppa.asc")
	fpr, err := openPGPv4Fingerprint(armored)
	if err != nil {
		t.Fatalf("contrib/k8s/git-core-ppa.asc: %v", err)
	}
	if fpr != gitCorePPAFingerprint {
		t.Fatalf("contrib/k8s/git-core-ppa.asc fingerprint = %s, want %s", fpr, gitCorePPAFingerprint)
	}
}

// TestMCPMailImageBuildsPatchedGit guards Dockerfile.mail: its Debian base has
// no fixed git package, so git is built from the checksum-pinned upstream
// release instead of installed from apt.
func TestMCPMailImageBuildsPatchedGit(t *testing.T) {
	const path = "contrib/k8s/Dockerfile.mail"
	dockerfile := readFile(t, repoRoot(t), path)

	requireNoDistroGitInstall(t, path, dockerfile)
	version := requireFixedGitArg(t, path, dockerfile, "GIT_VERSION", "")
	wantSHA, ok := knownGitTarballSHA256[version]
	if !ok {
		t.Fatalf("knownGitTarballSHA256 has no checksum for git %s; add the .tar.gz entry from kernel.org sha256sums.asc", version)
	}
	if !strings.Contains(dockerfile, "\nARG GIT_SHA256="+wantSHA+"\n") {
		t.Errorf("%s must pin ARG GIT_SHA256=%s for git %s", path, wantSHA, version)
	}
	for _, want := range []string{
		`"https://mirrors.edge.kernel.org/pub/software/scm/git/git-${GIT_VERSION}.tar.gz"`,
		`echo "${GIT_SHA256}  /tmp/git.tar.gz" | sha256sum -c -`,
		"NO_RUST=YesPlease",
		"&& git --version",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("%s missing %q", path, want)
		}
	}
}

// openPGPv4Fingerprint returns the upper-case hex v4 fingerprint of the first
// public-key packet in an ASCII-armored OpenPGP key.
func openPGPv4Fingerprint(armored string) (string, error) {
	var b64 strings.Builder
	sc := bufio.NewScanner(strings.NewReader(armored))
	inBlock, inBody := false, false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "-----BEGIN PGP PUBLIC KEY BLOCK-----":
			inBlock = true
		case !inBlock:
		case strings.HasPrefix(line, "-----END "):
			inBlock = false
		case !inBody:
			if line == "" {
				inBody = true
			}
		case strings.HasPrefix(line, "="):
		default:
			b64.WriteString(line)
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		return "", fmt.Errorf("decoding armor: %w", err)
	}
	if len(data) < 2 || data[0]&0x80 == 0 {
		return "", fmt.Errorf("not an OpenPGP packet")
	}
	var tag, bodyLen, off int
	if data[0]&0x40 != 0 {
		tag = int(data[0] & 0x3f)
		l1 := int(data[1])
		switch {
		case l1 < 192:
			bodyLen, off = l1, 2
		case l1 < 224:
			if len(data) < 3 {
				return "", fmt.Errorf("truncated packet length")
			}
			bodyLen, off = ((l1-192)<<8)+int(data[2])+192, 3
		case l1 == 255:
			if len(data) < 6 {
				return "", fmt.Errorf("truncated packet length")
			}
			bodyLen, off = int(data[2])<<24|int(data[3])<<16|int(data[4])<<8|int(data[5]), 6
		default:
			return "", fmt.Errorf("partial-length key packet not supported")
		}
	} else {
		tag = int(data[0]>>2) & 0x0f
		switch data[0] & 0x03 {
		case 0:
			bodyLen, off = int(data[1]), 2
		case 1:
			if len(data) < 3 {
				return "", fmt.Errorf("truncated packet length")
			}
			bodyLen, off = int(data[1])<<8|int(data[2]), 3
		case 2:
			if len(data) < 5 {
				return "", fmt.Errorf("truncated packet length")
			}
			bodyLen, off = int(data[1])<<24|int(data[2])<<16|int(data[3])<<8|int(data[4]), 5
		default:
			return "", fmt.Errorf("indeterminate-length key packet not supported")
		}
	}
	if tag != 6 {
		return "", fmt.Errorf("first packet tag = %d, want 6 (public key)", tag)
	}
	if off+bodyLen > len(data) || bodyLen < 1 || bodyLen > 0xffff {
		return "", fmt.Errorf("bad public-key packet length %d", bodyLen)
	}
	body := data[off : off+bodyLen]
	if body[0] != 4 {
		return "", fmt.Errorf("public-key packet version = %d, want 4", body[0])
	}
	h := sha1.New() //nolint:gosec // RFC 4880 sec 12.2 v4 fingerprint.
	h.Write([]byte{0x99, byte(bodyLen >> 8), byte(bodyLen)})
	h.Write(body)
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), nil
}
