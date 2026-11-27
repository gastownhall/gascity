package gastown_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gateSweepActivityFixture runs gate-sweep.sh repeatedly against one city, so
// the activity gate's state carries from run to run. The gc double answers
// `gc events --type <t>` from events/<t> (one JSON line per event) and logs
// every call to GC_CALL_LOG; the bd double logs "<scope> <args>" to BD_LOG
// and answers a gate check with bd's no-open-gates line unless the scope is
// listed in gated-scopes.
type gateSweepActivityFixture struct {
	binDir, bdLog, gcLog, eventsDir, stateDir string
	env                                       map[string]string
}

const gateSweepActivityRigs = `{"rigs":[{"name":"city","prefix":"ga","hq":true},{"name":"api","prefix":"api"},{"name":"web","prefix":"web"},{"name":"old","prefix":"old","suspended":true}]}`

func newGateSweepActivityFixture(t *testing.T) *gateSweepActivityFixture {
	t.Helper()
	binDir, bdLog, env := gateSweepEnv(t)
	f := &gateSweepActivityFixture{
		binDir:    binDir,
		bdLog:     bdLog,
		gcLog:     filepath.Join(t.TempDir(), "gc.log"),
		eventsDir: t.TempDir(),
		stateDir:  t.TempDir(),
		env:       env,
	}
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
if [ "${1:-}" = "events" ]; then
  [ -z "${FAKE_EVENTS_FAIL:-}" ] || exit 1
  fake_type=""
  while [ $# -gt 0 ]; do
    if [ "$1" = "--type" ]; then fake_type="$2"; shift; fi
    shift
  done
  [ ! -f "$FAKE_EVENTS_DIR/$fake_type" ] || cat "$FAKE_EVENTS_DIR/$fake_type"
  exit 0
fi
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s %s\n' "$GC_FAKE_SCOPE" "$*" >> "$BD_LOG"
if [ "${1:-}" = "gate" ] && [ "${2:-}" = "check" ]; then
  gated=""
  [ ! -f "$FAKE_GATED_SCOPES_FILE" ] || gated=$(cat "$FAKE_GATED_SCOPES_FILE")
  case " $gated " in
    *" $GC_FAKE_SCOPE "*) echo "Checked 1 gates: 0 resolved, 0 escalated, 0 errors" ;;
    *) echo "No open gates of type '${3#--type=}' found." ;;
  esac
fi
exit 0
`)
	env["GC_CALL_LOG"] = f.gcLog
	env["GC_PACK_STATE_DIR"] = f.stateDir
	env["FAKE_EVENTS_DIR"] = f.eventsDir
	env["FAKE_GATED_SCOPES_FILE"] = filepath.Join(f.eventsDir, "gated-scopes")
	env["FAKE_RIG_LIST_JSON"] = gateSweepActivityRigs
	return f
}

// run runs one sweep and returns the bd and gc calls it made.
func (f *gateSweepActivityFixture) run(t *testing.T) (bdCalls, gcCalls string) {
	t.Helper()
	for _, p := range []string{f.bdLog, f.gcLog} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatalf("Remove(%s): %v", p, err)
		}
	}
	runScript(t, coreScriptPath("gate-sweep.sh"), f.env)
	return readGateSweepLog(t, f.bdLog), readGateSweepLog(t, f.gcLog)
}

func (f *gateSweepActivityFixture) setGated(t *testing.T, scopes ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.eventsDir, "gated-scopes"), []byte(strings.Join(scopes, " ")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *gateSweepActivityFixture) setEvents(t *testing.T, eventType string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.eventsDir, eventType), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *gateSweepActivityFixture) clearEvents(t *testing.T) {
	t.Helper()
	for _, eventType := range []string{"bead.created", "bead.updated", "controller.started"} {
		if err := os.Remove(filepath.Join(f.eventsDir, eventType)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// ageBackstop moves the recorded full sweep back by d.
func (f *gateSweepActivityFixture) ageBackstop(t *testing.T, d time.Duration) {
	t.Helper()
	path := filepath.Join(f.stateDir, "gate-sweep-state")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(state): %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if v, ok := strings.CutPrefix(line, "last_full "); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatalf("state last_full %q: %v", v, err)
			}
			line = "last_full " + strconv.FormatInt(n-int64(d/time.Second), 10)
		}
		out = append(out, line)
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gateSweptScopes(bdCalls string) []string {
	var scopes []string
	for _, line := range strings.Split(bdCalls, "\n") {
		scope, rest, ok := strings.Cut(line, " ")
		if ok && strings.HasPrefix(rest, "gate check --type=timer") {
			scopes = append(scopes, scope)
		}
	}
	return scopes
}

func requireSwept(t *testing.T, bdCalls string, want ...string) {
	t.Helper()
	got := gateSweptScopes(bdCalls)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("swept %v, want %v; bd log:\n%s", got, want, bdCalls)
	}
}

// TestGateSweepActivityGateSkipsQuietStores pins the activity gate: after the
// first run's full sweep finds no gate anywhere, a run with no gate event
// touches no store and does not list the rigs.
func TestGateSweepActivityGateSkipsQuietStores(t *testing.T) {
	f := newGateSweepActivityFixture(t)

	bdCalls, _ := f.run(t)
	requireSwept(t, bdCalls, "city", "rig:api", "rig:web")

	bdCalls, gcCalls := f.run(t)
	if bdCalls != "" {
		t.Fatalf("a quiet run reached bd:\n%s", bdCalls)
	}
	if strings.Contains(gcCalls, "rig list") || strings.Contains(gcCalls, "bd ") {
		t.Fatalf("a quiet run should only read events; gc calls:\n%s", gcCalls)
	}
}

// TestGateSweepActivityGateSweepsTheStoreOfANewGate verifies that a gate
// created in one store is checked on the next run, in that store only, and
// keeps being checked while it is open.
func TestGateSweepActivityGateSweepsTheStoreOfANewGate(t *testing.T) {
	f := newGateSweepActivityFixture(t)
	f.run(t)

	f.setEvents(t, "bead.created",
		`{"seq":5,"type":"bead.created","payload":{"bead":{"id":"api-g1","issue_type":"gate","labels":[]}}}`,
		`{"seq":6,"type":"bead.created","payload":{"bead":{"id":"web-t1","issue_type":"task"}}}`)
	f.setGated(t, "rig:api")
	bdCalls, _ := f.run(t)
	requireSwept(t, bdCalls, "rig:api")

	f.clearEvents(t)
	bdCalls, _ = f.run(t)
	requireSwept(t, bdCalls, "rig:api")

	// The gate closes: this run still checks the store, and finds nothing.
	f.setGated(t)
	bdCalls, _ = f.run(t)
	requireSwept(t, bdCalls, "rig:api")
	bdCalls, _ = f.run(t)
	requireSwept(t, bdCalls)
}

// TestGateSweepActivityGateReadsTheLocalFallbackShape verifies that a gate
// event in the flat payload the `gc events` local fallback prints, and one in
// the city store (the hq prefix), are both found.
func TestGateSweepActivityGateReadsTheLocalFallbackShape(t *testing.T) {
	f := newGateSweepActivityFixture(t)
	f.run(t)

	f.setEvents(t, "bead.updated", `{"seq":9,"type":"bead.updated","payload":{"id":"ga-g2","issue_type":"gate"}}`)
	f.setGated(t, "city")
	bdCalls, _ := f.run(t)
	requireSwept(t, bdCalls, "city")
}

// TestGateSweepActivityGateKeepsAStoreWithOpenGatesFromTheFullSweep verifies
// that a store the full sweep finds holding a gate is checked on later runs
// without any event.
func TestGateSweepActivityGateKeepsAStoreWithOpenGatesFromTheFullSweep(t *testing.T) {
	f := newGateSweepActivityFixture(t)
	f.setGated(t, "rig:web")
	f.run(t)

	bdCalls, _ := f.run(t)
	requireSwept(t, bdCalls, "rig:web")
}

// TestGateSweepActivityGateFallsBackToAFullSweep covers every case where only
// a full sweep is safe.
func TestGateSweepActivityGateFallsBackToAFullSweep(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *gateSweepActivityFixture)
	}{
		{"backstop due", func(t *testing.T, f *gateSweepActivityFixture) { f.ageBackstop(t, 2*time.Hour) }},
		{"event log unreadable", func(_ *testing.T, f *gateSweepActivityFixture) { f.env["FAKE_EVENTS_FAIL"] = "1" }},
		{"controller restarted", func(t *testing.T, f *gateSweepActivityFixture) {
			f.setEvents(t, "controller.started", `{"seq":3,"type":"controller.started"}`)
		}},
		{"gate in a store no rig owns", func(t *testing.T, f *gateSweepActivityFixture) {
			f.setEvents(t, "bead.created", `{"seq":4,"type":"bead.created","payload":{"bead":{"id":"zz-g1","issue_type":"gate"}}}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateSweepActivityFixture(t)
			f.run(t)
			tc.setup(t, f)
			bdCalls, _ := f.run(t)
			requireSwept(t, bdCalls, "city", "rig:api", "rig:web")
		})
	}
}

// TestGateSweepActivityGateHonorsTheFullInterval verifies the backstop knob.
func TestGateSweepActivityGateHonorsTheFullInterval(t *testing.T) {
	f := newGateSweepActivityFixture(t)
	f.env["GC_GATE_SWEEP_FULL_INTERVAL"] = "10m"
	f.run(t)
	f.ageBackstop(t, 9*time.Minute)
	if bdCalls, _ := f.run(t); bdCalls != "" {
		t.Fatalf("backstop ran before its interval:\n%s", bdCalls)
	}
	f.ageBackstop(t, 2*time.Minute)
	bdCalls, _ := f.run(t)
	requireSwept(t, bdCalls, "city", "rig:api", "rig:web")
}

// TestGateSweepActivityGateKeepsASuspendedRigWatched verifies that a watched
// rig that is suspended is left alone, and checked again once it resumes.
func TestGateSweepActivityGateKeepsASuspendedRigWatched(t *testing.T) {
	f := newGateSweepActivityFixture(t)
	f.setGated(t, "rig:api")
	f.run(t)

	f.env["FAKE_RIG_LIST_JSON"] = strings.Replace(gateSweepActivityRigs, `{"name":"api","prefix":"api"}`, `{"name":"api","prefix":"api","suspended":true}`, 1)
	bdCalls, _ := f.run(t)
	requireSwept(t, bdCalls)

	f.env["FAKE_RIG_LIST_JSON"] = gateSweepActivityRigs
	bdCalls, _ = f.run(t)
	requireSwept(t, bdCalls, "rig:api")
}
