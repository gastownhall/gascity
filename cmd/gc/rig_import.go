package main

// `gc storage migrate --from-rigs`: move one rig's graph and infrastructure
// rows into the city binding (S4b).
//
// Per rig, independently, and resumable at every step:
//
//  1. Census: list the rig store (both tiers, closed included) and read its
//     edges once; plan the import (rig_import_plan.go). A refusal stops this
//     rig only and writes nothing.
//  2. Prepare: delete binding rows this rig's interrupted attempt stamped
//     (gc.infra_migrated_from=rig:<rig>) and its manifest does not record.
//  3. Copy rows with their ids, the import stamp on every row (wisps and
//     steps included) and gc.root_store_ref=rig:<rig> on any row without
//     one; copy edges inside the moved set; mint S3b gates.
//  4. Verify against a reopened binding: the rig's stamped population equals
//     the manifest's rows plus this run's, field-equal, with the planned
//     edges.
//  5. Write the manifest (S1's (id, created_at) format, plus #gc:scope,
//     #gc:scope-annotated and #gc:gate), then the marker.
//  6. Raise bd's min_hash_length in the rig store, so a short id minted
//     after the clear can never re-mint an imported one.
//  7. Back up, prove the backup, mark the cleared note pending, delete the
//     rig copies in batches, mark the note complete. No edge is re-added.
//
// A completed run on an unchanged rig prints "nothing to import". A rig with
// no infrastructure row at all gets a clean record. Both retire the rig's
// relic legs (rigGraphRelicFree).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// storageFromRigsFlag names the migrate verb's third source: the city's rig
// stores.
const storageFromRigsFlag = "from-rigs"

// storageRigFlag narrows --from-rigs to named rigs.
const storageRigFlag = "rig"

// rigImportInstruction is the command that imports one rig.
func rigImportInstruction(rig string) string {
	return "gc storage migrate --" + storageFromRigsFlag + " --" + storageRigFlag + " " + rig + " --" + storageFleetStoppedFlag
}

// rigImportAllInstruction is the command that imports every rig.
func rigImportAllInstruction() string {
	return "gc storage migrate --" + storageFromRigsFlag + " --" + storageFleetStoppedFlag
}

// rigImportDeleteChunk bounds one batched delete. The design measured about
// 30 ms per row for one Dolt commit per row and 1.0 s for 1,000 rows deleted
// in chunks of 250. A var so a test can force several chunks with few rows.
var rigImportDeleteChunk = 250

// rigImportLocation is where one rig's import records live: the manifest,
// marker, clean record and backup beside the city's infra.migrated at the
// binding root, and the cleared note in the city's .gc.
type rigImportLocation struct {
	Rig         string
	BindingRoot string
	CityPath    string
}

func newRigImportLocation(target infraBindingTarget, cityPath, rig string) rigImportLocation {
	return rigImportLocation{Rig: rig, BindingRoot: target.Root, CityPath: cityPath}
}

func (l rigImportLocation) MarkerPath() string {
	return filepath.Join(l.BindingRoot, rigGraphImportMarkerName(l.Rig))
}

func (l rigImportLocation) ManifestPath() string {
	return filepath.Join(l.BindingRoot, rigGraphImportMarkerName(l.Rig)+".beads")
}

func (l rigImportLocation) CleanPath() string {
	return filepath.Join(l.BindingRoot, rigGraphCleanMarkerName(l.Rig))
}

func (l rigImportLocation) BackupPath() string {
	return filepath.Join(l.BindingRoot, "infra.rig."+l.Rig+".retained-source.jsonl")
}

func (l rigImportLocation) NotePath() string { return rigGraphClearedNotePath(l.CityPath, l.Rig) }

// InFlightPath is the record an import writes before it touches the binding
// and removes once its marker is written. While it exists the binding may
// hold a half-finished copy (rows without their edges), so boot and every
// CLI route refuse (rigBootRefusal).
func (l rigImportLocation) InFlightPath() string {
	return filepath.Join(l.BindingRoot, rigGraphInFlightName(l.Rig))
}

// rigRecordStorePrefix starts the line of a rig record that names the store
// the record speaks for, so a new store under a reused rig name is not taken
// for the one that was imported or censused clean.
const rigRecordStorePrefix = "store "

// writeRecord writes one of the rig's records, naming the rig store it
// speaks for (storeRoot, the rig's canonical scope root).
func (l rigImportLocation) writeRecord(path, line, storeRoot string) error {
	return writeRigImportRecord(path, line+"\n"+rigRecordStorePrefix+storeRoot)
}

// rigRecordStore returns the store a rig record names, and whether it names
// one.
func rigRecordStore(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if root, ok := strings.CutPrefix(strings.TrimSpace(line), rigRecordStorePrefix); ok && root != "" {
			return root, true
		}
	}
	return "", false
}

// rigRecordSpeaksFor reports whether the record at path exists and names
// storeRoot.
func rigRecordSpeaksFor(path, storeRoot string) bool {
	root, ok := rigRecordStore(path)
	return ok && samePath(root, storeRoot)
}

// rigStoreRoot is the canonical scope root a rig's records name.
func rigStoreRoot(cityPath string, rig config.Rig) string {
	return resolveStoreScopeRoot(cityPath, rig.Path)
}

// readManifest reads the rig's manifest. It is trusted only behind the
// marker: before it, an interrupted attempt is re-copied.
func (l rigImportLocation) readManifest() (*infraCopyManifest, bool, error) {
	if !fileExists(l.MarkerPath()) {
		return nil, false, nil
	}
	m, recorded, err := readInfraCopyManifestRecordAt(l.ManifestPath(), l.MarkerPath())
	if err != nil {
		return nil, true, err
	}
	if !recorded {
		return nil, true, fmt.Errorf("rig %s carries an import marker %s with no manifest %s, so nothing says which rows the import carried. Restore the manifest from a backup of the binding root. Do not remove the marker: the binding may hold the only copy of the rows the import moved", l.Rig, l.MarkerPath(), l.ManifestPath())
	}
	return &m, true, nil
}

// writeRecord writes a one-line record file atomically.
func writeRigImportRecord(path, line string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := fmt.Fprintln(tmp, line); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := infraMigrationRename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	syncInfraDir(dir)
	return nil
}

// rigImportCrashPoint is a test seam: called at every step boundary of an
// import with the step's name; an error stops the import there, as a crash
// would.
var rigImportCrashPoint = func(string) error { return nil }

// rigImportStoreOpener opens one rig's store for the importer and the census.
// Tests override it with real file, bd or Dolt stores.
var rigImportStoreOpener = func(cityPath string, rig config.Rig) (beads.Store, error) {
	return openStorageScopeStore(rig.Path, cityPath)
}

// rigImportBindingOpener opens the city binding read-write for the importer.
var rigImportBindingOpener = openInfraDestination

// rigHashLengthConfig is the bd config surface the id reservation needs.
type rigHashLengthConfig interface {
	ConfigGet(key string) (string, error)
	ConfigSet(key, value string) error
}

// rigImportHashConfigOpener returns the rig's bd config surface, or nil for a
// rig whose store mints from a monotonic sequence (the file store), which a
// clear cannot rewind and so needs no reservation.
var rigImportHashConfigOpener = defaultRigImportHashConfigOpener

// defaultRigImportHashConfigOpener is the production rigImportHashConfigOpener.
func defaultRigImportHashConfigOpener(cityPath string, cfg *config.City, rig config.Rig) (rigHashLengthConfig, error) {
	root := resolveStoreScopeRoot(cityPath, rig.Path)
	switch provider := authoritativeBeadsProviderForScope(root, cityPath); {
	case provider == "file":
		return nil, nil
	case strings.HasPrefix(provider, "exec:"):
		return nil, fmt.Errorf("rig %s is served by provider %s, whose id minting this build cannot configure", rig.Name, provider)
	}
	return bdStoreForRig(root, cityPath, cfg), nil
}

const (
	bdMinHashLengthKey = "min_hash_length"
	bdMaxHashLengthKey = "max_hash_length"
	// bdDefaultMaxHashLength is bd's documented default ceiling for adaptive
	// ids when max_hash_length is unset (docs/reference/configuration.md).
	bdDefaultMaxHashLength = 8
)

// rigImportEnv is what one run of the importer needs.
type rigImportEnv struct {
	CityPath string
	CityName string
	Cfg      *config.City
	Target   infraBindingTarget
	Stdout   io.Writer
	Stderr   io.Writer
	// Started is the run's start, recorded in the marker.
	Started time.Time
}

// rigImportOutcome names what an import did for one rig.
type rigImportOutcome string

const (
	rigImportImported rigImportOutcome = "imported"
	rigImportClean    rigImportOutcome = "clean"
	rigImportNothing  rigImportOutcome = "nothing to import"
	rigImportSkipped  rigImportOutcome = "skipped"
	rigImportRefused  rigImportOutcome = "refused"
	rigImportFailed   rigImportOutcome = "failed"
)

// rigImportResult is one rig's import.
type rigImportResult struct {
	Rig        string
	Outcome    rigImportOutcome
	Why        string
	Copied     int
	Edges      int
	Gates      int
	Annotated  int
	Recorded   int
	Cleared    int
	OpenRouted int
	HashFloor  int
	Refusals   []rigImportRefusal
	Err        error
	Elapsed    time.Duration
}

func (r rigImportResult) ok() bool {
	switch r.Outcome {
	case rigImportImported, rigImportClean, rigImportNothing, rigImportSkipped:
		return true
	default:
		return false
	}
}

// importRig runs one rig's import end to end.
func importRig(env rigImportEnv, rig config.Rig) (result rigImportResult) {
	started := time.Now()
	result = rigImportResult{Rig: rig.Name}
	defer func() { result.Elapsed = time.Since(started) }()
	fail := func(err error) rigImportResult {
		result.Outcome, result.Err = rigImportFailed, err
		return result
	}
	if !rigNameIsFileSafe(rig.Name) {
		result.Outcome, result.Err = rigImportRefused, fmt.Errorf("rig name %q cannot name its import records", rig.Name)
		return result
	}
	if strings.TrimSpace(rig.Path) == "" {
		result.Outcome, result.Why = rigImportSkipped, "the rig is unbound and has no store of its own"
		return result
	}
	if samePath(resolveStoreScopeRoot(env.CityPath, rig.Path), resolveStoreScopeRoot(env.CityPath, env.CityPath)) {
		result.Outcome, result.Why = rigImportSkipped, "the rig's store is the city work store, which `"+storageMigrationCommand+"` moves"
		return result
	}
	loc := newRigImportLocation(env.Target, env.CityPath, rig.Name)
	storeRoot := rigStoreRoot(env.CityPath, rig)
	refuse := func(err error) rigImportResult {
		result.Outcome, result.Err = rigImportRefused, err
		return result
	}
	if err := loc.storeIdentityRefusal(storeRoot); err != nil {
		return refuse(err)
	}
	manifest, marked, err := loc.readManifest()
	if err != nil {
		return fail(err)
	}
	note, notePresent, err := readInfraClearedNoteAt(loc.NotePath())
	if err != nil {
		return fail(err)
	}
	if notePresent && !marked {
		return refuse(fmt.Errorf("rig %s has a cleared note %s but no import marker %s: rows may already have been removed from the rig, so the binding may hold their only copy. Restore the marker and manifest from a backup of the binding root, then re-run; nothing was written", rig.Name, loc.NotePath(), loc.MarkerPath()))
	}
	resuming := notePresent && !note.Complete

	if err := rigImportSharedStoreRefusal(env, rig); err != nil {
		return refuse(err)
	}
	hashConf, err := rigImportHashConfigOpener(env.CityPath, env.Cfg, rig)
	if err != nil {
		return refuse(fmt.Errorf("rig %s: %w; nothing was written", rig.Name, err))
	}

	rigStore, err := rigImportStoreOpener(env.CityPath, rig)
	if err != nil {
		return refuse(fmt.Errorf("opening rig %s's store: %w (an unreachable rig blocks only itself; it stays awaiting import and its relic legs keep it readable)", rig.Name, err))
	}
	defer closeBeadStoreHandle(rigStore) //nolint:errcheck // best-effort close
	snap, err := readRigSnapshot(rigStore)
	if err != nil {
		return refuse(fmt.Errorf("rig %s: %w", rig.Name, err))
	}
	binding, err := rigImportBindingOpener(env.Target)
	if err != nil {
		return fail(fmt.Errorf("opening the city binding: %w", err))
	}
	bindingOpen := true
	closeBinding := func() {
		if bindingOpen {
			_ = closeBeadStoreHandle(binding)
			bindingOpen = false
		}
	}
	defer closeBinding()

	ctx, err := rigImportPlanContext(env, rig, snap, manifest, binding)
	if err != nil {
		return fail(err)
	}
	plan := planRigImportReadingPayloads(ctx, rigStore)
	result.OpenRouted = plan.OpenRouted
	if len(plan.Refusals) > 0 {
		result.Outcome, result.Refusals = rigImportRefused, plan.Refusals
		return result
	}
	if len(plan.Move) == 0 && len(plan.Retained) == 0 && !resuming {
		if manifest != nil {
			// An interrupted incremental attempt's copies whose rows have
			// since left the rig: the binding may hold their only copy,
			// without their edges, and only the in-flight record keeps boot
			// from serving them.
			unrecorded, err := rigUnrecordedImportRows(binding, rig.Name, manifest)
			closeBinding()
			if err != nil {
				return fail(err)
			}
			if len(unrecorded) > 0 {
				return refuse(fmt.Errorf("rig %s: the binding holds %d row(s) stamped as this rig's import that its manifest does not record, and the rig no longer holds them (an interrupted incremental import whose rows have since left the rig), so the binding may hold their only copy, without their edges: %s. Nothing was removed and the in-flight record %s stays. Restore those rows to the rig store from a backup and re-run, or remove each binding copy once it is confirmed unwanted", rig.Name, len(unrecorded), infraStrandedIDList(unrecorded), loc.InFlightPath()))
			}
			_ = os.Remove(loc.InFlightPath())
			result.Outcome = rigImportNothing
			if notePresent && note.Complete {
				return result
			}
			// A marker whose clear never started: nothing left to clear.
			if err := writeInfraClearedNoteAt(loc.NotePath(), infraClearedNote{Binding: env.Target.Binding, Database: env.Target.Database, Backup: loc.BackupPath(), Complete: true}); err != nil {
				return fail(err)
			}
			return result
		}
		closeBinding()
		if plan.Infra == 0 {
			if fileExists(loc.InFlightPath()) {
				return refuse(fmt.Errorf("rig %s holds no infrastructure row but an interrupted import's record %s is present: the binding may hold the only copy of rows it moved. Restore the rig store, or its records, then re-run", rig.Name, loc.InFlightPath()))
			}
			if err := loc.writeRecord(loc.CleanPath(), fmt.Sprintf("rig %s holds no infrastructure bead (census %s)", rig.Name, time.Now().UTC().Format(time.RFC3339)), storeRoot); err != nil {
				return fail(fmt.Errorf("recording rig %s clean: %w", rig.Name, err))
			}
			result.Outcome = rigImportClean
			return result
		}
		// Every infrastructure row is already carried as the city's (a rig
		// on the city's database): nothing of this rig's to move.
		result.Outcome, result.Why = rigImportNothing, fmt.Sprintf("the binding already holds all %d of the rig's infrastructure row(s) as the city's copy", len(plan.Shared))
		return result
	}

	if len(plan.Move) > 0 {
		// The in-flight record goes down before the binding is touched: from
		// here until the marker the binding may hold rows without their
		// edges, and boot and every CLI route refuse rather than serve them.
		if !fileExists(loc.InFlightPath()) {
			if err := loc.writeRecord(loc.InFlightPath(), fmt.Sprintf("rig %s import in flight since %s; finish it with `%s`", rig.Name, time.Now().UTC().Format(time.RFC3339), rigImportInstruction(rig.Name)), storeRoot); err != nil {
				return fail(fmt.Errorf("recording rig %s's import in flight: %w", rig.Name, err))
			}
		}
		if err := rigImportPrepare(loc, binding, snap, manifest, plan); err != nil {
			var refusal *rigImportPrepareRefusal
			if errors.As(err, &refusal) {
				return refuse(err)
			}
			return fail(err)
		}
		if err := rigImportCrashPoint("prepared"); err != nil {
			return fail(err)
		}
		if err := rigImportCopy(loc, binding, rigStore, plan, &result); err != nil {
			return fail(err)
		}
		closeBinding()
		if err := rigImportVerify(env, loc, manifest, plan); err != nil {
			return fail(err)
		}
		if err := rigImportCrashPoint("verified"); err != nil {
			return fail(err)
		}
		if err := rigImportWriteManifest(loc, env.Target, manifest, plan); err != nil {
			return fail(err)
		}
		if err := rigImportCrashPoint("manifest"); err != nil {
			return fail(err)
		}
		if !fileExists(loc.MarkerPath()) {
			if err := loc.writeRecord(loc.MarkerPath(), fmt.Sprintf("rig %s graph rows imported into binding %s (%s) %s", rig.Name, env.Target.Binding, env.Target.Database, time.Now().UTC().Format(time.RFC3339)), storeRoot); err != nil {
				return fail(fmt.Errorf("writing rig %s's import marker: %w", rig.Name, err))
			}
		}
		if fileExists(loc.CleanPath()) {
			// A rig recorded clean that acquired rows since: the import record
			// now speaks for it.
			_ = os.Remove(loc.CleanPath())
		}
		if err := rigImportCrashPoint("marker"); err != nil {
			return fail(err)
		}
	} else {
		closeBinding()
	}
	// From the marker until the clear completes, the rig and the binding
	// may both hold a row (twins). The cleared note goes pending before the
	// in-flight record comes off, so boot keeps refusing across the handover.
	if fileExists(loc.InFlightPath()) {
		if cur, present, err := readInfraClearedNoteAt(loc.NotePath()); err != nil {
			return fail(err)
		} else if !present || cur.Complete {
			note = infraClearedNote{Binding: env.Target.Binding, Database: env.Target.Database, Backup: loc.BackupPath()}
			if err := writeInfraClearedNoteAt(loc.NotePath(), note); err != nil {
				return fail(err)
			}
			resuming = true
		}
		if err := os.Remove(loc.InFlightPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(fmt.Errorf("removing rig %s's in-flight record: %w", rig.Name, err))
		}
	}

	reread, _, err := loc.readManifest()
	if err != nil {
		return fail(err)
	}
	if reread == nil {
		return fail(fmt.Errorf("rig %s: the import marker is missing after the copy", rig.Name))
	}
	floor, err := rigImportReserveIDs(hashConf, reread)
	if err != nil {
		return fail(fmt.Errorf("raising rig %s's minimum id length before the clear: %w (nothing was removed from the rig; re-run to finish)", rig.Name, err))
	}
	result.HashFloor = floor
	if err := rigImportCrashPoint("reserved"); err != nil {
		return fail(err)
	}
	cleared, err := rigImportClear(env, loc, rigStore, snap, reread, note, resuming, plan)
	result.Cleared = cleared
	if err != nil {
		var refusal *rigImportPrepareRefusal
		if errors.As(err, &refusal) {
			return refuse(err)
		}
		return fail(err)
	}
	if len(plan.Move) == 0 && len(plan.Retained) == 0 {
		result.Outcome = rigImportNothing
		return result
	}
	result.Outcome = rigImportImported
	return result
}

// rigImportPrepareRefusal is a refusal the prepare or clear step reaches:
// nothing it names was deleted.
type rigImportPrepareRefusal struct{ msg string }

func (e *rigImportPrepareRefusal) Error() string { return e.msg }

// storeIdentityRefusal refuses a rig whose records name another store: the
// rig name was reused for a new store, and the old store's import must not
// speak for it.
func (l rigImportLocation) storeIdentityRefusal(storeRoot string) error {
	for _, path := range []string{l.MarkerPath(), l.CleanPath(), l.InFlightPath()} {
		root, ok := rigRecordStore(path)
		if !fileExists(path) || (ok && samePath(root, storeRoot)) {
			continue
		}
		named := root
		if !ok {
			named = "no store"
		}
		return fmt.Errorf("rig %s's record %s names %s, but the rig now resolves to %s: the name was reused for another store. Move the old rig's records (infra.rig.%s.* beside the binding, and its cleared note) aside once its rows are accounted for, then re-run", l.Rig, path, named, storeRoot, l.Rig)
	}
	return nil
}

// rigImportPlanContext gathers what the plan reads: the binding's rows under
// the ids it asks about, every copy of an open moved row's blocker held in
// another store, and the claimant rule for shared stores.
func rigImportPlanContext(env rigImportEnv, rig config.Rig, snap rigSnapshot, manifest *infraCopyManifest, binding beads.Store) (rigPlanContext, error) {
	gen := coordclass.CurrentGeneration
	if manifest != nil {
		gen = manifest.Generation
	}
	ctx := rigPlanContext{Rig: rig.Name, CityName: env.CityName, Snap: snap, Manifest: manifest}
	held, err := beads.GetMany(binding, bindingIDsToProbe(snap, gen))
	if err != nil {
		return ctx, fmt.Errorf("reading the city binding: %w", err)
	}
	ctx.Held = held
	if ids := elsewhereCandidates(snap, gen, held); len(ids) > 0 {
		ctx.Elsewhere, ctx.ElsewhereUnread = probeElsewhere(env, rig, ids)
	}
	if env.Cfg != nil {
		gate := newForeignClaimGate(env.Cfg, beads.SessionStore{Store: binding})
		ctx.ForeignClaimant = func(assignee string) bool { return !gate.observable(assignee) }
	}
	return ctx, nil
}

// planRigImportReadingPayloads is the plan the import and `gc storage
// preflight` both run: planRigImport over the gathered context, refused as
// well when the rig store cannot read an edge payload the import needs
// (rigImportPayloadRefusal).
func planRigImportReadingPayloads(ctx rigPlanContext, rigStore beads.Store) rigImportPlan {
	plan := planRigImport(ctx)
	if refusal, refused := rigImportPayloadRefusal(rigStore, ctx.Snap, plan); refused {
		plan.Refusals = append(plan.Refusals, refusal)
	}
	return plan
}

// rigImportPayloadRefusal reads, through the rig store, the payload on every
// edge the import reads one from: each edge it copies, and each edge the
// clear's backup records, which is every edge of a row it moves or clears.
// Those reads happen after the binding is touched, where a failure stops an
// import holding its in-flight record or a pending clear (and boot with it)
// on every re-run; read here first, a failure refuses before anything is
// written.
//
// Only a read can tell. The bead policy wrapper every production open
// returns implements beads.DepMetadataReader whatever its leaf is, and so do
// the caching and proxied stores; each answers with an error when the store
// beneath it cannot read a payload (the bd CLI store, which the default
// proxied-server topology opens; DoltLite; an exec store). The probe stops at
// the first edge whose read fails.
func rigImportPayloadRefusal(rigStore beads.Store, snap rigSnapshot, plan rigImportPlan) (rigImportRefusal, bool) {
	touched := make(map[string]bool, len(plan.Move)+len(plan.Retained))
	for _, b := range plan.Move {
		touched[b.ID] = true
	}
	for _, b := range plan.Retained {
		touched[b.ID] = true
	}
	var edges []beads.Dep
	for _, b := range snap.rows {
		for _, d := range snap.down[b.ID] {
			if d.IssueID == "" {
				d.IssueID = b.ID
			}
			if touched[d.IssueID] || touched[d.DependsOnID] {
				edges = append(edges, d)
			}
		}
	}
	for _, d := range edges {
		if _, err := infraReadEdgePayload(rigStore, d.IssueID, d.DependsOnID); err != nil {
			return rigImportRefusal{
				IDs:    []string{d.IssueID, d.DependsOnID},
				Reason: fmt.Sprintf("the rig store cannot report the payload on edge %s -> %s (%v), and the import reads the payload of each of the %d edge(s) it copies or backs up: an edge copied or cleared without its payload loses it", d.IssueID, d.DependsOnID, err, len(edges)),
				Remedy: "serve the rig from a store that reads edge payloads (a native Dolt open; a proxied-server rig opens natively with GC_BEADS_PROXIED_NATIVE=1), or re-run if the read failed transiently; nothing was written",
			}, true
		}
	}
	return rigImportRefusal{}, false
}

// probeElsewhere reads ids from every store other than rig's and the
// binding: the city work store and each other bound rig. A store that cannot
// be opened or read is named in unread.
func probeElsewhere(env rigImportEnv, rig config.Rig, ids []string) (map[string][]beads.Bead, []string) {
	found := map[string][]beads.Bead{}
	var unread []string
	read := func(label string, open func() (beads.Store, error)) {
		store, err := open()
		if err != nil {
			unread = append(unread, fmt.Sprintf("%s: %v", label, err))
			return
		}
		defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close
		got, err := beads.GetMany(store, ids)
		if err != nil {
			unread = append(unread, fmt.Sprintf("%s: %v", label, err))
			return
		}
		for _, id := range ids {
			if b, ok := got[id]; ok {
				found[id] = append(found[id], b)
			}
		}
	}
	read("the city work store", func() (beads.Store, error) { return rigImportCityWorkOpener(env.CityPath) })
	if env.Cfg != nil {
		self := rigStoreRoot(env.CityPath, rig)
		for _, other := range env.Cfg.Rigs {
			if other.Name == rig.Name || strings.TrimSpace(other.Path) == "" || samePath(rigStoreRoot(env.CityPath, other), self) {
				continue
			}
			r := other
			read("rig "+r.Name, func() (beads.Store, error) { return rigImportStoreOpener(env.CityPath, r) })
		}
	}
	return found, unread
}

// rigImportCityWorkOpener opens the city work store for the blocker probe.
var rigImportCityWorkOpener = func(cityPath string) (beads.Store, error) { return openInfraMigrationSource(cityPath) }

// rigImportOtherCities lists the other cities registered on this machine.
var rigImportOtherCities = func(cityPath string) ([]string, error) {
	entries, err := otherRegisteredCities(cityPath)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	return paths, nil
}

// rigDoltIdentity is the Dolt database a scope's store lives in.
type rigDoltIdentity struct {
	// Database is the database name; empty for a scope with no
	// authoritative Dolt target (a file store).
	Database string
	// Endpoint is the normalized server address (loopback spellings fold to
	// one; a host name resolves to its sorted addresses) or the socket path.
	Endpoint string
	// Unresolved reports that the endpoint could not be resolved, so the
	// identity names a database without saying where it lives.
	Unresolved bool
	Err        error
}

// rigImportScopeDoltTarget resolves the Dolt database a scope's store lives
// in.
var rigImportScopeDoltTarget = func(cityPath, scopeRoot string) rigDoltIdentity {
	target, ok, err := canonicalScopeDoltTarget(cityPath, scopeRoot)
	if err != nil {
		// The endpoint could not be resolved (that city's Dolt is down, its
		// runtime state is gone). The database name is still the scope's own
		// pin in .beads/metadata.json: compare by it, and leave it empty,
		// which refuses, only when that cannot be read either.
		db, _, _ := contract.ReadDoltDatabase(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "metadata.json"))
		return rigDoltIdentity{Database: strings.TrimSpace(db), Unresolved: true, Err: err}
	}
	if !ok || strings.TrimSpace(target.Database) == "" {
		return rigDoltIdentity{}
	}
	id := rigDoltIdentity{Database: strings.TrimSpace(target.Database)}
	if socket := strings.TrimSpace(target.Socket); socket != "" {
		id.Endpoint = "unix:" + filepath.Clean(socket)
		return id
	}
	host, herr := normalizeDoltHost(target.Host)
	if herr != nil {
		id.Unresolved, id.Err = true, herr
		return id
	}
	id.Endpoint = host + ":" + strings.TrimSpace(target.Port)
	return id
}

// normalizeDoltHost folds the loopback spellings (localhost, 127.0.0.1, ::1,
// empty) to one and resolves any other host name to its sorted addresses,
// so two spellings of one server compare equal.
func normalizeDoltHost(host string) (string, error) {
	host = strings.Trim(strings.TrimSpace(strings.ToLower(host)), "[]")
	switch host {
	case "", "localhost", "127.0.0.1", "::1", "0.0.0.0", "::":
		// A wildcard listen address names this host too: a client
		// configured with it reaches the server over loopback.
		return "loopback", nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() {
			return "loopback", nil
		}
		return ip.String(), nil
	}
	addrs, err := rigImportLookupHost(host)
	if err == nil && len(addrs) == 0 {
		err = errors.New("no addresses")
	}
	if err != nil {
		return "", fmt.Errorf("resolving Dolt host %q: %w", host, err)
	}
	norm := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.IsLoopback() {
			norm = append(norm, "loopback")
			continue
		}
		norm = append(norm, a)
	}
	sort.Strings(norm)
	return strings.Join(norm, ","), nil
}

// rigImportLookupHost resolves a host name; a test seam.
var rigImportLookupHost = net.LookupHost

// rigImportSharedStoreRefusal refuses a rig whose store another registered
// city also uses: the same scope root, or the same Dolt database. A shared
// or company store holds rows another city owns, and is refused until S5's
// single-writer attestation exists. A registry that cannot be read refuses
// too: absence of a sharer must be shown, not assumed.
func rigImportSharedStoreRefusal(env rigImportEnv, rig config.Rig) error {
	others, err := rigImportOtherCities(env.CityPath)
	if err != nil {
		return fmt.Errorf("rig %s: reading the city registry to rule out a shared rig store: %w; nothing was written", rig.Name, err)
	}
	selfRoot := rigStoreRoot(env.CityPath, rig)
	self := rigImportScopeDoltTarget(env.CityPath, selfRoot)
	// A rig with no Dolt database (a file store) shares none. One whose
	// endpoint cannot be resolved and which pins no database name cannot be
	// told apart from any other city's Dolt rig, so it is compared, and
	// refused, rather than skipped.
	noDatabase := self.Database == "" && !self.Unresolved
	for _, otherPath := range others {
		cfg, err := loadCityConfigWithoutBuiltinPackRefresh(otherPath, io.Discard)
		if err == nil && cfg == nil {
			err = errors.New("it loaded no configuration")
		}
		if err != nil {
			return fmt.Errorf("rig %s: city %s is registered but its config cannot be read (%w), so it cannot be ruled out as sharing the rig store; nothing was written", rig.Name, otherPath, err)
		}
		for _, other := range cfg.Rigs {
			if strings.TrimSpace(other.Path) == "" {
				continue
			}
			root := rigStoreRoot(otherPath, other)
			if samePath(root, selfRoot) {
				return fmt.Errorf("rig %s's store %s is also city %s's rig %s: a shared rig store holds rows another city owns, and is refused until the single-writer attestation exists; nothing was written", rig.Name, selfRoot, otherPath, other.Name)
			}
			if noDatabase {
				continue
			}
			theirs := rigImportScopeDoltTarget(otherPath, root)
			if theirs.Database == "" && !theirs.Unresolved {
				continue
			}
			switch {
			case self.Unresolved && self.Database == "":
				return fmt.Errorf("rig %s's Dolt endpoint cannot be resolved (%w) and its .beads/metadata.json pins no dolt_database, so it cannot be ruled out as city %s's rig %s's database: start this rig's Dolt server so its endpoint resolves, or pin its database name, then re-run; nothing was written", rig.Name, self.Err, otherPath, other.Name)
			case self.Unresolved || theirs.Unresolved:
				if theirs.Database != "" && theirs.Database != self.Database {
					continue
				}
				why := theirs.Err
				if self.Unresolved {
					why = self.Err
				}
				return fmt.Errorf("rig %s's Dolt database %q may also be city %s's rig %s, and the endpoints cannot be compared (%w): start that city's Dolt server (or this rig's) so they can be, or unregister that city if it is gone; nothing was written", rig.Name, self.Database, otherPath, other.Name, why)
			case theirs.Database == self.Database && theirs.Endpoint == self.Endpoint:
				return fmt.Errorf("rig %s's Dolt database %s at %s is also city %s's rig %s: a shared rig store holds rows another city owns, and is refused until the single-writer attestation exists; nothing was written", rig.Name, self.Database, self.Endpoint, otherPath, other.Name)
			}
		}
	}
	return nil
}

// rigImportPrepare removes the binding rows an interrupted earlier attempt
// of this rig's import wrote — but only rows it can prove are that attempt's
// copies of rows the rig still holds, unchanged: same (id, created_at), and
// the binding copy field-equal to what the import writes for the rig row. A
// stamped row the rig no longer holds may be the binding's only copy, and a
// copy that differs was written to since; both refuse, naming the ids, and
// nothing is deleted.
func rigImportPrepare(loc rigImportLocation, binding beads.Store, snap rigSnapshot, manifest *infraCopyManifest, plan rigImportPlan) error {
	stale, err := binding.List(beads.ListQuery{Metadata: map[string]string{infraMigrationStampKey: rigImportStamp(loc.Rig)}, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return fmt.Errorf("listing the binding's rows from rig %s: %w", loc.Rig, err)
	}
	moving := make(map[string]bool, len(plan.Move))
	for _, b := range plan.Move {
		moving[b.ID] = true
	}
	var drop, onlyCopy, changed []string
	for _, b := range stale {
		if manifest != nil && (manifest.IDs[b.ID] || manifest.Gates[b.ID]) {
			continue
		}
		if hasLabel(b.Labels, dispatch.CrossStoreGateLabel) {
			if moving[b.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey]] {
				drop = append(drop, b.ID)
				continue
			}
			onlyCopy = append(onlyCopy, b.ID)
			continue
		}
		r, ok := snap.byID[b.ID]
		if !ok || !moving[b.ID] || !rigSameCreated(r, b) {
			onlyCopy = append(onlyCopy, b.ID)
			continue
		}
		want := rigImportRow(r, loc.Rig, nil)
		if recorded, ok := b.Metadata[beadmeta.MigratedCrossEdgesMetadataKey]; ok {
			want.Metadata[beadmeta.MigratedCrossEdgesMetadataKey] = recorded
		}
		if diff := beadCopyDifference(want, b); diff != "" {
			changed = append(changed, fmt.Sprintf("%s (%s)", b.ID, diff))
			continue
		}
		drop = append(drop, b.ID)
	}
	var reasons []string
	if len(onlyCopy) > 0 {
		sort.Strings(onlyCopy)
		reasons = append(reasons, fmt.Sprintf("the binding holds %d row(s) stamped as rig %s's import that no manifest records and the rig does not hold as the same bead, so the binding may hold their only copy: %s", len(onlyCopy), loc.Rig, infraStrandedIDList(onlyCopy)))
	}
	if len(changed) > 0 {
		sort.Strings(changed)
		reasons = append(reasons, fmt.Sprintf("%d binding cop(ies) from an interrupted import of rig %s differ from the rig rows they were copied from, so one side was written during the interruption: %s", len(changed), loc.Rig, infraStrandedIDList(changed)))
	}
	if len(reasons) > 0 {
		return &rigImportPrepareRefusal{msg: strings.Join(reasons, "; ") + ". Nothing was deleted. Reconcile each row (keep the binding copy by recording it, or remove it once the rig row is confirmed to be the bead), then re-run"}
	}
	if err := deleteInBatches(binding, drop); err != nil {
		return fmt.Errorf("clearing rig %s's interrupted attempt from the binding: %w", loc.Rig, err)
	}
	return nil
}

// rigSameCreated reports whether two rows carry the same created_at at the
// stores' second granularity (both absent counts as the same).
func rigSameCreated(a, b beads.Bead) bool {
	if a.CreatedAt.IsZero() || b.CreatedAt.IsZero() {
		return a.CreatedAt.IsZero() && b.CreatedAt.IsZero()
	}
	return sameCreatedSecond(a.CreatedAt, b.CreatedAt)
}

// rigImportCopy is phase 3: copy rows, edges and gates.
func rigImportCopy(loc rigImportLocation, binding, rigStore beads.Store, plan rigImportPlan, result *rigImportResult) error {
	creator, ok := binding.(beads.ForeignIDCreator)
	if !ok {
		return fmt.Errorf("binding store %T cannot preserve bead ids", binding)
	}
	for i, b := range plan.Move {
		if _, err := creator.CreateWithForeignID(rigImportRow(b, loc.Rig, plan.Records[b.ID])); err != nil {
			return fmt.Errorf("importing bead %s: %w", b.ID, err)
		}
		result.Copied++
		if i == len(plan.Move)/2 {
			if err := rigImportCrashPoint("copying"); err != nil {
				return err
			}
		}
	}
	result.Annotated = len(plan.Annotated)
	if err := rigImportCrashPoint("rows"); err != nil {
		return err
	}
	for _, edge := range plan.Copy {
		var err error
		if strings.HasPrefix(edge.DependsOnID, "external:") {
			err = binding.DepAdd(edge.IssueID, edge.DependsOnID, edge.Type)
		} else {
			err = infraCopyDepEdge(binding, rigStore, edge.IssueID, edge.DependsOnID, edge.Type)
		}
		if err != nil {
			return fmt.Errorf("importing dep %s -> %s: %w", edge.IssueID, edge.DependsOnID, err)
		}
		result.Edges++
	}
	for _, g := range plan.Gates {
		gate, err := createRigImportGate(binding, loc.Rig, g)
		if err != nil {
			return err
		}
		if err := binding.DepAdd(g.Owner.ID, gate.ID, "blocks"); err != nil {
			return fmt.Errorf("holding %s on its cross-store gate %s: %w", g.Owner.ID, gate.ID, err)
		}
		result.Gates++
	}
	for _, recs := range plan.Records {
		result.Recorded += len(recs)
	}
	return nil
}

// createRigImportGate mints one S3b gate for g in the binding, stamped as
// this import's and scoped as its owner.
func createRigImportGate(binding beads.Store, rig string, g rigImportGate) (beads.Bead, error) {
	gate := dispatch.NewCrossStoreGate(binding, g.Owner, g.Blocker)
	gate.Metadata[infraMigrationStampKey] = rigImportStamp(rig)
	scope := strings.TrimSpace(g.Owner.Metadata[beadmeta.RootStoreRefMetadataKey])
	if scope == "" {
		scope = rigScopeRef(rig)
	}
	gate.Metadata[beadmeta.RootStoreRefMetadataKey] = scope
	created, err := binding.Create(gate)
	if err != nil {
		return beads.Bead{}, fmt.Errorf("minting the cross-store gate for %s on %s: %w", g.Owner.ID, g.Blocker, err)
	}
	return created, nil
}

// rigImportVerify is phase 4: against a reopened binding, the rig's stamped
// population must be exactly the manifest's rows still held, plus this run's
// rows and gates; every row this run copied field-equal and in its class; and
// every copied row's edges exactly the planned ones.
func rigImportVerify(env rigImportEnv, loc rigImportLocation, manifest *infraCopyManifest, plan rigImportPlan) error {
	binding, err := openInfraBindingReadOnly(env.Target)
	if err != nil {
		return fmt.Errorf("reopening the binding to verify rig %s's copy: %w", loc.Rig, err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // best-effort close
	stamped, err := binding.List(beads.ListQuery{Metadata: map[string]string{infraMigrationStampKey: rigImportStamp(loc.Rig)}, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return fmt.Errorf("listing the binding's rows from rig %s: %w", loc.Rig, err)
	}
	got := make(map[string]beads.Bead, len(stamped))
	for _, b := range stamped {
		got[b.ID] = b
	}
	gateOf := map[string]string{} // gate id -> owner
	for _, b := range stamped {
		if owner := b.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey]; owner != "" && hasLabel(b.Labels, dispatch.CrossStoreGateLabel) {
			gateOf[b.ID] = owner
		}
	}
	moved := make(map[string]bool, len(plan.Move))
	for _, b := range plan.Move {
		moved[b.ID] = true
		g, ok := got[b.ID]
		if !ok {
			return fmt.Errorf("verifying rig %s's copy: the binding does not hold %s", loc.Rig, b.ID)
		}
		want := rigImportRow(b, loc.Rig, plan.Records[b.ID])
		if diff := beadCopyDifference(want, g); diff != "" {
			return fmt.Errorf("verifying rig %s's copy: %s: %s", loc.Rig, b.ID, diff)
		}
		if diff := infraCopyClassDifference(b, g); diff != "" {
			return fmt.Errorf("verifying rig %s's copy: %s %s", loc.Rig, b.ID, diff)
		}
	}
	gatesWanted := 0
	for id := range got {
		switch {
		case moved[id]:
		case gateOf[id] != "":
			gatesWanted++
		case manifest != nil && (manifest.IDs[id] || manifest.Gates[id]):
		default:
			return fmt.Errorf("verifying rig %s's copy: the binding holds %s under this rig's import stamp, which neither this run nor an earlier import wrote", loc.Rig, id)
		}
	}
	priorGates := 0
	if manifest != nil {
		for id := range manifest.Gates {
			if gateOf[id] != "" {
				priorGates++
			}
		}
	}
	if gatesWanted-priorGates != len(plan.Gates) {
		return fmt.Errorf("verifying rig %s's copy: the binding holds %d new cross-store gate(s), the plan minted %d", loc.Rig, gatesWanted-priorGates, len(plan.Gates))
	}
	wantEdges := map[string][]string{}
	for _, e := range plan.Copy {
		wantEdges[e.IssueID] = append(wantEdges[e.IssueID], e.DependsOnID+" ("+e.Type+")")
	}
	for gateID, owner := range gateOf {
		if moved[owner] && (manifest == nil || !manifest.Gates[gateID]) {
			wantEdges[owner] = append(wantEdges[owner], gateID+" (blocks)")
		}
	}
	for _, b := range plan.Move {
		deps, err := binding.DepList(b.ID, "down")
		if err != nil {
			return fmt.Errorf("verifying rig %s's copy: listing deps of %s: %w", loc.Rig, b.ID, err)
		}
		if diff := stringSetDifference("dependency of "+b.ID, wantEdges[b.ID], censusEdges(deps)); diff != "" {
			return fmt.Errorf("verifying rig %s's copy: %s", loc.Rig, diff)
		}
	}
	return nil
}

// rigImportWriteManifest is phase 5's manifest: the earlier import's record
// (when the marker exists) plus this run's rows and gates.
func rigImportWriteManifest(loc rigImportLocation, target infraBindingTarget, manifest *infraCopyManifest, plan rigImportPlan) error {
	ids := map[string]bool{}
	created := map[string]time.Time{}
	annotated := map[string]bool{}
	gates := map[string]bool{}
	gen := coordclass.CurrentGeneration
	var writtenAt time.Time
	if manifest != nil {
		for id := range manifest.IDs {
			ids[id] = true
		}
		for id, at := range manifest.Created {
			created[id] = at
		}
		for id := range manifest.ScopeAnnotated {
			annotated[id] = true
		}
		for id := range manifest.Gates {
			gates[id] = true
		}
		gen = manifest.Generation
		writtenAt = manifest.WrittenAt
	}
	for _, b := range plan.Move {
		ids[b.ID] = true
		created[b.ID] = b.CreatedAt
	}
	for _, id := range plan.Annotated {
		annotated[id] = true
	}
	binding, err := openInfraBindingReadOnly(target)
	if err != nil {
		return fmt.Errorf("reading rig %s's gates for the manifest: %w", loc.Rig, err)
	}
	minted, err := binding.List(beads.ListQuery{Label: dispatch.CrossStoreGateLabel, Metadata: map[string]string{infraMigrationStampKey: rigImportStamp(loc.Rig)}, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	_ = closeBeadStoreHandle(binding)
	if err != nil {
		return fmt.Errorf("reading rig %s's gates for the manifest: %w", loc.Rig, err)
	}
	for _, g := range minted {
		gates[g.ID] = true
	}
	if writtenAt.IsZero() {
		writtenAt = time.Now().UTC()
	}
	extra := []string{infraManifestScopePrefix + rigScopeRef(loc.Rig)}
	for _, id := range sortedMapKeys(annotated) {
		extra = append(extra, infraManifestScopeAnnotatedPrefix+id)
	}
	for _, id := range sortedMapKeys(gates) {
		extra = append(extra, infraManifestGatePrefix+id)
	}
	return writeInfraCopyManifestFileAt(loc.BindingRoot, loc.ManifestPath(), sortedMapKeys(ids), created, writtenAt, writtenAt, gen, extra)
}

// rigImportReserveIDs is phase 6: raise the rig's bd min_hash_length past
// every imported id, never lowering it, and raise max_hash_length with it
// when the floor is above it (bd sizes ids between the two). It returns the
// floor in force (0 when no id needs one or the store mints from a monotonic
// sequence). conf was opened, and its provider checked, before anything was
// written.
func rigImportReserveIDs(conf rigHashLengthConfig, manifest *infraCopyManifest) (int, error) {
	floor := rigImportHashFloor(sortedMapKeys(manifest.IDs))
	if floor == 0 || conf == nil {
		return 0, nil
	}
	current, err := conf.ConfigGet(bdMinHashLengthKey)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", bdMinHashLengthKey, err)
	}
	if n, perr := strconv.Atoi(strings.TrimSpace(current)); perr == nil && n >= floor {
		floor = n
	} else if err := conf.ConfigSet(bdMinHashLengthKey, strconv.Itoa(floor)); err != nil {
		return 0, fmt.Errorf("setting %s=%d: %w", bdMinHashLengthKey, floor, err)
	}
	maxText, err := conf.ConfigGet(bdMaxHashLengthKey)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", bdMaxHashLengthKey, err)
	}
	maxLen := bdDefaultMaxHashLength
	if n, perr := strconv.Atoi(strings.TrimSpace(maxText)); perr == nil && n > 0 {
		maxLen = n
	}
	if maxLen < floor {
		if err := conf.ConfigSet(bdMaxHashLengthKey, strconv.Itoa(floor)); err != nil {
			return 0, fmt.Errorf("setting %s=%d: %w", bdMaxHashLengthKey, floor, err)
		}
	}
	return floor, nil
}

// rigImportClear is phase 7: back up, prove, mark pending, delete in
// batches, mark complete. It clears every row the manifest records as
// carried that the rig still holds. No edge is re-added.
func rigImportClear(env rigImportEnv, loc rigImportLocation, rigStore beads.Store, snap rigSnapshot, manifest *infraCopyManifest, note infraClearedNote, resuming bool, plan rigImportPlan) (int, error) {
	var toClear []beads.Bead
	for _, b := range snap.rows {
		if coordclass.ClassifyAt(b, manifest.Generation) == coordclass.ClassWork {
			continue
		}
		if manifest.Carried(b) {
			toClear = append(toClear, b)
		}
	}
	session := map[string]bool{}
	if resuming {
		for _, id := range note.Pending {
			session[id] = true
		}
	}
	clearing := map[string]bool{}
	for _, b := range toClear {
		session[b.ID] = true
		clearing[b.ID] = true
	}
	if len(session) == 0 {
		if !note.Complete {
			return 0, writeInfraClearedNoteAt(loc.NotePath(), infraClearedNote{Binding: env.Target.Binding, Database: env.Target.Database, Backup: loc.BackupPath(), Complete: true})
		}
		return 0, nil
	}
	if err := rigImportReprove(env, loc, rigStore, snap, toClear); err != nil {
		return 0, err
	}
	previous, _, err := readInfraRetainedBackupAt(loc.BackupPath())
	if err != nil {
		return 0, err
	}
	sessionBackup := map[string]infraBackupEntry{}
	if resuming {
		for _, e := range previous {
			if session[e.Bead.ID] {
				sessionBackup[e.Bead.ID] = e
			}
		}
	}
	up := map[string][]beads.Dep{}
	for _, b := range snap.rows {
		for _, d := range snap.down[b.ID] {
			if d.IssueID == "" {
				d.IssueID = b.ID
			}
			up[d.DependsOnID] = append(up[d.DependsOnID], d)
		}
	}
	disposition := rigEdgeDispositions(plan)
	entries := make([]infraBackupEntry, 0, len(toClear))
	for _, b := range toClear {
		if prior, ok := sessionBackup[b.ID]; ok {
			entries = append(entries, prior)
			continue
		}
		entry, err := rigBackupEntry(rigStore, b, snap.down[b.ID], up[b.ID], clearing, disposition)
		if err != nil {
			return 0, err
		}
		entries = append(entries, entry)
	}
	merged := mergeInfraBackupEntries(previous, entries)
	label := env.Target.Binding + " " + rigImportStamp(loc.Rig)
	if len(entries) > 0 {
		if err := writeInfraRetainedBackupAt(loc.BindingRoot, loc.BackupPath(), label, merged); err != nil {
			return 0, err
		}
		if err := verifyInfraRetainedBackupAt(loc.BackupPath(), entries); err != nil {
			return 0, err
		}
	}
	if err := rigImportCrashPoint("backup"); err != nil {
		return 0, err
	}
	pending := sortedMapKeys(session)
	if err := writeInfraClearedNoteAt(loc.NotePath(), infraClearedNote{Binding: env.Target.Binding, Database: env.Target.Database, Backup: loc.BackupPath(), Pending: pending}); err != nil {
		return 0, err
	}
	if err := rigImportCrashPoint("pending"); err != nil {
		return 0, err
	}
	// Every edge a delete would follow to a row that outlives it goes first.
	// bd's delete rewrites each surviving neighbor's text that names a
	// deleted id to "[deleted:<id>]" (bumping its updated_at), and it finds
	// the neighbors by their edges. That is every boundary edge (a work row
	// that tracks a moved row would have its own description rewritten), and
	// every edge between two cleared rows deleted in different batches: the
	// row of the later batch would no longer match its binding copy, so a
	// clear stopped between the two batches could never be re-proved and
	// resumed. rigClearChunks keeps each edge-connected group of rows in one
	// batch where it fits, so few edges cross one. With the edges removed, no
	// row that outlives a batch is its neighbor. The backup above records
	// every edge, and the binding holds the copies of those among cleared
	// rows.
	chunks, cut := rigClearChunks(toClear, snap.down)
	for _, b := range toClear {
		for _, d := range snap.down[b.ID] {
			if clearing[d.DependsOnID] {
				continue
			}
			if err := rigStore.DepRemove(b.ID, d.DependsOnID); err != nil && !errors.Is(err, beads.ErrNotFound) {
				return 0, fmt.Errorf("removing boundary edge %s -> %s from rig %s before the clear: %w", b.ID, d.DependsOnID, loc.Rig, err)
			}
		}
		for _, d := range up[b.ID] {
			if clearing[d.IssueID] {
				continue
			}
			if err := rigStore.DepRemove(d.IssueID, b.ID); err != nil && !errors.Is(err, beads.ErrNotFound) {
				return 0, fmt.Errorf("removing boundary edge %s -> %s from rig %s before the clear: %w", d.IssueID, b.ID, loc.Rig, err)
			}
		}
	}
	for _, d := range cut {
		if err := rigStore.DepRemove(d.IssueID, d.DependsOnID); err != nil && !errors.Is(err, beads.ErrNotFound) {
			return 0, fmt.Errorf("removing edge %s -> %s from rig %s before the clear (its ends are deleted in different batches): %w", d.IssueID, d.DependsOnID, loc.Rig, err)
		}
	}
	if err := rigImportCrashPoint("unlinked"); err != nil {
		return 0, err
	}
	if err := deleteChunks(rigStore, chunks, func() error { return rigImportCrashPoint("deleting") }); err != nil {
		return 0, fmt.Errorf("removing rig %s's imported copies: %w (the cleared note is pending; boot refuses until `%s` finishes the clear)", loc.Rig, err, rigImportInstruction(loc.Rig))
	}
	after, err := rigStore.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return 0, fmt.Errorf("re-reading rig %s after the clear: %w", loc.Rig, err)
	}
	var left []string
	for _, b := range after {
		if session[b.ID] && manifest.Carried(b) {
			left = append(left, b.ID)
		}
	}
	if len(left) > 0 {
		sort.Strings(left)
		return 0, fmt.Errorf("rig %s still holds %d imported cop(ies) after the clear: %s", loc.Rig, len(left), infraStrandedIDList(left))
	}
	if err := writeInfraClearedNoteAt(loc.NotePath(), infraClearedNote{Binding: env.Target.Binding, Database: env.Target.Database, Backup: loc.BackupPath(), Complete: true}); err != nil {
		return 0, err
	}
	return len(toClear), nil
}

// rigImportReprove proves, immediately before anything is removed, that
// every rig row about to be cleared is still the row the census read (no
// writer changed it since) and that the binding holds it: the same (id,
// created_at), under this rig's import stamp. A row that fails either is
// named and nothing is removed.
func rigImportReprove(env rigImportEnv, loc rigImportLocation, rigStore beads.Store, snap rigSnapshot, toClear []beads.Bead) error {
	fresh, err := rigStore.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return fmt.Errorf("re-reading rig %s before the clear: %w", loc.Rig, err)
	}
	now := make(map[string]beads.Bead, len(fresh))
	for _, b := range fresh {
		now[b.ID] = b
	}
	binding, err := openInfraBindingReadOnly(env.Target)
	if err != nil {
		return fmt.Errorf("opening the binding to prove rig %s's copies before the clear: %w", loc.Rig, err)
	}
	defer closeBeadStoreHandle(binding) //nolint:errcheck // best-effort close
	ids := make([]string, 0, len(toClear))
	for _, b := range toClear {
		ids = append(ids, b.ID)
	}
	held, err := beads.GetMany(binding, ids)
	if err != nil {
		return fmt.Errorf("reading the binding to prove rig %s's copies before the clear: %w", loc.Rig, err)
	}
	clearing := make(map[string]bool, len(toClear))
	for _, b := range toClear {
		clearing[b.ID] = true
	}
	var moved, unproven, differs []string
	for _, b := range toClear {
		f, ok := now[b.ID]
		if ok && (beadCopyDifference(b, f) != "" || (b.Revision != 0 && f.Revision != 0 && b.Revision != f.Revision)) {
			moved = append(moved, b.ID)
			continue
		}
		h, ok := held[b.ID]
		if !ok || h.Metadata[infraMigrationStampKey] != rigImportStamp(loc.Rig) || !rigSameCreated(b, h) {
			unproven = append(unproven, b.ID)
			continue
		}
		// The binding copy must still be the row the import wrote for this
		// rig row, field for field, with every edge among the cleared rows.
		want := rigImportRow(b, loc.Rig, nil)
		if recorded, ok := h.Metadata[beadmeta.MigratedCrossEdgesMetadataKey]; ok {
			want.Metadata[beadmeta.MigratedCrossEdgesMetadataKey] = recorded
		}
		if diff := beadCopyDifference(want, h); diff != "" {
			differs = append(differs, fmt.Sprintf("%s (%s)", b.ID, diff))
			continue
		}
		var wantEdges []string
		for _, d := range snap.down[b.ID] {
			if clearing[d.DependsOnID] {
				wantEdges = append(wantEdges, d.DependsOnID+" ("+d.Type+")")
			}
		}
		if len(wantEdges) == 0 {
			continue
		}
		deps, err := binding.DepList(b.ID, "down")
		if err != nil {
			return fmt.Errorf("reading the binding's edges of %s before the clear: %w", b.ID, err)
		}
		got := setOf(censusEdges(deps))
		for _, e := range wantEdges {
			if !got[e] {
				differs = append(differs, fmt.Sprintf("%s (the binding lacks its edge to %s)", b.ID, e))
				break
			}
		}
	}
	var reasons []string
	if len(moved) > 0 {
		sort.Strings(moved)
		reasons = append(reasons, fmt.Sprintf("%d rig row(s) changed after the census read them, so a writer is still reaching rig %s: %s. Stop it, then re-run", len(moved), loc.Rig, infraStrandedIDList(moved)))
	}
	if len(unproven) > 0 {
		sort.Strings(unproven)
		reasons = append(reasons, fmt.Sprintf("the binding does not hold %d row(s) the clear would remove from rig %s as this import's copy of the same bead, so the rig row may be the only copy: %s. Nothing was removed; restore the binding database from its backup, then re-run `%s`", len(unproven), loc.Rig, infraStrandedIDList(unproven), rigImportInstruction(loc.Rig)))
	}
	if len(differs) > 0 {
		sort.Strings(differs)
		reasons = append(reasons, fmt.Sprintf("%d binding cop(ies) no longer match the rig rows the clear would remove from rig %s: %s. Nothing was removed; reconcile each row (the rig row and its binding copy), then re-run `%s`", len(differs), loc.Rig, infraStrandedIDList(differs), rigImportInstruction(loc.Rig)))
	}
	if len(reasons) > 0 {
		return &rigImportPrepareRefusal{msg: strings.Join(reasons, "; ")}
	}
	return nil
}

// rigEdgeDispositions maps each edge the import decided to what it did with
// it, keyed "from\x00to": this run's planned edges, and the boundary edges an
// earlier run recorded on the rows it carried.
func rigEdgeDispositions(plan rigImportPlan) map[string]string {
	out := map[string]string{}
	for _, e := range plan.Copy {
		out[e.IssueID+"\x00"+e.DependsOnID] = rigEdgeCopied
	}
	for _, records := range []map[string][]rigCrossEdgeRecord{plan.RetainedRecords, plan.Records} {
		for id, recs := range records {
			for _, r := range recs {
				key := id + "\x00" + r.Other
				if r.Dir == "in" {
					key = r.Other + "\x00" + id
				}
				out[key] = r.Action
			}
		}
	}
	return out
}

// rigBackupEntry is one cleared row with its out-edges and its in-edges from
// rows that stay, each marked with what the import did with it.
func rigBackupEntry(rigStore beads.Store, b beads.Bead, down, up []beads.Dep, clearing map[string]bool, disposition map[string]string) (infraBackupEntry, error) {
	entry := infraBackupEntry{Bead: b, Class: coordclass.Classify(b).String()}
	for _, d := range down {
		if d.IssueID == "" {
			d.IssueID = b.ID
		}
		edge, err := rigBackupEdge(rigStore, d, disposition)
		if err != nil {
			return entry, err
		}
		entry.Deps = append(entry.Deps, edge)
	}
	for _, d := range up {
		if clearing[d.IssueID] {
			continue
		}
		edge, err := rigBackupEdge(rigStore, d, disposition)
		if err != nil {
			return entry, err
		}
		entry.Dependents = append(entry.Dependents, edge)
	}
	sortInfraBackupEdges(entry.Deps)
	sortInfraBackupEdges(entry.Dependents)
	return entry, nil
}

// rigBackupEdge is one edge of a cleared row as the backup records it: its
// payload, read through the rig store (the plan proved every such read
// answers), and what the import did with it.
func rigBackupEdge(rigStore beads.Store, d beads.Dep, disposition map[string]string) (infraBackupEdge, error) {
	action := disposition[d.IssueID+"\x00"+d.DependsOnID]
	if action == "" {
		action = rigEdgeCopied
	}
	edge, err := infraBackupEdgeFor(rigStore, d)
	if err != nil {
		return edge, err
	}
	edge.Disposition = action
	return edge, nil
}

// rigClearChunks splits the rows the clear removes into delete batches of at
// most rigImportDeleteChunk ids, and returns the edges between two of them
// that fall into different batches. Each edge-connected group of rows is
// taken in turn (groups by their least id, each breadth-first from it, so a
// chain or tree stays contiguous), and a group that fits a batch but not what
// is left of the current one starts the next, so only a group larger than a
// batch has edges that cross one.
func rigClearChunks(toClear []beads.Bead, down map[string][]beads.Dep) ([][]string, []beads.Dep) {
	ids := make([]string, 0, len(toClear))
	clearing := make(map[string]bool, len(toClear))
	for _, b := range toClear {
		ids = append(ids, b.ID)
		clearing[b.ID] = true
	}
	sort.Strings(ids)
	var internal []beads.Dep
	adjacent := map[string][]string{}
	for _, id := range ids {
		for _, d := range down[id] {
			if !clearing[d.DependsOnID] || d.DependsOnID == id {
				continue
			}
			internal = append(internal, beads.Dep{IssueID: id, DependsOnID: d.DependsOnID, Type: d.Type})
			adjacent[id] = append(adjacent[id], d.DependsOnID)
			adjacent[d.DependsOnID] = append(adjacent[d.DependsOnID], id)
		}
	}
	for id := range adjacent {
		sort.Strings(adjacent[id])
	}
	var chunks [][]string
	var current []string
	flush := func() {
		if len(current) > 0 {
			chunks = append(chunks, current)
			current = nil
		}
	}
	visited := make(map[string]bool, len(ids))
	for _, start := range ids {
		if visited[start] {
			continue
		}
		visited[start] = true
		group := []string{start}
		for i := 0; i < len(group); i++ {
			for _, next := range adjacent[group[i]] {
				if !visited[next] {
					visited[next] = true
					group = append(group, next)
				}
			}
		}
		if len(group) <= rigImportDeleteChunk && len(current)+len(group) > rigImportDeleteChunk {
			flush()
		}
		for _, id := range group {
			current = append(current, id)
			if len(current) == rigImportDeleteChunk {
				flush()
			}
		}
	}
	flush()
	chunkOf := make(map[string]int, len(ids))
	for i, chunk := range chunks {
		for _, id := range chunk {
			chunkOf[id] = i
		}
	}
	var cut []beads.Dep
	for _, d := range internal {
		if chunkOf[d.IssueID] != chunkOf[d.DependsOnID] {
			cut = append(cut, d)
		}
	}
	return chunks, cut
}

// deleteInBatches removes ids through the store's batched delete in chunks
// of rigImportDeleteChunk, falling back to one delete per id.
func deleteInBatches(store beads.Store, ids []string) error {
	var chunks [][]string
	for start := 0; start < len(ids); start += rigImportDeleteChunk {
		chunks = append(chunks, ids[start:min(start+rigImportDeleteChunk, len(ids))])
	}
	return deleteChunks(store, chunks, nil)
}

// deleteChunks removes each chunk through the store's batched delete, one
// call per chunk, falling back to one delete per id, and calls between after
// every chunk but the last.
func deleteChunks(store beads.Store, chunks [][]string, between func() error) error {
	batcher, batched := store.(beads.BatchDeleter)
	for i, chunk := range chunks {
		if batched {
			err := batcher.DeleteBatch(chunk)
			switch {
			case err == nil:
			case errors.Is(err, beads.ErrBatchDeleteUnsupported):
				batched = false
			default:
				return err
			}
		}
		if !batched {
			for _, id := range chunk {
				if err := store.Delete(id); err != nil && !errors.Is(err, beads.ErrNotFound) {
					return fmt.Errorf("deleting %s: %w", id, err)
				}
			}
		}
		if between != nil && i < len(chunks)-1 {
			if err := between(); err != nil {
				return err
			}
		}
	}
	return nil
}

// doStorageMigrateFromRigs is `gc storage migrate --from-rigs`.
func doStorageMigrateFromRigs(ctx context.Context, request storageOperatorRequest, rigNames []string, stdout, stderr io.Writer) int {
	const logPrefix = "gc storage migrate --from-rigs"
	if err := config.ReservedPrefixSplitRefusal(request.Cfg); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	target, ok, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !ok {
		fmt.Fprintf(stderr, "%s: this city serves every class from its work stores; there is nothing to import\n", logPrefix) //nolint:errcheck // best-effort stderr
		return 1
	}
	if _, err := resolveCityStoragePlan(request.CityPath, request.Cfg); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
		fmt.Fprintf(stderr, "%s: controller PID %d is live on this city. Stop it (%s) and run this again\n", logPrefix, pid, storageStopCommand) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !request.FleetStopped {
		fmt.Fprintf(stderr, "%s: pass --%s to attest that every writer that can reach this city's rig stores is stopped. This command proves the controller is stopped and cannot prove anything about the rest\n", logPrefix, storageFleetStoppedFlag) //nolint:errcheck // best-effort stderr
		return 1
	}
	if state, err := readInfraConvergenceState(target); err != nil || state != infraConvergenceMarked {
		fmt.Fprintf(stderr, "%s: the city binding %s has not converged (no %s), and a rig import never creates or re-copies a binding. Run `%s --%s` first\n", logPrefix, target.Binding, target.MarkerPath(), storageMigrationCommand, storageFleetStoppedFlag) //nolint:errcheck // best-effort stderr
		return 1
	}
	rigs, err := selectRigsForImport(request.Cfg, rigNames)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	guard, err := acquireStorageMigrationGuard(ctx, request.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer func() {
		if releaseErr := guard.Release(); releaseErr != nil {
			fmt.Fprintf(stderr, "%s: releasing the migration guard: %v\n", logPrefix, releaseErr) //nolint:errcheck // best-effort stderr
		}
	}()
	env := rigImportEnv{
		CityPath: request.CityPath,
		CityName: loadedCityName(request.Cfg, request.CityPath),
		Cfg:      request.Cfg,
		Target:   target,
		Stdout:   stdout,
		Stderr:   stderr,
		Started:  time.Now(),
	}
	shared := rigsSharingOneDatabase(env, rigs)
	rec := openCityRecorderAt(request.CityPath, stderr)
	exit := 0
	for _, rig := range rigs {
		var result rigImportResult
		if others, dup := shared[rig.Name]; dup {
			result = rigImportResult{Rig: rig.Name, Outcome: rigImportRefused, Err: fmt.Errorf("rig %s lists exactly the rows rig(s) %s list, so they resolve to one database; importing one would stamp the other's rows with the wrong scope. Give each rig its own database, then re-run", rig.Name, strings.Join(others, ", "))}
		} else {
			result = importRig(env, rig)
		}
		reportRigImport(stdout, stderr, result)
		recordRigImportEvent(rec, target, result)
		if !result.ok() {
			exit = 1
		}
	}
	return exit
}

// selectRigsForImport returns the configured rigs named, or every rig.
func selectRigsForImport(cfg *config.City, names []string) ([]config.Rig, error) {
	if len(names) == 0 {
		return append([]config.Rig(nil), cfg.Rigs...), nil
	}
	byName := make(map[string]config.Rig, len(cfg.Rigs))
	for _, r := range cfg.Rigs {
		byName[r.Name] = r
	}
	var out []config.Rig
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		r, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("this city declares no rig %q", name)
		}
		if !seen[name] {
			out = append(out, r)
			seen[name] = true
		}
	}
	return out, nil
}

// rigsSharingOneDatabase reports, for each rig of this run, the other rigs of
// this run whose store lists exactly the same non-empty set of row ids: two
// rigs on one database. A rig that cannot be read is not compared; its own
// import reports why.
func rigsSharingOneDatabase(env rigImportEnv, rigs []config.Rig) map[string][]string {
	if len(rigs) < 2 {
		return nil
	}
	byPrint := map[string][]string{}
	for _, rig := range rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		store, err := rigImportStoreOpener(env.CityPath, rig)
		if err != nil {
			continue
		}
		rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		_ = closeBeadStoreHandle(store)
		if err != nil || len(rows) == 0 {
			continue
		}
		ids := make([]string, 0, len(rows))
		for _, b := range rows {
			ids = append(ids, b.ID)
		}
		sort.Strings(ids)
		key := strings.Join(ids, "\x00")
		byPrint[key] = append(byPrint[key], rig.Name)
	}
	out := map[string][]string{}
	for _, names := range byPrint {
		if len(names) < 2 {
			continue
		}
		for _, n := range names {
			var others []string
			for _, o := range names {
				if o != n {
					others = append(others, o)
				}
			}
			out[n] = others
		}
	}
	return out
}

// reportRigImport prints one rig's outcome.
func reportRigImport(stdout, stderr io.Writer, r rigImportResult) {
	switch r.Outcome {
	case rigImportImported:
		fmt.Fprintf(stdout, "rig %s: imported %d bead(s) and %d edge(s) into the city binding; %d cross-store gate(s) minted, %d scope annotation(s), %d boundary edge(s) recorded, %d rig cop(ies) cleared", r.Rig, r.Copied, r.Edges, r.Gates, r.Annotated, r.Recorded, r.Cleared) //nolint:errcheck // best-effort stdout
		if r.HashFloor > 0 {
			fmt.Fprintf(stdout, "; rig min_hash_length %d", r.HashFloor) //nolint:errcheck // best-effort stdout
		}
		if r.OpenRouted > 0 {
			fmt.Fprintf(stdout, "; %d open routed bead(s) are now visible to demand", r.OpenRouted) //nolint:errcheck // best-effort stdout
		}
		fmt.Fprintf(stdout, " (%s)\n", r.Elapsed.Round(time.Millisecond)) //nolint:errcheck // best-effort stdout
	case rigImportClean:
		fmt.Fprintf(stdout, "rig %s: holds no infrastructure bead; recorded clean\n", r.Rig) //nolint:errcheck // best-effort stdout
	case rigImportNothing:
		msg := "nothing to import"
		if r.Why != "" {
			msg += " (" + r.Why + ")"
		}
		if r.Cleared > 0 {
			msg += fmt.Sprintf("; %d rig cop(ies) cleared", r.Cleared)
		}
		fmt.Fprintf(stdout, "rig %s: %s\n", r.Rig, msg) //nolint:errcheck // best-effort stdout
	case rigImportSkipped:
		fmt.Fprintf(stdout, "rig %s: skipped — %s\n", r.Rig, r.Why) //nolint:errcheck // best-effort stdout
	case rigImportRefused:
		if r.Err != nil {
			fmt.Fprintf(stderr, "rig %s: REFUSED — %v\n", r.Rig, r.Err) //nolint:errcheck // best-effort stderr
		}
		if len(r.Refusals) > 0 {
			fmt.Fprintf(stderr, "rig %s: REFUSED — %d shape(s) the import cannot move without a cross-store edge; nothing was written:\n", r.Rig, len(r.Refusals)) //nolint:errcheck // best-effort stderr
			for _, refusal := range r.Refusals {
				fmt.Fprintf(stderr, "  %s\n", refusal) //nolint:errcheck // best-effort stderr
			}
		}
	default:
		fmt.Fprintf(stderr, "rig %s: FAILED — %v. Re-run `%s` to resume\n", r.Rig, r.Err, rigImportInstruction(r.Rig)) //nolint:errcheck // best-effort stderr
	}
}

// recordRigImportEvent publishes one rig's import outcome.
func recordRigImportEvent(rec events.Recorder, target infraBindingTarget, r rigImportResult) {
	if rec == nil {
		return
	}
	detail := r.Why
	if r.Err != nil {
		detail = r.Err.Error()
	}
	if len(r.Refusals) > 0 {
		parts := make([]string, 0, len(r.Refusals))
		for _, refusal := range r.Refusals {
			parts = append(parts, refusal.String())
		}
		detail = strings.Join(parts, "; ")
	}
	raw, err := json.Marshal(storebinding.StorageRigPayload{
		Rig:     r.Rig,
		Binding: target.Binding,
		Outcome: string(r.Outcome),
		Beads:   r.Copied,
		Gates:   r.Gates,
		Cleared: r.Cleared,
		Detail:  detail,
	})
	if err != nil {
		return
	}
	rec.Record(events.Event{Type: events.StorageRigImport, Actor: "gc", Subject: r.Rig, Payload: raw})
}

// rigsWithoutImportRecord names the bound rigs that carry neither an import
// marker nor a clean record: file reads only, nothing is opened.
func rigsWithoutImportRecord(cityPath string, cfg *config.City, target infraBindingTarget) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" || !rigNameIsFileSafe(rig.Name) {
			continue
		}
		if samePath(resolveStoreScopeRoot(cityPath, rig.Path), resolveStoreScopeRoot(cityPath, cityPath)) {
			continue
		}
		loc := newRigImportLocation(target, cityPath, rig.Name)
		root := rigStoreRoot(cityPath, rig)
		if rigRecordSpeaksFor(loc.MarkerPath(), root) || rigRecordSpeaksFor(loc.CleanPath(), root) {
			continue
		}
		out = append(out, rig.Name)
	}
	sort.Strings(out)
	return out
}

// storageAbandonRigStoreFlag names the escape from a rig whose store is gone
// for good after its import's marker: the binding is proven to hold every row
// the manifest records, and the clear is recorded complete without the rig.
const storageAbandonRigStoreFlag = "abandon-rig-store"

// doStorageAbandonRigStore is `gc storage migrate --from-rigs --rig X
// --abandon-rig-store --fleet-stopped`: for a rig whose store is permanently
// gone after its import reached its marker (until S4c's reverse exists, the
// only way out of the boot refusal a pending clear holds). It opens no rig
// store. It proves the binding holds every row the rig's manifest records,
// under this rig's import stamp and with the recorded created_at, and only
// then records the clear complete. Rows the rig store still held are not
// removed by this: the store is declared gone.
func doStorageAbandonRigStore(ctx context.Context, request storageOperatorRequest, rigName string, stdout, stderr io.Writer) int {
	logPrefix := "gc storage migrate --from-rigs --" + storageAbandonRigStoreFlag
	target, ok, err := resolveInfraBindingTarget(request.CityPath, request.Cfg)
	if err != nil || !ok {
		fmt.Fprintf(stderr, "%s: this city has no converged binding to abandon a rig store into (%v)\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if pid := infraMigrationForeignControllerPID(request.CityPath); pid != 0 {
		fmt.Fprintf(stderr, "%s: controller PID %d is live on this city. Stop it (%s) and run this again\n", logPrefix, pid, storageStopCommand) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !request.FleetStopped {
		fmt.Fprintf(stderr, "%s: pass --%s\n", logPrefix, storageFleetStoppedFlag) //nolint:errcheck // best-effort stderr
		return 1
	}
	guard, err := acquireStorageMigrationGuard(ctx, request.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer guard.Release() //nolint:errcheck // best-effort release
	loc := newRigImportLocation(target, request.CityPath, rigName)
	manifest, marked, err := loc.readManifest()
	if err != nil || !marked || manifest == nil {
		fmt.Fprintf(stderr, "%s: rig %s has no import marker and manifest (%v): its import never proved the binding holds its rows, so the rig store cannot be abandoned. Restore the rig store and run `%s`\n", logPrefix, rigName, err, rigImportInstruction(rigName)) //nolint:errcheck // best-effort stderr
		return 1
	}
	if rig, declared := rigImportRigNamed(request.Cfg, rigName); declared && strings.TrimSpace(rig.Path) != "" {
		if store, err := rigImportStoreOpener(request.CityPath, rig); err == nil {
			_, listErr := store.List(beads.ListQuery{Limit: 1, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
			_ = closeBeadStoreHandle(store)
			if listErr == nil {
				fmt.Fprintf(stderr, "%s: rig %s's store is reachable, so it is not gone: finish the import with `%s` instead\n", logPrefix, rigName, rigImportInstruction(rigName)) //nolint:errcheck // best-effort stderr
				return 1
			}
		}
	}
	if fileExists(loc.InFlightPath()) {
		fmt.Fprintf(stderr, "%s: rig %s has an import in flight (%s): the binding may hold rows without their edges, which only the import can finish or undo. Restore the rig store and run `%s`\n", logPrefix, rigName, loc.InFlightPath(), rigImportInstruction(rigName)) //nolint:errcheck // best-effort stderr
		return 1
	}
	binding, err := openInfraBindingReadOnly(target)
	if err != nil {
		fmt.Fprintf(stderr, "%s: opening the binding: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	held, err := beads.GetMany(binding, sortedMapKeys(manifest.IDs))
	var unrecorded []string
	if err == nil {
		unrecorded, err = rigUnrecordedImportRows(binding, rigName, manifest)
	}
	_ = closeBeadStoreHandle(binding)
	if err != nil {
		fmt.Fprintf(stderr, "%s: reading the binding: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(unrecorded) > 0 {
		fmt.Fprintf(stderr, "%s: the binding holds %d row(s) stamped as rig %s's import that its manifest does not record (an interrupted incremental import): %s. Only the import can prove or undo them; restore the rig store and run `%s`\n", logPrefix, len(unrecorded), rigName, infraStrandedIDList(unrecorded), rigImportInstruction(rigName)) //nolint:errcheck // best-effort stderr
		return 1
	}
	var missing []string
	for id := range manifest.IDs {
		h, ok := held[id]
		if !ok || h.Metadata[infraMigrationStampKey] != rigImportStamp(rigName) || !manifest.Carried(h) {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		fmt.Fprintf(stderr, "%s: the binding does not hold %d row(s) rig %s's manifest records as the bead the import carried: %s. Abandoning the rig store would lose them; restore it instead\n", logPrefix, len(missing), rigName, infraStrandedIDList(missing)) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := writeInfraClearedNoteAt(loc.NotePath(), infraClearedNote{Binding: target.Binding, Database: target.Database, Backup: loc.BackupPath(), Complete: true}); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "rig %s: store abandoned; the binding holds all %d row(s) its import recorded, and the clear is recorded complete\n", rigName, manifest.Len()) //nolint:errcheck // best-effort stdout
	return 0
}

// rigUnrecordedImportRows names, sorted, the binding rows stamped as rig's
// import that its manifest records neither as a row nor as a gate: an
// interrupted incremental attempt's copies, which may lack their edges.
func rigUnrecordedImportRows(binding beads.Store, rig string, manifest *infraCopyManifest) ([]string, error) {
	stamped, err := binding.List(beads.ListQuery{Metadata: map[string]string{infraMigrationStampKey: rigImportStamp(rig)}, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return nil, fmt.Errorf("listing the binding's rows from rig %s: %w", rig, err)
	}
	var unrecorded []string
	for _, b := range stamped {
		if !manifest.IDs[b.ID] && !manifest.Gates[b.ID] {
			unrecorded = append(unrecorded, b.ID)
		}
	}
	sort.Strings(unrecorded)
	return unrecorded, nil
}

// rigImportRigNamed returns the configured rig named name.
func rigImportRigNamed(cfg *config.City, name string) (config.Rig, bool) {
	if cfg == nil {
		return config.Rig{}, false
	}
	for _, r := range cfg.Rigs {
		if r.Name == name {
			return r, true
		}
	}
	return config.Rig{}, false
}
