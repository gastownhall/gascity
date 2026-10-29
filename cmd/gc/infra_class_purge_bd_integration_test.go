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

	"github.com/gastownhall/gascity/internal/beads"
)

// TestPurgeStrandedMessagesOnARealBdWorkStore runs the purge against a real
// bd work store (embedded Dolt, driven through beads.BdStore, the store a bd
// city's work store is): wisp-tier messages created by `bd create -t message
// --ephemeral`, one under an id the binding holds for an older bead, and an
// inbound edge from a work bead. It proves the backup carries the inbound
// edge, the batched `bd delete --force` removes the rows and the edge and
// nothing else, the binding keeps its bead under the reused id, and status
// converges.
func TestPurgeStrandedMessagesOnARealBdWorkStore(t *testing.T) {
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
	run := beads.ExecCommandRunnerWithEnvWithoutAmbientBeads(map[string]string{
		"HOME":               home,
		"XDG_CONFIG_HOME":    filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL":  filepath.Join(home, ".gitconfig"),
		"BD_BIN":             bdPath,
		"BEADS_DIR":          filepath.Join(dir, ".beads"),
		"BD_NON_INTERACTIVE": "1",
		"BD_BACKUP_ENABLED":  "false",
		"BD_EXPORT_AUTO":     "false",
		"BEADS_ACTOR":        "gc-test",
	})
	must := func(name string, args ...string) string {
		t.Helper()
		out, err := run(dir, name, args...)
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	must("git", "init", "-q", ".")
	must("bd", "init", "-p", "mc", "--quiet", "--skip-hooks")

	// The cutover, from a pre-cutover snapshot holding the July bead under
	// the id bd will mint again.
	cityPath := recoveryCityPath(t)
	stubInfraControllerPing(t, 0)
	july := mustParseTime(t, "2026-07-15T12:00:00Z")
	snapshot := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: mcReusedID, Title: "handoff", Status: "open", Type: "message", Ephemeral: true, CreatedAt: july, UpdatedAt: july},
	}, nil)
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return snapshot, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)

	// After the cutover the work store is the real bd store.
	openInfraMigrationSource = func(string) (beads.Store, error) { return beads.NewBdStore(dir, run), nil }
	reused := parseCreatedBeadID(t, must("bd", "create", "--json", mcPurgeTitle, "--id", mcReusedID, "-t", "message", "--ephemeral"))
	second := parseCreatedBeadID(t, must("bd", "create", "--json", mcPurgeTitle, "-t", "message", "--ephemeral"))
	third := parseCreatedBeadID(t, must("bd", "create", "--json", mcPurgeTitle, "-t", "message", "--ephemeral"))
	neighbourText := "the handoff in " + second + " explains this"
	work := parseCreatedBeadID(t, must("bd", "create", "--json", "ordinary backlog work", "-t", "task", "-d", neighbourText))
	must("bd", "dep", "add", work, second)
	if got := strandedIDs(t, cityPath, target); len(got) != 3 {
		t.Fatalf("stranded = %v, want the 3 messages", got)
	}
	city := purgeCity{cityPath: cityPath, cfg: cfg, target: target}

	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, strandedPurgeOptions{Type: "message", DryRun: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("dry run exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "selected: 3 stranded work-store row(s) of type message") {
		t.Errorf("dry run output:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, confirmedPurge(3), &stdout, &stderr); code != 0 {
		t.Fatalf("purge exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	files := purgeBackupFiles(t, city)
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

	store := beads.NewBdStore(dir, run)
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
	binding := openMigratedDestination(t, target)
	if held, err := binding.Get(mcReusedID); err != nil || !sameCreatedSecond(held.CreatedAt, july) {
		t.Errorf("the binding's July %s changed or went: %+v (err %v)", mcReusedID, held, err)
	}
	if code, out := runPurgeStatus(t, city); code != 0 || !strings.Contains(out, "converged: yes") {
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
	request := storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true}
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
	if got := strandedIDs(t, cityPath, target); len(got) != 3 {
		t.Errorf("stranded after the restore = %v, want the 3 messages", got)
	}
}
