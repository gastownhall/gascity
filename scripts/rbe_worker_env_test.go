package scripts_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
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

// workerEnvList returns the words of tools/rbe/worker-env's NAME=( ... )
// array, comments dropped.
func workerEnvList(t *testing.T, envScript, name string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?ms)^` + name + `=\(\n?(.*?)\)$`).FindStringSubmatch(envScript)
	if m == nil {
		t.Fatalf("%s: no %s=(...)", rbeWorkerEnvScript, name)
	}
	var words []string
	for _, line := range strings.Split(m[1], "\n") {
		line, _, _ = strings.Cut(line, "#")
		words = append(words, strings.Fields(line)...)
	}
	return words
}

// workerEnvMeasured returns tools/rbe/worker-env's measured packages and the
// upstream version components each keeps.
func workerEnvMeasured(t *testing.T, envScript string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, w := range workerEnvList(t, envScript, "measured") {
		name, parts, ok := strings.Cut(w, ":")
		if !ok || (parts != "1" && parts != "2") {
			t.Fatalf("%s: measured entry %q is not NAME:1 or NAME:2", rbeWorkerEnvScript, w)
		}
		if _, dup := out[name]; dup {
			t.Fatalf("%s: %s measured twice", rbeWorkerEnvScript, name)
		}
		out[name] = int(parts[0] - '0')
	}
	return out
}

// TestRBEWorkerEnvManifestNamesTheWorkerHost: the committed manifest is a
// tools/rbe/worker-env rendering (sorted; one arch, dolt, go, os and yq
// line; every measured package installed, at its upstream release cut to the
// script's components and nothing finer) of a host with the Go of go.mod and
// the dolt blacksmith-worker.sh installs, so bumping either without a new
// manifest and pin fails here rather than on the farm.
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
	measured := workerEnvMeasured(t, envScript)
	var wantPkgs []string
	for name := range measured {
		wantPkgs = append(wantPkgs, name)
	}
	sort.Strings(wantPkgs)

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
		case "tool":
			if !regexp.MustCompile(`^yq \d+$`).MatchString(value) {
				t.Errorf("%s: tool %q, want yq at its major", rbeWorkerEnvManifest, value)
			}
		case "pkg":
			name, version, _ := strings.Cut(value, " ")
			parts := measured[name]
			if parts == 0 {
				parts = 1
			}
			if !regexp.MustCompile(`^\d+(\.\d+){0,` + strconv.Itoa(parts-1) + `}$`).MatchString(version) {
				t.Errorf("%s: package %s %q is not installed, or not its upstream release cut to %d components (a Debian revision in the pin re-keys every action on the next security update)",
					rbeWorkerEnvManifest, name, version, parts)
			}
			pkgs = append(pkgs, name)
		default:
			t.Errorf("%s: unexpected line %q", rbeWorkerEnvManifest, line)
		}
	}
	for _, kind := range []string{"arch", "dolt", "go", "os", "tool"} {
		if seen[kind] != 1 {
			t.Errorf("%s has %d %s lines, want 1", rbeWorkerEnvManifest, seen[kind], kind)
		}
	}
	if !slices.Equal(pkgs, wantPkgs) {
		t.Errorf("%s packages:\n%v\nwant tools/rbe/worker-env's measured packages:\n%v", rbeWorkerEnvManifest, pkgs, wantPkgs)
	}
}

// TestRBEWorkerEnvAccountsForTheToolset: every WORKER_TOOLSET package is
// either measured or named unmeasured (with its reason, in
// tools/rbe/worker-env), never both, and unmeasured names nothing the
// worker no longer installs. A package added to the toolset is a decision
// about the key, not an accident of it.
func TestRBEWorkerEnvAccountsForTheToolset(t *testing.T) {
	root := repoRoot(t)
	envScript := readFile(t, root, rbeWorkerEnvScript)
	measured := workerEnvMeasured(t, envScript)
	unmeasured := workerEnvList(t, envScript, "unmeasured")
	toolset := workerToolset(t, readFile(t, root, rbeWorkerScript))
	for _, p := range toolset {
		_, m := measured[p]
		u := slices.Contains(unmeasured, p)
		switch {
		case m && u:
			t.Errorf("%s: %s is both measured and unmeasured", rbeWorkerEnvScript, p)
		case !m && !u:
			t.Errorf("%s installs %s, which %s neither measures nor names unmeasured", rbeWorkerScript, p, rbeWorkerEnvScript)
		}
	}
	for _, p := range unmeasured {
		if !slices.Contains(toolset, p) {
			t.Errorf("%s: unmeasured %s is not in %s WORKER_TOOLSET", rbeWorkerEnvScript, p, rbeWorkerScript)
		}
	}
}

// TestRBEWorkerToolsetRunsTheHermeticToolchain: C/C++ and cgo actions build
// with the hermetic LLVM toolchain and sysroot of MODULE.bazel, so the
// toolset carries what the host needs to run that toolchain and the binaries
// it links, the manifest measures it at its ABI (major.minor), and neither
// carries a host compiler, linker or ICU headers: a package no action uses
// would only re-key every action when the runner image moves it.
func TestRBEWorkerToolsetRunsTheHermeticToolchain(t *testing.T) {
	root := repoRoot(t)
	toolset := workerToolset(t, readFile(t, root, rbeWorkerScript))
	measured := workerEnvMeasured(t, readFile(t, root, rbeWorkerEnvScript))
	for _, want := range []string{
		"libstdc++6", "libgcc-s1", "zlib1g", // clang, lld, llvm-*
		"libxml2", "liblzma5", // lld
		"libicu74", // Bazel-built binaries linking go-icu-regex
		"xz-utils", // the toolchain's .tar.xz
	} {
		if !slices.Contains(toolset, want) {
			t.Errorf("%s WORKER_TOOLSET lacks %s, which the hermetic toolchain or its binaries load", rbeWorkerScript, want)
		}
		if measured[want] != 2 {
			t.Errorf("%s measures %s at %d components, want 2 (major.minor)", rbeWorkerEnvScript, want, measured[want])
		}
	}
	if measured["libc6"] != 2 {
		t.Errorf("%s measures libc6 at %d components, want 2: glibc's ABI is its major.minor", rbeWorkerEnvScript, measured["libc6"])
	}
	for _, banned := range []string{"gcc", "g++", "clang", "lld", "libc6-dev", "libicu-dev", "build-essential"} {
		if slices.Contains(toolset, banned) {
			t.Errorf("%s WORKER_TOOLSET has %s; no action uses a host compiler, linker or ICU headers", rbeWorkerScript, banned)
		}
		if _, ok := measured[banned]; ok {
			t.Errorf("%s measures %s; no action uses a host compiler, linker or ICU headers", rbeWorkerEnvScript, banned)
		}
	}
}

// TestRBEWorkerEnvUpstream: upstream reduces a Debian version to its
// upstream release, cut to PARTS components: epoch and Ubuntu revision
// dropped, +really resolved to the release it really is, repack suffixes
// (.dfsg, +dfsg) dropped, and a version it cannot parse printed whole.
func TestRBEWorkerEnvUpstream(t *testing.T) {
	script := filepath.Join(repoRoot(t), rbeWorkerEnvScript)
	for _, tc := range []struct {
		version string
		parts   int
		want    string
	}{
		{"9.4-3ubuntu6.1", 2, "9.4"},
		{"9.4-3ubuntu6.2", 2, "9.4"},
		{"2.39-0ubuntu8.6", 2, "2.39"},
		{"2.39-0ubuntu8.8", 2, "2.39"},
		{"2.40-1ubuntu1", 2, "2.40"},
		{"1:2.52.0-0ppa1~ubuntu24.04.3", 1, "2"},
		{"1:2.55.0-0ppa1~ubuntu24.04.2", 1, "2"},
		{"1:3.0.0-0ppa1~ubuntu24.04.1", 1, "3"},
		{"1:2.43.0-1ubuntu7.3", 2, "2.43"},
		{"5.6.1+really5.4.5-1ubuntu0.3", 2, "5.4"},
		{"1:1.3.dfsg-3.1ubuntu2.2", 2, "1.3"},
		{"2.9.14+dfsg-1.3ubuntu3.9", 2, "2.9"},
		{"1.35+dfsg-3build1", 2, "1.35"},
		{"1.35+dfsg-3ubuntu0.4", 2, "1.35"},
		{"14.2.0-4ubuntu2~24.04.1", 2, "14.2"},
		{"2:4.0.4-4ubuntu3.2", 2, "4.0"},
		{"1:3.10-1build1", 2, "3.10"},
		{"6.4+20240113-1ubuntu2.2", 2, "6.4"},
		{"4.9", 2, "4.9"},         // native, no revision
		{"7", 2, "7"},             // fewer components than PARTS
		{"4.47.2", 1, "4"},        // yq's own version
		{"v4.47.2", 1, "v4.47.2"}, // not a Debian version: whole
		{"git-snapshot-1", 2, "git-snapshot-1"},
	} {
		out, stderr, err := runRBEScript("", []string{"PATH=/usr/bin:/bin"}, "-c",
			`source "$1" && upstream "$2" "$3"`, "upstream-test", script, tc.version, strconv.Itoa(tc.parts))
		if err != nil {
			t.Fatalf("upstream %q %d: %v\n%s", tc.version, tc.parts, err, stderr)
		}
		if got := strings.TrimSuffix(out, "\n"); got != tc.want {
			t.Errorf("upstream %q %d = %q, want %q", tc.version, tc.parts, got, tc.want)
		}
	}
}

// workerEnvHost describes a stub host for tools/rbe/worker-env: what uname,
// os-release, go, dolt and yq say, and dpkg's installed versions by package
// (one per architecture, space-separated; a package not listed is not
// installed; "!V" is deinstalled at V). An empty dolt, go or yq is not on
// the PATH.
type workerEnvHost struct {
	arch, osVersion, goVersion, dolt, yq string
	pkgs                                 map[string]string
}

func (h workerEnvHost) with(f func(*workerEnvHost)) workerEnvHost {
	c := h
	c.pkgs = maps.Clone(h.pkgs)
	f(&c)
	return c
}

// measureWorkerEnv runs tools/rbe/worker-env (args) on a stub host and
// returns what it prints, or its error and output.
func measureWorkerEnv(t *testing.T, h workerEnvHost, args ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	stubs := filepath.Join(dir, "bin")
	if err := os.MkdirAll(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	var pkgs strings.Builder
	for name, versions := range h.pkgs {
		pkgs.WriteString(name + " " + versions + "\n")
	}
	pkgFile := filepath.Join(dir, "pkgs")
	if err := os.WriteFile(pkgFile, []byte(pkgs.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	stubBodies := map[string]string{
		"uname": "echo " + h.arch + "\n",
		"dolt":  "echo 'dolt version " + h.dolt + "'\necho 'database storage format: NEW'\n",
		"go": `[ "$GOTOOLCHAIN" = local ] || { echo "GOTOOLCHAIN=$GOTOOLCHAIN" >&2; exit 1; }
[ "$(pwd)" = / ] || { echo "cwd $(pwd)" >&2; exit 1; }
echo 'go version go` + h.goVersion + ` linux/amd64'
`,
		"dpkg-query": `for p; do :; done
awk -v p="$p" '$1 == p { for (i = 2; i <= NF; i++) { v = $i; s = "installed"; if (v ~ /^!/) { s = "deinstall"; v = substr(v, 2) }; print s " " v }; found = 1 }
END { if (!found) { print "dpkg-query: no packages found matching " p > "/dev/stderr"; exit 1 } }' '` + pkgFile + `'
`,
	}
	for name, v := range map[string]string{"yq": h.yq, "dolt": h.dolt, "go": h.goVersion} {
		if v == "" {
			delete(stubBodies, name) // not on the PATH
		}
	}
	if h.yq != "" {
		stubBodies["yq"] = "echo '" + h.yq + "'\n"
	}
	for name, body := range stubBodies {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	osRelease := filepath.Join(dir, "os-release")
	if err := os.WriteFile(osRelease, []byte("NAME=\"Ubuntu\"\nID=ubuntu\nVERSION_ID=\""+h.osVersion+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Only the stubs and the shell's own tools: no host yq, go or dolt.
	shellTools := filepath.Join(dir, "shell")
	if err := os.MkdirAll(shellTools, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"sed", "sort", "paste", "awk", "cat"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if err := os.Symlink(p, filepath.Join(shellTools, tool)); err != nil {
			t.Fatal(err)
		}
	}
	path := stubs + string(os.PathListSeparator) + shellTools
	out, stderr, err := runRBEScript("", []string{"WORKER_ENV_PATH=" + path, "WORKER_ENV_OS_RELEASE=" + osRelease, "GOTOOLCHAIN=auto"},
		filepath.Join(repoRoot(t), rbeWorkerEnvScript), args...)
	if err != nil {
		return out + stderr, err
	}
	return out, nil
}

// blacksmithImage20261006 is the Blacksmith ubuntu-24.04 image the pool ran
// on before 2026-10-07, as dpkg reported it (the measured packages; curl,
// dash, openssl, python3 and unzip at the image's noble versions).
var blacksmithImage20261006 = workerEnvHost{
	arch: "x86_64", osVersion: "24.04", goVersion: "1.26.6", dolt: "2.1.8",
	yq: "yq (https://github.com/mikefarah/yq/) version v4.47.2",
	pkgs: map[string]string{
		"bash": "5.2.21-2ubuntu4", "coreutils": "9.4-3ubuntu6.1", "curl": "8.5.0-2ubuntu10.6",
		"dash": "0.5.12-6ubuntu5", "diffutils": "1:3.10-1build1", "findutils": "4.9.0-5build1",
		"git": "1:2.52.0-0ppa1~ubuntu24.04.3", "grep": "3.11-4build1", "jq": "1.7.1-3ubuntu0.24.04.2",
		"libc6": "2.39-0ubuntu8.6 2.39-0ubuntu8.6", "libgcc-s1": "14.2.0-4ubuntu2~24.04.1",
		"libicu74": "74.2-1ubuntu3.1", "liblzma5": "5.6.1+really5.4.5-1ubuntu0.3",
		"libstdc++6": "14.2.0-4ubuntu2~24.04.1", "libxml2": "2.9.14+dfsg-1.3ubuntu3.9",
		"lsof": "4.95.0-1build3", "make": "4.3-4.1build2", "openssl": "3.0.13-0ubuntu3.5",
		"procps": "2:4.0.4-4ubuntu3.2", "python3": "3.12.3-0ubuntu2.1", "sed": "4.9-2build1",
		"sqlite3": "3.45.1-1ubuntu2.8", "tar": "1.35+dfsg-3build1", "tmux": "3.4-1ubuntu0.1",
		"unzip": "6.0-28ubuntu4.1", "util-linux": "2.39.3-9ubuntu6.4",
		"xz-utils": "5.6.1+really5.4.5-1ubuntu0.3", "zlib1g": "1:1.3.dfsg-3.1ubuntu2.2",
		// Installed, and no part of the key.
		"cmake": "3.28.3-1build7", "python3-dev": "3.12.3-0ubuntu2.1",
	},
}

// TestRBEWorkerEnvScript runs tools/rbe/worker-env on a stub host: one
// sorted line per fact, each measured package at its upstream release cut to
// its components and reported per installed architecture once, a removed or
// unknown package "missing", go asked with GOTOOLCHAIN=local away from any
// go.mod, --raw printing dpkg's versions as installed, and a missing go or
// dolt fatal.
func TestRBEWorkerEnvScript(t *testing.T) {
	host := blacksmithImage20261006.with(func(h *workerEnvHost) {
		h.pkgs["libicu74"] = "74.2-1ubuntu3.1 74.2-1ubuntu3.1:i386-only"
		h.pkgs["tmux"] = "!3.4-1"
		delete(h.pkgs, "unzip")
	})
	got, err := measureWorkerEnv(t, host)
	if err != nil {
		t.Fatalf("worker-env: %v\n%s", err, got)
	}
	want := `arch x86_64
dolt dolt version 2.1.8
go go version go1.26.6 linux/amd64
os ubuntu 24.04
pkg bash 5.2
pkg coreutils 9.4
pkg curl 8.5
pkg dash 0.5
pkg diffutils 3.10
pkg findutils 4.9
pkg git 2
pkg grep 3.11
pkg jq 1.7
pkg libc6 2.39
pkg libgcc-s1 14.2
pkg libicu74 74.2
pkg liblzma5 5.4
pkg libstdc++6 14.2
pkg libxml2 2.9
pkg lsof 4.95
pkg make 4.3
pkg openssl 3.0
pkg procps 4.0
pkg python3 3.12
pkg sed 4.9
pkg sqlite3 3.45
pkg tar 1.35
pkg tmux missing
pkg unzip missing
pkg util-linux 2.39
pkg xz-utils 5.4
pkg zlib1g 1.3
tool yq 4
`
	if got != want {
		t.Errorf("worker-env output:\n%s\nwant:\n%s", got, want)
	}

	raw, err := measureWorkerEnv(t, host, "--raw")
	if err != nil {
		t.Fatalf("worker-env --raw: %v\n%s", err, raw)
	}
	for _, line := range []string{
		"pkg coreutils 9.4-3ubuntu6.1\n",
		"pkg git 1:2.52.0-0ppa1~ubuntu24.04.3\n",
		"pkg libc6 2.39-0ubuntu8.6\n",
		"pkg libicu74 74.2-1ubuntu3.1,74.2-1ubuntu3.1:i386-only\n",
		"pkg tmux missing\n",
		"tool yq yq (https://github.com/mikefarah/yq/) version v4.47.2\n",
	} {
		if !strings.Contains(raw, line) {
			t.Errorf("worker-env --raw lacks %q:\n%s", line, raw)
		}
	}
	if out, err := measureWorkerEnv(t, host, "--bogus"); err == nil {
		t.Errorf("worker-env --bogus succeeded:\n%s", out)
	}

	noYQ := host.with(func(h *workerEnvHost) { h.yq = "" })
	if out, err := measureWorkerEnv(t, noYQ); err != nil || !strings.Contains(out, "\ntool yq missing\n") {
		t.Errorf("worker-env without yq: %v\n%s", err, out)
	}
	otherYQ := host.with(func(h *workerEnvHost) { h.yq = "yq 3.4.3" }) // kislyuk's yq, another tool
	if out, err := measureWorkerEnv(t, otherYQ); err != nil || !strings.Contains(out, "\ntool yq yq 3.4.3\n") {
		t.Errorf("worker-env with another yq: %v\n%s", err, out)
	}

	// No dolt (or go) on the PATH: fatal, never a manifest without it.
	for _, tool := range []string{"dolt", "go"} {
		broken := host.with(func(h *workerEnvHost) {
			if tool == "dolt" {
				h.dolt = ""
			} else {
				h.goVersion = ""
			}
		})
		if out, err := measureWorkerEnv(t, broken); err == nil {
			t.Errorf("worker-env without %s succeeded:\n%s", tool, out)
		}
	}
}

// TestRBEWorkerEnvKeysToolchainNotPatches: the measurement moves with what
// can change an action's result and with nothing else. The 2026-10-07 image
// update (Ubuntu security revisions of coreutils, libc6, sed, tar and
// util-linux, and a git-core PPA minor) measures the same, so it neither
// re-keys the cache nor strands every action; another arch, OS release,
// glibc or library minor, archive tool minor, a git or yq major, Go, dolt,
// or a package gone, measures differently.
func TestRBEWorkerEnvKeysToolchainNotPatches(t *testing.T) {
	base, err := measureWorkerEnv(t, blacksmithImage20261006)
	if err != nil {
		t.Fatalf("worker-env: %v\n%s", err, base)
	}
	for _, tc := range []struct {
		name string
		edit func(*workerEnvHost)
		same bool
	}{
		{"2026-10-07 security updates", func(h *workerEnvHost) {
			h.pkgs["coreutils"] = "9.4-3ubuntu6.2"
			h.pkgs["git"] = "1:2.55.0-0ppa1~ubuntu24.04.2"
			h.pkgs["libc6"] = "2.39-0ubuntu8.8 2.39-0ubuntu8.8"
			h.pkgs["sed"] = "4.9-2ubuntu0.24.04.1"
			h.pkgs["tar"] = "1.35+dfsg-3ubuntu0.4"
			h.pkgs["util-linux"] = "2.39.3-9ubuntu6.5"
		}, true},
		{"libc6 installed with libc6-dev", func(h *workerEnvHost) { h.pkgs["libc6"] = "2.39-0ubuntu8.9" }, true},
		{"upstream patch release", func(h *workerEnvHost) { h.pkgs["sqlite3"] = "3.45.3-1ubuntu0.1" }, true},
		{"git from the archive", func(h *workerEnvHost) { h.pkgs["git"] = "1:2.43.0-1ubuntu7.3" }, true},
		{"yq minor", func(h *workerEnvHost) { h.yq = "yq (https://github.com/mikefarah/yq/) version v4.53.6" }, true},
		{"unmeasured packages", func(h *workerEnvHost) {
			h.pkgs["cmake"] = "3.31.0-1"
			h.pkgs["python3-dev"] = "3.13.0-1"
		}, true},

		{"glibc minor", func(h *workerEnvHost) { h.pkgs["libc6"] = "2.40-1ubuntu1" }, false},
		{"libstdc++ minor", func(h *workerEnvHost) { h.pkgs["libstdc++6"] = "14.3.0-1ubuntu1~24.04" }, false},
		{"ICU minor", func(h *workerEnvHost) { h.pkgs["libicu74"] = "74.3-1" }, false},
		{"arch", func(h *workerEnvHost) { h.arch = "aarch64" }, false},
		{"OS release", func(h *workerEnvHost) { h.osVersion = "26.04" }, false},
		{"git major", func(h *workerEnvHost) { h.pkgs["git"] = "1:3.0.0-0ppa1~ubuntu24.04.1" }, false},
		{"coreutils major", func(h *workerEnvHost) { h.pkgs["coreutils"] = "10.0-1ubuntu1" }, false},
		{"archive tool minor", func(h *workerEnvHost) { h.pkgs["tmux"] = "3.5-1" }, false},
		{"python minor", func(h *workerEnvHost) { h.pkgs["python3"] = "3.13.1-1" }, false},
		{"yq major", func(h *workerEnvHost) { h.yq = "yq (https://github.com/mikefarah/yq/) version v5.0.0" }, false},
		{"Go", func(h *workerEnvHost) { h.goVersion = "1.26.7" }, false},
		{"dolt", func(h *workerEnvHost) { h.dolt = "2.1.9" }, false},
		{"package removed", func(h *workerEnvHost) { delete(h.pkgs, "jq") }, false},
		{"second architecture", func(h *workerEnvHost) { h.pkgs["zlib1g"] = "1:1.3.dfsg-3.1ubuntu2.2 1:1.2.13-1" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := measureWorkerEnv(t, blacksmithImage20261006.with(tc.edit))
			if err != nil {
				t.Fatalf("worker-env: %v\n%s", err, got)
			}
			if same := got == base; same != tc.same {
				t.Errorf("same manifest = %v, want %v\nbase:\n%s\ngot:\n%s", same, tc.same, base, got)
			}
		})
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
// toolset, measures the host with tools/rbe/worker-env, and advertises the
// sha256 of the measurement (never the pin, never the raw listing) before
// any worker.json is rendered.
func TestRBEWorkerScriptAdvertisesWorkerEnv(t *testing.T) {
	script := readFile(t, repoRoot(t), rbeWorkerScript)
	at := 0
	for _, want := range []string{
		"\nWORKER_TOOLSET=(",
		"apt-get install -y -qq \\\n\t\"${WORKER_TOOLSET[@]}\" >/dev/null\n",
		"\tsudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf \"$RUNNER_TEMP/go.tgz\"\n",
		"\tsudo cp -f \"$RUNNER_TEMP/dolt-linux-amd64/bin/dolt\" /usr/local/bin/dolt\n",
		"\ntools/rbe/worker-env >\"$RUNNER_TEMP/worker-env.txt\"\n",
		"WORKER_ENV=sha256:$(sha256sum <\"$RUNNER_TEMP/worker-env.txt\" | cut -d' ' -f1)\n",
		"\ntools/rbe/worker-env --raw >\"$RUNNER_TEMP/worker-env.raw.txt\" || :\n",
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
