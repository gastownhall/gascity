//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// gcBdLatencyBudget is the wall time the fastest timed call of every `gc bd`
// shape must stay under: the product goal is every gc bd call under 1 s
// (ga-nrnrlr).
//
// The gate is on the fastest call because load on a shared worker only ever
// adds time: the fastest of several calls is the closest reading of what gc
// itself costs, and it moves only when every call of the shape got slower,
// which is what a fixed-cost regression does. Medians drift with worker load,
// and single calls pass the budget on a loaded worker.
const gcBdLatencyBudget = time.Second

// gcBdLatencyRuns is how many timed calls each shape gets, after one untimed
// warm-up call.
const gcBdLatencyRuns = 7

// gcBdLatencyShape is one `gc bd` call shape: an argv run from a cwd.
type gcBdLatencyShape struct {
	name string
	cwd  string // "city" or "rig"
	dir  string
	args func(round int) []string
	// subject, when set, is the bead id the call's stdout must name, so a call
	// that answered fast because it did nothing cannot pass.
	subject func() string
	// observe, when set, checks the call's stdout; a create also hands its new
	// bead to the close that follows it in the same round.
	observe func(t *testing.T, stdout string)
}

// gcBdLatencySample is one timed call: its wall time and the bd children it
// forked.
type gcBdLatencySample struct {
	elapsed  time.Duration
	children []helpers.Invocation
}

// TestGcBdLatencyBudget guards the per-call cost of `gc bd`: real gc and the
// pinned bd against the local Dolt servers of a fresh city and one bound rig.
// This city's calls run at a fraction of the budget, so it catches gross
// regressions only: a fixed cost that pushes a shape's fastest call past the
// budget, the order of a native store open with its preflight or several extra
// bd round trips. It does not reproduce production-size costs (a large config,
// a shared Dolt server, a remote store); the bd children column makes a single
// extra round trip visible without gating on it.
//
// The city is what `gc init` makes by default, so both stores are bd-owned
// proxied-server Dolt and every probe or guard read gc makes is a bd child.
// The table reports each shape's min/median/max and the bd children a recording
// shim in front of the pinned bd on PATH saw. It goes to stderr, not t.Log, so
// a passing run's test.log carries it as well.
func TestGcBdLatencyBudget(t *testing.T) {
	requireDoltIntegration(t)

	env := newIsolatedCommandEnv(t, true)
	cityName := uniqueCityName()
	cityDir := filepath.Join(t.TempDir(), cityName)
	registerIntegrationDoltSQLServerCleanup(t, cityDir)
	configPath := filepath.Join(t.TempDir(), "latency.toml")
	cityToml := fmt.Sprintf("[workspace]\nname = %q\n\n[session]\nprovider = \"subprocess\"\n", cityName)
	if err := os.WriteFile(configPath, []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing city config: %v", err)
	}
	initCityWithManagedDoltRecovery(t, env, configPath, cityDir)
	registerCityCommandEnv(cityDir, env)
	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCDoltWithEnv(env, "", "stop", cityDir)                //nolint:errcheck // best-effort cleanup
		runGCDoltWithEnv(env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best-effort cleanup
	})

	rigDir := filepath.Join(t.TempDir(), "latency-rig-alpha")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}
	registerIntegrationDoltSQLServerCleanup(t, rigDir)
	if out, err := retryOnDoltDirtyTableMigrationRace(func() (string, error) {
		return runGCDoltWithEnv(env, cityDir, "rig", "add", rigDir)
	}); err != nil {
		t.Fatalf("gc rig add: %v\noutput: %s", err, out)
	}

	// Only the timed calls find bd through the recorder: the supervisor, and so
	// the controller's own bd calls, keeps the PATH it was started with.
	recorder := helpers.NewRecordingBD(t, realBDBinary)
	timedEnv := replaceEnv(env, "PATH", prependPath(filepath.Dir(recorder.Path), parseEnvList(env)["PATH"]))

	cityBead := gcBdLatencyCreate(t, timedEnv, cityDir, "latency city bead")
	rigBead := gcBdLatencyCreate(t, timedEnv, rigDir, "latency rig bead")
	shapes := slices.Concat(
		gcBdLatencySameScopeShapes("city", cityDir, cityBead),
		gcBdLatencySameScopeShapes("rig", rigDir, rigBead),
		[]gcBdLatencyShape{
			{name: "show city bead --json (cross-scope)", cwd: "rig", dir: rigDir, args: gcBdLatencyArgs("show", cityBead, "--json"), subject: gcBdLatencyID(cityBead)},
			{name: "update city bead (cross-scope)", cwd: "rig", dir: rigDir, args: func(round int) []string {
				return []string{"update", cityBead, "--set-metadata", fmt.Sprintf("latency.cross=%d", round)}
			}, subject: gcBdLatencyID(cityBead)},
			{name: "show rig bead --json (cross-scope)", cwd: "city", dir: cityDir, args: gcBdLatencyArgs("show", rigBead, "--json"), subject: gcBdLatencyID(rigBead)},
		},
	)

	// Rounds interleave the shapes, so a burst of load on the host lands on
	// every shape alike instead of on whichever one was running. Round 0 is an
	// untimed warm-up that pays the one-time costs later calls amortize: the
	// page cache for the gc and bd binaries and each store's first connect.
	samples := make([][]gcBdLatencySample, len(shapes))
	for round := 0; round <= gcBdLatencyRuns; round++ {
		for i, shape := range shapes {
			sample := gcBdLatencyTimedCall(t, timedEnv, shape, recorder, round)
			if round > 0 {
				samples[i] = append(samples[i], sample)
			}
		}
	}

	fmt.Fprint(os.Stderr, gcBdLatencyTable(cityDir, rigDir, shapes, samples)) //nolint:errcheck // best-effort report
	for i, shape := range shapes {
		if fastest := gcBdLatencyStatsOf(samples[i]).min; fastest >= gcBdLatencyBudget {
			t.Errorf("gc bd %s from the %s cwd: fastest of %d calls took %s, budget %s", shape.name, shape.cwd, len(samples[i]), fastest.Round(time.Millisecond), gcBdLatencyBudget)
		}
	}
}

// gcBdLatencySameScopeShapes is every verb run from a scope's own cwd against
// that scope's store. Each round's close closes the bead its create made.
func gcBdLatencySameScopeShapes(cwd, dir, bead string) []gcBdLatencyShape {
	var created string
	shapes := []gcBdLatencyShape{
		{name: "show " + cwd + " bead --json", args: gcBdLatencyArgs("show", bead, "--json"), subject: gcBdLatencyID(bead)},
		{name: "show " + cwd + " bead", args: gcBdLatencyArgs("show", bead), subject: gcBdLatencyID(bead)},
		{name: "list --limit 20 --json", args: gcBdLatencyArgs("list", "--limit", "20", "--json"), observe: gcBdLatencyListed},
		{name: "update " + cwd + " bead --set-metadata", args: func(round int) []string {
			return []string{"update", bead, "--set-metadata", fmt.Sprintf("latency.round=%d", round)}
		}, subject: gcBdLatencyID(bead)},
		{name: "create --title --json", args: func(round int) []string {
			return []string{"create", "--title", fmt.Sprintf("latency %s create %d", cwd, round), "--json"}
		}, observe: func(t *testing.T, stdout string) { created = gcBdLatencyCreatedID(t, stdout) }},
		{name: "close the bead just created", args: func(int) []string { return []string{"close", created} }, subject: func() string { return created }},
	}
	for i := range shapes {
		shapes[i].cwd, shapes[i].dir = cwd, dir
	}
	return shapes
}

func gcBdLatencyArgs(args ...string) func(int) []string {
	return func(int) []string { return args }
}

func gcBdLatencyID(id string) func() string {
	return func() string { return id }
}

// gcBdLatencyTimedCall runs one `gc bd` call and returns its wall time and the
// bd children it forked.
func gcBdLatencyTimedCall(t *testing.T, env []string, shape gcBdLatencyShape, recorder *helpers.RecordingBD, round int) gcBdLatencySample {
	t.Helper()
	bdArgs := shape.args(round)
	recorder.Reset()
	start := time.Now()
	stdout, err := runCommandStdout(shape.dir, env, integrationGCCommandTimeout, gcBinary, append([]string{"bd"}, bdArgs...)...)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("gc bd %s (%s cwd): %v\nstdout: %s", strings.Join(bdArgs, " "), shape.cwd, err, stdout)
	}
	if shape.subject != nil && !strings.Contains(stdout, shape.subject()) {
		t.Fatalf("gc bd %s (%s cwd): stdout does not name %s:\n%s", strings.Join(bdArgs, " "), shape.cwd, shape.subject(), stdout)
	}
	if shape.observe != nil {
		shape.observe(t, stdout)
	}
	return gcBdLatencySample{elapsed: elapsed, children: recorder.Invocations()}
}

func gcBdLatencyCreate(t *testing.T, env []string, dir, title string) string {
	t.Helper()
	stdout, err := runCommandStdout(dir, env, integrationGCCommandTimeout, gcBinary, "bd", "create", "--title", title, "--json")
	if err != nil {
		t.Fatalf("gc bd create %q (cwd %s): %v\nstdout: %s", title, dir, err, stdout)
	}
	return gcBdLatencyCreatedID(t, stdout)
}

func gcBdLatencyCreatedID(t *testing.T, stdout string) string {
	t.Helper()
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(extractJSONPayload(stdout)), &created); err != nil || created.ID == "" {
		t.Fatalf("parsing created bead id: %v\nstdout: %s", err, stdout)
	}
	return created.ID
}

// gcBdLatencyListed fails unless a list answered with at least one bead: the
// scope's own bead is always open.
func gcBdLatencyListed(t *testing.T, stdout string) {
	t.Helper()
	var listed []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(extractJSONPayload(stdout)), &listed); err != nil || len(listed) == 0 {
		t.Fatalf("list answered no beads: %v\nstdout: %s", err, stdout)
	}
}

type gcBdLatencyStats struct {
	min, median, max time.Duration
}

func gcBdLatencyStatsOf(samples []gcBdLatencySample) gcBdLatencyStats {
	durations := make([]time.Duration, 0, len(samples))
	for _, s := range samples {
		durations = append(durations, s.elapsed)
	}
	slices.Sort(durations)
	return gcBdLatencyStats{
		min:    durations[0],
		median: durations[len(durations)/2],
		max:    durations[len(durations)-1],
	}
}

func gcBdLatencyTable(cityDir, rigDir string, shapes []gcBdLatencyShape, samples [][]gcBdLatencySample) string {
	var b strings.Builder
	fmt.Fprintf(&b, "gc bd per-call wall time: %d timed calls per shape after one warm-up; budget %s on the fastest call\n", gcBdLatencyRuns, gcBdLatencyBudget)
	fmt.Fprintf(&b, "stores: city %s, rig %s; cpus %d; loadavg %s\n",
		gcBdLatencyDoltMode(cityDir), gcBdLatencyDoltMode(rigDir), runtime.NumCPU(), gcBdLatencyLoadAverage())
	fmt.Fprintf(&b, "%-38s %-4s %8s %8s %8s  %s\n", "shape", "cwd", "min", "median", "max", "bd children per call")
	for i, shape := range shapes {
		stats := gcBdLatencyStatsOf(samples[i])
		fmt.Fprintf(&b, "%-38s %-4s %6dms %6dms %6dms  %s\n", shape.name, shape.cwd,
			stats.min.Milliseconds(), stats.median.Milliseconds(), stats.max.Milliseconds(),
			gcBdLatencyChildren(samples[i]))
	}
	return b.String()
}

// gcBdLatencyLoadAverage is the host's load average, which the table carries
// so a slow run can be told apart from a slow gc.
func gcBdLatencyLoadAverage() string {
	load, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unavailable (" + err.Error() + ")"
	}
	return strings.TrimSpace(string(load))
}

// gcBdLatencyDoltMode names a scope's persisted Dolt topology for the table.
func gcBdLatencyDoltMode(scopeRoot string) string {
	mode, ok, err := contract.ReadDoltMode(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "metadata.json"))
	switch {
	case err != nil:
		return "unreadable metadata (" + err.Error() + ")"
	case !ok:
		return "no dolt_mode"
	}
	return mode
}

// gcBdLatencyChildren renders a shape's bd children: the count (a range when
// calls differ) and the first call's children, by their leading argv.
func gcBdLatencyChildren(samples []gcBdLatencySample) string {
	lo, hi := len(samples[0].children), len(samples[0].children)
	for _, s := range samples {
		lo = min(lo, len(s.children))
		hi = max(hi, len(s.children))
	}
	count := fmt.Sprint(lo)
	if hi != lo {
		count = fmt.Sprintf("%d-%d", lo, hi)
	}
	argvs := make([]string, 0, len(samples[0].children))
	for _, child := range samples[0].children {
		argvs = append(argvs, strings.Join(child.Argv[:min(2, len(child.Argv))], " "))
	}
	return count + " [" + strings.Join(argvs, "; ") + "]"
}
