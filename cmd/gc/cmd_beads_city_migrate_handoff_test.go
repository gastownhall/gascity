package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bdHandoffCall records one invocation of bd, so a test can assert the ORDER
// the phases ran in as well as their arguments. Order is most of the contract:
// a prepare after the stop snapshots a workspace that has already changed, and
// a legacy-gone before the stop proves nothing.
type bdHandoffCall struct {
	Verb string
	Args []string
}

// legacyHandoffFixtureCity is a legacy GC-managed city with a live published
// runtime state — the only shape this command accepts.
func legacyHandoffFixtureCity(t *testing.T) string {
	t.Helper()
	city, _ := newLegacyManagedCityFixture(t)
	writeManagedDoltStateFile(t, city)
	setHandoffFixtureProjectID(t, city, "workspace-uuid")
	return city
}

// setHandoffFixtureProjectID gives the fixture the project_id bd verifies the
// --workspace argument against. A legacy city has one; the shared fixture
// predates the field.
func setHandoffFixtureProjectID(t *testing.T, scopeRoot, projectID string) {
	t.Helper()
	path := filepath.Join(scopeRoot, ".beads", "metadata.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	meta["project_id"] = projectID
	writeScopeMetadataRaw(t, scopeRoot, meta)
}

func setHandoffFixtureDoltMode(t *testing.T, scopeRoot, mode string) {
	t.Helper()
	path := filepath.Join(scopeRoot, ".beads", "metadata.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	meta["dolt_mode"] = mode
	writeScopeMetadataRaw(t, scopeRoot, meta)
}

// stubMigrateHandoffBd replaces bd and both process seams. answer is asked for
// each verb and returns the object bd would print plus whether it exited
// non-zero.
func stubMigrateHandoffBd(t *testing.T, answer func(verb string, args []string) (bdHandoffResult, error)) (*[]bdHandoffCall, *[]string) {
	t.Helper()
	calls := &[]bdHandoffCall{}
	order := &[]string{}

	previousBd := runBdScopeCommand
	runBdScopeCommand = func(_, _ string, args ...string) ([]byte, error) {
		verb := ""
		if len(args) >= 3 {
			verb = args[2]
		}
		*calls = append(*calls, bdHandoffCall{Verb: verb, Args: args})
		*order = append(*order, "bd:"+verb)
		result, err := answer(verb, args)
		result.SchemaVersion = 2
		body, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return body, err
	}
	previousStop := migrateHandoffStopLegacy
	migrateHandoffStopLegacy = func(_ string, endpoint legacyHandoffEndpoint) migrateHandoffStep {
		*order = append(*order, "gc:stop")
		return migrateHandoffStep{
			Step: "stop-legacy", Actor: "gc", Status: migrateHandoffStepOK,
			Detail: "stopped " + endpoint.String(),
		}
	}
	previousRestart := migrateHandoffRestartLegacy
	migrateHandoffRestartLegacy = func(string, legacyHandoffEndpoint) migrateHandoffStep {
		*order = append(*order, "gc:restart")
		return migrateHandoffStep{Step: "restart-legacy", Actor: "gc", Status: migrateHandoffStepOK}
	}
	t.Cleanup(func() {
		runBdScopeCommand = previousBd
		migrateHandoffStopLegacy = previousStop
		migrateHandoffRestartLegacy = previousRestart
	})
	return calls, order
}

// handoffPhaseFor maps a verb to the phase a successful run reaches, so a stub
// bd can answer the way the real one does without restating the table.
var handoffPhaseFor = map[string]string{
	"prepare": "prepared", "legacy-gone": "old_owner_stopped", "configure": "target_configured",
	"verify": "verified", "commit": "committed",
	"rollback": "legacy_config_restored", "rollback-finish": "rolled_back",
}

// happyBdHandoff answers every verb the way a successful transfer does.
func happyBdHandoff(verb string, _ []string) (bdHandoffResult, error) {
	result := bdHandoffResult{Phase: handoffPhaseFor[verb], Owner: "legacy-gc"}
	if verb == "commit" {
		result.Owner = "bd"
	}
	if verb == "configure" {
		result.Target.PID, result.Target.Host, result.Target.Port = 4242, "127.0.0.1", 3399
	}
	return result, nil
}

func runMigrateHandoffJSON(t *testing.T, city string, opts migrateHandoffOptions) (int, migrateHandoffReport, string) {
	t.Helper()
	opts.JSON = true
	var stdout, stderr bytes.Buffer
	code := doBeadsCityMigrateHandoff(city, opts, &stdout, &stderr)
	var report migrateHandoffReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &report); err != nil {
		t.Fatalf("decode report: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	return code, report, stderr.String()
}

func handoffStep(t *testing.T, report migrateHandoffReport, name string) migrateHandoffStep {
	t.Helper()
	for _, step := range report.Steps {
		if step.Step == name {
			return step
		}
	}
	t.Fatalf("report has no %q step: %+v", name, report.Steps)
	return migrateHandoffStep{}
}

func migrateHandoffRollbackFinishBackoffForTest(t *testing.T, backoff time.Duration) {
	t.Helper()
	previousFinish := migrateHandoffRollbackFinishBackoff
	previousBusy := migrateHandoffJournalBusyBackoff
	migrateHandoffRollbackFinishBackoff = backoff
	migrateHandoffJournalBusyBackoff = backoff
	t.Cleanup(func() {
		migrateHandoffRollbackFinishBackoff = previousFinish
		migrateHandoffJournalBusyBackoff = previousBusy
	})
}

// gc runs every bd child in its own process group, so an operator who kills gc
// leaves the verb it had started running to completion — and re-running then
// meets the journal lock that verb still holds. journal_busy is "wait", not
// "no", and waiting is the command's job.
func TestMigrateHandoffWaitsOutABusyJournal(t *testing.T) {
	city := legacyHandoffFixtureCity(t)
	busy := 0
	calls, _ := stubMigrateHandoffBd(t, func(verb string, args []string) (bdHandoffResult, error) {
		if verb == "prepare" && busy < 3 {
			busy++
			return bdHandoffResult{
				ErrorCode: "journal_busy",
				Error:     "another ownership handoff verb holds the journal",
			}, errors.New("exit 1")
		}
		return happyBdHandoff(verb, args)
	})
	migrateHandoffRollbackFinishBackoffForTest(t, 0)

	code, report, stderr := runMigrateHandoffJSON(t, city, migrateHandoffOptions{})
	if code != 0 {
		t.Fatalf("a transfer behind a busy journal = %d, want 0\nstderr=%s", code, stderr)
	}
	if report.Status != migrateHandoffStatusHandedOff {
		t.Fatalf("report status = %q, want %q", report.Status, migrateHandoffStatusHandedOff)
	}
	if got := handoffStep(t, report, "prepare").Status; got != migrateHandoffStepOK {
		t.Fatalf("prepare status = %q after the lock cleared, want ok", got)
	}
	prepares := 0
	for _, call := range *calls {
		if call.Verb == "prepare" {
			prepares++
		}
	}
	if prepares != busy+1 {
		t.Fatalf("prepare ran %d times, want %d (once per busy answer, then once more)", prepares, busy+1)
	}
}

// The wait is bounded. A journal that is busy forever is a report, not a hang.
func TestMigrateHandoffBoundsTheBusyJournalWait(t *testing.T) {
	city := legacyHandoffFixtureCity(t)
	attempts := 0
	_, _ = stubMigrateHandoffBd(t, func(verb string, args []string) (bdHandoffResult, error) {
		if verb == "prepare" {
			attempts++
			return bdHandoffResult{
				ErrorCode: "journal_busy",
				Error:     "another ownership handoff verb holds the journal",
			}, errors.New("exit 1")
		}
		return happyBdHandoff(verb, args)
	})
	migrateHandoffRollbackFinishBackoffForTest(t, 0)

	code, report, _ := runMigrateHandoffJSON(t, city, migrateHandoffOptions{})
	if code != 1 {
		t.Fatalf("a journal busy forever = %d, want 1", code)
	}
	if attempts != migrateHandoffJournalBusyAttempts {
		t.Fatalf("prepare ran %d times, want the bound of %d", attempts, migrateHandoffJournalBusyAttempts)
	}
	if got := handoffStep(t, report, "prepare").ErrorCode; got != "journal_busy" {
		t.Fatalf("the exhausted wait reports %q, want journal_busy", got)
	}
}

func TestNewBeadsCmdIncludesCityMigrateHandoff(t *testing.T) {
	cmd := newBeadsCmd(&bytes.Buffer{}, &bytes.Buffer{})
	migrate, _, err := cmd.Find([]string{"city", "migrate-handoff"})
	if err != nil {
		t.Fatalf("Find(city migrate-handoff): %v", err)
	}
	if migrate == nil || migrate.Name() != "migrate-handoff" {
		t.Fatalf("migrate-handoff command = %#v", migrate)
	}
	// The same conventions as its sibling migrate-proxied, so an operator who
	// knows one knows the other.
	for _, flag := range []string{"json", "dry-run", "rig"} {
		if migrate.Flags().Lookup(flag) == nil {
			t.Errorf("missing --%s flag", flag)
		}
	}
}

// P5: city root only. The refusal has to answer the question the operator
// actually has, which is "then what happens to my rigs" — not just "no".
func TestMigrateHandoffRefusesARig(t *testing.T) {
	city := legacyHandoffFixtureCity(t)
	stubMigrateHandoffBd(t, func(verb string, _ []string) (bdHandoffResult, error) {
		t.Fatalf("bd must not run for a refused rig request: %s", verb)
		return bdHandoffResult{}, nil
	})

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateHandoff(city, migrateHandoffOptions{Rigs: []string{"spike"}}, &stdout, &stderr); code != 1 {
		t.Fatalf("doBeadsCityMigrateHandoff() = %d, want 1\nstderr=%s", code, stderr.String())
	}
	for _, want := range []string{"city-root only", "canonical endpoint", "bd-qvjt"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the rig refusal does not mention %q:\n%s", want, stderr.String())
		}
	}
}

// Only a legacy GC-managed direct city with a live server is gc's to hand over.
// Every other shape is refused by name, before bd is invoked at all.
func TestMigrateHandoffRefusesAScopeThatIsNotGCsToHandOver(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(t *testing.T, city string)
		want  string
	}{
		"already proxied": {
			setup: func(t *testing.T, city string) { setHandoffFixtureDoltMode(t, city, "proxied-server") },
			want:  "proxied-server topology",
		},
		"embedded": {
			setup: func(t *testing.T, city string) { setHandoffFixtureDoltMode(t, city, "embedded") },
			want:  "embedded Dolt scope",
		},
		"already handed over": {
			setup: func(t *testing.T, city string) { writeCommittedHandoffJournal(t, city) },
			want:  "committed",
		},
		"gc never started it": {
			setup: func(t *testing.T, city string) {
				if err := os.Remove(managedDoltStatePath(city)); err != nil {
					t.Fatal(err)
				}
			},
			want: "start the city first",
		},
		"gc's server is not running": {
			setup: func(t *testing.T, city string) {
				if err := os.WriteFile(managedDoltStatePath(city), []byte(`{"running":false,"pid":0,"port":0}`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "no live server",
		},
		"no project identity to verify against": {
			setup: func(t *testing.T, city string) { setHandoffFixtureProjectID(t, city, "") },
			want:  "project_id",
		},
	} {
		t.Run(name, func(t *testing.T) {
			city := legacyHandoffFixtureCity(t)
			stubMigrateHandoffBd(t, func(verb string, _ []string) (bdHandoffResult, error) {
				t.Fatalf("bd ran for a scope gc refused: %s", verb)
				return bdHandoffResult{}, nil
			})
			tc.setup(t, city)
			var stdout, stderr bytes.Buffer
			code := doBeadsCityMigrateHandoff(city, migrateHandoffOptions{}, &stdout, &stderr)
			if name == "already handed over" {
				// A committed journal is not a refusal, it is the no-op this
				// command is idempotent about.
				if code != 0 {
					t.Fatalf("a committed handoff was re-run as an error: %d\nstderr=%s", code, stderr.String())
				}
				if !strings.Contains(stdout.String(), migrateHandoffStatusAlready) {
					t.Fatalf("stdout does not report the transfer as already done:\n%s", stdout.String())
				}
				return
			}
			if code != 1 {
				t.Fatalf("doBeadsCityMigrateHandoff() = %d, want 1\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("refusal does not name %q:\n%s", tc.want, stderr.String())
			}
		})
	}
}

func TestMigrateHandoffDryRunRunsNothing(t *testing.T) {
	city := legacyHandoffFixtureCity(t)
	stubMigrateHandoffBd(t, func(verb string, _ []string) (bdHandoffResult, error) {
		t.Fatalf("a dry run invoked bd: %s", verb)
		return bdHandoffResult{}, nil
	})
	before := mustReadFile(t, filepath.Join(city, ".beads", "metadata.json"))
	state := mustReadFile(t, managedDoltStatePath(city))

	code, report, stderr := runMigrateHandoffJSON(t, city, migrateHandoffOptions{DryRun: true})
	if code != 0 {
		t.Fatalf("dry run = %d, want 0\nstderr=%s", code, stderr)
	}
	if report.Status != migrateHandoffStatusPlanned || !report.DryRun {
		t.Fatalf("report = %+v", report)
	}
	for _, step := range report.Steps {
		if step.Status != migrateHandoffStepPlanned {
			t.Errorf("%s status = %q, want %q", step.Step, step.Status, migrateHandoffStepPlanned)
		}
	}
	if got := mustReadFile(t, filepath.Join(city, ".beads", "metadata.json")); !bytes.Equal(got, before) {
		t.Error("a dry run rewrote the city metadata")
	}
	if got := mustReadFile(t, managedDoltStatePath(city)); !bytes.Equal(got, state) {
		t.Error("a dry run retired gc's runtime publication")
	}
}

// The order is the contract. prepare snapshots the workspace while the legacy
// server is still up; the stop happens between prepare and legacy-gone, because
// bd cannot stop it and must not be asked to.
func TestMigrateHandoffDrivesBdsPhasesInOrder(t *testing.T) {
	city := legacyHandoffFixtureCity(t)
	calls, order := stubMigrateHandoffBd(t, happyBdHandoff)

	code, report, stderr := runMigrateHandoffJSON(t, city, migrateHandoffOptions{})
	if code != 0 {
		t.Fatalf("migrate-handoff = %d, want 0\nstderr=%s\nreport=%+v", code, stderr, report)
	}
	if report.Status != migrateHandoffStatusHandedOff {
		t.Fatalf("report status = %q, want %q", report.Status, migrateHandoffStatusHandedOff)
	}
	want := []string{"bd:prepare", "gc:stop", "bd:legacy-gone", "bd:configure", "bd:verify", "bd:commit"}
	if strings.Join(*order, ",") != strings.Join(want, ",") {
		t.Fatalf("phase order = %v, want %v", *order, want)
	}

	prepare := (*calls)[0]
	joined := strings.Join(prepare.Args, " ")
	for _, want := range []string{
		"migrate ownership-handoff prepare --json",
		"--root " + city,
		"--legacy-endpoint 127.0.0.1:35402",
		"--legacy-pid " + fmt.Sprint(os.Getpid()),
		"--caller gc",
		"--database hq",
		"--workspace workspace-uuid",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("prepare args %q do not contain %q", joined, want)
		}
	}
	// bd never calls gc, so it is never told where gc is. A GC_BIN on this
	// command line would be the withdrawn protocol coming back.
	for _, call := range *calls {
		if strings.Contains(strings.Join(call.Args, " "), "GC_BIN") {
			t.Errorf("%s was handed a GC_BIN: %v", call.Verb, call.Args)
		}
	}
	// The whole request goes on EVERY verb, not just the first. bd rebuilds it
	// from the command line each time and checks it against the journal, so a
	// verb missing a half of it is not a smaller request — it is an invalid one,
	// and the transfer stops with gc's server already down.
	for _, call := range *calls {
		joined := strings.Join(call.Args, " ")
		for _, want := range []string{
			"--root " + city,
			"--database hq",
			"--workspace workspace-uuid",
			"--legacy-endpoint 127.0.0.1:35402",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s was invoked without %q: %v", call.Verb, want, call.Args)
			}
		}
	}
	// The pid is a hint and is only ever offered at prepare, where bd records
	// it beside the identity it resolved for itself. Repeating it later
	// re-asserts a belief about a process that is supposed to be gone.
	for _, call := range (*calls)[1:] {
		if strings.Contains(strings.Join(call.Args, " "), "--legacy-pid") {
			t.Errorf("%s repeated the pid hint: %v", call.Verb, call.Args)
		}
	}
	// gc cannot mint bd's process-birth identity — it is bd's own
	// platform-versioned format — and the contract makes hints optional so a
	// caller can decline to invent one rather than pass something plausible.
	for _, call := range *calls {
		if strings.Contains(strings.Join(call.Args, " "), "--legacy-pid-birth") {
			t.Errorf("%s passed a birth identity gc has no way to compute: %v", call.Verb, call.Args)
		}
	}
}

// A re-run is a resume, not a restart. Everything bd's journal already records
// is reported and skipped — including gc's own stop, which must not run again
// against a server that is already gone.
func TestMigrateHandoffResumesFromBdsJournal(t *testing.T) {
	city := legacyHandoffFixtureCity(t)
	writeHandoffJournal(t, city, pendingHandoffJournal(city, "old_owner_stopped"))
	_, order := stubMigrateHandoffBd(t, happyBdHandoff)

	code, report, stderr := runMigrateHandoffJSON(t, city, migrateHandoffOptions{})
	if code != 0 {
		t.Fatalf("resume = %d, want 0\nstderr=%s", code, stderr)
	}
	want := []string{"bd:configure", "bd:verify", "bd:commit"}
	if strings.Join(*order, ",") != strings.Join(want, ",") {
		t.Fatalf("resume ran %v, want %v", *order, want)
	}
	for _, name := range []string{"prepare", "stop-legacy", "legacy-gone"} {
		if got := handoffStep(t, report, name).Status; got != migrateHandoffStepSkipped {
			t.Errorf("%s status = %q on a resume, want %q", name, got, migrateHandoffStepSkipped)
		}
	}
}

// Anything that fails after gc's server is stopped leaves the city owned by
// nobody. The compensation is not optional and it is not the operator's job.
func TestMigrateHandoffRollsBackAFailureAfterTheStop(t *testing.T) {
	city := legacyHandoffFixtureCity(t)
	_, order := stubMigrateHandoffBd(t, func(verb string, args []string) (bdHandoffResult, error) {
		if verb == "verify" {
			return bdHandoffResult{
				Phase: "target_configured", Owner: "legacy-gc",
				ErrorCode: "sentinel_mismatch", Error: "the replacement serves different data",
			}, errors.New("exit 1")
		}
		return happyBdHandoff(verb, args)
	})

	code, report, stderr := runMigrateHandoffJSON(t, city, migrateHandoffOptions{})
	if code != 1 {
		t.Fatalf("a failed transfer = %d, want 1\nstderr=%s", code, stderr)
	}
	if report.Status != migrateHandoffStatusRolledBack {
		t.Fatalf("report status = %q, want %q\n%+v", report.Status, migrateHandoffStatusRolledBack, report.Steps)
	}
	want := []string{
		"bd:prepare", "gc:stop", "bd:legacy-gone", "bd:configure", "bd:verify",
		"bd:rollback", "gc:restart", "bd:rollback-finish",
	}
	if strings.Join(*order, ",") != strings.Join(want, ",") {
		t.Fatalf("compensation ran %v, want %v", *order, want)
	}
	verify := handoffStep(t, report, "verify")
	if verify.ErrorCode != "sentinel_mismatch" {
		t.Errorf("the failing step does not carry bd's error_code: %+v", verify)
	}
	if !strings.Contains(stderr, "sentinel_mismatch") {
		t.Errorf("stderr does not name bd's refusal:\n%s", stderr)
	}
}

// gc knows what it asked for, not what happened. A stop that reports failure is
// reported — and then bd is asked, because bd is the side that can prove it.
func TestMigrateHandoffReportsItsOwnStopFailureAndBdsVerdict(t *testing.T) {
	city := legacyHandoffFixtureCity(t)
	_, order := stubMigrateHandoffBd(t, func(verb string, args []string) (bdHandoffResult, error) {
		if verb == "legacy-gone" {
			return bdHandoffResult{
				Phase: "prepared", Owner: "legacy-gc",
				ErrorCode: "legacy_alive", Error: "the legacy owner is still running",
				Evidence: map[string]struct {
					Gates      map[string]string `json:"gates"`
					PortHolder string            `json:"port_holder"`
				}{
					"prepared": {Gates: map[string]string{"endpoint_quiet": "passed", "data_dir_lock": "failed"}, PortHolder: "proc"},
				},
			}, errors.New("exit 1")
		}
		return happyBdHandoff(verb, args)
	})
	migrateHandoffStopLegacy = func(string, legacyHandoffEndpoint) migrateHandoffStep {
		*order = append(*order, "gc:stop")
		return migrateHandoffStep{
			Step: "stop-legacy", Actor: "gc", Status: migrateHandoffStepFailed,
			Error: "dolt process 41 exited but the data dir is not yet released",
		}
	}

	code, report, stderr := runMigrateHandoffJSON(t, city, migrateHandoffOptions{})
	if code != 1 {
		t.Fatalf("a transfer whose stop failed = %d, want 1\nstderr=%s", code, stderr)
	}
	stop := handoffStep(t, report, "stop-legacy")
	if stop.Status != migrateHandoffStepFailed || !strings.Contains(stop.Error, "not yet released") {
		t.Fatalf("gc's own stop failure is not in the report: %+v", stop)
	}
	gone := handoffStep(t, report, "legacy-gone")
	if gone.ErrorCode != "legacy_alive" {
		t.Fatalf("bd's verdict is not in the report: %+v", gone)
	}
	// The evidence is the point: an operator should not have to read bd's
	// journal to learn which gate said the owner was alive.
	if gone.Evidence["data_dir_lock"] != "failed" || gone.Evidence["port_holder"] != "proc" {
		t.Errorf("the refusal carries no usable evidence: %+v", gone.Evidence)
	}
	if !strings.Contains(stderr, "gc did not stop its own server") {
		t.Errorf("stderr does not connect gc's failure to bd's verdict:\n%s", stderr)
	}
	// And the fence has an exit: a journal left at prepared refuses every gc
	// lifecycle command on this city, so the compensation still runs.
	if !strings.Contains(strings.Join(*order, ","), "bd:rollback,gc:restart,bd:rollback-finish") {
		t.Fatalf("a legacy_alive refusal left bd's journal in place: %v", *order)
	}
}

// A journal on the rollback track is finished as a rollback, never resumed
// forward: whatever went wrong the first time is still what happened.
func TestMigrateHandoffResumesARollbackInProgress(t *testing.T) {
	city := legacyHandoffFixtureCity(t)
	journal := pendingHandoffJournal(city, "legacy_config_restored")
	journal.Request.Endpoint.Port = 35402
	journal.Request.Database = "from-the-journal"
	writeHandoffJournal(t, city, journal)
	calls, order := stubMigrateHandoffBd(t, happyBdHandoff)

	code, report, stderr := runMigrateHandoffJSON(t, city, migrateHandoffOptions{})
	if code != 1 {
		t.Fatalf("resuming a rollback = %d, want 1 (a rolled-back transfer is still a failed one)\nstderr=%s", code, stderr)
	}
	if report.Status != migrateHandoffStatusRolledBack {
		t.Fatalf("report status = %q, want %q", report.Status, migrateHandoffStatusRolledBack)
	}
	want := []string{"bd:rollback", "gc:restart", "bd:rollback-finish"}
	if strings.Join(*order, ",") != strings.Join(want, ",") {
		t.Fatalf("rollback resume ran %v, want %v", *order, want)
	}
	// The request comes back out of the journal, not out of a fresh
	// classification. gc's own publication is retired by this point, so there
	// is nothing left to re-derive it from — and bd refuses a request that
	// disagrees with its journal, which gc must not be the one to cause.
	for _, call := range *calls {
		if !strings.Contains(strings.Join(call.Args, " "), "--database from-the-journal") {
			t.Errorf("%s was resumed with a request gc invented rather than the journal's: %v", call.Verb, call.Args)
		}
	}
}

// rollback-finish refuses without advancing until the legacy owner is back, so
// the retry is bounded and its exhaustion is a report rather than a hang.
func TestMigrateHandoffBoundsTheRollbackFinishRetry(t *testing.T) {
	city := legacyHandoffFixtureCity(t)
	journal := pendingHandoffJournal(city, "legacy_config_restored")
	journal.Request.Endpoint.Port = 35402
	writeHandoffJournal(t, city, journal)
	attempts := 0
	_, _ = stubMigrateHandoffBd(t, func(verb string, args []string) (bdHandoffResult, error) {
		if verb == "rollback-finish" {
			attempts++
			return bdHandoffResult{
				Phase: "legacy_config_restored", Owner: "legacy-gc",
				ErrorCode: "legacy_not_back", Error: "the legacy endpoint does not answer",
			}, errors.New("exit 1")
		}
		return happyBdHandoff(verb, args)
	})
	migrateHandoffRollbackFinishBackoffForTest(t, 0)

	code, report, stderr := runMigrateHandoffJSON(t, city, migrateHandoffOptions{})
	if code != 1 {
		t.Fatalf("an unadmitted rollback = %d, want 1\nstderr=%s", code, stderr)
	}
	if attempts != migrateHandoffRollbackFinishAttempts {
		t.Fatalf("rollback-finish ran %d times, want the bound of %d", attempts, migrateHandoffRollbackFinishAttempts)
	}
	finish := handoffStep(t, report, "rollback-finish")
	if finish.ErrorCode != "legacy_not_back" || !strings.Contains(finish.Detail, "re-run this command") {
		t.Fatalf("the exhausted retry does not say how to continue: %+v", finish)
	}
}
