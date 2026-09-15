//go:build acceptance_a

// M8: a legacy GC-managed city handed to bd by the journaled ownership
// handoff.
//
// This is the eighth shape of the init/migration matrix and the one the epic
// actually wants: not gc rewriting bd's binding behind its back (M5 → AC-X,
// `gc beads city migrate-proxied`), but the city's Dolt lifecycle transferred
// to bd, journaled, with a compensation that puts it back.
//
// The two halves ship from different repositories and neither calls the other
// in the direction that used to be the design:
//
//   - bd owns the journal, the replacement server, the fences and the
//     rollback. Its verbs are `bd migrate ownership-handoff <phase>`, one
//     journaled phase per invocation, each idempotent. bd spawns nothing but
//     dolt and has no idea gc exists.
//   - gc owns its own server — starting it, stopping it, and knowing whether
//     it did — and drives the sequence. `gc beads city migrate-handoff` is the
//     front door, and `cmd/gc/dolt_handoff_projection.go` is the reader that
//     lets every later gc command see that the scope is no longer gc's.
//
// So there is no GC_BIN anywhere in this file. The earlier shape pinned one so
// bd could spawn gc and take its typed JSON as proof; that protocol is
// withdrawn, and a test that still pinned it would be testing something that
// no longer exists.
//
// Every fault below is a real one for the same reason. The legacy_alive case
// holds a Dolt store lock, which is a thing that genuinely happens to a
// stopping server; the interrupt case kills gc mid-orchestration. Neither
// replaces a binary with a script that lies.
//
// It skips typed against a bd that has no ownership-handoff verbs, which is
// every bd up to and including v1.3.0-rc.2.
//
//	GC_ACCEPTANCE_BD_BIN=<bd with migrate ownership-handoff> \
//	GC_ACCEPTANCE_LEGACY_GC_BIN=<gc that still initialises the old way> \
//	  go test -tags acceptance_a -run TestBeadsOwnershipHandoff ./test/acceptance
//
// The handoff contract is Linux-only (it identifies processes by pidfd and
// /proc start ticks), city-root only, and TCP only. See "The journaled
// ownership handoff (M8)" in engdocs/design/beads-proxied-local-default.md.
package acceptance_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// handoffJournalRelPath is where gc's ownership projection reads the journal.
// It is a contract between the two binaries, not an implementation detail:
// committedBeadsHandoffOwnsScope looks here and nowhere else, and a journal
// written anywhere else is invisible to every gc lifecycle decision that
// follows the handoff.
var handoffJournalRelPath = filepath.Join(".beads", "ownership-handoff.json")

// handoffStepReport is one row of `gc beads city migrate-handoff --json`.
type handoffStepReport struct {
	Step      string            `json:"step"`
	Actor     string            `json:"actor"`
	Status    string            `json:"status"`
	Phase     string            `json:"phase"`
	ErrorCode string            `json:"error_code"`
	Detail    string            `json:"detail"`
	Error     string            `json:"error"`
	Evidence  map[string]string `json:"evidence"`
}

// handoffReport is the whole object that command prints.
type handoffReport struct {
	City     string              `json:"city"`
	DryRun   bool                `json:"dry_run"`
	Endpoint string              `json:"legacy_endpoint"`
	Database string              `json:"database"`
	Status   string              `json:"status"`
	Steps    []handoffStepReport `json:"steps"`
	Failed   int                 `json:"failed"`
}

// ran reports whether a step appears in the report at all, for a case asserting
// presence rather than outcome.
func (r handoffReport) ran(name string) bool {
	for _, step := range r.Steps {
		if step.Step == name {
			return true
		}
	}
	return false
}

func (r handoffReport) step(t *testing.T, name string) handoffStepReport {
	t.Helper()
	for _, step := range r.Steps {
		if step.Step == name {
			return step
		}
	}
	t.Fatalf("the report has no %q step: %+v", name, r.Steps)
	return handoffStepReport{}
}

// handoffJournalDoc is the subset of bd's journal this test reads. The version
// is read with it because the phase names are shared between journal versions
// and do not mean the same thing in them — a phase read without its version is
// a guess, which is the defect the whole v2 gate exists to prevent.
type handoffJournalDoc struct {
	SchemaVersion int    `json:"schema_version"`
	Phase         string `json:"phase"`
	Owner         string `json:"owner"`
	Target        struct {
		PID  int    `json:"pid"`
		Port int    `json:"port"`
		Host string `json:"host"`
	} `json:"target"`
}

// managedDoltRuntimeState is gc's published record of the sql-server it runs
// itself. It is the only place the legacy endpoint is recorded with a live
// port, so it is where the handoff request's endpoint comes from.
type managedDoltRuntimeState struct {
	Running bool   `json:"running"`
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	DataDir string `json:"data_dir"`
}

// requireOwnershipHandoffTooling resolves the three real binaries M8 needs, or
// skips typed naming the one that is missing.
//
// The bd probe is a capability probe, not a version check, and it probes for a
// flag only the inverted verbs have: `--legacy-endpoint` is how gc tells bd
// where its own server listens, and it exists precisely because bd no longer
// asks gc anything. Every bd through v1.3.0-rc.2 skips here, which is what
// keeps this test out of the way of the rc.2 CI tier.
func requireOwnershipHandoffTooling(t *testing.T) (bdPath, doltPath, legacyGC string) {
	t.Helper()
	bdPath, doltPath = requireProxiedTooling(t)
	out, err := exec.Command(bdPath, "migrate", "ownership-handoff", "--help").CombinedOutput() //nolint:gosec // resolved test binary
	if err != nil || !strings.Contains(string(out), "--legacy-endpoint") {
		t.Skipf("bd at %s has no journaled `migrate ownership-handoff` verbs; set GC_ACCEPTANCE_BD_BIN to a bd that carries beads #6546", bdPath)
	}
	return bdPath, doltPath, requireLegacyGCBinary(t)
}

// readManagedDoltState reads gc's published managed-Dolt runtime state.
func readManagedDoltState(t *testing.T, cityRoot string) managedDoltRuntimeState {
	t.Helper()
	var state managedDoltRuntimeState
	readJSONFile(t, managedDoltStatePathFor(cityRoot), &state)
	if !state.Running || state.PID <= 0 || state.Port <= 0 {
		t.Fatalf("the legacy city published no live managed Dolt server: %+v", state)
	}
	return state
}

func managedDoltStatePathFor(cityRoot string) string {
	return filepath.Join(cityRoot, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
}

// runMigrateHandoff drives gc's front door and decodes its report.
//
// The report is the interface an operator has to this procedure, so the test
// reads it rather than parsing prose: a step whose status, phase, error_code or
// evidence is missing is a step an operator cannot diagnose.
func runMigrateHandoff(t *testing.T, city *helpers.City, extra ...string) (handoffReport, string, error) {
	t.Helper()
	args := append([]string{"beads", "city", "migrate-handoff", "--json"}, extra...)
	out, err := city.GCStdout(args...)
	var report handoffReport
	decodeHandoffJSON(t, out, "gc beads city migrate-handoff", &report)
	return report, out, err
}

// decodeHandoffJSON decodes the one JSON object a handoff front door prints,
// which may sit behind warning lines.
func decodeHandoffJSON(t *testing.T, out, what string, into any) {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "{") {
			continue
		}
		if err := json.Unmarshal([]byte(strings.Join(lines[i:], "\n")), into); err != nil {
			t.Fatalf("%s emitted an object that does not decode (%v):\n%s", what, err, out)
		}
		return
	}
	t.Fatalf("%s emitted no JSON object:\n%s", what, out)
}

// readHandoffJournal returns bd's journal, or ok=false when there is none.
// A rollback that ran to completion archives its journal, so "no journal" is
// the legacy condition rather than a missing file.
func readHandoffJournal(t *testing.T, scopeRoot string) (handoffJournalDoc, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(scopeRoot, handoffJournalRelPath))
	if os.IsNotExist(err) {
		return handoffJournalDoc{}, false
	}
	if err != nil {
		t.Fatalf("read the handoff journal: %v", err)
	}
	var doc handoffJournalDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse the handoff journal: %v\n%s", err, data)
	}
	return doc, true
}

// assertHandoffJournalIsWhereGCReadsIt is the contract assertion between the
// two repositories, and it is separate from "the handoff succeeded" on purpose.
//
// bd can report phase=committed having written its journal anywhere; gc's
// ownership projection reads exactly `<scope>/.beads/ownership-handoff.json`
// and treats a missing file as "no handoff ever happened". A journal in the
// wrong place is therefore not a cosmetic difference: every gc decision that
// follows silently reverts to the legacy answer, up to and including `gc start`
// raising a second sql-server over the scope bd just took.
func assertHandoffJournalIsWhereGCReadsIt(t *testing.T, scopeRoot, label string) {
	t.Helper()
	want := filepath.Join(scopeRoot, handoffJournalRelPath)
	doc, ok := readHandoffJournal(t, scopeRoot)
	if !ok {
		detail := "no ownership handoff journal exists anywhere under the scope root"
		if _, err := os.Stat(filepath.Join(scopeRoot, "ownership-handoff.json")); err == nil {
			detail = "bd wrote it to " + filepath.Join(scopeRoot, "ownership-handoff.json") + " instead"
		}
		t.Fatalf("%s: gc's ownership projection reads %s and there is no journal there — %s.\n"+
			"Until the two agree, every gc command after the handoff reads the scope as still legacy-owned.",
			label, want, detail)
	}
	// The version gate is the other half of the same contract. gc refuses a
	// journal it cannot version, so a committed transfer bd stamps with
	// anything but 2 is invisible to gc in exactly the way a misplaced one is.
	if doc.SchemaVersion != 2 {
		t.Fatalf("%s: the journal at %s is schema_version %d; gc reads version 2 and refuses every other version by name",
			label, want, doc.SchemaVersion)
	}
	if doc.Phase != "committed" || doc.Owner != "bd" {
		t.Fatalf("%s: the journal at %s is phase=%q owner=%q, want committed/bd", label, want, doc.Phase, doc.Owner)
	}
	if doc.Target.PID <= 0 || doc.Target.Port <= 0 {
		t.Fatalf("%s: the committed journal names no replacement server: %+v", label, doc.Target)
	}
}

// assertScopeIsProviderOwned proves the projection through the front door that
// depends on it rather than by re-reading the journal: the managed lifecycle
// must refuse to start a second server over a scope bd owns.
func assertScopeIsProviderOwned(t *testing.T, city *helpers.City, cityRoot string, port int, label string) {
	t.Helper()
	// --port is required, and a call without it is refused by flag validation
	// before the guard runs at all — which is how the first run of this test
	// read an ownership refusal that never happened.
	out, err := city.GC("dolt-state", "start-managed", "--city", cityRoot,
		"--host", "127.0.0.1", "--port", strconv.Itoa(port), "--user", "root")
	if err == nil {
		t.Fatalf("%s: gc dolt-state start-managed started a managed server over a scope bd owns:\n%s", label, out)
	}
	if !strings.Contains(out, "ownership handoff") {
		t.Errorf("%s: start-managed refused without naming the committed handoff:\n%s", label, out)
	}
}

// assertNoManagedDoltServer is the postcondition the whole handoff exists to
// produce: gc is out of the Dolt-running business for this city.
func assertNoManagedDoltServer(t *testing.T, cityRoot, label string) {
	t.Helper()
	if _, err := os.Stat(managedDoltStatePathFor(cityRoot)); err == nil {
		t.Errorf("%s: gc republished managed Dolt runtime state for a scope bd owns", label)
	} else if !os.IsNotExist(err) {
		t.Fatalf("%s: probe managed dolt state: %v", label, err)
	}
	for _, proc := range doltProcessesUnder(t, cityRoot) {
		if strings.Contains(proc, filepath.Join(".gc", "runtime", "packs", "dolt")) {
			t.Errorf("%s: a gc-managed Dolt server is still running:\n%s", label, proc)
		}
	}
}

// assertGCManagedDoltServer is the postcondition of a rollback: gc is back in
// the Dolt-running business for this city, by its own runtime state and by a
// live server under its own runtime directory.
func assertGCManagedDoltServer(t *testing.T, cityRoot, label string) {
	t.Helper()
	if _, err := os.Stat(managedDoltStatePathFor(cityRoot)); err != nil {
		t.Fatalf("%s: gc published no managed Dolt runtime state: %v", label, err)
	}
	for _, proc := range doltProcessesUnder(t, cityRoot) {
		if strings.Contains(proc, filepath.Join(".gc", "runtime", "packs", "dolt")) {
			return
		}
	}
	t.Errorf("%s: no gc-managed Dolt server is running:\n%s", label, strings.Join(doltProcessesUnder(t, cityRoot), "\n"))
}

// TestBeadsOwnershipHandoffLegacyCityToBd is M8 end to end.
func TestBeadsOwnershipHandoffLegacyCityToBd(t *testing.T) {
	bdPath, doltPath, legacyGC := requireOwnershipHandoffTooling(t)

	newEnv := proxiedEnv(t, bdPath, doltPath)
	oldEnv := legacyGCEnv(t, newEnv, legacyGC)

	rigDir := createGitRig(t)
	city := helpers.NewCity(t, oldEnv)
	cityRoot := city.Dir

	t.Cleanup(func() {
		_, _ = helpers.RunGC(newEnv, cityRoot, "stop", cityRoot)
		_, _ = helpers.RunGC(newEnv, "", "supervisor", "stop", "--wait")
		_, _ = helpers.RunGC(oldEnv, cityRoot, "stop", cityRoot)
		for _, root := range []string{cityRoot, rigDir} {
			if leaked := waitForNoDoltProcesses(t, root, 20*time.Second); len(leaked) > 0 {
				t.Errorf("processes survived cleanup under %s:\n%s", root, strings.Join(leaked, "\n"))
			}
		}
	})

	// --- the old way -----------------------------------------------------
	city.InitNoStart("claude")
	city.RigAdd(rigDir, "")
	assertLegacyManagedScope(t, cityRoot, "legacy city")
	assertLegacyManagedScope(t, rigDir, "legacy rig")

	cityIDs := seedLegacyScope(t, city, "")
	rigIDs := seedLegacyScope(t, city, "testrig")

	rigProjectID := readScopeProjectID(t, rigDir)
	cityDatabase := readScopeDoltDatabase(t, cityRoot)

	// The legacy server is up because the seeding above went through it. Its
	// endpoint is the one gc hands bd, and gc reads it from its own published
	// runtime state — the only record that carries a live port.
	managed := readManagedDoltState(t, cityRoot)

	// Every step below drives the NEW gc; only the fixture above is the old one.
	city.Env = newEnv

	t.Run("dry-run-plans-without-touching-anything", func(t *testing.T) {
		report, out, err := runMigrateHandoff(t, city, "--dry-run")
		if err != nil {
			t.Fatalf("dry run: %v\n%s", err, out)
		}
		if !report.DryRun || report.Status != "would-hand-off" {
			t.Fatalf("dry run report = %+v", report)
		}
		if report.Endpoint != "127.0.0.1:"+strconv.Itoa(managed.Port) || report.Database != cityDatabase {
			t.Errorf("the plan names endpoint=%q database=%q, want gc's own published server and database",
				report.Endpoint, report.Database)
		}
		if _, ok := readHandoffJournal(t, cityRoot); ok {
			t.Error("a dry run wrote a handoff journal")
		}
		if state := readManagedDoltState(t, cityRoot); state.PID != managed.PID {
			t.Errorf("a dry run disturbed the legacy server: %+v", state)
		}
	})

	t.Run("rig-scope-is-refused", func(t *testing.T) {
		// A rig is not a separate lifecycle owner — it shares the city's
		// server — so asking to hand one over has to be a typed refusal rather
		// than a second transfer of the same process. The rig's own story is
		// asserted after the city's handoff.
		out, err := city.GC("beads", "city", "migrate-handoff", "--rig", "testrig")
		if err == nil {
			t.Fatalf("migrate-handoff accepted a rig; the handoff is city-root only:\n%s", out)
		}
		for _, want := range []string{"city-root only", "canonical endpoint", "bd-qvjt"} {
			if !strings.Contains(out, want) {
				t.Errorf("the rig refusal does not mention %q:\n%s", want, out)
			}
		}
		if _, ok := readHandoffJournal(t, cityRoot); ok {
			t.Error("a refused rig request started a transfer")
		}
	})

	t.Run("handoff", func(t *testing.T) {
		report, out, err := runMigrateHandoff(t, city)
		if err != nil {
			t.Fatalf("gc beads city migrate-handoff: %v\nstatus=%q\n%s", err, report.Status, out)
		}
		if report.Status != "handed-off" || report.Failed != 0 {
			t.Fatalf("report status = %q failed=%d\n%s", report.Status, report.Failed, out)
		}
		// The order is the contract: prepare snapshots the workspace while
		// gc's server is still serving, and the stop sits between prepare and
		// legacy-gone because bd cannot stop it and must not be asked to.
		want := []string{"prepare", "stop-legacy", "legacy-gone", "configure", "verify", "commit"}
		got := make([]string, 0, len(report.Steps))
		for _, step := range report.Steps {
			got = append(got, step.Step)
			if step.Status != "ok" {
				t.Errorf("step %s = %q (%s %s)", step.Step, step.Status, step.ErrorCode, step.Error)
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("phase order = %v, want %v", got, want)
		}
		if actor := report.step(t, "stop-legacy").Actor; actor != "gc" {
			t.Errorf("the legacy stop is attributed to %q; it is gc's and only gc's", actor)
		}
		// bd's per-gate evidence reaches the operator. A refusal that only
		// says "no" is a refusal nobody can act on, and a commit that carries
		// unavailable gates should be visible as such.
		if gates := report.step(t, "legacy-gone").Evidence; len(gates) == 0 {
			t.Error("the release proof carries no gate evidence; bd journals one per gate")
		}
	})

	t.Run("journal-is-where-gc-reads-it", func(t *testing.T) {
		assertHandoffJournalIsWhereGCReadsIt(t, cityRoot, "committed city handoff")
	})

	t.Run("gc-sees-bd-as-the-owner", func(t *testing.T) {
		assertScopeIsProviderOwned(t, city, cityRoot, managed.Port, "handed-off city")
	})

	t.Run("re-running-is-a-no-op", func(t *testing.T) {
		report, out, err := runMigrateHandoff(t, city)
		if err != nil {
			t.Fatalf("re-running a committed handoff failed: %v\n%s", err, out)
		}
		if report.Status != "already-handed-off" {
			t.Fatalf("a committed transfer re-ran as %q\n%s", report.Status, out)
		}
	})

	t.Run("data-survived", func(t *testing.T) {
		assertSeededScope(t, city, "", cityIDs)
	})

	t.Run("doctor-green", func(t *testing.T) {
		if out, err := city.GC("doctor", "--fix"); err != nil {
			t.Logf("gc doctor --fix reported work outstanding: %v\n%s", err, out)
		}
		assertDoctorGreen(t, city, "a handed-off legacy city")
	})

	t.Run("start-does-not-take-the-scope-back", func(t *testing.T) {
		city.StartWithSupervisor()
		if out, err := city.GC("status"); err != nil {
			t.Fatalf("gc status after the handoff: %v\n%s", err, out)
		}
		assertNoManagedDoltServer(t, cityRoot, "started city")
	})

	t.Run("stop-and-restart", func(t *testing.T) {
		if out, err := city.GC("supervisor", "stop", "--wait"); err != nil {
			t.Fatalf("gc supervisor stop: %v\n%s", err, out)
		}
		if out, err := city.GC("stop", cityRoot); err != nil {
			t.Fatalf("gc stop after the handoff: %v\n%s", err, out)
		}
		for _, root := range []string{cityRoot, rigDir} {
			if leaked := waitForNoDoltProcesses(t, root, 20*time.Second); len(leaked) > 0 {
				t.Fatalf("gc stop did not retire bd's server under %s:\n%s", root, strings.Join(leaked, "\n"))
			}
		}
		if out, err := city.GC("stop", cityRoot); err != nil {
			t.Fatalf("gc stop is not re-runnable after the handoff: %v\n%s", out, err)
		}
		city.StartWithSupervisor()
		assertSeededScope(t, city, "", cityIDs)
		assertNoManagedDoltServer(t, cityRoot, "restarted city")
	})

	t.Run("the-inherited-rig", func(t *testing.T) {
		// The handoff is city-root only, and a legacy rig has no server of its
		// own: its database lives in the city's multi-database data dir and it
		// reached it through the city's managed endpoint. So the city's handoff
		// is the rig's handoff, and what has to be true is that the rig now
		// reads through bd's server rather than through an endpoint that no
		// longer exists.
		assertSeededScope(t, city, "testrig", rigIDs)
		if got := readScopeProjectID(t, rigDir); got != rigProjectID {
			t.Errorf("rig project_id = %q, want the pre-handoff %q", got, rigProjectID)
		}
		if _, ok := readHandoffJournal(t, rigDir); ok {
			t.Errorf("the rig grew its own handoff journal; the transfer is city-root only")
		}
	})

	t.Run("second-hop-to-proxied", func(t *testing.T) {
		if out, err := city.GC("supervisor", "stop", "--wait"); err != nil {
			t.Fatalf("gc supervisor stop: %v\n%s", err, out)
		}
		if out, err := city.GC("stop", cityRoot); err != nil {
			t.Fatalf("gc stop before the second hop: %v\n%s", err, out)
		}

		// gc's own wrapper is for cities gc still owns. A handed-off scope is
		// bd's, and `gc beads city migrate-proxied` says so rather than
		// rewriting a binding it no longer has any claim on. That refusal is
		// the contract, so it is asserted rather than stepped around.
		out, err := city.GC("beads", "city", "migrate-proxied", "--json")
		if err == nil {
			t.Fatalf("gc beads city migrate-proxied migrated a scope bd owns:\n%s", out)
		}
		if !strings.Contains(out, "ownership handoff") {
			t.Errorf("migrate-proxied refused for a reason other than the handoff:\n%s", out)
		}

		// What the hop actually needs next is not proven here, because there is
		// no supported path to prove. bd's own verb refuses the data dir it
		// inherited — gc's multi-database one holds <db>/.dolt, never .dolt —
		// and that refusal is asserted rather than stepped around: the `dolt
		// init` that clears it is work gc's own wrapper performs for a
		// gc-managed city (classifyMigrateProxiedScope's NeedsDoltInit) and
		// nothing performs for a handed-off one. The second hop is beads
		// bd-qvjt, and AC-X's rule — residue is handled by a documented gc
		// command or the lifecycle, never by hand edits in a test — is why this
		// stops at the refusal.
		hop := exec.Command(bdPath, "migrate", "from-server-to-proxied-server", "--idle-timeout", "0") //nolint:gosec // resolved test binary
		hop.Dir = cityRoot
		hop.Env = append(newEnv.List(), "BEADS_DIR="+filepath.Join(cityRoot, ".beads"))
		raw, hopErr := hop.CombinedOutput()
		if hopErr == nil {
			t.Fatalf("bd migrated the inherited multi-database data dir; the hop has a supported path now, so assert it end to end here:\n%s", raw)
		}
		if !strings.Contains(string(raw), "not a valid Dolt repository") {
			t.Fatalf("the second hop failed for a reason other than the inherited data dir layout: %v\n%s", hopErr, raw)
		}
		assertNoManagedDoltServer(t, cityRoot, "city after a refused second hop")
	})
}

// TestBeadsOwnershipHandoffLegacyAliveThenRollsBack is the compensation half,
// driven by a fault that genuinely happens rather than by a shim.
//
// The fault is a held Dolt store lock. Dolt holds an exclusive flock on each
// database's `.dolt/noms/LOCK` until its chunk journal is flushed, and a server
// that has exited has not necessarily had its locks reaped — which is why gc's
// own stop waits for release before it reports success, and why bd's
// legacy-gone gates on the same thing. Holding one for the length of the
// transfer produces both halves of the story honestly:
//
//   - gc's stop signals its server, the server exits, and the stop still fails,
//     because the data dir was not released. gc reports that and asks bd
//     anyway, because gc knows what it asked for and bd is the side that can
//     prove what happened;
//   - bd's legacy-gone refuses `legacy_alive` on the data-dir gate, with
//     nothing mutated outside its own journal.
//
// What must then happen is the whole point. A journal left mid-transfer fences
// every gc lifecycle command on the city, so the compensation is not optional:
// bd restores the workspace byte-exact, gc restarts its own server, and bd's
// rollback-finish admits it back and archives the journal. While the lock is
// still held gc cannot restart, and the command says so and stops — then the
// lock goes and re-running converges.
func TestBeadsOwnershipHandoffLegacyAliveThenRollsBack(t *testing.T) {
	bdPath, doltPath, legacyGC := requireOwnershipHandoffTooling(t)

	newEnv := proxiedEnv(t, bdPath, doltPath)
	oldEnv := legacyGCEnv(t, newEnv, legacyGC)

	city := helpers.NewCity(t, oldEnv)
	cityRoot := city.Dir
	t.Cleanup(func() {
		_, _ = helpers.RunGC(newEnv, cityRoot, "stop", cityRoot)
		_, _ = helpers.RunGC(newEnv, "", "supervisor", "stop", "--wait")
		_, _ = helpers.RunGC(oldEnv, cityRoot, "stop", cityRoot)
		if leaked := waitForNoDoltProcesses(t, cityRoot, 20*time.Second); len(leaked) > 0 {
			t.Errorf("processes survived cleanup:\n%s", strings.Join(leaked, "\n"))
		}
	})

	city.InitNoStart("claude")
	assertLegacyManagedScope(t, cityRoot, "legacy city")
	ids := seedLegacyScope(t, city, "")
	readManagedDoltState(t, cityRoot)
	before := readLegacyControlFiles(t, cityRoot)

	city.Env = newEnv
	release := holdDoltStoreLock(t, filepath.Join(cityRoot, ".beads", "dolt"))

	report, out, err := runMigrateHandoff(t, city)
	if err == nil {
		t.Fatalf("the transfer committed over a data dir that was never released:\n%s", out)
	}

	t.Run("gc-reports-its-own-stop-failure", func(t *testing.T) {
		stop := report.step(t, "stop-legacy")
		if stop.Status != "failed" {
			t.Fatalf("gc's stop reported %q over a held store lock: %+v", stop.Status, stop)
		}
		if !strings.Contains(stop.Error, "lock") && !strings.Contains(stop.Error, "released") {
			t.Errorf("the stop failure does not say the data dir was not released: %+v", stop)
		}
	})

	t.Run("bd-proves-it-with-legacy-alive", func(t *testing.T) {
		// Either code is bd saying "the old owner has not let go". The contract
		// table folds every failing release gate into legacy_alive; bd names
		// the data-dir gate specifically, which is the more useful of the two
		// and still in the typed vocabulary. What must not happen is bd
		// agreeing with gc's own report that the stop worked.
		gone := report.step(t, "legacy-gone")
		if gone.ErrorCode != "legacy_alive" && gone.ErrorCode != "data_dir_locked" {
			t.Fatalf("bd answered %q, want legacy_alive or data_dir_locked — gc's stop is a belief and bd's gates are the proof: %+v",
				gone.ErrorCode, gone)
		}
		if len(gone.Evidence) == 0 {
			t.Error("the refusal carries no gate evidence, so an operator cannot tell which gate said no")
		}
	})

	t.Run("the-fence-has-an-exit", func(t *testing.T) {
		// A journal stuck mid-transfer refuses every gc lifecycle command on
		// this city. The compensation is what clears it, so it runs even though
		// bd mutated nothing: leaving the operator with a fenced city and no
		// documented way out is the failure this case exists to prevent.
		if _, ok := readHandoffJournal(t, cityRoot); !ok {
			t.Skip("bd archived the journal on its own; there is no fence to clear")
		}
		if !report.ran("rollback") {
			t.Fatalf("a refused transfer left bd's journal in place with no rollback: %+v", report.Steps)
		}
	})

	// The lock goes; everything from here is the ordinary recovery an operator
	// performs, which is to re-run the command.
	// Release removes exactly what the fixture created, and says so if it
	// cannot: residue here is residue the product reads, and every assertion
	// after this point would be failing on the fixture rather than on gc.
	release()

	var settled handoffReport
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var resumeOut string
		settled, resumeOut, _ = runMigrateHandoff(t, city)
		if settled.Status == "rolled-back" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rollback never settled; last status %q\n%s", settled.Status, resumeOut)
		}
		time.Sleep(2 * time.Second)
	}

	t.Run("the-rollback-settles-and-archives", func(t *testing.T) {
		if _, ok := readHandoffJournal(t, cityRoot); ok {
			t.Fatal("a completed rollback left a live journal behind; bd archives it as its last act")
		}
		archived, err := filepath.Glob(filepath.Join(cityRoot, ".beads", "ownership-handoff.json.rolled-back-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(archived) == 0 {
			t.Error("the rollback left no archive; there is no record the city was ever touched")
		}
	})

	t.Run("legacy-files-are-restored", func(t *testing.T) {
		after := readLegacyControlFiles(t, cityRoot)
		for name, want := range before {
			got, ok := after[name]
			if !ok {
				t.Errorf("rollback did not restore .beads/%s", name)
				continue
			}
			if name == "config.yaml" {
				assertConfigRestoredThenExtendedByGC(t, want, got)
				continue
			}
			if got != want {
				t.Errorf("rollback restored .beads/%s with different bytes:\nbefore %q\nafter  %q", name, want, got)
			}
		}
		for name := range after {
			if _, ok := before[name]; !ok {
				t.Errorf("rollback left .beads/%s behind, which the legacy city never had", name)
			}
		}
	})

	t.Run("gc-manages-the-city-again", func(t *testing.T) {
		// The rollback is only real if gc will take the scope back. A settled
		// rollback leaves no journal at all, so the managed lifecycle is
		// admitted by the ordinary rules — and the city has to come up on it,
		// with its data.
		assertGCManagedDoltServer(t, cityRoot, "a rolled-back city")
		city.StartWithSupervisor()
		assertSeededScope(t, city, "", ids)
	})

	t.Run("stop-and-restart-still-work", func(t *testing.T) {
		if out, err := city.GC("supervisor", "stop", "--wait"); err != nil {
			t.Fatalf("gc supervisor stop after a rollback: %v\n%s", err, out)
		}
		if out, err := city.GC("stop", cityRoot); err != nil {
			t.Fatalf("gc stop after a rollback: %v\n%s", err, out)
		}
		city.StartWithSupervisor()
		assertGCManagedDoltServer(t, cityRoot, "a restarted rolled-back city")
		assertSeededScope(t, city, "", ids)
	})
}

// holdDoltStoreLock takes and holds Dolt's exclusive store lock under dataDir,
// and returns the function that releases it.
//
// It locks the multi-database root's own `.dolt/noms/LOCK` rather than a
// database's, because that file is gc's and bd's bookkeeping rather than the
// running server's: the live sql-server holds each DATABASE's lock, so a test
// that tried to take one of those would simply fail to. Both sides enumerate
// the root candidate as well as the per-database ones, so holding it is exactly
// the "a prior instance has not released the data dir" condition their guards
// are written for.
//
// Release puts the directory back exactly as it was found. That is not
// tidiness: `<dataDir>/.dolt` is a thing the product READS. A multi-database
// data dir does not have one, and a bare `.dolt` left behind makes bd treat the
// root as a Dolt repository and fail to open it — so a fixture that creates one
// and walks away has not injected a store lock, it has broken the store for
// every step after it. Only what did not exist is created, and only what was
// created is removed, deepest first, with `os.Remove` rather than `RemoveAll`
// so that anything the product put there in the meantime refuses to be deleted
// and is reported instead.
func holdDoltStoreLock(t *testing.T, dataDir string) func() {
	t.Helper()
	path := filepath.Join(dataDir, ".dolt", "noms", "LOCK")
	created := missingPathsUnder(t, dataDir, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("prepare the store lock %s: %v", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // test-controlled path under the fixture city
	if err != nil {
		t.Fatalf("open the store lock %s: %v", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		t.Fatalf("hold the store lock %s: %v", path, err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		for _, residue := range created {
			if err := os.Remove(residue); err != nil && !os.IsNotExist(err) {
				t.Errorf("the store-lock fixture could not remove what it created at %s: %v", residue, err)
			}
			if _, err := os.Stat(residue); !os.IsNotExist(err) {
				t.Errorf("the store-lock fixture left %s behind (%v).\n"+
					"Residue here is residue the product reads: a `.dolt` the multi-database root never "+
					"had makes bd treat it as a Dolt repository and refuse to open the store.", residue, err)
			}
		}
	}
	t.Cleanup(release)
	return release
}

// The store-lock fixture is itself worth a regression test: its residue broke
// the case that ran after it, and the failure surfaced as bd refusing to open a
// store rather than as anything pointing at the fixture. This needs no bd, no
// dolt and no city, so it runs on every acceptance pass.
func TestHoldDoltStoreLockLeavesNoResidue(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "dolt")
	if err := os.MkdirAll(filepath.Join(dataDir, "hq", ".dolt", "noms"), 0o755); err != nil {
		t.Fatal(err)
	}

	release := holdDoltStoreLock(t, dataDir)
	if _, err := os.Stat(filepath.Join(dataDir, ".dolt", "noms", "LOCK")); err != nil {
		t.Fatalf("the fixture did not create a lock to hold: %v", err)
	}
	release()
	if _, err := os.Stat(filepath.Join(dataDir, ".dolt")); !os.IsNotExist(err) {
		t.Fatalf("the fixture left %s behind: %v", filepath.Join(dataDir, ".dolt"), err)
	}
	// What it did not create, it does not remove.
	if _, err := os.Stat(filepath.Join(dataDir, "hq", ".dolt", "noms")); err != nil {
		t.Fatalf("the fixture removed a database directory it never created: %v", err)
	}
	release() // idempotent
}

// A data dir that already has its own `.dolt` keeps it. The fixture removes
// what it created, not what it found.
func TestHoldDoltStoreLockKeepsAnExistingStoreRoot(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "dolt")
	nomsDir := filepath.Join(dataDir, ".dolt", "noms")
	if err := os.MkdirAll(nomsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, ".dolt", "repo_state.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	holdDoltStoreLock(t, dataDir)()
	for _, kept := range []string{filepath.Join(dataDir, ".dolt"), nomsDir, filepath.Join(dataDir, ".dolt", "repo_state.json")} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("the fixture removed %s, which it did not create: %v", kept, err)
		}
	}
}

// missingPathsUnder returns path and every ancestor up to dataDir that does not
// exist yet, deepest first — exactly the set a create has to undo, and in the
// order it has to undo it. It stops at the first ancestor that already exists,
// so a `.dolt` the city legitimately has is never a candidate for removal.
func missingPathsUnder(t *testing.T, dataDir, path string) []string {
	t.Helper()
	var missing []string
	for p := path; p != dataDir && p != filepath.Dir(p); p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatalf("probe %s: %v", p, err)
		}
		missing = append(missing, p)
	}
	return missing
}

// assertConfigRestoredThenExtendedByGC states the contract for the one restored
// file gc rewrites itself. Every line the legacy city had must come back, and
// the only permitted difference is types.custom growing: that is gc's canonical
// vocabulary merge on the restart the rollback performs, and the journal's
// snapshot predates it by construction.
func assertConfigRestoredThenExtendedByGC(t *testing.T, before, after string) {
	t.Helper()
	const key = "types.custom: "
	beforeTypes, afterTypes := "", ""
	strip := func(body string, types *string) []string {
		var kept []string
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, key) {
				*types = strings.TrimPrefix(line, key)
				continue
			}
			kept = append(kept, line)
		}
		return kept
	}
	beforeLines := strip(before, &beforeTypes)
	afterLines := strip(after, &afterTypes)
	if strings.Join(beforeLines, "\n") != strings.Join(afterLines, "\n") {
		t.Errorf("rollback restored .beads/config.yaml with changes outside types.custom:\nbefore %q\nafter  %q", before, after)
	}
	have := map[string]bool{}
	for _, item := range strings.Split(afterTypes, ",") {
		have[strings.TrimSpace(item)] = true
	}
	for _, item := range strings.Split(beforeTypes, ",") {
		if item = strings.TrimSpace(item); item != "" && !have[item] {
			t.Errorf("gc dropped the legacy bead type %q from a restored config.yaml:\nbefore %q\nafter  %q", item, before, after)
		}
	}
}

// legacyControlFileNames are the three files the rollback checkpoint covers and
// gc's projection compares byte for byte before resuming legacy management.
var legacyControlFileNames = []string{"metadata.json", "config.yaml", "dolt-server.port"}

// readLegacyControlFiles snapshots whichever of them exist, so an absent file
// stays absent rather than being asserted into existence.
func readLegacyControlFiles(t *testing.T, cityRoot string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range legacyControlFileNames {
		data, err := os.ReadFile(filepath.Join(cityRoot, ".beads", name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read .beads/%s: %v", name, err)
		}
		out[name] = string(data)
	}
	return out
}

// TestBeadsOwnershipHandoffInterruptedAfterStop is the fault injection the epic
// asks for: kill the orchestrator between the legacy stop and the commit, then
// look at what the journal is permitted to claim and whether a re-run converges.
//
// The invariant under test is the one that makes an interrupted handoff safe to
// re-run: a durable record of the stop must exist before the stop happens, so a
// crash can never leave a journal claiming the legacy owner was never touched
// while its process is already dead. The recovery is then the ordinary one —
// re-running the command — because every one of bd's verbs is idempotent and
// gc's orchestrator resumes from whatever phase the journal is on.
func TestBeadsOwnershipHandoffInterruptedAfterStop(t *testing.T) {
	bdPath, doltPath, legacyGC := requireOwnershipHandoffTooling(t)

	newEnv := proxiedEnv(t, bdPath, doltPath)
	oldEnv := legacyGCEnv(t, newEnv, legacyGC)
	gcPath, err := helpers.ResolveGCPath(newEnv)
	if err != nil {
		t.Fatalf("resolve the gc under test: %v", err)
	}
	if gcPath, err = filepath.Abs(gcPath); err != nil {
		t.Fatalf("canonicalise the gc under test: %v", err)
	}

	city := helpers.NewCity(t, oldEnv)
	cityRoot := city.Dir
	t.Cleanup(func() {
		_, _ = helpers.RunGC(oldEnv, cityRoot, "stop", cityRoot)
		_, _ = helpers.RunGC(newEnv, cityRoot, "stop", cityRoot)
		if leaked := waitForNoDoltProcesses(t, cityRoot, 20*time.Second); len(leaked) > 0 {
			t.Errorf("processes survived cleanup:\n%s", strings.Join(leaked, "\n"))
		}
	})

	city.InitNoStart("claude")
	assertLegacyManagedScope(t, cityRoot, "legacy city")
	if out, err := city.GC("bd", "create", "interrupt probe", "-p", "2"); err != nil {
		t.Fatalf("seeding through the legacy server: %v\n%s", out, err)
	}
	readManagedDoltState(t, cityRoot)

	city.Env = newEnv

	// Its own process group, so the interrupt reaches whatever gc has spawned.
	// A SIGKILL to gc alone would leave a bd phase running against the journal
	// the retry is about to read.
	cmd := exec.Command(gcPath, "beads", "city", "migrate-handoff", "--json") //nolint:gosec // resolved test binary
	cmd.Dir = cityRoot
	cmd.Env = newEnv.List()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the interrupted handoff: %v", err)
	}
	group := cmd.Process.Pid
	interrupt := func() {
		_ = syscall.Kill(-group, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}
	t.Cleanup(interrupt)

	// Wait for the journal to record that the legacy owner is gone, then kill.
	// Anything at or past that phase is a legitimate interrupt point; the
	// assertion below is about what the journal may claim, not about catching
	// one exact instant.
	interruptedAt := waitForHandoffPhase(t, cityRoot, 3*time.Minute,
		"old_owner_stopped", "target_configured", "verified")
	interrupt()
	t.Logf("interrupted with the journal at %q", interruptedAt)

	// Killing gc does not kill bd. gc runs every bd child in its own process
	// group — deliberately, so a timeout can take down the tree — so the verb
	// gc had started runs to completion holding the journal lock, exactly as it
	// would for an operator who kills gc. Wait for it to let go before reading
	// the journal or re-running, or both are racing a live writer.
	waitForHandoffJournalUnlocked(t, cityRoot, 2*time.Minute)

	journal, ok := readHandoffJournal(t, cityRoot)
	if !ok {
		t.Fatal("an interrupted handoff that stopped the legacy owner wrote no journal at all; " +
			"nothing records that the scope was touched")
	}
	if journal.SchemaVersion != 2 {
		t.Fatalf("the journal is schema_version %d; gc reads version 2", journal.SchemaVersion)
	}
	if journal.Phase == "committed" {
		t.Fatal("an interrupted handoff claims committed")
	}
	if journal.Owner != "legacy-gc" {
		t.Errorf("an uncommitted journal reports owner %q, want legacy-gc", journal.Owner)
	}

	// The legacy server really is gone: the journal reached a phase bd only
	// records after proving it.
	if leaked := waitForNoDoltProcesses(t, cityRoot, 30*time.Second); len(leaked) > 0 && interruptedAt == "old_owner_stopped" {
		t.Fatalf("the journal says the legacy owner is gone but something is still running:\n%s", strings.Join(leaked, "\n"))
	}

	report, out, rerunErr := runMigrateHandoff(t, city)
	if rerunErr != nil {
		t.Fatalf("re-running the interrupted handoff did not converge: %v\nstatus=%q\n%s", rerunErr, report.Status, out)
	}
	if report.Status != "handed-off" {
		t.Fatalf("the retry settled at %q, want handed-off\n%s", report.Status, out)
	}
	// A resume is a resume: everything the journal already recorded is
	// reported and skipped, and gc's own stop in particular must not run again
	// against a server that is already gone.
	for _, step := range report.Steps {
		if step.Step == "stop-legacy" && step.Status != "skipped" {
			t.Errorf("the resume re-ran gc's stop (%q) over an already-stopped server", step.Status)
		}
	}
	assertHandoffJournalIsWhereGCReadsIt(t, cityRoot, "resumed handoff")
	assertNoManagedDoltServer(t, cityRoot, "resumed city")
}

// waitForHandoffJournalUnlocked blocks until no process holds bd's journal
// lock, which is bd's own serialisation point between verbs.
//
// The probe is the lock itself rather than a process scan: bd is what holds it,
// and a scan would have to guess which of the binaries on this box is the one
// that matters. Taking it non-blocking and letting it go is exactly what bd's
// next verb will do.
func waitForHandoffJournalUnlocked(t *testing.T, scopeRoot string, timeout time.Duration) {
	t.Helper()
	path := filepath.Join(scopeRoot, handoffJournalRelPath) + ".lock"
	if !pollUntil(timeout, 200*time.Millisecond, func() bool {
		free, err := handoffJournalLockIsFree(path)
		if err != nil {
			t.Fatalf("probe %s: %v", path, err)
		}
		return free
	}) {
		t.Fatalf("a bd verb still held %s after %s; the interrupt left a writer running", path, timeout)
	}
}

// pollUntil is the one place this file waits on wall time. Both things it waits
// for — a journal phase and a journal lock — are states another process reaches
// when it reaches them, with no signal to subscribe to, so polling is the honest
// mechanism; having one of them keeps it that way.
func pollUntil(timeout, interval time.Duration, done func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if done() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(interval)
	}
}

func handoffJournalLockIsFree(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600) //nolint:gosec // bd's lock beside its journal
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close() //nolint:errcheck // probe only
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false, nil //nolint:nilerr // a held lock is the answer, not a failure
	}
	return true, syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// waitForHandoffPhase blocks until bd's journal reaches one of phases, and
// returns which. It fails the test rather than returning empty, because every
// caller's next step is meaningless without it.
func waitForHandoffPhase(t *testing.T, cityRoot string, timeout time.Duration, phases ...string) string {
	t.Helper()
	want := map[string]bool{}
	for _, phase := range phases {
		want[phase] = true
	}
	last := "(no journal)"
	reached := ""
	if !pollUntil(timeout, 50*time.Millisecond, func() bool {
		doc, ok := readHandoffJournalQuietly(cityRoot)
		if !ok {
			return false
		}
		last = doc.Phase
		if doc.Phase == "committed" {
			t.Fatalf("the transfer committed before it could be interrupted")
		}
		if want[doc.Phase] {
			reached = doc.Phase
			return true
		}
		return false
	}) {
		t.Fatalf("the handoff never reached any of %v; last phase was %q", phases, last)
	}
	return reached
}

// readHandoffJournalQuietly is readHandoffJournal for a poll loop: a journal
// being rewritten under us is a retry, not a failure.
func readHandoffJournalQuietly(scopeRoot string) (handoffJournalDoc, bool) {
	data, err := os.ReadFile(filepath.Join(scopeRoot, handoffJournalRelPath))
	if err != nil {
		return handoffJournalDoc{}, false
	}
	var doc handoffJournalDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return handoffJournalDoc{}, false
	}
	return doc, true
}
