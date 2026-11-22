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
//     like the real tool when the file carries none. Like the real tool on a
//     bd, it writes the main module first and then a long dep list: about
//     800 KiB here, more than any pipe buffers, so a reader that stops at
//     the mod line kills it with SIGPIPE however large the host's pipes are.
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
	printf '%s: go1.25.0\n\tpath\tgithub.com/steveyegge/beads/cmd/bd\n\tmod\tgithub.com/steveyegge/beads\t%s\th1:stub=\n' "$file" "$v"
	awk 'BEGIN { for (i = 0; i < 16384; i++) printf "\tdep\tgithub.com/example/dep%05d\tv1.0.0\th1:stub=\n", i }'
	printf '\tbuild\tCGO_ENABLED=1\n'
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
	for _, dir := range []string{e.installDir, e.localBin(), e.tmpDir, e.binDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeExecutable(t, filepath.Join(e.binDir, "go"), installBdStubGo)
	e.env = append(os.Environ(),
		"PATH="+e.pathWith(),
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

func (e *installBdEnv) setenv(kv ...string) { e.env = append(e.env, kv...) }

func (e *installBdEnv) dest() string { return filepath.Join(e.installDir, "bd") }

// localBin is ~/.local/bin in the sandbox, where beads' own installer puts bd.
func (e *installBdEnv) localBin() string { return filepath.Join(e.home, ".local", "bin") }

// pathWith is a PATH of the stub go, then dirs, then the host's PATH without
// any directory holding a bd.
func (e *installBdEnv) pathWith(dirs ...string) string {
	return strings.Join(append(append([]string{e.binDir}, dirs...), pathWithoutCommand("bd")), string(os.PathListSeparator))
}

// run runs `make install-bd` against the real Makefile.
func (e *installBdEnv) run(t *testing.T) (string, error) {
	t.Helper()
	cmd := exec.Command("make", "--no-print-directory", "-f", filepath.Join(e.repoRoot, "Makefile"), "install-bd",
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

// TestInstallBdPinnedFreshInstall: with no bd at the destination, install-bd
// builds the pin outside the repository and installs it, reading the build's
// version from a `go version -m` that writes more than a pipe holds.
func TestInstallBdPinnedFreshInstall(t *testing.T) {
	e := newInstallBdEnv(t)
	goMod := filepath.Join(e.repoRoot, "go.mod")
	goSum := filepath.Join(e.repoRoot, "go.sum")
	modBefore, sumBefore := optionalFileSum(t, goMod), optionalFileSum(t, goSum)

	out, err := e.run(t)
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
	if _, err := os.Lstat(filepath.Join(e.localBin(), "bd")); !os.IsNotExist(err) {
		t.Fatalf("install-bd created %s (%v); it writes nothing but the destination\n%s", filepath.Join(e.localBin(), "bd"), err, out)
	}
	assertNoInstallBdLeftovers(t, e, out)
}

// TestMakeInstallBdIsAnExplicitTarget pins the Makefile wiring: install-bd is
// a phony target of its own, `install` does not run it, and `install` ends by
// noting the bd on PATH.
func TestMakeInstallBdIsAnExplicitTarget(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	text := string(makefile)
	if !strings.Contains(text, "\ninstall-bd:\n\t@scripts/install-bd-pinned.sh \"$(INSTALL_DIR)/bd\"\n") {
		t.Fatal("Makefile install-bd target does not run scripts/install-bd-pinned.sh on $(INSTALL_DIR)/bd")
	}
	if !strings.Contains(text, "\ninstall: check-self-contained\n") {
		t.Fatal("Makefile install target's prerequisites are no longer exactly check-self-contained; it must not build or replace bd")
	}
	if !strings.Contains(text, "\n\t@scripts/install-bd-pinned.sh --note \"$(INSTALL_DIR)/bd\"\n") {
		t.Fatal("Makefile install recipe does not note the bd on PATH")
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

// writeFakeBd writes a bd that the stub go reports as embedding version; an
// empty version writes one the stub reports as carrying no Go build info.
func writeFakeBd(t *testing.T, path, version string) {
	t.Helper()
	if version == "" {
		writeExecutable(t, path, "#!/bin/sh\necho 'a wrapper, not a Go binary'\n")
		return
	}
	writeExecutable(t, path, "#!/bin/sh\n# beads-version: "+version+"\n")
}

// TestInstallBdPinnedFastPath: a bd that already embeds the pin is left
// alone. Content, not mtime, is compared: mtime is too coarse on some
// filesystems to prove the file was not rewritten.
func TestInstallBdPinnedFastPath(t *testing.T) {
	e := newInstallBdEnv(t)
	writeFakeBd(t, e.dest(), installBdPin)
	before := fileSum(t, e.dest())

	out, err := e.run(t)
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

// TestInstallBdPinnedLeavesLocalBinBd: install-bd never writes
// ~/.local/bin/bd, the location beads' own installer uses. On the fast path
// and on the build path alike, a bd there that shadows the destination on
// PATH is named with its version and left byte for byte as it was, and none
// is created where there was none.
func TestInstallBdPinnedLeavesLocalBinBd(t *testing.T) {
	for _, tc := range []struct {
		name string
		// destVersion is the bd already at the destination ("" for none).
		destVersion string
		// local sets up ~/.local/bin/bd and returns what it must still be
		// afterwards ("" when it must still be absent).
		local func(t *testing.T, e *installBdEnv) string
	}{
		{
			name:        "fast path, regular file",
			destVersion: installBdPin,
			local:       localBinRegularBd,
		},
		{
			name:        "build path, regular file",
			destVersion: installBdSkewed,
			local:       localBinRegularBd,
		},
		{
			name:        "build path, symlink to another bd",
			destVersion: installBdSkewed,
			local: func(t *testing.T, e *installBdEnv) string {
				other := filepath.Join(t.TempDir(), "bd")
				writeFakeBd(t, other, installBdSkewed)
				if err := os.Symlink(other, filepath.Join(e.localBin(), "bd")); err != nil {
					t.Fatal(err)
				}
				return "symlink to " + other
			},
		},
		{
			name:        "build path, none",
			destVersion: "",
			local:       func(*testing.T, *installBdEnv) string { return "" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newInstallBdEnv(t)
			if tc.destVersion != "" {
				writeFakeBd(t, e.dest(), tc.destVersion)
			}
			want := tc.local(t, e)
			e.setenv("PATH=" + e.pathWith(e.localBin(), e.installDir))

			out, err := e.run(t)
			if err != nil {
				t.Fatalf("make install-bd: %v\n%s", err, out)
			}
			wantInstalls := 1
			if tc.destVersion == installBdPin {
				wantInstalls = 0
			}
			if installs := e.goInstalls(t); len(installs) != wantInstalls {
				t.Fatalf("go install calls = %q, want %d\n%s", installs, wantInstalls, out)
			}
			if got := embeddedVersion(t, e, e.dest()); got != installBdPin {
				t.Fatalf("destination bd embeds beads %s, want %s\n%s", got, installBdPin, out)
			}
			local := filepath.Join(e.localBin(), "bd")
			if got := describeLocalBinBd(t, local); got != want {
				t.Fatalf("%s is %q after install-bd, want it left as %q\n%s", local, got, want, out)
			}
			if want != "" && (!strings.Contains(out, "NOTE: bd on PATH is "+local) || !strings.Contains(out, installBdSkewed)) {
				t.Fatalf("output does not name the shadowing %s and its beads %s:\n%s", local, installBdSkewed, out)
			}
		})
	}
}

// localBinRegularBd writes an older regular-file bd at ~/.local/bin/bd and
// returns its description.
func localBinRegularBd(t *testing.T, e *installBdEnv) string {
	t.Helper()
	local := filepath.Join(e.localBin(), "bd")
	writeFakeBd(t, local, installBdSkewed)
	return "file " + fileSum(t, local)
}

// describeLocalBinBd says what is at path: "" when nothing, else a regular
// file's content hash or a symlink's target.
func describeLocalBinBd(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			t.Fatal(err)
		}
		return "symlink to " + target
	}
	return "file " + fileSum(t, path)
}

// installBdSkewed is a beads version older than the pin.
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

// TestInstallBdPinnedReplacesAnOlderBd: a bd older than the pin is replaced
// by a rename, never rewritten in place, so a process running the old binary
// (or a hard link to it) keeps a complete file.
func TestInstallBdPinnedReplacesAnOlderBd(t *testing.T) {
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

	out, err := e.run(t)
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

// TestInstallBdPinnedOnlyUpgrades: install-bd replaces a bd only when it is
// older than the pin by semver precedence (a prerelease or pseudo-version of
// the pinned release is older; a pseudo-version past it is newer). A newer bd
// is kept: an older bd cannot read a store the newer one migrated.
func TestInstallBdPinnedOnlyUpgrades(t *testing.T) {
	for _, tc := range []struct {
		version  string
		replaced bool
	}{
		{"v1.3.3", true},
		{"v1.3.4-rc.1", true},
		{"v1.3.4-0.20261001000000-abcdef123456", true},
		{"v0.9.0", true},
		{"v1.3.5", false},
		{"v1.3.10", false},
		{"v1.4.0-rc.2", false},
		{"v1.3.5-0.20261101120000-abcdef123456", false},
		{"v2.0.0", false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			e := newInstallBdEnv(t)
			writeFakeBd(t, e.dest(), tc.version)
			before := fileSum(t, e.dest())

			out, err := e.run(t)
			if err != nil {
				t.Fatalf("make install-bd: %v\n%s", err, out)
			}
			got := embeddedVersion(t, e, e.dest())
			if tc.replaced {
				if got != installBdPin || len(e.goInstalls(t)) != 1 {
					t.Fatalf("bd at %s: destination embeds %s after %d build(s), want it replaced by %s\n%s", tc.version, got, len(e.goInstalls(t)), installBdPin, out)
				}
				return
			}
			if fileSum(t, e.dest()) != before || len(e.goInstalls(t)) != 0 {
				t.Fatalf("bd at %s, newer than the pin, was rebuilt or rewritten\n%s", tc.version, out)
			}
			if !strings.Contains(out, "newer than the "+installBdPin) || !strings.Contains(out, tc.version) {
				t.Fatalf("output does not say %s is newer than the pin %s and kept:\n%s", tc.version, installBdPin, out)
			}
		})
	}
}

// TestInstallBdPinnedRefusesABdItCannotRead: a bd at the destination whose
// beads version cannot be compared (not a Go binary, or a modified source
// build) is neither built over nor rewritten; install-bd fails and says how
// to get the pinned bd.
func TestInstallBdPinnedRefusesABdItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
	}{
		{"not a Go binary", ""},
		{"modified source build", "v1.3.4+dirty"},
		{"modified pseudo-version build", "v0.0.0-20261107052026-05ebdd78fd3c+dirty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newInstallBdEnv(t)
			writeFakeBd(t, e.dest(), tc.version)
			before := fileSum(t, e.dest())

			out, err := e.run(t)
			if err == nil {
				t.Fatalf("make install-bd succeeded over a bd it cannot read:\n%s", out)
			}
			if fileSum(t, e.dest()) != before || len(e.goInstalls(t)) != 0 {
				t.Fatalf("install-bd rebuilt or rewrote a bd it cannot read\n%s", out)
			}
			for _, want := range []string{e.dest(), "Move it aside", installBdPin} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			assertNoInstallBdLeftovers(t, e, out)
		})
	}
}

// TestInstallBdPinnedFailsClosed: when the pinned bd cannot be built, or the
// build does not embed the pin, install-bd fails and the installed bd is the
// old one, byte for byte.
func TestInstallBdPinnedFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     []string
		wantOut []string
	}{
		{
			name:    "build fails",
			env:     []string{"STUB_INSTALL_FAIL=1"},
			wantOut: []string{"C toolchain", installBdPin},
		},
		{
			name:    "staged bd embeds another version",
			env:     []string{"STUB_STAGED_VERSION=v1.1.0"},
			wantOut: []string{"v1.1.0", installBdPin},
		},
		{
			name:    "staged bd has no readable build info",
			env:     []string{"STUB_STAGED_VERSION="},
			wantOut: []string{"embeds beads 'unknown'", installBdPin},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newInstallBdEnv(t)
			e.setenv(tc.env...)
			writeFakeBd(t, e.dest(), installBdSkewed)
			before := fileSum(t, e.dest())

			out, err := e.run(t)
			if err == nil {
				t.Fatalf("make install-bd succeeded:\n%s", out)
			}
			if fileSum(t, e.dest()) != before {
				t.Fatalf("the installed bd changed after a failed install-bd\n%s", out)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			assertNoInstallBdLeftovers(t, e, out)
		})
	}
}

// TestInstallBdPinnedRefusesReplacedBeads: gc links a go.mod replacement of
// beads, not the pin, so no bd built from the pin matches it.
func TestInstallBdPinnedRefusesReplacedBeads(t *testing.T) {
	e := newInstallBdEnv(t)
	e.setenv(`STUB_REPLACE_JSON=,"Replace":[{"Old":{"Path":"github.com/steveyegge/beads"},"New":{"Path":"../beads"}}]`)
	out, err := e.run(t)
	if err == nil {
		t.Fatalf("make install-bd succeeded with beads replaced:\n%s", out)
	}
	if !strings.Contains(out, "replaces github.com/steveyegge/beads") {
		t.Fatalf("output does not name the replace directive:\n%s", out)
	}
	if installs := e.goInstalls(t); len(installs) != 0 {
		t.Fatalf("go install ran with beads replaced: %q", installs)
	}
	if _, err := os.Stat(e.dest()); !os.IsNotExist(err) {
		t.Fatalf("a bd was installed with beads replaced (%v)", err)
	}
}

// TestInstallBdPinnedNotesShadowingPathBd: when the bd PATH resolves is not
// the installed one, install-bd names both and leaves the other alone.
func TestInstallBdPinnedNotesShadowingPathBd(t *testing.T) {
	e := newInstallBdEnv(t)
	shadowDir := t.TempDir()
	shadow := filepath.Join(shadowDir, "bd")
	writeFakeBd(t, shadow, installBdSkewed)
	before := fileSum(t, shadow)
	e.setenv("PATH=" + e.pathWith(shadowDir))

	out, err := e.run(t)
	if err != nil {
		t.Fatalf("make install-bd: %v\n%s", err, out)
	}
	for _, want := range []string{shadow, installBdSkewed, e.dest()} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not name %q (the shadowing bd, its version, the installed bd):\n%s", want, out)
		}
	}
	if fileSum(t, shadow) != before {
		t.Fatal("install-bd modified the shadowing bd")
	}
}

// runMakeInstall runs `make install` with the gc build prerequisite dropped,
// installing a fake gc and companion from a build dir into the install dir.
func (e *installBdEnv) runMakeInstall(t *testing.T) (string, error) {
	t.Helper()
	tmp := t.TempDir()
	buildDir := filepath.Join(tmp, "build")
	if err := os.Mkdir(buildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, bin := range []string{"gc", "gc-osc7501"} {
		writeExecutable(t, filepath.Join(buildDir, bin), "new "+bin)
	}
	makefile, err := os.ReadFile(filepath.Join(e.repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	const rule = "\ninstall: check-self-contained\n"
	if !strings.Contains(string(makefile), rule) {
		t.Fatal("Makefile install target no longer depends on check-self-contained alone")
	}
	testMakefile := filepath.Join(tmp, "Makefile")
	if err := os.WriteFile(testMakefile, []byte(strings.Replace(string(makefile), rule, "\ninstall:\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("make", "--no-print-directory", "-f", testMakefile, "install",
		"BUILD_DIR="+buildDir,
		"INSTALL_DIR="+e.installDir,
	)
	cmd.Dir = e.repoRoot
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	if err == nil {
		for _, bin := range []string{"gc", "gc-osc7501"} {
			if got, readErr := os.ReadFile(filepath.Join(e.installDir, bin)); readErr != nil || string(got) != "new "+bin {
				t.Fatalf("make install left %s = %q (%v), want the new build\n%s", bin, got, readErr, out)
			}
		}
	}
	return string(out), err
}

// TestMakeInstallNotesButNeverBuildsBd: `make install` installs gc and never
// builds or replaces a bd. It says so when PATH resolves no bd or one older
// than the pin, and is quiet about a bd at, or newer than, the pin and about
// one whose beads version it cannot read.
func TestMakeInstallNotesButNeverBuildsBd(t *testing.T) {
	for _, tc := range []struct {
		name string
		// pathBd is the beads version of the bd on PATH, or "none".
		pathBd   string
		wantNote string
	}{
		{name: "no bd on PATH", pathBd: "none", wantNote: "NOTE: no bd on PATH"},
		{name: "older bd", pathBd: installBdSkewed, wantNote: "NOTE: bd on PATH"},
		{name: "pinned bd", pathBd: installBdPin},
		{name: "newer bd", pathBd: "v1.4.0-rc.2"},
		{name: "unreadable bd", pathBd: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newInstallBdEnv(t)
			if tc.pathBd != "none" {
				writeFakeBd(t, e.dest(), tc.pathBd)
				e.setenv("PATH=" + e.pathWith(e.installDir))
			}
			bdBefore := optionalFileSum(t, e.dest())

			out, err := e.runMakeInstall(t)
			if err != nil {
				t.Fatalf("make install: %v\n%s", err, out)
			}
			if installs := e.goInstalls(t); len(installs) != 0 {
				t.Fatalf("make install built bd: %q\n%s", installs, out)
			}
			if optionalFileSum(t, e.dest()) != bdBefore {
				t.Fatalf("make install changed %s\n%s", e.dest(), out)
			}
			if tc.wantNote == "" {
				if strings.Contains(out, "NOTE:") {
					t.Fatalf("make install noted a bd that needs nothing:\n%s", out)
				}
				return
			}
			for _, want := range []string{tc.wantNote, installBdPin, "make install-bd"} {
				if !strings.Contains(out, want) {
					t.Fatalf("make install output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestMakeInstallSucceedsWhenGoModReplacesBeads: with beads replaced there is
// no pin to compare a bd with; `make install` still succeeds and says why it
// did not check bd.
func TestMakeInstallSucceedsWhenGoModReplacesBeads(t *testing.T) {
	e := newInstallBdEnv(t)
	e.setenv(`STUB_REPLACE_JSON=,"Replace":[{"Old":{"Path":"github.com/steveyegge/beads"},"New":{"Path":"../beads"}}]`)
	out, err := e.runMakeInstall(t)
	if err != nil {
		t.Fatalf("make install failed with beads replaced: %v\n%s", err, out)
	}
	if !strings.Contains(out, "NOTE: bd not checked") || !strings.Contains(out, "replaces github.com/steveyegge/beads") {
		t.Fatalf("make install output does not say why bd was not checked:\n%s", out)
	}
}
