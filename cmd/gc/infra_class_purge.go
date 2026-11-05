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
// puts elsewhere — a wisp step typed message is graph — is not mail. It is
// left exactly where it is and named, as a stranded row of another type is,
// and the run purges the rest. recover-stranded carries those rows once the
// mail is gone; run before that, its copy would carry the mail too, which is
// what the purge exists to prevent, so that is the order the run names.
//
// A work-store row the binding holds as the same bead (the same id, created
// in the same second) is never deleted. Such a row is not stranded, so it is
// never selected; and refuseInfraPurgeOfBindingRows re-reads the binding for
// every selected row before anything is written.
//
// # Tied to the reviewed list
//
// The dry run prints the selection's count and digest (infraPurgeDigest: the
// sorted (id, created_at) pairs). The delete needs --confirm-purge,
// --expect-count and --expect-digest, and refuses when the selection it
// computes is not the one the operator reviewed. The count alone does not pin
// it: one stranded row leaving and another arriving keeps the count.
//
// # Back up, prove, then delete
//
// Every selected row is written, with its metadata, labels and every edge
// touching it, to a timestamped JSONL file at the binding root (under
// .gc/store), in recover-stranded's choice-backup shape
// (infraRecoverChoiceRecord). An edge's far end outside the selection is
// recorded by which bead it is, its id and created_at, because bd mints a
// deleted id again: a restore puts back only an edge whose far end is still
// that bead (infraPurgeRestorableEdges), and names the rest. The dry run and
// the run name every edge whose far end the work store does not hold, which
// no restore could put back. The file is synced, renamed into place, its
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
// A run that dies after the backup leaves its "backed-up" record with no later
// record of the same run, and status refuses ("purge: INTERRUPTED"). The next
// run finishes that run over the run's own rows, by (id, created_at): a row
// stranded since the backup, or a new bead bd minted under an id the run
// deleted, is left in place and named, for a purge once the run is finished
// (infraPurgeRowsOfRun). Taking it in would write a second backup with split
// accounting, and holding the run open for it would leave a run nothing could
// finish: a restore must not overwrite a new bead under a reused id. The next
// run:
//
//   - records as purged every row of that run the work store no longer holds,
//     so a run that died after the delete and before the record converges;
//   - resumes from the same backup when every row of the run the work store
//     still holds is selected, each as backed up apart from what the run's own
//     edge removal changed (infraPurgeEntryCovers);
//   - refuses when a row of the run changed since the backup, or is still in
//     the work store and not selected now, which the run's edge removal may
//     have left without its edges (infraPurgeLeftBehindRefusal).
//
// A dry run over an interrupted run refuses wherever the run would.
//
// --abandon-purge closes an interrupted run whose rows and edges, payloads
// included, were all put back by hand (the raw bd fallback); --restore-purge
// closes it itself, and restores no other run's backup while it is open.
// recover-stranded, its copy and its per-id answers alike, refuses while a run
// is interrupted (infraPurgeUnsettledRefusal): it would carry the run's rows
// into the binding without the edges the run removed, which only the backup
// holds, and the restore writes only the work store.
//
// A restore of a finished run opens the run again before it writes anything
// (a "restoring" record), and its "restored" record closes it. A restore that
// dies between the two leaves the run interrupted: rows back without the edges
// the restore had still to add, which recover-stranded must not carry into the
// binding either. Running the restore again finishes it, running the purge
// again deletes the rows it put back, and --abandon-purge closes it once its
// rows and edges are all back.
//
// # Capabilities
//
// Every production open hands the work store out under the bead-policy
// wrapper, which declares every optional capability whatever the store under
// it can do. The purge and the restore decide the edge-payload read and the
// verbatim import on the store under it (infraPurgeStores.underPolicy), so a
// dry run refuses exactly where the run would.
//
// A work store that refuses relation listing (bd's Postgres backend, whose
// edges recover-stranded reads off the listing's inline projection instead)
// is refused: the restore and --abandon-purge read every edge back through
// that listing, so a purge there could not be undone.
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

// infraPurgePayloadFreeClasses are the purge classes whose edges never carry a
// payload. gc writes edge payloads only on recipe steps (BdStore.ApplyGraphPlan
// carries them into bd, which keeps them), and a messaging row is never a
// recipe step. Only for such a class does a work store with no payload read
// back an edge up as carrying none (infraPurgeBackupEdge).
var infraPurgePayloadFreeClasses = map[coordclass.Class]bool{
	coordclass.ClassMessaging: true,
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
// rows --restore-purge put back. restoring opens a finished run again before
// --restore-purge writes anything, until its restored record closes it.
const (
	infraPurgeEventBackedUp  = "backed-up"
	infraPurgeEventPurged    = "purged"
	infraPurgeEventAbandoned = "abandoned"
	infraPurgeEventRestoring = "restoring"
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
	// ExpectDigest is the selection digest the operator reviewed; empty means
	// not given. The delete refuses without it.
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
	infraPurgeAfterEdgesRemoved   = func() error { return nil }
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
// resolved binding, its manifest, the open stores, and (for a writing run)
// the migration guard.
type infraPurgeSession struct {
	target   infraBindingTarget
	manifest infraCopyManifest
	stores   *infraPurgeStores
	release  func()
}

func (s *infraPurgeSession) close() {
	if s.stores != nil {
		s.stores.closeWork()
	}
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
	stores, err := openInfraPurgeStores(request.CityPath, target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: opening the work store: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		if session.release != nil {
			session.release()
		}
		return nil, false
	}
	session.stores = stores
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
	target, manifest, source := session.target, session.manifest, session.stores.work

	ledger, err := readInfraPurgeLedger(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	pending, hasPending := infraPurgePendingRun(ledger)
	var pendingGone []infraPurgeRowKey
	var pendingReminted []string
	if hasPending {
		pendingGone, pendingReminted, err = infraPurgeKeysGone(source, pending.Rows)
		if err != nil {
			fmt.Fprintf(stderr, "%s: reading the work store: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	if opts.Abandon {
		return abandonInfraPurge(session, ledger, pending, hasPending, pendingGone, pendingReminted, opts.DryRun, logPrefix, stdout, stderr)
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
	// A stranded row of another type, or of the type and another class, is not
	// what this purge deletes: it stays, and is named. Neither ever refuses the
	// run, so neither can hold an interrupted run's resume either.
	var selected, others []beads.Bead
	var otherClass, vanished []string
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
			otherClass = append(otherClass, fmt.Sprintf("%s (classifies as %s)", b.ID, got))
			continue
		}
		selected = append(selected, b)
	}
	if len(vanished) > 0 {
		fmt.Fprintf(stderr, "%s: the work store changed while it was read (%s were stranded a moment ago and are gone); a purge judged against a moving store proves nothing. Stop every writer and run this again. Nothing was written\n", //nolint:errcheck // best-effort stderr
			logPrefix, strings.Join(vanished, ", "))
		return 1
	}
	// An interrupted run is finished over its own rows only; any other
	// stranded row of the type waits for the run after it.
	var notOfRun []beads.Bead
	if hasPending {
		selected, notOfRun = infraPurgeRowsOfRun(selected, pending.Rows)
	}
	recoverAfter := infraPurgeRecoverAfter(opts.Type, len(selected)+len(notOfRun) > 0)
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
		fmt.Fprintf(stdout, "not purged: %d stranded row(s) of other types stay where they are, and the boot keeps refusing over them until they are recovered with `%s`%s: %s\n", //nolint:errcheck // best-effort stdout
			len(others), storageRecoveryCommand, recoverAfter, beadIDList(others))
	}
	if len(otherClass) > 0 {
		fmt.Fprintf(stdout, "not purged: %d stranded %s row(s) are not %s-class, so this purge does not delete them; they stay where they are, and the boot keeps refusing over them until they are recovered with `%s`%s: %s\n", //nolint:errcheck // best-effort stdout
			len(otherClass), opts.Type, class, storageRecoveryCommand, recoverAfter, strings.Join(otherClass, ", "))
	}
	if hasPending {
		fmt.Fprintf(stdout, "interrupted purge: %s; %d of its %d row(s) are no longer in the work store\n", infraPurgePendingSummary(pending), len(pendingGone), len(pending.Rows)) //nolint:errcheck // best-effort stdout
	}
	if len(notOfRun) > 0 {
		ids := make([]string, 0, len(notOfRun))
		for _, b := range notOfRun {
			ids = append(ids, b.ID)
		}
		fmt.Fprintf(stdout, "not in the interrupted run: %d stranded %s row(s) are not rows run %s backed up (stranded since its backup, or a new bead under an id it deleted); they stay in place, and a purge after this run is finished selects them: %s\n", //nolint:errcheck // best-effort stdout
			len(notOfRun), opts.Type, pending.Run, infraStrandedIDList(ids))
	}

	if len(selected) == 0 {
		if hasPending {
			return settleInfraPurgeWithNothingSelected(target, ledger, pending, pendingGone, opts.DryRun, logPrefix, stdout, stderr)
		}
		fmt.Fprintf(stdout, "nothing to purge: no stranded row of type %s\n", opts.Type) //nolint:errcheck // best-effort stdout
		return 0
	}
	digest := infraPurgeDigest(infraPurgeKeys(selected))
	reportInfraPurgeSelection(stdout, opts.Type, class, selected, setOf(gap.HeldDifferent), digest)

	// Read every edge now, the dry run included, so the rehearsal pays the
	// edge reads the window will and fails where the window would.
	entries, err := infraPurgeBackupEntries(session.stores, selected, all, manifest.Generation)
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
	if _, unheld := infraPurgeRestorableEdges(entries, byID); len(unheld) > 0 {
		fmt.Fprintf(stdout, "  not restorable: %d edge(s) end at a row the work store does not hold, so --%s could not put them back; the backup keeps them: %s\n", //nolint:errcheck // best-effort stdout
			len(unheld), strandedRestorePurgeFlag, infraPurgeLeftOutList(unheld))
	}

	// An interrupted run resumes only over its own backup, checked before the
	// dry run returns so the rehearsal refuses where the run would.
	if hasPending {
		if err := infraPurgeResumeRefusal(pending, pendingGone, entries); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if opts.DryRun {
		fmt.Fprintf(stdout, "dry-run: nothing was written. To back up and delete exactly these %d row(s), run with:  --%s --%s --%s %d --%s %s\n", //nolint:errcheck // best-effort stdout
			len(selected), storageFleetStoppedFlag, strandedConfirmPurgeFlag, strandedExpectCountFlag, len(selected), strandedExpectDigestFlag, digest)
		return 0
	}
	if err := infraPurgeConfirmationRefusal(opts, len(selected), digest); err != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// The binding re-read, before anything is written.
	if err := infraPurgeBeforeBindingGuard(); err != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was written\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	binding, closeBinding, err := session.stores.openBinding()
	if err != nil {
		fmt.Fprintf(stderr, "%s: opening binding %s: %v. Nothing was written\n", logPrefix, target.Database, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	guardErr := refuseInfraPurgeOfBindingRows(binding, selected)
	closeBinding()
	if guardErr != nil {
		fmt.Fprintf(stderr, "%s: %v. Nothing was written\n", logPrefix, guardErr) //nolint:errcheck // best-effort stderr
		return 1
	}

	keys := infraPurgeKeys(selected)
	backupPath, run := pending.Backup, pending.Run
	if hasPending {
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
	purgedKeys, _, err := infraPurgeKeysGone(source, pending.Rows)
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
// nothing is left to delete: once the work store holds none of its rows, they
// are recorded as purged. A run whose rows are all still there, and none
// stranded, was not a purge at all, and only the operator can close it; one
// with some still there is refused as a resume would be
// (infraPurgeLeftBehindRefusal). A dry run refuses where the run would and
// records nothing.
func settleInfraPurgeWithNothingSelected(target infraBindingTarget, ledger []infraPurgeLedgerRecord, pending infraPurgeLedgerRecord, gone []infraPurgeRowKey, dryRun bool, logPrefix string, stdout, stderr io.Writer) int {
	if len(gone) == 0 {
		fmt.Fprintf(stderr, "%s: every one of the %d row(s) of the interrupted purge run %s is in the work store, and none of them is stranded now. If they were put back on purpose, close the run with --%s\n", //nolint:errcheck // best-effort stderr
			logPrefix, len(pending.Rows), pending.Run, strandedAbandonPurgeFlag)
		return 1
	}
	if err := infraPurgeLeftBehindRefusal(pending, gone, nil); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if dryRun {
		fmt.Fprintf(stdout, "dry-run: nothing was written. To finish the interrupted purge run %s and record its %d deleted row(s), run with:  --%s\n", pending.Run, len(gone), storageFleetStoppedFlag) //nolint:errcheck // best-effort stdout
		return 0
	}
	if err := recordInfraPurged(target, ledger, pending, gone); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "finished the interrupted purge run %s: recorded %d deleted row(s) (backup %s)\nnext: gc storage status\n", pending.Run, len(gone), pending.Backup) //nolint:errcheck // best-effort stdout
	return 0
}

// abandonInfraPurge is --abandon-purge: it closes an interrupted run whose
// rows and edges are all back in the work store, put back by hand or by a
// restore that did not record its end. It refuses while any row is missing,
// because closing the run then would lose the record of a delete, and while a
// backed-up edge a restore would put back (infraPurgeEdgesNotBack) is not back
// as backed up, payload included (infraPurgeEdgeDifference): the run removes
// its edges before its rows, and once the run is closed nothing points at the
// backup that is their only copy. reminted are the ids of gone rows the work
// store holds for a different bead now, which --restore-purge refuses too, so
// the refusal names the purge itself as the way out.
func abandonInfraPurge(session *infraPurgeSession, ledger []infraPurgeLedgerRecord, pending infraPurgeLedgerRecord, hasPending bool, gone []infraPurgeRowKey, reminted []string, dryRun bool, logPrefix string, stdout, stderr io.Writer) int {
	target := session.target
	if !hasPending {
		fmt.Fprintf(stderr, "%s: --%s closes an interrupted purge, and the ledger %s shows none\n", logPrefix, strandedAbandonPurgeFlag, target.PurgeLedgerPath()) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(gone) > 0 {
		ids := make([]string, 0, len(gone))
		for _, key := range gone {
			ids = append(ids, key.ID)
		}
		remedy := fmt.Sprintf("Put them back with --%s %s, or finish the purge by running it again without --%s", strandedRestorePurgeFlag, pending.Backup, strandedAbandonPurgeFlag)
		if len(reminted) > 0 {
			remedy = fmt.Sprintf("The work store holds a different bead under %s now (bd minted the id again), so --%s cannot put the backed-up row back either. Finish the purge instead by running it again without --%s: it leaves the new bead in place for a later purge",
				infraStrandedIDList(reminted), strandedRestorePurgeFlag, strandedAbandonPurgeFlag)
		}
		fmt.Fprintf(stderr, "%s: --%s refused: %d row(s) of the interrupted run %s are not in the work store (%s). %s\n", //nolint:errcheck // best-effort stderr
			logPrefix, strandedAbandonPurgeFlag, len(gone), pending.Run, infraStrandedIDList(ids), remedy)
		return 1
	}
	missing, err := infraPurgeEdgesNotBack(session.stores, pending.Backup)
	if err != nil {
		fmt.Fprintf(stderr, "%s: --%s: checking the interrupted run's edges: %v. Nothing was written\n", logPrefix, strandedAbandonPurgeFlag, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "%s: --%s refused: %d backed-up edge(s) of the interrupted run %s are not back in the work store as backed up although both their ends are there as the beads the backup recorded (%s), and the backup %s is their only copy. Restore the run with --%s %s, which puts them back and closes it, or put them back by hand and run this again\n", //nolint:errcheck // best-effort stderr
			logPrefix, strandedAbandonPurgeFlag, len(missing), pending.Run, strings.Join(missing, "; "), pending.Backup, strandedRestorePurgeFlag, pending.Backup)
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

// infraPurgePendingRun returns the interrupted run: the latest record that
// opens a run (backed-up, or restoring) no later record of the same run
// (purged, abandoned or restored) closed. A record of another run does not
// close it. A purge never starts a run while one is pending, and a restore
// refuses another run's backup then, so at most one run is ever pending.
func infraPurgePendingRun(ledger []infraPurgeLedgerRecord) (infraPurgeLedgerRecord, bool) {
	closed := map[string]bool{}
	for i := len(ledger) - 1; i >= 0; i-- {
		record := ledger[i]
		switch record.Event {
		case infraPurgeEventBackedUp, infraPurgeEventRestoring:
			if !closed[record.Run] {
				return record, true
			}
		default:
			closed[record.Run] = true
		}
	}
	return infraPurgeLedgerRecord{}, false
}

// infraPurgePendingSummary says what left run, the interrupted run's open
// record, unfinished.
func infraPurgePendingSummary(run infraPurgeLedgerRecord) string {
	if run.Event == infraPurgeEventRestoring {
		return fmt.Sprintf("the restore of run %s from %s did not record its end", run.Run, run.Backup)
	}
	return fmt.Sprintf("run %s backed up %d row(s) to %s and did not record the delete", run.Run, len(run.Rows), run.Backup)
}

// infraPurgeRunRemedy names the three ways to settle the interrupted run, each
// as the command an operator runs, the restore first when a restore left it
// open. Status and recover-stranded's refusal print it.
func infraPurgeRunRemedy(run infraPurgeLedgerRecord) string {
	purge := fmt.Sprintf("`%s --%s %s --dry-run`, then the same command without --dry-run and with the flags its last line names", storageRecoveryCommand, strandedPurgeTypeFlag, run.Type)
	restore := fmt.Sprintf("`%s --%s --%s %s`", storageRecoveryCommand, storageFleetStoppedFlag, strandedRestorePurgeFlag, run.Backup)
	abandon := fmt.Sprintf("`%s --%s --%s %s --%s`", storageRecoveryCommand, storageFleetStoppedFlag, strandedPurgeTypeFlag, run.Type, strandedAbandonPurgeFlag)
	if run.Event == infraPurgeEventRestoring {
		return fmt.Sprintf("finish the restore by running %s again; delete the rows it put back by running %s; or, if its rows and edges are all back, close it with %s", restore, purge, abandon)
	}
	return fmt.Sprintf("finish it by running %s (a run with nothing left to delete only records it); restore it with %s; or, if its rows and edges were all put back by hand, close it with %s", purge, restore, abandon)
}

// infraPurgeUnsettledRefusal says why recover-stranded must write nothing now,
// its copy and its per-id answers alike, or returns "" when the purge ledger
// lets it: no purge run is interrupted. An interrupted run owns the rows it
// backed up until it is settled: it may have removed their edges, or its
// restore may have put the rows back before their edges, with its backup their
// only copy. A copy would carry those rows into the binding without them, and
// --restore-purge, which writes only the work store, could not put them back
// where the city reads them. A ledger that cannot be read cannot say whether a
// run is interrupted, so it refuses too.
func infraPurgeUnsettledRefusal(target infraBindingTarget) string {
	ledger, err := readInfraPurgeLedger(target)
	if err != nil {
		return fmt.Sprintf("%v, so nothing can say whether a purge run is interrupted, and an interrupted run's backup may be the only copy of the edges of rows this would write. Nothing was written; `gc storage status` names the remedy", err)
	}
	run, interrupted := infraPurgePendingRun(ledger)
	if !interrupted {
		return ""
	}
	why := "it may have removed their edges"
	if run.Event == infraPurgeEventRestoring {
		why = "its restore did not finish, and may have put rows back without the edges it had still to add"
	}
	return fmt.Sprintf("the purge run %s is interrupted (backup %s), and it owns the rows it backed up until it is settled: %s, with its backup their only copy, and a copy now would carry those rows into the binding without them. Nothing was written. Settle the run first: %s",
		run.Run, run.Backup, why, infraPurgeRunRemedy(run))
}

// infraPurgeRecoverAfter is the order recover-stranded's copy must take for
// the stranded rows a purge of purgeType leaves in place, or "" when no
// stranded row of purgeType is left to purge (mail): the copy carries every
// stranded row, so run before the purge it would carry that mail too.
func infraPurgeRecoverAfter(purgeType string, mail bool) string {
	if !mail {
		return ""
	}
	return fmt.Sprintf(" once the stranded %s rows are purged (run before that, its copy would carry them too)", purgeType)
}

// infraPurgeConfirmationRefusal says why opts do not confirm deleting the
// selection now, n rows with digest, or returns nil when they do: the delete
// needs --confirm-purge and the count and digest the operator reviewed.
func infraPurgeConfirmationRefusal(opts strandedPurgeOptions, n int, digest string) error {
	if !opts.Confirm || opts.ExpectCount < 0 || opts.ExpectDigest == "" {
		return fmt.Errorf("deleting %d row(s) needs --%s, --%s %d and --%s %s, the selection reviewed above or in a dry run",
			n, strandedConfirmPurgeFlag, strandedExpectCountFlag, n, strandedExpectDigestFlag, digest)
	}
	if opts.ExpectCount != n {
		return fmt.Errorf("--%s %d does not match the %d row(s) selected now; the selection changed since it was reviewed. Run --dry-run again",
			strandedExpectCountFlag, opts.ExpectCount, n)
	}
	if opts.ExpectDigest != digest {
		return fmt.Errorf("--%s %s does not match the selection's digest %s; the selection changed since it was reviewed. Run --dry-run again",
			strandedExpectDigestFlag, opts.ExpectDigest, digest)
	}
	return nil
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
// the store can report them, and which bead each far end the work store holds
// outside rows is (infraPurgeEndpoints). The class is recorded under gen, the
// manifest's classifier generation, which is what selected the row.
func infraPurgeBackupEntries(stores *infraPurgeStores, rows, all []beads.Bead, gen coordclass.Generation) ([]infraBackupEntry, error) {
	reader, err := newSourceDepReader(stores.work, rows[0].ID, sourceAnchorIDs(rows))
	if err != nil {
		return nil, err
	}
	inbound, err := infraPurgeInboundEdges(stores.work, reader, rows, all)
	if err != nil {
		return nil, err
	}
	selected := make(map[string]bool, len(rows))
	for _, b := range rows {
		selected[b.ID] = true
	}
	held := make(map[string]beads.Bead, len(all))
	for _, b := range all {
		held[b.ID] = b
	}
	entries := make([]infraBackupEntry, 0, len(rows))
	for _, b := range rows {
		class := coordclass.ClassifyAt(b, gen)
		entry := infraBackupEntry{Bead: b, Class: class.String()}
		down, ok, err := reader.deps(b)
		if !ok {
			return nil, fmt.Errorf("the edges of %s cannot be read: %w", b.ID, err)
		}
		for _, d := range down {
			edge, err := infraPurgeBackupEdge(stores, class, d)
			if err != nil {
				return nil, err
			}
			entry.Deps = append(entry.Deps, edge)
		}
		for _, d := range inbound[b.ID] {
			edge, err := infraPurgeBackupEdge(stores, class, d)
			if err != nil {
				return nil, err
			}
			entry.Dependents = append(entry.Dependents, edge)
		}
		sortInfraBackupEdges(entry.Deps)
		sortInfraBackupEdges(entry.Dependents)
		entry.Endpoints = infraPurgeEndpoints(entry, selected, held)
		entries = append(entries, entry)
	}
	return entries, nil
}

// infraPurgeEndpoints is which bead each far end of entry's edges is, by id
// and created_at, sorted by id, for every far end that held, the work store's
// rows by id, holds and that is not a selected row. A selected far end is
// backed up itself. A far end the work store does not hold has no identity to
// record, and no restore can put its edge back (infraPurgeRestorableEdges).
func infraPurgeEndpoints(entry infraBackupEntry, selected map[string]bool, held map[string]beads.Bead) []infraPurgeRowKey {
	seen := map[string]bool{}
	var keys []infraPurgeRowKey
	for _, list := range [][]infraBackupEdge{entry.Deps, entry.Dependents} {
		for _, edge := range list {
			for _, end := range []string{edge.IssueID, edge.DependsOnID} {
				b, ok := held[end]
				if !ok || selected[end] || seen[end] {
					continue
				}
				seen[end] = true
				keys = append(keys, infraPurgeRowKey{ID: end, CreatedAt: b.CreatedAt.UTC()})
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return keys
}

// infraPurgeInboundEdges returns, per selected row, every edge into it from
// any work-store row. It reads them in batches of every row's outbound edges
// (infraPurgeInboundEdgesInBatches) rather than one dependents query per row:
// a hosted work store answers each query with a bd process over the network,
// and a purge of hundreds of rows would pay one round trip per row. A store
// that cannot batch, and one whose batches cannot be trusted, is asked for
// each selected row's dependents instead.
//
// A store that refuses relation listing is refused, though recover-stranded
// reads its edges off the listing's inline projection: --restore-purge and
// --abandon-purge read every edge back through the relation listing, so a
// purge there could neither be restored nor closed by hand.
func infraPurgeInboundEdges(source beads.WorkStore, reader *sourceDepReader, rows, all []beads.Bead) (map[string][]beads.Dep, error) {
	if !reader.relationsOK {
		return nil, fmt.Errorf("the work store refuses relation listing (%w), and --%s and --%s read every edge back through it, so a purge here could not be undone", reader.relationsErr, strandedRestorePurgeFlag, strandedAbandonPurgeFlag)
	}
	selected := make(map[string]bool, len(rows))
	for _, b := range rows {
		selected[b.ID] = true
	}
	if batch, ok := beads.DepListBatchFor(source); ok {
		inbound, kept, err := infraPurgeInboundEdgesInBatches(batch, selected, all)
		if err != nil || kept {
			return inbound, err
		}
	}
	inbound := make(map[string][]beads.Dep, len(rows))
	for _, b := range rows {
		up, err := source.DepList(b.ID, "up")
		if err != nil {
			return nil, fmt.Errorf("listing dependents of %s: %w", b.ID, err)
		}
		inbound[b.ID] = up
	}
	return inbound, nil
}

// infraPurgeInboundEdgesInBatches reads the outbound edges of every row of
// all, infraPurgeBatchChunk rows at a time, and keeps the edges into a
// selected row. It reports false, with no map, when the answer cannot stand
// for the store's edges: the store cannot batch, or a batch answered nothing.
// BdStore answers a batch bd refuses as "not found" (one anchor bd cannot
// resolve is enough) with an empty map and no error, whatever edges the
// batch's other anchors carry. These are the edges `bd delete --force` would
// rewrite the neighbors over, so such an answer is set aside, as
// sourceDepReader.prefetch sets it aside, and with it every batch before it:
// a partial answer would read the edges it lost as none. The price is
// prefetch's too: on a store that omits edge-free anchors, a batch of rows
// with no edges sends the run to the per-row reads.
func infraPurgeInboundEdgesInBatches(batch beads.DependencyBatchLister, selected map[string]bool, all []beads.Bead) (map[string][]beads.Dep, bool, error) {
	inbound := map[string][]beads.Dep{}
	for start := 0; start < len(all); start += infraPurgeBatchChunk {
		anchors := sourceAnchorIDs(all[start:min(start+infraPurgeBatchChunk, len(all))])
		edges, err := batch.DepListBatch(anchors)
		if errors.Is(err, beads.ErrDepListBatchUnsupported) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("reading the work store's edges in batches: %w", err)
		}
		if len(edges) == 0 {
			return nil, false, nil
		}
		for _, anchor := range anchors {
			for _, d := range edges[anchor] {
				if selected[d.DependsOnID] {
					inbound[d.DependsOnID] = append(inbound[d.DependsOnID], d)
				}
			}
		}
	}
	return inbound, true, nil
}

// infraPurgeBackupEdge records one edge of a row of class, with its payload
// when the store can report one. A store with no payload read at all (bd's CLI
// provider, exec) cannot say whether an edge carries one, which is not the
// same as carrying none: BdStore.ApplyGraphPlan writes payloads into bd. Its
// edges are recorded without one only for a class whose edges never carry a
// payload (infraPurgePayloadFreeClasses); for any other class the run
// refuses, rather than remove a payload no backup holds. A store that has the
// read and cannot answer it now (a proxied store whose native leaf stood
// down) refuses the run, as every unanswerable read does. The read is decided
// on the store under the policy wrapper (infraPurgeStores.underPolicy).
func infraPurgeBackupEdge(stores *infraPurgeStores, class coordclass.Class, d beads.Dep) (infraBackupEdge, error) {
	edge := infraBackupEdge{IssueID: d.IssueID, DependsOnID: d.DependsOnID, Type: d.Type}
	reader, ok := beads.As[beads.DepMetadataReader](stores.underPolicy)
	if !ok {
		if !infraPurgePayloadFreeClasses[class] {
			return infraBackupEdge{}, fmt.Errorf("the work store %s cannot report edge payloads, and the edge %s of a %s-class row can carry one, which the backup would then not hold", stores.engine, edge, class)
		}
		return edge, nil
	}
	payload, err := infraReadEdgePayloadFrom(reader, d.IssueID, d.DependsOnID)
	if err != nil {
		return infraBackupEdge{}, err
	}
	edge.Payload, edge.Carried = payload.Payload, payload.Carried
	return edge, nil
}

// infraPurgeReadEdgePayload reads the payload on one edge on the store under
// the policy wrapper, refusing a store that has no payload read as
// infraReadEdgePayload does.
func infraPurgeReadEdgePayload(stores *infraPurgeStores, issueID, dependsOnID string) (infraEdgePayload, error) {
	reader, ok := beads.As[beads.DepMetadataReader](stores.underPolicy)
	if !ok {
		return infraEdgePayload{}, infraEdgePayloadUnanswerable(stores.engine, issueID, dependsOnID)
	}
	return infraReadEdgePayloadFrom(reader, issueID, dependsOnID)
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

// infraPurgeResumeRefusal says why the interrupted run pending cannot resume
// with entries, the run's rows selected now (infraPurgeRowsOfRun), or returns
// nil when it can: each must be covered by the run's backup
// (infraPurgeBackupCovers), and they must be every row of the run the work
// store still holds (infraPurgeLeftBehindRefusal). gone are the run's rows the
// work store no longer holds.
func infraPurgeResumeRefusal(pending infraPurgeLedgerRecord, gone []infraPurgeRowKey, entries []infraBackupEntry) error {
	changed, err := infraPurgeBackupCovers(pending.Backup, entries)
	if err != nil {
		return fmt.Errorf("the interrupted purge run %s: %w. Nothing was written", pending.Run, err)
	}
	if len(changed) > 0 {
		return fmt.Errorf("%d row(s) of the interrupted purge run %s changed since it backed them up (%s are not in %s as the work store holds them now), so this run will not delete them over that backup. Nothing was written. Close that run first: once its rows and edges are all back, --%s closes it and names anything missing; --%s %s restores it, and refuses a row whose fields changed",
			len(changed), pending.Run, infraStrandedIDList(changed), pending.Backup, strandedAbandonPurgeFlag, strandedRestorePurgeFlag, pending.Backup)
	}
	return infraPurgeLeftBehindRefusal(pending, gone, entries)
}

// infraPurgeRowsOfRun splits rows into those the run backed up, matched to its
// keys run by id and created_at second, and the rest: a row stranded since the
// run's backup, or a new bead bd minted under an id the run deleted.
func infraPurgeRowsOfRun(rows []beads.Bead, run []infraPurgeRowKey) (of, others []beads.Bead) {
	keys := make(map[infraPurgeRowKey]bool, len(run))
	for _, key := range run {
		keys[infraPurgeSecondKey(key.ID, key.CreatedAt)] = true
	}
	for _, b := range rows {
		if keys[infraPurgeSecondKey(b.ID, b.CreatedAt)] {
			of = append(of, b)
		} else {
			others = append(others, b)
		}
	}
	return of, others
}

// infraPurgeSecondKey is the key of the row id created at, at the work
// store's second granularity, comparable as a map key.
func infraPurgeSecondKey(id string, at time.Time) infraPurgeRowKey {
	return infraPurgeRowKey{ID: id, CreatedAt: at.UTC().Truncate(time.Second)}
}

// infraPurgeLeftBehindRefusal says why the interrupted run pending cannot be
// closed while the work store still holds a row of it that entries, the rows
// this run deletes, leave out, or returns nil when there is none. Such a row
// left the selection after the backup (it was recovered into the binding, or
// is no longer stranded). The run removed every backed-up edge before any row,
// so the row may be without its edges, and once the run is closed nothing
// points at the backup that is their only copy. gone are the run's rows the
// work store no longer holds.
func infraPurgeLeftBehindRefusal(pending infraPurgeLedgerRecord, gone []infraPurgeRowKey, entries []infraBackupEntry) error {
	settled := make(map[string]bool, len(gone)+len(entries))
	for _, key := range gone {
		settled[key.ID] = true
	}
	for _, entry := range entries {
		settled[entry.Bead.ID] = true
	}
	var left []string
	for _, key := range pending.Rows {
		if !settled[key.ID] {
			left = append(left, key.ID)
		}
	}
	if len(left) == 0 {
		return nil
	}
	sort.Strings(left)
	return fmt.Errorf("the interrupted purge run %s backed up %d row(s) that are still in the work store and are not selected now (%s), so finishing the run would close it without them, and its edge removal may have left them without the edges only its backup %s holds. Nothing was written. Restore the run with --%s %s, which puts back its rows and edges and closes it, then purge again; or, if its rows and edges are all back, close it with --%s",
		pending.Run, len(left), infraStrandedIDList(left), pending.Backup, strandedRestorePurgeFlag, pending.Backup, strandedAbandonPurgeFlag)
}

// infraPurgeBackupCovers returns the ids of entries the backup at path does
// not cover. An entry is covered when the backup holds its row and every edge
// it has now: a run interrupted after the delete removed the edges and before
// it removed the rows leaves rows with fewer edges than their backup, and that
// is still the backup to resume from. None means a resume may delete from that
// backup.
func infraPurgeBackupCovers(path string, entries []infraBackupEntry) ([]string, error) {
	records, err := readInfraPurgeBackup(path)
	if err != nil {
		return nil, err
	}
	held := make(map[string]infraBackupEntry, len(records))
	for _, record := range records {
		held[record.Entry.Bead.ID] = record.Entry
	}
	var missing []string
	for _, entry := range entries {
		backed, ok := held[entry.Bead.ID]
		if !ok || !infraPurgeEntryCovers(backed, entry) {
			missing = append(missing, entry.Bead.ID)
		}
	}
	sort.Strings(missing)
	return missing, nil
}

// infraPurgeEntryCovers reports whether backed, a backup entry, holds now's
// row and a superset of its edges, each ending at the bead it ended at then
// (infraPurgeEndpointsSubset). The rows must be the same in every field but
// those a store derives from the row's edges (infraPurgeRowWithoutEdgeState),
// because the run's own edge removal changes them: bd and native Dolt list a
// row with its outbound edges, and bd also reports is_blocked and a parent
// read off a parent-child edge.
func infraPurgeEntryCovers(backed, now infraBackupEntry) bool {
	if backed.Class != now.Class || !infraPurgeEdgesSubset(now.Deps, backed.Deps) || !infraPurgeEdgesSubset(now.Dependents, backed.Dependents) ||
		!infraPurgeEndpointsSubset(now.Endpoints, backed.Endpoints) {
		return false
	}
	wantRow, err := json.Marshal(infraPurgeRowWithoutEdgeState(backed.Bead, backed.Deps))
	if err != nil {
		return false
	}
	gotRow, err := json.Marshal(infraPurgeRowWithoutEdgeState(now.Bead, backed.Deps))
	return err == nil && bytes.Equal(wantRow, gotRow)
}

// infraPurgeRowWithoutEdgeState is row without the fields a store derives
// from its edges: the inline dependencies, is_blocked, and a parent set by a
// parent-child edge among edges, the row's backed-up outbound edges.
func infraPurgeRowWithoutEdgeState(row beads.Bead, edges []infraBackupEdge) beads.Bead {
	row.Dependencies = nil
	row.IsBlocked = nil
	return infraPurgeRowWithoutEdgeParent(row, edges)
}

// infraPurgeRowWithoutEdgeParent is row without a parent set by a
// parent-child edge among edges (infraPurgeEdgeSetsParent).
func infraPurgeRowWithoutEdgeParent(row beads.Bead, edges []infraBackupEdge) beads.Bead {
	for _, edge := range edges {
		if infraPurgeEdgeSetsParent(edge, row) {
			row.ParentID = ""
		}
	}
	return row
}

// infraPurgeEdgeSetsParent reports whether edge is the parent-child edge from
// row to its parent: bd keeps no parent of its own, and reads row's off that
// edge.
func infraPurgeEdgeSetsParent(edge infraBackupEdge, row beads.Bead) bool {
	return edge.Type == "parent-child" && edge.IssueID == row.ID && edge.DependsOnID == row.ParentID
}

// infraPurgeEdgesSubset reports whether every edge of sub is in super, with
// its type and payload.
func infraPurgeEdgesSubset(sub, super []infraBackupEdge) bool {
	have := make(map[infraBackupEdge]bool, len(super))
	for _, edge := range super {
		have[edge] = true
	}
	for _, edge := range sub {
		if !have[edge] {
			return false
		}
	}
	return true
}

// infraPurgeEndpointsSubset reports whether every far end of sub is in super
// as the same bead: the same id, created in the same second. An edge that now
// ends at a bead bd minted under the id since the backup, or at a row the
// backup's work store did not hold, is not the edge the backup holds, though
// its pair and type are.
func infraPurgeEndpointsSubset(sub, super []infraPurgeRowKey) bool {
	have := make(map[string]time.Time, len(super))
	for _, key := range super {
		have[key.ID] = key.CreatedAt
	}
	for _, key := range sub {
		at, ok := have[key.ID]
		if !ok || !sameCreatedSecond(at, key.CreatedAt) {
			return false
		}
	}
	return true
}

// infraPurgeKeysGone returns the keys the work store no longer holds as the
// same bead, and the ids of those it holds for a different bead now: bd minted
// the id again once the row was deleted.
func infraPurgeKeysGone(source beads.WorkStore, keys []infraPurgeRowKey) ([]infraPurgeRowKey, []string, error) {
	present, reminted, err := infraPurgeKeysHeld(source, keys)
	if err != nil {
		return nil, nil, err
	}
	gone := make([]infraPurgeRowKey, 0, len(keys))
	for _, key := range keys {
		if !present[key.ID] {
			gone = append(gone, key)
		}
	}
	return gone, reminted, nil
}

// refuseInfraPurgeOfBindingRows is the check before anything is written: no
// row the binding holds as the same bead (the same id, created in the same
// second) may be deleted, whatever selected it. A row the binding holds for a
// different bead (a reused id) passes: deleting the work-store row leaves the
// binding's bead alone.
func refuseInfraPurgeOfBindingRows(binding beads.MailStore, rows []beads.Bead) error {
	same, err := infraPurgeRowsTheBindingHolds(binding, rows)
	if err != nil {
		return err
	}
	if len(same) > 0 {
		return fmt.Errorf("the binding holds %d selected row(s) as the same bead (the same id and created_at), so they are not stranded and are never purged: %s", len(same), strings.Join(same, ", "))
	}
	return nil
}

// infraPurgeRowsTheBindingHolds returns, sorted, the ids of rows the binding
// holds as the same bead: the same id, created in the same second.
func infraPurgeRowsTheBindingHolds(binding beads.MailStore, rows []beads.Bead) ([]string, error) {
	var same []string
	for _, b := range rows {
		held, present, err := readInfraHeldRow(binding, b.ID)
		if err != nil {
			return nil, err
		}
		if present && infraHeldRowIsThisBead(b, held) {
			same = append(same, b.ID)
		}
	}
	sort.Strings(same)
	return same, nil
}

// deleteInfraPurgeRows deletes the backed-up rows from the work store. Every
// recorded edge goes first: `bd delete --force` rewrites the text of every
// row still linked to a deleted one, replacing each mention of the id with
// "[deleted:<id>]", and a row with no links left is not rewritten. DepRemove
// names no type, so it runs once per recorded edge: a store that keeps two
// types on one pair removes one per call, and bd, SQLite and native Dolt, which
// keep one edge per pair, remove it on the first. Then the rows, in one batch
// where the store can and one by one otherwise.
func deleteInfraPurgeRows(source beads.WorkStore, entries []infraBackupEntry) error {
	for _, edge := range infraPurgeEdges(entries) {
		if err := source.DepRemove(edge.IssueID, edge.DependsOnID); err != nil && !errors.Is(err, beads.ErrNotFound) {
			return fmt.Errorf("removing dep %s: %w", edge, err)
		}
	}
	if err := infraPurgeAfterEdgesRemoved(); err != nil {
		return err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.Bead.ID)
	}
	if deleter, ok := beads.As[beads.BatchDeleter](source); ok {
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

// infraPurgeEdges lists every edge of entries once, outbound and inbound: an
// edge between two backed-up rows is in both rows' entries. An edge is its pair
// and its type, because a store may keep a parent-child edge beside an edge of
// another type on the same pair (MemStore and FileStore do), and each is
// removed and restored.
func infraPurgeEdges(entries []infraBackupEntry) []infraBackupEdge {
	seen := map[[3]string]bool{}
	var edges []infraBackupEdge
	for _, entry := range entries {
		for _, list := range [][]infraBackupEdge{entry.Deps, entry.Dependents} {
			for _, edge := range list {
				key := [3]string{edge.IssueID, edge.DependsOnID, edge.Type}
				if !seen[key] {
					seen[key] = true
					edges = append(edges, edge)
				}
			}
		}
	}
	return edges
}

// infraPurgeRestorableEdges splits every edge of entries, once, into those a
// restore can put back and the rest, each named with why not. An end of an
// edge is restorable when it is a row of entries, which the restore puts back
// itself, or when work, the work store's rows by id, holds under its id the
// bead the entry recorded there (infraBackupEntry.Endpoints, by id and created
// second). Any other end is not proven the bead the edge pointed at, and the
// edge is left out rather than attached to whatever holds the id now: an end
// the work store no longer holds, one it holds for a different bead (bd minted
// the id again), and one the entry recorded no bead for, which the work store
// did not hold when the backup was written, or which a backup written before
// endpoints were recorded cannot vouch for. A row whose parent-child edge is
// left out comes back without the parent that edge set, and the edge's name
// says so.
// The restore and --abandon-purge judge a backup's edges this way, and the
// purge names up front every edge no restore could put back.
func infraPurgeRestorableEdges(entries []infraBackupEntry, work map[string]beads.Bead) ([]infraBackupEdge, []infraPurgeLeftOutEdge) {
	backed := make(map[string]bool, len(entries))
	for _, entry := range entries {
		backed[entry.Bead.ID] = true
	}
	seen := map[[3]string]bool{}
	var edges []infraBackupEdge
	var left []infraPurgeLeftOutEdge
	for _, entry := range entries {
		recorded := make(map[string]time.Time, len(entry.Endpoints))
		for _, key := range entry.Endpoints {
			recorded[key.ID] = key.CreatedAt
		}
		for _, list := range [][]infraBackupEdge{entry.Deps, entry.Dependents} {
			for _, edge := range list {
				key := [3]string{edge.IssueID, edge.DependsOnID, edge.Type}
				if seen[key] {
					continue
				}
				seen[key] = true
				if why := infraPurgeEdgeEndUnproven(edge, backed, recorded, work); why != "" {
					if infraPurgeEdgeSetsParent(edge, entry.Bead) {
						why += fmt.Sprintf("; %s comes back without this parent", entry.Bead.ID)
					}
					left = append(left, infraPurgeLeftOutEdge{edge: edge, why: why})
					continue
				}
				edges = append(edges, edge)
			}
		}
	}
	sort.Slice(left, func(i, j int) bool { return left[i].String() < left[j].String() })
	return edges, left
}

// infraPurgeLeftOutEdge is a backed-up edge no restore puts back
// (infraPurgeRestorableEdges), with why.
type infraPurgeLeftOutEdge struct {
	edge infraBackupEdge
	why  string
}

func (l infraPurgeLeftOutEdge) String() string {
	return fmt.Sprintf("%s (%s)", l.edge, l.why)
}

// infraPurgeLeftOutList names every edge of left with why it is left out, for
// one line of output.
func infraPurgeLeftOutList(left []infraPurgeLeftOutEdge) string {
	names := make([]string, 0, len(left))
	for _, l := range left {
		names = append(names, l.String())
	}
	return strings.Join(names, "; ")
}

// infraPurgeEdgeEndUnproven names why an end of edge is not proven the bead it
// was when the edge was backed up (infraPurgeRestorableEdges), or returns ""
// when both ends are. recorded is the backup entry's far ends by id.
func infraPurgeEdgeEndUnproven(edge infraBackupEdge, backed map[string]bool, recorded map[string]time.Time, work map[string]beads.Bead) string {
	for _, end := range []string{edge.IssueID, edge.DependsOnID} {
		if backed[end] {
			continue
		}
		at, known := recorded[end]
		got, held := work[end]
		switch {
		case !held && known:
			return fmt.Sprintf("no row %s: the bead created %s that it pointed at is gone", end, at.UTC().Format(time.RFC3339))
		case !held:
			return fmt.Sprintf("no row %s", end)
		case !known:
			return fmt.Sprintf("the backup records no bead under %s, so nothing proves the row the work store holds under it now is the one the edge pointed at", end)
		case !sameCreatedSecond(at, got.CreatedAt):
			return fmt.Sprintf("%s is a different bead now, created %s: bd minted the id again since the edge pointed at the one created %s", end, got.CreatedAt.UTC().Format(time.RFC3339), at.UTC().Format(time.RFC3339))
		}
	}
	return ""
}

// infraPurgeRecordEntries is the backup entries records carry.
func infraPurgeRecordEntries(records []infraRecoverChoiceRecord) []infraBackupEntry {
	entries := make([]infraBackupEntry, 0, len(records))
	for _, record := range records {
		entries = append(entries, record.Entry)
	}
	return entries
}

// infraPurgeEdgesNotBack names the edges of the backup at path that a restore
// would put back (infraPurgeRestorableEdges) and that the work store does not
// hold as backed up (infraPurgeEdgeDifference). A run interrupted after its
// edge removal leaves exactly those, with the backup their only copy. An edge
// whose far end the work store no longer holds, or holds for a different bead,
// could not be put back, so it is not named. --abandon-purge asks only once
// every row of the run is in the work store.
func infraPurgeEdgesNotBack(stores *infraPurgeStores, path string) ([]string, error) {
	records, err := readInfraPurgeBackup(path)
	if err != nil {
		return nil, err
	}
	rows, err := stores.work.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return nil, fmt.Errorf("listing work store: %w", err)
	}
	work := make(map[string]beads.Bead, len(rows))
	for _, b := range rows {
		work[b.ID] = b
	}
	edges, _ := infraPurgeRestorableEdges(infraPurgeRecordEntries(records), work)
	var missing []string
	for _, edge := range edges {
		diff, err := infraPurgeEdgeDifference(stores, edge)
		if err != nil {
			return nil, fmt.Errorf("reading edge %s: %w", edge, err)
		}
		if diff != "" {
			missing = append(missing, fmt.Sprintf("%s (%s)", edge, diff))
		}
	}
	sort.Strings(missing)
	return missing, nil
}

// infraPurgeEdgeDifference names how the work store does not hold edge as the
// backup records it, or returns "" when it does: the pair with its type and,
// when the backup carried a payload, that payload. An edge back without its
// payload is not the edge the purge removed: on a store that keeps payloads
// (SQLite, native Dolt) a waits-for gate means something else without it. The
// payload is read on the store under the policy wrapper, where the backup read
// it (infraPurgeBackupEdge).
func infraPurgeEdgeDifference(stores *infraPurgeStores, edge infraBackupEdge) (string, error) {
	deps, err := stores.work.DepList(edge.IssueID, "down")
	if err != nil {
		return "", err
	}
	held := false
	for _, d := range deps {
		if d.DependsOnID == edge.DependsOnID && (d.Type == edge.Type || (d.Type == "" && edge.Type == "blocks") || (edge.Type == "" && d.Type == "blocks")) {
			held = true
			break
		}
	}
	if !held {
		return "missing", nil
	}
	if !edge.Carried {
		return "", nil
	}
	got, err := infraPurgeReadEdgePayload(stores, edge.IssueID, edge.DependsOnID)
	if err != nil {
		return "", err
	}
	if want := (infraEdgePayload{Payload: edge.Payload, Carried: true}); got != want {
		return fmt.Sprintf("carries %s, backed up carrying %s", infraFormatEdgePayload(got), infraFormatEdgePayload(want)), nil
	}
	return "", nil
}

// infraPurgeRowsStillPresent re-reads the work store and returns the ids of
// keys it still holds as the same bead.
func infraPurgeRowsStillPresent(source beads.WorkStore, keys []infraPurgeRowKey) ([]string, error) {
	present, _, err := infraPurgeKeysHeld(source, keys)
	if err != nil {
		return nil, err
	}
	return sortedMapKeys(present), nil
}

// infraPurgeKeysHeld re-reads the work store and returns the ids of keys it
// holds as the same bead (the same id, created in the same second), and,
// sorted, the ids it holds for a different bead.
func infraPurgeKeysHeld(source beads.WorkStore, keys []infraPurgeRowKey) (map[string]bool, []string, error) {
	rows, err := source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return nil, nil, err
	}
	want := make(map[string]time.Time, len(keys))
	for _, key := range keys {
		want[key.ID] = key.CreatedAt
	}
	present := map[string]bool{}
	var reminted []string
	for _, b := range rows {
		at, ok := want[b.ID]
		if !ok {
			continue
		}
		if sameCreatedSecond(at, b.CreatedAt) {
			present[b.ID] = true
		} else {
			reminted = append(reminted, b.ID)
		}
	}
	sort.Strings(reminted)
	return present, reminted, nil
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

// infraPurgeNetRows replays the ledger: a purged record adds its rows, and a
// restored or abandoned record removes them, because --abandon-purge closes a
// run only once every row of it is back in the work store (a finished run its
// restore left open included). What is left is every row a purge deleted and
// nothing has put back.
func infraPurgeNetRows(ledger []infraPurgeLedgerRecord) map[infraPurgeRowKey]string {
	net := map[infraPurgeRowKey]string{}
	for _, record := range ledger {
		for _, row := range record.Rows {
			key := infraPurgeSecondKey(row.ID, row.CreatedAt)
			switch record.Event {
			case infraPurgeEventPurged:
				net[key] = record.Backup
			case infraPurgeEventRestored, infraPurgeEventAbandoned:
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
		fmt.Fprintf(stdout, "  purge: INTERRUPTED (%s)\nblocking invariant: %s. Until then recover-stranded refuses\n", //nolint:errcheck // best-effort stdout
			infraPurgePendingSummary(last), infraPurgeRunRemedy(last))
		return false
	}
	return true
}
