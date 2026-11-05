//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// realBdPurgeCity is a converged split city whose work store is a real bd
// store (embedded Dolt, driven through beads.BdStore, the store a bd city's
// work store is), opened as production opens it: under the bead-policy
// wrapper. The cutover carried a July bead under mcReusedID into the binding.
type realBdPurgeCity struct {
	purgeCity
	t    *testing.T
	dir  string
	run  beads.CommandRunner
	july time.Time
}

func newRealBdPurgeCity(t *testing.T) realBdPurgeCity {
	t.Helper()
	bdPath, err := findPreferredBinary("bd", strings.TrimSpace(os.Getenv("GC_TEST_BD_BIN")))
	if err != nil {
		t.Skip("bd not found (set GC_TEST_BD_BIN or install bd)")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not found")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	dir := filepath.Join(root, "ws")
	for _, d := range []string{home, dir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeTestGitIdentity(home); err != nil {
		t.Fatal(err)
	}
	if err := writeTestDoltIdentity(home); err != nil {
		t.Fatal(err)
	}
	city := realBdPurgeCity{t: t, dir: dir, run: beads.ExecCommandRunnerWithEnvWithoutAmbientBeads(map[string]string{
		"HOME":               home,
		"XDG_CONFIG_HOME":    filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL":  filepath.Join(home, ".gitconfig"),
		"BD_BIN":             bdPath,
		"BEADS_DIR":          filepath.Join(dir, ".beads"),
		"BD_NON_INTERACTIVE": "1",
		"BD_BACKUP_ENABLED":  "false",
		"BD_EXPORT_AUTO":     "false",
		"BEADS_ACTOR":        "gc-test",
	})}
	city.must("git", "init", "-q", ".")
	city.must("bd", "init", "-p", "mc", "--quiet", "--skip-hooks")

	// The cutover, from a pre-cutover snapshot holding the July bead under
	// the id bd will mint again.
	cityPath := recoveryCityPath(t)
	stubInfraControllerPing(t, 0)
	city.july = mustParseTime(t, "2026-07-15T12:00:00Z")
	snapshot := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: mcReusedID, Title: "handoff", Status: "open", Type: "message", Ephemeral: true, CreatedAt: city.july, UpdatedAt: city.july},
	}, nil)
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return snapshot, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	city.purgeCity = purgeCity{cityPath: cityPath, cfg: cfg, target: mustResolveInfraTarget(t, cityPath, cfg)}

	// After the cutover the work store is the real bd store, policy-wrapped
	// as every production open hands it out.
	openInfraMigrationSource = func(string) (beads.Store, error) {
		return wrapStoreWithBeadPolicies(city.store(), cfg), nil
	}
	return city
}

// must runs name in the bd workspace and returns its output, failing the test
// on an error.
func (c realBdPurgeCity) must(name string, args ...string) string {
	c.t.Helper()
	out, err := c.run(c.dir, name, args...)
	if err != nil {
		c.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// store is the bd work store itself, for the test's own reads.
func (c realBdPurgeCity) store() *beads.BdStore {
	return beads.NewBdStore(c.dir, c.run)
}

// TestPurgeStrandedMessagesOnARealBdWorkStore runs the purge against a real
// bd work store: wisp-tier messages created by `bd create -t message
// --ephemeral`, one under an id the binding holds for an older bead, and an
// inbound edge from a work bead. It proves the backup carries the inbound
// edge, the batched `bd delete --force` removes the rows and the edge and
// nothing else, the binding keeps its bead under the reused id, and status
// converges.
func TestPurgeStrandedMessagesOnARealBdWorkStore(t *testing.T) {
	city := newRealBdPurgeCity(t)
	reused := parseCreatedBeadID(t, city.must("bd", "create", "--json", mcPurgeTitle, "--id", mcReusedID, "-t", "message", "--ephemeral"))
	second := parseCreatedBeadID(t, city.must("bd", "create", "--json", mcPurgeTitle, "-t", "message", "--ephemeral"))
	third := parseCreatedBeadID(t, city.must("bd", "create", "--json", mcPurgeTitle, "-t", "message", "--ephemeral"))
	neighbourText := "the handoff in " + second + " explains this"
	work := parseCreatedBeadID(t, city.must("bd", "create", "--json", "ordinary backlog work", "-t", "task", "-d", neighbourText))
	city.must("bd", "dep", "add", work, second)
	if got := strandedIDs(t, city.cityPath, city.target); len(got) != 3 {
		t.Fatalf("stranded = %v, want the 3 messages", got)
	}

	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city.purgeCity, strandedPurgeOptions{Type: "message", DryRun: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("dry run exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "selected: 3 stranded work-store row(s) of type message") {
		t.Errorf("dry run output:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city.purgeCity, confirmedPurge(t, city.purgeCity, 3), &stdout, &stderr); code != 0 {
		t.Fatalf("purge exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	files := purgeBackupFiles(t, city.purgeCity)
	if len(files) != 1 {
		t.Fatalf("backups = %v, want one", files)
	}
	records, err := readInfraPurgeBackup(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("backup holds %d record(s), want 3", len(records))
	}
	for _, record := range records {
		if record.Entry.Bead.ID != second {
			continue
		}
		if len(record.Entry.Dependents) != 1 || record.Entry.Dependents[0].IssueID != work {
			t.Errorf("the backup of %s does not carry the inbound edge from %s: %+v", second, work, record.Entry.Dependents)
		}
	}

	store := city.store()
	rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		if b.ID == reused || b.ID == second || b.ID == third {
			t.Errorf("%s is still in the bd work store", b.ID)
		}
	}
	if _, err := store.Get(work); err != nil {
		t.Errorf("the purge removed the work bead %s: %v", work, err)
	}
	if deps, err := store.DepList(work, "down"); err != nil || len(deps) != 0 {
		t.Errorf("the work bead keeps an edge into a purged row: %v (err %v)", deps, err)
	}
	// bd delete --force rewrites the text of rows still linked to a deleted
	// one; the purge removes the links first, so the neighbour is untouched.
	if neighbour, err := store.Get(work); err != nil || neighbour.Description != neighbourText {
		t.Errorf("the purge rewrote the neighbour's text: %q, want %q (err %v)", neighbour.Description, neighbourText, err)
	}
	binding := openMigratedDestination(t, city.target)
	if held, err := binding.Get(mcReusedID); err != nil || !sameCreatedSecond(held.CreatedAt, city.july) {
		t.Errorf("the binding's July %s changed or went: %+v (err %v)", mcReusedID, held, err)
	}
	if code, out := runPurgeStatus(t, city.purgeCity); code != 0 || !strings.Contains(out, "converged: yes") {
		t.Errorf("status exited %d:\n%s", code, out)
	}

	// The restore puts the rows back verbatim, with the inbound edge, and they
	// are stranded again.
	before := map[string]beads.Bead{}
	for _, record := range records {
		before[record.Entry.Bead.ID] = record.Entry.Bead
	}
	stdout.Reset()
	stderr.Reset()
	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}
	if code := doStorageRestorePurge(context.Background(), request, files[0], false, &stdout, &stderr); code != 0 {
		t.Fatalf("restore exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for id, want := range before {
		got, err := store.Get(id)
		if err != nil {
			t.Errorf("%s did not come back: %v", id, err)
			continue
		}
		if diff := restoredRowDifference(want, got); diff != "" {
			t.Errorf("%s came back different: %s", id, diff)
		}
		if !got.Ephemeral {
			t.Errorf("%s came back out of the wisp tier", id)
		}
	}
	if deps, err := store.DepList(work, "down"); err != nil || len(deps) != 1 || deps[0].DependsOnID != second {
		t.Errorf("the inbound edge from %s did not come back: %v (err %v)", work, deps, err)
	}
	if got := strandedIDs(t, city.cityPath, city.target); len(got) != 3 {
		t.Errorf("stranded after the restore = %v, want the 3 messages", got)
	}
}

// TestPurgeStrandedResumesAfterTheEdgesWereRemovedOnARealBdWorkStore: bd lists
// a row with its outbound edges inline, and with a parent read off a
// parent-child edge, so a run interrupted after its edge removal re-reads its
// rows without them. --abandon-purge refuses while those edges are gone, and
// running the purge again resumes from the backup and finishes.
func TestPurgeStrandedResumesAfterTheEdgesWereRemovedOnARealBdWorkStore(t *testing.T) {
	city := newRealBdPurgeCity(t)
	mail := parseCreatedBeadID(t, city.must("bd", "create", "--json", mcPurgeTitle, "-t", "message", "--ephemeral"))
	other := parseCreatedBeadID(t, city.must("bd", "create", "--json", mcPurgeTitle, "-t", "message", "--ephemeral"))
	task := parseCreatedBeadID(t, city.must("bd", "create", "--json", "ordinary backlog work", "-t", "task"))
	city.must("bd", "dep", "add", mail, task)
	city.must("bd", "dep", "add", other, task, "--type", "parent-child")
	if got := strandedIDs(t, city.cityPath, city.target); len(got) != 2 {
		t.Fatalf("stranded = %v, want the 2 messages", got)
	}

	finish := failPurgeAt(t, &infraPurgeAfterEdgesRemoved, "simulated crash after the edges")
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city.purgeCity, confirmedPurge(t, city.purgeCity, 2), &stdout, &stderr); code == 0 {
		t.Fatalf("the crashed run exited 0")
	}
	finish()
	store := city.store()
	if deps, err := store.DepList(mail, "down"); err != nil || len(deps) != 0 {
		t.Fatalf("the crash point is after the edge removal, yet %s keeps its edges: %v (err %v)", mail, deps, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city.purgeCity, strandedPurgeOptions{Type: "message", Abandon: true, ExpectCount: -1}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), mail+" -[blocks]-> "+task) {
		t.Fatalf("abandon with the run's edge gone exited %d: %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city.purgeCity, confirmedPurge(t, city.purgeCity, 2), &stdout, &stderr); code != 0 {
		t.Fatalf("the resume exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "resuming") {
		t.Errorf("the run did not resume:\n%s", stdout.String())
	}
	files := purgeBackupFiles(t, city.purgeCity)
	if len(files) != 1 {
		t.Fatalf("backups = %v, want only the interrupted run's", files)
	}
	records, err := readInfraPurgeBackup(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		entry := record.Entry
		switch entry.Bead.ID {
		case mail:
			if len(entry.Deps) != 1 || entry.Deps[0].DependsOnID != task {
				t.Errorf("the backup lost %s's edge into %s: %+v", mail, task, entry.Deps)
			}
		case other:
			if len(entry.Deps) != 1 || entry.Deps[0].Type != "parent-child" || entry.Bead.ParentID != task {
				t.Errorf("the backup lost %s's parent edge into %s: deps %+v, parent %q", other, task, entry.Deps, entry.Bead.ParentID)
			}
		}
	}
	rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		if b.ID == mail || b.ID == other {
			t.Errorf("%s is still in the bd work store", b.ID)
		}
	}
	if _, err := store.Get(task); err != nil {
		t.Errorf("the purge removed the work bead %s: %v", task, err)
	}
	if code, out := runPurgeStatus(t, city.purgeCity); code != 0 || !strings.Contains(out, "converged: yes") || !strings.Contains(out, "purged stranded: 2 bead(s)") {
		t.Errorf("status after the resume exited %d:\n%s", code, out)
	}
}

// TestRestorePurgePutsAParentBackOnARealBdWorkStore: bd keeps no parent of its
// own. It reads a row's parent off the row's parent-child edge, and bd import
// drops it, so a purged mail with an outbound parent-child edge reads back with
// no parent until the restore adds the edge again. The restore, its dry run
// first, puts the mail back with its parent and its neighbour's blocks edge,
// after a full purge and after a run interrupted once it removed its edges,
// which the restore then closes.
func TestRestorePurgePutsAParentBackOnARealBdWorkStore(t *testing.T) {
	city := newRealBdPurgeCity(t)
	mail := parseCreatedBeadID(t, city.must("bd", "create", "--json", mcPurgeTitle, "-t", "message", "--ephemeral"))
	other := parseCreatedBeadID(t, city.must("bd", "create", "--json", mcPurgeTitle, "-t", "message", "--ephemeral"))
	task := parseCreatedBeadID(t, city.must("bd", "create", "--json", "ordinary backlog work", "-t", "task"))
	city.must("bd", "dep", "add", mail, task, "--type", "parent-child")
	city.must("bd", "dep", "add", other, task)
	if got := strandedIDs(t, city.cityPath, city.target); len(got) != 2 {
		t.Fatalf("stranded = %v, want the 2 messages", got)
	}
	store := city.store()
	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}
	var stdout, stderr bytes.Buffer

	// restore runs the restore of backup, its dry run first, and proves every
	// row reads back as backed up, parent included, with both edges.
	restore := func(backup, plan string) {
		t.Helper()
		records, err := readInfraPurgeBackup(backup)
		if err != nil {
			t.Fatal(err)
		}
		for _, dryRun := range []bool{true, false} {
			stdout.Reset()
			stderr.Reset()
			if code := doStorageRestorePurge(context.Background(), request, backup, dryRun, &stdout, &stderr); code != 0 {
				t.Fatalf("the restore (dry run %v) exited %d; stdout: %s stderr: %s", dryRun, code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), plan) {
				t.Errorf("the restore (dry run %v) does not say %q:\n%s", dryRun, plan, stdout.String())
			}
		}
		for _, record := range records {
			want := record.Entry.Bead
			got, err := store.Get(want.ID)
			if err != nil {
				t.Errorf("%s did not come back: %v", want.ID, err)
				continue
			}
			if diff := restoredRowDifference(want, got); diff != "" {
				t.Errorf("%s came back different: %s", want.ID, diff)
			}
		}
		if got, err := store.Get(mail); err != nil || got.ParentID != task {
			t.Errorf("%s came back with parent %q, want %q (err %v)", mail, got.ParentID, task, err)
		}
		stores, err := openInfraPurgeStores(city.cityPath, city.target)
		if err != nil {
			t.Fatal(err)
		}
		defer stores.closeWork()
		for _, edge := range []infraBackupEdge{{IssueID: mail, DependsOnID: task, Type: "parent-child"}, {IssueID: other, DependsOnID: task, Type: "blocks"}} {
			if diff, err := infraPurgeEdgeDifference(stores, edge); err != nil || diff != "" {
				t.Errorf("the edge %s did not come back: %s (err %v)", edge, diff, err)
			}
		}
	}

	if code := runStrandedPurge(t, city.purgeCity, confirmedPurge(t, city.purgeCity, 2), &stdout, &stderr); code != 0 {
		t.Fatalf("purge exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	files := purgeBackupFiles(t, city.purgeCity)
	if len(files) != 1 {
		t.Fatalf("backups = %v, want one", files)
	}
	restore(files[0], "restore: 2 row(s), 0 already back, 2 edge(s)")

	finish := failPurgeAt(t, &infraPurgeAfterEdgesRemoved, "simulated crash after the edges")
	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city.purgeCity, confirmedPurge(t, city.purgeCity, 2), &stdout, &stderr); code == 0 {
		t.Fatalf("the crashed run exited 0")
	}
	finish()
	if got, err := store.Get(mail); err != nil || got.ParentID != "" {
		t.Fatalf("the crash point is after the edge removal, yet %s reads back with parent %q (err %v)", mail, got.ParentID, err)
	}
	var interrupted string
	for _, file := range purgeBackupFiles(t, city.purgeCity) {
		if file != files[0] {
			interrupted = file
		}
	}
	if interrupted == "" {
		t.Fatal("the interrupted run wrote no backup")
	}
	restore(interrupted, "restore: 0 row(s), 2 already back, 2 edge(s)")
	if _, out := runPurgeStatus(t, city.purgeCity); strings.Contains(out, "INTERRUPTED") || !strings.Contains(out, "stranded:    2") {
		t.Errorf("status after the restore should show the rows stranded and no interrupted purge:\n%s", out)
	}
}

// TestRestorePurgeLeavesOutAParentThatIsGoneOrADifferentBeadOnARealBdWorkStore:
// bd reads a row's parent off its parent-child edge, and bd import drops it.
// When a purged mail's parent is deleted before the restore, or its id then
// holds a different bead, the restore, its dry run first, leaves that edge out
// and says the mail comes back without the parent. It exits 0, records the
// restore and closes the run, and attaches nothing to the id; the neighbour's
// blocks edge, whose far end is still the bead it was, comes back. After a full
// purge, and after a run interrupted once it removed its edges.
func TestRestorePurgeLeavesOutAParentThatIsGoneOrADifferentBeadOnARealBdWorkStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		// crash is the seam the purge dies at; nil runs it to the end.
		crash *func() error
		// remint gives the parent's id to a new bead once it is deleted.
		remint bool
		// plan is what the restore says it puts back.
		plan string
	}{
		{"after a full purge, parent gone", nil, false, "restore: 2 row(s), 0 already back, 1 edge(s)"},
		{"after a run interrupted once its edges were removed, parent minted again", &infraPurgeAfterEdgesRemoved, true, "restore: 0 row(s), 2 already back, 1 edge(s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := newRealBdPurgeCity(t)
			mail := parseCreatedBeadID(t, city.must("bd", "create", "--json", mcPurgeTitle, "-t", "message", "--ephemeral"))
			other := parseCreatedBeadID(t, city.must("bd", "create", "--json", mcPurgeTitle, "-t", "message", "--ephemeral"))
			parent := parseCreatedBeadID(t, city.must("bd", "create", "--json", "work the mail is filed under", "-t", "task"))
			task := parseCreatedBeadID(t, city.must("bd", "create", "--json", "ordinary backlog work", "-t", "task"))
			city.must("bd", "dep", "add", mail, parent, "--type", "parent-child")
			city.must("bd", "dep", "add", other, task)
			if got := strandedIDs(t, city.cityPath, city.target); len(got) != 2 {
				t.Fatalf("stranded = %v, want the 2 messages", got)
			}
			store := city.store()
			original, err := store.Get(parent)
			if err != nil {
				t.Fatal(err)
			}

			var stdout, stderr bytes.Buffer
			if tc.crash != nil {
				finish := failPurgeAt(t, tc.crash, "simulated crash after the edges")
				if code := runStrandedPurge(t, city.purgeCity, confirmedPurge(t, city.purgeCity, 2), &stdout, &stderr); code == 0 {
					t.Fatalf("the crashed run exited 0")
				}
				finish()
			} else if code := runStrandedPurge(t, city.purgeCity, confirmedPurge(t, city.purgeCity, 2), &stdout, &stderr); code != 0 {
				t.Fatalf("purge exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
			}
			files := purgeBackupFiles(t, city.purgeCity)
			if len(files) != 1 {
				t.Fatalf("backups = %v, want one", files)
			}
			records, err := readInfraPurgeBackup(files[0])
			if err != nil {
				t.Fatal(err)
			}
			backed := map[string]beads.Bead{}
			for _, record := range records {
				backed[record.Entry.Bead.ID] = record.Entry.Bead
			}
			if got := backed[mail].ParentID; got != parent {
				t.Fatalf("the backup records %s with parent %q, want %q; the scenario under test was never created", mail, got, parent)
			}

			// After the purge the parent is deleted, and in one case its id goes
			// to a new bead, imported with a creation time apart from the old
			// bead's, which a create in the same second would not have.
			city.must("bd", "delete", "--force", parent)
			why := "no row " + parent + ": the bead created "
			if tc.remint {
				at := original.CreatedAt.Add(48 * time.Hour)
				if err := store.ImportVerbatim([]beads.Bead{{ID: parent, Title: "a new bead under the id", Status: "open", Type: "task", CreatedAt: at, UpdatedAt: at}}); err != nil {
					t.Fatal(err)
				}
				why = parent + " is a different bead now"
			}

			request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}
			for _, dryRun := range []bool{true, false} {
				stdout.Reset()
				stderr.Reset()
				if code := doStorageRestorePurge(context.Background(), request, files[0], dryRun, &stdout, &stderr); code != 0 {
					t.Fatalf("the restore (dry run %v) exited %d; stdout: %s stderr: %s", dryRun, code, stdout.String(), stderr.String())
				}
				for _, want := range []string{tc.plan, "left out: 1 backed-up edge(s)", mail + " -[parent-child]-> " + parent + " (" + why, "; " + mail + " comes back without this parent)"} {
					if !strings.Contains(stdout.String(), want) {
						t.Errorf("the restore (dry run %v) does not say %q:\n%s", dryRun, want, stdout.String())
					}
				}
			}
			orphan := backed[mail]
			orphan.ParentID = ""
			for id, want := range map[string]beads.Bead{mail: orphan, other: backed[other]} {
				got, err := store.Get(id)
				if err != nil {
					t.Errorf("%s did not come back: %v", id, err)
					continue
				}
				if diff := restoredRowDifference(want, got); diff != "" {
					t.Errorf("%s came back different: %s", id, diff)
				}
			}
			if deps, err := store.DepList(mail, "down"); err != nil || len(deps) != 0 {
				t.Errorf("the restore attached %s to %s: %v (err %v)", mail, parent, deps, err)
			}
			stores, err := openInfraPurgeStores(city.cityPath, city.target)
			if err != nil {
				t.Fatal(err)
			}
			defer stores.closeWork()
			if diff, err := infraPurgeEdgeDifference(stores, infraBackupEdge{IssueID: other, DependsOnID: task, Type: "blocks"}); err != nil || diff != "" {
				t.Errorf("the edge %s -> %s did not come back: %s (err %v)", other, task, diff, err)
			}
			ledger, err := readInfraPurgeLedger(city.target)
			if err != nil {
				t.Fatal(err)
			}
			if _, interrupted := infraPurgePendingRun(ledger); interrupted || ledger[len(ledger)-1].Event != infraPurgeEventRestored {
				t.Errorf("the restore did not record its end and close the run: ledger %+v", ledger)
			}
			if _, out := runPurgeStatus(t, city.purgeCity); strings.Contains(out, "INTERRUPTED") || !strings.Contains(out, "stranded:    2") {
				t.Errorf("status after the restore should show the rows stranded and no interrupted purge:\n%s", out)
			}
		})
	}
}
