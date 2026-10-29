package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// The tests for `gc storage recover-stranded --from-work --purge-type message`
// run against real stores: the work store is a beads.FileStore on disk, opened
// fresh on every open exactly as a city's file provider is, and the binding is
// the production SQLite opener. The fixture is shaped like maintainer-city's:
// a converged city whose work store has since gained 791 open wisp-tier
// messages titled "context cycle", created 2026-08-12 .. 2026-08-30, one of
// them under an id (mc-wisp-jkl) the work store minted again after the copy
// carried the original into the binding.

const (
	mcPurgeRows   = 791
	mcReusedID    = "mc-wisp-jkl"
	mcPurgeTitle  = "context cycle"
	mcFirstMailAt = "2026-08-12T17:54:00Z"
	mcLastMailAt  = "2026-08-30T04:42:00Z"
)

// purgeCity is a converged split city whose work store is a real FileStore.
type purgeCity struct {
	cityPath string
	cfg      *config.City
	target   infraBindingTarget
	workPath string
	// reusedOriginal is the bead the copy carried under mcReusedID, as the
	// binding holds it.
	reusedOriginal beads.Bead
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return at
}

// openPurgeWorkStore opens the fixture's work store the way the provider does.
func openPurgeWorkStore(t *testing.T, path string) *beads.FileStore {
	t.Helper()
	store, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("opening the work store %s: %v", path, err)
	}
	return store
}

// editPurgeWorkStore rewrites the work store's file, appending add verbatim,
// ids and created_at included. It is how the
// fixture writes the rows a long-lived city accumulated with their real dates
// and ids, which no create call would mint.
func editPurgeWorkStore(t *testing.T, path string, add []beads.Bead) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the work store: %v", err)
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decoding the work store: %v", err)
	}
	var rows []beads.Bead
	if err := json.Unmarshal(file["beads"], &rows); err != nil {
		t.Fatalf("decoding the work store rows: %v", err)
	}
	encoded, err := json.Marshal(append(rows, add...))
	if err != nil {
		t.Fatalf("encoding the work store rows: %v", err)
	}
	file["beads"] = encoded
	out, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("encoding the work store: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("writing the work store: %v", err)
	}
}

// newConvergedPurgeCity cuts a city over (copy, prove, mark and clear) from a
// real FileStore work store, so the manifest records the July bead under
// mcReusedID and the work store keeps only work.
func newConvergedPurgeCity(t *testing.T) purgeCity {
	t.Helper()
	cityPath := recoveryCityPath(t)
	workPath := filepath.Join(cityPath, ".gc", "work-beads.json")
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) {
		return beads.OpenFileStore(fsys.OSFS{}, workPath)
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })
	stubInfraControllerPing(t, 0)

	work := openPurgeWorkStore(t, workPath)
	mustCreateInfraBead(t, work, beads.Bead{Title: "ordinary backlog work", Type: "task"})
	editPurgeWorkStore(t, workPath, []beads.Bead{
		{ID: "mc-sess-1", Title: "a session", Status: "open", Type: "session", Labels: []string{"gc:session"}, CreatedAt: mustParseTime(t, "2026-07-01T09:00:00Z")},
		{ID: mcReusedID, Title: "handoff", Status: "open", Type: "message", Ephemeral: true, Labels: []string{"thread:july"}, CreatedAt: mustParseTime(t, "2026-07-15T12:00:00Z")},
	})

	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	binding := openMigratedDestination(t, target)
	original, err := binding.Get(mcReusedID)
	if err != nil {
		t.Fatalf("the cutover did not carry %s into the binding: %v", mcReusedID, err)
	}
	return purgeCity{cityPath: cityPath, cfg: cfg, target: target, workPath: workPath, reusedOriginal: original}
}

// mcAutoHandoffMail is the n-th of maintainer-city's stranded auto-handoff
// mails: open, wisp tier, titled "context cycle", created in August.
func mcAutoHandoffMail(t *testing.T, n int) beads.Bead {
	t.Helper()
	first := mustParseTime(t, mcFirstMailAt)
	last := mustParseTime(t, mcLastMailAt)
	at := first
	if mcPurgeRows > 1 {
		at = first.Add(time.Duration(int64(last.Sub(first)) * int64(n) / int64(mcPurgeRows-1)))
	}
	id := fmt.Sprintf("mc-wisp-%04d", n)
	if n == mcPurgeRows/2 {
		id = mcReusedID
	}
	return beads.Bead{
		ID: id, Title: mcPurgeTitle, Status: "open", Type: "message", Ephemeral: true,
		Assignee: "mayor", From: "mayor",
		Labels:    []string{"thread:handoff-" + id, "gc:handoff"},
		Metadata:  map[string]string{"mail.kind": "handoff"},
		CreatedAt: at, UpdatedAt: at,
	}
}

// newMCShapedPurgeCity is newConvergedPurgeCity after the incident: the work
// store has gained maintainer-city's 791 stranded auto-handoff mails, one
// under the reused id.
func newMCShapedPurgeCity(t *testing.T) purgeCity {
	t.Helper()
	city := newConvergedPurgeCity(t)
	mails := make([]beads.Bead, 0, mcPurgeRows)
	for n := 0; n < mcPurgeRows; n++ {
		mails = append(mails, mcAutoHandoffMail(t, n))
	}
	editPurgeWorkStore(t, city.workPath, mails)
	if got := strandedIDs(t, city.cityPath, city.target); len(got) != mcPurgeRows {
		t.Fatalf("the fixture produced %d stranded row(s), want %d; the scenario under test was never created", len(got), mcPurgeRows)
	}
	return city
}

// runStrandedPurge is the test-side spelling of
// `gc storage recover-stranded --from-work --fleet-stopped --purge-type ...`.
func runStrandedPurge(t *testing.T, city purgeCity, opts strandedPurgeOptions, stdout, stderr io.Writer) int {
	t.Helper()
	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}
	return doStoragePurgeStranded(context.Background(), request, opts, stdout, stderr)
}

// confirmedPurge is the options of a confirmed purge of n reviewed rows.
func confirmedPurge(n int) strandedPurgeOptions {
	return strandedPurgeOptions{Type: "message", Confirm: true, ExpectCount: n}
}

func purgeWorkRows(t *testing.T, city purgeCity) []beads.Bead {
	t.Helper()
	rows, err := openPurgeWorkStore(t, city.workPath).List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("listing the work store: %v", err)
	}
	return rows
}

func countPurgeRowsOfType(rows []beads.Bead, typ string) int {
	n := 0
	for _, b := range rows {
		if b.Type == typ {
			n++
		}
	}
	return n
}

// purgeBackupFiles lists the timestamped purge backups at the binding root.
func purgeBackupFiles(t *testing.T, city purgeCity) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(city.target.Root, infraPurgedStrandedBackupPrefix+"*.jsonl"))
	if err != nil {
		t.Fatalf("globbing purge backups: %v", err)
	}
	return matches
}

func runPurgeStatus(t *testing.T, city purgeCity) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := doStorageStatus(storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg}, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// TestPurgeStrandedMessagesDryRunListsTheMCShapeAndWritesNothing pins the
// rehearsal: the count, the id range, the titles and the dates, and nothing
// written — no backup, no ledger, no row removed. It runs with no
// --fleet-stopped and no confirmation, as the rehearsal does on a live city.
func TestPurgeStrandedMessagesDryRunListsTheMCShapeAndWritesNothing(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	stubInfraControllerPing(t, 4242) // the rehearsal runs with the fleet live
	before := purgeWorkRows(t, city)

	var stdout, stderr bytes.Buffer
	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg}
	if code := doStoragePurgeStranded(context.Background(), request, strandedPurgeOptions{Type: "message", DryRun: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("dry run exited %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"selected: 791 stranded work-store row(s) of type message",
		"ids:     mc-wisp-0000 .. mc-wisp-jkl",
		"created: " + mcFirstMailAt + " .. " + mcLastMailAt,
		`791  "context cycle"`,
		"status:  open 791",
		mcReusedID + "  created=",
		"dry-run: nothing was written",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "binding holds a different bead under the id") {
		t.Errorf("dry-run output does not say %s is a reused id the binding holds for another bead:\n%s", mcReusedID, out)
	}
	if after := purgeWorkRows(t, city); len(after) != len(before) {
		t.Errorf("the dry run removed rows: %d before, %d after", len(before), len(after))
	}
	if files := purgeBackupFiles(t, city); len(files) != 0 {
		t.Errorf("the dry run wrote a backup: %v", files)
	}
	if _, err := os.Stat(city.target.PurgeLedgerPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dry run wrote the purge ledger (stat err %v)", err)
	}
}

// TestPurgeStrandedMessagesRequiresAnExplicitConfirmation: without
// --confirm-purge the command lists what it would delete and deletes nothing.
func TestPurgeStrandedMessagesRequiresAnExplicitConfirmation(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, strandedPurgeOptions{Type: "message", ExpectCount: -1}, &stdout, &stderr); code == 0 {
		t.Fatalf("an unconfirmed purge exited 0; stdout: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--confirm-purge") {
		t.Errorf("the refusal does not name --confirm-purge: %s", stderr.String())
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != mcPurgeRows {
		t.Errorf("an unconfirmed purge removed rows: %d message rows left, want %d", got, mcPurgeRows)
	}
	if files := purgeBackupFiles(t, city); len(files) != 0 {
		t.Errorf("an unconfirmed purge wrote a backup: %v", files)
	}
}

// TestPurgeStrandedMessagesRequiresTheFleetStoppedAttestation: the delete is
// a write against a store the fleet writes, so it carries the attestation
// every other writing recover-stranded run carries.
func TestPurgeStrandedMessagesRequiresTheFleetStoppedAttestation(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	var stdout, stderr bytes.Buffer
	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg}
	if code := doStoragePurgeStranded(context.Background(), request, confirmedPurge(791), &stdout, &stderr); code == 0 {
		t.Fatalf("a purge without --fleet-stopped exited 0")
	}
	if !strings.Contains(stderr.String(), "--"+storageFleetStoppedFlag) {
		t.Errorf("the refusal does not name --%s: %s", storageFleetStoppedFlag, stderr.String())
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != mcPurgeRows {
		t.Errorf("rows were removed without the attestation: %d left", got)
	}
}

// TestPurgeStrandedMessagesRefusesWhileTheControllerIsLive: a live controller
// is still writing the work store.
func TestPurgeStrandedMessagesRefusesWhileTheControllerIsLive(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	stubInfraControllerPing(t, 4242)
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(791), &stdout, &stderr); code == 0 {
		t.Fatalf("a purge with a live controller exited 0")
	}
	if !strings.Contains(stderr.String(), "4242") {
		t.Errorf("the refusal does not name the live controller: %s", stderr.String())
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != mcPurgeRows {
		t.Errorf("rows were removed under a live controller: %d left", got)
	}
}

// TestPurgeStrandedMessagesBacksUpThenDeletesAndStatusConverges is the MC
// path end to end: every stranded mail is backed up in full (metadata,
// labels, edges) to a timestamped file under .gc/store, then deleted from the
// work store; the binding's own row under the reused id is untouched; the
// work bead stays; the purge is recorded; `gc storage status` exits 0 and
// reports converged; and a second run is a no-op.
func TestPurgeStrandedMessagesBacksUpThenDeletesAndStatusConverges(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	// One mail carries an edge into the work bead, so the backup has to carry
	// the edge and the delete has to take it.
	rows := purgeWorkRows(t, city)
	var workID string
	for _, b := range rows {
		if b.Type == "task" {
			workID = b.ID
		}
	}
	work := openPurgeWorkStore(t, city.workPath)
	if err := work.DepAdd("mc-wisp-0001", workID, "relates-to"); err != nil {
		t.Fatalf("seeding an edge: %v", err)
	}
	if code, out := runPurgeStatus(t, city); code == 0 {
		t.Fatalf("status exited 0 before the purge; the fixture is not stranded:\n%s", out)
	}

	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(791), &stdout, &stderr); code != 0 {
		t.Fatalf("purge exited %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}

	files := purgeBackupFiles(t, city)
	if len(files) != 1 {
		t.Fatalf("purge backups = %v, want exactly one", files)
	}
	storeDir := filepath.Join(city.cityPath, ".gc", "store")
	if inside, err := infraPathInside(storeDir, files[0]); err != nil || !inside {
		t.Errorf("the backup %s is not under %s (err %v)", files[0], storeDir, err)
	}
	if !strings.Contains(stdout.String(), files[0]) {
		t.Errorf("the run did not print the backup path %s:\n%s", files[0], stdout.String())
	}
	records, err := readInfraPurgeBackup(files[0])
	if err != nil {
		t.Fatalf("reading the backup: %v", err)
	}
	if len(records) != mcPurgeRows {
		t.Fatalf("the backup holds %d record(s), want %d", len(records), mcPurgeRows)
	}
	byID := map[string]infraRecoverChoiceRecord{}
	for _, r := range records {
		byID[r.Entry.Bead.ID] = r
	}
	reused := byID[mcReusedID].Entry.Bead
	want := mcAutoHandoffMail(t, mcPurgeRows/2)
	if !reused.CreatedAt.Equal(want.CreatedAt) || reused.Title != mcPurgeTitle || reused.Metadata["mail.kind"] != "handoff" ||
		!slices.Contains(reused.Labels, "gc:handoff") || !reused.Ephemeral {
		t.Errorf("the backup of %s is not the full row: %+v", mcReusedID, reused)
	}
	if edges := byID["mc-wisp-0001"].Entry.Deps; len(edges) != 1 || edges[0].DependsOnID != workID || edges[0].Type != "relates-to" {
		t.Errorf("the backup of mc-wisp-0001 does not carry its edge into %s: %+v", workID, edges)
	}

	after := purgeWorkRows(t, city)
	if got := countPurgeRowsOfType(after, "message"); got != 0 {
		t.Errorf("%d message row(s) are left in the work store", got)
	}
	if got := countPurgeRowsOfType(after, "task"); got != 1 {
		t.Errorf("the purge touched the work bead: %d task row(s) left, want 1", got)
	}
	if deps, err := openPurgeWorkStore(t, city.workPath).DepList(workID, "up"); err != nil || len(deps) != 0 {
		t.Errorf("an edge into the purged row is left on the work bead: %v (err %v)", deps, err)
	}

	binding := openMigratedDestination(t, city.target)
	held, err := binding.Get(mcReusedID)
	if err != nil {
		t.Fatalf("the binding lost its own %s: %v", mcReusedID, err)
	}
	if !sameCreatedSecond(held.CreatedAt, city.reusedOriginal.CreatedAt) || held.Title != city.reusedOriginal.Title {
		t.Errorf("the binding's %s changed: %+v, want %+v", mcReusedID, held, city.reusedOriginal)
	}

	ledger, err := readInfraPurgeLedger(city.target)
	if err != nil {
		t.Fatalf("reading the purge ledger: %v", err)
	}
	if len(ledger) != 2 || ledger[0].Event != infraPurgeEventBackedUp || ledger[1].Event != infraPurgeEventPurged || len(ledger[1].Rows) != mcPurgeRows || ledger[1].Backup != files[0] {
		t.Errorf("the ledger does not record the purge: %+v", ledger)
	}

	code, out := runPurgeStatus(t, city)
	if code != 0 {
		t.Fatalf("status exited %d after the purge, want 0:\n%s", code, out)
	}
	for _, want := range []string{"converged: yes", "stranded:    0", "purged stranded: 791 bead(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, confirmedPurge(791), &stdout, &stderr); code != 0 {
		t.Fatalf("a second purge exited %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "nothing to purge") {
		t.Errorf("the second run did not report a no-op:\n%s", stdout.String())
	}
	if files := purgeBackupFiles(t, city); len(files) != 1 {
		t.Errorf("the second run wrote another backup: %v", files)
	}
}

// TestPurgeStrandedRefusesAStrandedRowOfTheTypeThatIsNotMessageClass: a
// type=message row the classifier puts in another class (here graph, by its
// gc:wisp label) is not mail, and the purge is a purge of mail. The whole run
// refuses and nothing is written.
func TestPurgeStrandedRefusesAStrandedRowOfTheTypeThatIsNotMessageClass(t *testing.T) {
	city := newConvergedPurgeCity(t)
	at := mustParseTime(t, mcFirstMailAt)
	editPurgeWorkStore(t, city.workPath, []beads.Bead{
		mcAutoHandoffMail(t, 1),
		{ID: "mc-wisp-graph", Title: "a wisp step typed message", Status: "open", Type: "message", Labels: []string{"gc:wisp"}, CreatedAt: at},
	})
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(2), &stdout, &stderr); code == 0 {
		t.Fatalf("the purge exited 0 with a non-message-class row selected; stdout: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "mc-wisp-graph") || !strings.Contains(stderr.String(), "graph") {
		t.Errorf("the refusal does not name the row and its class: %s", stderr.String())
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != 2 {
		t.Errorf("the refused purge removed rows: %d message rows left, want 2", got)
	}
	if files := purgeBackupFiles(t, city); len(files) != 0 {
		t.Errorf("the refused purge wrote a backup: %v", files)
	}
}

// TestPurgeStrandedLeavesOtherStrandedRowsAndNonStrandedMessages: a stranded
// row of another type is not selected, so it stays (and status keeps naming
// it), and a message row the binding holds as the same bead (id and
// created_at) is not stranded, so it is never deleted.
func TestPurgeStrandedLeavesOtherStrandedRowsAndNonStrandedMessages(t *testing.T) {
	city := newConvergedPurgeCity(t)
	at := mustParseTime(t, mcFirstMailAt)
	retained := city.reusedOriginal
	editPurgeWorkStore(t, city.workPath, []beads.Bead{
		mcAutoHandoffMail(t, 1),
		{ID: "mc-sess-9", Title: "a stranded session", Status: "open", Type: "session", Labels: []string{"gc:session"}, CreatedAt: at},
		retained,
	})
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(1), &stdout, &stderr); code != 0 {
		t.Fatalf("purge exited %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	rows := purgeWorkRows(t, city)
	ids := make([]string, 0, len(rows))
	for _, b := range rows {
		ids = append(ids, b.ID)
	}
	if slices.Contains(ids, "mc-wisp-0001") {
		t.Errorf("the stranded mail was not purged: %v", ids)
	}
	if !slices.Contains(ids, "mc-sess-9") {
		t.Errorf("the purge deleted a stranded row of another type: %v", ids)
	}
	if !slices.Contains(ids, mcReusedID) {
		t.Errorf("the purge deleted %s, which the binding holds as the same bead: %v", mcReusedID, ids)
	}
	if !strings.Contains(stdout.String(), "mc-sess-9") {
		t.Errorf("the run does not name the stranded row it left: %s", stdout.String())
	}
	if code, out := runPurgeStatus(t, city); code == 0 || !strings.Contains(out, "mc-sess-9") {
		t.Errorf("status exited %d and should still name the stranded session:\n%s", code, out)
	}
}

// TestPurgeGuardRefusesARowTheBindingHoldsAsTheSameBead pins the last check
// before the delete on its own: whatever selected it, a row the binding holds
// under the same (id, created_at) is never deleted.
func TestPurgeGuardRefusesARowTheBindingHoldsAsTheSameBead(t *testing.T) {
	city := newConvergedPurgeCity(t)
	binding := openMigratedDestination(t, city.target)
	if err := refuseInfraPurgeOfBindingRows(binding, []beads.Bead{city.reusedOriginal}); err == nil || !strings.Contains(err.Error(), mcReusedID) {
		t.Fatalf("the guard let a row the binding holds as the same bead through: %v", err)
	}
	reused := mcAutoHandoffMail(t, mcPurgeRows/2)
	if err := refuseInfraPurgeOfBindingRows(binding, []beads.Bead{reused}); err != nil {
		t.Fatalf("the guard refused a reused id the binding holds for a different bead: %v", err)
	}
}

// TestPurgeStrandedResumesAfterACrashBetweenBackupAndDelete: a run that dies
// after the backup is proven and before the delete leaves every row in place
// and the ledger saying so; status still refuses and names the interrupted
// purge; the next run resumes from the same backup rather than writing a
// second one, deletes, and status converges.
func TestPurgeStrandedResumesAfterACrashBetweenBackupAndDelete(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	prev := infraPurgeAfterBackup
	infraPurgeAfterBackup = func() error { return errors.New("simulated crash after the backup") }
	t.Cleanup(func() { infraPurgeAfterBackup = prev })

	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(791), &stdout, &stderr); code == 0 {
		t.Fatalf("the crashed run exited 0")
	}
	files := purgeBackupFiles(t, city)
	if len(files) != 1 {
		t.Fatalf("backups after the crash = %v, want one", files)
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != mcPurgeRows {
		t.Fatalf("the crashed run deleted rows: %d left", got)
	}
	code, out := runPurgeStatus(t, city)
	if code == 0 || !strings.Contains(out, "purge: INTERRUPTED") {
		t.Errorf("status after the crash exited %d and should name the interrupted purge:\n%s", code, out)
	}

	// The interrupted delete had already committed a first chunk.
	work := openPurgeWorkStore(t, city.workPath)
	for n := 0; n < 10; n++ {
		if err := work.Delete(fmt.Sprintf("mc-wisp-%04d", n)); err != nil {
			t.Fatalf("simulating a committed first chunk: %v", err)
		}
	}

	infraPurgeAfterBackup = prev
	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, confirmedPurge(781), &stdout, &stderr); code != 0 {
		t.Fatalf("the resumed run exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "resuming") {
		t.Errorf("the resumed run does not say it resumed:\n%s", stdout.String())
	}
	if after := purgeBackupFiles(t, city); !slices.Equal(after, files) {
		t.Errorf("the resumed run wrote another backup: %v, want %v", after, files)
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != 0 {
		t.Errorf("%d message rows left after the resume", got)
	}
	if code, out := runPurgeStatus(t, city); code != 0 || !strings.Contains(out, "converged: yes") || !strings.Contains(out, "purged stranded: 791 bead(s)") {
		t.Errorf("status after the resume exited %d, and should count every row the run backed up:\n%s", code, out)
	}
}

// TestPurgeStrandedRefusesAnUnsupportedType: the purge is of mail, the one
// class the owner decided to purge rather than recover.
func TestPurgeStrandedRefusesAnUnsupportedType(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, strandedPurgeOptions{Type: "session", Confirm: true}, &stdout, &stderr); code == 0 {
		t.Fatalf("a purge of type session exited 0")
	}
	if !strings.Contains(stderr.String(), "message") {
		t.Errorf("the refusal does not name the supported type: %s", stderr.String())
	}
}

// TestPurgeTypeFlagExcludesThePerIDChoices: --purge-type selects by class, so
// it does not combine with the per-id answers, and the refusal comes before
// any city is resolved.
func TestPurgeTypeFlagExcludesThePerIDChoices(t *testing.T) {
	for _, extra := range [][]string{
		{"--move", "mc-1"},
		{"--record-carried", "mc-1"},
		{"--replace-binding-row", "mc-1"},
		{"--drop-work-row", "mc-1"},
		{"--dump", "/tmp/x.json"},
	} {
		var stdout, stderr bytes.Buffer
		surface, err := parseOperatorCommandSpelling(storageRecoveryCommand)
		if err != nil {
			t.Fatal(err)
		}
		cmd := newStorageRecoverCmd(surface, &stdout, &stderr)
		cmd.SetArgs(append([]string{"--from-work", "--purge-type", "message"}, extra...))
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err == nil {
			t.Errorf("--purge-type with %v was accepted", extra)
		}
		if !strings.Contains(stderr.String(), "--purge-type") {
			t.Errorf("the refusal for %v does not name --purge-type: %s", extra, stderr.String())
		}
	}
}

// purgeDigestFrom reads the digest a dry run printed.
func purgeDigestFrom(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "digest:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("the dry run printed no digest:\n%s", out)
	return ""
}

// TestPurgeStrandedDeleteIsTiedToTheReviewedDryRun: the delete needs
// --expect-count, refuses a count or digest that is not the selection's,
// and accepts the digest the dry run printed.
func TestPurgeStrandedDeleteIsTiedToTheReviewedDryRun(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, strandedPurgeOptions{Type: "message", DryRun: true, ExpectCount: -1}, &stdout, &stderr); code != 0 {
		t.Fatalf("dry run exited %d: %s", code, stderr.String())
	}
	digest := purgeDigestFrom(t, stdout.String())
	if !strings.Contains(stdout.String(), fmt.Sprintf("--%s %d --%s %s", strandedExpectCountFlag, mcPurgeRows, strandedExpectDigestFlag, digest)) {
		t.Errorf("the dry run does not print the confirmation to pass:\n%s", stdout.String())
	}
	for name, opts := range map[string]strandedPurgeOptions{
		"no count":     {Type: "message", Confirm: true, ExpectCount: -1},
		"wrong count":  {Type: "message", Confirm: true, ExpectCount: mcPurgeRows - 1},
		"wrong digest": {Type: "message", Confirm: true, ExpectCount: mcPurgeRows, ExpectDigest: "0000000000000000"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := runStrandedPurge(t, city, opts, &stdout, &stderr); code == 0 {
			t.Errorf("%s: the purge exited 0", name)
		}
		if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != mcPurgeRows {
			t.Fatalf("%s: rows were deleted: %d left", name, got)
		}
		if files := purgeBackupFiles(t, city); len(files) != 0 {
			t.Fatalf("%s: a backup was written: %v", name, files)
		}
	}
	stdout.Reset()
	stderr.Reset()
	opts := confirmedPurge(mcPurgeRows)
	opts.ExpectDigest = digest
	if code := runStrandedPurge(t, city, opts, &stdout, &stderr); code != 0 {
		t.Fatalf("the purge with the reviewed count and digest exited %d: %s", code, stderr.String())
	}
}

// TestPurgeStrandedFinishesARunThatDiedAfterTheDelete: every row is gone but
// the run died before recording it, so nothing is stranded and the ledger
// says INTERRUPTED. Status refuses until the next run records the deleted
// rows, which it does with nothing left to select.
func TestPurgeStrandedFinishesARunThatDiedAfterTheDelete(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	prev := infraPurgeAfterDeleteRecorded
	infraPurgeAfterDeleteRecorded = func() error { return errors.New("simulated crash after the delete") }
	t.Cleanup(func() { infraPurgeAfterDeleteRecorded = prev })
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(mcPurgeRows), &stdout, &stderr); code == 0 {
		t.Fatalf("the crashed run exited 0")
	}
	infraPurgeAfterDeleteRecorded = prev
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != 0 {
		t.Fatalf("the crash point is after the delete, yet %d rows are left", got)
	}
	if got := strandedIDs(t, city.cityPath, city.target); len(got) != 0 {
		t.Fatalf("stranded = %v, want none", got)
	}
	if code, out := runPurgeStatus(t, city); code == 0 || !strings.Contains(out, "purge: INTERRUPTED") {
		t.Fatalf("status with zero stranded rows and an interrupted ledger exited %d:\n%s", code, out)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, strandedPurgeOptions{Type: "message", ExpectCount: -1}, &stdout, &stderr); code != 0 {
		t.Fatalf("the finishing run exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "recorded 791 deleted row(s)") {
		t.Errorf("the finishing run does not say it recorded the rows:\n%s", stdout.String())
	}
	code, out := runPurgeStatus(t, city)
	if code != 0 || !strings.Contains(out, "converged: yes") || !strings.Contains(out, "purged stranded: 791 bead(s)") || strings.Contains(out, "INTERRUPTED") {
		t.Errorf("status after the finishing run exited %d:\n%s", code, out)
	}
}

// TestPurgeStrandedRefusesAResumeWhoseSelectionChanged: a row stranded after
// the interrupted run's backup is not in it, so the run refuses rather than
// writing a second backup and splitting the run's accounting.
func TestPurgeStrandedRefusesAResumeWhoseSelectionChanged(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	prev := infraPurgeAfterBackup
	infraPurgeAfterBackup = func() error { return errors.New("simulated crash after the backup") }
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(mcPurgeRows), &stdout, &stderr); code == 0 {
		t.Fatalf("the crashed run exited 0")
	}
	infraPurgeAfterBackup = prev
	late := mcAutoHandoffMail(t, 3)
	late.ID = "mc-wisp-late"
	editPurgeWorkStore(t, city.workPath, []beads.Bead{late})

	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, confirmedPurge(mcPurgeRows+1), &stdout, &stderr); code == 0 {
		t.Fatalf("a resume over a changed selection exited 0")
	}
	if !strings.Contains(stderr.String(), "mc-wisp-late") || !strings.Contains(stderr.String(), "--"+strandedAbandonPurgeFlag) {
		t.Errorf("the refusal does not name the new row and the way out: %s", stderr.String())
	}
	if files := purgeBackupFiles(t, city); len(files) != 1 {
		t.Errorf("backups = %v, want only the interrupted run's", files)
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != mcPurgeRows+1 {
		t.Errorf("rows were deleted: %d left", got)
	}
}

// TestPurgeStrandedAbandonClosesARunWhoseRowsAreBack: --abandon-purge refuses
// while a row of the interrupted run is missing, and closes the run once all
// are back.
func TestPurgeStrandedAbandonClosesARunWhoseRowsAreBack(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	prev := infraPurgeAfterBackup
	infraPurgeAfterBackup = func() error { return errors.New("simulated crash after the backup") }
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(mcPurgeRows), &stdout, &stderr); code == 0 {
		t.Fatalf("the crashed run exited 0")
	}
	infraPurgeAfterBackup = prev

	gone := mcAutoHandoffMail(t, 7)
	if err := openPurgeWorkStore(t, city.workPath).Delete(gone.ID); err != nil {
		t.Fatal(err)
	}
	abandon := strandedPurgeOptions{Type: "message", Abandon: true, ExpectCount: -1}
	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, abandon, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), gone.ID) {
		t.Fatalf("abandon with a missing row exited %d: %s", code, stderr.String())
	}
	editPurgeWorkStore(t, city.workPath, []beads.Bead{gone})
	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, abandon, &stdout, &stderr); code != 0 {
		t.Fatalf("abandon with every row back exited %d: %s", code, stderr.String())
	}
	if _, out := runPurgeStatus(t, city); strings.Contains(out, "INTERRUPTED") || !strings.Contains(out, "stranded:    791") {
		t.Errorf("status after abandon should show the rows stranded and no interrupted purge:\n%s", out)
	}
}

// TestPurgeStrandedRefusesABackupThatDoesNotReadBack: the read-back compare is
// what licenses the delete; a backup that lands different deletes nothing.
func TestPurgeStrandedRefusesABackupThatDoesNotReadBack(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	prev := infraMigrationRename
	infraMigrationRename = func(from, to string) error {
		if err := prev(from, to); err != nil {
			return err
		}
		if !strings.HasPrefix(filepath.Base(to), infraPurgedStrandedBackupPrefix) {
			return nil
		}
		raw, err := os.ReadFile(to)
		if err != nil {
			return err
		}
		return os.WriteFile(to, bytes.Replace(raw, []byte(mcPurgeTitle), []byte("context cycl3"), 1), 0o644)
	}
	t.Cleanup(func() { infraMigrationRename = prev })
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(mcPurgeRows), &stdout, &stderr); code == 0 {
		t.Fatalf("a purge whose backup read back different exited 0")
	}
	if !strings.Contains(stderr.String(), "proving the purge backup") {
		t.Errorf("the refusal does not name the failed proof: %s", stderr.String())
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != mcPurgeRows {
		t.Errorf("rows were deleted over an unproven backup: %d left", got)
	}
	if ledger, _ := readInfraPurgeLedger(city.target); len(ledger) != 0 {
		t.Errorf("the ledger recorded a backup that was never proven: %+v", ledger)
	}
}

// TestPurgeStrandedRunRereadsTheBindingBeforeWriting: a row the binding comes
// to hold as the same bead after the selection is refused by the run's own
// re-read, before any backup or ledger is written.
func TestPurgeStrandedRunRereadsTheBindingBeforeWriting(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	mail := mcAutoHandoffMail(t, 5)
	prev := infraPurgeBeforeBindingGuard
	infraPurgeBeforeBindingGuard = func() error {
		binding, err := openInfraDestination(city.target)
		if err != nil {
			return err
		}
		defer closeBeadStoreHandle(binding) //nolint:errcheck // test
		_, err = binding.(beads.ForeignIDCreator).CreateWithForeignID(mail)
		return err
	}
	t.Cleanup(func() { infraPurgeBeforeBindingGuard = prev })
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(mcPurgeRows), &stdout, &stderr); code == 0 {
		t.Fatalf("the purge exited 0 over a row the binding holds as the same bead")
	}
	if !strings.Contains(stderr.String(), mail.ID) {
		t.Errorf("the refusal does not name %s: %s", mail.ID, stderr.String())
	}
	if files := purgeBackupFiles(t, city); len(files) != 0 {
		t.Errorf("a backup was written before the guard: %v", files)
	}
	if _, err := os.Stat(city.target.PurgeLedgerPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the ledger was written before the guard (stat err %v)", err)
	}
}

// TestRestorePurgePutsTheRowsAndEdgesBack: --restore-purge refuses an id
// minted again, then puts every row back exactly (created_at included) with
// its outbound and inbound edges, and status shows the rows stranded again.
func TestRestorePurgePutsTheRowsAndEdgesBack(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	var workID string
	for _, b := range purgeWorkRows(t, city) {
		if b.Type == "task" {
			workID = b.ID
		}
	}
	work := openPurgeWorkStore(t, city.workPath)
	if err := work.DepAdd("mc-wisp-0001", workID, "relates-to"); err != nil {
		t.Fatal(err)
	}
	if err := work.DepAdd(workID, "mc-wisp-0002", "blocks"); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(mcPurgeRows), &stdout, &stderr); code != 0 {
		t.Fatalf("purge exited %d: %s", code, stderr.String())
	}
	backup := purgeBackupFiles(t, city)[0]
	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}

	// bd minted one id again since the purge.
	again := mcAutoHandoffMail(t, 9)
	again.CreatedAt = mustParseTime(t, "2026-10-01T00:00:00Z")
	editPurgeWorkStore(t, city.workPath, []beads.Bead{again})
	stdout.Reset()
	stderr.Reset()
	if code := doStorageRestorePurge(context.Background(), request, backup, false, &stdout, &stderr); code == 0 {
		t.Fatalf("a restore over an id minted again exited 0")
	}
	if !strings.Contains(stderr.String(), again.ID) {
		t.Errorf("the refusal does not name %s: %s", again.ID, stderr.String())
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != 1 {
		t.Fatalf("the refused restore wrote rows: %d message rows", got)
	}
	if err := openPurgeWorkStore(t, city.workPath).Delete(again.ID); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := doStorageRestorePurge(context.Background(), request, backup, false, &stdout, &stderr); code != 0 {
		t.Fatalf("restore exited %d: %s", code, stderr.String())
	}
	rows := purgeWorkRows(t, city)
	if got := countPurgeRowsOfType(rows, "message"); got != mcPurgeRows {
		t.Fatalf("%d message rows after the restore, want %d", got, mcPurgeRows)
	}
	for _, b := range rows {
		if b.ID == mcReusedID && !sameCreatedSecond(b.CreatedAt, mcAutoHandoffMail(t, mcPurgeRows/2).CreatedAt) {
			t.Errorf("%s came back created %s, not as backed up", mcReusedID, b.CreatedAt)
		}
	}
	store := openPurgeWorkStore(t, city.workPath)
	if deps, _ := store.DepList("mc-wisp-0001", "down"); len(deps) != 1 || deps[0].DependsOnID != workID {
		t.Errorf("the outbound edge did not come back: %+v", deps)
	}
	if deps, _ := store.DepList(workID, "down"); len(deps) != 1 || deps[0].DependsOnID != "mc-wisp-0002" {
		t.Errorf("the inbound edge did not come back: %+v", deps)
	}
	code, out := runPurgeStatus(t, city)
	if code == 0 || !strings.Contains(out, "stranded:    791") || !strings.Contains(out, "purged stranded: 0 bead(s)") || !strings.Contains(out, "restored by --restore-purge: 791") {
		t.Errorf("status after the restore exited %d:\n%s", code, out)
	}
	// A second restore finds everything back and changes nothing.
	stdout.Reset()
	stderr.Reset()
	if code := doStorageRestorePurge(context.Background(), request, backup, false, &stdout, &stderr); code != 0 {
		t.Fatalf("a second restore exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "791 already back") {
		t.Errorf("the second restore does not report the rows already back:\n%s", stdout.String())
	}
}

// TestPurgeStatusNamesARemedyForAnUnreadableLedger.
func TestPurgeStatusNamesARemedyForAnUnreadableLedger(t *testing.T) {
	city := newConvergedPurgeCity(t)
	if err := os.WriteFile(city.target.PurgeLedgerPath(), []byte("{not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runPurgeStatus(t, city)
	if code == 0 || !strings.Contains(out, "UNREADABLE") || !strings.Contains(out, "blocking invariant") || !strings.Contains(out, "backups") {
		t.Errorf("status over an unreadable ledger exited %d without a remedy:\n%s", code, out)
	}
}
