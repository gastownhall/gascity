package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultRecoveryHooksSurfaceFailures pins the recovery-diagnostic contract
// on the DEFAULT on_death/on_boot hooks. The exact generated shell is locked
// byte-for-byte by the golden fixtures (TestWorkQueryGolden); this test asserts
// the three load-bearing properties by name so a refactor that keeps the golden
// superficially plausible but breaks the contract still fails:
//
//   - the hook emits the RecoveryHookMarker the controller callers filter on;
//   - a failed bd write's stderr is CAPTURED (`2>&1 >/dev/null`), not discarded;
//   - the capture rides an `if ! err=$(...)` guard so the pipeline still exits 0
//     even on failure — otherwise shellCommand's cmd.Output() discards the
//     diagnostic on its error return and the whole feature silently regresses;
//   - a failed bd READ is named too ("read failed"), not only a failed write.
//
// It does not exec the shell; TestDefaultRecoveryHooksReadFailures executes the
// generated hooks and the golden fixtures are the byte-for-byte pin.
func TestDefaultRecoveryHooksSurfaceFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"on_death", (&Agent{Name: "worker"}).EffectiveOnDeathFor(QueryTopology{})},
		{"on_boot", (&Agent{Name: "worker"}).EffectiveOnBootFor(QueryTopology{})},
	} {
		if !strings.Contains(tc.script, RecoveryHookMarker) {
			t.Errorf("%s hook does not emit the %q recovery marker:\n%s", tc.name, RecoveryHookMarker, tc.script)
		}
		if !strings.Contains(tc.script, "2>&1 >/dev/null") {
			t.Errorf("%s hook does not capture a failed bd write's stderr (want `2>&1 >/dev/null`):\n%s", tc.name, tc.script)
		}
		if !strings.Contains(tc.script, "if ! err=$(") {
			t.Errorf("%s hook does not use the exit-0-preserving `if ! err=$(...)` capture guard:\n%s", tc.name, tc.script)
		}
		if want := RecoveryHookMarker + " " + tc.name + " read failed"; !strings.Contains(tc.script, want) {
			t.Errorf("%s hook does not surface a failed bd read (want %q):\n%s", tc.name, want, tc.script)
		}
	}
}

// fakeRecoveryBD is a `bd` stand-in for executing the DEFAULT recovery hooks.
// Each read has its own mode and payload, chosen through the environment:
//
//	FAKE_BD_<READ>      ok (default) | warn (exit 0, noise on stderr) | fail
//	FAKE_BD_<READ>_OUT  what the read prints; a failing read prints it BEFORE it
//	                    exits 1, which is how a truncated list looks
//
// READ is ASSIGNEE, ROUTED or RUN_TARGET (the three `bd list` reads, told apart
// by their arguments) or QUERY. `bd update` always succeeds. Every call is
// appended to $FAKE_BD_LOG.
const fakeRecoveryBD = `#!/bin/sh
[ -n "$FAKE_BD_LOG" ] && printf '%s\n' "$*" >> "$FAKE_BD_LOG"
case "$1" in
update) exit 0 ;;
query) mode=$FAKE_BD_QUERY; out=$FAKE_BD_QUERY_OUT ;;
list)
	case "$*" in
	*gc.run_target=*) mode=$FAKE_BD_RUN_TARGET; out=$FAKE_BD_RUN_TARGET_OUT ;;
	*gc.routed_to=*) mode=$FAKE_BD_ROUTED; out=$FAKE_BD_ROUTED_OUT ;;
	*) mode=$FAKE_BD_ASSIGNEE; out=$FAKE_BD_ASSIGNEE_OUT ;;
	esac ;;
esac
case "$mode" in
fail) printf '%s' "$out"; printf 'bd: schema version mismatch\n' >&2; exit 1 ;;
warn) printf 'warning: schema is ahead of this bd\n' >&2 ;;
esac
printf '%s' "${out:-[]}"
`

// recoveryHookRun is one execution of a generated recovery hook.
type recoveryHookRun struct {
	stdout  string
	exit    int
	updates []string // the id each `bd update` was invoked with, in order
	reads   []string // every `bd list` / `bd query` invocation, in order
}

// runRecoveryHook executes command (a generated on_death or on_boot script)
// against fakeRecoveryBD configured by env.
func runRecoveryHook(t *testing.T, command string, env map[string]string) recoveryHookRun {
	t.Helper()
	requireJQ(t)
	bdLog := filepath.Join(t.TempDir(), "bd-invocations")
	withLog := map[string]string{"FAKE_BD_LOG": bdLog}
	for k, v := range env {
		withLog[k] = v
	}
	res := runGeneratedQueryWithBD(t, command, withLog, "#!/bin/sh\nexit 0\n", fakeRecoveryBD)
	run := recoveryHookRun{stdout: res.stdout, exit: res.exit}
	data, err := os.ReadFile(bdLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read bd log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) >= 2 && fields[0] == "update":
			run.updates = append(run.updates, fields[1])
		case len(fields) >= 1 && (fields[0] == "list" || fields[0] == "query"):
			run.reads = append(run.reads, line)
		}
	}
	return run
}

// markerLines returns the stdout lines that carry the recovery marker.
func (r recoveryHookRun) markerLines() []string {
	var out []string
	for _, line := range strings.Split(r.stdout, "\n") {
		if strings.HasPrefix(line, RecoveryHookMarker) {
			out = append(out, line)
		}
	}
	return out
}

// countReads counts the recorded reads that contain every one of parts.
func (r recoveryHookRun) countReads(parts ...string) int {
	n := 0
reads:
	for _, read := range r.reads {
		for _, p := range parts {
			if !strings.Contains(read, p) {
				continue reads
			}
		}
		n++
	}
	return n
}

// TestDefaultRecoveryHooksReadFailures executes the GENERATED default on_death
// and on_boot hooks and pins how they treat a bd READ that exits non-zero — the
// way a schema-skewed or unreadable store fails:
//
//   - each failed read prints exactly one marker line, naming the hook, the
//     read, and its exit status, and the hook still exits 0 (the controller
//     drops stdout when a hook exits non-zero);
//   - a failed read contributes nothing to the sweep, even rows it printed
//     before it failed, and does not stop the other reads being released;
//   - the marker never reaches the release loop as a bead id;
//   - a healthy read that merely warns on stderr releases and prints no marker;
//   - every read runs exactly once.
func TestDefaultRecoveryHooksReadFailures(t *testing.T) {
	const (
		deathListBead  = `[{"id":"d-list","metadata":{"gc.routed_to":"worker"}}]`
		deathQueryBead = `[{"id":"d-query","assignee":"worker","metadata":{"gc.routed_to":"worker"}}]`
		bootRoutedBead = `[{"id":"b-routed"}]`
		bootRunBead    = `[{"id":"b-run","metadata":{}}]`
		bootQueryBead  = `[{"id":"b-query","assignee":"","metadata":{"gc.routed_to":"worker"}}]`
	)
	death := (&Agent{Name: "worker"}).EffectiveOnDeathFor(QueryTopology{})
	boot := (&Agent{Name: "worker"}).EffectiveOnBootFor(QueryTopology{})

	// readCounts is how many times each read must have run: (list, query) for
	// on_death, (routed list, run_target list, query) for on_boot.
	type readCounts struct{ assignee, routed, runTarget, query int }
	type tc struct {
		name        string
		command     string
		hook        string
		env         map[string]string
		wantMarkers []string // substrings, one per expected marker line, in order
		wantUpdates []string
		wantReads   readCounts
	}
	deathReads := readCounts{assignee: 1, query: 1}
	bootReads := readCounts{routed: 1, runTarget: 1, query: 1}
	cases := []tc{
		{
			name: "on_death list fails, query serves a bead", command: death, hook: "on_death",
			env: map[string]string{
				"FAKE_BD_ASSIGNEE": "fail", "FAKE_BD_QUERY_OUT": deathQueryBead,
			},
			wantMarkers: []string{"on_death read failed: bd list"},
			wantUpdates: []string{"d-query"},
			wantReads:   deathReads,
		},
		{
			name: "on_death every read fails", command: death, hook: "on_death",
			env:         map[string]string{"FAKE_BD_ASSIGNEE": "fail", "FAKE_BD_QUERY": "fail"},
			wantMarkers: []string{"on_death read failed: bd list", "on_death read failed: bd query"},
			wantReads:   deathReads,
		},
		{
			name: "on_death list prints a bead then fails", command: death, hook: "on_death",
			env: map[string]string{
				"FAKE_BD_ASSIGNEE": "fail", "FAKE_BD_ASSIGNEE_OUT": deathListBead,
			},
			wantMarkers: []string{"on_death read failed: bd list"},
			wantReads:   deathReads,
		},
		{
			name: "on_death healthy bd warns on stderr", command: death, hook: "on_death",
			env: map[string]string{
				"FAKE_BD_ASSIGNEE": "warn", "FAKE_BD_ASSIGNEE_OUT": deathListBead,
				"FAKE_BD_QUERY": "warn", "FAKE_BD_QUERY_OUT": deathQueryBead,
			},
			wantUpdates: []string{"d-list", "d-query"},
			wantReads:   deathReads,
		},
		{
			name: "on_boot lists fail, query serves a bead", command: boot, hook: "on_boot",
			env: map[string]string{
				"FAKE_BD_ROUTED": "fail", "FAKE_BD_RUN_TARGET": "fail", "FAKE_BD_QUERY_OUT": bootQueryBead,
			},
			wantMarkers: []string{
				"on_boot read failed: bd list gc.routed_to",
				"on_boot read failed: bd list gc.run_target",
			},
			wantUpdates: []string{"b-query"},
			wantReads:   bootReads,
		},
		{
			name: "on_boot every read fails", command: boot, hook: "on_boot",
			env: map[string]string{"FAKE_BD_ROUTED": "fail", "FAKE_BD_RUN_TARGET": "fail", "FAKE_BD_QUERY": "fail"},
			wantMarkers: []string{
				"on_boot read failed: bd list gc.routed_to",
				"on_boot read failed: bd list gc.run_target",
				"on_boot read failed: bd query",
			},
			wantReads: bootReads,
		},
		{
			name: "on_boot list prints a bead then fails", command: boot, hook: "on_boot",
			env: map[string]string{
				"FAKE_BD_ROUTED": "fail", "FAKE_BD_ROUTED_OUT": bootRoutedBead,
				"FAKE_BD_RUN_TARGET_OUT": bootRunBead,
			},
			wantMarkers: []string{"on_boot read failed: bd list gc.routed_to"},
			wantUpdates: []string{"b-run"},
			wantReads:   bootReads,
		},
		{
			name: "on_boot healthy bd warns on stderr", command: boot, hook: "on_boot",
			env: map[string]string{
				"FAKE_BD_ROUTED": "warn", "FAKE_BD_ROUTED_OUT": bootRoutedBead,
				"FAKE_BD_RUN_TARGET": "warn", "FAKE_BD_RUN_TARGET_OUT": bootRunBead,
				"FAKE_BD_QUERY": "warn", "FAKE_BD_QUERY_OUT": bootQueryBead,
			},
			wantUpdates: []string{"b-routed", "b-run", "b-query"},
			wantReads:   bootReads,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := runRecoveryHook(t, c.command, c.env)
			if run.exit != 0 {
				t.Errorf("%s exited %d; the controller drops stdout from a hook that exits non-zero", c.hook, run.exit)
			}
			markers := run.markerLines()
			if len(markers) != len(c.wantMarkers) {
				t.Fatalf("got %d marker lines, want %d: %q (stdout %q)", len(markers), len(c.wantMarkers), markers, run.stdout)
			}
			for i, want := range c.wantMarkers {
				if !strings.Contains(markers[i], want) || !strings.Contains(markers[i], "(exit 1)") {
					t.Errorf("marker %d = %q, want it to contain %q and %q", i, markers[i], want, "(exit 1)")
				}
			}
			if strings.Join(run.updates, ",") != strings.Join(c.wantUpdates, ",") {
				t.Errorf("bd update ids = %q, want %q", run.updates, c.wantUpdates)
			}
			for _, id := range run.updates {
				if strings.HasPrefix(id, "gc-recovery") {
					t.Errorf("a recovery marker reached the release loop as a bead id: bd update %q", id)
				}
			}
			got := readCounts{
				assignee:  run.countReads("list", "--assignee="),
				routed:    run.countReads("list", "gc.routed_to="),
				runTarget: run.countReads("list", "gc.run_target="),
				query:     run.countReads("query"),
			}
			if got != c.wantReads {
				t.Errorf("reads run = %+v, want each exactly once: %+v (log %q)", got, c.wantReads, run.reads)
			}
		})
	}
}
