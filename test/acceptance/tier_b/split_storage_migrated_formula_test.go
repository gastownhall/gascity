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
//   - MigratedByV150RC1: the same, but the migration is run by a v1.5.0-rc1
//     gc (GC_ACCEPTANCE_SPLIT_RC1_GC_BIN). Booting that city with this binary
//     must refuse and name the repair command; running it repairs the city.
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
//	make test-acceptance-split-storage \
//	  GC_ACCEPTANCE_BD_BIN=/path/to/bd-1.3.1 \
//	  GC_ACCEPTANCE_SPLIT_RC1_GC_BIN=/path/to/gc-v1.5.0-rc1
package tierb_test

import (
	"bufio"
	"encoding/json"
	"fmt"
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

	splitPhaseTimeout  = 4 * time.Minute
	splitFinishTimeout = 4 * time.Minute
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
LOG="$GC_CITY/.gc/split-e2e-worker.log"
BUDGET="$GC_CITY/.gc/split-e2e-budget"
log() { printf '%s %s\n' "$(date -u +%H:%M:%S.%N)" "$*" >> "$LOG"; }
log "start session=${GC_SESSION_NAME:-} id=${GC_SESSION_ID:-}"
while true; do
  if [ -f "$BUDGET" ] && [ "$(cat "$BUDGET" 2>/dev/null || echo 0)" -le 0 ]; then
    sleep 0.3
    continue
  fi
  out=$(timeout 60 gc hook --claim --json 2>>"$LOG")
  last=$(printf '%s\n' "$out" | tail -n 1)
  action=$(printf '%s\n' "$last" | jq -r '.action // empty' 2>/dev/null)
  id=$(printf '%s\n' "$last" | jq -r '.bead_id // empty' 2>/dev/null)
  if [ "$action" != "work" ] || [ -z "$id" ]; then
    sleep 0.3
    continue
  fi
  log "claimed $id"
  if timeout 60 gc bd update "$id" --set-metadata gc.outcome=pass --set-metadata gc.work_outcome=no-op --status closed >>"$LOG.bd" 2>&1; then
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
		c := newSplitE2ECity(t, bdPath, doltPath, false)
		root := c.runFirstHalf()
		c.authorSplit()
		c.migrate(c.gcBin)
		c.finishAndAssert(root)
	})

	t.Run("MigratedByV150RC1", func(t *testing.T) {
		rc1 := splitRC1Binary(t)
		c := newSplitE2ECity(t, bdPath, doltPath, false)
		root := c.runFirstHalf()
		c.authorSplit()
		c.migrate(rc1)
		c.expectBootRefusalNamingRepair()
		c.migrate(c.gcBin)
		c.finishAndAssert(root)
	})

	t.Run("BornSplit", func(t *testing.T) {
		c := newSplitE2ECity(t, bdPath, doltPath, true)
		root := c.runFirstHalf()
		c.finishAndAssert(root)
	})
}

// splitRC1Binary returns the v1.5.0-rc1 gc, skipping (or failing under
// splitRequireRC1Env) when it is not configured. A set-but-unusable path fails.
func splitRC1Binary(t *testing.T) string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(splitRC1GCBinEnv))
	if raw == "" {
		reason := fmt.Sprintf("%s is unset; build gc from tag v1.5.0-rc1 and point it there to run the upgraded-city scenario", splitRC1GCBinEnv)
		if v := strings.TrimSpace(os.Getenv(splitRequireRC1Env)); v != "" && v != "0" {
			t.Fatalf("%s is set, so this scenario must run, but %s", splitRequireRC1Env, reason)
		}
		t.Skip(reason)
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

// splitE2ECity is one isolated city plus the paths the assertions read.
type splitE2ECity struct {
	t     *testing.T
	env   *helpers.Env
	city  *helpers.City
	root  string
	dir   string
	gcBin string
}

func newSplitE2ECity(t *testing.T, bdPath, doltPath string, bornSplit bool) *splitE2ECity {
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
	base := helpers.NewEnv(gcBin, gcHome, runtimeDir).With("GC_SESSION", "subprocess")
	env := helpers.TopologyEnv(t, base, root, bdPath, doltPath)

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
	city := helpers.NewCityAt(t, env, dir)
	t.Cleanup(city.CleanupRuntime)
	out, err := helpers.RunGC(env, "", "init", "--skip-provider-readiness", "--no-start", "--provider", "claude",
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
	c := &splitE2ECity{t: t, env: env, city: city, root: root, dir: dir, gcBin: gcBin}
	c.reduceToScriptedWorker(scriptPath, bornSplit)
	if err := os.WriteFile(c.path("formulas", splitFormulaName+".toml"), []byte(splitFormulaTOML), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	return c
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
	}, splitPhaseTimeout) {
		c.dumpDiagnostics(root)
		c.t.Fatalf("worker did not close %d steps within %s", splitStepsBeforeStop, splitPhaseTimeout)
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
	helpers.RunGC(c.env, "", "supervisor", "stop", "--wait") //nolint:errcheck // a stale supervisor must not carry an old env
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
}

func (c *splitE2ECity) authorSplit() {
	c.t.Helper()
	c.city.AppendToConfig(splitStorageTOML)
}

// migrate runs the cutover with bin (this build, or the rc1 build).
func (c *splitE2ECity) migrate(bin string) {
	c.t.Helper()
	cmd := exec.Command(bin, "storage", "migrate", "--from-work", "--fleet-stopped")
	cmd.Dir = c.dir
	cmd.Env = c.env.List()
	out, err := cmd.CombinedOutput()
	if err != nil {
		c.t.Fatalf("%s storage migrate --from-work --fleet-stopped: %v\n%s", bin, err, out)
	}
	c.t.Logf("%s storage migrate:\n%s", filepath.Base(bin), out)
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
}

// finishAndAssert lifts the budget, starts the city, waits for the root to
// close (or for the re-dispatch loop to show itself), and asserts the run.
func (c *splitE2ECity) finishAndAssert(root string) {
	c.t.Helper()
	if err := os.Remove(c.path(".gc", "split-e2e-budget")); err != nil && !os.IsNotExist(err) {
		c.t.Fatalf("lift worker budget: %v", err)
	}
	c.start()

	deadline := time.Now().Add(splitFinishTimeout)
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
			c.t.Fatalf("workflow root %s did not close within %s", root, splitFinishTimeout)
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
