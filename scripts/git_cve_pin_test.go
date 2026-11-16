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
// merge-buffer size. Upstream fixed it on each maintained branch (the same
// releases also fix CVE-2026-111760, -113408, -114466, -114467 and -114469).
// Every release below its branch's fixed patch is vulnerable (e.g. 2.51.2,
// 2.55.0, 2.56.0), as is everything before 2.50.
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

// gitFloorExcludesCVE2026105437 reports whether every release at or above
// floor is fixed. A floor is weaker than an exact release: 2.55.1 is itself
// fixed but admits the vulnerable 2.56.0, so a floor must reach the newest
// affected branch's fixed release.
func gitFloorExcludesCVE2026105437(floor string) (bool, error) {
	if _, err := gitReleaseFixesCVE2026105437(floor); err != nil {
		return false, err
	}
	newest := 0
	for minor := range gitCVE2026105437FixedPatch {
		newest = max(newest, minor)
	}
	newestFixed := fmt.Sprintf("2.%d.%d", newest, gitCVE2026105437FixedPatch[newest])
	return compareGitReleases(floor, newestFixed) >= 0, nil
}

// compareGitReleases compares two well-formed MAJOR.MINOR.PATCH versions.
func compareGitReleases(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range 3 {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func TestGitFloorExcludesCVE2026105437(t *testing.T) {
	for _, tc := range []struct {
		floor string
		want  bool
	}{
		{"2.50.2", false},
		{"2.55.1", false},
		{"2.56.0", false},
		{"2.56.1", true},
		{"2.56.2", true},
		{"2.57.0", true},
		{"3.0.0", true},
	} {
		got, err := gitFloorExcludesCVE2026105437(tc.floor)
		if err != nil {
			t.Fatalf("gitFloorExcludesCVE2026105437(%q): %v", tc.floor, err)
		}
		if got != tc.want {
			t.Errorf("gitFloorExcludesCVE2026105437(%q) = %v, want %v", tc.floor, got, tc.want)
		}
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
// unless X.Y.Z is a release that fixes CVE-2026-105437. With floor set, X.Y.Z is
// a minimum version, so every release it admits must be fixed.
func requireFixedGitArg(t *testing.T, path, dockerfile, name, prefix string, floor bool) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^ARG ` + regexp.QuoteMeta(name) + `=` + regexp.QuoteMeta(prefix) + `(\S+)$`)
	m := re.FindStringSubmatch(dockerfile)
	if m == nil {
		t.Fatalf("%s must declare ARG %s=%sX.Y.Z", path, name, prefix)
	}
	check := gitReleaseFixesCVE2026105437
	if floor {
		check = gitFloorExcludesCVE2026105437
	}
	fixed, err := check(m[1])
	if err != nil {
		t.Fatalf("%s ARG %s: %v", path, name, err)
	}
	if !fixed {
		t.Errorf("%s ARG %s=%s%s admits a git vulnerable to CVE-2026-105437 (exact releases: 2.50.2, 2.51.3, 2.52.1, 2.53.1, 2.54.1, 2.55.1, 2.56.1 or later; floors: 2.56.1 or later)", path, name, prefix, m[1])
	}
	return m[1]
}

// dockerfileRuns returns each RUN instruction with its line continuations
// joined, so a package list can be tokenized however it is wrapped.
func dockerfileRuns(dockerfile string) []string {
	joined := strings.ReplaceAll(dockerfile, "\\\n", " ")
	var runs []string
	for _, line := range strings.Split(joined, "\n") {
		if strings.HasPrefix(line, "RUN ") {
			runs = append(runs, line)
		}
	}
	return runs
}

// installsGit reports whether any apt-get install in run names the git
// package (bare or version-pinned).
func installsGit(run string) bool {
	for _, cmd := range strings.Split(run, "&&") {
		_, args, ok := strings.Cut(cmd, "apt-get install")
		if !ok {
			continue
		}
		for _, tok := range strings.Fields(args) {
			if tok == "git" || strings.HasPrefix(tok, "git=") {
				return true
			}
		}
	}
	return false
}

// requireNoDistroGitInstall fails if a RUN installs git with apt without first
// configuring the git-core PPA: every Debian and Ubuntu archive git is still
// vulnerable to CVE-2026-105437.
func requireNoDistroGitInstall(t *testing.T, path, dockerfile string) {
	t.Helper()
	for _, run := range dockerfileRuns(dockerfile) {
		if installsGit(run) && !strings.Contains(run, "/etc/apt/sources.list.d/git-core-ppa.sources") {
			t.Errorf("%s installs the distro git package, which no Debian/Ubuntu archive has fixed for CVE-2026-105437: %.120s", path, run)
		}
	}
}

func TestRequireNoDistroGitInstallTokenizesInstalls(t *testing.T) {
	for _, tc := range []struct {
		run  string
		want bool
	}{
		{"RUN apt-get update && apt-get install -y --no-install-recommends     curl     git     jq", true},
		{"RUN apt-get install -y bash ca-certificates curl gh git", true},
		{"RUN apt-get install -y git=1:2.43.0-1ubuntu7.4", true},
		{"RUN apt-get install -y gitlab-runner git-lfs", false},
		{"RUN apt-get purge -y git git-man", false},
	} {
		if got := installsGit(tc.run); got != tc.want {
			t.Errorf("installsGit(%q) = %v, want %v", tc.run, got, tc.want)
		}
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
	requireFixedGitArg(t, path, runtime, "GIT_MIN_VERSION", "1:", true)
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
	version := requireFixedGitArg(t, path, dockerfile, "GIT_VERSION", "", false)
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
		"sysconfdir=/etc",
		`! dpkg -s git >/dev/null 2>&1`,
		`! ldd "$(git --exec-path)/git-remote-https" | grep -q 'not found'`,
		"&& git --version",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("%s missing %q", path, want)
		}
	}
}

// openPGPv4Fingerprint returns the upper-case hex v4 fingerprint of an
// ASCII-armored OpenPGP key that holds exactly one key: one armor block, one
// public-key packet and no subkeys or second key, since apt's Signed-By would
// trust every key in the file.
func openPGPv4Fingerprint(armored string) (string, error) {
	if n := strings.Count(armored, "-----BEGIN PGP PUBLIC KEY BLOCK-----"); n != 1 {
		return "", fmt.Errorf("want exactly one armor block, found %d", n)
	}
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
	var key []byte
	for len(data) > 0 {
		tag, body, rest, err := nextOpenPGPPacket(data)
		if err != nil {
			return "", err
		}
		switch tag {
		case 6:
			if key != nil {
				return "", fmt.Errorf("more than one public-key packet")
			}
			key = body
		case 14:
			return "", fmt.Errorf("unexpected public-subkey packet")
		}
		data = rest
	}
	if key == nil {
		return "", fmt.Errorf("no public-key packet")
	}
	if len(key) < 1 || len(key) > 0xffff || key[0] != 4 {
		return "", fmt.Errorf("public-key packet is not a v4 key")
	}
	h := sha1.New() //nolint:gosec // RFC 4880 sec 12.2 v4 fingerprint.
	h.Write([]byte{0x99, byte(len(key) >> 8), byte(len(key))})
	h.Write(key)
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), nil
}

// nextOpenPGPPacket splits the first packet (RFC 4880 sec 4.2) off data.
// Partial and indeterminate lengths do not occur in key files and are rejected.
func nextOpenPGPPacket(data []byte) (tag int, body, rest []byte, err error) {
	if len(data) < 2 || data[0]&0x80 == 0 {
		return 0, nil, nil, fmt.Errorf("not an OpenPGP packet")
	}
	var bodyLen, off int
	if data[0]&0x40 != 0 {
		tag = int(data[0] & 0x3f)
		l1 := int(data[1])
		switch {
		case l1 < 192:
			bodyLen, off = l1, 2
		case l1 < 224 && len(data) >= 3:
			bodyLen, off = ((l1-192)<<8)+int(data[2])+192, 3
		case l1 == 255 && len(data) >= 6:
			bodyLen, off = int(data[2])<<24|int(data[3])<<16|int(data[4])<<8|int(data[5]), 6
		default:
			return 0, nil, nil, fmt.Errorf("unsupported or truncated new-format packet length")
		}
	} else {
		tag = int(data[0]>>2) & 0x0f
		switch {
		case data[0]&0x03 == 0:
			bodyLen, off = int(data[1]), 2
		case data[0]&0x03 == 1 && len(data) >= 3:
			bodyLen, off = int(data[1])<<8|int(data[2]), 3
		case data[0]&0x03 == 2 && len(data) >= 5:
			bodyLen, off = int(data[1])<<24|int(data[2])<<16|int(data[3])<<8|int(data[4]), 5
		default:
			return 0, nil, nil, fmt.Errorf("unsupported or truncated old-format packet length")
		}
	}
	if bodyLen < 0 || off+bodyLen > len(data) {
		return 0, nil, nil, fmt.Errorf("packet length %d overruns the key data", bodyLen)
	}
	return tag, data[off : off+bodyLen], data[off+bodyLen:], nil
}
