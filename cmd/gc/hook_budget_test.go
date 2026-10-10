package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// The hook request budget (engdocs/design/worker-hook-request-budget.md).
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
// on the store opens. The ceilings are ratchets: each slice of the design
// lowers them.
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

func (f hookBudgetFixture) sendMail(t *testing.T, from, to, subject string) {
	t.Helper()
	mp, code := openCityMailProvider(io.Discard, "hook budget fixture")
	if code != 0 {
		t.Fatalf("openCityMailProvider = %d", code)
	}
	if _, err := mp.Send(from, to, subject, "body of "+subject); err != nil {
		t.Fatalf("Send(%s -> %s): %v", from, to, err)
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
	prev := observeOpenedStore
	observeOpenedStore = func(_ string, store beads.Store) beads.Store {
		rec := beadstest.NewOpRecordingStore(store)
		mu.Lock()
		recorders = append(recorders, rec)
		mu.Unlock()
		return rec
	}
	var stdout, stderr bytes.Buffer
	code := fn(&stdout, &stderr)
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
// which no longer depends on the ledger size; Opens bounds store opens.
type hookBudgetCeiling struct {
	Requests int
	Opens    int
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
	oneMail := func(t *testing.T, f hookBudgetFixture) { f.sendMail(t, "human", hookBudgetIdentity, "status please") }
	handoff := func(t *testing.T, f hookBudgetFixture) { f.autoHandoff(t) }
	return []hookBudgetScenario{
		{name: "mail check --inject/empty", seed: none, run: mailCheckInject, ceiling: hookBudgetCeiling{Requests: 21, Opens: 2}},
		{name: "mail check --inject/one mail", seed: oneMail, run: mailCheckInject, ceiling: hookBudgetCeiling{Requests: 23, Opens: 2}, wantStdout: "status please"},
		{name: "mail check --inject/auto-handoff", seed: handoff, run: mailCheckInject, ceiling: hookBudgetCeiling{Requests: 27, Opens: 2}, wantStdout: "context cycle"},
		{name: "nudge drain --inject/empty", seed: none, run: nudgeDrainInject, ceiling: hookBudgetCeiling{Requests: 13, Opens: 1}},
		{name: "nudge drain --inject/empty with usage stdin", seed: none, run: nudgeDrainInjectWithUsage, ceiling: hookBudgetCeiling{Requests: 20, Opens: 2}},
		{name: "nudge drain --inject/active step", seed: func(t *testing.T, f hookBudgetFixture) { f.activeStep(t) }, run: nudgeDrainInject, ceiling: hookBudgetCeiling{Requests: 13, Opens: 2}, wantStdout: "inspect"},
		{name: "nudge drain --inject/one nudge", seed: func(t *testing.T, f hookBudgetFixture) { f.queueNudge(t) }, run: nudgeDrainInject, ceiling: hookBudgetCeiling{Requests: 33, Opens: 5}, wantStdout: "review queued work"},
		{name: "prime --hook/fresh", seed: none, run: primeHook, ceiling: hookBudgetCeiling{Requests: 47, Opens: 3}},
		{name: "prime --hook/auto-handoff", seed: handoff, run: primeHook, ceiling: hookBudgetCeiling{Requests: 53, Opens: 3}, wantStdout: "context cycle"},
		{name: "handoff --auto", seed: none, ceiling: hookBudgetCeiling{Requests: 8, Opens: 1}, run: func(_ *testing.T, _ hookBudgetFixture, stdout, stderr io.Writer) int {
			return cmdHandoffWithForce([]string{"context cycle"}, "", true, "", false, stdout, stderr)
		}},
		{name: "mail send human", seed: none, ceiling: hookBudgetCeiling{Requests: 17, Opens: 2}, run: func(_ *testing.T, _ hookBudgetFixture, stdout, stderr io.Writer) int {
			return cmdMailSend([]string{"human"}, false, false, "", "", "done", "work finished", stdout, stderr)
		}},
	}
}

func TestHookRequestBudget(t *testing.T) {
	model := beadstest.WireModel{Handshake: hookBudgetHandshake, LedgerRows: hookBudgetLedgerRows}
	biggerLedger := beadstest.WireModel{Handshake: hookBudgetHandshake, LedgerRows: 10 * hookBudgetLedgerRows}
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
			if t.Failed() {
				t.Logf("recorded operations:\n%s", describeHookBudgetOps(run.ops))
			}
		})
	}
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
			fmt.Fprintf(&b, " %+v", op.Query)
		}
		if op.Err != nil {
			b.WriteString(" err=" + op.Err.Error())
		}
		b.WriteString("\n")
	}
	return b.String()
}
