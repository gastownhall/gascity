package core

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/orders"
)

// The on-disk copies of the two human-gate orders' scripts. These tests run
// them, so they need real paths.
const (
	notifyHumanGateScriptPath  = "assets/scripts/notify-on-human-gate-creation.sh"
	renudgeHumanGateScriptPath = "assets/scripts/renudge-stale-human-gates.sh"
)

// humanGateEnvKeys are the settings that would point either script at a live
// session's city, state, trace file, or routing. Each test supplies its own.
var humanGateEnvKeys = map[string]struct{}{
	"GC_CITY":                        {},
	"GC_CITY_RUNTIME_DIR":            {},
	"GC_PACK_STATE_DIR":              {},
	"GC_BD_TRACE_JSON":               {},
	"GC_ESCALATION_RECIPIENT":        {},
	"GC_NOTIFY_GATE_LOOKBACK":        {},
	"GC_NOTIFY_GATE_RETENTION":       {},
	"GC_STALE_GATE_THRESHOLD":        {},
	"GC_STALE_GATE_RENUDGE_INTERVAL": {},
	"GC_STALE_GATE_STATE_RETENTION":  {},
	orders.ExecOutcomeFileEnv:        {},
}

// readyFailure is what the fake gc ready prints to stderr when it fails: the
// federated read refuses the whole listing when one store is unreadable.
const readyFailure = `rig "frontend" store: dolt: connection refused`

// humanGateFake describes what the fake gc answers. Each field is the stdout
// of one gc call shape; an empty field answers with nothing.
type humanGateFake struct {
	events    string            // gc events --type bead.created ...
	gateList  string            // gc bd gate list [--rig r] --limit 0 --json
	ready     string            // gc ready --status open --metadata-field ... --json
	readyFail bool              // gc ready prints readyFailure to stderr and exits 1
	shows     map[string]string // gc bd show <id> ... --json, keyed by id
}

// install writes the fake's answers under a fresh dir and returns the shell
// body fakeGCBin runs after logging argv.
func (f humanGateFake) install(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write fake answer %s: %v", name, err)
		}
	}
	write("events", f.events)
	write("gatelist", f.gateList)
	write("ready", f.ready)
	write("rigs", `{"rigs":[]}`)
	for id, row := range f.shows {
		write("show-"+id, row)
	}
	readyAnswer := `cat "$D/ready"`
	if f.readyFail {
		readyAnswer = `echo '` + readyFailure + `' >&2; exit 1`
	}
	return `D='` + dir + `'
case "$1" in
events) cat "$D/events" ;;
rig) cat "$D/rigs" ;;
ready) ` + readyAnswer + ` ;;
mail) exit 0 ;;
bd)
    case "$2" in
    show) [ -f "$D/show-$3" ] || exit 1; cat "$D/show-$3" ;;
    gate) cat "$D/gatelist" ;;
    esac ;;
esac
exit 0
`
}

func requireJQ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the human-gate scripts require it")
	}
}

// gcLog returns the fake gc's argv log split into one entry per invocation.
// A mail body spans lines, so an entry starts at each "gc " line prefix.
func gcLog(t *testing.T, logPath string) []string {
	t.Helper()
	logged, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read gc log: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(string(logged), "\n") {
		if strings.HasPrefix(line, "gc ") || len(calls) == 0 {
			calls = append(calls, line)
			continue
		}
		calls[len(calls)-1] += "\n" + line
	}
	return calls
}

func callsWithPrefix(calls []string, prefix string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func runHumanGateScript(t *testing.T, script, binDir, stateDir string, extraEnv ...string) string {
	t.Helper()
	env := append([]string{"GC_PACK_STATE_DIR=" + stateDir}, extraEnv...)
	out, err := runPackScript(t, script, binDir, humanGateEnvKeys, env)
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", script, err, out)
	}
	return out
}

// emptyOutcomeFile creates the empty per-run outcome file the controller hands
// an exec order, and returns its path.
func emptyOutcomeFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "outcome.json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("create outcome file: %v", err)
	}
	return path
}

// readOutcome decodes the outcome file the way the controller does; declared
// is false when the run wrote nothing.
func readOutcome(t *testing.T, path string) (outcome orders.ExecOutcome, declared bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read outcome file: %v", err)
	}
	outcome, declared, err = orders.DecodeExecOutcome(data)
	if err != nil {
		t.Fatalf("the controller cannot decode the declared outcome %q: %v", data, err)
	}
	return outcome, declared
}

// readRenudgeState returns the re-nudge sweep's cadence state: the time each
// gate was last re-nudged, keyed by gate id.
func readRenudgeState(t *testing.T, stateDir string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "renudge-stale-human-gates-state.json"))
	if err != nil {
		t.Fatalf("read re-nudge state: %v", err)
	}
	state := map[string]string{}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode re-nudge state %q: %v", data, err)
	}
	return state
}

const gateCreatedEvent = `{"type":"bead.created","payload":{"bead":{"id":"gcg-1","issue_type":"gate"}}}` + "\n"

// TestNotifyOnHumanGateCreationSelectsByDeclaredType pins which gates the
// creation notify mails. A gate a formula [steps.gate] synthesizes carries its
// declared type only in gc.gate_type metadata, because gc's stores have no
// await_type column; bd's own gates carry await_type. Either marks a human
// gate. A gate typed neither way (legacy, graph fences) is never mailed, and
// neither is a non-human or already-closed gate. The hint names
// `gc bd close`, which every store serves, including a split city's class
// binding that refuses `gc bd gate resolve`.
func TestNotifyOnHumanGateCreationSelectsByDeclaredType(t *testing.T) {
	requireJQ(t)
	cases := []struct {
		name     string
		row      string
		wantMail bool
	}{
		{
			name:     "formula human gate stamped gc.gate_type",
			row:      `[{"id":"gcg-1","issue_type":"gate","status":"open","title":"Gate: human approval","metadata":{"gc.gate_type":"human"}}]`,
			wantMail: true,
		},
		{
			name: "formula timer gate",
			row:  `[{"id":"gcg-1","issue_type":"gate","status":"open","metadata":{"gc.gate_type":"timer"}}]`,
		},
		{
			name: "untyped gate",
			row:  `[{"id":"gcg-1","issue_type":"gate","status":"open","metadata":{"gc.root_bead_id":"gcg-0"}}]`,
		},
		{
			name:     "bd human gate with await_type",
			row:      `[{"id":"gcg-1","issue_type":"gate","status":"open","await_type":"human"}]`,
			wantMail: true,
		},
		{
			name: "closed formula human gate",
			row:  `[{"id":"gcg-1","issue_type":"gate","status":"closed","metadata":{"gc.gate_type":"human"}}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir, logPath := fakeGCBin(t, humanGateFake{
				events: gateCreatedEvent,
				shows:  map[string]string{"gcg-1": tc.row},
			}.install(t))
			stateDir := t.TempDir()

			runHumanGateScript(t, notifyHumanGateScriptPath, binDir, stateDir)

			calls := gcLog(t, logPath)
			sends := callsWithPrefix(calls, "gc mail send ")
			if !tc.wantMail {
				if len(sends) != 0 {
					t.Fatalf("want no mail, got %d:\n%s", len(sends), strings.Join(calls, "\n"))
				}
				return
			}
			if len(sends) != 1 {
				t.Fatalf("want exactly one mail, got %d:\n%s", len(sends), strings.Join(calls, "\n"))
			}
			send := sends[0]
			if !strings.HasPrefix(send, "gc mail send human -s Human gate awaiting you: gcg-1 ") {
				t.Errorf("mail addressed or titled wrong:\n%s", send)
			}
			if !strings.Contains(send, "Resolve with: gc bd close gcg-1") {
				t.Errorf("mail body must name `gc bd close gcg-1`:\n%s", send)
			}
			if !strings.HasSuffix(strings.TrimSpace(send), "--notify") {
				t.Errorf("mail must pass --notify:\n%s", send)
			}
			state, err := os.ReadFile(filepath.Join(stateDir, "notify-on-human-gate-creation-state.json"))
			if err != nil {
				t.Fatalf("read state: %v", err)
			}
			if !strings.Contains(string(state), `"gcg-1"`) {
				t.Errorf("state does not record gcg-1:\n%s", state)
			}
		})
	}
}

const (
	// formulaHumanGateRow is a formula-synthesized human gate as a split
	// city's class binding returns it: no await_type, the declared type in
	// gc.gate_type, and an RFC3339Nano created_at.
	formulaHumanGateRow = `{"id":"gcg-1","issue_type":"gate","status":"open","created_at":"2026-01-01T00:00:00.123456789Z","title":"Gate: human approval","metadata":{"gc.gate_type":"human"}}`
	bdHumanGateRow      = `{"id":"ga-7","issue_type":"gate","status":"open","created_at":"2026-01-01T00:00:00Z","await_type":"human"}`
)

// TestRenudgeStaleHumanGatesCoversFormulaGates pins the re-nudge sweep's two
// enumerations. bd's own human gates come from the per-scope `gc bd gate list`
// (await_type == human). Gates a formula synthesizes are typed only by
// gc.gate_type and, on a split city, live in the class binding that no bd
// scope lists, so they come from `gc ready --status open` filtered on the
// stamp. Their re-fetch carries no --rig, because gc bd resolves the owning
// store from the id and refuses a --rig pin for a bead the binding holds.
//
// gc ready refuses the whole read when one store is unreadable, so a failed or
// unreadable answer is not "no formula gates": the run must say so on stderr
// and declare the enumeration as a skipped scope (a partial outcome the
// controller records as order.skipped), while the bd gates it did list are
// still re-nudged and recorded.
func TestRenudgeStaleHumanGatesCoversFormulaGates(t *testing.T) {
	requireJQ(t)
	cases := []struct {
		name        string
		fake        humanGateFake
		env         []string        // extra script environment for this case
		wantTo      map[string]bool // gate ids that must be re-nudged exactly once
		wantShows   []string        // exact `gc bd show` invocations expected
		wantSkipped string          // declared reason the formula gates were not swept; "" declares nothing
	}{
		{
			name: "formula gate listed only by gc ready",
			fake: humanGateFake{
				gateList: `[]`,
				ready:    `[` + formulaHumanGateRow + `]`,
				shows:    map[string]string{"gcg-1": `[` + formulaHumanGateRow + `]`},
			},
			wantTo:    map[string]bool{"gcg-1": true},
			wantShows: []string{"gc bd show gcg-1 --json"},
		},
		{
			// A zero re-nudge interval disables the cadence gate, so only
			// the per-sweep dedupe keeps the second listing from re-sending.
			name: "formula gate listed by both enumerations",
			fake: humanGateFake{
				gateList: `[{"id":"gcg-1","issue_type":"gate","status":"open","created_at":"2026-01-01T00:00:00Z","await_type":"human"}]`,
				ready:    `[` + formulaHumanGateRow + `]`,
				shows:    map[string]string{"gcg-1": `[` + formulaHumanGateRow + `]`},
			},
			env:       []string{"GC_STALE_GATE_RENUDGE_INTERVAL=0s"},
			wantTo:    map[string]bool{"gcg-1": true},
			wantShows: []string{"gc bd show gcg-1 --json"},
		},
		{
			name: "bd human gate",
			fake: humanGateFake{
				gateList: `[` + bdHumanGateRow + `]`,
				ready:    `[]`,
				shows:    map[string]string{"ga-7": `[` + bdHumanGateRow + `]`},
			},
			wantTo: map[string]bool{"ga-7": true},
		},
		{
			name: "gc ready failure is declared and bd gates are still swept",
			fake: humanGateFake{
				gateList:  `[` + bdHumanGateRow + `]`,
				readyFail: true,
				shows:     map[string]string{"ga-7": `[` + bdHumanGateRow + `]`},
			},
			wantTo:      map[string]bool{"ga-7": true},
			wantSkipped: "gc ready --status open failed (exit 1): " + readyFailure,
		},
		{
			name: "gc ready answer that is not a JSON array is declared",
			fake: humanGateFake{
				gateList: `[` + bdHumanGateRow + `]`,
				ready:    `{"rows":[` + formulaHumanGateRow + `]}`,
				shows:    map[string]string{"ga-7": `[` + bdHumanGateRow + `]`},
			},
			wantTo:      map[string]bool{"ga-7": true},
			wantSkipped: "gc ready --status open answered without a readable JSON array",
		},
		{
			name: "formula timer gate re-fetched is dropped",
			fake: humanGateFake{
				gateList: `[]`,
				ready:    `[` + formulaHumanGateRow + `]`,
				shows: map[string]string{"gcg-1": `[{"id":"gcg-1","issue_type":"gate","status":"open",` +
					`"created_at":"2026-01-01T00:00:00Z","metadata":{"gc.gate_type":"timer"}}]`},
			},
			wantTo: map[string]bool{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir, logPath := fakeGCBin(t, tc.fake.install(t))
			stateDir := t.TempDir()
			outcomePath := emptyOutcomeFile(t)

			out := runHumanGateScript(t, renudgeHumanGateScriptPath, binDir, stateDir,
				append([]string{"GC_STALE_GATE_THRESHOLD=0s", orders.ExecOutcomeFileEnv + "=" + outcomePath}, tc.env...)...)

			outcome, declared := readOutcome(t, outcomePath)
			if tc.wantSkipped == "" {
				if declared {
					t.Errorf("a sweep that read every enumeration must declare nothing, got %+v", outcome)
				}
			} else {
				want := orders.ExecOutcome{
					Outcome: orders.ExecOutcomePartial,
					Reason:  "some scopes or steps did not run",
					Scopes:  []orders.ExecOutcomeScope{{Scope: "formula gates (gc ready)", Reason: tc.wantSkipped}},
				}
				if !declared || !reflect.DeepEqual(outcome, want) {
					t.Errorf("declared outcome = %+v (declared %v), want %+v", outcome, declared, want)
				}
				wantLine := "renudge-stale-human-gates: " + tc.wantSkipped + "; formula gates were not swept (will retry next sweep)"
				if !strings.Contains(out, wantLine) {
					t.Errorf("output must say the formula gates were not swept (%q), got:\n%s", wantLine, out)
				}
			}
			state := readRenudgeState(t, stateDir)
			for id := range tc.wantTo {
				if state[id] == "" {
					t.Errorf("state does not record the reminder to %s, so the next sweep would repeat it: %v", id, state)
				}
			}

			calls := gcLog(t, logPath)
			const wantReady = "gc ready --status open --metadata-field gc.gate_type=human --json"
			if ready := callsWithPrefix(calls, "gc ready"); len(ready) != 1 || strings.TrimSpace(ready[0]) != wantReady {
				t.Errorf("gc ready calls = %q, want exactly [%q]", ready, wantReady)
			}
			sends := callsWithPrefix(calls, "gc mail send ")
			if len(sends) != len(tc.wantTo) {
				t.Fatalf("want %d reminder(s), got %d:\n%s", len(tc.wantTo), len(sends), strings.Join(calls, "\n"))
			}
			for id := range tc.wantTo {
				n := len(callsWithPrefix(sends, "gc mail send human -s Reminder — human gate still open: "+id+" "))
				if n != 1 {
					t.Errorf("gate %s re-nudged %d times, want 1:\n%s", id, n, strings.Join(sends, "\n"))
				}
			}
			for _, send := range sends {
				if !strings.Contains(send, "Resolve with: gc bd close ") {
					t.Errorf("reminder must name `gc bd close`:\n%s", send)
				}
			}
			if tc.wantShows != nil {
				got := callsWithPrefix(calls, "gc bd show ")
				if strings.Join(got, "\n") != strings.Join(tc.wantShows, "\n") {
					t.Errorf("bd show calls = %q, want %q", got, tc.wantShows)
				}
			}
		})
	}
}

// TestRenudgeStaleHumanGatesHonorsIntervalForFormulaGates pins the cadence
// for a formula gate: a second sweep inside GC_STALE_GATE_RENUDGE_INTERVAL
// sends nothing new.
func TestRenudgeStaleHumanGatesHonorsIntervalForFormulaGates(t *testing.T) {
	requireJQ(t)
	binDir, logPath := fakeGCBin(t, humanGateFake{
		gateList: `[]`,
		ready:    `[` + formulaHumanGateRow + `]`,
		shows:    map[string]string{"gcg-1": `[` + formulaHumanGateRow + `]`},
	}.install(t))
	stateDir := t.TempDir()

	for i := 0; i < 2; i++ {
		runHumanGateScript(t, renudgeHumanGateScriptPath, binDir, stateDir,
			"GC_STALE_GATE_THRESHOLD=0s", "GC_STALE_GATE_RENUDGE_INTERVAL=1h")
	}

	calls := gcLog(t, logPath)
	if sends := callsWithPrefix(calls, "gc mail send "); len(sends) != 1 {
		t.Fatalf("want one reminder across two sweeps, got %d:\n%s", len(sends), strings.Join(calls, "\n"))
	}
}
