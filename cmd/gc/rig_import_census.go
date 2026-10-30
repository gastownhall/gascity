package main

// The per-rig census (S4b): what each rig of a split city holds against its
// import records, for `gc storage status`, `gc storage preflight`, `gc
// doctor` and the controller's one asynchronous census at boot. Containment
// is checked there and nowhere else: not in every CLI process, because a rig
// listing can be a remote round trip and S1's city check already lists the
// whole city work store per process.
//
// Boot itself reads only files per rig (rigBootRefusal): a pending cleared
// note means twins exist and refuses; a rig with no record is awaiting import
// and serves, its relic legs keeping it correct.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// rigCensusState is one rig's verdict.
type rigCensusState string

const (
	// rigCensusImported: the import finished and the rig holds no
	// infrastructure row.
	rigCensusImported rigCensusState = "imported"
	// rigCensusClean: recorded clean and still holding no infrastructure row.
	rigCensusClean rigCensusState = "clean"
	// rigCensusAwaiting: no import record yet.
	rigCensusAwaiting rigCensusState = "awaiting import"
	// rigCensusRetained: the rig still holds rows the import carried (the
	// clear is pending or was interrupted).
	rigCensusRetained rigCensusState = "retained"
	// rigCensusStranded: infrastructure rows the import never carried, in a
	// rig recorded imported or clean.
	rigCensusStranded rigCensusState = "stranded"
	// rigCensusUnreachable: the rig's store could not be opened or listed.
	rigCensusUnreachable rigCensusState = "unreachable"
	// rigCensusSkipped: an unbound rig, or one whose store is the city's.
	rigCensusSkipped rigCensusState = "skipped"
)

// rigCensusRow is one rig's census.
type rigCensusRow struct {
	Rig      string
	State    rigCensusState
	Why      string
	Infra    int
	Open     int
	Stranded []string
	Retained []string
	// Reused names rig rows, of any class, under an id the rig's import
	// moved for a different bead (same id, another created_at): by-id reads
	// answer them with the binding's relic. The import raises the rig's
	// min_hash_length so this does not recur.
	Reused []string
	// Plan is the import's plan, on a full census (preflight).
	Plan *rigImportPlan
	Err  error
}

// blocking reports whether the row holds the deploy gate (`gc storage
// status` exits non-zero).
func (r rigCensusRow) blocking() bool {
	switch r.State {
	case rigCensusImported, rigCensusClean, rigCensusSkipped:
		return false
	default:
		return true
	}
}

// censusRigs censuses every rig of a split city. full adds the import plan
// (edges and binding lookups, read-only), which preflight reports.
func censusRigs(cityPath string, cfg *config.City, target infraBindingTarget, full bool) []rigCensusRow {
	if cfg == nil {
		return nil
	}
	cityName := loadedCityName(cfg, cityPath)
	rows := make([]rigCensusRow, 0, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		rows = append(rows, censusRig(cityPath, cityName, cfg, target, rig, full))
	}
	return rows
}

// censusRig censuses one rig. It writes nothing anywhere.
func censusRig(cityPath, cityName string, cfg *config.City, target infraBindingTarget, rig config.Rig, full bool) rigCensusRow {
	row := rigCensusRow{Rig: rig.Name}
	if strings.TrimSpace(rig.Path) == "" {
		row.State, row.Why = rigCensusSkipped, "unbound"
		return row
	}
	if samePath(resolveStoreScopeRoot(cityPath, rig.Path), resolveStoreScopeRoot(cityPath, cityPath)) {
		row.State, row.Why = rigCensusSkipped, "its store is the city work store"
		return row
	}
	if !rigNameIsFileSafe(rig.Name) {
		row.State, row.Err = rigCensusUnreachable, fmt.Errorf("rig name %q cannot name its import records", rig.Name)
		return row
	}
	loc := newRigImportLocation(target, cityPath, rig.Name)
	if err := loc.storeIdentityRefusal(rigStoreRoot(cityPath, rig)); err != nil {
		row.State, row.Err = rigCensusUnreachable, err
		return row
	}
	manifest, marked, err := loc.readManifest()
	if err != nil {
		row.State, row.Err = rigCensusUnreachable, err
		return row
	}
	note, notePresent, err := readInfraClearedNoteAt(loc.NotePath())
	if err != nil {
		row.State, row.Err = rigCensusUnreachable, err
		return row
	}
	store, err := rigImportStoreOpener(cityPath, rig)
	if err != nil {
		row.State, row.Err = rigCensusUnreachable, fmt.Errorf("opening rig %s's store: %w", rig.Name, err)
		return row
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close
	var snap rigSnapshot
	if full {
		snap, err = readRigSnapshot(store)
	} else {
		var listed []beads.Bead
		listed, err = store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		snap = rigSnapshot{rows: listed}
	}
	if err != nil {
		row.State, row.Err = rigCensusUnreachable, fmt.Errorf("listing rig %s: %w", rig.Name, err)
		return row
	}
	gen := coordclass.CurrentGeneration
	if manifest != nil {
		gen = manifest.Generation
	}
	for _, b := range snap.rows {
		if manifest != nil && manifest.Match(b) == infraManifestReusedID {
			row.Reused = append(row.Reused, b.ID)
		}
		if coordclass.ClassifyAt(b, gen) == coordclass.ClassWork {
			continue
		}
		row.Infra++
		if rigRowOpen(b) {
			row.Open++
		}
		if manifest != nil && manifest.Carried(b) {
			row.Retained = append(row.Retained, b.ID)
		} else {
			row.Stranded = append(row.Stranded, b.ID)
		}
	}
	sort.Strings(row.Retained)
	sort.Strings(row.Stranded)
	sort.Strings(row.Reused)
	clean := fileExists(loc.CleanPath())
	switch {
	case fileExists(loc.InFlightPath()):
		row.State, row.Why = rigCensusRetained, "an import is in flight"
	case notePresent && !note.Complete:
		row.State = rigCensusRetained
	case marked && len(row.Retained) > 0:
		row.State = rigCensusRetained
	case (marked || clean) && len(row.Stranded) > 0:
		row.State = rigCensusStranded
	case marked:
		row.State = rigCensusImported
	case clean:
		row.State = rigCensusClean
	default:
		row.State = rigCensusAwaiting
	}
	if full {
		env := rigImportEnv{CityPath: cityPath, CityName: cityName, Cfg: cfg, Target: target}
		ctx := rigPlanContext{Rig: rig.Name, CityName: cityName, Snap: snap, Manifest: manifest}
		if present, _ := infraPathExists(target.Database); present {
			if binding, oerr := openInfraBindingReadOnly(target); oerr == nil {
				if c, cerr := rigImportPlanContext(env, rig, snap, manifest, binding); cerr == nil {
					ctx = c
				} else {
					row.Err = cerr
				}
				_ = closeBeadStoreHandle(binding)
			} else {
				row.Err = fmt.Errorf("opening the city binding: %w", oerr)
			}
		}
		if row.Err != nil {
			// A plan over a binding that could not be read would name the
			// rows the import carried as gone from it; the error is the
			// answer.
			return row
		}
		plan := planRigImportReadingPayloads(ctx, store)
		if err := rigImportSharedStoreRefusal(env, rig); err != nil {
			plan.Refusals = append(plan.Refusals, rigImportRefusal{IDs: []string{rig.Name}, Reason: err.Error(), Remedy: "give the rig a store of its own"})
		}
		if _, err := rigImportHashConfigOpener(cityPath, cfg, rig); err != nil {
			plan.Refusals = append(plan.Refusals, rigImportRefusal{IDs: []string{rig.Name}, Reason: err.Error(), Remedy: "serve the rig from a bd or file provider"})
		}
		row.Plan = &plan
	}
	return row
}

// rigCensusLine renders one row for an operator.
func rigCensusLine(r rigCensusRow) string {
	switch r.State {
	case rigCensusImported:
		return fmt.Sprintf("rig %s: imported — the rig store holds only work", r.Rig)
	case rigCensusClean:
		return fmt.Sprintf("rig %s: clean — the rig store holds only work", r.Rig)
	case rigCensusSkipped:
		return fmt.Sprintf("rig %s: n/a (%s)", r.Rig, r.Why)
	case rigCensusUnreachable:
		return fmt.Sprintf("rig %s: UNREACHABLE — %v", r.Rig, r.Err)
	case rigCensusAwaiting:
		return fmt.Sprintf("rig %s: awaiting import (%d infrastructure bead(s), %d open); run `%s`", r.Rig, r.Infra, r.Open, rigImportInstruction(r.Rig))
	case rigCensusRetained:
		if r.Why != "" {
			return fmt.Sprintf("rig %s: retained — %s; finish it with `%s`", r.Rig, r.Why, rigImportInstruction(r.Rig))
		}
		return fmt.Sprintf("rig %s: retained — the rig still holds %d bead(s) its import carried (the clear is pending); finish it with `%s`", r.Rig, len(r.Retained), rigImportInstruction(r.Rig))
	case rigCensusStranded:
		return fmt.Sprintf("rig %s: stranded — %d infrastructure bead(s) the import never carried: %s; copy them in with `%s`", r.Rig, len(r.Stranded), infraStrandedIDList(r.Stranded), rigImportInstruction(r.Rig))
	default:
		return fmt.Sprintf("rig %s: %s", r.Rig, r.State)
	}
}

// reportRigCensus prints the per-rig state for `gc storage status` and
// returns 1 when any rig holds the deploy gate.
func reportRigCensus(stdout io.Writer, rows []rigCensusRow) int {
	exit := 0
	for _, r := range rows {
		fmt.Fprintf(stdout, "  %s\n", rigCensusLine(r)) //nolint:errcheck // best-effort stdout
		if r.blocking() {
			exit = 1
		}
	}
	return exit
}

// rigBootRefusal is the boot gate's per-rig check, file reads only. It reads
// the records on disk, not the configured rigs, so a rig removed from config
// after a crash still holds boot:
//
//   - an import in flight (infra.rig.<rig>.importing beside the binding):
//     the binding may hold rows without their edges;
//   - a pending cleared note (.gc/storage-infra-cleared.rig.<rig>.json): a
//     row may be held by both the rig store and the binding.
//
// An unreadable note refuses as a pending one would.
func rigBootRefusal(cityPath string, target infraBindingTarget) error {
	var reasons []string
	inFlight, _ := filepath.Glob(filepath.Join(target.Root, rigGraphInFlightName("*")))
	sort.Strings(inFlight)
	for _, path := range inFlight {
		rig := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "infra.rig."), ".importing")
		reasons = append(reasons, fmt.Sprintf("rig %s's import is in flight (%s): the binding may hold its rows without their edges, so a blocked step would read as ready. Stop the city and finish it with `%s`", rig, path, rigImportInstruction(rig)))
	}
	notes, _ := filepath.Glob(rigGraphClearedNotePath(cityPath, "*"))
	sort.Strings(notes)
	for _, path := range notes {
		rig := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "storage-infra-cleared.rig."), ".json")
		note, present, err := readInfraClearedNoteAt(path)
		switch {
		case err != nil:
			reasons = append(reasons, fmt.Sprintf("rig %s's cleared note cannot be read (%v); it holds as a pending one would. Finish the import with `%s`", rig, err, rigImportInstruction(rig)))
		case present && !note.Complete:
			reasons = append(reasons, fmt.Sprintf("rig %s's import was interrupted during its clear: %d bead(s) may be held by both the rig store and the city binding, and a reader that reaches the rig first is served the frozen copy. Stop the city and finish it with `%s`", rig, len(note.Pending), rigImportInstruction(rig)))
		}
	}
	if len(reasons) == 0 {
		return nil
	}
	return errors.New(strings.Join(reasons, "; "))
}

// rigResidueCensusDone is a test seam: closed when a boot census finishes.
var rigResidueCensusDone = func() {}

// startRigResidueCensus runs the controller's one rig containment census, in
// the background so boot never waits on a rig store. A suspended rig is not
// opened (reading it would restart its retired proxy and Dolt); it is named
// unchecked.
func (cr *CityRuntime) startRigResidueCensus(ctx context.Context) {
	cfg, cityPath := cr.cfg, cr.cityPath
	if cfg == nil || len(cfg.Rigs) == 0 || cityPath == "" {
		return
	}
	target, ok, err := resolveInfraBindingTarget(cityPath, cfg)
	if err != nil || !ok {
		return
	}
	suspended := buildEffectiveSuspendedRigNames(cfg, loadSuspensionStateBestEffort(cityPath))
	rigs := make([]config.Rig, 0, len(cfg.Rigs))
	var unchecked []string
	for _, rig := range cfg.Rigs {
		if suspended[rig.Name] {
			unchecked = append(unchecked, rig.Name)
			continue
		}
		rigs = append(rigs, rig)
	}
	census := *cfg
	census.Rigs = rigs
	rec, stderr, logPrefix := cr.rec, cr.stderr, cr.logPrefix
	go func() {
		defer rigResidueCensusDone()
		if ctx.Err() != nil {
			return
		}
		emitRigResidue(rec, stderr, logPrefix, target, censusRigs(cityPath, &census, target, false), unchecked)
	}()
}

// emitRigResidue publishes storage.rig_residue for every rig whose census
// holds the deploy gate, and warns once on stderr.
func emitRigResidue(rec events.Recorder, stderr io.Writer, logPrefix string, target infraBindingTarget, rows []rigCensusRow, unchecked []string) {
	for _, r := range rows {
		if !r.blocking() {
			continue
		}
		line := rigCensusLine(r)
		if stderr != nil {
			fmt.Fprintf(stderr, "%s: storage: %s\n", logPrefix, line) //nolint:errcheck // best-effort stderr
		}
		if rec == nil {
			continue
		}
		count := r.Infra
		raw, err := json.Marshal(storebinding.StorageRigPayload{Rig: r.Rig, Binding: target.Binding, Outcome: string(r.State), Beads: count, Detail: line})
		if err != nil {
			continue
		}
		rec.Record(events.Event{Type: events.StorageRigResidue, Actor: "gc", Subject: r.Rig, Payload: raw})
	}
	if len(unchecked) > 0 && stderr != nil {
		sort.Strings(unchecked)
		fmt.Fprintf(stderr, "%s: storage: suspended rig(s) %s were not censused at boot; `gc storage status` reports them\n", logPrefix, strings.Join(unchecked, ", ")) //nolint:errcheck // best-effort stderr
	}
}

// rigStoreListsEmpty reports whether the rig's store holds no row at all,
// closed and both tiers included. A store that cannot be opened, listed or
// closed is not proven empty.
func rigStoreListsEmpty(cityPath string, r config.Rig) bool {
	// A scope with no store on disk at all holds no row, and opening it
	// would only create (or start a server for) the store being asked about.
	root := resolveStoreScopeRoot(cityPath, r.Path)
	if !fileExists(filepath.Join(root, ".beads")) && !fileExists(filepath.Join(root, ".gc", "beads.json")) {
		return true
	}
	store, err := openStorageScopeStore(r.Path, cityPath)
	if err != nil {
		return false
	}
	rows, listErr := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	closeErr := closeBeadStoreHandle(store)
	return listErr == nil && closeErr == nil && len(rows) == 0
}
