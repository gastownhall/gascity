//go:build acceptance_b

// Split-storage end-to-end acceptance (#5987, gc 1.5.1 lane split151).
//
// A real controller and a scripted, non-inference worker run a graph.v2
// formula across a storage cutover. The worker claims through `gc hook
// --claim` and closes through `gc bd`, the same front doors a real agent uses,
// so every by-id write lands wherever gc routes it and every demand read is
// whatever the controller and the hook federate.
//
// The defect this pins: after `gc storage migrate --from-work --fleet-stopped`
// every relocated infrastructure bead is co-resident — the binding copy plus
// the retained work-store copy. Claims and closes reach the binding, demand
// reads the work copy first, so a step closed in the binding stays open in the
// work store and is dispatched again, forever. The loop is detected by event
// counts within a bounded wait, never by a hang.
//
// Scenarios:
//
//   - FreshMigration: a city without [storage] runs half the formula, stops,
//     authors the split, migrates with this binary, starts and finishes.
//   - MigratedByV150RC1: the same, but everything up to and including the
//     migration is run by a v1.5.0-rc1 gc (GC_ACCEPTANCE_SPLIT_RC1_GC_BIN, or
//     built here from the tag when the clone has it). Booting that city with
//     this binary must refuse and name the repair command; running it repairs
//     the city.
//   - BornSplit: a city with [storage] from its first boot. The control.
//
// Requires: bd >= 1.3.0, dolt, jq. The work store is a bd city bound to an
// external Dolt server the test owns (the --dolt-host canonical-endpoint
// shape, AC-M M3a). That shape is chosen because it is the one a stopped city
// can still be migrated from by every binary involved: gc reads it through the
// native Dolt store, which reports edge payloads. A gc-managed or bd-proxied
// server goes down with `gc stop`, leaving only the BdStore lane, which the
// migration's edge-payload check refuses (v1.5.0-rc1 has no proxied-native
// lane at all), and the file store has no CLI write path a scripted worker
// could close a step through. Each scenario gets its own GC_HOME,
// XDG_RUNTIME_DIR, supervisor port, bd/dolt tool home and Dolt server on an
// ephemeral loopback port, asserted never to be 3307.
//
// Run:
//
//	git fetch --no-tags --depth=1 origin +refs/tags/v1.5.0-rc1:refs/tags/v1.5.0-rc1
//	make test-acceptance-split-storage GC_ACCEPTANCE_BD_BIN=/path/to/bd-1.3.1
package tierb_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

const (
	// splitRC1GCBinEnv names a gc built from tag v1.5.0-rc1, the release whose
	// migration retains the work-store copies this test catches re-dispatching.
	splitRC1GCBinEnv = "GC_ACCEPTANCE_SPLIT_RC1_GC_BIN"
	// splitRequireRC1Env makes a missing rc1 binary fatal instead of a skip.
	splitRequireRC1Env = "GC_REQUIRE_ACCEPTANCE_SPLIT_RC1_GC"

	// splitRepairCommand is the command a boot refusal of an unrepaired
	// migrated city must name, and the one that repairs it.
	splitRepairCommand = "gc storage migrate --from-work --fleet-stopped"

	splitFormulaName = "split-e2e"
	// splitWorkerSteps is the number of formula steps the worker closes; the
	// controller closes the synthesized workflow-finalize step itself.
	splitWorkerSteps = 4
	// splitStepsBeforeStop is how many steps close before the cutover.
	splitStepsBeforeStop = 2

	// splitPhaseTimeoutDefault bounds each formula half. A run under a
	// tracer (the home-isolation audit runs the scenarios under strace)
	// raises it with splitPhaseTimeoutEnv.
	splitPhaseTimeoutDefault = 4 * time.Minute
	splitPhaseTimeoutEnv     = "GC_ACCEPTANCE_SPLIT_PHASE_TIMEOUT"
)

const splitFormulaTOML = `formula = "split-e2e"
description = "Sequential graph.v2 chain for the split-storage acceptance test"
version = 2
contract = "graph.v2"

[[steps]]
id = "s1"
title = "Step one"

[[steps]]
id = "s2"
title = "Step two"
needs = ["s1"]

[[steps]]
id = "s3"
title = "Step three"
needs = ["s2"]

[[steps]]
id = "s4"
title = "Step four"
needs = ["s3"]
`

// splitStorageTOML is the runbook's split: work stays on the work ledger, the
// five infrastructure classes move to one SQLite binding.
const splitStorageTOML = `
[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/store"
`

// splitWorkerScript is the scripted worker. It claims one routed bead at a
// time through the hook and closes it through gc bd. While the budget file
// exists it closes at most that many beads, which is how the test stops the
// formula halfway at a deterministic point.
const splitWorkerScript = `#!/bin/bash
set -u
cd "$GC_CITY" || exit 1
# GC_BIN is the exact binary the controller runs as; a bare gc is whatever
# PATH holds.
GC="${GC_BIN:-gc}"
LOG="$GC_CITY/.gc/split-e2e-worker.log"
BUDGET="$GC_CITY/.gc/split-e2e-budget"
log() { printf '%s %s\n' "$(date -u +%H:%M:%S.%N)" "$*" >> "$LOG"; }
log "start session=${GC_SESSION_NAME:-} id=${GC_SESSION_ID:-} gc=$GC"
while true; do
  if [ -f "$BUDGET" ] && [ "$(cat "$BUDGET" 2>/dev/null || echo 0)" -le 0 ]; then
    sleep 0.3
    continue
  fi
  out=$(timeout 60 "$GC" hook --claim --json 2>>"$LOG")
  last=$(printf '%s\n' "$out" | tail -n 1)
  action=$(printf '%s\n' "$last" | jq -r '.action // empty' 2>/dev/null)
  id=$(printf '%s\n' "$last" | jq -r '.bead_id // empty' 2>/dev/null)
  if [ "$action" != "work" ] || [ -z "$id" ]; then
    sleep 0.3
    continue
  fi
  log "claimed $id"
  if timeout 60 "$GC" bd update "$id" --set-metadata gc.outcome=pass --set-metadata gc.work_outcome=no-op --status closed >>"$LOG.bd" 2>&1; then
    log "closed $id"
    if [ -f "$BUDGET" ]; then
      echo $(( $(cat "$BUDGET") - 1 )) > "$BUDGET"
    fi
  else
    log "close-failed $id"
  fi
done
`

func TestSplitStorageMigratedFormulaCompletes(t *testing.T) {
	bdPath, doltPath := helpers.RequireTopologyTooling(t)
	if _, err := exec.LookPath("jq"); err != nil {
		helpers.MissingTooling(t, "jq is not installed; the scripted worker parses gc hook --claim --json with it")
	}

	t.Run("FreshMigration", func(t *testing.T) {
		c := newSplitE2ECity(t, bdPath, doltPath, false, "")
		root := c.runFirstHalf()
		edge := c.addWorkEdgeInto(root)
		c.authorSplit()
		c.assertEdgeAtRiskBeforeClear(c.migrate(), edge)
		c.finishAndAssert(root)
		c.assertEdgeLost(edge)
		c.rollbackAndRestoreFromBackup(root)
		c.assertEdgeRestored(edge)
	})

	t.Run("MigratedByV150RC1", func(t *testing.T) {
		// The whole pre-cutover life of this city is v1.5.0-rc1's: it is
		// initialized, run, stopped and migrated by that binary, under its
		// own supervisor, exactly as an operator who adopted 1.5.0 did. Only
		// then is the binary under test handed the city.
		rc1 := splitRC1Binary(t)
		c := newSplitE2ECity(t, bdPath, doltPath, false, rc1)
		root := c.runFirstHalf()
		c.authorSplit()
		c.migrate()
		c.handOver()
		c.expectBootRefusalNamingRepair()
		c.migrate()
		c.finishAndAssert(root)
	})

	t.Run("BornSplit", func(t *testing.T) {
		c := newSplitE2ECity(t, bdPath, doltPath, true, "")
		root := c.runFirstHalf()
		c.finishAndAssert(root)
	})
}

// splitPhaseTimeout is splitPhaseTimeoutDefault, or the duration in
// splitPhaseTimeoutEnv when that parses.
func splitPhaseTimeout() time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(splitPhaseTimeoutEnv))); err == nil && d > 0 {
		return d
	}
	return splitPhaseTimeoutDefault
}

// splitRC1Tag is the release whose migration retains the work-store copies.
const splitRC1Tag = "v1.5.0-rc1"

// splitRC1Binary returns the v1.5.0-rc1 gc: GC_ACCEPTANCE_SPLIT_RC1_GC_BIN when
// set, otherwise one built here from the tag in this clone's history. It skips
// (or fails under splitRequireRC1Env) only when neither is available — a
// shallow CI checkout has no tags until it fetches this one. A set-but-unusable
// path, or a build of a present tag that fails, is always a failure.
func splitRC1Binary(t *testing.T) string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(splitRC1GCBinEnv))
	if raw == "" {
		return buildSplitRC1FromTag(t)
	}
	bin, err := filepath.Abs(raw)
	if err != nil {
		t.Fatalf("resolving %s %q: %v", splitRC1GCBinEnv, raw, err)
	}
	if info, err := os.Stat(bin); err != nil || info.IsDir() {
		t.Fatalf("%s %s is not an executable file: %v", splitRC1GCBinEnv, bin, err)
	}
	return bin
}

// buildSplitRC1FromTag extracts the tagged tree with git archive (no worktree
// is registered, nothing in the clone changes) and builds its gc into a
// test-owned temp dir. The only network it can need is the Go module proxy
// for dependencies the module cache lacks, the same access `go test` has.
func buildSplitRC1FromTag(t *testing.T) string {
	t.Helper()
	repo := helpers.FindModuleRoot()
	commit, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", splitRC1Tag+"^{commit}").Output()
	if err != nil || strings.TrimSpace(string(commit)) == "" {
		reason := fmt.Sprintf("%s is unset and tag %s is not in this clone (git fetch --no-tags --depth=1 origin +refs/tags/%s:refs/tags/%s), so there is no v1.5.0-rc1 gc for the upgraded-city scenario",
			splitRC1GCBinEnv, splitRC1Tag, splitRC1Tag, splitRC1Tag)
		if v := strings.TrimSpace(os.Getenv(splitRequireRC1Env)); v != "" && v != "0" {
			t.Fatalf("%s is set, so this scenario must run, but %s", splitRequireRC1Env, reason)
		}
		t.Skip(reason)
	}
	dir := helpers.TempDir(t)
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("create rc1 source dir: %v", err)
	}
	archive := exec.Command("sh", "-c", `git -C "$1" archive --format=tar "$2" | tar -x -C "$3"`, "--", repo, strings.TrimSpace(string(commit)), src)
	if out, err := archive.CombinedOutput(); err != nil {
		t.Fatalf("extract %s: %v\n%s", splitRC1Tag, err, out)
	}
	bin := filepath.Join(dir, "bin", "gc")
	build := exec.Command("go", "build", "-o", bin, "./cmd/gc")
	build.Dir = src
	build.Env = splitGoBuildEnv(t, dir)
	started := time.Now()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gc from %s: %v\n%s", splitRC1Tag, err, out)
	}
	t.Logf("built gc %s (%s) in %s", splitRC1Tag, strings.TrimSpace(string(commit))[:10], time.Since(started).Round(time.Second))
	return bin
}

// splitGoBuildEnv is the environment the rc1 build runs under. The toolchain
// keeps per-user state through HOME and XDG_CONFIG_HOME — its go/env file and
// the telemetry counters it rewrites on every invocation — so both point into
// dir. The cache, module and toolchain settings that would have come from the
// go/env file are resolved first and passed explicitly, so the build still
// shares the developer's (or CI's) warm build and module caches.
func splitGoBuildEnv(t *testing.T, dir string) []string {
	t.Helper()
	keys := []string{"GOCACHE", "GOMODCACHE", "GOPATH", "GOPROXY", "GOSUMDB", "GONOSUMDB", "GONOPROXY", "GOPRIVATE", "GOTOOLCHAIN", "GOTMPDIR", "GOROOT"}
	out, err := exec.Command("go", append([]string{"env", "-json"}, keys...)...).Output()
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	var vals map[string]string
	if err := json.Unmarshal(out, &vals); err != nil {
		t.Fatalf("decode go env -json: %v\n%s", err, out)
	}
	home := filepath.Join(dir, "build-home")
	config := filepath.Join(home, ".config")
	if err := os.MkdirAll(config, 0o755); err != nil {
		t.Fatalf("create %s: %v", config, err)
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + config,
		"TMPDIR=" + os.TempDir(),
		"CGO_ENABLED=0",
		"GOWORK=off",
		"GOFLAGS=-mod=mod",
		"GOTELEMETRY=off",
	}
	for _, k := range keys {
		if v := strings.TrimSpace(vals[k]); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// splitE2ECity is one isolated city plus the paths the assertions read.
type splitE2ECity struct {
	t    *testing.T
	env  *helpers.Env // the binary under test
	city *helpers.City
	root string
	dir  string
	// preEnv runs everything before the hand-over: the binary under test, or
	// for the upgrade scenario the v1.5.0-rc1 gc, first on PATH as `gc` so
	// the worker's own gc calls and the supervisor resolve the same binary.
	preEnv *helpers.Env
	// standalone runs the city's controller as `gc start --foreground`
	// instead of under a supervisor: the v1.5.0-rc1 phase does, because that
	// binary's supervisor refuses any HOME but the passwd one, and this test
	// never hands any gc the real HOME.
	standalone bool
	// startedControllers counts the controllers this test has seen start.
	startedControllers int
	// controller is the running standalone controller, nil otherwise.
	controller *exec.Cmd
	// controllerDone is closed once controller has exited.
	controllerDone chan struct{}
}

func newSplitE2ECity(t *testing.T, bdPath, doltPath string, bornSplit bool, preBin string) *splitE2ECity {
	t.Helper()
	gcBin, err := helpers.ResolveGCPath(testEnvB)
	if err != nil {
		t.Fatalf("resolve gc binary: %v", err)
	}
	root := helpers.TempDir(t)
	gcHome := filepath.Join(root, "gc-home")
	runtimeDir := filepath.Join(root, "runtime")
	for _, dir := range []string{gcHome, runtimeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	if err := helpers.WriteSupervisorConfig(gcHome); err != nil {
		t.Fatalf("write supervisor config: %v", err)
	}
	base := splitIsolatedUserDirs(t, helpers.NewEnv(gcBin, gcHome, runtimeDir).With("GC_SESSION", "subprocess"), root)
	// The binary under test is installed as `gc` in a directory of its own.
	// helpers.NewEnv only prepends the binary's directory to PATH, so a binary
	// not named gc (gc-a2, say) left the worker's `gc hook`/`gc bd` calls and
	// the sessions resolving whatever `gc` sat beside it — a different build.
	env := splitBinaryEnv(t, helpers.TopologyEnv(t, base, root, bdPath, doltPath), gcBin, filepath.Join(root, "gc-bin"))
	preEnv := env
	if preBin != "" {
		// The pre-cutover binary keeps the isolated HOME too. v1.5.0-rc1
		// predates GC_SUPERVISOR_ISOLATED_HOME (#7033), and its supervisor
		// start paths refuse any HOME but the passwd one, so that phase runs
		// a standalone controller instead (see splitE2ECity.standalone).
		preEnv = splitBinaryEnv(t, env, preBin, filepath.Join(root, "pre-bin"))
	}

	// Registered before the upstream and the city so it runs after both have
	// stopped: no Dolt server or bd proxy under this root may outlive the test.
	t.Cleanup(func() {
		if left := helpers.WaitForNoDoltProcesses(t, root, 30*time.Second); len(left) > 0 {
			t.Errorf("dolt/bd proxy processes survived the test:\n%s", strings.Join(left, "\n"))
		}
	})
	upstream := helpers.StartExternalDolt(t, env, filepath.Join(root, "upstream"), "split_e2e")
	if upstream.Port == strconv.Itoa(splitLegacyDoltPort) {
		t.Fatalf("the test's Dolt server landed on %d, the host's shared default; acceptance must never touch it", splitLegacyDoltPort)
	}
	upstream.ProvisionBeadsDatabase(t, env, bdPath, filepath.Join(root, "provision"), "hosted")

	scriptPath := filepath.Join(root, "split-e2e-worker.sh")
	if err := os.WriteFile(scriptPath, []byte(splitWorkerScript), 0o755); err != nil {
		t.Fatalf("write worker script: %v", err)
	}
	dir := filepath.Join(root, "city")
	city := helpers.NewCityAt(t, preEnv, dir)
	t.Cleanup(func() {
		city.CleanupRuntime()
		// Whichever binary the city ended on, stop the other one's
		// supervisor too; both share this test's GC_HOME.
		helpers.RunGC(preEnv, "", "supervisor", "stop", "--wait") //nolint:errcheck // best-effort cleanup
		helpers.RunGC(env, "", "supervisor", "stop", "--wait")    //nolint:errcheck // best-effort cleanup
	})
	out, err := helpers.RunGC(preEnv, "", "init", "--skip-provider-readiness", "--no-start", "--provider", "claude",
		"--dolt-host", upstream.Host, "--dolt-port", upstream.Port,
		"--dolt-database", upstream.Database, "--dolt-project-id", upstream.ProjectID, dir)
	if err != nil {
		// A loaded host can blow the 2s `dolt config` identity probe. gc init
		// has created the city by then and only startup is blocked, which the
		// first start (retried on the same symptom) completes.
		if !strings.Contains(out, splitDoltProbeTimeout) {
			t.Fatalf("gc init --dolt-host: %v\n%s", err, out)
		}
		t.Logf("gc init hit the dolt identity probe timeout; gc start completes it:\n%s", out)
	}
	c := &splitE2ECity{t: t, env: env, city: city, root: root, dir: dir, preEnv: preEnv, standalone: preBin != ""}
	t.Cleanup(c.stopStandaloneController)
	c.reduceToScriptedWorker(scriptPath, bornSplit)
	if err := os.WriteFile(c.path("formulas", splitFormulaName+".toml"), []byte(splitFormulaTOML), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	return c
}

// splitIsolatedUserDirs points every per-user state directory a gc, bd, dolt
// or agent could reach at the test root: Claude's config dir, the XDG base
// directories and Dolt's root. HOME itself is already isolated by
// helpers.NewEnv; these close the paths that do not go through HOME.
func splitIsolatedUserDirs(t *testing.T, env *helpers.Env, root string) *helpers.Env {
	t.Helper()
	dirs := map[string]string{
		"CLAUDE_CONFIG_DIR": filepath.Join(root, "user", "claude"),
		"XDG_CONFIG_HOME":   filepath.Join(root, "user", "config"),
		"XDG_DATA_HOME":     filepath.Join(root, "user", "data"),
		"XDG_STATE_HOME":    filepath.Join(root, "user", "state"),
		"XDG_CACHE_HOME":    filepath.Join(root, "user", "cache"),
		"DOLT_ROOT_PATH":    env.Get("GC_HOME"),
	}
	for key, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
		env.With(key, dir)
	}
	return env
}

// splitBinaryEnv is env with bin as the gc every child resolves: the harness's
// pinned binary and the first `gc` on PATH, so the supervisor, the sessions it
// spawns and the worker's own `gc hook`/`gc bd` calls all run bin.
func splitBinaryEnv(t *testing.T, env *helpers.Env, bin, linkDir string) *helpers.Env {
	t.Helper()
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatalf("create %s: %v", linkDir, err)
	}
	// A real file, not a symlink: gc puts the directory of its resolved
	// executable first on every session's PATH, and a symlink resolves back
	// to bin's own directory and whatever `gc` sits there.
	link := filepath.Join(linkDir, "gc")
	if err := os.Link(bin, link); err != nil {
		if err := copySplitBinary(bin, link); err != nil {
			t.Fatalf("install %s as %s: %v", bin, link, err)
		}
	}
	return env.Clone().
		With("GC_ACCEPTANCE_GC_BIN", link).
		With("PATH", linkDir+string(os.PathListSeparator)+env.Get("PATH"))
}

func copySplitBinary(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// handOver retires the pre-cutover binary's supervisor with that binary and
// gives the city to the binary under test. The order matters: a supervisor is
// stopped by the build that started it, before the next build starts its own
// against the same GC_HOME.
func (c *splitE2ECity) handOver() {
	c.t.Helper()
	if c.controller != nil {
		c.t.Fatalf("hand-over with the pre-cutover controller still running")
	}
	if !c.standalone {
		if out, err := helpers.RunGC(c.preEnv, "", "supervisor", "stop", "--wait"); err != nil {
			c.t.Logf("stopping the pre-cutover supervisor: %v\n%s", err, out)
		}
	}
	c.city.Env = c.env
	c.preEnv = c.env
	c.standalone = false
}

// splitLegacyDoltPort is the host's shared Dolt default, which no acceptance
// run may ever bind or dial.
const splitLegacyDoltPort = 3307

var (
	splitDoltSectionRE  = regexp.MustCompile(`(?ms)^\[dolt\]\n.*?(?:\n\n|\z)`)
	splitImportGCRE     = regexp.MustCompile(`(?ms)^\[imports\.gc\]\n(?:[^\[\n][^\n]*\n)*`)
	splitNamedSessionRE = regexp.MustCompile(`(?ms)^\[\[named_session\]\]\n(?:[^\[\n][^\n]*\n?)*`)
)

// reduceToScriptedWorker turns the `gc init --dolt-host` scaffold into a city
// that runs nothing but the scripted worker: the canonical [dolt] endpoint gc
// init wrote is kept verbatim, the inference-backed default role pack and its
// always-on session are dropped, and the worker becomes the one named session.
func (c *splitE2ECity) reduceToScriptedWorker(scriptPath string, bornSplit bool) {
	c.t.Helper()
	cityTOML := c.city.ReadFile("city.toml")
	dolt := splitDoltSectionRE.FindString(cityTOML)
	if dolt == "" {
		c.t.Fatalf("gc init --dolt-host wrote no [dolt] endpoint:\n%s", cityTOML)
	}
	if regexp.MustCompile(`(?m)^port\s*=\s*` + strconv.Itoa(splitLegacyDoltPort) + `\s*$`).MatchString(dolt) {
		c.t.Fatalf("the city's Dolt endpoint is port %d, the host's shared default:\n%s", splitLegacyDoltPort, dolt)
	}
	cfg := "[workspace]\n\n" + strings.TrimSpace(dolt) + `

[session]
provider = "subprocess"

[daemon]
formula_v2 = true
patrol_interval = "200ms"
start_ready_timeout = "10m"
`
	if bornSplit {
		cfg += splitStorageTOML
	}
	c.city.WriteConfig(cfg)

	pack := c.city.ReadFile("pack.toml")
	pack = splitImportGCRE.ReplaceAllString(pack, "")
	pack = splitNamedSessionRE.ReplaceAllString(pack, "")
	pack = strings.TrimRight(pack, "\n") + fmt.Sprintf(`

[[agent]]
name = "worker"
scope = "city"
max_active_sessions = 1
start_command = %q

[[named_session]]
template = "worker"
mode = "always"
`, "bash "+scriptPath)
	if err := os.WriteFile(c.path("pack.toml"), []byte(pack), 0o644); err != nil {
		c.t.Fatalf("write pack.toml: %v", err)
	}
	if err := os.RemoveAll(c.path("agents")); err != nil {
		c.t.Fatalf("remove scaffolded agents: %v", err)
	}
}

func (c *splitE2ECity) path(rel ...string) string {
	return filepath.Join(append([]string{c.dir}, rel...)...)
}

// runFirstHalf boots the city, slings the formula, waits for the worker to
// close splitStepsBeforeStop steps, and stops the city. It returns the
// workflow root id.
func (c *splitE2ECity) runFirstHalf() string {
	c.t.Helper()
	c.setBudget(splitStepsBeforeStop)
	c.start()

	out, err := c.city.GCStdout("sling", "worker", splitFormulaName, "--formula", "--json")
	if err != nil {
		c.t.Fatalf("gc sling worker %s --formula: %v\n%s", splitFormulaName, err, out)
	}
	var slung struct {
		WorkflowID string `json:"workflow_id"`
		BeadID     string `json:"bead_id"`
	}
	if err := json.Unmarshal([]byte(lastJSONObject(out)), &slung); err != nil {
		c.t.Fatalf("decode gc sling --json: %v\n%s", err, out)
	}
	root := slung.WorkflowID
	if root == "" {
		root = slung.BeadID
	}
	if root == "" {
		c.t.Fatalf("gc sling returned no workflow id:\n%s", out)
	}

	if !c.city.WaitForCondition(func() bool {
		return len(c.workerLog().closed) >= splitStepsBeforeStop && c.budget() == 0
	}, splitPhaseTimeout()) {
		c.dumpDiagnostics(root)
		c.t.Fatalf("worker did not close %d steps within %s", splitStepsBeforeStop, splitPhaseTimeout())
	}
	c.stop()
	return root
}

// splitDoltProbeTimeout is gc's 2s `dolt config` identity probe giving up on
// a loaded host; it says nothing about the city.
const splitDoltProbeTimeout = "dolt config probe timed out"

// start boots the city under its own supervisor.
func (c *splitE2ECity) start() {
	c.t.Helper()
	if c.standalone {
		c.startStandaloneController()
		return
	}
	helpers.RunGC(c.city.Env, "", "supervisor", "stop", "--wait") //nolint:errcheck // a stale supervisor must not carry an old env
	if out, err := c.tryStart(); err != nil {
		c.t.Fatalf("gc start: %v\n%s", err, out)
	}
}

// tryStart runs gc start, retrying only the dolt identity probe timeout.
func (c *splitE2ECity) tryStart() (string, error) {
	var out string
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		out, err = c.city.GC("start", c.dir)
		if err == nil || !strings.Contains(out, splitDoltProbeTimeout) {
			return out, err
		}
		c.t.Logf("gc start attempt %d hit the dolt identity probe timeout; retrying", attempt)
	}
	return out, err
}

func (c *splitE2ECity) stop() {
	c.t.Helper()
	if out, err := c.city.GC("stop", c.dir); err != nil {
		c.t.Fatalf("gc stop: %v\n%s", err, out)
	}
	if c.controller != nil {
		select {
		case <-c.controllerDone:
		case <-time.After(splitControllerStopTimeout):
			c.t.Fatalf("the standalone controller did not exit within %s of gc stop", splitControllerStopTimeout)
		}
		c.controller = nil
	}
}

// splitControllerStopTimeout bounds how long a standalone controller may take
// to exit after `gc stop`.
const splitControllerStopTimeout = time.Minute

// startStandaloneController runs `gc start --foreground` in the background,
// which never touches a supervisor, and waits for the controller to answer.
func (c *splitE2ECity) startStandaloneController() {
	c.t.Helper()
	gcPath, err := helpers.ResolveGCPath(c.city.Env)
	if err != nil {
		c.t.Fatalf("resolve gc: %v", err)
	}
	logPath := filepath.Join(c.root, "standalone-controller.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		c.t.Fatalf("open controller log: %v", err)
	}
	cmd := exec.Command(gcPath, "start", "--foreground", c.dir)
	cmd.Dir = c.dir
	cmd.Env = c.city.Env.List()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		c.t.Fatalf("gc start --foreground: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
		close(done)
	}()
	c.controller, c.controllerDone = cmd, done
	if !c.city.WaitForCondition(func() bool {
		select {
		case <-done:
			return true
		default:
		}
		_, err := c.city.GC("status", "--city", c.dir)
		return err == nil && c.events().counts["controller.started"] > c.controllerStarts()
	}, 2*time.Minute) {
		log, _ := os.ReadFile(logPath)
		c.t.Fatalf("the standalone controller did not come up within 2m:\n%s", log)
	}
	select {
	case <-done:
		log, _ := os.ReadFile(logPath)
		c.t.Fatalf("the standalone controller exited during startup:\n%s", log)
	default:
	}
	c.startedControllers++
}

// controllerStarts is how many controller.started events this test has
// already accounted for.
func (c *splitE2ECity) controllerStarts() int { return c.startedControllers }

// stopStandaloneController is the cleanup for a standalone controller the
// test did not stop itself.
func (c *splitE2ECity) stopStandaloneController() {
	if c.controller == nil {
		return
	}
	helpers.RunGC(c.city.Env, c.dir, "stop", c.dir) //nolint:errcheck // best-effort cleanup
	select {
	case <-c.controllerDone:
	case <-time.After(splitControllerStopTimeout):
		if c.controller.Process != nil {
			_ = c.controller.Process.Kill()
		}
		<-c.controllerDone
	}
	c.controller = nil
}

func (c *splitE2ECity) authorSplit() {
	c.t.Helper()
	c.city.AppendToConfig(splitStorageTOML)
}

// migrate runs the cutover with whichever binary currently owns the city and
// returns its output.
func (c *splitE2ECity) migrate() string {
	c.t.Helper()
	bin, err := helpers.ResolveGCPath(c.city.Env)
	if err != nil {
		c.t.Fatalf("resolve gc: %v", err)
	}
	out, err := c.city.GC("storage", "migrate", "--from-work", "--fleet-stopped")
	if err != nil {
		c.t.Fatalf("%s storage migrate --from-work --fleet-stopped: %v\n%s", bin, err, out)
	}
	c.t.Logf("%s storage migrate:\n%s", bin, out)
	return out
}

// expectBootRefusalNamingRepair starts the rc1-migrated city with this build
// and requires the boot to refuse and name the repair command.
func (c *splitE2ECity) expectBootRefusalNamingRepair() {
	c.t.Helper()
	out, err := c.tryStart()
	// Whatever the verdict, the next step needs a stopped city.
	defer func() {
		if stopOut, stopErr := c.city.GC("stop", c.dir); stopErr != nil {
			c.t.Logf("gc stop after the refused boot: %v\n%s", stopErr, stopOut)
		}
	}()
	if err == nil {
		c.t.Fatalf("gc start served a city migrated by v1.5.0-rc1 whose work store still holds the relocated copies; want a refusal naming %q\n%s", splitRepairCommand, out)
	}
	if !strings.Contains(out, splitRepairCommand) {
		c.t.Fatalf("gc start refused the rc1-migrated city without naming %q:\n%s", splitRepairCommand, out)
	}
	c.t.Logf("gc start refused the rc1-migrated city as required:\n%s", out)
}

// finishAndAssert lifts the budget, starts the city, waits for the root to
// close (or for the re-dispatch loop to show itself), and asserts the run.
func (c *splitE2ECity) finishAndAssert(root string) {
	c.t.Helper()
	if err := os.Remove(c.path(".gc", "split-e2e-budget")); err != nil && !os.IsNotExist(err) {
		c.t.Fatalf("lift worker budget: %v", err)
	}
	c.start()

	deadline := time.Now().Add(splitPhaseTimeout())
	for {
		if v := c.violations(); len(v) > 0 {
			// Give the loop a moment to repeat so the evidence is unambiguous.
			time.Sleep(3 * time.Second)
			c.dumpDiagnostics(root)
			c.t.Fatalf("formula re-dispatch after the cutover:\n  %s", strings.Join(c.violations(), "\n  "))
		}
		if c.status(root) == "closed" {
			break
		}
		if time.Now().After(deadline) {
			c.dumpDiagnostics(root)
			c.t.Fatalf("workflow root %s did not close within %s", root, splitPhaseTimeout())
		}
		time.Sleep(time.Second)
	}

	if v := c.violations(); len(v) > 0 {
		c.dumpDiagnostics(root)
		c.t.Fatalf("formula re-dispatch after the cutover:\n  %s", strings.Join(v, "\n  "))
	}
	wl := c.workerLog()
	if len(wl.closed) != splitWorkerSteps {
		c.dumpDiagnostics(root)
		c.t.Fatalf("worker closed %d distinct steps, want %d: %v", len(wl.closed), splitWorkerSteps, wl.closed)
	}
	for id, n := range wl.closed {
		if n != 1 {
			c.t.Errorf("step %s closed %d times, want exactly once", id, n)
		}
	}
	steps := c.events().stepsOf(root)
	if len(steps) < splitWorkerSteps {
		c.t.Fatalf("events define %d steps for %s, want at least %d", len(steps), root, splitWorkerSteps)
	}
	for _, id := range steps {
		if got := c.status(id); got != "closed" {
			c.t.Errorf("step %s is %q after the root closed, want closed", id, got)
		}
	}

	statusOut, err := c.city.GC("storage", "status")
	if err != nil {
		c.t.Fatalf("gc storage status exited non-zero on the finished city: %v\n%s", err, statusOut)
	}
	m := regexp.MustCompile(`(?m)^source:\s+(\d+)`).FindStringSubmatch(statusOut)
	if m == nil {
		c.t.Fatalf("gc storage status printed no source: census:\n%s", statusOut)
	}
	if m[1] != "0" {
		c.t.Errorf("work store still holds %s infrastructure bead(s) after the cutover, want 0:\n%s", m[1], statusOut)
	}
	ev := c.events()
	c.t.Logf("root %s closed; steps %v; worker closes %v; step_started %v; claim_rejected=%d dead_assignee_reopened=%d; status source=%s",
		root, steps, wl.closed, ev.started, ev.counts["bead.claim_rejected"], ev.counts["bead.dead_assignee_reopened"], m[1])
}

// violations reports every sign of re-dispatch in the event log and the
// worker's own log.
func (c *splitE2ECity) violations() []string {
	var out []string
	ev := c.events()
	for _, subject := range sortedKeys(ev.startedAfterClose) {
		out = append(out, fmt.Sprintf("execution.step_started on already-closed step %s (%d times)", subject, ev.startedAfterClose[subject]))
	}
	for _, subject := range sortedKeys(ev.started) {
		if n := ev.started[subject]; n > 1 {
			out = append(out, fmt.Sprintf("execution.step_started %d times for step %s", n, subject))
		}
	}
	for _, typ := range []string{"bead.claim_rejected", "bead.dead_assignee_reopened"} {
		if n := ev.counts[typ]; n > 0 {
			out = append(out, fmt.Sprintf("%s fired %d times (subjects %v)", typ, n, ev.subjects[typ]))
		}
	}
	wl := c.workerLog()
	for _, id := range sortedKeys(wl.claimed) {
		if n := wl.claimed[id]; n > 1 {
			out = append(out, fmt.Sprintf("worker claimed %s %d times", id, n))
		}
	}
	return out
}

type splitEventSummary struct {
	counts            map[string]int
	subjects          map[string][]string
	started           map[string]int
	startedAfterClose map[string]int
	stepDefined       []splitEvent
}

type splitEvent struct {
	Seq     int64  `json:"seq"`
	Type    string `json:"type"`
	Subject string `json:"subject"`
	RunID   string `json:"run_id"`
	StepID  string `json:"step_id"`
}

// events reads the city's event log in sequence order.
func (c *splitE2ECity) events() splitEventSummary {
	s := splitEventSummary{
		counts:            map[string]int{},
		subjects:          map[string][]string{},
		started:           map[string]int{},
		startedAfterClose: map[string]int{},
	}
	f, err := os.Open(c.path(".gc", "events.jsonl"))
	if err != nil {
		return s
	}
	defer f.Close() //nolint:errcheck // read-only
	var all []splitEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var e splitEvent
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Type != "" {
			all = append(all, e)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Seq < all[j].Seq })
	closed := map[string]bool{}
	for _, e := range all {
		s.counts[e.Type]++
		switch e.Type {
		case "bead.claim_rejected", "bead.dead_assignee_reopened":
			s.subjects[e.Type] = append(s.subjects[e.Type], e.Subject)
		case "execution.step_defined":
			s.stepDefined = append(s.stepDefined, e)
		case "execution.step_completed", "bead.closed":
			closed[e.Subject] = true
		case "execution.step_started":
			s.started[e.Subject]++
			if closed[e.Subject] {
				s.startedAfterClose[e.Subject]++
			}
		}
	}
	return s
}

// stepsOf returns the step bead ids the controller defined for root.
func (s splitEventSummary) stepsOf(root string) []string {
	seen := map[string]bool{}
	var ids []string
	for _, e := range s.stepDefined {
		if e.RunID != root || e.Subject == "" || seen[e.Subject] {
			continue
		}
		seen[e.Subject] = true
		ids = append(ids, e.Subject)
	}
	return ids
}

type splitWorkerLog struct {
	claimed map[string]int
	closed  map[string]int
	raw     string
}

func (c *splitE2ECity) workerLog() splitWorkerLog {
	wl := splitWorkerLog{claimed: map[string]int{}, closed: map[string]int{}}
	data, err := os.ReadFile(c.path(".gc", "split-e2e-worker.log"))
	if err != nil {
		return wl
	}
	wl.raw = string(data)
	for _, line := range strings.Split(wl.raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		switch fields[1] {
		case "claimed":
			wl.claimed[fields[2]]++
		case "closed":
			wl.closed[fields[2]]++
		}
	}
	return wl
}

func (c *splitE2ECity) setBudget(n int) {
	c.t.Helper()
	if err := os.WriteFile(c.path(".gc", "split-e2e-budget"), []byte(strconv.Itoa(n)+"\n"), 0o644); err != nil {
		c.t.Fatalf("write worker budget: %v", err)
	}
}

func (c *splitE2ECity) budget() int {
	data, err := os.ReadFile(c.path(".gc", "split-e2e-budget"))
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return -1
	}
	return n
}

// status reads a bead's status through gc bd, which answers a relocated id
// from the binding and a work id from the work store. "" means unreadable.
func (c *splitE2ECity) status(id string) string {
	out, err := c.city.GCStdout("bd", "show", id, "--json")
	if err != nil {
		return ""
	}
	payload := strings.TrimSpace(out)
	if i := strings.IndexAny(payload, "[{"); i >= 0 {
		payload = payload[i:]
	}
	var one struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(payload), &one) == nil && one.Status != "" {
		return one.Status
	}
	var many []struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(payload), &many) == nil && len(many) > 0 {
		return many[0].Status
	}
	return ""
}

func (c *splitE2ECity) dumpDiagnostics(root string) {
	c.t.Helper()
	ev := c.events()
	c.t.Logf("=== split-e2e diagnostics for %s (root %s) ===", c.dir, root)
	c.t.Logf("event counts: %v", ev.counts)
	c.t.Logf("step_started per subject: %v", ev.started)
	wl := c.workerLog()
	lines := strings.Split(strings.TrimSpace(wl.raw), "\n")
	var marks []string
	for _, l := range lines {
		if f := strings.Fields(l); len(f) >= 2 && (f[1] == "claimed" || f[1] == "closed" || f[1] == "close-failed" || f[1] == "start") {
			marks = append(marks, l)
		}
	}
	if len(marks) > 60 {
		marks = marks[len(marks)-60:]
	}
	c.t.Logf("worker log (claims/closes):\n%s", strings.Join(marks, "\n"))
	// Everything else the worker logged is its gc calls' stderr; the tail is
	// what explains a worker that started and never claimed.
	var other []string
	for _, l := range lines {
		if f := strings.Fields(l); len(f) >= 2 && (f[1] == "claimed" || f[1] == "closed" || f[1] == "close-failed" || f[1] == "start") {
			continue
		}
		if strings.TrimSpace(l) != "" {
			other = append(other, l)
		}
	}
	if len(other) > 15 {
		other = other[len(other)-15:]
	}
	if len(other) > 0 {
		c.t.Logf("worker log (gc stderr tail):\n%s", strings.Join(other, "\n"))
	}
	out, err := c.city.GC("storage", "status")
	c.t.Logf("gc storage status (err=%v):\n%s", err, out)
}

func lastJSONObject(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "{") {
			return l
		}
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// splitRunbookPath is the operator runbook whose restore recipe the rollback
// leg runs verbatim, so the documented commands are what is tested.
const splitRunbookPath = "docs/runbooks/split-storage-classes.md"

// splitRestoreSection is the runbook heading the recipe lives under.
const splitRestoreSection = "### Restoring the work store from the backup"

// splitWorkOnlyStorageTOML is the documented rollback class map: every class
// on `work`, and no binding block.
const splitWorkOnlyStorageTOML = `
[storage.classes]
work = "work"
graph = "work"
sessions = "work"
messaging = "work"
orders = "work"
nudges = "work"
`

// rollbackAndRestoreFromBackup is the runbook's rollback after the clear, end
// to end: revert the class map, watch the boot refuse because the work store
// no longer holds the infrastructure state, remove the notes, run the
// runbook's restore recipe against the backup the migration wrote, and boot.
func (c *splitE2ECity) rollbackAndRestoreFromBackup(root string) {
	c.t.Helper()
	backup := c.path(".gc", "store", "infra.retained-source.jsonl")
	rows := readSplitBackup(c.t, backup)

	// (1) stop; (2) point every class back at work and drop the binding.
	c.stop()
	cfg := c.city.ReadFile("city.toml")
	i := strings.Index(cfg, "\n[storage.classes]")
	if i < 0 {
		c.t.Fatalf("city.toml has no [storage.classes] to revert:\n%s", cfg)
	}
	c.city.WriteConfig(cfg[:i] + splitWorkOnlyStorageTOML)

	// (3) the boot holds the reverted city: its work store has nothing.
	out, err := c.tryStart()
	if err == nil {
		c.t.Fatalf("gc start served a reverted city whose infrastructure state lives only in the backup:\n%s", out)
	}
	for _, want := range []string{"NO infrastructure state", "Restoring the work store from the backup"} {
		if !strings.Contains(out, want) {
			c.t.Errorf("reverted-city boot refusal does not mention %q:\n%s", want, out)
		}
	}
	c.t.Logf("gc start refused the reverted city as required:\n%s", out)
	if stopOut, stopErr := c.city.GC("stop", c.dir); stopErr != nil {
		c.t.Logf("gc stop after the refused boot: %v\n%s", stopErr, stopOut)
	}

	// (4) the operator's attestation: remove the notes.
	for _, note := range []string{"storage-infra-cleared.json", "storage-served-binding.json"} {
		if err := os.Remove(c.path(".gc", note)); err != nil {
			c.t.Fatalf("remove .gc/%s: %v", note, err)
		}
	}

	// (5) the runbook's recipe, as written.
	recipe := splitRunbookRestoreRecipe(c.t, backup, filepath.Join(c.root, "restore"))
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", recipe)
	cmd.Dir = c.dir
	cmd.Env = c.city.Env.List()
	recipeOut, err := cmd.CombinedOutput()
	if err != nil {
		c.t.Fatalf("the runbook's restore recipe failed: %v\n--- recipe ---\n%s\n--- output ---\n%s", err, recipe, recipeOut)
	}
	c.t.Logf("runbook restore recipe:\n%s", recipeOut)

	// (6) the city boots on its work store and serves the restored rows.
	c.start()
	rootRow, ok := rows[root]
	if !ok {
		c.t.Fatalf("backup %s does not hold the workflow root %s", backup, root)
	}
	got := c.show(root)
	if got.Status != rootRow.Status {
		c.t.Errorf("restored root %s status = %q, backup holds %q", root, got.Status, rootRow.Status)
	}
	for k, v := range rootRow.Metadata {
		if got.Metadata[k] != v {
			c.t.Errorf("restored root %s metadata %s = %q, backup holds %q", root, k, got.Metadata[k], v)
		}
	}
	// A restored step keeps its links: the outbound edges the backup recorded
	// for it (graph.v2 steps reach their root through these, not a parent),
	// and its parent when it has one.
	stepChecked := false
	for _, id := range sortedKeys(rows) {
		row := rows[id]
		if id == root || row.Metadata["gc.root_bead_id"] != root || len(row.Deps) == 0 {
			continue
		}
		step := c.show(id)
		for _, dep := range row.Deps {
			if !step.hasDep(dep.DependsOnID, dep.Type) {
				c.t.Errorf("restored step %s lost its edge -[%s]-> %s; show gives deps %+v", id, dep.Type, dep.DependsOnID, step.Dependencies)
			}
		}
		if row.Parent != "" && step.Parent != row.Parent && !step.hasDep(row.Parent, "parent-child") {
			c.t.Errorf("restored step %s lost its parent %s: show gives parent %q, deps %+v", id, row.Parent, step.Parent, step.Dependencies)
		}
		c.t.Logf("restored step %s: status %q, parent %q, deps %+v", id, step.Status, step.Parent, step.Dependencies)
		stepChecked = true
		break
	}
	if !stepChecked {
		c.t.Errorf("backup %s holds no step of %s with recorded edges; nothing proves the restore keeps a step's links", backup, root)
	}
	c.t.Logf("rollback restore: %d backup row(s); root %s restored as %q with %d metadata key(s)", len(rows), root, got.Status, len(got.Metadata))
}

// splitBackupBead is the part of a backup row the rollback leg compares.
type splitBackupBead struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Parent   string            `json:"parent"`
	Metadata map[string]string `json:"metadata"`
	// Deps is the record's outbound edges, lifted from the entry beside the
	// bead.
	Deps []splitBackupEdge `json:"-"`
}

// splitBackupEdge is one edge as the backup records it.
type splitBackupEdge struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
}

func readSplitBackup(t *testing.T, path string) map[string]splitBackupBead {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration backup: %v", err)
	}
	rows := map[string]splitBackupBead{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec struct {
			Bead *splitBackupBead  `json:"bead"`
			Deps []splitBackupEdge `json:"deps"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Bead != nil && rec.Bead.ID != "" {
			rec.Bead.Deps = rec.Deps
			rows[rec.Bead.ID] = *rec.Bead
		}
	}
	if len(rows) == 0 {
		t.Fatalf("migration backup %s holds no bead rows", path)
	}
	return rows
}

// splitRunbookRestoreRecipe returns the first bash block under the runbook's
// restore heading, with the backup path filled in and its scratch files moved
// from /tmp into scratch.
func splitRunbookRestoreRecipe(t *testing.T, backup, scratch string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(helpers.FindModuleRoot(), splitRunbookPath))
	if err != nil {
		t.Fatalf("read runbook: %v", err)
	}
	doc := string(data)
	i := strings.Index(doc, splitRestoreSection)
	if i < 0 {
		t.Fatalf("%s has no %q section", splitRunbookPath, splitRestoreSection)
	}
	section := doc[i:]
	start := strings.Index(section, "```bash\n")
	if start < 0 {
		t.Fatalf("%s %q has no bash block", splitRunbookPath, splitRestoreSection)
	}
	body := section[start+len("```bash\n"):]
	end := strings.Index(body, "\n```")
	if end < 0 {
		t.Fatalf("%s %q has an unterminated bash block", splitRunbookPath, splitRestoreSection)
	}
	recipe := body[:end]
	const placeholder = "B=<binding root>/infra.retained-source.jsonl"
	if !strings.Contains(recipe, placeholder) {
		t.Fatalf("the runbook recipe no longer sets %q; update this test with it:\n%s", placeholder, recipe)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatalf("create %s: %v", scratch, err)
	}
	recipe = strings.Replace(recipe, placeholder, "B="+strconv.Quote(backup), 1)
	return strings.ReplaceAll(recipe, "/tmp/gc-restore", filepath.Join(scratch, "gc-restore"))
}

// splitShownBead is the part of `gc bd show --json` the rollback leg reads.
type splitShownBead struct {
	Status       string            `json:"status"`
	Parent       string            `json:"parent"`
	Metadata     map[string]string `json:"metadata"`
	Dependencies []struct {
		DependsOnID string `json:"depends_on_id"`
		ID          string `json:"id"`
		Type        string `json:"type"`
		DepType     string `json:"dependency_type"`
	} `json:"dependencies"`
}

func (b splitShownBead) hasDep(on, typ string) bool {
	for _, d := range b.Dependencies {
		if (d.DependsOnID == on || d.ID == on) && (d.Type == typ || d.DepType == typ) {
			return true
		}
	}
	return false
}

// show reads one bead through gc bd and fails the test if it is unreadable.
func (c *splitE2ECity) show(id string) splitShownBead {
	c.t.Helper()
	out, err := c.city.GCStdout("bd", "show", id, "--json")
	if err != nil {
		c.t.Fatalf("gc bd show %s --json: %v\n%s", id, err, out)
	}
	payload := strings.TrimSpace(out)
	if i := strings.IndexAny(payload, "[{"); i >= 0 {
		payload = payload[i:]
	}
	var many []splitShownBead
	if json.Unmarshal([]byte(payload), &many) == nil && len(many) > 0 {
		return many[0]
	}
	var one splitShownBead
	if err := json.Unmarshal([]byte(payload), &one); err != nil {
		c.t.Fatalf("decode gc bd show %s --json: %v\n%s", id, err, out)
	}
	return one
}

// splitCrossEdge is an edge from a work bead into an infrastructure bead the
// migration moves out of the work store.
type splitCrossEdge struct {
	work, target, kind string
}

// String is the spelling the migration, `gc storage status` and the event all
// use for an edge: "issue -[type]-> depends_on".
func (e splitCrossEdge) String() string {
	return fmt.Sprintf("%s -[%s]-> %s", e.work, e.kind, e.target)
}

// addWorkEdgeInto creates a work bead on the stopped, pre-cutover city with a
// tracks edge into target (the still-open workflow root): the shape of a work
// convoy tracking a run.
func (c *splitE2ECity) addWorkEdgeInto(target string) splitCrossEdge {
	c.t.Helper()
	out, err := c.city.GCStdout("bd", "create", "--json", "--type", "task", "split-e2e work bead tracking the run")
	if err != nil {
		c.t.Fatalf("gc bd create: %v\n%s", err, out)
	}
	var created struct {
		ID string `json:"id"`
	}
	payload := strings.TrimSpace(out)
	if i := strings.Index(payload, "{"); i >= 0 {
		payload = payload[i:]
	}
	if err := json.Unmarshal([]byte(payload), &created); err != nil || created.ID == "" {
		c.t.Fatalf("decode gc bd create --json: %v\n%s", err, out)
	}
	edge := splitCrossEdge{work: created.ID, target: target, kind: "tracks"}
	if out, err := c.city.GC("bd", "dep", "add", edge.work, edge.target, "--type", edge.kind); err != nil {
		c.t.Fatalf("gc bd dep add %s: %v\n%s", edge, err, out)
	}
	if !c.show(edge.work).hasDep(edge.target, edge.kind) {
		c.t.Fatalf("work bead %s does not carry the edge %s it was just given", edge.work, edge)
	}
	return edge
}

// assertEdgeAtRiskBeforeClear requires the migration to name the edge under
// AT RISK before it reports removing any row.
func (c *splitE2ECity) assertEdgeAtRiskBeforeClear(out string, edge splitCrossEdge) {
	c.t.Helper()
	risk := strings.Index(out, "cross-store edges AT RISK")
	if risk < 0 {
		c.t.Fatalf("gc storage migrate did not announce the cross-store edges AT RISK:\n%s", out)
	}
	at := strings.Index(out[risk:], edge.String())
	if at < 0 {
		c.t.Fatalf("gc storage migrate's AT RISK list does not name %s:\n%s", edge, out)
	}
	if cleared := strings.Index(out, "cop(ies) cleared"); cleared >= 0 && cleared < risk+at {
		c.t.Errorf("gc storage migrate named %s AT RISK only after reporting the clear:\n%s", edge, out)
	}
}

// assertEdgeLost pins what a bd/Dolt work store does with the edge once its
// target is cleared: drops it, and says so in `gc storage status` and in the
// storage.binding.converged event.
func (c *splitE2ECity) assertEdgeLost(edge splitCrossEdge) {
	c.t.Helper()
	if c.show(edge.work).hasDep(edge.target, edge.kind) {
		c.t.Errorf("work bead %s still carries %s after its target was cleared; this test pins the bd/Dolt work store dropping it", edge.work, edge)
	}
	status, err := c.city.GC("storage", "status")
	if err != nil {
		c.t.Fatalf("gc storage status: %v\n%s", err, status)
	}
	lost := strings.Index(status, "lost cross-store edges:")
	if lost < 0 || !strings.Contains(status[lost:], edge.String()) {
		c.t.Errorf("gc storage status does not list %s under lost cross-store edges:\n%s", edge, status)
	}
	found := false
	for _, e := range c.convergedEvents() {
		for _, l := range e.LostCrossEdges {
			if strings.HasPrefix(l, edge.String()) {
				found = true
			}
		}
	}
	if !found {
		c.t.Errorf("no storage.binding.converged event carries %s in lost_cross_edges: %+v", edge, c.convergedEvents())
	}
}

// assertEdgeRestored requires the runbook recipe's dep-add loop to have put
// the edge back.
func (c *splitE2ECity) assertEdgeRestored(edge splitCrossEdge) {
	c.t.Helper()
	got := c.show(edge.work)
	if !got.hasDep(edge.target, edge.kind) {
		c.t.Errorf("the runbook restore did not put back %s: gc bd show %s gives deps %+v", edge, edge.work, got.Dependencies)
	}
}

// splitConvergedEvent is the part of a storage.binding.converged event the
// cross-edge assertions read.
type splitConvergedEvent struct {
	Seq            int64    `json:"seq"`
	LostCrossEdges []string `json:"lost_cross_edges"`
}

func (c *splitE2ECity) convergedEvents() []splitConvergedEvent {
	data, err := os.ReadFile(c.path(".gc", "events.jsonl"))
	if err != nil {
		return nil
	}
	var out []splitConvergedEvent
	for _, line := range strings.Split(string(data), "\n") {
		var e struct {
			Seq     int64               `json:"seq"`
			Type    string              `json:"type"`
			Payload splitConvergedEvent `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &e) != nil || e.Type != "storage.binding.converged" {
			continue
		}
		e.Payload.Seq = e.Seq
		out = append(out, e.Payload)
	}
	return out
}
