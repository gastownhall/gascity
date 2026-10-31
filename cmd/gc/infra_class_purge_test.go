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
	"github.com/gastownhall/gascity/internal/pathutil"
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

// confirmedPurge is the options of a purge of n reviewed rows, confirmed as an
// operator confirms one: with the count and the digest a dry run printed. A
// dry run that selects nothing prints no digest, and a run that selects
// nothing asks for none.
func confirmedPurge(t *testing.T, city purgeCity, n int) strandedPurgeOptions {
	t.Helper()
	var stdout, stderr bytes.Buffer
	runStrandedPurge(t, city, strandedPurgeOptions{Type: "message", DryRun: true, ExpectCount: -1}, &stdout, &stderr)
	opts := strandedPurgeOptions{Type: "message", Confirm: true, ExpectCount: n}
	opts.ExpectDigest, _ = purgeDigestIn(stdout.String())
	return opts
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
	if code := doStoragePurgeStranded(context.Background(), request, confirmedPurge(t, city, 791), &stdout, &stderr); code == 0 {
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
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 791), &stdout, &stderr); code == 0 {
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
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 791), &stdout, &stderr); code != 0 {
		t.Fatalf("purge exited %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}

	files := purgeBackupFiles(t, city)
	if len(files) != 1 {
		t.Fatalf("purge backups = %v, want exactly one", files)
	}
	// By location, not spelling: the binding root is canonicalized, which on
	// macOS spells the city's /private/var/... as /var/....
	storeDir := filepath.Join(city.cityPath, ".gc", "store")
	if !pathutil.PathWithin(storeDir, files[0]) {
		t.Errorf("the backup %s is not under %s", files[0], storeDir)
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
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 791), &stdout, &stderr); code != 0 {
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
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 2), &stdout, &stderr); code == 0 {
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
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 1), &stdout, &stderr); code != 0 {
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
	binding := beads.MailStore{Store: openMigratedDestination(t, city.target)}
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
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 791), &stdout, &stderr); code == 0 {
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
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 781), &stdout, &stderr); code != 0 {
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

// purgeDigestIn reads the digest a dry run printed, and whether it printed one.
func purgeDigestIn(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "digest:"); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

// purgeDigestFrom reads the digest a dry run printed.
func purgeDigestFrom(t *testing.T, out string) string {
	t.Helper()
	digest, ok := purgeDigestIn(out)
	if !ok {
		t.Fatalf("the dry run printed no digest:\n%s", out)
	}
	return digest
}

// TestPurgeStrandedDeleteIsTiedToTheReviewedDryRun: the delete needs the count
// and the digest the dry run printed, and refuses a count or digest that is
// not the selection's. The count alone does not pin the selection: one
// stranded row leaving and another arriving keeps it.
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
	for name, tc := range map[string]struct {
		opts strandedPurgeOptions
		// refusal is what the refusal must say.
		refusal string
	}{
		"no count":     {strandedPurgeOptions{Type: "message", Confirm: true, ExpectCount: -1, ExpectDigest: digest}, fmt.Sprintf("--%s %d", strandedExpectCountFlag, mcPurgeRows)},
		"wrong count":  {strandedPurgeOptions{Type: "message", Confirm: true, ExpectCount: mcPurgeRows - 1, ExpectDigest: digest}, "does not match the"},
		"no digest":    {strandedPurgeOptions{Type: "message", Confirm: true, ExpectCount: mcPurgeRows}, fmt.Sprintf("--%s %s", strandedExpectDigestFlag, digest)},
		"wrong digest": {strandedPurgeOptions{Type: "message", Confirm: true, ExpectCount: mcPurgeRows, ExpectDigest: "0000000000000000"}, "does not match the selection's digest"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := runStrandedPurge(t, city, tc.opts, &stdout, &stderr); code == 0 {
			t.Errorf("%s: the purge exited 0", name)
		}
		if !strings.Contains(stderr.String(), tc.refusal) {
			t.Errorf("%s: the refusal does not say %q: %s", name, tc.refusal, stderr.String())
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
	reviewed := strandedPurgeOptions{Type: "message", Confirm: true, ExpectCount: mcPurgeRows, ExpectDigest: digest}
	if code := runStrandedPurge(t, city, reviewed, &stdout, &stderr); code != 0 {
		t.Fatalf("the purge with the reviewed count and digest exited %d: %s", code, stderr.String())
	}
}

// TestPurgeStrandedFinishesARunThatDiedAfterTheDelete: every row is gone but
// the run died before recording it, so nothing is stranded and the ledger
// says INTERRUPTED. Status refuses until the next run records the deleted
// rows, which it does with nothing left to select; its dry run says so and
// records nothing.
func TestPurgeStrandedFinishesARunThatDiedAfterTheDelete(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	prev := infraPurgeAfterDeleteRecorded
	infraPurgeAfterDeleteRecorded = func() error { return errors.New("simulated crash after the delete") }
	t.Cleanup(func() { infraPurgeAfterDeleteRecorded = prev })
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, mcPurgeRows), &stdout, &stderr); code == 0 {
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

	// The dry run says what the run will do, and records nothing.
	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, strandedPurgeOptions{Type: "message", DryRun: true, ExpectCount: -1}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "To finish the interrupted purge run") {
		t.Fatalf("the dry run of the finishing run exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	if code, out := runPurgeStatus(t, city); code == 0 || !strings.Contains(out, "purge: INTERRUPTED") {
		t.Fatalf("the dry run recorded the run: status exited %d:\n%s", code, out)
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

// TestPurgeStrandedResumeLeavesARowStrandedSinceTheBackup: a row stranded
// after the interrupted run's backup is not one of the run's rows. The resume
// names it and leaves it in place, rather than writing a second backup and
// splitting the run's accounting, and finishes the run over the rows its
// backup holds. The purge after it takes the new row as a run of its own.
func TestPurgeStrandedResumeLeavesARowStrandedSinceTheBackup(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	finish := failPurgeAt(t, &infraPurgeAfterBackup, "simulated crash after the backup")
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, mcPurgeRows), &stdout, &stderr); code == 0 {
		t.Fatalf("the crashed run exited 0")
	}
	finish()
	late := mcAutoHandoffMail(t, 3)
	late.ID = "mc-wisp-late"
	editPurgeWorkStore(t, city.workPath, []beads.Bead{late})

	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, strandedPurgeOptions{Type: "message", DryRun: true, ExpectCount: -1}, &stdout, &stderr); code != 0 {
		t.Fatalf("the dry run of the resume exited %d: %s", code, stderr.String())
	}
	for _, want := range []string{fmt.Sprintf("selected: %d stranded", mcPurgeRows), "not in the interrupted run: 1 stranded message row(s)", "mc-wisp-late"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("the dry run of the resume lacks %q:\n%s", want, stdout.String())
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, mcPurgeRows), &stdout, &stderr); code != 0 {
		t.Fatalf("the resume exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "resuming") {
		t.Errorf("the run did not resume:\n%s", stdout.String())
	}
	if files := purgeBackupFiles(t, city); len(files) != 1 {
		t.Errorf("backups = %v, want only the interrupted run's", files)
	}
	rows := purgeWorkRows(t, city)
	if got := countPurgeRowsOfType(rows, "message"); got != 1 || !slices.ContainsFunc(rows, func(b beads.Bead) bool { return b.ID == late.ID }) {
		t.Errorf("the resume did not leave exactly %s: %d message row(s) left", late.ID, got)
	}
	if code, out := runPurgeStatus(t, city); code == 0 || strings.Contains(out, "INTERRUPTED") || !strings.Contains(out, fmt.Sprintf("purged stranded: %d bead(s)", mcPurgeRows)) || !strings.Contains(out, "stranded:    1") {
		t.Errorf("status after the resume exited %d, and should show the run finished and the new row stranded:\n%s", code, out)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 1), &stdout, &stderr); code != 0 {
		t.Fatalf("the purge of the new row exited %d: %s", code, stderr.String())
	}
	if files := purgeBackupFiles(t, city); len(files) != 2 {
		t.Errorf("backups = %v, want the interrupted run's and the new run's", files)
	}
	if code, out := runPurgeStatus(t, city); code != 0 || !strings.Contains(out, fmt.Sprintf("purged stranded: %d bead(s)", mcPurgeRows+1)) {
		t.Errorf("status after both runs exited %d:\n%s", code, out)
	}
}

// TestPurgeStrandedRefusesAResumeOverARowThatChangedSinceTheBackup: a row of
// the interrupted run that changed since its backup is not deleted over that
// backup. The resume refuses, its dry run too, and writes nothing, and
// --abandon-purge, which the refusal names, closes the run once its rows and
// edges are all there.
func TestPurgeStrandedRefusesAResumeOverARowThatChangedSinceTheBackup(t *testing.T) {
	city := newSmallPurgeCity(t, 3)
	finish := failPurgeAt(t, &infraPurgeAfterBackup, "simulated crash after the backup")
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 3), &stdout, &stderr); code == 0 {
		t.Fatalf("the crashed run exited 0")
	}
	finish()
	title := "read since the backup"
	if err := openPurgeWorkStore(t, city.workPath).Update("mc-wisp-0002", beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatal(err)
	}

	for _, opts := range []strandedPurgeOptions{
		{Type: "message", DryRun: true, ExpectCount: -1},
		confirmedPurge(t, city, 3),
	} {
		stdout.Reset()
		stderr.Reset()
		if code := runStrandedPurge(t, city, opts, &stdout, &stderr); code == 0 {
			t.Fatalf("a resume (dry run %v) over a row changed since the backup exited 0:\n%s", opts.DryRun, stdout.String())
		}
		for _, want := range []string{"mc-wisp-0002", "changed since", "--" + strandedAbandonPurgeFlag} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("the refusal (dry run %v) lacks %q: %s", opts.DryRun, want, stderr.String())
			}
		}
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != 3 {
		t.Fatalf("the refused resume deleted rows: %d left", got)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, strandedPurgeOptions{Type: "message", Abandon: true, ExpectCount: -1}, &stdout, &stderr); code != 0 {
		t.Fatalf("abandon with every row and edge there exited %d: %s", code, stderr.String())
	}
	if _, out := runPurgeStatus(t, city); strings.Contains(out, "INTERRUPTED") || !strings.Contains(out, "stranded:    3") {
		t.Errorf("status after abandon should show the rows stranded and no interrupted purge:\n%s", out)
	}
}

// TestPurgeStrandedFinishesARunWhoseDeletedIDWasMintedAgain: an interrupted
// run deleted a row, and bd minted the row's id again for a new stranded
// message before the run was finished. The new bead is not the backed-up row,
// so the resume leaves it in place and finishes the run over the run's own
// rows. --abandon-purge and --restore-purge cannot close the run while the id
// is taken: they refuse, name the id, and name running the purge again. The
// purge after the run takes the new bead as a run of its own.
func TestPurgeStrandedFinishesARunWhoseDeletedIDWasMintedAgain(t *testing.T) {
	for _, tc := range []struct {
		name string
		// crash is the seam the run dies at.
		crash *func() error
		// deleted are the run's rows the interrupted delete took before the
		// crash point.
		deleted []string
		// left is how many of the run's rows are still to delete.
		left int
	}{
		{"with rows left to delete", &infraPurgeAfterBackup, []string{"mc-wisp-0002"}, 2},
		{"with nothing left to delete", &infraPurgeAfterDeleteRecorded, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := newSmallPurgeCity(t, 3)
			finish := failPurgeAt(t, tc.crash, "simulated crash")
			var stdout, stderr bytes.Buffer
			if code := runStrandedPurge(t, city, confirmedPurge(t, city, 3), &stdout, &stderr); code == 0 {
				t.Fatalf("the crashed run exited 0")
			}
			finish()
			backup := purgeBackupFiles(t, city)[0]
			work := openPurgeWorkStore(t, city.workPath)
			for _, id := range tc.deleted {
				if err := work.Delete(id); err != nil {
					t.Fatal(err)
				}
			}
			again := mcAutoHandoffMail(t, 2)
			again.Title = "a handoff written after the crash"
			again.CreatedAt = mustParseTime(t, "2026-10-01T00:00:00Z")
			again.UpdatedAt = again.CreatedAt
			editPurgeWorkStore(t, city.workPath, []beads.Bead{again})
			if got := strandedIDs(t, city.cityPath, city.target); len(got) != tc.left+1 || !slices.Contains(got, again.ID) {
				t.Fatalf("stranded = %v, want the run's %d row(s) left and the new %s; the scenario under test was never created", got, tc.left, again.ID)
			}

			// The exits that cannot close the run name the one that can.
			stdout.Reset()
			stderr.Reset()
			if code := runStrandedPurge(t, city, strandedPurgeOptions{Type: "message", Abandon: true, ExpectCount: -1}, &stdout, &stderr); code == 0 {
				t.Fatalf("abandon with a row of the run gone exited 0")
			}
			for _, want := range []string{again.ID, "minted the id again", "running it again without --" + strandedAbandonPurgeFlag} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("the abandon refusal lacks %q: %s", want, stderr.String())
				}
			}
			request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}
			for _, dryRun := range []bool{true, false} {
				stdout.Reset()
				stderr.Reset()
				if code := doStorageRestorePurge(context.Background(), request, backup, dryRun, &stdout, &stderr); code == 0 {
					t.Fatalf("a restore (dry run %v) over the id minted again exited 0:\n%s", dryRun, stdout.String())
				}
				for _, want := range []string{again.ID, "minted the id again", "running the purge again"} {
					if !strings.Contains(stderr.String(), want) {
						t.Errorf("the restore refusal (dry run %v) lacks %q: %s", dryRun, want, stderr.String())
					}
				}
			}
			if code, out := runPurgeStatus(t, city); code == 0 || !strings.Contains(out, "purge: INTERRUPTED") {
				t.Fatalf("a refused exit closed the run: status exited %d:\n%s", code, out)
			}

			// The purge finishes the run over its own rows and leaves the new bead.
			stdout.Reset()
			stderr.Reset()
			if code := runStrandedPurge(t, city, strandedPurgeOptions{Type: "message", DryRun: true, ExpectCount: -1}, &stdout, &stderr); code != 0 {
				t.Fatalf("the dry run of the finishing run exited %d: %s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "not in the interrupted run: 1 stranded message row(s)") || !strings.Contains(stdout.String(), again.ID) {
				t.Errorf("the dry run does not name the new bead it leaves:\n%s", stdout.String())
			}
			stdout.Reset()
			stderr.Reset()
			if code := runStrandedPurge(t, city, confirmedPurge(t, city, tc.left), &stdout, &stderr); code != 0 {
				t.Fatalf("the finishing run exited %d: %s", code, stderr.String())
			}
			rows := purgeWorkRows(t, city)
			if got := countPurgeRowsOfType(rows, "message"); got != 1 {
				t.Errorf("%d message row(s) after the finishing run, want only the new %s", got, again.ID)
			}
			for _, b := range rows {
				if b.ID == again.ID && !sameCreatedSecond(b.CreatedAt, again.CreatedAt) {
					t.Errorf("%s is the backed-up row again, not the new bead: created %s", again.ID, b.CreatedAt)
				}
			}
			if code, out := runPurgeStatus(t, city); code == 0 || strings.Contains(out, "INTERRUPTED") || !strings.Contains(out, "purged stranded: 3 bead(s)") || !strings.Contains(out, "stranded:    1") {
				t.Errorf("status after the finishing run exited %d, and should show the run finished and the new bead stranded:\n%s", code, out)
			}

			// The new bead goes with a purge of its own.
			stdout.Reset()
			stderr.Reset()
			if code := runStrandedPurge(t, city, confirmedPurge(t, city, 1), &stdout, &stderr); code != 0 {
				t.Fatalf("the purge of the new bead exited %d: %s", code, stderr.String())
			}
			if code, out := runPurgeStatus(t, city); code != 0 || !strings.Contains(out, "purged stranded: 4 bead(s)") {
				t.Errorf("status after both runs exited %d:\n%s", code, out)
			}
		})
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
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, mcPurgeRows), &stdout, &stderr); code == 0 {
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
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, mcPurgeRows), &stdout, &stderr); code == 0 {
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
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, mcPurgeRows), &stdout, &stderr); code == 0 {
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
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, mcPurgeRows), &stdout, &stderr); code != 0 {
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

// TestRestorePurgeRestoresOnlyWhatAPurgeRecorded: a backup the ledger does
// not know, or a known backup with a row or an edge added by hand, is refused
// before anything is written, so the restore cannot write arbitrary rows under
// fixed ids, or an edge between rows the purge never touched.
func TestRestorePurgeRestoresOnlyWhatAPurgeRecorded(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, mcPurgeRows), &stdout, &stderr); code != 0 {
		t.Fatalf("purge exited %d: %s", code, stderr.String())
	}
	backup := purgeBackupFiles(t, city)[0]
	raw, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	records, err := readInfraPurgeBackup(backup)
	if err != nil {
		t.Fatal(err)
	}
	forged := infraRecoverChoiceRecord{
		Format: infraRecoverChoicesFormat, Choice: infraPurgeChoice, Store: infraRecoverChoiceStoreWork,
		Entry: infraBackupEntry{Bead: beads.Bead{ID: "mc-forged", Title: "not purged", Status: "open", Type: "message", CreatedAt: mustParseTime(t, mcFirstMailAt)}},
	}
	line, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}

	// A known backup with a row added by hand.
	if err := os.WriteFile(backup, append(append(raw, line...), '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := doStorageRestorePurge(context.Background(), request, backup, false, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "mc-forged") {
		t.Errorf("a backup with a forged row restored (exit %d): %s", code, stderr.String())
	}

	// A known backup with an edge added by hand between two work beads the
	// purge never touched.
	workID := purgeWorkBeadID(t, city)
	other := mustCreateInfraBead(t, openPurgeWorkStore(t, city.workPath), beads.Bead{Title: "other backlog work", Type: "task"})
	forgedEdge := infraBackupEdge{IssueID: other.ID, DependsOnID: workID, Type: "blocks"}
	edited := slices.Clone(records)
	edited[0].Entry.Deps = append(slices.Clone(edited[0].Entry.Deps), forgedEdge)
	var lines bytes.Buffer
	for _, record := range edited {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		lines.Write(append(line, '\n'))
	}
	if err := os.WriteFile(backup, lines.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := doStorageRestorePurge(context.Background(), request, backup, false, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), forgedEdge.String()) {
		t.Errorf("a backup with a forged edge restored (exit %d): %s", code, stderr.String())
	}
	if deps, err := openPurgeWorkStore(t, city.workPath).DepList(other.ID, "down"); err != nil || len(deps) != 0 {
		t.Errorf("a refused restore wrote the forged edge: %+v (err %v)", deps, err)
	}

	// A file the ledger never recorded.
	elsewhere := filepath.Join(t.TempDir(), "hand-built.jsonl")
	if err := os.WriteFile(elsewhere, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := doStorageRestorePurge(context.Background(), request, elsewhere, false, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "not a backup the purge ledger records") {
		t.Errorf("a backup the ledger does not know restored (exit %d): %s", code, stderr.String())
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != 0 {
		t.Errorf("a refused restore wrote %d row(s)", got)
	}
}

// TestPurgeStrandedResumesAfterTheEdgesWereRemoved: a run that died after it
// removed the edges and before it deleted the rows resumes from its backup,
// which still holds the edges, rather than refusing.
func TestPurgeStrandedResumesAfterTheEdgesWereRemoved(t *testing.T) {
	city := newMCShapedPurgeCity(t)
	var workID string
	for _, b := range purgeWorkRows(t, city) {
		if b.Type == "task" {
			workID = b.ID
		}
	}
	if err := openPurgeWorkStore(t, city.workPath).DepAdd("mc-wisp-0001", workID, "relates-to"); err != nil {
		t.Fatal(err)
	}
	prev := infraPurgeAfterEdgesRemoved
	infraPurgeAfterEdgesRemoved = func() error { return errors.New("simulated crash after the edges") }
	t.Cleanup(func() { infraPurgeAfterEdgesRemoved = prev })
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, mcPurgeRows), &stdout, &stderr); code == 0 {
		t.Fatalf("the crashed run exited 0")
	}
	infraPurgeAfterEdgesRemoved = prev
	if deps, _ := openPurgeWorkStore(t, city.workPath).DepList("mc-wisp-0001", "down"); len(deps) != 0 {
		t.Fatalf("the crash point is after the edge removal, yet the edge is still there: %+v", deps)
	}
	files := purgeBackupFiles(t, city)

	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, mcPurgeRows), &stdout, &stderr); code != 0 {
		t.Fatalf("the resume exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "resuming") {
		t.Errorf("the run did not resume:\n%s", stdout.String())
	}
	if after := purgeBackupFiles(t, city); !slices.Equal(after, files) {
		t.Errorf("backups = %v, want only %v", after, files)
	}
	records, err := readInfraPurgeBackup(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Entry.Bead.ID == "mc-wisp-0001" && len(record.Entry.Deps) != 1 {
			t.Errorf("the backup lost the edge: %+v", record.Entry.Deps)
		}
	}
	if code, out := runPurgeStatus(t, city); code != 0 || !strings.Contains(out, "purged stranded: 791 bead(s)") {
		t.Errorf("status after the resume exited %d:\n%s", code, out)
	}
}

// newSmallPurgeCity is newConvergedPurgeCity after its work store gained n
// stranded auto-handoff mails, mc-wisp-0001 .. mc-wisp-<n>.
func newSmallPurgeCity(t *testing.T, n int) purgeCity {
	t.Helper()
	city := newConvergedPurgeCity(t)
	mails := make([]beads.Bead, 0, n)
	for i := 1; i <= n; i++ {
		mails = append(mails, mcAutoHandoffMail(t, i))
	}
	editPurgeWorkStore(t, city.workPath, mails)
	if got := strandedIDs(t, city.cityPath, city.target); len(got) != n {
		t.Fatalf("the fixture produced %d stranded row(s), want %d; the scenario under test was never created", len(got), n)
	}
	return city
}

// purgeWorkBeadID is the id of the fixture's one work bead.
func purgeWorkBeadID(t *testing.T, city purgeCity) string {
	t.Helper()
	for _, b := range purgeWorkRows(t, city) {
		if b.Type == "task" {
			return b.ID
		}
	}
	t.Fatal("the fixture has no work bead")
	return ""
}

// failPurgeAt makes seam fail, standing in for a crash at that point, until
// the returned func puts it back; the test's cleanup puts it back too.
func failPurgeAt(t *testing.T, seam *func() error, crash string) func() {
	t.Helper()
	prev := *seam
	*seam = func() error { return errors.New(crash) }
	restore := func() { *seam = prev }
	t.Cleanup(restore)
	return restore
}

// openPurgeWorkStoreAs points the work-store opener at the fixture's work store
// as wrap presents it.
func openPurgeWorkStoreAs(t *testing.T, city purgeCity, wrap func(*beads.FileStore) beads.Store) {
	t.Helper()
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) {
		store, err := beads.OpenFileStore(fsys.OSFS{}, city.workPath)
		if err != nil {
			return nil, err
		}
		return wrap(store), nil
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })
}

// purgeInlineEdgesStore is a FileStore that lists its rows the way bd lists
// them (bdIssue.toBead): each with its outbound edges inline, with is_blocked,
// and with a parent read off a parent-child edge. Native Dolt lists the edges
// inline too. The purge's own edge removal changes all three.
type purgeInlineEdgesStore struct{ *beads.FileStore }

func (s purgeInlineEdgesStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.FileStore.List(query)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		deps, err := s.DepList(rows[i].ID, "down")
		if err != nil {
			return nil, err
		}
		blocked := false
		for _, d := range deps {
			switch d.Type {
			case "blocks":
				blocked = true
			case "parent-child":
				if rows[i].ParentID == "" {
					rows[i].ParentID = d.DependsOnID
				}
			}
		}
		rows[i].Dependencies = deps
		rows[i].IsBlocked = &blocked
	}
	return rows, nil
}

// ImportVerbatim imports as bd import does: bd keeps no parent of its own and
// reads one off the parent-child edge, so an imported row reads back with no
// parent until its edge is added again.
func (s purgeInlineEdgesStore) ImportVerbatim(rows []beads.Bead) error {
	imported := make([]beads.Bead, 0, len(rows))
	for _, b := range rows {
		b.ParentID = ""
		imported = append(imported, b)
	}
	return s.FileStore.ImportVerbatim(imported)
}

// purgePlainStore is a work store with the Store surface and none of the
// optional capabilities: no edge-payload read, as the bd CLI provider has
// none, and no verbatim import, as native Dolt has none. The production policy
// wrapper declares both whatever the store under it can do.
type purgePlainStore struct{ beads.Store }

// TestPurgeStrandedResumesAfterTheEdgesWereRemovedOnAStoreThatListsEdgesInline:
// bd lists a row with its outbound edges inline, with is_blocked, and with a
// parent read off a parent-child edge, so the run's own edge removal changes
// how its rows read back. A run interrupted after the edge removal resumes
// from its backup all the same, and the backup keeps the edges.
func TestPurgeStrandedResumesAfterTheEdgesWereRemovedOnAStoreThatListsEdgesInline(t *testing.T) {
	city := newSmallPurgeCity(t, 3)
	workID := purgeWorkBeadID(t, city)
	work := openPurgeWorkStore(t, city.workPath)
	if err := work.DepAdd("mc-wisp-0001", workID, "blocks"); err != nil {
		t.Fatal(err)
	}
	if err := work.DepAdd("mc-wisp-0002", workID, "parent-child"); err != nil {
		t.Fatal(err)
	}
	openPurgeWorkStoreAs(t, city, func(store *beads.FileStore) beads.Store { return purgeInlineEdgesStore{store} })
	finish := failPurgeAt(t, &infraPurgeAfterEdgesRemoved, "simulated crash after the edges")
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 3), &stdout, &stderr); code == 0 {
		t.Fatalf("the crashed run exited 0")
	}
	finish()
	if deps, err := openPurgeWorkStore(t, city.workPath).DepList(workID, "up"); err != nil || len(deps) != 0 {
		t.Fatalf("the crash point is after the edge removal, yet edges into %s are left: %+v (err %v)", workID, deps, err)
	}
	files := purgeBackupFiles(t, city)

	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 3), &stdout, &stderr); code != 0 {
		t.Fatalf("the resume exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "resuming") {
		t.Errorf("the run did not resume:\n%s", stdout.String())
	}
	if after := purgeBackupFiles(t, city); !slices.Equal(after, files) {
		t.Errorf("backups = %v, want only %v", after, files)
	}
	records, err := readInfraPurgeBackup(files[0])
	if err != nil {
		t.Fatal(err)
	}
	backed := map[string]infraBackupEntry{}
	for _, record := range records {
		backed[record.Entry.Bead.ID] = record.Entry
	}
	if deps := backed["mc-wisp-0001"].Deps; len(deps) != 1 || deps[0].DependsOnID != workID || deps[0].Type != "blocks" {
		t.Errorf("the backup lost mc-wisp-0001's edge: %+v", deps)
	}
	if entry := backed["mc-wisp-0002"]; len(entry.Deps) != 1 || entry.Deps[0].Type != "parent-child" || entry.Bead.ParentID != workID {
		t.Errorf("the backup lost mc-wisp-0002's parent edge: %+v", entry)
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != 0 {
		t.Errorf("%d message row(s) left after the resume", got)
	}
	if code, out := runPurgeStatus(t, city); code != 0 || !strings.Contains(out, "purged stranded: 3 bead(s)") {
		t.Errorf("status after the resume exited %d:\n%s", code, out)
	}
}

// TestPurgeStrandedAbandonRefusesWhileTheRunsRemovedEdgesAreMissing: a run
// interrupted after its edge removal leaves every row in place and the edges
// gone, with the backup their only copy. --abandon-purge would close the run
// and leave that copy listed nowhere, so it refuses while an edge whose two
// ends are in the work store is missing, and names --restore-purge, which puts
// the edges back. Once they are back by hand it closes the run. An edge whose
// far end is gone cannot be put back, and does not hold the run open.
func TestPurgeStrandedAbandonRefusesWhileTheRunsRemovedEdgesAreMissing(t *testing.T) {
	city := newSmallPurgeCity(t, 2)
	workID := purgeWorkBeadID(t, city)
	work := openPurgeWorkStore(t, city.workPath)
	doomed := mustCreateInfraBead(t, work, beads.Bead{Title: "work deleted after the crash", Type: "task"})
	for _, edge := range []beads.Dep{
		{IssueID: "mc-wisp-0001", DependsOnID: workID, Type: "relates-to"},
		{IssueID: workID, DependsOnID: "mc-wisp-0002", Type: "blocks"},
		{IssueID: "mc-wisp-0002", DependsOnID: doomed.ID, Type: "relates-to"},
	} {
		if err := work.DepAdd(edge.IssueID, edge.DependsOnID, edge.Type); err != nil {
			t.Fatal(err)
		}
	}
	finish := failPurgeAt(t, &infraPurgeAfterEdgesRemoved, "simulated crash after the edges")
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 2), &stdout, &stderr); code == 0 {
		t.Fatalf("the crashed run exited 0")
	}
	finish()
	if err := openPurgeWorkStore(t, city.workPath).Delete(doomed.ID); err != nil {
		t.Fatal(err)
	}
	backup := purgeBackupFiles(t, city)[0]

	abandon := strandedPurgeOptions{Type: "message", Abandon: true, ExpectCount: -1}
	for _, dryRun := range []bool{true, false} {
		opts := abandon
		opts.DryRun = dryRun
		stdout.Reset()
		stderr.Reset()
		if code := runStrandedPurge(t, city, opts, &stdout, &stderr); code == 0 {
			t.Fatalf("abandon (dry run %v) with the run's edges missing exited 0: %s", dryRun, stdout.String())
		}
		for _, want := range []string{"mc-wisp-0001 -[relates-to]-> " + workID, workID + " -[blocks]-> mc-wisp-0002", "--" + strandedRestorePurgeFlag + " " + backup} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("the refusal (dry run %v) lacks %q: %s", dryRun, want, stderr.String())
			}
		}
		if strings.Contains(stderr.String(), doomed.ID) {
			t.Errorf("the refusal (dry run %v) holds the run open over an edge whose far end is gone: %s", dryRun, stderr.String())
		}
	}
	if code, out := runPurgeStatus(t, city); code == 0 || !strings.Contains(out, "purge: INTERRUPTED") {
		t.Fatalf("a refused abandon closed the run: status exited %d:\n%s", code, out)
	}

	// The raw bd fallback puts the edges back by hand.
	work = openPurgeWorkStore(t, city.workPath)
	if err := work.DepAdd("mc-wisp-0001", workID, "relates-to"); err != nil {
		t.Fatal(err)
	}
	if err := work.DepAdd(workID, "mc-wisp-0002", "blocks"); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, abandon, &stdout, &stderr); code != 0 {
		t.Fatalf("abandon with the rows and edges back exited %d: %s", code, stderr.String())
	}
	if _, out := runPurgeStatus(t, city); strings.Contains(out, "INTERRUPTED") || !strings.Contains(out, "stranded:    2") {
		t.Errorf("status after abandon should show the rows stranded and no interrupted purge:\n%s", out)
	}
}

// TestPurgeDecidesTheStoreCapabilitiesUnderThePolicyWrapper: every production
// open hands the work store out policy-wrapped, and the wrapper declares every
// optional capability whatever the store under it can do. The purge decides
// them under the wrapper. An edge on a store with no edge-payload read (the bd
// CLI provider) is backed up without a payload instead of refusing the run,
// and the restore rehearsal on a store that cannot import verbatim (native
// Dolt) refuses where the restore would, instead of reporting the rollback
// ready.
func TestPurgeDecidesTheStoreCapabilitiesUnderThePolicyWrapper(t *testing.T) {
	city := newSmallPurgeCity(t, 2)
	workID := purgeWorkBeadID(t, city)
	if err := openPurgeWorkStore(t, city.workPath).DepAdd(workID, "mc-wisp-0001", "blocks"); err != nil {
		t.Fatal(err)
	}
	openPurgeWorkStoreAs(t, city, func(store *beads.FileStore) beads.Store {
		return wrapStoreWithBeadPolicies(purgePlainStore{store}, city.cfg)
	})
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, strandedPurgeOptions{Type: "message", DryRun: true, ExpectCount: -1}, &stdout, &stderr); code != 0 {
		t.Fatalf("the dry run over a store with no edge-payload read exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "edges:   0 outbound, 1 inbound") {
		t.Errorf("the dry run does not count the inbound edge:\n%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 2), &stdout, &stderr); code != 0 {
		t.Fatalf("the purge over a store with no edge-payload read exited %d: %s", code, stderr.String())
	}
	backup := purgeBackupFiles(t, city)[0]
	records, err := readInfraPurgeBackup(backup)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Entry.Bead.ID != "mc-wisp-0001" {
			continue
		}
		if edges := record.Entry.Dependents; len(edges) != 1 || edges[0].IssueID != workID || edges[0].Carried || edges[0].Payload != "" {
			t.Errorf("the backup of mc-wisp-0001 does not carry its inbound edge without a payload: %+v", edges)
		}
	}

	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}
	for _, dryRun := range []bool{true, false} {
		stdout.Reset()
		stderr.Reset()
		if code := doStorageRestorePurge(context.Background(), request, backup, dryRun, &stdout, &stderr); code == 0 {
			t.Fatalf("a restore (dry run %v) into a store that cannot import verbatim exited 0:\n%s", dryRun, stdout.String())
		}
		if !strings.Contains(stderr.String(), "cannot import rows verbatim") {
			t.Errorf("the refusal (dry run %v) does not say the store cannot import verbatim: %s", dryRun, stderr.String())
		}
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != 0 {
		t.Fatalf("a refused restore wrote %d row(s)", got)
	}

	// Under the same wrapper, a store that imports verbatim restores.
	openPurgeWorkStoreAs(t, city, func(store *beads.FileStore) beads.Store {
		return wrapStoreWithBeadPolicies(store, city.cfg)
	})
	stdout.Reset()
	stderr.Reset()
	if code := doStorageRestorePurge(context.Background(), request, backup, false, &stdout, &stderr); code != 0 {
		t.Fatalf("the restore through the policy wrapper exited %d: %s", code, stderr.String())
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != 2 {
		t.Errorf("%d message row(s) after the restore, want 2", got)
	}
	if deps, err := openPurgeWorkStore(t, city.workPath).DepList(workID, "down"); err != nil || len(deps) != 1 || deps[0].DependsOnID != "mc-wisp-0001" {
		t.Errorf("the inbound edge did not come back: %+v (err %v)", deps, err)
	}
}

// TestRestorePurgeOfAnotherBackupRefusesWhileARunIsInterrupted: restoring one
// run's backup while another run is interrupted would close the interrupted
// run, and the rows it deleted would drop out of the ledger's accounting. The
// restore refuses before anything is written. Once the interrupted run is
// finished, the restore runs and both runs stay accounted for.
func TestRestorePurgeOfAnotherBackupRefusesWhileARunIsInterrupted(t *testing.T) {
	city := newSmallPurgeCity(t, 2)
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 2), &stdout, &stderr); code != 0 {
		t.Fatalf("the first purge exited %d: %s", code, stderr.String())
	}
	first := purgeBackupFiles(t, city)[0]

	editPurgeWorkStore(t, city.workPath, []beads.Bead{mcAutoHandoffMail(t, 3), mcAutoHandoffMail(t, 4), mcAutoHandoffMail(t, 5)})
	finish := failPurgeAt(t, &infraPurgeAfterDeleteRecorded, "simulated crash after the delete")
	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 3), &stdout, &stderr); code == 0 {
		t.Fatalf("the crashed second run exited 0")
	}
	finish()
	var second string
	for _, file := range purgeBackupFiles(t, city) {
		if file != first {
			second = file
		}
	}
	if second == "" {
		t.Fatal("the second run wrote no backup")
	}

	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}
	for _, dryRun := range []bool{true, false} {
		stdout.Reset()
		stderr.Reset()
		if code := doStorageRestorePurge(context.Background(), request, first, dryRun, &stdout, &stderr); code == 0 {
			t.Fatalf("a restore (dry run %v) of the first run while the second is interrupted exited 0:\n%s", dryRun, stdout.String())
		}
		if !strings.Contains(stderr.String(), second) {
			t.Errorf("the refusal (dry run %v) does not name the interrupted run's backup %s: %s", dryRun, second, stderr.String())
		}
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != 0 {
		t.Fatalf("a refused restore wrote %d row(s)", got)
	}
	if code, out := runPurgeStatus(t, city); code == 0 || !strings.Contains(out, "purge: INTERRUPTED") {
		t.Fatalf("a refused restore closed the interrupted run: status exited %d:\n%s", code, out)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, strandedPurgeOptions{Type: "message", ExpectCount: -1}, &stdout, &stderr); code != 0 {
		t.Fatalf("finishing the second run exited %d: %s", code, stderr.String())
	}
	if code, out := runPurgeStatus(t, city); code != 0 || !strings.Contains(out, "purged stranded: 5 bead(s)") {
		t.Fatalf("status after the second run finished exited %d:\n%s", code, out)
	}
	stdout.Reset()
	stderr.Reset()
	if code := doStorageRestorePurge(context.Background(), request, first, false, &stdout, &stderr); code != 0 {
		t.Fatalf("the restore of the first run exited %d: %s", code, stderr.String())
	}
	if _, out := runPurgeStatus(t, city); !strings.Contains(out, "purged stranded: 3 bead(s)") || !strings.Contains(out, "restored by --restore-purge: 2 row(s)") || strings.Contains(out, "INTERRUPTED") {
		t.Errorf("status after the restore does not account for both runs:\n%s", out)
	}
}

// TestPurgePendingRunIsReadPerRun: a run is interrupted until a record of that
// same run closes it. A record of another run that lands after its backup does
// not close it.
func TestPurgePendingRunIsReadPerRun(t *testing.T) {
	record := func(run, event string) infraPurgeLedgerRecord {
		return infraPurgeLedgerRecord{Format: infraPurgeLedgerFormat, Event: event, Run: run, Type: "message", Backup: "/store/" + run + ".jsonl"}
	}
	finished := []infraPurgeLedgerRecord{record("a", infraPurgeEventBackedUp), record("a", infraPurgeEventPurged)}
	interrupted := append(slices.Clone(finished), record("b", infraPurgeEventBackedUp))
	for _, tc := range []struct {
		name    string
		ledger  []infraPurgeLedgerRecord
		pending string
	}{
		{"no ledger", nil, ""},
		{"a finished run", finished, ""},
		{"an interrupted run", interrupted, "b"},
		{"an earlier run restored after it", append(slices.Clone(interrupted), record("a", infraPurgeEventRestored)), "b"},
		{"the run restored", append(slices.Clone(interrupted), record("b", infraPurgeEventRestored)), ""},
		{"the run abandoned", append(slices.Clone(interrupted), record("b", infraPurgeEventAbandoned)), ""},
		{"the run finished", append(slices.Clone(interrupted), record("b", infraPurgeEventPurged)), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, pending := infraPurgePendingRun(tc.ledger)
			if pending != (tc.pending != "") || got.Run != tc.pending {
				t.Errorf("infraPurgePendingRun = (%q, %v), want run %q pending", got.Run, pending, tc.pending)
			}
		})
	}
}

// TestRestorePurgeMatchesTheLedgerBackupByLocation: the ledger names a backup
// in the binding root's canonical spelling, and an operator's path to the same
// file need not resolve to it: on macOS the ledger says /var/... where the file
// resolves to /private/var/.... Here the ledger reaches the binding root
// through a symlink. The restore matches the backup by location, and records
// the restore under the ledger's own spelling.
func TestRestorePurgeMatchesTheLedgerBackupByLocation(t *testing.T) {
	city := newSmallPurgeCity(t, 2)
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 2), &stdout, &stderr); code != 0 {
		t.Fatalf("purge exited %d: %s", code, stderr.String())
	}
	backup := purgeBackupFiles(t, city)[0]
	alias := filepath.Join(t.TempDir(), "binding-root")
	if err := os.Symlink(city.target.Root, alias); err != nil {
		t.Fatal(err)
	}
	spelled := filepath.Join(alias, filepath.Base(backup))
	ledger, err := readInfraPurgeLedger(city.target)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ledger {
		ledger[i].Backup = spelled
	}
	if err := appendInfraPurgeLedger(city.target, ledger[:len(ledger)-1], ledger[len(ledger)-1]); err != nil {
		t.Fatal(err)
	}

	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}
	stdout.Reset()
	stderr.Reset()
	if code := doStorageRestorePurge(context.Background(), request, backup, false, &stdout, &stderr); code != 0 {
		t.Fatalf("the restore of %s, which the ledger spells %s, exited %d: %s", backup, spelled, code, stderr.String())
	}
	after, err := readInfraPurgeLedger(city.target)
	if err != nil {
		t.Fatal(err)
	}
	if last := after[len(after)-1]; last.Event != infraPurgeEventRestored || last.Backup != spelled || last.Run != ledger[0].Run || len(last.Rows) != 2 {
		t.Errorf("the restore was not recorded under the ledger's spelling of its backup: %+v", last)
	}
	if _, out := runPurgeStatus(t, city); !strings.Contains(out, "purged stranded: 0 bead(s)") || !strings.Contains(out, "restored by --restore-purge: 2 row(s)") {
		t.Errorf("status after the restore:\n%s", out)
	}
}

// TestRestorePurgePutsAParentBackOnAStoreThatReadsItOffAnEdge: bd keeps no
// parent of its own. It reads a row's parent off the row's parent-child edge,
// and bd import drops it, so a row the purge stripped of its edges, or one the
// restore imported, reads back with no parent until the restore adds the edge
// again. The restore proves the rows without that parent before it writes an
// edge, and with it once the edges are back: after a full purge, and after a
// run interrupted once it removed its edges, which the restore then closes.
func TestRestorePurgePutsAParentBackOnAStoreThatReadsItOffAnEdge(t *testing.T) {
	for _, tc := range []struct {
		name string
		// crash is the seam the purge dies at; nil runs it to the end.
		crash *func() error
		// plan is what the restore says it puts back.
		plan string
	}{
		{"after a full purge", nil, "restore: 2 row(s), 0 already back, 2 edge(s)"},
		{"after a run interrupted once its edges were removed", &infraPurgeAfterEdgesRemoved, "restore: 0 row(s), 2 already back, 2 edge(s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := newSmallPurgeCity(t, 2)
			workID := purgeWorkBeadID(t, city)
			work := openPurgeWorkStore(t, city.workPath)
			if err := work.DepAdd("mc-wisp-0001", workID, "parent-child"); err != nil {
				t.Fatal(err)
			}
			if err := work.DepAdd("mc-wisp-0002", workID, "blocks"); err != nil {
				t.Fatal(err)
			}
			openPurgeWorkStoreAs(t, city, func(store *beads.FileStore) beads.Store { return purgeInlineEdgesStore{store} })
			var stdout, stderr bytes.Buffer
			if tc.crash != nil {
				finish := failPurgeAt(t, tc.crash, "simulated crash after the edges")
				if code := runStrandedPurge(t, city, confirmedPurge(t, city, 2), &stdout, &stderr); code == 0 {
					t.Fatalf("the crashed run exited 0")
				}
				finish()
			} else if code := runStrandedPurge(t, city, confirmedPurge(t, city, 2), &stdout, &stderr); code != 0 {
				t.Fatalf("purge exited %d: %s", code, stderr.String())
			}
			backup := purgeBackupFiles(t, city)[0]
			records, err := readInfraPurgeBackup(backup)
			if err != nil {
				t.Fatal(err)
			}

			request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}
			for _, dryRun := range []bool{true, false} {
				stdout.Reset()
				stderr.Reset()
				if code := doStorageRestorePurge(context.Background(), request, backup, dryRun, &stdout, &stderr); code != 0 {
					t.Fatalf("the restore (dry run %v) exited %d: %s", dryRun, code, stderr.String())
				}
				if !strings.Contains(stdout.String(), tc.plan) {
					t.Errorf("the restore (dry run %v) does not say %q:\n%s", dryRun, tc.plan, stdout.String())
				}
			}
			listed, err := purgeInlineEdgesStore{openPurgeWorkStore(t, city.workPath)}.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
			if err != nil {
				t.Fatal(err)
			}
			back := map[string]beads.Bead{}
			for _, b := range listed {
				back[b.ID] = b
			}
			for _, record := range records {
				want := record.Entry.Bead
				if diff := restoredRowDifference(want, back[want.ID]); diff != "" {
					t.Errorf("%s does not read back as backed up after the restore: %s", want.ID, diff)
				}
			}
			if parent := back["mc-wisp-0001"].ParentID; parent != workID {
				t.Errorf("mc-wisp-0001 came back with parent %q, want %q", parent, workID)
			}
			if code, out := runPurgeStatus(t, city); strings.Contains(out, "INTERRUPTED") || !strings.Contains(out, "stranded:    2") {
				t.Errorf("status after the restore exited %d, and should show the rows stranded and no interrupted purge:\n%s", code, out)
			}
		})
	}
}

// TestPurgeStrandedRemovesAndRestoresEveryEdgeTypeOnAPair: a store may keep a
// parent-child edge beside an edge of another type on the same pair, as
// FileStore does. The purge removes both before it deletes the row, so neither
// is left pointing at the deleted id; the backup keeps both, ordered by type;
// and --restore-purge puts both back.
func TestPurgeStrandedRemovesAndRestoresEveryEdgeTypeOnAPair(t *testing.T) {
	city := newSmallPurgeCity(t, 2)
	workID := purgeWorkBeadID(t, city)
	work := openPurgeWorkStore(t, city.workPath)
	for _, typ := range []string{"parent-child", "blocks"} {
		if err := work.DepAdd("mc-wisp-0001", workID, typ); err != nil {
			t.Fatal(err)
		}
	}
	edgeTypes := func(deps []beads.Dep) []string {
		var types []string
		for _, d := range deps {
			types = append(types, d.Type)
		}
		slices.Sort(types)
		return types
	}
	if deps, err := work.DepList("mc-wisp-0001", "down"); err != nil || !slices.Equal(edgeTypes(deps), []string{"blocks", "parent-child"}) {
		t.Fatalf("the work store does not keep both types on the pair, so the scenario under test was never created: %+v (err %v)", deps, err)
	}

	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 2), &stdout, &stderr); code != 0 {
		t.Fatalf("purge exited %d: %s", code, stderr.String())
	}
	if deps, err := openPurgeWorkStore(t, city.workPath).DepList("mc-wisp-0001", "down"); err != nil || len(deps) != 0 {
		t.Errorf("an edge of the purged row is left pointing at its deleted id: %+v (err %v)", deps, err)
	}
	backup := purgeBackupFiles(t, city)[0]
	records, err := readInfraPurgeBackup(backup)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Entry.Bead.ID != "mc-wisp-0001" {
			continue
		}
		if deps := record.Entry.Deps; len(deps) != 2 || deps[0].Type != "blocks" || deps[1].Type != "parent-child" {
			t.Errorf("the backup does not keep both types on the pair, ordered by type: %+v", deps)
		}
	}

	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}
	stdout.Reset()
	stderr.Reset()
	if code := doStorageRestorePurge(context.Background(), request, backup, false, &stdout, &stderr); code != 0 {
		t.Fatalf("restore exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "2 edge(s)") {
		t.Errorf("the restore does not count both edges:\n%s", stdout.String())
	}
	if deps, err := openPurgeWorkStore(t, city.workPath).DepList("mc-wisp-0001", "down"); err != nil || !slices.Equal(edgeTypes(deps), []string{"blocks", "parent-child"}) {
		t.Errorf("the restore did not put both types on the pair back: %+v (err %v)", deps, err)
	}
}

// TestPurgeStrandedWillNotCloseARunWhileABackedUpRowIsLeftBehind: a row the
// interrupted run backed up can leave the selection and stay in the work store,
// here because it was carried into the binding after the run removed its edges.
// recover-stranded refuses that copy now
// (TestRecoverStrandedRefusesWhileAPurgeIsInterrupted), so the copy stands in
// for an older build's recover-stranded or a hand import. The next run does not
// delete the row, so finishing the run, by resuming or with nothing left to
// select, would close the run with that row's edges gone and the backup that
// holds them listed nowhere. Both refuse and write nothing, their dry runs
// too. --restore-purge refuses as well: the binding serves the row, and the
// restore, which writes only the work store, would put its edge back on the
// work store's copy and report the rollback done. Rolled back by hand,
// --abandon-purge closes the run.
func TestPurgeStrandedWillNotCloseARunWhileABackedUpRowIsLeftBehind(t *testing.T) {
	for _, tc := range []struct {
		name string
		// gone are the backed-up rows the interrupted delete took.
		gone []string
		// selected is how many rows the next run selects.
		selected int
	}{
		{"resuming", nil, 2},
		{"with nothing left to select", []string{"mc-wisp-0002", "mc-wisp-0003"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := newSmallPurgeCity(t, 3)
			workID := purgeWorkBeadID(t, city)
			if err := openPurgeWorkStore(t, city.workPath).DepAdd("mc-wisp-0001", workID, "relates-to"); err != nil {
				t.Fatal(err)
			}
			finish := failPurgeAt(t, &infraPurgeAfterEdgesRemoved, "simulated crash after the edges")
			var stdout, stderr bytes.Buffer
			if code := runStrandedPurge(t, city, confirmedPurge(t, city, 3), &stdout, &stderr); code == 0 {
				t.Fatalf("the crashed run exited 0")
			}
			finish()
			backup := purgeBackupFiles(t, city)[0]
			work := openPurgeWorkStore(t, city.workPath)
			for _, id := range tc.gone {
				if err := work.Delete(id); err != nil {
					t.Fatal(err)
				}
			}
			binding, err := openInfraDestination(city.target)
			if err != nil {
				t.Fatal(err)
			}
			_, err = binding.(beads.ForeignIDCreator).CreateWithForeignID(mcAutoHandoffMail(t, 1))
			_ = closeBeadStoreHandle(binding)
			if err != nil {
				t.Fatalf("carrying mc-wisp-0001 into the binding: %v", err)
			}
			if got := strandedIDs(t, city.cityPath, city.target); slices.Contains(got, "mc-wisp-0001") || len(got) != tc.selected {
				t.Fatalf("stranded = %v, want %d rows and not mc-wisp-0001; the scenario under test was never created", got, tc.selected)
			}

			// The dry run refuses where the run would.
			for _, opts := range []strandedPurgeOptions{
				{Type: "message", DryRun: true, ExpectCount: -1},
				confirmedPurge(t, city, tc.selected),
			} {
				stdout.Reset()
				stderr.Reset()
				if code := runStrandedPurge(t, city, opts, &stdout, &stderr); code == 0 {
					t.Fatalf("the run (dry run %v) would close the interrupted run with a backed-up row left behind:\n%s", opts.DryRun, stdout.String())
				}
				for _, want := range []string{"mc-wisp-0001", "--" + strandedRestorePurgeFlag + " " + backup} {
					if !strings.Contains(stderr.String(), want) {
						t.Errorf("the refusal (dry run %v) lacks %q: %s", opts.DryRun, want, stderr.String())
					}
				}
			}
			if ledger, err := readInfraPurgeLedger(city.target); err != nil || len(ledger) != 1 {
				t.Errorf("the refused run wrote the ledger: %+v (err %v)", ledger, err)
			}
			if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != 3-len(tc.gone) {
				t.Errorf("the refused run deleted rows: %d message row(s) left, want %d", got, 3-len(tc.gone))
			}
			if code, out := runPurgeStatus(t, city); code == 0 || !strings.Contains(out, "purge: INTERRUPTED") {
				t.Errorf("the refused run closed the interrupted run: status exited %d:\n%s", code, out)
			}

			request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}
			for _, dryRun := range []bool{true, false} {
				stdout.Reset()
				stderr.Reset()
				if code := doStorageRestorePurge(context.Background(), request, backup, dryRun, &stdout, &stderr); code == 0 {
					t.Fatalf("the restore (dry run %v) exited 0 with a row of the backup in the binding:\n%s", dryRun, stdout.String())
				}
				for _, want := range []string{"the binding holds 1 row(s)", "mc-wisp-0001", "Roll back by hand"} {
					if !strings.Contains(stderr.String(), want) {
						t.Errorf("the restore refusal (dry run %v) lacks %q: %s", dryRun, want, stderr.String())
					}
				}
			}
			if deps, err := openPurgeWorkStore(t, city.workPath).DepList("mc-wisp-0001", "down"); err != nil || len(deps) != 0 {
				t.Errorf("the refused restore put the edge back on the work store's copy: %+v (err %v)", deps, err)
			}
			if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != 3-len(tc.gone) {
				t.Errorf("the refused restore wrote rows: %d message row(s), want %d", got, 3-len(tc.gone))
			}
			binding, err = openInfraDestination(city.target)
			if err != nil {
				t.Fatal(err)
			}
			held, getErr := binding.Get("mc-wisp-0001")
			deps, depErr := binding.DepList("mc-wisp-0001", "down")
			_ = closeBeadStoreHandle(binding)
			if getErr != nil || held.Title != mcPurgeTitle || depErr != nil || len(deps) != 0 {
				t.Errorf("the refused restore touched the binding's copy: %+v, edges %+v (errs %v, %v)", held, deps, getErr, depErr)
			}
			if code, out := runPurgeStatus(t, city); code == 0 || !strings.Contains(out, "purge: INTERRUPTED") {
				t.Errorf("the refused restore closed the run: status exited %d:\n%s", code, out)
			}

			// The rollback by hand (the raw bd fallback): the gone rows and the
			// edge back in the work store. --abandon-purge then closes the run.
			records, err := readInfraPurgeBackup(backup)
			if err != nil {
				t.Fatal(err)
			}
			var back []beads.Bead
			for _, record := range records {
				if slices.Contains(tc.gone, record.Entry.Bead.ID) {
					row := record.Entry.Bead
					row.Dependencies = nil
					back = append(back, row)
				}
			}
			editPurgeWorkStore(t, city.workPath, back)
			if err := openPurgeWorkStore(t, city.workPath).DepAdd("mc-wisp-0001", workID, "relates-to"); err != nil {
				t.Fatal(err)
			}
			stdout.Reset()
			stderr.Reset()
			if code := runStrandedPurge(t, city, strandedPurgeOptions{Type: "message", Abandon: true, ExpectCount: -1}, &stdout, &stderr); code != 0 {
				t.Fatalf("abandon after the rollback by hand exited %d: %s", code, stderr.String())
			}
			if _, out := runPurgeStatus(t, city); strings.Contains(out, "INTERRUPTED") {
				t.Errorf("abandon did not close the run:\n%s", out)
			}
		})
	}
}

// purgeNotFoundBatchStore is a FileStore that answers a batched edge read as
// BdStore answers one bd refuses as "not found" (one anchor bd cannot resolve
// is enough): with an empty map and no error, whatever edges the anchors
// carry. Its delete rewrites, as `bd delete --force` does, every mention of a
// deleted id in the description of each row still linked to it.
type purgeNotFoundBatchStore struct{ *beads.FileStore }

func (s purgeNotFoundBatchStore) DepListBatch([]string) (map[string][]beads.Dep, error) {
	return map[string][]beads.Dep{}, nil
}

func (s purgeNotFoundBatchStore) Delete(id string) error {
	var linked []string
	for _, direction := range []string{"up", "down"} {
		deps, err := s.DepList(id, direction)
		if err != nil {
			return err
		}
		for _, d := range deps {
			linked = append(linked, d.IssueID, d.DependsOnID)
		}
	}
	for _, other := range linked {
		if other == id {
			continue
		}
		row, err := s.Get(other)
		if err != nil {
			return err
		}
		text := strings.ReplaceAll(row.Description, id, "[deleted:"+id+"]")
		if err := s.Update(other, beads.UpdateOpts{Description: &text}); err != nil {
			return err
		}
	}
	return s.FileStore.Delete(id)
}

// TestPurgeStrandedKeepsAnInboundEdgeWhenABatchAnswersNothing: BdStore answers
// a batched edge read that bd refuses as "not found" with an empty map and no
// error, whatever edges the anchors carry, so an empty answer is not "no
// inbound edges". The purge asks each selected row for its dependents
// instead: the dry run counts the edge into the row, the backup keeps it, the
// edge goes before the row, and the neighbor that `bd delete --force` would
// rewrite keeps its text.
func TestPurgeStrandedKeepsAnInboundEdgeWhenABatchAnswersNothing(t *testing.T) {
	city := newSmallPurgeCity(t, 2)
	workID := purgeWorkBeadID(t, city)
	neighborText := "the handoff in mc-wisp-0001 explains this"
	work := openPurgeWorkStore(t, city.workPath)
	if err := work.Update(workID, beads.UpdateOpts{Description: &neighborText}); err != nil {
		t.Fatal(err)
	}
	if err := work.DepAdd(workID, "mc-wisp-0001", "blocks"); err != nil {
		t.Fatal(err)
	}
	openPurgeWorkStoreAs(t, city, func(store *beads.FileStore) beads.Store { return purgeNotFoundBatchStore{store} })

	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, strandedPurgeOptions{Type: "message", DryRun: true, ExpectCount: -1}, &stdout, &stderr); code != 0 {
		t.Fatalf("the dry run exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "edges:   0 outbound, 1 inbound") {
		t.Errorf("the dry run does not count the edge into mc-wisp-0001:\n%s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 2), &stdout, &stderr); code != 0 {
		t.Fatalf("purge exited %d: %s", code, stderr.String())
	}
	records, err := readInfraPurgeBackup(purgeBackupFiles(t, city)[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Entry.Bead.ID != "mc-wisp-0001" {
			continue
		}
		if edges := record.Entry.Dependents; len(edges) != 1 || edges[0].IssueID != workID || edges[0].Type != "blocks" {
			t.Errorf("the backup of mc-wisp-0001 does not keep the edge from %s: %+v", workID, edges)
		}
	}
	store := openPurgeWorkStore(t, city.workPath)
	if neighbor, err := store.Get(workID); err != nil || neighbor.Description != neighborText {
		t.Errorf("the purge rewrote the neighbor's text: %q, want %q (err %v)", neighbor.Description, neighborText, err)
	}
	if deps, err := store.DepList(workID, "down"); err != nil || len(deps) != 0 {
		t.Errorf("the neighbor keeps an edge into the purged row: %+v (err %v)", deps, err)
	}
}

// purgePayloadStore is a FileStore that keeps edge payloads, as SQLite does:
// DepAddWithMetadata adds an edge carrying one, DepMetadata reads it back, and
// DepAdd and DepRemove leave the pair carrying none. The payloads outlive one
// open of the store, as its rows do.
type purgePayloadStore struct {
	*beads.FileStore
	payloads map[[2]string]string
}

func (s purgePayloadStore) DepAdd(issueID, dependsOnID, depType string) error {
	if err := s.FileStore.DepAdd(issueID, dependsOnID, depType); err != nil {
		return err
	}
	delete(s.payloads, [2]string{issueID, dependsOnID})
	return nil
}

func (s purgePayloadStore) DepAddWithMetadata(issueID, dependsOnID, depType, metadata string) error {
	if err := s.DepAdd(issueID, dependsOnID, depType); err != nil {
		return err
	}
	if metadata != "" {
		s.payloads[[2]string{issueID, dependsOnID}] = metadata
	}
	return nil
}

func (s purgePayloadStore) DepRemove(issueID, dependsOnID string) error {
	if err := s.FileStore.DepRemove(issueID, dependsOnID); err != nil {
		return err
	}
	delete(s.payloads, [2]string{issueID, dependsOnID})
	return nil
}

func (s purgePayloadStore) DepMetadata(issueID, dependsOnID string) (string, bool, error) { //nolint:unparam // the error is the beads.DepMetadataReader contract; a map read cannot fail
	payload, carried := s.payloads[[2]string{issueID, dependsOnID}]
	return payload, carried, nil
}

// TestPurgeEdgeIsBackOnlyWithItsPayload: on a store that keeps edge payloads,
// as SQLite and native Dolt do, an edge back without the payload its backup
// carries is not the edge the purge removed: a waits-for gate means something
// else without it. After a run interrupted once it removed its edges, and the
// edge put back by hand without its payload, --abandon-purge refuses and
// names the payload, and --restore-purge writes the edge again with it rather
// than skipping it as already back.
func TestPurgeEdgeIsBackOnlyWithItsPayload(t *testing.T) {
	city := newSmallPurgeCity(t, 2)
	workID := purgeWorkBeadID(t, city)
	payloads := map[[2]string]string{}
	openPurgeWorkStoreAs(t, city, func(store *beads.FileStore) beads.Store { return purgePayloadStore{store, payloads} })
	const gate = `{"gate":"waits_for","threshold":1}`
	edge := infraBackupEdge{IssueID: workID, DependsOnID: "mc-wisp-0001", Type: "waits-for", Payload: gate, Carried: true}
	if err := (purgePayloadStore{openPurgeWorkStore(t, city.workPath), payloads}).DepAddWithMetadata(edge.IssueID, edge.DependsOnID, edge.Type, gate); err != nil {
		t.Fatal(err)
	}
	finish := failPurgeAt(t, &infraPurgeAfterEdgesRemoved, "simulated crash after the edges")
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 2), &stdout, &stderr); code == 0 {
		t.Fatalf("the crashed run exited 0")
	}
	finish()
	backup := purgeBackupFiles(t, city)[0]
	records, err := readInfraPurgeBackup(backup)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Entry.Bead.ID == "mc-wisp-0001" && !slices.Equal(record.Entry.Dependents, []infraBackupEdge{edge}) {
			t.Fatalf("the backup does not keep the edge with its payload, so the scenario under test was never created: %+v", record.Entry.Dependents)
		}
	}

	// The raw bd fallback puts the edge back by hand, without its payload.
	work := purgePayloadStore{openPurgeWorkStore(t, city.workPath), payloads}
	if err := work.DepAdd(edge.IssueID, edge.DependsOnID, edge.Type); err != nil {
		t.Fatal(err)
	}
	abandon := strandedPurgeOptions{Type: "message", Abandon: true, ExpectCount: -1}
	for _, dryRun := range []bool{true, false} {
		opts := abandon
		opts.DryRun = dryRun
		stdout.Reset()
		stderr.Reset()
		if code := runStrandedPurge(t, city, opts, &stdout, &stderr); code == 0 {
			t.Fatalf("abandon (dry run %v) with the edge back without its payload exited 0: %s", dryRun, stdout.String())
		}
		if want := edge.String() + " (carries no payload"; !strings.Contains(stderr.String(), want) {
			t.Errorf("the refusal (dry run %v) lacks %q: %s", dryRun, want, stderr.String())
		}
	}

	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}
	stdout.Reset()
	stderr.Reset()
	if code := doStorageRestorePurge(context.Background(), request, backup, false, &stdout, &stderr); code != 0 {
		t.Fatalf("the restore exited %d: %s", code, stderr.String())
	}
	if payload, carried, _ := work.DepMetadata(edge.IssueID, edge.DependsOnID); !carried || payload != gate {
		t.Errorf("the restore left the edge carrying %q (carried %v), want %q", payload, carried, gate)
	}
	if _, out := runPurgeStatus(t, city); strings.Contains(out, "INTERRUPTED") {
		t.Errorf("the restore did not close the run:\n%s", out)
	}
}

// purgeEdgeTouchesRowStore is a FileStore whose DepAdd also touches the row the
// edge starts at, moving its updated_at, as a store that stamps a row on every
// edge written to it would.
type purgeEdgeTouchesRowStore struct{ *beads.FileStore }

func (s purgeEdgeTouchesRowStore) DepAdd(issueID, dependsOnID, depType string) error {
	if err := s.FileStore.DepAdd(issueID, dependsOnID, depType); err != nil {
		return err
	}
	return s.Update(issueID, beads.UpdateOpts{})
}

// TestRestorePurgeRefusesARowThatChangesOnceItsEdgesAreBack: the restore proves
// the rows again, whole, once their edges are back, because adding an edge can
// change a row: bd reads a parent off a parent-child edge, and a store that
// stamps a row on every edge write moves its updated_at. A row that does not
// read back as backed up then fails the restore, which names it and records
// nothing.
func TestRestorePurgeRefusesARowThatChangesOnceItsEdgesAreBack(t *testing.T) {
	city := newSmallPurgeCity(t, 2)
	workID := purgeWorkBeadID(t, city)
	if err := openPurgeWorkStore(t, city.workPath).DepAdd("mc-wisp-0001", workID, "blocks"); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 2), &stdout, &stderr); code != 0 {
		t.Fatalf("purge exited %d: %s", code, stderr.String())
	}
	backup := purgeBackupFiles(t, city)[0]
	before, err := readInfraPurgeLedger(city.target)
	if err != nil {
		t.Fatal(err)
	}

	openPurgeWorkStoreAs(t, city, func(store *beads.FileStore) beads.Store { return purgeEdgeTouchesRowStore{store} })
	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}
	stdout.Reset()
	stderr.Reset()
	if code := doStorageRestorePurge(context.Background(), request, backup, false, &stdout, &stderr); code == 0 {
		t.Fatalf("a restore whose row changed once its edge was back exited 0:\n%s", stdout.String())
	}
	for _, want := range []string{"once their edges are back", "mc-wisp-0001 (updated_at"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the refusal lacks %q: %s", want, stderr.String())
		}
	}
	if after, err := readInfraPurgeLedger(city.target); err != nil || len(after) != len(before) {
		t.Errorf("the failed restore recorded itself in the ledger: %+v (err %v)", after, err)
	}
}

// TestRecoverStrandedRefusesWhileAPurgeIsInterrupted: a purge run interrupted
// after it removed its rows' edges leaves the rows stranded and their edges in
// its backup alone. A copy into the binding then would carry the rows without
// those edges, and --restore-purge writes only the work store, so
// recover-stranded copies nothing, its dry run included, and names the
// purge's remedies, which are all status names. Once the run is restored, the
// copy carries the rows with their edge.
func TestRecoverStrandedRefusesWhileAPurgeIsInterrupted(t *testing.T) {
	city := newSmallPurgeCity(t, 2)
	if err := openPurgeWorkStore(t, city.workPath).DepAdd("mc-wisp-0001", "mc-wisp-0002", "relates-to"); err != nil {
		t.Fatal(err)
	}
	finish := failPurgeAt(t, &infraPurgeAfterEdgesRemoved, "simulated crash after the edges")
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 2), &stdout, &stderr); code == 0 {
		t.Fatalf("the crashed run exited 0")
	}
	finish()
	if deps, err := openPurgeWorkStore(t, city.workPath).DepList("mc-wisp-0001", "down"); err != nil || len(deps) != 0 {
		t.Fatalf("the crash point is after the edge removal, yet the edge is still there: %+v (err %v)", deps, err)
	}
	backup := purgeBackupFiles(t, city)[0]
	manifest, err := os.ReadFile(city.target.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}

	for _, dryRun := range []bool{true, false} {
		code, out, errOut := runStrandedRecoveryWith(t, city.cityPath, city.cfg, strandedRecoveryOptions{DryRun: dryRun})
		if code == 0 {
			t.Fatalf("recover-stranded (dry run %v) exited 0 while a purge run is interrupted:\n%s", dryRun, out)
		}
		for _, want := range []string{"is interrupted", "--" + strandedPurgeTypeFlag + " message --dry-run", "--" + strandedRestorePurgeFlag + " " + backup, "--" + strandedAbandonPurgeFlag} {
			if !strings.Contains(errOut, want) {
				t.Errorf("the refusal (dry run %v) lacks %q: %s", dryRun, want, errOut)
			}
		}
	}
	binding, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"mc-wisp-0001", "mc-wisp-0002"} {
		if _, err := binding.Get(id); !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("the refused recover-stranded wrote %s into the binding (err %v)", id, err)
		}
	}
	_ = closeBeadStoreHandle(binding)
	if after, err := os.ReadFile(city.target.ManifestPath()); err != nil || !bytes.Equal(after, manifest) {
		t.Errorf("the refused recover-stranded changed the manifest (err %v)", err)
	}

	code, out := runPurgeStatus(t, city)
	if code == 0 || !strings.Contains(out, "purge: INTERRUPTED") || !strings.Contains(out, "stranded ids: mc-wisp-0001, mc-wisp-0002") {
		t.Fatalf("status during the interrupted run exited %d:\n%s", code, out)
	}
	if n := strings.Count(out, "blocking invariant:"); n != 1 || strings.Contains(out, "copy them into the binding") {
		t.Errorf("status names %d blocking invariant(s), and should name only the purge's:\n%s", n, out)
	}

	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}
	stdout.Reset()
	stderr.Reset()
	if code := doStorageRestorePurge(context.Background(), request, backup, false, &stdout, &stderr); code != 0 {
		t.Fatalf("the restore exited %d: %s", code, stderr.String())
	}
	if code, _, errOut := runStrandedRecoveryWith(t, city.cityPath, city.cfg, strandedRecoveryOptions{}); code != 0 {
		t.Fatalf("recover-stranded after the restore exited %d: %s", code, errOut)
	}
	binding, err = openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // test
	if deps, err := binding.DepList("mc-wisp-0001", "down"); err != nil || len(deps) != 1 || deps[0].DependsOnID != "mc-wisp-0002" {
		t.Errorf("the binding's copy of mc-wisp-0001 does not carry its edge: %+v (err %v)", deps, err)
	}
}

// TestRecoverStrandedRefusesOverAnUnreadablePurgeLedger: a purge ledger that
// cannot be read cannot say whether a purge run is interrupted, so
// recover-stranded copies nothing, and status names the ledger's remedy and
// not a copy.
func TestRecoverStrandedRefusesOverAnUnreadablePurgeLedger(t *testing.T) {
	city := newSmallPurgeCity(t, 1)
	if err := os.WriteFile(city.target.PurgeLedgerPath(), []byte("{not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runStrandedRecoveryWith(t, city.cityPath, city.cfg, strandedRecoveryOptions{})
	if code == 0 || !strings.Contains(errOut, "reading the purge ledger") || !strings.Contains(errOut, "Nothing was written") {
		t.Fatalf("recover-stranded over an unreadable purge ledger exited %d:\n%s%s", code, out, errOut)
	}
	binding, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // test
	if _, err := binding.Get("mc-wisp-0001"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("the refused recover-stranded wrote mc-wisp-0001 into the binding (err %v)", err)
	}
	code, status := runPurgeStatus(t, city)
	if code == 0 || !strings.Contains(status, "purge ledger: UNREADABLE") || strings.Count(status, "blocking invariant:") != 1 {
		t.Errorf("status over an unreadable ledger and a stranded row exited %d, and should name only the ledger's remedy:\n%s", code, status)
	}
}

// purgeNoRelationsStore is a FileStore behind a backend that refuses relation
// listing, as bd's Postgres backend refuses IssueRelations: DepList refuses
// the capability, DepListBatch is unsupported, and List carries each row's
// outbound edges inline, the projection recover-stranded reads around the
// refusal with.
type purgeNoRelationsStore struct{ *beads.FileStore }

func (s purgeNoRelationsStore) DepList(string, string) ([]beads.Dep, error) {
	return nil, errors.New(`operation "IssueRelations" not supported by the postgres backend`)
}

func (s purgeNoRelationsStore) DepListBatch([]string) (map[string][]beads.Dep, error) {
	return nil, beads.ErrDepListBatchUnsupported
}

func (s purgeNoRelationsStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.FileStore.List(query)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		deps, err := s.FileStore.DepList(rows[i].ID, "down")
		if err != nil {
			return nil, err
		}
		rows[i].Dependencies = deps
	}
	return rows, nil
}

// TestPurgeRefusesAWorkStoreThatRefusesRelationListing: the restore and
// --abandon-purge read every edge back through the work store's relation
// listing. On a store that refuses it, the purge refuses, its dry run too, and
// writes nothing, though the listing carries the edges inline; a restore there
// refuses before it imports a row, rather than import the rows and fail on the
// first edge read.
func TestPurgeRefusesAWorkStoreThatRefusesRelationListing(t *testing.T) {
	city := newSmallPurgeCity(t, 2)
	workID := purgeWorkBeadID(t, city)
	if err := openPurgeWorkStore(t, city.workPath).DepAdd(workID, "mc-wisp-0001", "blocks"); err != nil {
		t.Fatal(err)
	}
	openPurgeWorkStoreAs(t, city, func(store *beads.FileStore) beads.Store { return purgeNoRelationsStore{store} })
	var stdout, stderr bytes.Buffer
	for _, opts := range []strandedPurgeOptions{
		{Type: "message", DryRun: true, ExpectCount: -1},
		confirmedPurge(t, city, 2),
	} {
		stdout.Reset()
		stderr.Reset()
		if code := runStrandedPurge(t, city, opts, &stdout, &stderr); code == 0 {
			t.Fatalf("the purge (dry run %v) on a store that refuses relation listing exited 0:\n%s", opts.DryRun, stdout.String())
		}
		if !strings.Contains(stderr.String(), "refuses relation listing") || !strings.Contains(stderr.String(), "Nothing was written") {
			t.Errorf("the refusal (dry run %v) does not name the relation listing: %s", opts.DryRun, stderr.String())
		}
	}
	if files := purgeBackupFiles(t, city); len(files) != 0 {
		t.Errorf("the refused purge wrote a backup: %v", files)
	}
	if _, err := os.Stat(city.target.PurgeLedgerPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused purge wrote the ledger (stat err %v)", err)
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != 2 {
		t.Fatalf("the refused purge deleted rows: %d left", got)
	}

	// A backup taken where relations list, restored where they do not.
	openPurgeWorkStoreAs(t, city, func(store *beads.FileStore) beads.Store { return store })
	stdout.Reset()
	stderr.Reset()
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, 2), &stdout, &stderr); code != 0 {
		t.Fatalf("purge exited %d: %s", code, stderr.String())
	}
	backup := purgeBackupFiles(t, city)[0]
	openPurgeWorkStoreAs(t, city, func(store *beads.FileStore) beads.Store { return purgeNoRelationsStore{store} })
	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: city.cfg, FleetStopped: true}
	for _, dryRun := range []bool{true, false} {
		stdout.Reset()
		stderr.Reset()
		if code := doStorageRestorePurge(context.Background(), request, backup, dryRun, &stdout, &stderr); code == 0 {
			t.Fatalf("a restore (dry run %v) on a store that refuses relation listing exited 0:\n%s", dryRun, stdout.String())
		}
		if !strings.Contains(stderr.String(), "refuses relation listing") || !strings.Contains(stderr.String(), "raw bd") {
			t.Errorf("the refusal (dry run %v) does not name the relation listing and the raw bd fallback: %s", dryRun, stderr.String())
		}
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != 0 {
		t.Errorf("the refused restore imported %d row(s)", got)
	}
	if code, out := runPurgeStatus(t, city); code != 0 || !strings.Contains(out, "purged stranded: 2 bead(s)") {
		t.Errorf("the refused restore recorded itself: status exited %d:\n%s", code, out)
	}
}
