package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/session"
)

// The hook request budget: how many beads requests each session-side hook
// command sends a remote ledger. Agent hooks run these commands as often as
// every prompt, so one whose cost grows with the ledger slows every agent as
// the ledger grows.
//
// Every session-side hook command runs here in process against a FILE-BACKED
// city, with observeOpenedStore wrapping each store it opens in a recorder.
// The file store is only the fixture: it answers the operations so the command
// takes its real path, and nothing about its own cost is measured. The
// recorded operations are priced by beadstest.WireModel AS THE NATIVE STORE
// WOULD SEND THEM — its open handshake, its two-request Get, and
// beads.NativeListPlan for every listing — that is, what the same command
// would cost against a remote ledger of hookBudgetLedgerRows rows,
// where a listing whose plan carries no narrowing predicate pages through the
// whole ledger. Each scenario asserts that no listing walks the whole ledger,
// so its cost does not grow with the ledger, and a ceiling on that total and
// on the store opens. The ceilings are ratchets: they started at the counts
// measured when this harness landed, and come down as a hook sheds requests.
//
// `gc hook --claim` issues no store operation before its work query, which is a
// generated script of `bd` invocations; its budget is
// TestEffectiveWorkQueryNoWorkBdInvocationBudget in internal/config.

// hookBudgetLedgerRows is the ledger size the wire model prices whole-ledger
// walks at: the size fitted to the field measurement.
const hookBudgetLedgerRows = 4000

// hookBudgetHandshake is the requests one native store open costs: the
// handshake, the issue-prefix read and the types.infra read the list pushdown
// takes its pushable types from.
const hookBudgetHandshake = 3

// hookBudgetIdentity is the configured named session every scenario runs as.
const hookBudgetIdentity = "probe-a"

type hookBudgetFixture struct {
	cityDir   string
	sessionID string
}

// newHookBudgetFixture builds a file-backed city with one named session and
// projects the environment template resolution gives that session.
func newHookBudgetFixture(t *testing.T) hookBudgetFixture {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_MAIL", "")
	t.Setenv("GC_INJECT_CONTEXT", "")

	cityDir := t.TempDir()
	writeHookBudgetCityTOML(t, cityDir)
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{hookBudgetIdentity}, "", "", "", true, false, 0, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionNew = %d; stderr=%s", code, stderr.String())
	}
	sessionBead := onlySessionBead(t, cityDir)
	resetHookBudgetProcessState(t)

	t.Setenv("GC_SESSION_ID", sessionBead.ID)
	t.Setenv("GC_SESSION_NAME", hookBudgetIdentity)
	t.Setenv("GC_ALIAS", hookBudgetIdentity)
	t.Setenv("GC_AGENT", hookBudgetIdentity)
	t.Setenv("GC_TEMPLATE", hookBudgetIdentity)
	t.Setenv(managedSessionHookEnv, "1")

	prevAPI := mailCheckAPIClient
	mailCheckAPIClient = func(string) (*api.Client, string) { return nil, "" }
	t.Cleanup(func() { mailCheckAPIClient = prevAPI })
	prevPoller := startNudgePoller
	startNudgePoller = func(string, string, string) error { return nil }
	t.Cleanup(func() { startNudgePoller = prevPoller })

	return hookBudgetFixture{cityDir: cityDir, sessionID: sessionBead.ID}
}

// writeHookBudgetCityTOML writes a file-backed city whose one named session is
// hookBudgetIdentity.
func writeHookBudgetCityTOML(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	pack := fmt.Sprintf("[pack]\nname = \"test-city\"\nschema = 2\n\n[[named_session]]\ntemplate = %q\n", hookBudgetIdentity)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(pack), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	writeBuiltinImportsFixture(t, dir, "core")
	siteTOML := fmt.Sprintf("workspace_name = %q\n", namedSessionTestWorkspace)
	if err := os.WriteFile(filepath.Join(dir, ".gc", "site.toml"), []byte(siteTOML), 0o644); err != nil {
		t.Fatalf("WriteFile(.gc/site.toml): %v", err)
	}
	writeCatalogFile(t, dir, "agents/"+hookBudgetIdentity+"/agent.toml", "provider = \"codex\"\nstart_command = \"echo\"\n")
}

// resetHookBudgetProcessState drops the process-lifetime store memos, so each
// recorded command opens what a fresh process would.
func resetHookBudgetProcessState(t *testing.T) {
	t.Helper()
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatalf("closeCLIStorageRoutes: %v", err)
	}
	cliStoreCache.mu.Lock()
	cliStoreCache.path = ""
	cliStoreCache.store = nil
	cliStoreCache.mu.Unlock()
}

// sendMail sends the fixture session one message from human.
func (f hookBudgetFixture) sendMail(t *testing.T, subject string) {
	t.Helper()
	mp, code := openCityMailProvider(io.Discard, "hook budget fixture")
	if code != 0 {
		t.Fatalf("openCityMailProvider = %d", code)
	}
	if _, err := mp.Send("human", hookBudgetIdentity, subject, "body of "+subject); err != nil {
		t.Fatalf("Send(%q): %v", subject, err)
	}
	resetHookBudgetProcessState(t)
}

func (f hookBudgetFixture) autoHandoff(t *testing.T) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := cmdHandoffWithForce([]string{"context cycle"}, "", true, "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc handoff --auto = %d; stderr=%s", code, stderr.String())
	}
	resetHookBudgetProcessState(t)
}

func (f hookBudgetFixture) queueNudge(t *testing.T) {
	t.Helper()
	if err := enqueueQueuedNudge(f.cityDir, newQueuedNudge(hookBudgetIdentity, "review queued work", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	resetHookBudgetProcessState(t)
}

func (f hookBudgetFixture) activeStep(t *testing.T) {
	t.Helper()
	store, err := openCityStoreAt(f.cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	root, err := store.Create(beads.Bead{Title: "patrol", Type: "molecule", Assignee: hookBudgetIdentity})
	if err != nil {
		t.Fatalf("Create(molecule): %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(root.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("Update(molecule): %v", err)
	}
	step, err := store.Create(beads.Bead{Title: "inspect", Type: "step", ParentID: root.ID, Description: "inspect the queue"})
	if err != nil {
		t.Fatalf("Create(step): %v", err)
	}
	if err := store.Update(step.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("Update(step): %v", err)
	}
	resetHookBudgetProcessState(t)
}

// predecessorMail gives the fixture session two earlier aliases, each the
// current alias of one closed session that itself carries a session name and
// an alias history, and sends one message to the second predecessor's id. The
// session inbox then joins more routes than one identity carries, so its one
// listing names more assignees than a small fan-out would push. Each earlier
// alias is resolved as a mailbox address, whose exact-id probe misses
// (session.Store.ResolveMailboxAddress), so the scenario's GetMisses ceiling
// is two per alias.
func (f hookBudgetFixture) predecessorMail(t *testing.T) {
	t.Helper()
	store, err := openCityStoreAt(f.cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if err := store.SetMetadata(f.sessionID, "alias_history", "older-probe,old-probe"); err != nil {
		t.Fatalf("SetMetadata(alias_history): %v", err)
	}
	var lastID string
	for _, alias := range []string{"old-probe", "older-probe"} {
		pred, err := store.Create(beads.Bead{
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"alias":         alias,
				"session_name":  "runtime-" + alias,
				"alias_history": "first-" + alias,
			},
		})
		if err != nil {
			t.Fatalf("Create(predecessor %s): %v", alias, err)
		}
		if err := store.Close(pred.ID); err != nil {
			t.Fatalf("Close(predecessor %s): %v", alias, err)
		}
		lastID = pred.ID
	}
	resetHookBudgetProcessState(t)
	mp, code := openCityMailProvider(io.Discard, "hook budget fixture")
	if code != 0 {
		t.Fatalf("openCityMailProvider = %d", code)
	}
	if _, err := mp.Send("human", lastID, "for my predecessor", "inherited mail"); err != nil {
		t.Fatalf("Send(predecessor): %v", err)
	}
	resetHookBudgetProcessState(t)
}

// hookBudgetRun is what one recorded command did to its ledger.
type hookBudgetRun struct {
	opens  int
	ops    []beadstest.RecordedOp
	stdout string
	stderr string
	code   int
}

// recordHookCommand runs fn with every store it opens wrapped in a recorder.
func recordHookCommand(t *testing.T, fn func(stdout, stderr io.Writer) int) hookBudgetRun {
	t.Helper()
	resetHookBudgetProcessState(t)
	var (
		mu        sync.Mutex
		recorders []*beadstest.OpRecordingStore
	)
	// The cleanup restores the seam when fn fails the test; the restore after
	// fn ends the recording as soon as it returns.
	prev := observeOpenedStore
	t.Cleanup(func() { observeOpenedStore = prev })
	observeOpenedStore = func(_ string, store beads.Store) beads.Store {
		rec := beadstest.NewOpRecordingStore(store)
		mu.Lock()
		recorders = append(recorders, rec)
		mu.Unlock()
		return rec
	}
	// run() opens the invocation store memo for every hook verb measured here
	// (TestInvocationStoreMemoCoversTheHookVerbs); the in-process call does the
	// same, and ends it the way run() does at exit.
	endStoreMemo := beginInvocationStoreMemo()
	var stdout, stderr bytes.Buffer
	code := fn(&stdout, &stderr)
	if err := endStoreMemo(); err != nil {
		t.Errorf("releasing the invocation's stores: %v", err)
	}
	observeOpenedStore = prev
	resetHookBudgetProcessState(t)

	mu.Lock()
	defer mu.Unlock()
	run := hookBudgetRun{opens: len(recorders), stdout: stdout.String(), stderr: stderr.String(), code: code}
	for _, rec := range recorders {
		run.ops = append(run.ops, rec.Ops()...)
	}
	return run
}

// hookBudgetCeiling bounds one scenario. Requests is the modeled wire total,
// which no longer depends on the ledger size; Opens bounds store opens;
// SessionGets bounds the Gets of the hook's own session bead; GetMisses bounds
// Gets that find nothing — an address probed as if it were a bead id.
type hookBudgetCeiling struct {
	Requests    int
	Opens       int
	SessionGets int
	GetMisses   int
}

type hookBudgetScenario struct {
	name    string
	seed    func(t *testing.T, f hookBudgetFixture)
	run     func(t *testing.T, f hookBudgetFixture, stdout, stderr io.Writer) int
	ceiling hookBudgetCeiling
	// wantStdout is a substring the command's output must carry, proving the
	// scenario exercised the path it names.
	wantStdout string
}

func mailCheckInject(_ *testing.T, _ hookBudgetFixture, stdout, stderr io.Writer) int {
	return cmdMailCheckWithFormat(nil, true, "", stdout, stderr)
}

func nudgeDrainInject(_ *testing.T, _ hookBudgetFixture, stdout, stderr io.Writer) int {
	return cmdNudgeDrainWithFormat(nil, true, "", stdout, stderr)
}

func nudgeDrainInjectWithUsage(t *testing.T, f hookBudgetFixture, stdout, stderr io.Writer) int {
	withHookStdin(t, hookInputFor(writeTranscript(t, usageLine("claude-fable-5", 1_000, 98_000, 1_000))))
	return nudgeDrainInject(t, f, stdout, stderr)
}

// primeHook runs the SessionStart hook the codex overlay stages.
func primeHook(t *testing.T, _ hookBudgetFixture, stdout, stderr io.Writer) int {
	t.Setenv("GC_HOOK_EVENT_NAME", "SessionStart")
	t.Setenv("GC_HOOK_SOURCE", "startup")
	withPrimeHookStdin(t)
	return doPrimeWithHookFormat(nil, stdout, stderr, true, "codex", false)
}

func hookBudgetScenarios() []hookBudgetScenario {
	none := func(*testing.T, hookBudgetFixture) {}
	oneMail := func(t *testing.T, f hookBudgetFixture) { f.sendMail(t, "status please") }
	handoff := func(t *testing.T, f hookBudgetFixture) { f.autoHandoff(t) }
	// A priority:1 handoff sent before more ordinary mail than the injection
	// window holds: the window keeps the handoff and the newest mail.
	priorityWindow := func(t *testing.T, f hookBudgetFixture) {
		f.autoHandoff(t)
		for i := 1; i <= mailInjectMaxMessages+1; i++ {
			f.sendMail(t, fmt.Sprintf("update %d", i))
		}
	}
	handoffAndMail := func(t *testing.T, f hookBudgetFixture) {
		f.autoHandoff(t)
		f.sendMail(t, "status please")
	}
	return []hookBudgetScenario{
		{name: "mail check --inject/empty", seed: none, run: mailCheckInject, ceiling: hookBudgetCeiling{Requests: 7, Opens: 1, SessionGets: 1, GetMisses: 0}},
		{name: "mail check --inject/one mail", seed: oneMail, run: mailCheckInject, ceiling: hookBudgetCeiling{Requests: 7, Opens: 1, SessionGets: 1, GetMisses: 0}, wantStdout: "status please"},
		{name: "mail check --inject/auto-handoff", seed: handoff, run: mailCheckInject, ceiling: hookBudgetCeiling{Requests: 9, Opens: 1, SessionGets: 1, GetMisses: 0}, wantStdout: "context cycle"},
		{name: "mail check --inject/priority window", seed: priorityWindow, run: mailCheckInject, ceiling: hookBudgetCeiling{Requests: 9, Opens: 1, SessionGets: 1, GetMisses: 0}, wantStdout: "update " + fmt.Sprint(mailInjectMaxMessages+1)},
		{name: "mail check --inject/closed predecessors", seed: func(t *testing.T, f hookBudgetFixture) { f.predecessorMail(t) }, run: mailCheckInject, ceiling: hookBudgetCeiling{Requests: 27, Opens: 1, SessionGets: 1, GetMisses: 4}, wantStdout: "for my predecessor"},
		{name: "nudge drain --inject/empty", seed: none, run: nudgeDrainInject, ceiling: hookBudgetCeiling{Requests: 7, Opens: 1, SessionGets: 0, GetMisses: 0}},
		{name: "nudge drain --inject/empty with usage stdin", seed: none, run: nudgeDrainInjectWithUsage, ceiling: hookBudgetCeiling{Requests: 9, Opens: 1, SessionGets: 1, GetMisses: 0}},
		{name: "nudge drain --inject/active step", seed: func(t *testing.T, f hookBudgetFixture) { f.activeStep(t) }, run: nudgeDrainInject, ceiling: hookBudgetCeiling{Requests: 8, Opens: 1, SessionGets: 1, GetMisses: 0}, wantStdout: "inspect"},
		{name: "nudge drain --inject/one nudge", seed: func(t *testing.T, f hookBudgetFixture) { f.queueNudge(t) }, run: nudgeDrainInject, ceiling: hookBudgetCeiling{Requests: 13, Opens: 1, SessionGets: 1, GetMisses: 0}, wantStdout: "review queued work"},
		{name: "prime --hook/fresh", seed: none, run: primeHook, ceiling: hookBudgetCeiling{Requests: 11, Opens: 1, SessionGets: 1, GetMisses: 0}},
		{name: "prime --hook/auto-handoff", seed: handoff, run: primeHook, ceiling: hookBudgetCeiling{Requests: 13, Opens: 1, SessionGets: 1, GetMisses: 0}, wantStdout: "context cycle"},
		{name: "prime --hook/auto-handoff and mail", seed: handoffAndMail, run: primeHook, ceiling: hookBudgetCeiling{Requests: 13, Opens: 1, SessionGets: 1, GetMisses: 0}, wantStdout: "status please"},
		{name: "handoff --auto", seed: none, ceiling: hookBudgetCeiling{Requests: 6, Opens: 1, SessionGets: 1, GetMisses: 0}, run: func(_ *testing.T, _ hookBudgetFixture, stdout, stderr io.Writer) int {
			return cmdHandoffWithForce([]string{"context cycle"}, "", true, "", false, stdout, stderr)
		}},
		{name: "mail send human", seed: none, ceiling: hookBudgetCeiling{Requests: 6, Opens: 1, SessionGets: 1, GetMisses: 0}, run: func(_ *testing.T, _ hookBudgetFixture, stdout, stderr io.Writer) int {
			return cmdMailSend([]string{"human"}, false, false, "", "", "done", "work finished", stdout, stderr)
		}},
	}
}

func TestHookRequestBudget(t *testing.T) {
	model := beadstest.WireModel{Handshake: hookBudgetHandshake, LedgerRows: hookBudgetLedgerRows}
	biggerLedger := beadstest.WireModel{Handshake: hookBudgetHandshake, LedgerRows: 10 * hookBudgetLedgerRows}
	hookBudgetUpdateGolden = os.Getenv(hookBudgetGoldenUpdateEnv) != ""
	goldenDir, err := filepath.Abs(filepath.Join("testdata", "hook_budget"))
	if err != nil {
		t.Fatalf("golden dir: %v", err)
	}
	for _, sc := range hookBudgetScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			f := newHookBudgetFixture(t)
			sc.seed(t, f)
			run := recordHookCommand(t, func(stdout, stderr io.Writer) int { return sc.run(t, f, stdout, stderr) })
			if run.code != 0 {
				t.Fatalf("exit %d; stderr=%s", run.code, run.stderr)
			}
			if sc.wantStdout != "" && !strings.Contains(run.stdout, sc.wantStdout) {
				t.Fatalf("stdout does not carry %q; the scenario did not take its path:\n%s", sc.wantStdout, run.stdout)
			}
			cost := model.Price(run.opens, run.ops)
			t.Logf("%s", cost)
			if slices.ContainsFunc(run.ops, func(op beadstest.RecordedOp) bool { return op.Kind == beadstest.OpReady }) {
				t.Errorf("recorded a Ready, which beadstest.WireModel prices at a 1-request floor; price Ready there before budgeting it")
			}
			assertHookBudgetGolden(t, goldenDir, sc.name, f, run)
			if cost.Unkeyed != 0 {
				t.Errorf("%d listing requests walk the whole ledger, want none", cost.Unkeyed)
			}
			if bigger := biggerLedger.Price(run.opens, run.ops); bigger.Requests != cost.Requests {
				t.Errorf("modeled requests grow with the ledger: %d at %d rows, %d at %d rows",
					cost.Requests, model.LedgerRows, bigger.Requests, biggerLedger.LedgerRows)
			}
			if cost.Opens > sc.ceiling.Opens {
				t.Errorf("opened %d stores, ceiling %d", cost.Opens, sc.ceiling.Opens)
			}
			if cost.Requests > sc.ceiling.Requests {
				t.Errorf("modeled %d requests at %d ledger rows, ceiling %d", cost.Requests, hookBudgetLedgerRows, sc.ceiling.Requests)
			}
			if cost.GetMisses > sc.ceiling.GetMisses {
				t.Errorf("%d Gets missed (an address probed as a bead id), ceiling %d", cost.GetMisses, sc.ceiling.GetMisses)
			}
			if got := countGetsOf(run.ops, f.sessionID); got > sc.ceiling.SessionGets {
				t.Errorf("read the session bead %s %d times, ceiling %d", f.sessionID, got, sc.ceiling.SessionGets)
			}
			if t.Failed() || testing.Verbose() {
				t.Logf("recorded operations:\n%s", describeHookBudgetOps(run.ops))
			}
		})
	}
}

// countGetsOf counts the recorded Gets of one bead.
func countGetsOf(ops []beadstest.RecordedOp, id string) int {
	n := 0
	for _, op := range ops {
		if op.Kind == beadstest.OpGet && op.ID == id {
			n++
		}
	}
	return n
}

func describeHookBudgetOps(ops []beadstest.RecordedOp) string {
	var b strings.Builder
	for _, op := range ops {
		b.WriteString("  ")
		b.WriteString(op.Kind)
		if op.Method != "" && op.Method != op.Kind {
			b.WriteString(" via " + op.Method)
		}
		if op.ID != "" {
			b.WriteString(" " + op.ID)
		}
		if op.Kind == beadstest.OpList {
			b.WriteString(" {" + beadstest.DescribeListQuery(op.Query) + "}")
		}
		if op.Err != nil {
			b.WriteString(" err=" + op.Err.Error())
		}
		b.WriteString("\n")
		if len(op.Caller) > 0 {
			b.WriteString("      from " + strings.Join(op.Caller, " < ") + "\n")
		}
	}
	return b.String()
}

// TestHookVerbsOpenOneStoreThroughRun runs hook verbs through run(), the real
// entry point, so the store memo is the one run() opens for them rather than
// the one recordHookCommand opens.
func TestHookVerbsOpenOneStoreThroughRun(t *testing.T) {
	for _, args := range [][]string{
		{"mail", "check", "--inject"},
		{"nudge", "drain", "--inject"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			newHookBudgetFixture(t)
			resetHookBudgetProcessState(t)
			var opens int
			prev := observeOpenedStore
			observeOpenedStore = func(_ string, store beads.Store) beads.Store {
				opens++
				return store
			}
			t.Cleanup(func() { observeOpenedStore = prev })
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 0 {
				t.Fatalf("gc %s = %d; stderr=%s", strings.Join(args, " "), code, stderr.String())
			}
			if opens != 1 {
				t.Fatalf("gc %s opened %d stores, want 1", strings.Join(args, " "), opens)
			}
			if activeInvocationStoreMemo() != nil {
				t.Fatal("run() left the invocation store memo active")
			}
		})
	}
}

// hookBudgetGoldenUpdateEnv names the variable that rewrites the hook output
// goldens from the current output instead of comparing against them.
const hookBudgetGoldenUpdateEnv = "HOOK_BUDGET_UPDATE_GOLDEN"

var (
	hookBudgetClockLine = regexp.MustCompile(`(?m)^Current time: .*$`)
	hookBudgetPrimeHead = regexp.MustCompile(`^\[([^\]]+)\] (\S+) • \S+`)
	hookBudgetReminder  = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
)

// normalizeHookBudgetOutput makes one hook's output comparable across runs:
// the clock line is masked, and a prime hook's additionalContext keeps only its
// header and the system reminders the hook appends to the prompt. The rendered
// prompt body is the pack's, not this hook's, and is not pinned here.
func normalizeHookBudgetOutput(t *testing.T, stdout string) string {
	t.Helper()
	trimmed := strings.TrimSpace(stdout)
	if strings.HasPrefix(trimmed, "{") {
		var payload struct {
			HookSpecificOutput struct {
				AdditionalContext string `json:"additionalContext"`
				HookEventName     string `json:"hookEventName"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
			t.Fatalf("decoding hook JSON output: %v\n%s", err, stdout)
		}
		ctx := payload.HookSpecificOutput.AdditionalContext
		var b strings.Builder
		fmt.Fprintf(&b, "hookEventName: %s\n", payload.HookSpecificOutput.HookEventName)
		fmt.Fprintf(&b, "header: %s\n", hookBudgetPrimeHead.ReplaceAllString(strings.SplitN(ctx, "\n", 2)[0], "[$1] $2 • <now>"))
		for _, reminder := range hookBudgetReminder.FindAllString(ctx, -1) {
			b.WriteString(reminder)
			b.WriteString("\n")
		}
		return b.String()
	}
	return hookBudgetClockLine.ReplaceAllString(stdout, "Current time: <now>")
}

// hookBudgetUpdateGolden is read once, before any fixture clears the
// environment.
var hookBudgetUpdateGolden bool

// assertHookBudgetGolden compares a scenario's normalized output — stdout,
// then stderr when the hook wrote any — with its golden, so a change that
// lowers a hook's request count is shown to leave what the hook says, to the
// agent and to the operator, byte-identical.
func assertHookBudgetGolden(t *testing.T, goldenDir, scenario string, f hookBudgetFixture, run hookBudgetRun) {
	t.Helper()
	name := strings.NewReplacer(" ", "_", "/", "__", "-", "").Replace(scenario)
	path := filepath.Join(goldenDir, name+".golden")
	got := normalizeHookBudgetOutput(t, run.stdout)
	if run.stderr != "" {
		got += "--- stderr ---\n" + strings.ReplaceAll(run.stderr, f.cityDir, "<city>")
	}
	if hookBudgetUpdateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s (set %s=1 to write it): %v", path, hookBudgetGoldenUpdateEnv, err)
	}
	if got != string(want) {
		t.Errorf("hook output differs from %s:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
