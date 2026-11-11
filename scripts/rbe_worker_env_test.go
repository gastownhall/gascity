package scripts_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// Test actions exec host tools through the client's PATH, and the host runs
// the hermetic C toolchain and loads the shared libraries of every binary it
// links (libstdc++, ICU), so the worker host is an input to every
// result rbe-west caches. These tests pin gascity's half of the worker-env
// platform property that puts the host into the action key:
//
//   - every build (CI trusted, rbe-fork and fork-cache runs alike) executes on
//     //platforms:rbe_worker, whose worker-env is the sha256 of the committed
//     manifest tools/rbe/worker-env.txt;
//   - that manifest has the shape the worker's measurement prints, and names
//     the Go of go.mod.
//
// The worker half (worker/worker-env, which measures a host, and
// worker/blacksmith-worker.sh, which advertises the sha256 of what it
// measured) lives in gastownhall/rbe-worker at .github/actions/rbe-worker/pin,
// with its own tests. The contract between the two (the manifest's package
// set is worker-env's measured list, its dolt line is the worker's
// DOLT_VERSION, the .bazelrc test PATH is the one worker-env measures) is
// rbe-worker's cmd/product-check. The worker-host job (bazel.yml) runs the
// pinned commit's product-check against this checkout on every PR, and
// measures a Blacksmith host against its manifest whenever the worker host
// or the rbe-worker pin changes. rbe-worker's own CI also runs
// product-check against gascity main, but only as information; it gates
// nothing here.
//
// The manifest is the host's toolchain, not its image: arch, OS release, Go,
// dolt, and the upstream releases of the libraries and tools actions reach,
// cut to the components that carry ABI or behavior. A toolchain change (a
// glibc minor, another arch, a tool's major) serves no action until the
// manifest and pin move, and the new pin is a new key for every action. A
// security patch of the same releases (the Blacksmith image's routine Ubuntu
// updates) measures the same, so it neither re-keys nor strands the pool.

const (
	rbeWorkerPlatformBuild = "platforms/BUILD.bazel"
	rbeWorkerPlatformLabel = "//platforms:rbe_worker"
	rbeWorkerEnvManifest   = "tools/rbe/worker-env.txt"
	rbeWorkerEnvProperty   = "worker-env"
	// The PATH rbe-worker's worker/worker-env measures, and so the one CI's
	// tests must run with (cmd/product-check checks rbe-worker's side).
	rbeWorkerTestPath = "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"
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

// TestRBEWorkerEnvManifestNamesTheWorkerHost: the committed manifest is a
// worker-env rendering (sorted; one arch, dolt, go, os and yq line; every
// package at an upstream release, never a Debian revision) of a host with
// the Go of go.mod, so bumping Go without a new manifest and pin fails here
// rather than on the farm. Which packages it names, and the dolt it names,
// are the pinned rbe-worker's (cmd/product-check).
func TestRBEWorkerEnvManifestNamesTheWorkerHost(t *testing.T) {
	root := repoRoot(t)
	manifest := readFile(t, root, rbeWorkerEnvManifest)
	if !strings.HasSuffix(manifest, "\n") {
		t.Fatalf("%s must end with a newline, as worker-env prints it", rbeWorkerEnvManifest)
	}
	lines := strings.Split(strings.TrimSuffix(manifest, "\n"), "\n")
	if !slices.IsSorted(lines) {
		t.Errorf("%s is not sorted (LC_ALL=C), as worker-env prints it", rbeWorkerEnvManifest)
	}
	goVersion := regexp.MustCompile(`(?m)^go (\S+)$`).FindStringSubmatch(readFile(t, root, "go.mod"))
	if goVersion == nil {
		t.Fatal("go.mod has no go line")
	}
	single := map[string]*regexp.Regexp{
		"arch": regexp.MustCompile(`^x86_64$`), // the workers' ISA platform property
		"dolt": regexp.MustCompile(`^dolt version \d+\.\d+\.\d+$`),
		"go":   regexp.MustCompile(`^go version go` + regexp.QuoteMeta(goVersion[1]) + ` linux/amd64$`),
		"os":   regexp.MustCompile(`^ubuntu \d+\.\d+$`),
		"tool": regexp.MustCompile(`^yq \d+$`),
	}
	pkg := regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]* \d+(\.\d+)*$`)
	seen := map[string]int{}
	for _, line := range lines {
		kind, value, _ := strings.Cut(line, " ")
		seen[kind]++
		if kind == "pkg" {
			if !pkg.MatchString(value) {
				t.Errorf("%s: package %q is not a name and its upstream release (a Debian revision in the pin re-keys every action on the next security update)",
					rbeWorkerEnvManifest, value)
			}
			continue
		}
		re, ok := single[kind]
		if !ok {
			t.Errorf("%s: unexpected line %q", rbeWorkerEnvManifest, line)
			continue
		}
		if !re.MatchString(value) {
			t.Errorf("%s: %s %q, want %s", rbeWorkerEnvManifest, kind, value, re)
		}
	}
	for kind := range single {
		if seen[kind] != 1 {
			t.Errorf("%s has %d %s lines, want 1", rbeWorkerEnvManifest, seen[kind], kind)
		}
	}
	if seen["pkg"] == 0 {
		t.Errorf("%s names no package", rbeWorkerEnvManifest)
	}
}

// runRBEScript runs a script with bash in dir (the test's cwd if
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
	if testPath != rbeWorkerTestPath {
		t.Errorf("CI tests' PATH %q, worker-env measures %q: they must be the same", testPath, rbeWorkerTestPath)
	}
}
