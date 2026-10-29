package main

// The stranded-write purge: `gc storage recover-stranded --from-work
// --purge-type message`. It backs up, then deletes, the stranded work-store
// rows of one type, where recover-stranded's default copies them into the
// binding. `--restore-purge <backup>` (infra_class_purge_restore.go) puts
// them back.
//
// # Why a purge exists
//
// A stranded row is one the boot refuses over: the work store holds it, the
// converged binding does not, and the proven-copy manifest does not record it
// (confirmInfraConvergence). recover-stranded's answer is to carry it in. For
// mail that is the wrong answer when the mail is dead: maintainer-city's 791
// stranded rows are auto-handoff mails ("context cycle") from August, open
// only because nothing ever read them, and carrying them in would deliver a
// month of stale handoffs to live agents. The owner's decision is to back
// them up and purge them.
//
// # What it selects, and what it refuses
//
// Exactly the rows the containment check reports as stranded
// (classifyInfraContainmentGap, the function the boot gate and `gc storage
// status` call), whose type is the purge type. It never selects by manifest
// id: bd reuses short wisp ids, so an id names a bead only together with its
// created_at, and the check already judges every row that way.
//
// Every selected row must also classify (coordclass, under the manifest's
// generation) as the purge type's class. A type=message row the classifier
// puts elsewhere — a wisp step typed message is graph — is not mail, and the
// whole run refuses rather than purging it. Stranded rows of other types are
// left exactly where they are and named.
//
// A work-store row the binding holds as the same bead (the same id, created
// in the same second) is never deleted. Such a row is not stranded, so it is
// never selected; and refuseInfraPurgeOfBindingRows re-reads the binding for
// every selected row before anything is written.
//
// # Tied to the reviewed list
//
// The dry run prints the selection's count and digest (infraPurgeDigest: the
// sorted (id, created_at) pairs). The delete needs --confirm-purge and
// --expect-count, and --expect-digest when given, and refuses when the
// selection it computes is not the one the operator reviewed.
//
// # Back up, prove, then delete
//
// Every selected row is written, with its metadata, labels and every edge
// touching it, to a timestamped JSONL file at the binding root (under
// .gc/store), in recover-stranded's choice-backup shape
// (infraRecoverChoiceRecord). The file is synced, renamed into place, its
// directory synced, and re-read and compared record for record before
// anything is deleted. The run is recorded in a ledger beside it
// (infra.purged-stranded.jsonl): "backed-up" before the delete, "purged"
// after it.
//
// The delete removes every backed-up edge first and only then the rows. The
// order matters on bd: `bd delete --force` rewrites every mention of a
// deleted id in the text of the rows still linked to it to "[deleted:<id>]",
// and with the edges gone no surviving row is linked.
//
// # Interrupted runs
//
// A run that dies after the backup leaves the ledger's last record at
// "backed-up", and status refuses ("purge: INTERRUPTED"). The next run:
//
//   - records as purged every row of that run the work store no longer holds,
//     so a run that died after the delete and before the record converges;
//   - resumes from the same backup when the remaining selection is a subset of
//     it, exactly as backed up;
//   - refuses when the selection has changed, rather than writing a second
//     backup with split accounting.
//
// --abandon-purge closes an interrupted run whose rows were all put back by
// hand (the raw bd fallback); --restore-purge closes it itself.
//
// # Status
//
// The purged rows leave the work store, so the containment check stops
// naming them and `gc storage status` reports converged once nothing else is
// stranded. The manifest is NOT extended: its meaning is "the copy was proven
// to deliver this bead", and a purged row was never delivered. Status reads
// the ledger instead (reportInfraPurgeLedger).
//
// # Exclusion
//
// The same as recover-stranded's: the controller socket must be silent, the
// operator attests --fleet-stopped, and the run holds the city's migration
// guard. A dry run is read-only and takes none of them, so it can rehearse
// against a live city.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// The purge's flags on recover-stranded.
const (
	strandedPurgeTypeFlag    = "purge-type"
	strandedConfirmPurgeFlag = "confirm-purge"
	strandedExpectCountFlag  = "expect-count"
	strandedExpectDigestFlag = "expect-digest"
	strandedAbandonPurgeFlag = "abandon-purge"
	strandedRestorePurgeFlag = "restore-purge"
)

// infraPurgeTypes maps each purgeable bead type to the class its rows must
// classify as. Mail is the one class the owner decided to purge rather than
// recover; a type is added here only by the same decision.
var infraPurgeTypes = map[string]coordclass.Class{
	"message": coordclass.ClassMessaging,
}

// infraPurgeChoice is the Choice a purge backup record carries.
const infraPurgeChoice = "purge-type"

// The ledger and the backups live at the binding root, beside the manifest.
const (
	infraPurgeLedgerName            = "infra.purged-stranded.jsonl"
	infraPurgedStrandedBackupPrefix = "infra.purged-stranded-"
	infraPurgeLedgerFormat          = "gc.infra.purged-stranded/v1"
)

// The ledger's events. One purge run writes backed-up then purged; abandoned
// closes an interrupted run whose rows were put back by hand; restored records
// rows --restore-purge put back.
const (
	infraPurgeEventBackedUp  = "backed-up"
	infraPurgeEventPurged    = "purged"
	infraPurgeEventAbandoned = "abandoned"
	infraPurgeEventRestored  = "restored"
)

// PurgeLedgerPath is where recover-stranded --purge-type records its runs.
func (t infraBindingTarget) PurgeLedgerPath() string {
	return filepath.Join(t.Root, infraPurgeLedgerName)
}

// strandedPurgeOptions are the purge's knobs.
type strandedPurgeOptions struct {
	// Type is the bead type to purge (infraPurgeTypes).
	Type string
	// DryRun lists the selection and writes nothing.
	DryRun bool
	// Confirm is the operator's explicit go-ahead to delete.
	Confirm bool
	// ExpectCount is the selection size the operator reviewed; negative means
	// not given. The delete refuses without it.
	ExpectCount int
	// ExpectDigest, when set, is the selection digest the operator reviewed.
	ExpectDigest string
	// Abandon closes an interrupted run whose rows were all put back.
	Abandon bool
}

// infraPurgeRowKey is one purged row's identity: an id names a bead only with
// its created_at, because bd reuses short wisp ids.
type infraPurgeRowKey struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

func (k infraPurgeRowKey) String() string {
	return k.ID + "@" + k.CreatedAt.UTC().Format(time.RFC3339)
}

// infraPurgeLedgerRecord is one ledger line.
type infraPurgeLedgerRecord struct {
	Format     string             `json:"format"`
	Event      string             `json:"event"`
	Run        string             `json:"run"`
	Type       string             `json:"type"`
	Backup     string             `json:"backup"`
	Rows       []infraPurgeRowKey `json:"rows"`
	RecordedAt time.Time          `json:"recorded_at"`
}

// Seams: a test makes one fail to stand in for a crash at that point, or
// changes the binding just before the guard reads it.
var (
	infraPurgeAfterBackup         = func() error { return nil }
	infraPurgeBeforeBindingGuard  = func() error { return nil }
	infraPurgeAfterDeleteRecorded = func() error { return nil }
)

// validateStrandedPurgeFlags refuses purge flags that do not belong together:
// the purge selects by class, so it does not combine with the per-id answers,
// and its confirmations mean nothing without it.
func validateStrandedPurgeFlags(opts strandedPurgeOptions, restorePath string, perID map[string]bool) error {
	if opts.Type != "" && restorePath != "" {
		return fmt.Errorf("--%s and --%s are separate runs", strandedPurgeTypeFlag, strandedRestorePurgeFlag)
	}
	if opts.Type == "" {
		var stray []string
		if opts.Confirm {
			stray = append(stray, "--"+strandedConfirmPurgeFlag)
		}
		if opts.ExpectCount >= 0 {
			stray = append(stray, "--"+strandedExpectCountFlag)
		}
		if opts.ExpectDigest != "" {
			stray = append(stray, "--"+strandedExpectDigestFlag)
		}
		if opts.Abandon {
			stray = append(stray, "--"+strandedAbandonPurgeFlag)
		}
		if len(stray) > 0 {
			return fmt.Errorf("%s belong to a --%s run; pass --%s message", strings.Join(stray, ", "), strandedPurgeTypeFlag, strandedPurgeTypeFlag)
		}
		if restorePath == "" {
			return nil
		}
	}
	var named []string
	for flag, set := range perID {
		if set {
			named = append(named, "--"+flag)
		}
	}
	if len(named) > 0 {
		sort.Strings(named)
		flag := strandedPurgeTypeFlag
		if restorePath != "" {
			flag = strandedRestorePurgeFlag
		}
		return fmt.Errorf("--%s does not combine with %s; run them separately", flag, strings.Join(named, ", "))
	}
	return nil
}

// infraPurgeSession is what both the purge and the restore hold: the
// resolved binding, its manifest, the open work store, and (for a writing
// run) the migration guard.
type infraPurgeSession struct {
	target   infraBindingTarget
	manifest infraCopyManifest
	source   beads.Store
	release  func()
}

func (s *infraPurgeSession) close() {
	_ = closeBeadStoreHandle(s.source)
	if s.release != nil {
		s.release()
	}
}

// openInfraPurgeSession runs the preflights recover-stranded runs, and for a
// writing run takes its exclusion: a silent controller socket, the
// --fleet-stopped attestation, and the migration guard.
func openInfraPurgeSession(ctx context.Context, request storageOperatorRequest, write bool, logPrefix string, stdout, stderr io.Writer) (*infraPurgeSession, bool) {
	target, ok, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return nil, false
	}
	if !ok {
		reportUnresolvedInfraBinding(request, logPrefix, stdout, stderr)
		return nil, false
	}
	if _, err := resolveCityStoragePlan(request.CityPath, request.Cfg); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return nil, false
	}
	state, err := readInfraConvergenceState(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return nil, false
	}
	if state != infraConvergenceMarked {
		fmt.Fprintf(stderr, "%s: this city has not converged onto binding %q, so nothing here is a stranded write. Run:  %s\n", //nolint:errcheck // best-effort stderr
			logPrefix, target.Binding, storageMigrationCommand)
		return nil, false
	}
	if blocked, held := servedBindingNoteHold(request.CityPath, target.Binding, config.StorageProviderSQLiteBeads, target.Database); held {
		blocked.Target = target
		fmt.Fprintln(stderr, infraMigrationOperatorAdvice(blocked, logPrefix)) //nolint:errcheck // best-effort stderr
		return nil, false
	}
	session := &infraPurgeSession{target: target}
	if write {
		if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
			fmt.Fprintf(stderr, "%s: controller PID %d is live on this city and is still writing the work store. Stop it (gc stop) and run this again\n", logPrefix, pid) //nolint:errcheck // best-effort stderr
			return nil, false
		}
		if !request.FleetStopped {
			fmt.Fprintf(stderr, "%s: pass --%s to attest that %s. This run writes the work store, and a writer still running could write the rows it is judging\n", //nolint:errcheck // best-effort stderr
				logPrefix, storageFleetStoppedFlag, storageFleetStoppedAttestation)
			return nil, false
		}
		guard, err := storebinding.AcquireMigrationGuard(ctx, cityMigrationGuardDirectory(request.CityPath), storageMigrationGeneration)
		if err != nil {
			if errors.Is(err, storebinding.ErrMigrationGuardBusy) {
				fmt.Fprintf(stderr, "%s: another storage migration or repair holds this city. Wait for it to finish, or resolve it, and run this again\n", logPrefix) //nolint:errcheck // best-effort stderr
				return nil, false
			}
			fmt.Fprintf(stderr, "%s: taking the migration guard: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return nil, false
		}
		session.release = func() {
			if releaseErr := guard.Release(); releaseErr != nil {
				fmt.Fprintf(stderr, "%s: releasing the migration guard: %v\n", logPrefix, releaseErr) //nolint:errcheck // best-effort stderr
			}
		}
	}
	manifest, recorded, err := readInfraCopyManifestRecord(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		session.close()
		return nil, false
	}
	if !recorded {
		fmt.Fprintf(stderr, "%s: %s converged before %s was recorded, so nothing tells a stranded row from one the binding's own GC collected. Re-converge the binding to restore the check\n", //nolint:errcheck // best-effort stderr
			logPrefix, target.Database, target.ManifestPath())
		session.close()
		return nil, false
	}
	session.manifest = manifest
	source, err := openInfraMigrationSource(request.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: opening the work store: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		if session.release != nil {
			session.release()
		}
		return nil, false
	}
	session.source = source
	return session, true
}

// doStoragePurgeStranded is `gc storage recover-stranded --from-work
// --purge-type <type>`.
func doStoragePurgeStranded(ctx context.Context, request storageOperatorRequest, opts strandedPurgeOptions, stdout, stderr io.Writer) int {
	logPrefix := storageRecoveryLogPrefix()
	class, supported := infraPurgeTypes[opts.Type]
	if !supported {
		fmt.Fprintf(stderr, "%s: --%s %q is not supported; the only purgeable type is message (mail). Recover other stranded rows with `%s`\n", //nolint:errcheck // best-effort stderr
			logPrefix, strandedPurgeTypeFlag, opts.Type, storageRecoveryCommand)
		return 1
	}
	session, ok := openInfraPurgeSession(ctx, request, !opts.DryRun, logPrefix, stdout, stderr)
	if !ok {
		return 1
	}
	defer session.close()
	target, manifest, source := session.target, session.manifest, session.source

	ledger, err := readInfraPurgeLedger(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	pending, hasPending := infraPurgePendingRun(ledger)
	var pendingGone []infraPurgeRowKey
	if hasPending {
		pendingGone, err = infraPurgeKeysGone(source, pending.Rows)
		if err != nil {
			fmt.Fprintf(stderr, "%s: reading the work store: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	if opts.Abandon {
		return abandonInfraPurge(target, ledger, pending, hasPending, pendingGone, opts.DryRun, logPrefix, stdout, stderr)
	}

	// The selection, by the containment check itself.
	gap, err := classifyInfraContainmentGap(request.CityPath, target, manifest)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	all, err := source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		fmt.Fprintf(stderr, "%s: listing work store: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	byID := make(map[string]beads.Bead, len(all))
	for _, b := range all {
		byID[b.ID] = b
	}
	var selected, others []beads.Bead
	var wrongClass, vanished []string
	for _, id := range gap.Stranded {
		b, present := byID[id]
		if !present {
			vanished = append(vanished, id)
			continue
		}
		if b.Type != opts.Type {
			others = append(others, b)
			continue
		}
		if got := coordclass.ClassifyAt(b, manifest.Generation); got != class {
			wrongClass = append(wrongClass, fmt.Sprintf("%s (classifies as %s, not %s)", b.ID, got, class))
			continue
		}
		selected = append(selected, b)
	}
	if len(vanished) > 0 {
		fmt.Fprintf(stderr, "%s: the work store changed while it was read (%s were stranded a moment ago and are gone); a purge judged against a moving store proves nothing. Stop every writer and run this again. Nothing was written\n", //nolint:errcheck // best-effort stderr
			logPrefix, strings.Join(vanished, ", "))
		return 1
	}
	if len(wrongClass) > 0 {
		fmt.Fprintf(stderr, "%s: %d stranded row(s) of type %s are not %s-class, and this purge only deletes %s: %s. Nothing was written. Recover them with `%s`\n", //nolint:errcheck // best-effort stderr
			logPrefix, len(wrongClass), opts.Type, class, class, strings.Join(wrongClass, "; "), storageRecoveryCommand)
		return 1
	}
	nonStranded := 0
	stranded := setOf(gap.Stranded)
	for _, b := range all {
		if b.Type == opts.Type && !stranded[b.ID] {
			nonStranded++
		}
	}

	fmt.Fprintf(stdout, "city:     %s\nbinding:  %s\nwork-store rows: %d\nstranded: %d\n", request.CityPath, target.Database, len(all), len(gap.Stranded)) //nolint:errcheck // best-effort stdout
	if nonStranded > 0 {
		fmt.Fprintf(stdout, "left in place: %d %s row(s) the containment check does not call stranded (the binding holds them, or the manifest records them); they are never purged\n", nonStranded, opts.Type) //nolint:errcheck // best-effort stdout
	}
	if len(others) > 0 {
		fmt.Fprintf(stdout, "not purged: %d stranded row(s) of other types stay where they are, and the boot keeps refusing over them until they are recovered with `%s`: %s\n", //nolint:errcheck // best-effort stdout
			len(others), storageRecoveryCommand, beadIDList(others))
	}
	if hasPending {
		fmt.Fprintf(stdout, "interrupted purge: run %s backed up %d row(s) to %s; %d of them are no longer in the work store\n", pending.Run, len(pending.Rows), pending.Backup, len(pendingGone)) //nolint:errcheck // best-effort stdout
	}

	if len(selected) == 0 {
		if hasPending && !opts.DryRun {
			return settleInfraPurgeWithNothingSelected(target, ledger, pending, pendingGone, logPrefix, stdout, stderr)
		}
		fmt.Fprintf(stdout, "nothing to purge: no stranded row of type %s\n", opts.Type) //nolint:errcheck // best-effort stdout
		return 0
	}
	digest := infraPurgeDigest(infraPurgeKeys(selected))
	reportInfraPurgeSelection(stdout, opts.Type, class, selected, setOf(gap.HeldDifferent), digest)

	// Read every edge now, the dry run included, so the rehearsal pays the
	// edge reads the window will and fails where the window would.
	entries, err := infraPurgeBackupEntries(source, selected, all, manifest.Generation)
	if err != nil {
		fmt.Fprintf(stderr, "%s: reading the selected rows' edges for the backup: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	outbound, inbound := 0, 0
	for _, entry := range entries {
		outbound += len(entry.Deps)
		inbound += len(entry.Dependents)
	}
	fmt.Fprintf(stdout, "  edges:   %d outbound, %d inbound (backed up and removed with the rows)\n", outbound, inbound) //nolint:errcheck // best-effort stdout

	if opts.DryRun {
		fmt.Fprintf(stdout, "dry-run: nothing was written. To back up and delete exactly these %d row(s), run with:  --%s --%s --%s %d --%s %s\n", //nolint:errcheck // best-effort stdout
			len(selected), storageFleetStoppedFlag, strandedConfirmPurgeFlag, strandedExpectCountFlag, len(selected), strandedExpectDigestFlag, digest)
		return 0
	}
	if !opts.Confirm || opts.ExpectCount < 0 {
		fmt.Fprintf(stderr, "%s: nothing was written. Deleting %d row(s) needs --%s and --%s %d (and --%s %s), the selection reviewed above or in a dry run\n", //nolint:errcheck // best-effort stderr
			logPrefix, len(selected), strandedConfirmPurgeFlag, strandedExpectCountFlag, len(selected), strandedExpectDigestFlag, digest)
		return 1
	}
	if opts.ExpectCount != len(selected) {
		fmt.Fprintf(stderr, "%s: --%s %d does not match the %d row(s) selected now; the selection changed since it was reviewed. Run --dry-run again. Nothing was written\n", //nolint:errcheck // best-effort stderr
			logPrefix, strandedExpectCountFlag, opts.ExpectCount, len(selected))
		return 1
	}
	if opts.ExpectDigest != "" && opts.ExpectDigest != digest {
		fmt.Fprintf(stderr, "%s: --%s %s does not match the selection's digest %s; the selection changed since it was reviewed. Run --dry-run again. Nothing was written\n", //nolint:errcheck // best-effort stderr
			logPrefix, strandedExpectDigestFlag, opts.ExpectDigest, digest)
		return 1
	}

	// The binding re-read, before anything is written.
	if err := infraPurgeBeforeBindingGuard(); err != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	binding, err := openInfraBindingReadOnly(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: opening binding %s: %v. Nothing was written\n", logPrefix, target.Database, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	guardErr := refuseInfraPurgeOfBindingRows(binding, selected)
	_ = closeBeadStoreHandle(binding)
	if guardErr != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was written\n", logPrefix, guardErr) //nolint:errcheck // best-effort stderr
		return 1
	}

	keys := infraPurgeKeys(selected)
	backupPath, run := pending.Backup, pending.Run
	if hasPending {
		covered, err := infraPurgeBackupCovers(pending.Backup, entries)
		if err != nil {
			fmt.Fprintf(stderr, "%s: the interrupted purge run %s: %v. Nothing was written\n", logPrefix, pending.Run, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if len(covered) > 0 {
			fmt.Fprintf(stderr, "%s: the interrupted purge run %s backed up a different selection than this one (%s not in %s as the work store holds them now). Nothing was written. Finish or close that run first: restore it with --%s %s, or, if its rows were put back by hand, close it with --%s\n", //nolint:errcheck // best-effort stderr
				logPrefix, pending.Run, infraStrandedIDList(covered), pending.Backup, strandedRestorePurgeFlag, pending.Backup, strandedAbandonPurgeFlag)
			return 1
		}
		fmt.Fprintf(stdout, "resuming the purge run %s: its backup %s already holds every selected row as the work store holds it\n", run, backupPath) //nolint:errcheck // best-effort stdout
	} else {
		now := time.Now().UTC()
		run = now.Format("20060102T150405.000000000Z")
		backupPath = filepath.Join(target.Root, infraPurgedStrandedBackupPrefix+run+".jsonl")
		records := make([]infraRecoverChoiceRecord, 0, len(entries))
		for _, entry := range entries {
			records = append(records, infraRecoverChoiceRecord{Format: infraRecoverChoicesFormat, Choice: infraPurgeChoice, Store: infraRecoverChoiceStoreWork, RecordedAt: now, Entry: entry})
		}
		if err := writeInfraPurgeBackup(target.Root, backupPath, records); err != nil {
			fmt.Fprintf(stderr, "%s: %v. Nothing was deleted\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		pending = infraPurgeLedgerRecord{
			Format: infraPurgeLedgerFormat, Event: infraPurgeEventBackedUp, Run: run, Type: opts.Type,
			Backup: backupPath, Rows: keys, RecordedAt: now,
		}
		if err := appendInfraPurgeLedger(target, ledger, pending); err != nil {
			fmt.Fprintf(stderr, "%s: %v. Nothing was deleted; the backup is %s\n", logPrefix, err, backupPath) //nolint:errcheck // best-effort stderr
			return 1
		}
		ledger = append(ledger, pending)
	}
	fmt.Fprintf(stdout, "backup: %s (%d row(s) with their edges, proven from disk)\n", backupPath, len(entries)) //nolint:errcheck // best-effort stdout

	if err := infraPurgeAfterBackup(); err != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was deleted; run this again to resume from %s\n", logPrefix, err, backupPath) //nolint:errcheck // best-effort stderr
		return 1
	}

	if err := deleteInfraPurgeRows(source, entries); err != nil {
		fmt.Fprintf(stderr, "%s: deleting the backed-up rows: %v. Every row is in %s; run this again to finish\n", logPrefix, err, backupPath) //nolint:errcheck // best-effort stderr
		return 1
	}
	if left, err := infraPurgeRowsStillPresent(source, keys); err != nil {
		fmt.Fprintf(stderr, "%s: re-reading the work store after the delete: %v. Every row is in %s; run this again to finish\n", logPrefix, err, backupPath) //nolint:errcheck // best-effort stderr
		return 1
	} else if len(left) > 0 {
		fmt.Fprintf(stderr, "%s: %d row(s) are still in the work store after the delete: %s. Every row is in %s; run this again to finish\n", logPrefix, len(left), infraStrandedIDList(left), backupPath) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := infraPurgeAfterDeleteRecorded(); err != nil {
		fmt.Fprintf(stderr, "%s: %v. The rows are deleted and backed up in %s; run this again to record the purge\n", logPrefix, err, backupPath) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Every row of the run the work store no longer holds, so rows an
	// interrupted delete already took are counted.
	purgedKeys, err := infraPurgeKeysGone(source, pending.Rows)
	if err != nil {
		fmt.Fprintf(stderr, "%s: re-reading the work store after the delete: %v. Every row is in %s; run this again to record the purge\n", logPrefix, err, backupPath) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := recordInfraPurged(target, ledger, pending, purgedKeys); err != nil {
		fmt.Fprintf(stderr, "%s: the %d row(s) were deleted and backed up in %s, but recording the purge failed: %v. Run this again to record it\n", logPrefix, len(selected), backupPath, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "purged: %d stranded %s row(s) deleted from the work store; backup %s\nnext: gc storage status\n", len(purgedKeys), opts.Type, backupPath) //nolint:errcheck // best-effort stdout
	return 0
}

// settleInfraPurgeWithNothingSelected finishes an interrupted run when
// nothing is left to delete: the rows the work store no longer holds are
// recorded as purged. A run whose rows are all still there, and none
// stranded, was not a purge at all, and only the operator can close it.
func settleInfraPurgeWithNothingSelected(target infraBindingTarget, ledger []infraPurgeLedgerRecord, pending infraPurgeLedgerRecord, gone []infraPurgeRowKey, logPrefix string, stdout, stderr io.Writer) int {
	if len(gone) == 0 {
		fmt.Fprintf(stderr, "%s: the interrupted purge run %s deleted none of its %d row(s), and none of them is stranded now. If they were put back on purpose, close the run with --%s\n", //nolint:errcheck // best-effort stderr
			logPrefix, pending.Run, len(pending.Rows), strandedAbandonPurgeFlag)
		return 1
	}
	if err := recordInfraPurged(target, ledger, pending, gone); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "finished the interrupted purge run %s: recorded %d deleted row(s) (backup %s)\nnext: gc storage status\n", pending.Run, len(gone), pending.Backup) //nolint:errcheck // best-effort stdout
	return 0
}

// abandonInfraPurge is --abandon-purge: it closes an interrupted run whose
// rows are all back in the work store, put back by hand. It refuses while any
// is missing, because closing the run then would lose the record of a delete.
func abandonInfraPurge(target infraBindingTarget, ledger []infraPurgeLedgerRecord, pending infraPurgeLedgerRecord, hasPending bool, gone []infraPurgeRowKey, dryRun bool, logPrefix string, stdout, stderr io.Writer) int {
	if !hasPending {
		fmt.Fprintf(stderr, "%s: --%s closes an interrupted purge, and the ledger %s shows none\n", logPrefix, strandedAbandonPurgeFlag, target.PurgeLedgerPath()) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(gone) > 0 {
		ids := make([]string, 0, len(gone))
		for _, key := range gone {
			ids = append(ids, key.ID)
		}
		fmt.Fprintf(stderr, "%s: --%s refused: %d row(s) of the interrupted run %s are not in the work store (%s). Put them back with --%s %s, or finish the purge by running it again without --%s\n", //nolint:errcheck // best-effort stderr
			logPrefix, strandedAbandonPurgeFlag, len(gone), pending.Run, infraStrandedIDList(ids), strandedRestorePurgeFlag, pending.Backup, strandedAbandonPurgeFlag)
		return 1
	}
	if dryRun {
		fmt.Fprintf(stdout, "dry-run: would close the interrupted purge run %s; all %d of its row(s) are in the work store\n", pending.Run, len(pending.Rows)) //nolint:errcheck // best-effort stdout
		return 0
	}
	record := pending
	record.Event = infraPurgeEventAbandoned
	record.RecordedAt = time.Now().UTC()
	if err := appendInfraPurgeLedger(target, ledger, record); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "closed the interrupted purge run %s: all %d of its row(s) are in the work store; its backup %s is kept\n", pending.Run, len(pending.Rows), pending.Backup) //nolint:errcheck // best-effort stdout
	return 0
}

// recordInfraPurged appends the purged record for run.
func recordInfraPurged(target infraBindingTarget, ledger []infraPurgeLedgerRecord, run infraPurgeLedgerRecord, rows []infraPurgeRowKey) error {
	record := run
	record.Event = infraPurgeEventPurged
	record.Rows = rows
	record.RecordedAt = time.Now().UTC()
	return appendInfraPurgeLedger(target, ledger, record)
}

// infraPurgePendingRun returns the interrupted run: the ledger's last record,
// when it is a backup no later record closed.
func infraPurgePendingRun(ledger []infraPurgeLedgerRecord) (infraPurgeLedgerRecord, bool) {
	if len(ledger) == 0 || ledger[len(ledger)-1].Event != infraPurgeEventBackedUp {
		return infraPurgeLedgerRecord{}, false
	}
	return ledger[len(ledger)-1], true
}

// infraPurgeDigest is the selection's fingerprint: the first 16 hex digits of
// the SHA-256 of its sorted (id, created_at second) pairs. The dry run prints
// it and --expect-digest pins the delete to it.
func infraPurgeDigest(keys []infraPurgeRowKey) string {
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("%s\t%d\n", key.ID, key.CreatedAt.Unix()))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "")))
	return hex.EncodeToString(sum[:])[:16]
}

// reportInfraPurgeSelection prints what the purge selected: the count, the
// digest, the id range, the statuses, the creation dates, the title
// histogram, and one line per row.
func reportInfraPurgeSelection(w io.Writer, purgeType string, class coordclass.Class, rows []beads.Bead, heldDifferent map[string]bool, digest string) {
	ids := make([]string, 0, len(rows))
	statuses := map[string]int{}
	titles := map[string]int{}
	ephemeral := 0
	first, last := rows[0].CreatedAt, rows[0].CreatedAt
	for _, b := range rows {
		ids = append(ids, b.ID)
		statuses[b.Status]++
		titles[b.Title]++
		if b.Ephemeral {
			ephemeral++
		}
		if b.CreatedAt.Before(first) {
			first = b.CreatedAt
		}
		if b.CreatedAt.After(last) {
			last = b.CreatedAt
		}
	}
	sort.Strings(ids)
	var parts []string
	for _, status := range sortedMapKeys(statuses) {
		parts = append(parts, fmt.Sprintf("%s %d", status, statuses[status]))
	}
	fmt.Fprintf(w, "selected: %d stranded work-store row(s) of type %s (class %s)\n", len(rows), purgeType, class) //nolint:errcheck // best-effort output
	fmt.Fprintf(w, "  digest:  %s\n", digest)                                                                      //nolint:errcheck // best-effort output
	fmt.Fprintf(w, "  ids:     %s .. %s\n", ids[0], ids[len(ids)-1])                                               //nolint:errcheck // best-effort output
	fmt.Fprintf(w, "  created: %s .. %s\n", first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339))     //nolint:errcheck // best-effort output
	fmt.Fprintf(w, "  status:  %s\n", strings.Join(parts, ", "))                                                   //nolint:errcheck // best-effort output
	fmt.Fprintf(w, "  tier:    wisp %d, issues %d\n", ephemeral, len(rows)-ephemeral)                              //nolint:errcheck // best-effort output
	fmt.Fprintln(w, "  titles:")                                                                                   //nolint:errcheck // best-effort output
	byCount := sortedMapKeys(titles)
	sort.SliceStable(byCount, func(i, j int) bool { return titles[byCount[i]] > titles[byCount[j]] })
	for _, title := range byCount {
		fmt.Fprintf(w, "    %d  %q\n", titles[title], title) //nolint:errcheck // best-effort output
	}
	fmt.Fprintln(w, "  rows:") //nolint:errcheck // best-effort output
	sorted := append([]beads.Bead(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, b := range sorted {
		note := ""
		if heldDifferent[b.ID] {
			note = "  (reused id: the binding holds a different bead under the id, and keeps it)"
		}
		fmt.Fprintf(w, "    %s  created=%s  status=%s  title=%q%s\n", b.ID, b.CreatedAt.UTC().Format(time.RFC3339), b.Status, b.Title, note) //nolint:errcheck // best-effort output
	}
}

// infraPurgeBatchChunk bounds the anchors of one batched edge read.
const infraPurgeBatchChunk = 256

// infraPurgeBackupEntries reads every selected row's edges: its own outbound
// edges and every inbound edge from any work-store row, with payloads where
// the store can report them. The class is recorded under gen, the manifest's
// classifier generation, which is what selected the row.
func infraPurgeBackupEntries(source beads.Store, rows, all []beads.Bead, gen coordclass.Generation) ([]infraBackupEntry, error) {
	reader, err := newSourceDepReader(source, rows[0].ID, sourceAnchorIDs(rows))
	if err != nil {
		return nil, err
	}
	inbound, err := infraPurgeInboundEdges(source, reader, rows, all)
	if err != nil {
		return nil, err
	}
	entries := make([]infraBackupEntry, 0, len(rows))
	for _, b := range rows {
		entry := infraBackupEntry{Bead: b, Class: coordclass.ClassifyAt(b, gen).String()}
		down, ok, err := reader.deps(b)
		if !ok {
			return nil, fmt.Errorf("the edges of %s cannot be read: %w", b.ID, err)
		}
		for _, d := range down {
			edge, err := infraPurgeBackupEdge(source, d)
			if err != nil {
				return nil, err
			}
			entry.Deps = append(entry.Deps, edge)
		}
		for _, d := range inbound[b.ID] {
			edge, err := infraPurgeBackupEdge(source, d)
			if err != nil {
				return nil, err
			}
			entry.Dependents = append(entry.Dependents, edge)
		}
		sortInfraBackupEdges(entry.Deps)
		sortInfraBackupEdges(entry.Dependents)
		entries = append(entries, entry)
	}
	return entries, nil
}

// infraPurgeInboundEdges returns, per selected row, every edge into it from
// any work-store row. It reads them from the whole store's listing rather
// than one dependents query per row: a hosted work store answers each query
// with a bd process over the network, and a purge of hundreds of rows would
// pay one round trip per row. A store that lists relations is asked for every
// row's outbound edges in batches; a store that refuses relation listing
// carries them inline on the listing itself, which the dep reader has
// witnessed live. Only a store with neither falls back to a query per row.
func infraPurgeInboundEdges(source beads.Store, reader *sourceDepReader, rows, all []beads.Bead) (map[string][]beads.Dep, error) {
	selected := make(map[string]bool, len(rows))
	for _, b := range rows {
		selected[b.ID] = true
	}
	inbound := map[string][]beads.Dep{}
	if !reader.relationsOK {
		if !reader.inlineWitnessed {
			return nil, fmt.Errorf("the work store refuses relation listing (%w) and carries no inline dependency projection, so nothing can state the edges a backup must keep", reader.relationsErr)
		}
		for _, b := range all {
			for _, d := range b.Dependencies {
				if selected[d.DependsOnID] {
					inbound[d.DependsOnID] = append(inbound[d.DependsOnID], d)
				}
			}
		}
		return inbound, nil
	}
	if batch, ok := beads.DepListBatchFor(source); ok {
		ids := make([]string, 0, len(all))
		for _, b := range all {
			ids = append(ids, b.ID)
		}
		supported := true
		for start := 0; start < len(ids) && supported; start += infraPurgeBatchChunk {
			end := min(start+infraPurgeBatchChunk, len(ids))
			edges, err := batch.DepListBatch(ids[start:end])
			if errors.Is(err, beads.ErrDepListBatchUnsupported) {
				supported = false
				break
			}
			if err != nil {
				return nil, fmt.Errorf("reading the work store's edges in batches: %w", err)
			}
			for _, anchor := range ids[start:end] {
				for _, d := range edges[anchor] {
					if selected[d.DependsOnID] {
						inbound[d.DependsOnID] = append(inbound[d.DependsOnID], d)
					}
				}
			}
		}
		if supported {
			return inbound, nil
		}
		inbound = map[string][]beads.Dep{}
	}
	for _, b := range rows {
		up, err := source.DepList(b.ID, "up")
		if err != nil {
			return nil, fmt.Errorf("listing dependents of %s: %w", b.ID, err)
		}
		inbound[b.ID] = up
	}
	return inbound, nil
}

// infraPurgeBackupEdge records one edge, with its payload when the store can
// report one. A store that has no payload read at all (bd, exec) never stored
// a payload through gc, so the edge is recorded without one.
func infraPurgeBackupEdge(source beads.Store, d beads.Dep) (infraBackupEdge, error) {
	if _, ok := source.(beads.DepMetadataReader); !ok {
		return infraBackupEdge{IssueID: d.IssueID, DependsOnID: d.DependsOnID, Type: d.Type}, nil
	}
	return infraBackupEdgeFor(source, d)
}

// infraPurgeKeys lists the (id, created_at) of rows, sorted by id.
func infraPurgeKeys(rows []beads.Bead) []infraPurgeRowKey {
	keys := make([]infraPurgeRowKey, 0, len(rows))
	for _, b := range rows {
		keys = append(keys, infraPurgeRowKey{ID: b.ID, CreatedAt: b.CreatedAt.UTC()})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return keys
}

// infraPurgeBackupCovers returns the ids of entries the backup at path does
// not hold exactly (the row and its edges as the work store holds them now).
// None means a resume may delete from that backup.
func infraPurgeBackupCovers(path string, entries []infraBackupEntry) ([]string, error) {
	records, err := readInfraPurgeBackup(path)
	if err != nil {
		return nil, err
	}
	held := make(map[string][]byte, len(records))
	for _, record := range records {
		encoded, err := json.Marshal(record.Entry)
		if err != nil {
			return nil, err
		}
		held[record.Entry.Bead.ID] = encoded
	}
	var missing []string
	for _, entry := range entries {
		encoded, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(held[entry.Bead.ID], encoded) {
			missing = append(missing, entry.Bead.ID)
		}
	}
	sort.Strings(missing)
	return missing, nil
}

// infraPurgeKeysGone returns the keys the work store no longer holds as the
// same bead.
func infraPurgeKeysGone(source beads.Store, keys []infraPurgeRowKey) ([]infraPurgeRowKey, error) {
	left, err := infraPurgeRowsStillPresent(source, keys)
	if err != nil {
		return nil, err
	}
	present := setOf(left)
	gone := make([]infraPurgeRowKey, 0, len(keys))
	for _, key := range keys {
		if !present[key.ID] {
			gone = append(gone, key)
		}
	}
	return gone, nil
}

// refuseInfraPurgeOfBindingRows is the check before anything is written: no
// row the binding holds as the same bead (the same id, created in the same
// second) may be deleted, whatever selected it. A row the binding holds for a
// different bead (a reused id) passes: deleting the work-store row leaves the
// binding's bead alone.
func refuseInfraPurgeOfBindingRows(binding beads.Store, rows []beads.Bead) error {
	var same []string
	for _, b := range rows {
		held, present, err := readInfraHeldRow(binding, b.ID)
		if err != nil {
			return err
		}
		if present && infraHeldRowIsThisBead(b, held) {
			same = append(same, b.ID)
		}
	}
	if len(same) > 0 {
		sort.Strings(same)
		return fmt.Errorf("the binding holds %d selected row(s) as the same bead (the same id and created_at), so they are not stranded and are never purged: %s", len(same), strings.Join(same, ", "))
	}
	return nil
}

// deleteInfraPurgeRows deletes the backed-up rows from the work store. Every
// recorded edge goes first: `bd delete --force` rewrites the text of every
// row still linked to a deleted one, replacing each mention of the id with
// "[deleted:<id>]", and a row with no links left is not rewritten. Then the
// rows, in one batch where the store can and one by one otherwise.
func deleteInfraPurgeRows(source beads.Store, entries []infraBackupEntry) error {
	seen := map[[2]string]bool{}
	for _, entry := range entries {
		for _, edges := range [][]infraBackupEdge{entry.Deps, entry.Dependents} {
			for _, edge := range edges {
				pair := [2]string{edge.IssueID, edge.DependsOnID}
				if seen[pair] {
					continue
				}
				seen[pair] = true
				if err := source.DepRemove(edge.IssueID, edge.DependsOnID); err != nil && !errors.Is(err, beads.ErrNotFound) {
					return fmt.Errorf("removing dep %s: %w", edge, err)
				}
			}
		}
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.Bead.ID)
	}
	if deleter, ok := source.(beads.BatchDeleter); ok {
		err := deleter.DeleteBatch(ids)
		if err == nil || !errors.Is(err, beads.ErrBatchDeleteUnsupported) {
			return err
		}
	}
	for _, id := range ids {
		if err := source.Delete(id); err != nil && !errors.Is(err, beads.ErrNotFound) {
			return fmt.Errorf("removing %s: %w", id, err)
		}
	}
	return nil
}

// infraPurgeRowsStillPresent re-reads the work store and returns the ids of
// keys it still holds as the same bead.
func infraPurgeRowsStillPresent(source beads.Store, keys []infraPurgeRowKey) ([]string, error) {
	rows, err := source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return nil, err
	}
	want := make(map[string]time.Time, len(keys))
	for _, key := range keys {
		want[key.ID] = key.CreatedAt
	}
	var left []string
	for _, b := range rows {
		if at, ok := want[b.ID]; ok && sameCreatedSecond(at, b.CreatedAt) {
			left = append(left, b.ID)
		}
	}
	sort.Strings(left)
	return left, nil
}

// readInfraPurgeBackup reads one purge backup.
func readInfraPurgeBackup(path string) ([]infraRecoverChoiceRecord, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the purge backup %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	var records []infraRecoverChoiceRecord
	for decoder.More() {
		var record infraRecoverChoiceRecord
		if err := decoder.Decode(&record); err != nil {
			return nil, fmt.Errorf("reading the purge backup %s: record %d: %w", path, len(records)+1, err)
		}
		if record.Format != infraRecoverChoicesFormat || record.Choice != infraPurgeChoice {
			return nil, fmt.Errorf("reading the purge backup %s: record %d is %q/%q, want %q/%q", path, len(records)+1, record.Format, record.Choice, infraRecoverChoicesFormat, infraPurgeChoice)
		}
		records = append(records, record)
	}
	return records, nil
}

// writeInfraPurgeBackup writes records to path durably (temp file, sync,
// rename, directory sync) and proves them by reading them back. Nothing is
// deleted until it returns nil.
func writeInfraPurgeBackup(dir, path string, records []infraRecoverChoiceRecord) error {
	if err := writeInfraPurgeFile(dir, path, func(encoder *json.Encoder) error {
		for _, record := range records {
			if err := encoder.Encode(record); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("writing the purge backup %s: %w", path, err)
	}
	got, err := readInfraPurgeBackup(path)
	if err != nil {
		return fmt.Errorf("proving the purge backup: %w", err)
	}
	if len(got) != len(records) {
		return fmt.Errorf("proving the purge backup: %s holds %d record(s), want %d", path, len(got), len(records))
	}
	for i := range records {
		want, err := json.Marshal(records[i])
		if err != nil {
			return err
		}
		have, err := json.Marshal(got[i])
		if err != nil {
			return err
		}
		if !bytes.Equal(want, have) {
			return fmt.Errorf("proving the purge backup: %s holds a different %s than the work store", path, records[i].Entry.Bead.ID)
		}
	}
	return nil
}

// writeInfraPurgeFile replaces path with what write encodes, durably: a temp
// file in dir, synced, renamed over path, and the directory synced, so a crash
// leaves either the old file or the new one and never a torn line.
func writeInfraPurgeFile(dir, path string, write func(*json.Encoder) error) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(cause error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return cause
	}
	writer := bufio.NewWriter(tmp)
	if err := write(json.NewEncoder(writer)); err != nil {
		return fail(err)
	}
	if err := writer.Flush(); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := infraMigrationRename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close() //nolint:errcheck // read-only directory handle
	return handle.Sync()
}

// readInfraPurgeLedger returns every ledger record, oldest first. An absent
// ledger holds none.
func readInfraPurgeLedger(target infraBindingTarget) ([]infraPurgeLedgerRecord, error) {
	contents, err := os.ReadFile(target.PurgeLedgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the purge ledger %s: %w", target.PurgeLedgerPath(), err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	var records []infraPurgeLedgerRecord
	for decoder.More() {
		var record infraPurgeLedgerRecord
		if err := decoder.Decode(&record); err != nil {
			return nil, fmt.Errorf("reading the purge ledger %s: record %d: %w", target.PurgeLedgerPath(), len(records)+1, err)
		}
		if record.Format != infraPurgeLedgerFormat {
			return nil, fmt.Errorf("reading the purge ledger %s: record %d has format %q, want %q", target.PurgeLedgerPath(), len(records)+1, record.Format, infraPurgeLedgerFormat)
		}
		records = append(records, record)
	}
	return records, nil
}

// appendInfraPurgeLedger adds record to the ledger durably. previous is the
// ledger as this run read it under the migration guard.
func appendInfraPurgeLedger(target infraBindingTarget, previous []infraPurgeLedgerRecord, record infraPurgeLedgerRecord) error {
	err := writeInfraPurgeFile(target.Root, target.PurgeLedgerPath(), func(encoder *json.Encoder) error {
		for _, r := range append(append([]infraPurgeLedgerRecord(nil), previous...), record) {
			if err := encoder.Encode(r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("recording the purge in %s: %w", target.PurgeLedgerPath(), err)
	}
	return nil
}

// infraPurgeNetRows replays the ledger: a purged record adds its rows, a
// restored record removes them. What is left is every row a purge deleted and
// nothing has put back.
func infraPurgeNetRows(ledger []infraPurgeLedgerRecord) map[infraPurgeRowKey]string {
	net := map[infraPurgeRowKey]string{}
	for _, record := range ledger {
		for _, key := range record.Rows {
			key.CreatedAt = key.CreatedAt.UTC().Truncate(time.Second)
			switch record.Event {
			case infraPurgeEventPurged:
				net[key] = record.Backup
			case infraPurgeEventRestored:
				delete(net, key)
			}
		}
	}
	return net
}

// reportInfraPurgeLedger is `gc storage status`'s purge readout. It returns
// false when the ledger blocks: an interrupted purge, or a ledger nothing can
// read.
func reportInfraPurgeLedger(target infraBindingTarget, stdout io.Writer) bool {
	ledger, err := readInfraPurgeLedger(target)
	if err != nil {
		fmt.Fprintf(stdout, "  purge ledger: UNREADABLE — %v\nblocking invariant: the ledger of `%s --%s` runs cannot be read, so nothing can say whether a purge is unfinished. Every purged row is in the infra.purged-stranded-*.jsonl backups beside it, which this does not touch. Restore the ledger from your snapshot, or move it aside once `%s --%s message --dry-run` shows nothing left to purge and no row of those backups is missing that should not be\n", //nolint:errcheck // best-effort stdout
			err, storageRecoveryCommand, strandedPurgeTypeFlag, storageRecoveryCommand, strandedPurgeTypeFlag)
		return false
	}
	if len(ledger) == 0 {
		return true
	}
	net := infraPurgeNetRows(ledger)
	backups := map[string]int{}
	for _, backup := range net {
		backups[backup]++
	}
	restored := 0
	for _, record := range ledger {
		if record.Event == infraPurgeEventRestored {
			restored += len(record.Rows)
		}
	}
	if len(net) > 0 || restored > 0 {
		fmt.Fprintf(stdout, "  purged stranded: %d bead(s) by --%s\n", len(net), strandedPurgeTypeFlag) //nolint:errcheck // best-effort stdout
		for _, backup := range sortedMapKeys(backups) {
			fmt.Fprintf(stdout, "    backup %s (%d row(s))\n", backup, backups[backup]) //nolint:errcheck // best-effort stdout
		}
		if restored > 0 {
			fmt.Fprintf(stdout, "  restored by --%s: %d row(s)\n", strandedRestorePurgeFlag, restored) //nolint:errcheck // best-effort stdout
		}
	}
	if last, pending := infraPurgePendingRun(ledger); pending {
		fmt.Fprintf(stdout, "  purge: INTERRUPTED (run %s backed up %d row(s) to %s and did not record the delete)\nblocking invariant: finish it by running the purge again (`%s --%s --%s %s --%s --%s <n>`; a run with nothing left to delete records it), restore it with `%s --%s --%s %s`, or, if its rows were put back by hand, close it with --%s\n", //nolint:errcheck // best-effort stdout
			last.Run, len(last.Rows), last.Backup,
			storageRecoveryCommand, storageFleetStoppedFlag, strandedPurgeTypeFlag, last.Type, strandedConfirmPurgeFlag, strandedExpectCountFlag,
			storageRecoveryCommand, storageFleetStoppedFlag, strandedRestorePurgeFlag, last.Backup, strandedAbandonPurgeFlag)
		return false
	}
	return true
}
