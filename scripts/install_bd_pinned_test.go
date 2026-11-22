package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file exercises scripts/install-bd-pinned.sh (`make install-bd`) through
// a stub `go`: gc migrates a city store forward to the schema of the beads
// module it links, so the bd on PATH must be built from that same module or
// every read fails with "schema version mismatch" (ga-upttvz). The stub never
// builds anything; it answers `go mod edit -json` with a fixed pin, writes a
// marker-carrying fake bd on `go install`, and answers `go version -m` from
// each file's #BEADS_PIN marker the way the real go answers from build info.

// bdStubPin is the beads version the stub's go.mod requires.
const bdStubPin = "v1.3.4"

// bdStubGo is a stub go command whose answers the test controls.
type bdStubGo struct {
	// binDir holds the stub `go` and is first on PATH.
	binDir string
	// home is the run's HOME, so the ~/.local/bin symlink lands in the test.
	home string
	// tmp is the run's TMPDIR; leftovers there are staging files.
	tmp string
	// modJSON is the file `go mod edit -json` prints.
	modJSON string
	// logPath records one argv line per stub invocation.
	logPath string
	// installPin is the #BEADS_PIN marker the stub's `go install` embeds.
	installPin string
	// installFail makes the stub's `go install` exit 1.
	installFail bool
}

// writeBdStubGo writes the stub go binary and its fixed go.mod answer.
func writeBdStubGo(t *testing.T) *bdStubGo {
	t.Helper()
	tmp := t.TempDir()
	stub := &bdStubGo{
		binDir:     filepath.Join(tmp, "bin"),
		home:       filepath.Join(tmp, "home"),
		tmp:        filepath.Join(tmp, "tmp"),
		modJSON:    filepath.Join(tmp, "mod.json"),
		logPath:    filepath.Join(tmp, "argv.log"),
		installPin: bdStubPin,
	}
	for _, dir := range []string{stub.binDir, stub.home, filepath.Join(stub.home, ".local", "bin"), stub.tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeGoModJSON(t, stub.modJSON, false)
	writeExecutable(t, filepath.Join(stub.binDir, "go"), `#!/bin/sh
[ -n "$BD_STUB_LOG" ] && printf '%s\n' "$*" >> "$BD_STUB_LOG"
case "$1" in
  mod)
    cat "$BD_STUB_MODJSON"
    ;;
  version)
    shift 2
    for f in "$@"; do
      printf 'path\tgithub.com/steveyegge/beads/cmd/bd\n'
      pin=$(sed -n 's/^#BEADS_PIN=//p' "$f" | head -n 1)
      if [ -n "$pin" ]; then
        printf 'mod\tgithub.com/steveyegge/beads\t%s\n' "$pin"
      else
        printf 'mod\texample.com/other\tv1.0.0\n'
      fi
    done
    ;;
  install)
    [ -n "$BD_STUB_LOG" ] && printf 'GOBIN %s\n' "$GOBIN" >> "$BD_STUB_LOG"
    if [ "$BD_STUB_INSTALL_FAIL" = "1" ]; then
      echo 'go: build failed: no C toolchain' >&2
      exit 1
    fi
    printf '#!/bin/sh\n#BEADS_PIN=%s\n' "$BD_STUB_INSTALL_PIN" > "$GOBIN/bd"
    chmod 0755 "$GOBIN/bd"
    ;;
esac
exit 0
`)
	return stub
}

// writeGoModJSON writes the stub's `go mod edit -json` answer, optionally with
// a replace directive that moves beads elsewhere.
func writeGoModJSON(t *testing.T, path string, withBeadsReplace bool) {
	t.Helper()
	json := `{
	"Module": {
		"Path": "github.com/gastownhall/gascity"
	},
	"Go": "1.27.4",
	"Require": [
		{
			"Path": "github.com/spf13/cobra",
			"Version": "v1.10.0"
		},
		{
			"Path": "github.com/steveyegge/beads",
			"Version": "` + bdStubPin + `"
		}
	]`
	if withBeadsReplace {
		json += `,
	"Replace": [
		{
			"Old": {
				"Path": "github.com/steveyegge/beads"
			},
			"New": {
				"Path": "../beads"
			}
		}
	]`
	}
	json += "\n}\n"
	if err := os.WriteFile(path, []byte(json), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeStubBd writes a fake bd whose #BEADS_PIN marker the stub go answers
// from, the way a real bd binary answers from its embedded build info.
func writeStubBd(t *testing.T, path, pin string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n#BEADS_PIN="+pin+"\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// stubBdVersionOf asks the stub go what `go version -m path` would report.
func stubBdVersionOf(t *testing.T, stub *bdStubGo, path string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(stub.binDir, "go"), "version", "-m", path)
	cmd.Env = append(os.Environ(), "BD_STUB_LOG=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("stub go version -m %s: %v", path, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if fields := strings.Split(line, "\t"); len(fields) >= 3 && fields[0] == "mod" && fields[1] == "github.com/steveyegge/beads" {
			return fields[2]
		}
	}
	return ""
}

// runInstallBdPinned runs the install script under the stub go, with a PATH
// that holds no other bd unless extraPATH prepends one.
func runInstallBdPinned(t *testing.T, stub *bdStubGo, extraPATH string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	path := stub.binDir + string(os.PathListSeparator) + pathWithoutCommand("bd")
	if extraPATH != "" {
		path = extraPATH + string(os.PathListSeparator) + path
	}
	cmd := exec.Command(filepath.Join(root, "scripts", "install-bd-pinned.sh"))
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+path,
		"HOME="+stub.home,
		"INSTALL_DIR="+stub.installDir(),
		"TMPDIR="+stub.tmp,
		"BD_STUB_LOG="+stub.logPath,
		"BD_STUB_MODJSON="+stub.modJSON,
		"BD_STUB_INSTALL_PIN="+stub.installPin,
	)
	if stub.installFail {
		cmd.Env = append(cmd.Env, "BD_STUB_INSTALL_FAIL=1")
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (s *bdStubGo) installDir() string { return filepath.Join(s.home, "gc-bin") }

// stubArgv returns the stub go's argv log, one line per invocation.
func stubArgv(t *testing.T, stub *bdStubGo) []string {
	t.Helper()
	data, err := os.ReadFile(stub.logPath)
	if err != nil {
		t.Fatalf("read %s: %v", stub.logPath, err)
	}
	var argv []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			argv = append(argv, line)
		}
	}
	return argv
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// stubInstallGOBINs returns the GOBIN directory of every `go install` the
// stub served.
func stubInstallGOBINs(t *testing.T, stub *bdStubGo) []string {
	t.Helper()
	var bins []string
	for _, line := range stubArgv(t, stub) {
		if after, ok := strings.CutPrefix(line, "GOBIN "); ok {
			bins = append(bins, after)
		}
	}
	return bins
}

// assertNoLeftovers fails when the install dir holds a temporary file or the
// run's TMPDIR holds anything at all: staging files must never outlive a run.
func assertNoLeftovers(t *testing.T, stub *bdStubGo) {
	t.Helper()
	entries, err := os.ReadDir(stub.installDir())
	if err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".bd.tmp") || strings.Contains(entry.Name(), "install-bd.") {
				t.Errorf("leftover temporary file in the install dir: %s", entry.Name())
			}
		}
	}
	if tmpEntries, err := os.ReadDir(stub.tmp); err == nil && len(tmpEntries) > 0 {
		names := make([]string, 0, len(tmpEntries))
		for _, entry := range tmpEntries {
			names = append(names, entry.Name())
		}
		t.Errorf("leftover staging files in TMPDIR: %v", names)
	}
}

// TestInstallBdPinnedReplacesASkewedBdThroughStaging is AC-3: a bd embedding
// some other beads version is replaced by the pinned build. The build stages
// outside the install dir and lands through tmp + rename, so the install dir
// never holds a half-written bd, and no staging or tmp file survives the run.
func TestInstallBdPinnedReplacesASkewedBdThroughStaging(t *testing.T) {
	stub := writeBdStubGo(t)
	if err := os.MkdirAll(stub.installDir(), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", stub.installDir(), err)
	}
	bd := filepath.Join(stub.installDir(), "bd")
	writeStubBd(t, bd, "v1.2.0")

	out, err := runInstallBdPinned(t, stub, "")
	if err != nil {
		t.Fatalf("install-bd-pinned.sh over a skewed bd: %v\n%s", err, out)
	}
	if got := stubBdVersionOf(t, stub, bd); got != bdStubPin {
		t.Fatalf("the skewed bd was not replaced: `go version -m` reports %q, want %q:\n%s", got, bdStubPin, out)
	}
	for _, gobin := range stubInstallGOBINs(t, stub) {
		if gobin == stub.installDir() {
			t.Errorf("the build wrote straight into the install dir (GOBIN=%s); it must stage elsewhere and land by rename:\n%s", gobin, out)
		}
	}
	assertNoLeftovers(t, stub)
}

// TestInstallBdPinnedFreshInstallEmbedsThePin is AC-1: with no bd installed,
// make install-bd builds bd from go.mod's beads pin and installs it beside gc
// without touching the module's own go.mod or go.sum.
func TestInstallBdPinnedFreshInstallEmbedsThePin(t *testing.T) {
	root := repoRoot(t)
	stub := writeBdStubGo(t)
	goModSum := map[string]string{
		"go.mod": fileSHA(t, filepath.Join(root, "go.mod")),
		"go.sum": fileSHA(t, filepath.Join(root, "go.sum")),
	}

	out, err := runInstallBdPinned(t, stub, "")
	if err != nil {
		t.Fatalf("install-bd-pinned.sh: %v\n%s", err, out)
	}
	bd := filepath.Join(stub.installDir(), "bd")
	info, statErr := os.Stat(bd)
	if statErr != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		t.Fatalf("installed bd %s is not an executable file (%v):\n%s", bd, statErr, out)
	}
	if got := stubBdVersionOf(t, stub, bd); got != bdStubPin {
		t.Fatalf("`go version -m %s` reports beads %q, want %q:\n%s", bd, got, bdStubPin, out)
	}
	for name, want := range goModSum {
		if got := fileSHA(t, filepath.Join(root, name)); got != want {
			t.Errorf("%s changed across the install (a scratch build must never touch it)", name)
		}
	}
	link := filepath.Join(stub.home, ".local", "bin", "bd")
	if target, linkErr := os.Readlink(link); linkErr != nil || target != bd {
		t.Errorf("~/.local/bin/bd = %q (%v), want a symlink to %s", target, linkErr, bd)
	}
}

// TestInstallBdPinnedFastPathSkipsTheBuild is AC-2: when the installed bd
// already embeds the pin, the script is a no-op — no `go install` runs (a
// rebuild is minutes of CGO compilation for nothing) and the file's content,
// not its mtime, is what must stay unchanged.
func TestInstallBdPinnedFastPathSkipsTheBuild(t *testing.T) {
	stub := writeBdStubGo(t)
	bd := filepath.Join(stub.installDir(), "bd")
	if err := os.MkdirAll(stub.installDir(), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", stub.installDir(), err)
	}
	writeStubBd(t, bd, bdStubPin)
	before := fileSHA(t, bd)

	out, err := runInstallBdPinned(t, stub, "")
	if err != nil {
		t.Fatalf("install-bd-pinned.sh over an already-pinned bd: %v\n%s", err, out)
	}
	for _, argv := range stubArgv(t, stub) {
		if strings.HasPrefix(argv, "install ") {
			t.Errorf("the fast path still ran `go %s`:\n%s", argv, out)
		}
	}
	if after := fileSHA(t, bd); after != before {
		t.Errorf("the installed bd was rewritten by a no-op run:\n%s", out)
	}
}

// TestInstallBdPinnedRefusesAReplacedModule: a go.mod replace directive moves
// beads' code elsewhere, so its Require version says nothing about the bd gc
// agrees with; the script must refuse rather than build a meaningless pin.
func TestInstallBdPinnedRefusesAReplacedModule(t *testing.T) {
	stub := writeBdStubGo(t)
	writeGoModJSON(t, stub.modJSON, true)

	out, err := runInstallBdPinned(t, stub, "")
	if err == nil {
		t.Fatalf("install-bd-pinned.sh succeeded over a replaced beads module:\n%s", out)
	}
	if !strings.Contains(out, "replace") {
		t.Fatalf("the refusal does not name the replace directive:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(stub.installDir(), "bd")); !os.IsNotExist(statErr) {
		t.Fatalf("a refused install still wrote a bd (%v):\n%s", statErr, out)
	}
}

// TestInstallBdPinnedNotesAPATHShadow: when `command -v bd` resolves to a
// different file than the one just installed, the script says so — the skew
// it fixes is back the moment PATH prefers the old binary.
func TestInstallBdPinnedNotesAPATHShadow(t *testing.T) {
	stub := writeBdStubGo(t)
	otherDir := filepath.Join(stub.tmp, "other")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", otherDir, err)
	}
	otherBd := filepath.Join(otherDir, "bd")
	writeStubBd(t, otherBd, "v0.0.1-shadow")

	out, err := runInstallBdPinned(t, stub, otherDir)
	if err != nil {
		t.Fatalf("install-bd-pinned.sh: %v\n%s", err, out)
	}
	if !strings.Contains(out, otherBd) {
		t.Fatalf("the PATH note does not name the shadowing bd %s:\n%s", otherBd, out)
	}
	if !strings.Contains(out, filepath.Join(stub.installDir(), "bd")) {
		t.Fatalf("the PATH note does not name the installed bd:\n%s", out)
	}
}
