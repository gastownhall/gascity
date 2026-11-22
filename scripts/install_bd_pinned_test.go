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

// installBdPin is the beads version the stub go reports go.mod pins.
const installBdPin = "v1.3.4"

// installBdStubGo stands in for the go toolchain under `make install-bd`.
// It logs every invocation (argv plus the build-relevant environment and
// working directory) and answers:
//   - `env ...` (the Makefile's parse-time probes) with nothing;
//   - `mod edit -json <go.mod>` with a module requiring beads at
//     $STUB_PIN, plus $STUB_REPLACE_JSON appended as a Replace array;
//   - `install ...` by writing a fake bd into $GOBIN that embeds
//     $STUB_STAGED_VERSION, or by failing when $STUB_INSTALL_FAIL is set;
//   - `version -m <file>` from the file's "beads-version:" marker, failing
//     like the real tool when the file carries none.
const installBdStubGo = `#!/bin/sh
printf '%s|GOFLAGS=%s|GOWORK=%s|CGO_ENABLED=%s|PWD=%s\n' "$*" "${GOFLAGS-unset}" "${GOWORK-unset}" "${CGO_ENABLED-unset}" "$(pwd)" >> "$STUB_GO_LOG"
case "$1" in
env) exit 0 ;;
mod)
	printf '{\n\t"Module": {\n\t\t"Path": "github.com/gastownhall/gascity"\n\t},\n\t"Go": "1.25",\n\t"Require": [\n\t\t{\n\t\t\t"Path": "github.com/steveyegge/beads-extra",\n\t\t\t"Version": "v9.9.9"\n\t\t},\n\t\t{\n\t\t\t"Path": "github.com/steveyegge/beads",\n\t\t\t"Version": "%s"\n\t\t}\n\t]%s\n}\n' "$STUB_PIN" "$STUB_REPLACE_JSON"
	;;
install)
	if [ -n "$STUB_INSTALL_FAIL" ]; then
		echo 'cgo: C compiler "gcc" not found' >&2
		exit 1
	fi
	printf '#!/bin/sh\n# beads-version: %s\n' "$STUB_STAGED_VERSION" > "$GOBIN/bd"
	chmod 0755 "$GOBIN/bd"
	;;
version)
	file=$3
	v=$(sed -n 's/^# beads-version: //p' "$file")
	if [ -z "$v" ]; then
		echo "$file: could not read Go build info from $file: not a Go executable" >&2
		exit 1
	fi
	printf '%s: go1.25.0\n\tpath\tgithub.com/steveyegge/beads/cmd/bd\n\tmod\tgithub.com/steveyegge/beads\t%s\th1:stub=\n\tdep\tgithub.com/spf13/cobra\tv1.9.1\th1:stub=\n' "$file" "$v"
	;;
*) exit 0 ;;
esac
`

// installBdEnv is one `make install-bd` scenario's sandbox.
type installBdEnv struct {
	repoRoot   string
	installDir string
	home       string
	tmpDir     string
	binDir     string
	goLog      string
	env        []string
}

func newInstallBdEnv(t *testing.T) *installBdEnv {
	t.Helper()
	tmp := t.TempDir()
	e := &installBdEnv{
		repoRoot:   repoRoot(t),
		installDir: filepath.Join(tmp, "install"),
		home:       filepath.Join(tmp, "home"),
		tmpDir:     filepath.Join(tmp, "tmp"),
		binDir:     filepath.Join(tmp, "stubbin"),
		goLog:      filepath.Join(tmp, "go.log"),
	}
	for _, dir := range []string{e.installDir, filepath.Join(e.home, ".local", "bin"), e.tmpDir, e.binDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeExecutable(t, filepath.Join(e.binDir, "go"), installBdStubGo)
	e.env = append(os.Environ(),
		"PATH="+e.binDir+string(os.PathListSeparator)+pathWithoutCommand("bd"),
		"HOME="+e.home,
		"TMPDIR="+e.tmpDir,
		"STUB_GO_LOG="+e.goLog,
		"STUB_PIN="+installBdPin,
		"STUB_STAGED_VERSION="+installBdPin,
		"STUB_REPLACE_JSON=",
		"STUB_INSTALL_FAIL=",
		// A caller's GOFLAGS (-mod=mod, -modfile=...) must not reach the build.
		"GOFLAGS=-mod=mod",
		"GOWORK=",
	)
	return e
}

func (e *installBdEnv) dest() string { return filepath.Join(e.installDir, "bd") }

// run runs `make install-bd` (or target) against the real Makefile.
func (e *installBdEnv) run(t *testing.T, target string) (string, error) {
	t.Helper()
	cmd := exec.Command("make", "--no-print-directory", "-f", filepath.Join(e.repoRoot, "Makefile"), target,
		"INSTALL_DIR="+e.installDir,
	)
	cmd.Dir = e.repoRoot
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// goInstalls returns the logged stub `go install` invocations.
func (e *installBdEnv) goInstalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(e.goLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read go log: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "install ") {
			calls = append(calls, line)
		}
	}
	return calls
}

func fileSum(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// optionalFileSum hashes path, or returns "absent" when it does not exist.
func optionalFileSum(t *testing.T, path string) string {
	t.Helper()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "absent"
	}
	return fileSum(t, path)
}

// embeddedVersion asks the stub `go version -m` which beads a bd embeds,
// the same question the script asks.
func embeddedVersion(t *testing.T, e *installBdEnv, path string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(e.binDir, "go"), "version", "-m", path)
	cmd.Env = append(os.Environ(), "STUB_GO_LOG="+filepath.Join(t.TempDir(), "probe.log"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go version -m %s: %v", path, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "mod" && fields[1] == "github.com/steveyegge/beads" {
			return fields[2]
		}
	}
	t.Fatalf("go version -m %s names no beads main module:\n%s", path, out)
	return ""
}

func TestInstallBdPinnedFreshInstall(t *testing.T) {
	e := newInstallBdEnv(t)
	goMod := filepath.Join(e.repoRoot, "go.mod")
	goSum := filepath.Join(e.repoRoot, "go.sum")
	modBefore, sumBefore := optionalFileSum(t, goMod), optionalFileSum(t, goSum)

	out, err := e.run(t, "install-bd")
	if err != nil {
		t.Fatalf("make install-bd: %v\n%s", err, out)
	}

	info, err := os.Stat(e.dest())
	if err != nil {
		t.Fatalf("installed bd: %v\n%s", err, out)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed bd mode = %v, want executable", info.Mode())
	}
	if got := embeddedVersion(t, e, e.dest()); got != installBdPin {
		t.Fatalf("installed bd embeds beads %s, want %s", got, installBdPin)
	}
	if optionalFileSum(t, goMod) != modBefore || optionalFileSum(t, goSum) != sumBefore {
		t.Fatal("make install-bd changed go.mod or go.sum")
	}

	installs := e.goInstalls(t)
	if len(installs) != 1 {
		t.Fatalf("go install calls = %q, want exactly one", installs)
	}
	want := "install -tags gms_pure_go github.com/steveyegge/beads/cmd/bd@" + installBdPin + "|GOFLAGS=|GOWORK=off|CGO_ENABLED=1|"
	if !strings.HasPrefix(installs[0], want) {
		t.Fatalf("go install call = %q, want prefix %q", installs[0], want)
	}
	// The build runs outside the repository, so no go.mod or go.work there
	// can take part in it.
	if strings.Contains(installs[0], "|PWD="+e.repoRoot) {
		t.Fatalf("go install ran inside the repository: %q", installs[0])
	}

	link := filepath.Join(e.home, ".local", "bin", "bd")
	target, err := os.Readlink(link)
	if err != nil || target != e.dest() {
		t.Fatalf("%s -> %q (%v), want a symlink to %s\n%s", link, target, err, e.dest(), out)
	}
}

// TestMakeInstallDependsOnInstallBd pins the Makefile wiring: `install`
// builds the pinned bd before it replaces gc, and `install-bd` is phony.
func TestMakeInstallDependsOnInstallBd(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	text := string(makefile)
	if !strings.Contains(text, "\ninstall: check-self-contained install-bd\n") {
		t.Fatal("Makefile install target does not depend on install-bd")
	}
	if !strings.Contains(text, "\ninstall-bd:\n") {
		t.Fatal("Makefile has no install-bd target")
	}
	phony := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, ".PHONY:") && strings.Contains(line+" ", " install-bd ") {
			phony = true
		}
	}
	if !phony {
		t.Fatal("install-bd is not declared .PHONY")
	}
}

// writeFakeBd writes a bd that the stub go reports as embedding version.
func writeFakeBd(t *testing.T, path, version string) {
	t.Helper()
	writeExecutable(t, path, "#!/bin/sh\n# beads-version: "+version+"\n")
}

// TestInstallBdPinnedFastPath: a bd that already embeds the pin is left
// alone. Content, not mtime, is compared: mtime is too coarse on some
// filesystems to prove the file was not rewritten.
func TestInstallBdPinnedFastPath(t *testing.T) {
	e := newInstallBdEnv(t)
	writeFakeBd(t, e.dest(), installBdPin)
	before := fileSum(t, e.dest())

	out, err := e.run(t, "install-bd")
	if err != nil {
		t.Fatalf("make install-bd: %v\n%s", err, out)
	}
	if installs := e.goInstalls(t); len(installs) != 0 {
		t.Fatalf("go install ran although bd already embeds the pin: %q\n%s", installs, out)
	}
	if fileSum(t, e.dest()) != before {
		t.Fatalf("make install-bd rewrote a bd that already embeds the pin\n%s", out)
	}
	if !strings.Contains(out, "already") || !strings.Contains(out, installBdPin) {
		t.Fatalf("make install-bd output does not say bd is already at %s:\n%s", installBdPin, out)
	}
}

// installBdSkewed is a beads version other than the pin.
const installBdSkewed = "v1.2.0"

// assertNoInstallBdLeftovers fails when a staging directory or an install
// temp file survived the run.
func assertNoInstallBdLeftovers(t *testing.T, e *installBdEnv, out string) {
	t.Helper()
	staging, err := os.ReadDir(e.tmpDir)
	if err != nil {
		t.Fatalf("read TMPDIR: %v", err)
	}
	for _, entry := range staging {
		t.Errorf("staging left behind in TMPDIR: %s", entry.Name())
	}
	installed, err := os.ReadDir(e.installDir)
	if err != nil {
		t.Fatalf("read install dir: %v", err)
	}
	for _, entry := range installed {
		if strings.HasPrefix(entry.Name(), ".bd.tmp.") {
			t.Errorf("install temp file left behind: %s", entry.Name())
		}
	}
	if t.Failed() {
		t.Logf("make output:\n%s", out)
	}
}

// TestInstallBdPinnedReplacesSkewedBd: a bd from another beads version is
// replaced by a rename, never rewritten in place, so a process running the
// old binary (or a hard link to it) keeps a complete file.
func TestInstallBdPinnedReplacesSkewedBd(t *testing.T) {
	e := newInstallBdEnv(t)
	writeFakeBd(t, e.dest(), installBdSkewed)
	oldContent, err := os.ReadFile(e.dest())
	if err != nil {
		t.Fatal(err)
	}
	oldLink := filepath.Join(t.TempDir(), "old-bd")
	if err := os.Link(e.dest(), oldLink); err != nil {
		t.Fatalf("hard-link the old bd: %v", err)
	}

	out, err := e.run(t, "install-bd")
	if err != nil {
		t.Fatalf("make install-bd: %v\n%s", err, out)
	}
	if got := embeddedVersion(t, e, e.dest()); got != installBdPin {
		t.Fatalf("installed bd embeds beads %s, want %s\n%s", got, installBdPin, out)
	}
	if len(e.goInstalls(t)) != 1 {
		t.Fatalf("go install calls = %q, want one", e.goInstalls(t))
	}
	oldInfo, err := os.Stat(oldLink)
	if err != nil {
		t.Fatal(err)
	}
	newInfo, err := os.Stat(e.dest())
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(oldInfo, newInfo) {
		t.Fatalf("bd was rewritten in place, not replaced by a rename\n%s", out)
	}
	if kept, _ := os.ReadFile(oldLink); string(kept) != string(oldContent) {
		t.Fatalf("the old bd's inode was modified: %q\n%s", kept, out)
	}
	assertNoInstallBdLeftovers(t, e, out)
}
