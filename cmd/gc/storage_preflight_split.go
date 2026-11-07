package main

// `gc storage preflight --split` (S5 §2.5): the split rehearsed on a city
// that has not authored [storage].
//
// It builds the configuration the split would write (cfg' = cfg with the
// born-split [storage] block, the one `gc init --storage split` writes) and
// runs the preflight's checks against it, unchanged: every refusal the
// migration makes there, the S5-D holds and the source-lane gates (S5-B)
// included. To those it adds what only a split asks: the session reconciler
// cfg' resolves to and, when that is v2, the boot census v2 refuses over; the
// lane the migration's window would open the work store on; the id floor; and
// estimates of the time the copy and the clear take, the disk the split
// needs, and how many times --time-budget would pause the run.
//
// It is read-only in the preflight's sense, and starts nothing a running city
// would not. The migration's window starts the city's bead-store provider when
// it is down (storage_maintenance.go); a rehearsal must not, so on a city
// whose provider is down, or whose bd-owned proxy is stopped (any bd read
// restarts it), it reports that and what lets a later run read, and runs no
// check that reads a store. Any proxied scope whose proxy is not running
// counts, bd-owned or not, judged as doctor judges it
// (doctorStoreGate.decide): a suspended rig or city is left cold, and a proxy
// that retired on its idle timeout while the city's controller runs is read,
// which restarts it as any bd read on the running city would, until it retires
// again. Where the provider is up it registers, for the reads alone, a window
// that started nothing, holds no lock and carries the city's own
// configuration, only so the work-store opens take the lanes the migration's
// window forces (the proxied-native lane, O9's transport override) and the
// lane they land on is recorded; closing it stops nothing, because it started
// nothing. It never replaces a window already open in the process (the guided
// split's), and the doctor never opens one.
//
// One race is left, and named here rather than closed: a proxy that
// idle-retires between the gate and a read is restarted by that read, as any
// bd read restarts it. The gate is a check, not a lock; the restarted proxy
// retires again on its own idle timeout.
//
// Every estimate is labeled one. The counts are what this run read; the
// rates are measurements, pinned below with their sources.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// storagePreflightSplitFlag selects the split rehearsal.
const storagePreflightSplitFlag = "split"

// The rates the time estimate multiplies the counts by. Each is a
// measurement on a loaded host (load average about 110), and
// TestSplitEstimateConstantsArePinned pins them: changing one changes every
// estimate this command prints.
const (
	// The copy, from BenchmarkInfraMigrateMCShaped at 40,000 rows and 18,662
	// edges (S5-C, #8799), a SQLite work store into the SQLite binding: plan
	// 1.1 s, rows 6.2 s, edges 0.5 s, verify 2.0 s.
	splitPlanPerRow   = 27500 * time.Nanosecond
	splitCopyPerRow   = 155 * time.Microsecond
	splitCopyPerEdge  = 27 * time.Microsecond
	splitVerifyPerRow = 50 * time.Microsecond
	// The clear of a SQLite work store, from the same run: 21.3 s for 40,000
	// rows, their edges included.
	splitSQLiteClearPerRow = 533 * time.Microsecond
	// The clear on native Dolt, on a dolt sql-server (#8799, 300 rows and
	// edges): DepRemoveBatch 62 ms an edge, DepRemove 129 ms an edge (one
	// commit each), the remainder DepList 7.4 ms a row, DeleteBatch 4.6 ms a
	// row.
	splitDoltBatchedPerEdge = 62 * time.Millisecond
	splitDoltPerEdge        = 129 * time.Millisecond
	splitDoltClearPerRow    = 12 * time.Millisecond
)

// The sizes the disk estimate multiplies the rows' JSON by. Measured on the
// MC-shaped fixture (1,000 and 4,000 rows) migrated into a real binding; pinned
// by TestSplitEstimateConstantsArePinned and checked against a real binding by
// TestSplitDiskEstimateCoversARealBinding.
const (
	// splitBindingPerJSONByte is the binding root a copy leaves (the database,
	// the retained-source backup and the manifest) per byte of the moved rows'
	// JSON: 4.9 measured (database 3.3 to 3.6, backup 1.3), with a margin.
	splitBindingPerJSONByte = 6
	// splitSnapshotPerJSONByte is the snapshot's JSONL per byte of the rows'
	// JSON: each entry carries the row, its class and its edges (1.3 measured
	// on the backup, which has the same shape), with a margin.
	splitSnapshotPerJSONByte = 2
)

// splitClearLane is the clear's cost on one work-store lane.
type splitClearLane struct {
	name            string
	perRow, perEdge time.Duration
	// basis says where the rates come from.
	basis string
	// floor marks rates no run measured on this lane: the estimate is a
	// lower bound.
	floor bool
}

// splitClearLaneFor is the clear's lane for the work store an open reported,
// known false when no open in the window reported one.
func splitClearLaneFor(diag beads.BeadsDiagnostic, known bool) splitClearLane {
	perEdge := splitClearLane{perRow: splitDoltClearPerRow, perEdge: splitDoltPerEdge, floor: true}
	switch {
	case !known:
		perEdge.name = "unknown"
		perEdge.basis = "no open reported its lane, so the slowest measured rate is the floor: 129 ms an edge, 12 ms a row"
		return perEdge
	case diag.Store == beads.BeadsStoreNameNativeDoltStore && diag.Proxied == nil:
		return splitClearLane{
			name: "native Dolt", perRow: splitDoltClearPerRow, perEdge: splitDoltBatchedPerEdge,
			basis: "measured on native Dolt: 62 ms an edge in batches, 7.4 + 4.6 ms a row",
		}
	case diag.Store == beads.BeadsStoreNameNativeDoltStore:
		return splitClearLane{
			name: "proxied", perRow: splitDoltClearPerRow, perEdge: splitDoltPerEdge, floor: true,
			basis: "the proxied lane removes edges one at a time, one Dolt commit each, through the proxy, which no run measured; 129 ms an edge (native Dolt, one by one) and 7.4 + 4.6 ms a row are its floor",
		}
	case diag.Store == beads.BeadsStoreNameBdStore || diag.Store == beads.BeadsStoreNameExecStore:
		perEdge.name = "per-edge " + diag.Store
		perEdge.basis = "this lane (bd CLI, hosted bd, exec) removes an edge per process call, which no run measured; 129 ms an edge (native Dolt, one by one) and 12 ms a row are its floor"
		return perEdge
	}
	return splitClearLane{
		name: diag.Store + " (SQLite rates)", perRow: splitSQLiteClearPerRow,
		basis: "measured on a SQLite work store: 21.3 s for 40,000 rows and their edges; this store's own rate is not measured",
	}
}

// splitEstimateInput is what the time estimate counts.
type splitEstimateInput struct {
	// rows and gates are written into the binding; copyEdges are the edges
	// written with them; clearRows and clearEdges are removed from the work
	// store.
	rows, gates, copyEdges, clearRows, clearEdges int
	// The rig imports, which run after the city's (S5 §5.2 P8).
	rigRows, rigCopyEdges, rigClearRows, rigClearEdges int
	lane                                               splitClearLane
}

// splitTimeEstimate is the time estimate, by phase.
type splitTimeEstimate struct {
	copy, clear, rigs time.Duration
}

// estimateSplitTime multiplies the counts by the rates.
func estimateSplitTime(in splitEstimateInput) splitTimeEstimate {
	n := func(v int) time.Duration { return time.Duration(v) }
	perCopiedRow := splitPlanPerRow + splitCopyPerRow + splitVerifyPerRow
	return splitTimeEstimate{
		copy:  n(in.rows+in.gates)*perCopiedRow + n(in.copyEdges)*splitCopyPerEdge,
		clear: n(in.clearRows)*in.lane.perRow + n(in.clearEdges)*in.lane.perEdge,
		rigs: n(in.rigRows)*perCopiedRow + n(in.rigCopyEdges)*splitCopyPerEdge +
			n(in.rigClearRows)*in.lane.perRow + n(in.rigClearEdges)*in.lane.perEdge,
	}
}

// splitBudgetResumes is how many times a run of work under budget pauses
// (exit 75) before one finishes it: each run of the same command gets the
// whole budget again. Zero budget is none.
func splitBudgetResumes(work, budget time.Duration) int {
	if budget <= 0 {
		return 0
	}
	return int(max(work-1, 0) / budget)
}

// splitBudgetAdvice is the --time-budget row: whether it is a note, and its
// text. city is the city's copy and clear estimate; floor marks a lane whose
// rates are a lower bound, so neither "would not pause" nor the pause count
// can be promised there.
func splitBudgetAdvice(city, budget time.Duration, floor bool) (note bool, text string) {
	about, atLeast := "about "+splitDuration(city), ""
	if floor {
		about, atLeast = "at least "+splitDuration(city), "at least "
	}
	fits := splitBudgetFitting(city)
	resumes := splitBudgetResumes(city, budget)
	switch {
	case budget <= 0:
		return false, fmt.Sprintf("no --%s: the run does not pause, and the city stays stopped for all of it (%s)", storageTimeBudgetFlag, about)
	case resumes == 0 && !floor:
		return false, fmt.Sprintf("ESTIMATE: the city's copy and clear (%s) fit --%s %s; the run would not pause", about, storageTimeBudgetFlag, budget)
	case resumes == 0:
		return true, fmt.Sprintf("ESTIMATE: the city's copy and clear take %s against --%s %s; this lane's rate is not measured, so whether the run pauses is not known. `--%s %s` or more runs the estimate in one", about, storageTimeBudgetFlag, budget, storageTimeBudgetFlag, fits)
	}
	fitsIn := fmt.Sprintf("`--%s %s` runs it in one", storageTimeBudgetFlag, fits)
	if floor {
		fitsIn = fmt.Sprintf("`--%s %s` or more may run it in one", storageTimeBudgetFlag, fits)
	}
	return true, fmt.Sprintf("ESTIMATE: the city's copy and clear take %s, over --%s %s: the run would pause (exit %d) %s%d time(s) and need %s%d resume(s) of the same command, and the city stays stopped until the last run finishes. %s",
		about, storageTimeBudgetFlag, budget, storagePausedExitCode, atLeast, resumes, atLeast, resumes, fitsIn)
}

// splitBudgetFitting is a --time-budget the estimate fits in one run: a
// quarter over it, rounded up to the minute.
func splitBudgetFitting(city time.Duration) time.Duration {
	d := city + city/4
	if rem := d % time.Minute; rem != 0 || d == 0 {
		d += time.Minute - rem
	}
	return d
}

// splitDiskEstimate is the disk estimate, in bytes.
type splitDiskEstimate struct {
	snapshot, binding, wal int64
	// exact names the work store's exact copy in the snapshot (S5 §5.3).
	exact string
	// rigs is the part of snapshot the rigs' stores take.
	rigs int64
}

func (d splitDiskEstimate) total() int64 { return d.snapshot + d.binding + d.wal }

// splitSnapshotBytes sizes one store in the mandatory snapshot from its rows'
// JSON: its JSONL, plus the exact copy when that is a file.
func splitSnapshotBytes(rowsJSON int64, exact string) int64 {
	jsonl := splitSnapshotPerJSONByte * rowsJSON
	if exact == snapshotMethodBdExport || exact == snapshotMethodFileCopy {
		return 2 * jsonl
	}
	return jsonl
}

// estimateSplitDisk sizes the split: the mandatory snapshot of the work store
// and, rigs, of the rigs (splitRigSnapshotBytes), the binding root the copy
// leaves, and the WAL growth S5-E's snapshot allows for.
func estimateSplitDisk(allJSON, movedJSON int64, exact string, rigs int64) splitDiskEstimate {
	return splitDiskEstimate{
		snapshot: splitSnapshotBytes(allJSON, exact) + rigs,
		binding:  splitBindingPerJSONByte * movedJSON,
		wal:      storageSnapshotWALGrowthFloor,
		exact:    exact,
		rigs:     rigs,
	}
}

// splitRigSnapshotBytes sizes the rigs in the snapshot Ready names (`gc
// storage snapshot --work --rigs`): every rig snapshotRigs selects, sized as
// the work store is, from the rows the census read from its store. The chain
// cleared, so the census read every one. No open reports a rig's lane, so a
// rig whose kind could take a Dolt tag is sized with a bd export, the larger.
func splitRigSnapshotBytes(request storageOperatorRequest, census []rigCensusRow) (size int64, rows int) {
	read := make(map[string][]beads.Bead, len(census))
	for _, r := range census {
		read["rig:"+r.Rig] = r.Rows
	}
	for _, scope := range snapshotRigs(request) {
		exact := snapshotExactMethod(rawBeadsProviderForScope(scope.root, request.CityPath), false)
		size += splitSnapshotBytes(splitJSONBytes(read[scope.name]), exact)
		rows += len(read[scope.name])
	}
	return size, rows
}

// splitEstimate is everything the rehearsal estimated.
type splitEstimate struct {
	input   splitEstimateInput
	time    splitTimeEstimate
	resumes int
	disk    splitDiskEstimate
}

// splitPreflightSummary is what one rehearsal concluded, for a caller that
// words it its own way: the guided split's first phase (S5 §5.2 P0).
type splitPreflightSummary struct {
	// Blocks names each check that would refuse the split.
	Blocks []string
	// Unread says why no check that reads a store ran, or "", and
	// UnreadRemedy what lets a later run read them.
	Unread, UnreadRemedy string
	// Reconciler is the session reconciler the split city resolves to.
	Reconciler string
	// Estimate is nil when the work store was not read.
	Estimate *splitEstimate
	// RigImports reports a rig whose import moves rows after the city's.
	RigImports bool
}

// ready reports a rehearsal that would clear the split.
func (s splitPreflightSummary) ready() bool { return len(s.Blocks) == 0 && s.Unread == "" }

// splitHypotheticalConfig is cfg as the split leaves it: the born-split
// [storage] block, nothing else changed.
func splitHypotheticalConfig(cfg *config.City) *config.City {
	split := *cfg
	split.Storage = bornSplitStorageConfig()
	return &split
}

// doStorageSplitPreflight rehearses the split and reports it.
func doStorageSplitPreflight(request storageOperatorRequest, stdout, stderr io.Writer) (int, splitPreflightSummary) {
	var summary splitPreflightSummary
	block := func(check string, cause error) {
		preflightBlockRow(stdout, check, cause)
		summary.Blocks = append(summary.Blocks, check)
	}
	fmt.Fprintf(stdout, "city: %s\n", request.CityPath) //nolint:errcheck // best-effort stdout

	// The config the split writes. A city that already authored one has a
	// split to rehearse with the plain preflight, and the guided split
	// writes no second one.
	if request.Cfg.Storage != nil {
		block("config", fmt.Errorf("this city already authors [storage], so there is no split to rehearse: `gc storage %s` (without --%s) rehearses the migration it describes", storagePreflightVerb, storagePreflightSplitFlag))
		return splitPreflightFooter(stdout, request, summary), summary
	}
	if blocked, held := revertHoldingNote(storageSplitNone, request.CityPath); held {
		block("config", errors.New(preflightAdviceDetail(blocked)))
		return splitPreflightFooter(stdout, request, summary), summary
	}
	split := splitHypotheticalConfig(request.Cfg)
	fmt.Fprintf(stdout, "the [storage] block the split writes (hypothetical; nothing is written):\n\n%s\n", splitStorageTOML(split)) //nolint:errcheck // best-effort stdout
	preflightPass(stdout, "config", "city.toml authors no [storage]; the split writes the block above")

	// The reconciler, before and after: an existing city moves its storage
	// and its reconciler in one step. A mode the split city would refuse to
	// start in is a refusal here.
	_, today := doctorReconcilerMode(request.Cfg, request.CityPath, nil)
	after, resolved := doctorReconcilerMode(split, request.CityPath, nil)
	summary.Reconciler = strings.TrimPrefix(resolved, "session reconciler: ")
	detail := fmt.Sprintf("after the split: %s; today: %s", summary.Reconciler, strings.TrimPrefix(today, "session reconciler: "))
	switch _, err := resolveReconcilerMode(split, request.CityPath, reconcilerModeLookupEnv); {
	case err != nil:
		block("reconciler", fmt.Errorf("the split city would not start: %s", detail))
	case after == reconcilerV2:
		preflightPass(stdout, "reconciler", detail)
	default:
		preflightNote(stdout, "reconciler", detail)
	}
	splitPreflightVouch(request.CityPath, stdout)

	// Nothing below may start the provider.
	if why, remedy := splitStoreReadGate(request.CityPath, request.Cfg); why != "" {
		summary.Unread, summary.UnreadRemedy = why, remedy
		preflightNote(stdout, "source lane", why+"; the split starts it in its maintenance window, and the lane is decided there")
		return splitPreflightFooter(stdout, request, summary), summary
	}
	if after == reconcilerV2 {
		splitPreflightCensus(request, split, stdout, block)
		preflightNote(stdout, "v2 demand", "the split runs legacy's demand-migration repairs in its window, before the copy; `gc doctor --check v2-demand-migrations` counts them")
	}

	// The migration's checks against cfg', unchanged, and the reads that
	// follow them, inside the lane window and only there.
	rehearsal := request
	rehearsal.Cfg = split
	found := preflightFindings{quiet: true}
	closeWindow := openPreflightLaneWindow(request.CityPath, request.Cfg)
	code := runStoragePreflightChecks(rehearsal, &found, stdout, stderr)
	diag, known := migrationWindowLane(request.CityPath)
	if code == 0 && found.read && !found.converged {
		splitPreflightFloor(request.CityPath, found, stdout)
	}
	if found.converged {
		block("cutover", splitConvergedRefusal(request.CityPath, split))
	}
	closeWindow()
	if code != 0 {
		summary.Blocks = append(summary.Blocks, found.blocked)
	}
	lane := splitClearLaneFor(diag, known)
	if known {
		preflightPass(stdout, "source lane", fmt.Sprintf("the work store opens on %s in the window (%s lane)", diag.Store, lane.name))
	}
	switch {
	case !found.read:
		preflightNote(stdout, "estimate", "not computed: the checks above stopped before the work store was read")
		return splitPreflightFooter(stdout, request, summary), summary
	case code != 0 || found.converged:
		preflightNote(stdout, "estimate", "not computed: a check above blocked, so the copy's plan is partial")
		return splitPreflightFooter(stdout, request, summary), summary
	}
	for _, r := range found.rigs {
		summary.RigImports = summary.RigImports || (r.Plan != nil && len(r.Plan.Move)+len(r.Plan.Retained) > 0)
	}
	summary.Estimate = splitPreflightEstimate(request, found, lane, diag, known, stdout, block)
	return splitPreflightFooter(stdout, request, summary), summary
}

// splitConvergedRefusal is the refusal for an unsplit city whose split
// binding already carries a convergence marker: a city migrated and then
// backed out by removing [storage] before any split boot. Its infrastructure
// state may live in the binding, so migrating again would copy the work
// store's slice over it. It reports what boot's own check (identity, then
// containment, confirmInfraConvergence's halves) finds there, read-only: the
// marker's identity is not back-filled, as boot's check would.
func splitConvergedRefusal(cityPath string, split *config.City) error {
	target, ok, err := resolveInfraBindingTarget(cityPath, split)
	if err != nil {
		return fmt.Errorf("a convergence marker for the split binding exists, and the binding cannot be resolved to check it (%w)", err)
	}
	if !ok {
		return errors.New("a convergence marker for the split binding exists, and the split config resolves no binding to check it against")
	}
	const prefix = "gc storage preflight --split"
	_, _, hold := markerWorkStoreIdentityHold(cityPath, target, prefix, io.Discard)
	var report infraMigrationReport
	if hold != nil {
		report = *hold
	} else {
		report = confirmInfraContainment(cityPath, target, prefix, io.Discard)
	}
	report.Target = target
	switch report.Outcome {
	case infraMigrationConverged:
		return fmt.Errorf("%s records that this city already moved its infrastructure classes into binding %s, and boot's check confirms the binding holds them: city.toml lost its [storage] block, not its split. Restore the block above and run `%s`; migrating again would copy the work store's slice over the binding", target.MarkerPath(), target.Binding, storageStatusInstruction())
	case infraMigrationIdentityMismatch:
		return fmt.Errorf("%s: %s", report.Outcome, strings.TrimPrefix(infraMigrationOperatorAdvice(report, prefix), prefix+": "))
	case infraMigrationRetained, infraMigrationStranded:
		// No [storage] and no served note: the binding was never served, so
		// the work store is the current copy and boot's remedy for a served
		// binding (clear the work store's copies) would lose its changes.
		return fmt.Errorf("%s records a copy into binding %s that this city never served: city.toml has no [storage] and no note says it served one, so the work store holds the current rows, and boot's check of the binding reports %s. %s is a leftover. Move it aside and run this again. Restoring the [storage] block and clearing instead would discard every change made in the work store since %s was written", target.MarkerPath(), target.Binding, report.Outcome, target.Root, target.MarkerPath())
	}
	if report.Fault == nil {
		report.Fault = errors.New("no reason recorded")
	}
	return fmt.Errorf("%s records an earlier copy into binding %s, and boot's check of it could not finish (%s: %w). Resolve that, or move %s aside, and run this again", target.MarkerPath(), target.Binding, report.Outcome, report.Fault, target.Root)
}

// splitStorageTOML renders cfg's [storage] as city.toml spells it.
func splitStorageTOML(cfg *config.City) string {
	var out bytes.Buffer
	enc := toml.NewEncoder(&out)
	enc.Indent = ""
	if err := enc.Encode(struct {
		Storage *config.StorageConfig `toml:"storage"`
	}{cfg.Storage}); err != nil {
		return fmt.Sprintf("(the block could not be rendered: %v)", err)
	}
	return strings.TrimSpace(out.String()) + "\n"
}

// splitPreflightVouch reports the city scope's bd trust verdict (S5 §2.5): a
// split city's gc bd runs only a bd whose command tree matched a pinned
// inventory or that the city vouched for. It reads the verdict cache and the
// vouch file only: no walk, no lock, no event, so an unjudged binary is
// reported as not checked, never as OK.
func splitPreflightVouch(cityPath string, stdout io.Writer) {
	verdict, ok := splitBdVouchStatus(cityPath)
	if ok {
		preflightPass(stdout, "bd vouch", verdict)
		return
	}
	preflightNote(stdout, "bd vouch", verdict)
}

// splitBdVouchStatus is the city scope's bd verdict as the cache and the
// vouch file record it, and whether it admits the binary.
func splitBdVouchStatus(cityPath string) (string, bool) {
	if !cityUsesBdStoreContract(cityPath) {
		return "n/a: the city's work store is not served through bd", true
	}
	bdPath, err := resolveBdBinaryForScope(cityPath, cityPath)
	if err != nil {
		return "not checked: " + err.Error(), false
	}
	id, err := statBdBinary(bdPath)
	if err != nil {
		return "not checked: " + err.Error(), false
	}
	set := bdflags.InventorySetFingerprint()
	for _, e := range loadBdTrustCache(cityPath).Entries {
		if e.bdBinaryID != id || e.InventorySet != set || e.WalkError != "" {
			continue
		}
		if e.Inventory != "" && bdflags.InventoryByName(e.Inventory) != nil {
			return fmt.Sprintf("%s matched inventory %s (cached verdict)", id.Path, e.Inventory), true
		}
		return fmt.Sprintf("%s matches no pinned inventory (cached verdict), so the split city's gc bd refuses it: %s", id.Path, strings.Join(e.Diffs, "; ")), false
	}
	if sum, err := fileSHA256(id.Path); err == nil {
		if v, ok := findBdVouch(cityPath, id.Path, sum, set); ok {
			return fmt.Sprintf("%s is vouched as inventory %s", id.Path, v.Inventory), true
		}
	}
	return fmt.Sprintf("not checked: no cached verdict or vouch for %s; `gc storage vouch-bd` walks it", id.Path), false
}

// splitPreflightSessionLegs opens the census legs read-only; tests replace
// it, having no real session stores.
var splitPreflightSessionLegs = openV2SessionLegs

// splitPreflightCensus runs v2's boot census over the city's session legs as
// they are now, judged as the split city's v2 judges them: the split refuses
// on it inside its window (S5 §5.2 P2).
func splitPreflightCensus(request storageOperatorRequest, split *config.City, stdout io.Writer, block func(string, error)) {
	check := newV2SessionMigrationCheck(split, request.CityPath)
	check.openLegs = func() []v2SessionLeg { return splitPreflightSessionLegs(request.CityPath, request.Cfg) }
	census := check.census()
	switch refusal := census.m.refusal(); {
	case refusal != nil:
		block("v2 sessions", fmt.Errorf("v2's boot census refuses this city's session rows (%w); the split refuses on it. `gc doctor --check v2-session-migration` lists them", refusal))
	case census.faulted > 0:
		block("v2 sessions", fmt.Errorf("%d session leg(s) could not be read, so v2's boot census cannot be cleared; `gc doctor --check v2-session-migration` names them", census.faulted))
	case census.notChecked > 0:
		preflightNote(stdout, "v2 sessions", fmt.Sprintf("%d open session row(s), none refused; %d leg(s) have no read-only open and were not checked", len(census.rows), census.notChecked))
	default:
		preflightPass(stdout, "v2 sessions", fmt.Sprintf("%d open session row(s); v2's boot census refuses none", len(census.rows)))
	}
}

// splitPreflightFloor reports the city work store's id floor the split sets
// (S5 §8.2).
func splitPreflightFloor(cityPath string, found preflightFindings, stdout io.Writer) {
	ids := make([]string, 0, len(found.rows))
	for _, b := range found.rows {
		ids = append(ids, b.ID)
	}
	plan, err := planCityIDFloor(cityPath, ids)
	switch {
	case err != nil:
		preflightNote(stdout, "id floor", "not read: "+err.Error())
	case plan.Skipped != "":
		preflightPass(stdout, "id floor", "n/a: "+plan.Skipped)
	case plan.Need == 0:
		preflightPass(stdout, "id floor", "no carried id of the work store's prefix needs one")
	case !plan.below():
		preflightPass(stdout, "id floor", fmt.Sprintf("%s is already %d, at or above the %d the carried ids need", bdMinHashLengthKey, plan.Current, plan.Need))
	case plan.Hosted != "":
		preflightNote(stdout, "id floor", fmt.Sprintf("the work store is hosted bd at %s, at %s=%d: set %s=%d (and %s at least %d) on the hosted project through the bd serve admin path, out of band (owner decision O10), before the window", plan.Hosted, bdMinHashLengthKey, plan.Current, bdMinHashLengthKey, plan.Need, bdMaxHashLengthKey, plan.Need))
	default:
		preflightPass(stdout, "id floor", fmt.Sprintf("the split raises %s from %d to %d", bdMinHashLengthKey, plan.Current, plan.Need))
	}
}

// splitPreflightEstimate prints the estimates and blocks on a disk the split
// would not fit.
func splitPreflightEstimate(request storageOperatorRequest, found preflightFindings, lane splitClearLane, diag beads.BeadsDiagnostic, known bool, stdout io.Writer, block func(string, error)) *splitEstimate {
	est := &splitEstimate{input: splitEstimateInputFor(found, lane)}
	est.time = estimateSplitTime(est.input)
	city := est.time.copy + est.time.clear
	est.resumes = splitBudgetResumes(city, request.TimeBudget)
	in := est.input
	about := "about"
	if lane.floor {
		about = "at least"
	}
	preflightNote(stdout, "estimate", fmt.Sprintf("ESTIMATE, not a measurement: the city's copy about %s (%d row(s), %d gate(s), %d edge(s)) and clear %s %s (%d row(s), %d edge(s), %s lane)",
		splitDuration(est.time.copy), in.rows, in.gates, in.copyEdges, about, splitDuration(est.time.clear), in.clearRows, in.clearEdges, lane.name))
	fmt.Fprintf(stdout, "%sbased on: the row and edge counts read above, read-only; copy rates %s a row and %s an edge, from BenchmarkInfraMigrateMCShaped (40,000 rows, a SQLite work store, a loaded host); clear: %s\n", //nolint:errcheck // best-effort stdout
		preflightDetailIndent, splitPlanPerRow+splitCopyPerRow+splitVerifyPerRow, splitCopyPerEdge, lane.basis)
	if in.rigRows+in.rigClearRows > 0 {
		fmt.Fprintf(stdout, "%sthe rig imports: %s %s more (%d row(s) to copy, %d to clear), outside --%s: the rig import takes none\n", //nolint:errcheck // best-effort stdout
			preflightDetailIndent, about, splitDuration(est.time.rigs), in.rigRows, in.rigClearRows, storageTimeBudgetFlag)
	}
	if note, text := splitBudgetAdvice(city, request.TimeBudget, lane.floor); note {
		preflightNote(stdout, "time budget", text)
	} else {
		preflightPass(stdout, "time budget", text)
	}

	exact := snapshotExactMethod(rawBeadsProviderForScope(request.CityPath, request.CityPath), known && diag.Store == beads.BeadsStoreNameNativeDoltStore && diag.Proxied == nil)
	all := make([]beads.Bead, 0, len(found.all))
	for _, b := range found.all {
		all = append(all, b)
	}
	allJSON := splitJSONBytes(all)
	moved := append([]beads.Bead(nil), found.rows...)
	for _, r := range found.rigs {
		if r.Plan != nil {
			moved = append(moved, r.Plan.Move...)
		}
	}
	rigs, rigRows := splitRigSnapshotBytes(request, found.rigs)
	est.disk = estimateSplitDisk(allJSON, splitJSONBytes(moved), exact, rigs)
	dir := filepath.Join(request.CityPath, ".gc")
	snapshot := fmt.Sprintf("the work store's %s rows' JSONL, exact copy: %s", strconv.Itoa(len(found.all)), est.disk.exact)
	if rigRows > 0 {
		snapshot += fmt.Sprintf("; the rigs' %d rows' JSONL and exact copies, %s", rigRows, splitBytes(est.disk.rigs))
	}
	sizes := fmt.Sprintf("ESTIMATE: about %s: the snapshot %s (%s), the binding %s (%d times the moved rows' %s of JSON, measured 4.9), WAL growth %s",
		splitBytes(est.disk.total()), splitBytes(est.disk.snapshot), snapshot, splitBytes(est.disk.binding), splitBindingPerJSONByte, splitBytes(splitJSONBytes(moved)), splitBytes(est.disk.wal))
	free, err := doltContainerFreeBytesFunc(dir)
	if err != nil {
		preflightNote(stdout, "disk", sizes+"; free space on "+dir+" not measured: "+err.Error())
		return est
	}
	critical, warn := doltDiskMinFreeBytes(), doltDiskWarnFreeBytes()
	switch refuse, low := snapshotDiskVerdict(free, est.disk.total(), critical, warn); {
	case refuse:
		block("disk", fmt.Errorf("%s; %s has %s free and must keep %s free for a managed Dolt on the same filesystem. Free space before the window", sizes, dir, splitBytes(free), splitBytes(critical)))
	case low:
		preflightNote(stdout, "disk", fmt.Sprintf("%s; %s has %s free, which leaves less than the %s warning floor", sizes, dir, splitBytes(free), splitBytes(warn)))
	default:
		preflightPass(stdout, "disk", fmt.Sprintf("%s; %s has %s free", sizes, dir, splitBytes(free)))
	}
	return est
}

// splitEstimateInputFor counts what the copy, the clear and the rig imports
// would move.
func splitEstimateInputFor(found preflightFindings, lane splitClearLane) splitEstimateInput {
	in := splitEstimateInput{
		rows:       len(found.plan.rows),
		gates:      len(found.plan.boundary.Gates),
		copyEdges:  len(found.plan.boundary.Copy) + len(found.plan.boundary.Gates),
		clearRows:  len(found.plan.rows),
		clearEdges: splitClearEdgeCount(found.plan),
		lane:       lane,
	}
	for _, r := range found.rigs {
		if r.Plan == nil {
			continue
		}
		p := r.Plan
		in.rigRows += len(p.Move) + len(p.Gates)
		in.rigCopyEdges += len(p.Copy) + len(p.Gates)
		in.rigClearRows += len(p.Move) + len(p.Retained)
		in.rigClearEdges += len(p.Copy)
		for _, records := range p.Records {
			in.rigClearEdges += len(records)
		}
	}
	return in
}

// splitClearEdgeCount is the edges the clear removes: every edge touching a
// moved row, each once (detachInfraClearedRow).
func splitClearEdgeCount(plan infraCopyPlan) int {
	seen := map[beads.DepPair]bool{}
	for _, b := range plan.rows {
		for _, d := range plan.down[b.ID] {
			seen[beads.DepPair{IssueID: b.ID, DependsOnID: d.DependsOnID}] = true
		}
		for _, d := range plan.up[b.ID] {
			seen[beads.DepPair{IssueID: d.IssueID, DependsOnID: b.ID}] = true
		}
	}
	return len(seen)
}

// splitJSONBytes is the rows' JSON size, the unit the disk estimate scales.
func splitJSONBytes(rows []beads.Bead) int64 {
	var n int64
	for _, b := range rows {
		if data, err := json.Marshal(b); err == nil {
			n += int64(len(data))
		}
	}
	return n
}

// splitPreflightFooter closes the report and returns the exit code. Ready
// names the window's commands in order, each carrying the rehearsal's --force
// attestations: the snapshot the disk estimate counts (migrate takes none),
// the [storage] block printed above, then the migrations, the city's with the
// --time-budget the estimate was made against (the rig import takes none).
func splitPreflightFooter(stdout io.Writer, request storageOperatorRequest, summary splitPreflightSummary) int {
	prefix := storagePreflightLogPrefix + " --" + storagePreflightSplitFlag
	switch {
	case len(summary.Blocks) > 0:
		fmt.Fprintf(stdout, "\n%s: the split would refuse (%s). Fix the blocking checks above and run this again.\n", prefix, strings.Join(summary.Blocks, ", ")) //nolint:errcheck // best-effort stdout
		return 1
	case summary.Unread != "":
		fmt.Fprintf(stdout, "\n%s: the checks that read the city's stores did not run, because this command starts nothing. %s.\n", prefix, summary.UnreadRemedy) //nolint:errcheck // best-effort stdout
		return 1
	}
	force := request.Force.flags()
	snapshot := []string{"gc storage " + storageSnapshotVerb, "--work"}
	if len(request.Cfg.Rigs) > 0 {
		snapshot = append(snapshot, "--rigs")
	}
	snapshot = append(append(snapshot, "--"+storageFleetStoppedFlag), force...)
	migrate := append(append([]string{storageClearInstruction()}, force...), fmt.Sprintf("--%s %s", storageTimeBudgetFlag, request.TimeBudget))
	steps := []string{
		strings.Join(snapshot, " ") + "  (the disk estimate counts this snapshot; migrate takes none)",
		"add the [storage] block printed above to city.toml",
		strings.Join(migrate, " "),
	}
	if summary.RigImports {
		steps = append(steps, strings.Join(append([]string{rigImportAllInstruction()}, force...), " "))
	}
	fmt.Fprintf(stdout, "\nReady. The figures marked ESTIMATE are estimates. Stop the fleet, then, in order (the city stays stopped until the last finishes):\n") //nolint:errcheck // best-effort stdout
	for i, step := range steps {
		fmt.Fprintf(stdout, "  %d. %s\n", i+1, step) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// What lets a later rehearsal read the stores this one could not.
const (
	// splitUnreadWhileRunning: nothing answered the city's controller, so the
	// stores are down because the city is; a running city has them up.
	splitUnreadWhileRunning = "Run it again while the city is running"
	// splitUnreadResolve: the gate itself could not decide.
	splitUnreadResolve = "Resolve that, then run this again"
)

// splitStoreReadGate says why the rehearsal may not read the city's stores
// without starting something, or "" when it may, and what lets a later run
// read them. It may when every scope's server is up, or the caller holds the
// migration's window, which owns them. A proxied scope, bd-owned or not, is
// judged by splitScopeGate.stopped, at the root the reads open (rigStoreRoot,
// for a relative [[rigs]] path too); the city's other providers, by the
// window's own down-probe.
func splitStoreReadGate(cityPath string, cfg *config.City) (why, remedy string) {
	if activeMigrationWindow(cityPath) != nil {
		return "", ""
	}
	gate := splitScopeGate{
		suspended: suspendedBeadsScopes(cityPath, cfg),
		running:   sync.OnceValue(func() bool { return infraMigrationForeignControllerPID(cityPath) != 0 }),
	}
	if why, remedy := gate.stopped(cityPath, ""); why != "" {
		return why, remedy
	}
	owned, err := cityScopeProviderOwned(cityPath)
	if err != nil {
		return fmt.Sprintf("whether bd owns this city's proxied server cannot be told (%v)", err), splitUnreadResolve
	}
	if !owned {
		down, err := migrationWindowProviderDown(cityPath)
		if err != nil {
			return fmt.Sprintf("the bead-store provider could not be checked (%v)", err), splitUnreadResolve
		}
		if down {
			return fmt.Sprintf("the city's bead-store provider (%s) is down", beadsProvider(cityPath)), gate.downRemedy("its bead-store provider", "`gc doctor` shows why")
		}
	}
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		if why, remedy := gate.stopped(rigStoreRoot(cityPath, rig), rig.Name); why != "" {
			return why, remedy
		}
	}
	return "", ""
}

// splitScopeGate judges a scope whose proxied server is not running as
// doctor's store gate does (doctorStoreGate.decide).
type splitScopeGate struct {
	suspended beadsScopeSuspension
	// running reports a controller answering on the city. It is asked only
	// when a stopped scope needs the answer.
	running func() bool
}

// stopped says why the rehearsal may not read the store at root, rig's or
// the city's (rig ""), and what lets a later run read it, or "" when it may:
// its proxy runs, or it retired on its idle timeout while the city's
// controller runs, and the read restarts it as any bd read would. A suspended
// scope is left cold and named with the command that resumes it.
func (g splitScopeGate) stopped(root, rig string) (why, remedy string) {
	if !doctorProxiedStoreNotRunning(root) {
		return "", ""
	}
	store, scope, resume := "the city's work store", "the city", "gc resume"
	if rig != "" {
		store = "rig " + rig
		if !g.suspended.City() {
			scope, resume = store, "gc rig resume "+rig
		}
	}
	switch {
	case g.suspended.Suspended(root):
		return fmt.Sprintf("%s is suspended, and the proxied server for %s is not running; any bd read would start it", scope, store),
			fmt.Sprintf("The split's window starts the proxied server for %s. To rehearse with it running, resume %s (`%s`), run this again, then suspend %s again", store, scope, resume, scope)
	case !g.running():
		return fmt.Sprintf("the proxied server for %s is not running, and any bd read would start it", store), splitUnreadWhileRunning
	case doctorProxiedStoreRetiresWhenIdle(root):
		return "", ""
	}
	return fmt.Sprintf("the proxied server for %s is not running while the city's controller is, and it does not retire on an idle timeout", store),
		g.downRemedy("the proxied server for "+store, "`gc start` (or `gc bd dolt start` in the scope) restarts it, and the proxied-endpoint line in `gc doctor` shows why it is down")
}

// downRemedy is what lets a later run read a store whose server, what, is
// down: with no controller answering, a running city; with one running, the
// server went down on its own, and fix says what restarts it or why.
func (g splitScopeGate) downRemedy(what, fix string) string {
	if !g.running() {
		return splitUnreadWhileRunning
	}
	return fmt.Sprintf("The city's controller is running, so %s went down on its own: %s. Run this again once it is up", what, fix)
}

// openPreflightLaneWindow registers a window on cityPath that started nothing,
// holds no lock and carries cfg, the city's own configuration, so the
// rehearsal's opens take the window's lanes and record the one they land on.
// It returns the closer, which stops nothing. Inside any window already open
// in the process (the guided split's) it registers none and its closer
// touches nothing.
func openPreflightLaneWindow(cityPath string, cfg *config.City) func() {
	w := &migrationSourceWindow{cityPath: cityPath, cfg: cfg}
	migrationWindows.Lock()
	defer migrationWindows.Unlock()
	if migrationWindows.active != nil {
		return func() {}
	}
	migrationWindows.active = w
	return func() { _ = w.Close() }
}

// migrationWindowLane is the lane the window open on cityPath recorded.
func migrationWindowLane(cityPath string) (beads.BeadsDiagnostic, bool) {
	migrationWindows.Lock()
	defer migrationWindows.Unlock()
	if w := migrationWindows.active; w != nil && samePath(w.cityPath, cityPath) && w.laneKnown {
		return w.lane, true
	}
	return beads.BeadsDiagnostic{}, false
}

// splitDuration renders an estimated duration to a precision it has.
func splitDuration(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return d.Round(time.Second).String()
	case d >= time.Second:
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Millisecond).String()
}

// splitBytes renders a size in binary units.
func splitBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
