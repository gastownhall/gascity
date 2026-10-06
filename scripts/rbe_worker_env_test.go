package scripts_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// Test actions exec host tools through the client's PATH, and the host runs
// the hermetic C toolchain and loads the shared libraries of every binary it
// links (libstdc++, ICU), so the worker host is an input to every
// result rbe-west caches. These tests pin the worker-env platform property
// that puts the host into the action key:
//
//   - every build (CI trusted, rbe-fork and fork-cache runs alike) executes on
//     //platforms:rbe_worker, whose worker-env is the sha256 of the committed
//     manifest tools/rbe/worker-env.txt;
//   - that manifest is what tools/rbe/worker-env prints on the pinned host,
//     and names the Go and dolt the worker installs;
//   - blacksmith-worker.sh measures its own host with tools/rbe/worker-env
//     and advertises the sha256 of what it measured, which rbe-west's
//     schedulers match exactly against the action's.
//
// A host change (new runner image, package or Go upgrade) therefore serves no
// action until the manifest and pin move, and the new pin is a new key for
// every action: no result from the old host is ever a hit for the new one.

const (
	rbeWorkerPlatformBuild = "platforms/BUILD.bazel"
	rbeWorkerPlatformLabel = "//platforms:rbe_worker"
	rbeWorkerEnvManifest   = "tools/rbe/worker-env.txt"
	rbeWorkerEnvScript     = "tools/rbe/worker-env"
	rbeWorkerEnvProperty   = "worker-env"
	// What the golden worker.json renderings advertise.
	rbeWorkerEnvSample = "sha256:5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"
)

var workerEnvPinRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// rbeWorkerPlatformExecProperties returns the exec_properties of the
// rbe_worker platform in platforms/BUILD.bazel.
func rbeWorkerPlatformExecProperties(t *testing.T, build string) map[string]string {
	t.Helper()
	m := regexp.MustCompile(`(?s)\nplatform\(\n    name = "rbe_worker",\n(.*?)\n\)\n`).FindStringSubmatch(build)
	if m == nil {
		t.Fatalf("%s: no platform rbe_worker", rbeWorkerPlatformBuild)
	}
	props := regexp.MustCompile(`(?s)exec_properties = \{\n(.*?)\n    \},`).FindStringSubmatch(m[1])
	if props == nil {
		t.Fatalf("%s: platform rbe_worker has no exec_properties", rbeWorkerPlatformBuild)
	}
	out := map[string]string{}
	entry := regexp.MustCompile(`^\s*"([^"]+)": "([^"]*)",$`)
	for _, line := range strings.Split(props[1], "\n") {
		e := entry.FindStringSubmatch(line)
		if e == nil {
			t.Fatalf("%s: unexpected exec_properties line %q", rbeWorkerPlatformBuild, line)
		}
		out[e[1]] = e[2]
	}
	return out
}

// TestRBEWorkerPlatformPinsWorkerEnv: the platform's worker-env is the
// sha256 of the committed manifest, and worker-env is all it adds to the
// key (anything else would be a property no worker advertises).
func TestRBEWorkerPlatformPinsWorkerEnv(t *testing.T) {
	root := repoRoot(t)
	props := rbeWorkerPlatformExecProperties(t, readFile(t, root, rbeWorkerPlatformBuild))
	pin := props[rbeWorkerEnvProperty]
	if len(props) != 1 || !workerEnvPinRE.MatchString(pin) {
		t.Fatalf("rbe_worker exec_properties = %v; want %s=sha256:<hex> alone", props, rbeWorkerEnvProperty)
	}
	sum := sha256.Sum256([]byte(readFile(t, root, rbeWorkerEnvManifest)))
	if want := "sha256:" + hex.EncodeToString(sum[:]); pin != want {
		t.Errorf("%s pins %s=%s, but %s hashes to %s: commit the manifest and its sha256 together",
			rbeWorkerPlatformBuild, rbeWorkerEnvProperty, pin, rbeWorkerEnvManifest, want)
	}
}

// workerToolset returns blacksmith-worker.sh's WORKER_TOOLSET packages.
func workerToolset(t *testing.T, script string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?s)\nWORKER_TOOLSET=\(([^)]*)\)\n`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("%s: no WORKER_TOOLSET=(...)", rbeWorkerScript)
	}
	return strings.Fields(m[1])
}

// TestRBEWorkerEnvManifestNamesTheWorkerHost: the committed manifest is a
// tools/rbe/worker-env rendering (sorted, one arch, dolt, go and os line,
// every base and toolset package installed) of a host with the Go of go.mod
// and the dolt blacksmith-worker.sh installs, so bumping either without a
// new manifest and pin fails here rather than on the farm.
func TestRBEWorkerEnvManifestNamesTheWorkerHost(t *testing.T) {
	root := repoRoot(t)
	manifest := readFile(t, root, rbeWorkerEnvManifest)
	script := readFile(t, root, rbeWorkerScript)
	envScript := readFile(t, root, rbeWorkerEnvScript)
	if !strings.HasSuffix(manifest, "\n") {
		t.Fatalf("%s must end with a newline, as tools/rbe/worker-env prints it", rbeWorkerEnvManifest)
	}
	lines := strings.Split(strings.TrimSuffix(manifest, "\n"), "\n")
	if !slices.IsSorted(lines) {
		t.Errorf("%s is not sorted (LC_ALL=C), as tools/rbe/worker-env prints it", rbeWorkerEnvManifest)
	}
	goVersion := regexp.MustCompile(`(?m)^go (\S+)$`).FindStringSubmatch(readFile(t, root, "go.mod"))
	dolt := regexp.MustCompile(`(?m)^DOLT_VERSION=(\S+)$`).FindStringSubmatch(script)
	if goVersion == nil || dolt == nil {
		t.Fatalf("go.mod go line %v, %s DOLT_VERSION %v", goVersion, rbeWorkerScript, dolt)
	}
	single := map[string]string{
		"arch": "x86_64", // the workers' ISA platform property
		"dolt": "dolt version " + dolt[1],
		"go":   "go version go" + goVersion[1] + " linux/amd64",
	}
	base := regexp.MustCompile(`(?m)^base=\(([^)]*)\)$`).FindStringSubmatch(envScript)
	if base == nil {
		t.Fatalf("%s: no base=(...)", rbeWorkerEnvScript)
	}
	wantPkgs := append(strings.Fields(base[1]), workerToolset(t, script)...)
	sort.Strings(wantPkgs)
	wantPkgs = slices.Compact(wantPkgs)

	seen := map[string]int{}
	var pkgs []string
	for _, line := range lines {
		kind, value, _ := strings.Cut(line, " ")
		seen[kind]++
		switch kind {
		case "arch", "dolt", "go":
			if value != single[kind] {
				t.Errorf("%s: %s %q, want %q", rbeWorkerEnvManifest, kind, value, single[kind])
			}
		case "os":
			if !regexp.MustCompile(`^ubuntu \d+\.\d+$`).MatchString(value) {
				t.Errorf("%s: os %q", rbeWorkerEnvManifest, value)
			}
		case "pkg":
			name, version, _ := strings.Cut(value, " ")
			if version == "" || version == "missing" {
				t.Errorf("%s: package %s is not installed on the pinned host", rbeWorkerEnvManifest, name)
			}
			pkgs = append(pkgs, name)
		default:
			t.Errorf("%s: unexpected line %q", rbeWorkerEnvManifest, line)
		}
	}
	for _, kind := range []string{"arch", "dolt", "go", "os"} {
		if seen[kind] != 1 {
			t.Errorf("%s has %d %s lines, want 1", rbeWorkerEnvManifest, seen[kind], kind)
		}
	}
	if !slices.Equal(pkgs, wantPkgs) {
		t.Errorf("%s packages:\n%v\nwant the worker-env base and WORKER_TOOLSET packages:\n%v", rbeWorkerEnvManifest, pkgs, wantPkgs)
	}
}

// TestRBEWorkerToolsetRunsTheHermeticToolchain: C/C++ and cgo actions build
// with the hermetic LLVM toolchain and sysroot of MODULE.bazel, so the
// toolset (and with it the worker-env manifest) carries what the host needs
// to run that toolchain and the binaries it links, and no host compiler,
// linker or ICU headers: a package no action uses would only re-key every
// action when the runner image moves it.
func TestRBEWorkerToolsetRunsTheHermeticToolchain(t *testing.T) {
	toolset := workerToolset(t, readFile(t, repoRoot(t), rbeWorkerScript))
	for _, want := range []string{
		"libstdc++6", "libgcc-s1", "zlib1g", // clang, lld, llvm-*
		"libxml2", "liblzma5", // lld
		"libicu74", // Bazel-built binaries linking go-icu-regex
		"xz-utils", // the toolchain's .tar.xz
	} {
		if !slices.Contains(toolset, want) {
			t.Errorf("%s WORKER_TOOLSET lacks %s, which the hermetic toolchain or its binaries load", rbeWorkerScript, want)
		}
	}
	for _, banned := range []string{"gcc", "g++", "clang", "lld", "libc6-dev", "libicu-dev", "build-essential"} {
		if slices.Contains(toolset, banned) {
			t.Errorf("%s WORKER_TOOLSET has %s; no action uses a host compiler, linker or ICU headers", rbeWorkerScript, banned)
		}
	}
}

// TestRBEWorkerEnvScript runs tools/rbe/worker-env against stub tools: one
// sorted line per fact, packages deduplicated and reported per installed
// architecture once, a removed or unknown package "missing", go asked with
// GOTOOLCHAIN=local away from any go.mod, and a missing tool fatal.
func TestRBEWorkerEnvScript(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	stubs := filepath.Join(dir, "bin")
	if err := os.MkdirAll(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"uname": "echo x86_64\n",
		"dolt":  "echo 'dolt version 9.9.9'\necho 'database storage format: NEW'\n",
		"go": `[ "$GOTOOLCHAIN" = local ] || { echo "GOTOOLCHAIN=$GOTOOLCHAIN" >&2; exit 1; }
[ "$(pwd)" = / ] || { echo "cwd $(pwd)" >&2; exit 1; }
echo 'go version go1.99.1 linux/amd64'
`,
		"dpkg-query": `for p; do :; done
case "$p" in
libc6) printf 'installed 2.39-0ubuntu8\ninstalled 2.39-0ubuntu8\n' ;;
libfoo) printf 'installed 1.2:i386-only\ninstalled 1.3\n' ;;
tmux) printf 'deinstall 3.4-1\n' ;;
nosuch) echo "dpkg-query: no packages found matching $p" >&2; exit 1 ;;
*) printf 'installed 1.0-%s\n' "$p" ;;
esac
`,
	} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	osRelease := filepath.Join(dir, "os-release")
	if err := os.WriteFile(osRelease, []byte("NAME=\"Ubuntu\"\nID=ubuntu\nVERSION_ID=\"24.04\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(path string, args ...string) (string, error) {
		out, stderr, err := runRBEScript("", []string{"WORKER_ENV_PATH=" + path, "WORKER_ENV_OS_RELEASE=" + osRelease, "GOTOOLCHAIN=auto"},
			filepath.Join(root, rbeWorkerEnvScript), args...)
		if err != nil {
			return out + stderr, err
		}
		return out, nil
	}
	path := stubs + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin"
	got, err := run(path, "tmux", "nosuch", "libfoo", "bash", "zlib1g-dev")
	if err != nil {
		t.Fatalf("worker-env: %v\n%s", err, got)
	}
	want := `arch x86_64
dolt dolt version 9.9.9
go go version go1.99.1 linux/amd64
os ubuntu 24.04
pkg bash 1.0-bash
pkg coreutils 1.0-coreutils
pkg diffutils 1.0-diffutils
pkg findutils 1.0-findutils
pkg grep 1.0-grep
pkg libc6 2.39-0ubuntu8
pkg libfoo 1.2:i386-only,1.3
pkg nosuch missing
pkg procps 1.0-procps
pkg sed 1.0-sed
pkg tar 1.0-tar
pkg tmux missing
pkg util-linux 1.0-util-linux
pkg zlib1g-dev 1.0-zlib1g-dev
`
	if got != want {
		t.Errorf("worker-env output:\n%s\nwant:\n%s", got, want)
	}

	// No dolt (or go) on the PATH: fatal, never a manifest without it.
	noDolt := filepath.Join(dir, "nodolt")
	if err := os.MkdirAll(noDolt, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"uname", "go", "dpkg-query"} {
		if err := os.Symlink(filepath.Join(stubs, name), filepath.Join(noDolt, name)); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := run(noDolt+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin", "tmux"); err == nil {
		t.Errorf("worker-env without dolt succeeded:\n%s", out)
	}
}

// runRBEScript runs a tools/rbe script with bash in dir (the test's cwd if
// empty) and exactly env, and returns its stdout and stderr.
func runRBEScript(dir string, env []string, script string, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{script}, args...)...)
	cmd.Dir = dir
	cmd.Env = env
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// TestRBEWorkerScriptAdvertisesWorkerEnv: blacksmith-worker.sh installs its
// toolset, measures the host with tools/rbe/worker-env over that same
// toolset, and advertises the sha256 of the measurement (never the pin)
// before any worker.json is rendered.
func TestRBEWorkerScriptAdvertisesWorkerEnv(t *testing.T) {
	script := readFile(t, repoRoot(t), rbeWorkerScript)
	at := 0
	for _, want := range []string{
		"\nWORKER_TOOLSET=(",
		"apt-get install -y -qq \\\n\t\"${WORKER_TOOLSET[@]}\" >/dev/null\n",
		"\tsudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf \"$RUNNER_TEMP/go.tgz\"\n",
		"\tsudo cp -f \"$RUNNER_TEMP/dolt-linux-amd64/bin/dolt\" /usr/local/bin/dolt\n",
		"\ntools/rbe/worker-env \"${WORKER_TOOLSET[@]}\" >\"$RUNNER_TEMP/worker-env.txt\"\n",
		"WORKER_ENV=sha256:$(sha256sum <\"$RUNNER_TEMP/worker-env.txt\" | cut -d' ' -f1)\n",
		"\nrender() {\n",
		`--arg worker_env "$WORKER_ENV"`,
		`"worker-env": { values: [$worker_env] }`,
	} {
		i := strings.Index(script[at:], want)
		if i < 0 {
			t.Fatalf("%s: %q missing or out of order", rbeWorkerScript, want)
		}
		at += i + len(want)
	}
	if n := strings.Count(script, "WORKER_ENV="); n != 1 {
		t.Errorf("%s assigns WORKER_ENV %d times; the measurement is its only source", rbeWorkerScript, n)
	}
	if n := strings.Count(script, "apt-get install"); n != 2 {
		t.Errorf("%s has %d apt-get installs, want the toolset's and the isolation phase's", rbeWorkerScript, n)
	}
	// The image's apt lists carry newer candidates than some installed
	// packages (installing libc6-dev upgrades libc6), so an install after
	// the measurement could move a measured package on a host that already
	// advertised its hash. Every later install adds packages only.
	measured := strings.Index(script, "\ntools/rbe/worker-env ")
	for _, m := range regexp.MustCompile(`apt-get install[^\n]*`).FindAllStringIndex(script, -1) {
		if m[0] > measured && !strings.Contains(script[m[0]:m[1]], " --no-upgrade") {
			t.Errorf("%s: %q runs after the worker-env measurement without --no-upgrade", rbeWorkerScript, script[m[0]:m[1]])
		}
	}
}

// checkWorkerJSONAdvertises checks a rendered worker.json advertises
// worker-env=want and nothing else of it.
func checkWorkerJSONAdvertises(out []byte, want string) error {
	var cfg struct {
		Workers []struct {
			Local struct {
				PlatformProperties map[string]struct {
					Values []string `json:"values"`
				} `json:"platform_properties"`
			} `json:"local"`
		} `json:"workers"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return err
	}
	if len(cfg.Workers) != 1 {
		return errors.New("want exactly one worker")
	}
	got := cfg.Workers[0].Local.PlatformProperties[rbeWorkerEnvProperty].Values
	if !slices.Equal(got, []string{want}) {
		return errors.New("worker-env values " + strings.Join(got, ",") + ", want " + want)
	}
	return nil
}

// platformFlag reports whether a .bazelrc option selects or alters the
// execution or target platform, or adds exec properties: all key-affecting.
func platformFlag(flag string) bool {
	name, _, _ := strings.Cut(flag, "=")
	return strings.Contains(name, "platforms") || strings.Contains(name, "host_platform") || strings.Contains(name, "exec_properties")
}

// TestBazelExecutesOnWorkerPlatform: .bazelrc selects //platforms:rbe_worker
// unconditionally (a key-affecting flag under remote-exec or fork-cache, or
// in a CI-written rc, would key those runs apart) and nothing
// else touches platforms or exec properties, and the PATH CI's tests run
// with is the one worker-env measures.
func TestBazelExecutesOnWorkerPlatform(t *testing.T) {
	root := repoRoot(t)
	want := "build --extra_execution_platforms=" + rbeWorkerPlatformLabel
	var platforms []string
	testPath := ""
	for _, line := range strings.Split(readFile(t, root, ".bazelrc"), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		for _, flag := range fields[1:] {
			if platformFlag(flag) {
				platforms = append(platforms, fields[0]+" "+flag)
			}
			if v, ok := strings.CutPrefix(flag, "--test_env=PATH="); ok && fields[0] == "test" {
				testPath = v
			}
		}
	}
	if !slices.Equal(platforms, []string{want}) {
		t.Errorf(".bazelrc platform flags %q, want %q alone", platforms, want)
	}

	// bazel.yml's lanes: setup-bazel's generated rc stays off platforms too
	// (.bazelrc's build:ci lines are checked above with every other config).
	for _, line := range strings.Split(readFile(t, root, ".github/actions/setup-bazel/write-bazelrc.sh"), "\n") {
		for _, flag := range strings.Fields(line) {
			if strings.HasPrefix(flag, "--") && platformFlag(flag) {
				t.Errorf("setup-bazel's write-bazelrc.sh writes %q; platform flags are key-affecting and belong in .bazelrc", line)
			}
		}
	}
	m := regexp.MustCompile(`(?m)^PATH=\$\{WORKER_ENV_PATH:-([^}]*)\}$`).FindStringSubmatch(readFile(t, root, rbeWorkerEnvScript))
	if m == nil || testPath == "" || m[1] != testPath {
		t.Errorf("CI tests' PATH %q, worker-env measures %v: they must be the same", testPath, m)
	}
}
