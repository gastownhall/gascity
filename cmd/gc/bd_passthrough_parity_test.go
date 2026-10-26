//go:build !windows

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// bdParityFakeBd records, per invocation, everything the passthrough decides
// for bd: argv, working directory, which binary ran, and the routing env.
// It answers like singleExecFakeBd.
const bdParityFakeBd = `#!/bin/sh
{
  printf 'argv=%s\n' "$*"
  printf 'pwd=%s\n' "$(pwd -P)"
  printf 'bin=%s\n' "$0"
  for v in GC_STORE_ROOT GC_STORE_SCOPE GC_BEADS_PREFIX BEADS_DIR GC_RIG GC_RIG_ROOT BD_EXPORT_AUTO GC_BIN BD_ACTOR GC_BD_ALLOW_RELOCATED_CLASS_READ; do
    eval "printf '%s=%s\n' $v \"\${$v}\""
  done
  printf 'http_token=%s\n' "${BEADS_HTTP_TOKEN:+set}"
  printf -- '--\n'
} >> "${BD_PARITY_LOG}.$$"
case " $* " in
  *" demo-missing "*) echo 'no issue found' >&2; echo 'not-found-stdout'; exit 3 ;;
esac
case "$1" in
  show) echo '[{"id":"demo-abc","title":"t","status":"open","issue_type":"task"}]' ;;
  list|ready) echo '[]' ;;
  update) echo '{"id":"demo-abc"}' ;;
  *) : ;;
esac
exit 0
`

// bdParityCity is one arm's fixture. Every path it creates lives under root,
// so normalizing root makes two arms' outputs comparable.
type bdParityCity struct {
	// rawRoot is root as t.TempDir spells it; on macOS it reaches root through
	// the /var -> /private/var symlink, and gc reports some paths each way.
	rawRoot                       string
	root, city, rig, bin, scratch string
}

type bdParityOpts struct {
	storage    string // appended to city.toml ([storage.*] for a split city)
	registered bool   // register the city and its rig in the supervisor registry
	cityTOML   string // replaces city.toml outright (error fixtures)
}

// newBdParityCity builds a v2 city with a bound rig (frontend), an unbound
// declared rig (backend), the canonical builtin imports, and the parity fake
// bd pinned through [workspace.env] PATH so the pinned-bd resolution is part
// of what is compared.
func newBdParityCity(t *testing.T, opts bdParityOpts) bdParityCity {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	config.ResetRemoteCacheValidationCache()
	resetHostedCredentialProbeCache()
	restoreFlag(t, &cityFlag)
	restoreFlag(t, &rigFlag)
	resetCLIStorageRoutes(t)
	captureCLIStorageStderr(t)

	rawRoot := t.TempDir()
	root, err := filepath.EvalSymlinks(rawRoot)
	if err != nil {
		t.Fatal(err)
	}
	c := bdParityCity{
		rawRoot: rawRoot,
		root:    root,
		city:    filepath.Join(root, "city"),
		bin:     filepath.Join(root, "bin"),
		scratch: filepath.Join(root, "scratch"),
	}
	c.rig = filepath.Join(c.city, "frontend")
	for _, dir := range []string{
		filepath.Join(c.city, ".beads"), filepath.Join(c.city, ".gc"),
		filepath.Join(c.rig, ".beads"), c.bin, c.scratch,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cityTOML := opts.cityTOML
	if cityTOML == "" {
		cityTOML = "[workspace.env]\nPATH = " + strconv.Quote(c.bin+string(os.PathListSeparator)+"$PATH") + "\n" +
			"\n[[rigs]]\nname = \"frontend\"\nprefix = \"fe\"\n" +
			"\n[[rigs]]\nname = \"backend\"\nprefix = \"be\"\n" + opts.storage
	}
	if err := os.WriteFile(filepath.Join(c.city, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	site := fmt.Sprintf("workspace_name = \"demo\"\nworkspace_prefix = \"demo\"\n\n[[rig]]\nname = \"frontend\"\npath = %q\n", c.rig)
	if err := os.WriteFile(filepath.Join(c.city, ".gc", "site.toml"), []byte(site), 0o644); err != nil {
		t.Fatal(err)
	}
	if opts.cityTOML == "" {
		writeBuiltinImportsFixture(t, c.city, "core", "bd")
	}
	if opts.storage != "" {
		writeReachableManagedDoltState(t, c.city)
		if err := os.WriteFile(filepath.Join(c.city, ".beads", "config.yaml"), []byte("issue_prefix: demo\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if opts.registered {
		if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(c.city, "demo"); err != nil {
			t.Fatalf("registering city: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(c.bin, "bd"), []byte(bdParityFakeBd), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", c.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_PARITY_LOG", filepath.Join(c.scratch, "bd.log"))
	t.Setenv("GC_CITY_PATH", c.city)
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_DIR", "")
	t.Setenv(bdRelocatedClassOverrideEnvVar, "")
	setCwd(t, c.city)
	return c
}

// bdParityTimestamp matches the RFC 3339 times an in-process answer renders,
// which differ between two arms that straddle a second boundary.
var bdParityTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)

type bdParityResult struct {
	code           int
	stdout, stderr string
	bd             string
}

// runBdParityArm builds a fresh fixture, applies the case, and runs doBd once,
// on the legacy loaders or the trimmed ones.
func runBdParityArm(t *testing.T, legacy bool, opts bdParityOpts, setup func(t *testing.T, c bdParityCity) []string) bdParityResult {
	t.Helper()
	c := newBdParityCity(t, opts)
	args := setup(t, c)
	if legacy {
		defer useLegacyBdPassthroughSeams()()
	}
	var stdout, stderr bytes.Buffer
	code := doBd(args, &stdout, &stderr)
	log := readBdParityLog(t, c.scratch)
	gcHome := os.Getenv("GC_HOME")
	norm := func(s string) string {
		// GC_HOME first: clearGCEnv puts it outside root, and a cache path in
		// an error names it.
		if gcHome != "" {
			s = strings.ReplaceAll(s, gcHome, "$GC_HOME")
		}
		s = strings.ReplaceAll(strings.ReplaceAll(s, c.root, "$ROOT"), c.rawRoot, "$ROOT")
		return bdParityTimestamp.ReplaceAllString(s, "$$TIME")
	}
	return bdParityResult{code: code, stdout: norm(stdout.String()), stderr: norm(stderr.String()), bd: norm(log)}
}

// TestGcBdPassthroughTrimsKeepRouting runs every case twice, on the loaders
// gc bd used before the trims (the general resolver with its rig decoration,
// and the full self-healing load) and on the trimmed ones, and requires the
// same exit code, stdout, stderr, and bd invocations (argv, working
// directory, binary, routing env).
func TestGcBdPassthroughTrimsKeepRouting(t *testing.T) {
	plain := bdParityOpts{}
	split := bdParityOpts{storage: bdSQLRefusalSplitStorage}
	registered := bdParityOpts{registered: true}
	args := func(a ...string) func(*testing.T, bdParityCity) []string {
		return func(*testing.T, bdParityCity) []string { return a }
	}
	cases := []struct {
		name  string
		opts  bdParityOpts
		setup func(t *testing.T, c bdParityCity) []string
	}{
		// P4: scope selection.
		{"P4 explicit --rig", plain, args("--rig", "frontend", "show", "demo-abc")},
		{"P4 explicit --city", plain, func(t *testing.T, c bdParityCity) []string {
			setCwd(t, c.rig)
			return []string{"--city", c.city, "list", "--json"}
		}},
		{"P4 gc --rig", registered, func(t *testing.T, _ bdParityCity) []string {
			t.Setenv("GC_CITY_PATH", "")
			rigFlag = "frontend"
			return []string{"list", "--json"}
		}},
		{"P4 gc --city", plain, func(_ *testing.T, c bdParityCity) []string {
			cityFlag = c.city
			return []string{"list", "--json"}
		}},
		{"P4 bead-prefix probe", plain, args("show", "fe-abc")},
		{"P4 -C rig", plain, func(_ *testing.T, c bdParityCity) []string {
			return []string{"-C", c.rig, "list", "--json"}
		}},
		{"P4 bound GC_RIG", plain, func(t *testing.T, _ bdParityCity) []string {
			t.Setenv("GC_RIG", "frontend")
			return []string{"list", "--json"}
		}},
		{"P4 unbound GC_RIG warns", plain, func(t *testing.T, _ bdParityCity) []string {
			t.Setenv("GC_RIG", "nope")
			return []string{"show", "demo-abc"}
		}},
		{"P4 cwd inside a rig", plain, func(t *testing.T, c bdParityCity) []string {
			setCwd(t, c.rig)
			return []string{"list", "--json"}
		}},
		{"P4 GC_CITY with a rig GC_DIR", plain, func(t *testing.T, c bdParityCity) []string {
			t.Setenv("GC_DIR", c.rig)
			return []string{"list", "--json"}
		}},
		{"P4 GC_DIR city from a rig cwd", plain, func(t *testing.T, c bdParityCity) []string {
			t.Setenv("GC_CITY_PATH", "")
			t.Setenv("GC_DIR", c.city)
			setCwd(t, c.rig)
			return []string{"list", "--json"}
		}},
		{"P4 registered rig cwd", registered, func(t *testing.T, c bdParityCity) []string {
			t.Setenv("GC_CITY_PATH", "")
			setCwd(t, c.rig)
			return []string{"show", "demo-abc"}
		}},
		// P5: writes.
		{"P5 update", plain, args("update", "demo-abc", "--status", "in_progress")},
		{"P5 close warn gate", plain, args("close", "demo-abc")},
		{"P5 close enforce gate", plain, func(t *testing.T, _ bdParityCity) []string {
			t.Setenv("GC_WORK_RECORD_ENFORCE", "1")
			return []string{"close", "demo-abc"}
		}},
		{"P5 heartbeat", plain, args("heartbeat", "demo-abc")},
		{"P5 rig write from a rig cwd", plain, func(t *testing.T, c bdParityCity) []string {
			setCwd(t, c.rig)
			return []string{"update", "fe-abc", "--status", "in_progress"}
		}},
		{"P5 release-if-current", plain, args("release-if-current", "demo-abc", "someone")},
		{"P5 create", plain, args("create", "--title", "x")},
		// P6: wisp tier.
		{"P6 wisp-tier list", plain, args("list", "--type", "molecule", "--json")},
		// P2/P3 on a split city.
		{"P2 ready", split, args("ready", "--json")},
		{"P2 list --ready", split, args("list", "--ready", "--json")},
		{"P2 ready with the override", split, func(t *testing.T, _ bdParityCity) []string {
			t.Setenv(bdRelocatedClassOverrideEnvVar, "1")
			return []string{"ready", "--json"}
		}},
		{"P3 sql", split, args("sql", "select id from issues where issue_type = 'molecule'")},
		{"P3 query", split, args("query", "type=molecule")},
		{"P3 list --type molecule", split, args("list", "--type", "molecule")},
		{"P3 create --type session", split, args("create", "--type", "session", "--title", "x")},
		// P1: relocated-class ids on a split city.
		{"P1 show a class bead", split, func(t *testing.T, c bdParityCity) []string {
			return []string{"show", seedRelocatedBindingBead(t, c.city, "held").ID}
		}},
		{"P1 close a class bead", split, func(t *testing.T, c bdParityCity) []string {
			return []string{"close", seedRelocatedBindingBead(t, c.city, "held").ID}
		}},
		{"P1 unserved show --long", split, func(t *testing.T, c bdParityCity) []string {
			return []string{"show", "--long", seedRelocatedBindingBead(t, c.city, "held").ID}
		}},
		{"P1 --rig with a class id", split, func(t *testing.T, c bdParityCity) []string {
			return []string{"--rig", "frontend", "show", seedRelocatedBindingBead(t, c.city, "held").ID}
		}},
		// P9: provider guard.
		{"P9 non-bd provider", plain, func(t *testing.T, c bdParityCity) []string {
			appendFile(t, filepath.Join(c.city, "city.toml"), "\n[beads]\nprovider = \"file\"\n")
			return []string{"show", "demo-abc"}
		}},
		{"P9 exit code", plain, args("show", "demo-missing")},
		// P10: error surfaces.
		{"P10 no city", plain, func(t *testing.T, c bdParityCity) []string {
			t.Setenv("GC_CITY_PATH", "")
			setCwd(t, c.scratch)
			return []string{"show", "demo-abc"}
		}},
		{"P10 remote target", plain, func(t *testing.T, _ bdParityCity) []string {
			t.Setenv("GC_CITY_PATH", "")
			t.Setenv("GC_CITY_URL", "https://box.internal:9443")
			return []string{"show", "demo-abc"}
		}},
		{"P10 city.toml syntax error", bdParityOpts{cityTOML: "[[rigs]\nname = \n"}, args("show", "demo-abc")},
		{"P10 unknown --rig", plain, args("--rig", "zzz", "show", "demo-abc")},
		{"P10 unbound --rig", plain, args("--rig", "backend", "show", "demo-abc")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			legacy := runBdParityArm(t, true, tc.opts, tc.setup)
			trimmed := runBdParityArm(t, false, tc.opts, tc.setup)
			if legacy != trimmed {
				t.Fatalf("routing changed:\nlegacy:  code=%d\n stdout=%q\n stderr=%q\n bd=%s\ntrimmed: code=%d\n stdout=%q\n stderr=%q\n bd=%s",
					legacy.code, legacy.stdout, legacy.stderr, legacy.bd,
					trimmed.code, trimmed.stdout, trimmed.stderr, trimmed.bd)
			}
		})
	}
}

// readBdParityLog joins the fake bd's records, one file per invocation (named
// by its pid), in sorted order. A two-tier BdStore list runs its bd list and bd
// query reads at once, so their order, and any interleaving of two processes
// appending to one file, are not part of what the arms are compared on.
func readBdParityLog(t *testing.T, scratch string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(scratch, "bd.log.*"))
	if err != nil {
		t.Fatal(err)
	}
	records := make([]string, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, string(data))
	}
	sort.Strings(records)
	return strings.Join(records, "")
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
