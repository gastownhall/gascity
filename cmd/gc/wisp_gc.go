package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/storeref"
)

// closeAbandonedEnv is the opt-in environment variable that enables the
// abandoned-root closer. It DEFAULTS TO DRY-RUN: with the variable unset (or
// not truthy) closeAbandonedRoots only logs/counts the roots it WOULD close
// and never mutates the store. Set it to a truthy value ("1", "true", "yes",
// "on") to actually close abandoned open roots. The dry-run default lets this
// change be cherry-picked onto a live branch for observation before enforcing.
const closeAbandonedEnv = "GC_WISP_GC_CLOSE_ABANDONED"

// reapOrphansEnv is the opt-in environment variable that enables reaping of
// orphaned closed wisp descendants. Like closeAbandonedEnv it DEFAULTS TO
// DRY-RUN: with the variable unset (or not truthy) reapOrphanedClosedWisps only
// counts/logs the descendants it WOULD reap and never mutates the store. Set it
// to a truthy value ("1", "true", "yes", "on") to actually delete them.
const reapOrphansEnv = "GC_WISP_GC_REAP_ORPHANS"

// workMoleculesEnv is the opt-in environment variable that lets the wisp GC's
// WORK arm mutate a work ledger: closing abandoned v1 poured molecule roots
// and purging expired closed ones on the work legs of a split city
// (workMoleculeGC, wisp_gc_work_lane.go). Like reapOrphansEnv it DEFAULTS TO DRY-RUN:
// unset (or not truthy), the arm only logs the roots it WOULD close or purge.
// A work ledger holds the operator's own records, so this is opt-in on its
// own, separately from the graph-class sweeps. The abandoned-root closes it
// performs also still require closeAbandonedEnv.
const workMoleculesEnv = "GC_WISP_GC_WORK_MOLECULES"

// workMoleculesEnforced reports whether the work arm may mutate (true) or runs
// dry (false). A package var so tests can flip it.
var workMoleculesEnforced = func() bool {
	return parseBoolEnv(os.Getenv(workMoleculesEnv))
}

// abandonedRootCloseReason is the close_reason stamped on open workflow roots
// closed by the periodic abandoned-root sweep (distinct from the reactive
// moleculeAutocloseReason so an operator reading bd show can tell a periodic
// GC close apart from an edge-triggered child-close autoclose).
const abandonedRootCloseReason = "wisp gc: abandoned root closed — all descendants terminal and root idle past TTL"

// wispGCCloseAbandonedTTL is the conservative minimum idle age an open root
// must reach (no activity newer than now-TTL) before the abandoned-root sweep
// will close it. It is a package var so tests can shrink it; it deliberately
// exceeds the controller tick AND the external operational reconciler cadence
// (which reaps residue within ~1h) so live/in-flight roots and the reconciler
// are never raced. Defaults to 24h, matching the closed-wisp purge TTL.
var wispGCCloseAbandonedTTL = 24 * time.Hour

// closeAbandonedEnforced reports whether the abandoned-root closer should
// actually close (true) or run dry (false). It is a package var so tests can
// flip it without touching the process environment. By default it reads
// closeAbandonedEnv, which is unset in production until an operator opts in.
var closeAbandonedEnforced = func() bool {
	return parseBoolEnv(os.Getenv(closeAbandonedEnv))
}

// wispGCReapOrphanBatchCap bounds how many orphaned closed wisp descendants a
// single sweep will reap. The orphaned-closure backlog (~8k rows) drains over
// roughly cap-sized batches across successive sweeps so no single tick does an
// unbounded amount of deletion work. Package var so tests can shrink it.
var wispGCReapOrphanBatchCap = 500

// wispGCReapOrphanProbeCap bounds how many rootless candidates one sweep will
// probe for leaf-ness (Children/DepList). Unlike wispGCReapOrphanBatchCap it
// applies in DRY-RUN TOO: the dry run performs no deletes but still pays one
// backend read per aged rootless candidate, so without this bound the default
// config does unbounded reads per tick against the very backlog this reaps.
// Package var so tests can shrink it.
var wispGCReapOrphanProbeCap = 500

// wispGCClosurePurgeBatchCap bounds how many closed-root ownership closures a
// single sweep will purge. Like wispGCReapOrphanBatchCap it caps DELETE
// ATTEMPTS per tick — counting failures, not just successful purges — so the
// first-deploy backlog of newly-collectible closed graph.v2/workflow roots, and
// a failing delete backend, drain across successive ticks instead of one
// unbounded pass. Package var so tests can shrink it.
var wispGCClosurePurgeBatchCap = 500

// infraSessionPurgeAgeDefault is how long a closed infra session bead is kept.
// Three days, not the Dolt reaper's 30-day SESSION_PURGE_AGE: these rows are
// coordination records, and a 30-day tail is what made the graph file large
// enough for SQLITE_BUSY_SNAPSHOT. Override with GC_INFRA_SESSION_PURGE_AGE.
const infraSessionPurgeAgeDefault = 72 * time.Hour

// infraSessionPurgeAge is the idle age a closed session bead must reach
// before purgeClosedInfraSessions deletes it. Tests replace it.
var infraSessionPurgeAge = defaultInfraSessionPurgeAge

func defaultInfraSessionPurgeAge() time.Duration {
	raw := strings.TrimSpace(os.Getenv("GC_INFRA_SESSION_PURGE_AGE"))
	if raw == "" {
		return infraSessionPurgeAgeDefault
	}
	age, err := time.ParseDuration(raw)
	if err != nil || age <= 0 {
		infraSessionPurgeAgeWarnOnce.Do(func() {
			log.Printf("wisp gc: GC_INFRA_SESSION_PURGE_AGE=%q is not a positive Go duration (e.g. 72h; days like 30d are not accepted); using the default %s",
				raw, infraSessionPurgeAgeDefault)
		})
		return infraSessionPurgeAgeDefault
	}
	return age
}

// infraSessionPurgeAgeWarnOnce limits the invalid-override warning to one line
// per process: the age is re-read every GC tick. Tests reset it.
var infraSessionPurgeAgeWarnOnce = &sync.Once{}

// wispGCSessionPurgeBatchCap bounds how many closed infra session beads one
// sweep deletes. The backlog drains across ticks instead of one unbounded pass.
var wispGCSessionPurgeBatchCap = 500

// wispGCSessionPurgeScanCap bounds how many candidates one sweep examines,
// kept or deleted. A session that still owns children is kept, and it counts
// against this budget, so the per-tick Children/DepList/Get probes stay bounded
// even when the backlog is mostly sessions that cannot be purged. The sweep
// resumes after the last examined candidate on the next tick (see
// memoryWispGC.sessionPurgeCursor), so kept sessions do not starve the rest.
var wispGCSessionPurgeScanCap = 2000

// reapOrphansEnforced reports whether orphaned-closed-wisp reaping should
// actually delete rows (true) or run dry (false). It is a package var so tests
// can flip it without touching the process environment. By default it reads
// reapOrphansEnv, which is unset in production until an operator opts in.
var reapOrphansEnforced = func() bool {
	return parseBoolEnv(os.Getenv(reapOrphansEnv))
}

func parseBoolEnv(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	switch strings.ToLower(v) {
	case "yes", "on":
		return true
	default:
		return false
	}
}

// wispGC performs mechanical garbage collection of closed molecules that
// have exceeded their TTL. Follows the nil-guard tracker pattern used by
// crashTracker and idleTracker: nil means disabled.
type wispGC interface {
	// shouldRun returns true if enough time has elapsed since the last run.
	shouldRun(now time.Time) bool

	// runGC lists closed molecules, deletes those older than TTL, and returns
	// the count of purged entries. Errors from individual deletes are
	// best-effort and surfaced without stopping the purge; the returned error
	// also covers list failures. The molecule/wisp/workflow purge arm operates on
	// the graph-class store; the read-message retention arm on the messaging-class
	// store. Both wrap the same underlying work store until either class relocates.
	//
	// sessionLedger is the sessions-class store ONLY when the city has relocated
	// that class off its work store onto a SQLite infra ledger (see
	// relocatedSQLiteSessionLedger); otherwise its Store is nil and the closed
	// session purge arm does nothing. Sessions on a work store belong to the
	// reaper order's step 6, with its own pattern, backup and anomaly guards.
	//
	// legs is the work side of a split city: the stores that hold attachment
	// holders and work-class molecule roots (see wispGCLegs). nil means the
	// graph-class store is the city's only store, and every read stays on it.
	runGC(legs wispGCLegs, graphStore beads.GraphStore, sessionLedger beads.SessionStore, mailStore beads.MailStore, now time.Time) (int, error)
}

// wispGCLegs is the leg set a wisp GC pass reads beyond the graph-class store
// on a city that relocates the graph class: the city work store, the serving
// rigs and the relocated bindings, in plan order (WorkLegs satisfies it).
//
// Two of the GC's reads belong there. The abandoned-root sweep's live-holder
// guard looks for the beads still pointing at a root through molecule_id /
// workflow_id, and those holders are WORK beads (R7). And a v1 POURED molecule
// is work class, so its root is on the work ledger rather than in the binding:
// the GC must find it there to close it when abandoned and collect it once
// expired.
type wispGCLegs interface {
	each(visit func(beads.Store))
	// eachLeg is each with every leg's store ref, its stable identity.
	eachLeg(visit func(ref string, store beads.Store))
	// unusable is why the leg set answers nothing (a refused city). A holder
	// guard that cannot read its legs fails closed.
	unusable() error
	// suspendedRigs names the declared rigs left out of the legs because they
	// are suspended. They may hold live attachment sources the guard cannot
	// read.
	suspendedRigs() []string
	// storeForRef resolves a gc.source_store_ref to the leg it names:
	// ok=false when it names no leg this set knows, suspended=true when it
	// names a suspended rig (which is not read).
	storeForRef(ref string) (store beads.Store, suspended, ok bool)
}

// memoryWispGC is the production implementation of wispGC.
type memoryWispGC struct {
	interval         time.Duration
	ttl              time.Duration
	mailRetentionTTL time.Duration
	lastRun          time.Time
	// sessionPurgeCursor is where the closed infra session sweep resumes: the
	// last candidate the previous sweep examined, in (created_at, id) order.
	// nil starts from the oldest candidate; a sweep that reaches the end of the
	// candidate list resets it so the next pass starts over.
	sessionPurgeCursor *beads.SeekBoundary
	// notes are the refusals and per-root log lines this tracker remembers
	// across passes (wispGCNotes).
	notes wispGCNotes
	// cityName is the name this city's "city:<name>" store refs carry. On a
	// single-store city (no legs) only that ref resolves a stepless root's
	// source pointer; "" (a tracker built without one) accepts any city ref.
	cityName string
}

// newWispGC creates a wisp GC tracker. Returns nil if disabled. The tracker
// runs when an interval is configured and at least one retention policy is
// enabled.
func newWispGC(interval, ttl, mailRetentionTTL time.Duration) wispGC {
	if interval <= 0 || (ttl <= 0 && mailRetentionTTL <= 0) {
		return nil
	}
	return &memoryWispGC{
		interval:         interval,
		ttl:              ttl,
		mailRetentionTTL: mailRetentionTTL,
	}
}

// newWispGCForCity is newWispGCForConfig for the named city.
func newWispGCForCity(cfg *config.City, cityName string) wispGC {
	wg := newWispGCForConfig(cfg)
	if m, ok := wg.(*memoryWispGC); ok {
		m.cityName = strings.TrimSpace(cityName)
	}
	return wg
}

func newWispGCForConfig(cfg *config.City) wispGC {
	if cfg == nil {
		return nil
	}
	mailRetentionTTL, err := cfg.Mail.RetentionTTLDuration()
	if err != nil {
		mailRetentionTTL = 0
	}
	return newWispGC(cfg.Daemon.WispGCIntervalDuration(), cfg.Daemon.WispTTLDuration(), mailRetentionTTL)
}

func (m *memoryWispGC) shouldRun(now time.Time) bool {
	return now.Sub(m.lastRun) >= m.interval
}

func (m *memoryWispGC) runGC(legs wispGCLegs, graphStore beads.GraphStore, sessionLedger beads.SessionStore, mailStore beads.MailStore, now time.Time) (int, error) {
	m.lastRun = now
	// The molecule/wisp/workflow purge arm operates on the graph-class store; the
	// read-message retention arm on the messaging-class store. Pass the unwrapped
	// .Store to the generic beads.Store-typed purge helpers.
	store := graphStore.Store
	if store == nil {
		return 0, fmt.Errorf("listing closed molecules: bead store unavailable")
	}

	purged := 0
	var deleteErr error
	if m.ttl > 0 {
		closedSpecs, specErr := sourceworkflow.CloseSpecSidecarsForClosedRoots(store, sourceworkflow.WorkflowSpecSidecarClosedReason)
		if specErr != nil {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("closing generated spec sidecars for closed workflow roots: %w", specErr))
		} else if closedSpecs > 0 {
			log.Printf("wisp gc: closed %d generated spec sidecars for closed workflow roots", closedSpecs)
		}

		closedMembers, memberErr := closeGeneratedMembersForClosedRoots(store)
		if memberErr != nil {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("closing generated members for terminal workflow roots: %w", memberErr))
		} else if closedMembers > 0 {
			log.Printf("wisp gc: closed %d generated members for terminal workflow roots", closedMembers)
		}

		// Close abandoned OPEN roots BEFORE the closed-root purge below so a
		// root the sweep closes this tick can be collected by the purge in the
		// same tick when it has already aged past m.ttl (the purge gates on
		// CreatedAt, not close time), or on a later tick otherwise. Best-effort:
		// never fails the GC tick.
		pass := &wispGCPassLog{}
		holders := wispGCHolders{graph: store, legs: legs, pass: pass, cityName: m.cityName, memo: &m.notes, now: now}
		if abandonedErr := closeAbandonedRoots(store, holders, now, &m.notes); abandonedErr != nil {
			deleteErr = errors.Join(deleteErr, abandonedErr)
		}
		pass.logSuspension()

		entries, err := closedWispGCEntries(store)
		if err != nil {
			return 0, err
		}

		cutoff := now.Add(-m.ttl)
		closurePurged, closureDeleteErr := purgeExpiredBeadClosuresWith(store, entries, closurePurgeOpts{
			cutoff:   cutoff,
			batchCap: wispGCClosurePurgeBatchCap,
			arm:      "graph",
			notes:    &m.notes,
			now:      now,
		})
		purged += closurePurged
		deleteErr = errors.Join(deleteErr, closureDeleteErr)

		// Reap closed wisp-tier descendants the root-rooted closure purge above
		// never enumerates (their owning root is gone or never appears in the
		// closed-root list). This touches a disjoint set from closeAbandonedRoots
		// — that path closes OPEN roots, this path reaps CLOSED descendants of
		// already-collectible roots. Best-effort: its error is joined so a failure
		// never aborts the tick.
		orphanReaped, orphanErr := reapOrphanedClosedWisps(store, cutoff, wispGCReapOrphanBatchCap)
		purged += orphanReaped
		deleteErr = errors.Join(deleteErr, orphanErr)

		// Forget refusals of closed roots no longer listed (purged, or deleted
		// elsewhere). The work-class roots on the work legs are the work arm's,
		// on its own lane (wisp_gc_work_lane.go), not this pass's.
		m.notes.pruneRefusals("graph", rootIDSet(entries))
		m.notes.endPass()
	}

	// Closed agent-session rows (gcg-session-*, gcs-*) in a SQLite infra ledger
	// are invisible to the reaper order, whose step 6 queries the Dolt work
	// database, and the root-blind SQLite retention sweeper stays off, so this
	// arm is what deletes them. It runs only when both hold:
	//   - the graph-class GC is on (wisp_ttl > 0). A GC enabled only for mail
	//     retention is a messaging policy and does not opt the city into
	//     deleting session history;
	//   - the caller handed over a relocated SQLite sessions ledger. On an
	//     unsplit city sessions live on the work store, sessionLedger.Store is
	//     nil, and this arm is a no-op: those rows belong to reaper step 6.
	// Its clock is infraSessionPurgeAge (GC_INFRA_SESSION_PURGE_AGE, default
	// 3 days) — neither the 24h wisp TTL nor the Dolt reaper's 30-day
	// GC_REAPER_SESSION_PURGE_AGE — and it never deletes a non-session bead.
	if m.ttl > 0 && sessionLedger.Store != nil {
		sessionPurged, next, sessionErr := purgeClosedInfraSessionsPage(sessionLedger.Store, now, infraSessionPurgeAge(),
			wispGCSessionPurgeBatchCap, wispGCSessionPurgeScanCap, m.sessionPurgeCursor)
		m.sessionPurgeCursor = next
		purged += sessionPurged
		deleteErr = errors.Join(deleteErr, sessionErr)
	}

	if m.mailRetentionTTL > 0 && mailStore.Store != nil {
		// The read-message retention arm is messaging-class: its candidate query
		// and wisp-tier delete loop live inside the messaging edge (beadmail),
		// against the messaging store — disjoint from the graph-class purge above.
		mailPurged, mailErr := beadmail.PurgeReadMessageWisps(mailStore, now.Add(-m.mailRetentionTTL))
		purged += mailPurged
		if mailErr != nil {
			deleteErr = errors.Join(deleteErr, mailErr)
		}
		if mailPurged > 0 {
			log.Printf("wisp gc: purged %d read message wisps (retention_ttl=%s)", mailPurged, gcRetentionTTLString(m.mailRetentionTTL))
		}
	}

	return purged, deleteErr
}

// enumerateWorkClassMoleculeRoots lists the v1 poured molecule roots on a work
// leg in the given statuses: type=molecule roots that classify as work. It
// reads with the molecule selector alone, so a work ledger pays one query per
// status rather than the graph store's four.
//
// bd proto templates are never candidates. They are molecule rows too (the
// formula protos `bd mol` instantiates from), so the read stays on the issues
// tier — a TierBoth read makes bd add --include-templates — and any row
// carrying the template label is dropped as well.
func enumerateWorkClassMoleculeRoots(store beads.Store, statuses ...string) ([]beads.Bead, error) {
	sel := wispGCMoleculeRootSelector()
	sel.query.TierMode = beads.TierIssues
	// The work leg of the wisp GC root union: graph molecule roots (wisps)
	// live in the graph store (closedWispGCEntries, openWispGCRootCandidates).
	var roots []beads.Bead
	err := beads.FederatedRead(func() (err error) {
		roots, err = enumerateWispGCRootsWith(store, []wispGCRootSelector{sel}, statuses...)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := roots[:0]
	for _, root := range roots {
		if !beads.IsMoleculeType(root.Type) || strings.TrimSpace(root.ParentID) != "" || coordclass.Classify(root) != coordclass.ClassWork {
			continue
		}
		if slices.Contains(root.Labels, bdTemplateLabel) {
			continue
		}
		out = append(out, root)
	}
	return out, nil
}

// bdTemplateLabel marks a bd proto template row.
const bdTemplateLabel = "template"

// wispGCHolders answers the abandoned-root sweep's live-holder question: does
// any bead still point at a root through molecule_id or workflow_id. Holders
// are work beads, so on a split city they are read from every leg
// (wispGCLegs) as well as the graph-class store.
type wispGCHolders struct {
	graph beads.Store
	legs  wispGCLegs
	// own, when set, is the root's own work leg: the guard reads only it and
	// the graph store, and no rig's suspension is consulted (the work arm).
	own beads.Store
	// pass collects what the guard decided this pass, for one summary line.
	pass *wispGCPassLog
	// cityName, on a single-store city (no legs), is the only city a source
	// ref may name ("": any).
	cityName string
	// memo remembers, per root, the answer the guard's reads gave, until the
	// root changes or wispGCRefusalRecheck passes (nil: every pass reads).
	// The graph arm runs on the controller tick, and on a ledger served by bd
	// the holder query is a few bd calls per leg, which a root the guard holds
	// open, or a dry run's would-close root, would otherwise pay every pass.
	memo *wispGCNotes
	// now is the pass's clock, for memo.
	now time.Time
}

// The graph arm's memo of holder answers (wispGCHolders.memo).
const (
	graphArmHolderMemo = "graph-holders"
	holderAnswerHeld   = "held"
	holderAnswerFree   = "not held"
)

// wispGCPassLog collects, for one pass, the stepless roots the holder guard
// left open because a suspended rig might hold their source, so the pass logs
// one line rather than one per root.
type wispGCPassLog struct {
	suspendedRigs []string
	heldOpen      int
}

func (p *wispGCPassLog) noteSuspended(rigs []string) {
	if p == nil {
		return
	}
	p.suspendedRigs = rigs
	p.heldOpen++
}

func (p *wispGCPassLog) logSuspension() {
	if p == nil || p.heldOpen == 0 {
		return
	}
	log.Printf("wisp gc: left %d stepless root(s) open: a live source in suspended rig(s) %s cannot be ruled out until they resume", p.heldOpen, strings.Join(p.suspendedRigs, ","))
}

// closeGeneratedMembersForClosedRoots repairs terminal workflow roots whose
// finalizer, supersession, or partial materialization left generated members
// open. Closed roots are the authority boundary: live roots are never listed
// and therefore never touched. Each subtree close is ordered and idempotent.
//
// Only graph-class roots (workflows, wisps) are repaired: their generated
// members are settled with them, so an open member under a settled one is
// residue. A v1 POURED molecule root is work class and has no finalizer; an
// open step under a closed one is a step someone reopened, which is live
// work, not residue, and is left alone (the closure purge refuses it too).
func closeGeneratedMembersForClosedRoots(store beads.Store) (int, error) {
	roots, err := closedWispGCEntries(store)
	if err != nil {
		return 0, err
	}
	closed := 0
	var closeErr error
	for _, root := range roots {
		if coordclass.Classify(root) == coordclass.ClassWork {
			continue
		}
		n, err := molecule.CloseSubtreeWithMetadata(store, root.ID, map[string]string{
			beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped,
			"close_reason":              sourceworkflow.WorkflowSkippedCloseReason,
		})
		closed += n
		if err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("closing terminal workflow subtree %s: %w", root.ID, err))
		}
	}
	return closed, closeErr
}

// wispGCRootSelector pairs a List selector with a short label used for error
// context. The selectors returned by wispGCRootSelectors, unioned, cover every
// root class the wisp GC can close or collect.
type wispGCRootSelector struct {
	label string
	query beads.ListQuery
}

// wispGCRootSelectors returns the conjunctive List selectors whose union is the
// full root universe the wisp GC reaps:
//   - v1 poured molecule roots (type=molecule)
//   - wisp-kinded roots (gc.kind=wisp)
//   - graph.v2 workflow roots, which compile as type=task carrying
//     gc.formula_contract=graph.v2 AND gc.kind=workflow (see
//     internal/formula/compile.go) — NOT type=molecule, so the molecule
//     selector alone never matches them.
//
// closedWispGCEntries and openWispGCRootCandidates MUST enumerate this same
// universe, differing only in the statuses they pass, so that every root the
// abandoned-root closer can close (openWispGCRootCandidates) is later
// collectible by the closed-root purge (closedWispGCEntries).
func wispGCRootSelectors() []wispGCRootSelector {
	return []wispGCRootSelector{
		wispGCMoleculeRootSelector(),
		{"wisp", beads.ListQuery{Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}, TierMode: beads.TierBoth}},
		{"graph.v2", beads.ListQuery{Metadata: map[string]string{beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2}, TierMode: beads.TierBoth}},
		{"workflow", beads.ListQuery{Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}, TierMode: beads.TierBoth}},
	}
}

// wispGCMoleculeRootSelector is the type=molecule root selector: v1 poured
// molecule roots, the one root shape a work ledger holds on a split city.
func wispGCMoleculeRootSelector() wispGCRootSelector {
	return wispGCRootSelector{"molecule", beads.ListQuery{Type: "molecule", TierMode: beads.TierBoth}}
}

// enumerateWispGCRoots unions wispGCRootSelectors across the given statuses,
// deduping by bead ID so a root matching more than one selector (for example a
// graph.v2 root that also carries gc.kind=workflow) appears once.
func enumerateWispGCRoots(store beads.Store, statuses ...string) ([]beads.Bead, error) {
	return enumerateWispGCRootsWith(store, wispGCRootSelectors(), statuses...)
}

// enumerateWispGCRootsWith is enumerateWispGCRoots over the given selectors.
func enumerateWispGCRootsWith(store beads.Store, selectors []wispGCRootSelector, statuses ...string) ([]beads.Bead, error) {
	entries := make([]beads.Bead, 0)
	seen := make(map[string]struct{})
	for _, status := range statuses {
		for _, sel := range selectors {
			q := sel.query
			q.Status = status
			items, err := store.List(q)
			if err != nil {
				return nil, fmt.Errorf("listing %s %s roots: %w", status, sel.label, err)
			}
			for _, item := range items {
				if item.ID == "" {
					continue
				}
				if _, ok := seen[item.ID]; ok {
					continue
				}
				seen[item.ID] = struct{}{}
				entries = append(entries, item)
			}
		}
	}
	return entries, nil
}

// closedWispGCEntries lists every CLOSED root across the full wisp GC root
// universe (see wispGCRootSelectors). The closed-root purge deletes each whose
// ownership closure has aged past the TTL. Enumerating graph.v2/workflow roots
// here — not just molecule/wisp — is what lets a graph workflow root the
// abandoned-root sweep closes (type=task) actually be collected rather than
// linger as permanent closed residue.
func closedWispGCEntries(store beads.Store) (roots []beads.Bead, err error) {
	// The graph leg of the wisp GC root union: a v1 poured molecule root
	// (work class) lives on a work leg (enumerateWorkClassMoleculeRoots).
	err = beads.FederatedRead(func() (err error) {
		roots, err = enumerateWispGCRoots(store, "closed")
		return err
	})
	return roots, err
}

// reapOrphanedClosedWisps reaps closed wisp-tier descendants whose owning root
// is gone or already terminal but which the root-rooted closure purge never
// enumerates (their root is absent from, or never appears in, the closed-root
// list), plus rootless plain-task wisps that never had an owning root at all
// (gastownhall/gascity#3780). Candidates are closed wisp-tier rows older than
// cutoff, either carrying a gc.root_bead_id pointer or a bare type=task row.
//
// Safety: a descendant with a root pointer is reaped only when its root is
// provably collectible — the root Get returns ErrNotFound (root gone) or the
// root is terminal (closed/tombstone). A live/open root, or any other
// (unreadable) Get error, causes the descendant to be SKIPPED so an in-flight
// workflow is never stripped of its closed steps. The per-root Get decision
// is cached so many siblings sharing one dead root cost a single Get. A
// rootless row needs no such check when it is a plain-task LEAF — no parent and
// no children by either the parent_id COLUMN or a parent-child DEP ROW, since
// ownership travels over both (collectExpiredBeadClosureStatus walks both too) —
// because its own closed status is then the entire collectibility fence;
// deleteWorkflowBead removes a single bead, not a closure, so a rootless row
// that owns a subtree stays SKIPPED rather than stranding descendants no GC
// path could reach again, and one that is a subtree MEMBER stays SKIPPED rather
// than being stripped from a live parent. A rootless row that is not a plain
// task is an unrecognized shape and also stays SKIPPED. The leaf-ness probes
// cost backend reads even in dry-run, so wispGCReapOrphanProbeCap bounds how
// many of them one sweep performs regardless of enforcement.
//
// With reapOrphansEnforced() false (the dry-run default, GC_WISP_GC_REAP_ORPHANS
// unset) the function mutates nothing: it counts the would-be reaps and logs a
// dry-run notice. Per-bead delete errors are joined and never abort the sweep.
// The batch cap bounds reaps per sweep so the backlog drains over multiple ticks.
func reapOrphanedClosedWisps(store beads.Store, cutoff time.Time, batchCap int) (int, error) {
	if store == nil {
		return 0, fmt.Errorf("reaping orphaned closed wisps: bead store unavailable")
	}

	candidates, err := store.List(beads.ListQuery{
		Status:   "closed",
		TierMode: beads.TierWisps,
	})
	if err != nil {
		return 0, fmt.Errorf("listing closed wisp-tier rows: %w", err)
	}

	enforce := reapOrphansEnforced()

	// rootCollectible caches the per-root reap decision so many siblings
	// sharing one dead root cost a single Get.
	rootCollectible := make(map[string]bool)
	var collectErr error

	reaped := 0
	attempted := 0
	// probed bounds the rootless leaf-ness probes (Children + DepList) for the
	// sweep. It advances in DRY-RUN TOO, unlike attempted: the dry run performs
	// no deletes but still pays those backend reads per aged rootless candidate.
	probed := 0
	probeTruncated := false
	var deleteErr error
	for _, c := range candidates {
		// The batch cap bounds DELETION ATTEMPTS per sweep — counting failed
		// deletes, not just successful reaps — so a failing delete backend can't
		// attempt the whole backlog in one tick. In dry-run attempted stays 0 and
		// reaped keeps counting past the cap so the logged estimate reflects the
		// true eligible backlog an operator needs before enabling enforcement, not
		// just the cap-sized prefix the enforced sweep would reap this tick.
		if enforce && batchCap > 0 && attempted >= batchCap {
			break
		}

		rootID := c.Metadata[beadmeta.RootBeadIDMetadataKey]
		if rootID == "" && c.Type != "task" {
			// No root pointer and not a plain task: an unrecognized shape
			// this reaper cannot prove safe to collect — out of scope.
			continue
		}

		// Reuse the closure purge's age semantics: skip zero/recent rows.
		if c.CreatedAt.IsZero() || !c.CreatedAt.Before(cutoff) {
			continue
		}

		var decision bool
		if rootID == "" {
			// A rootless plain-task wisp is collectible only when it is a
			// LEAF. deleteWorkflowBead removes a single bead, not a closure
			// (unlike the root-rooted purge, which deletes the whole
			// ownership tree), so reaping a row that owns a parent-child
			// subtree would strand descendants that neither GC path can
			// reach again, and reaping a subtree member would strip a step
			// from a live parent. Its own closed status is the entire
			// collectibility fence only once both directions of ownership —
			// column and dep row — come back empty (gastownhall/gascity#3780).
			if strings.TrimSpace(c.ParentID) != "" {
				continue
			}
			// Every remaining rootless candidate costs backend reads whether or
			// not this sweep enforces, so the probe budget is checked before the
			// first of them. break, not continue: the rest of the candidate
			// slice cannot be probed either, so walking it buys nothing.
			if wispGCReapOrphanProbeCap > 0 && probed >= wispGCReapOrphanProbeCap {
				probeTruncated = true
				break
			}
			probed++
			children, childErr := store.Children(c.ID, beads.IncludeClosed, beads.WithBothTiers)
			if childErr != nil {
				// Cannot prove safe — skip, matching the unreadable-Get
				// posture on the rooted branch.
				collectErr = errors.Join(collectErr, fmt.Errorf("listing children for rootless orphan %q: %w", c.ID, childErr))
				continue
			}
			if len(children) > 0 {
				continue
			}
			// Mirror collectExpiredBeadClosureStatus: ownership is carried by the
			// parent_id COLUMN or by a parent-child DEP ROW, and some step
			// beads have only the dep row. A column-only fence would reap such
			// a row out from under a live parent ("down": rows where this bead
			// depends on another), or out from under its own dep-linked
			// children ("up": rows where another bead depends on this one).
			// Both reads sit inside the single probe budget charged above.
			// Live, as the purge's is: a closed row can sit in the controller's
			// cache while another process adds an edge to it without an event.
			linked, linkErr := hasParentChildDepEdge(beads.HandlesFor(store).Live, c.ID)
			if linkErr != nil {
				collectErr = errors.Join(collectErr, fmt.Errorf("listing parent-child deps for rootless orphan %q: %w", c.ID, linkErr))
				continue
			}
			if linked {
				continue
			}
			decision = true
		} else {
			cached, ok := rootCollectible[rootID]
			if !ok {
				// Live: the root's status gates a delete, and a cached row can
				// still say terminal after another process reopened it.
				root, getErr := beads.HandlesFor(store).Live.Get(rootID)
				switch {
				case errors.Is(getErr, beads.ErrNotFound):
					cached = true // root gone
				case getErr == nil && convoycore.IsTerminalStatus(root.Status):
					cached = true // root terminal
				case getErr == nil:
					cached = false // root live/open — never reap its descendants
				default:
					// Any other Get error: cannot prove safe — skip without
					// caching as collectible. Surface so the sweep records
					// the failure.
					cached = false
					collectErr = errors.Join(collectErr, fmt.Errorf("resolving root %q for orphan %q: %w", rootID, c.ID, getErr))
				}
				rootCollectible[rootID] = cached
			}
			decision = cached
		}
		if !decision {
			continue
		}

		if !enforce {
			reaped++
			continue
		}

		err := deleteWorkflowBead(store, c.ID)
		if errors.Is(err, errWorkflowDeleteLiveDescendants) {
			// A closed orphan can itself be an intermediate step that still
			// owns open sub-steps; the delete refuses those. That is a skip,
			// not a sweep failure — it becomes eligible once they finish —
			// and it is not charged to the batch cap: no delete was
			// attempted, and an orphan refused on every sweep would otherwise
			// hold a cap slot on every sweep, starving the reapable orphans
			// listed behind it.
			continue
		}
		attempted++
		if err != nil {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("reaping orphaned closed wisp %q: %w", c.ID, err))
			continue
		}
		reaped++
	}

	if reaped > 0 {
		switch {
		case enforce:
			log.Printf("wisp gc: reaped %d orphaned closed wisp(s)", reaped)
		case batchCap > 0:
			log.Printf("wisp gc: %d orphaned closed wisp(s) would be reaped (dry-run; set %s=1 to enforce; enforced sweeps reap up to %d per tick)", reaped, reapOrphansEnv, batchCap)
		default:
			log.Printf("wisp gc: %d orphaned closed wisp(s) would be reaped (dry-run; set %s=1 to enforce)", reaped, reapOrphansEnv)
		}
	}

	if probeTruncated {
		log.Printf("wisp gc: rootless-orphan scan stopped after %d probes (cap); the reported count is a floor, not the full eligible backlog", probed)
	}

	if !enforce {
		// Dry-run never mutates: report would-be count via log only, return 0
		// purged so callers don't over-count deletions that did not happen.
		return 0, errors.Join(collectErr, deleteErr)
	}

	return reaped, errors.Join(collectErr, deleteErr)
}

// relocatedSQLiteSessionLedger returns sessionStore — the sessions-class store
// as resolveSessionStore / sessionsBeadStore() resolved it — when, and only
// when, the storage routes relocate the sessions class off the work store onto
// a SQLite Beads ledger (the whole-split infra binding). Anything else — no
// routes, sessions left on the work binding, a relocated binding served by
// another engine (a Dolt workspace), or a refused binding — returns nil, which
// is what keeps the closed session purge off every store the reaper order
// already governs.
//
// The engine is identified under the controller's cache (bindingEngine), and
// the store handed back is sessionStore itself, so on the controller the purge
// deletes through that cache: the cache drops the rows and the deletes emit
// bead.deleted like every other controller write to the binding.
func relocatedSQLiteSessionLedger(routes *storageRoutes, sessionStore, workStore beads.Store) beads.Store {
	routed, relocated := routes.storeFor(coordclass.ClassSessions) // residency:allow — asks whether the sessions class is relocated, to gate a purge; resolves no bead
	if !relocated || routed == nil || sessionStore == nil {
		return nil
	}
	ledger, ok := bindingEngine(sessionStore).(*beads.SQLiteStore)
	if !ok || ledger == nil {
		return nil
	}
	if routedLedger, ok := bindingEngine(routed).(*beads.SQLiteStore); !ok || routedLedger != ledger {
		return nil
	}
	if work, ok := bindingEngine(workStore).(*beads.SQLiteStore); ok && work == ledger {
		return nil
	}
	return sessionStore
}

// purgeClosedInfraSessions deletes closed session beads that have been idle
// longer than age, examining candidates from the oldest. It is the one-pass
// form of purgeClosedInfraSessionsPage with the default scan budget.
func purgeClosedInfraSessions(store beads.Store, now time.Time, age time.Duration, batchCap int) (int, error) {
	purged, _, err := purgeClosedInfraSessionsPage(store, now, age, batchCap, wispGCSessionPurgeScanCap, nil)
	return purged, err
}

// purgeClosedInfraSessionsPage deletes closed session beads that have been
// idle longer than age. The store must be the relocated SQLite sessions ledger
// (relocatedSQLiteSessionLedger), where agent sessions are minted as
// gcg-session-* or the older gcs-* prefix. The Dolt reaper only sees
// issue_type=session in a Dolt `issues` table, so this arm is what keeps the
// sqlite ledger from retaining every closed session forever.
//
// It deletes only type=session rows that are closed and old. A closed workflow
// step, an open session, and a session that still owns a parent-child child
// stay. Each candidate is re-read immediately before its delete, and skipped
// unless it is still a closed session past the cutoff, so a session reopened
// after the list is not deleted. Delete is the store's own delete, so labels,
// metadata, and deps go with the bead.
//
// The sweep is bounded twice: at most batchCap delete attempts and at most
// scanCap examined candidates, kept ones included. It starts after cursor in
// (created_at, id) order and returns where the next sweep should resume: the
// last examined candidate, or nil once the candidate list is exhausted.
func purgeClosedInfraSessionsPage(store beads.Store, now time.Time, age time.Duration, batchCap, scanCap int, cursor *beads.SeekBoundary) (int, *beads.SeekBoundary, error) {
	if store == nil || age <= 0 {
		return 0, nil, nil
	}
	cutoff := now.Add(-age)
	query := beads.ListQuery{
		Status:        "closed",
		Type:          "session",
		UpdatedBefore: cutoff,
		TierMode:      beads.TierBoth,
		Sort:          beads.SortCreatedAsc,
		SeekAfter:     cursor,
	}
	if scanCap > 0 {
		query.Limit = scanCap
	}
	candidates, err := store.List(query)
	if err != nil {
		return 0, cursor, fmt.Errorf("listing closed infra sessions: %w", err)
	}
	// The safety reads go past any cache: the controller hands this a cached
	// ledger, and a reopen or a new edge another process wrote without an event
	// must still stop the delete.
	live := beads.HandlesFor(store).Live
	purged := 0
	attempted := 0
	examined := 0
	var last *beads.SeekBoundary
	var deleteErr error
	for _, candidate := range candidates {
		if batchCap > 0 && attempted >= batchCap {
			break
		}
		examined++
		last = &beads.SeekBoundary{CreatedAt: candidate.CreatedAt, ID: candidate.ID}
		if !closedInfraSessionPastCutoff(candidate, cutoff) {
			continue
		}
		children, childErr := store.Children(candidate.ID, beads.IncludeClosed, beads.WithBothTiers)
		if childErr != nil {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("listing children for session %q: %w", candidate.ID, childErr))
			continue
		}
		if len(children) > 0 {
			continue
		}
		linked, linkErr := hasParentChildDepEdge(live, candidate.ID)
		if linkErr != nil {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("listing parent-child deps for session %q: %w", candidate.ID, linkErr))
			continue
		}
		if linked {
			continue
		}
		// Re-read right before the delete: the list is a snapshot, and a
		// configured named session can be reopened in the meantime.
		current, getErr := live.Get(candidate.ID)
		if getErr != nil {
			if !errors.Is(getErr, beads.ErrNotFound) {
				deleteErr = errors.Join(deleteErr, fmt.Errorf("re-reading session %q: %w", candidate.ID, getErr))
			}
			continue
		}
		if !closedInfraSessionPastCutoff(current, cutoff) {
			continue
		}
		attempted++
		if err := deleteClosedInfraSessionIfMatch(store, current); err != nil {
			if errors.Is(err, beads.ErrNotFound) || beads.IsPreconditionFailed(err) {
				continue
			}
			deleteErr = errors.Join(deleteErr, fmt.Errorf("purging closed infra session %q: %w", candidate.ID, err))
			continue
		}
		purged++
	}
	next := last
	if examined == len(candidates) && (scanCap <= 0 || len(candidates) < scanCap) {
		// Every candidate after the cursor was examined: the next sweep
		// starts over from the oldest.
		next = nil
	}
	if purged > 0 {
		log.Printf("wisp gc: purged %d closed infra session bead(s) older than %s", purged, age)
	}
	return purged, next, deleteErr
}

// deleteClosedInfraSessionIfMatch deletes current, the session the purge's
// live re-read proved purgeable, only while the row still has that read's
// revision. A write that lands after the re-read (a configured named session
// reopened) fails the fence, and the row is kept for the next sweep to judge.
// A store that cannot fence (BdStore without --if-revision, a legacy SQLite
// layout) keeps the plain delete after the re-read, which leaves the window
// between that read and the delete.
func deleteClosedInfraSessionIfMatch(store beads.Store, current beads.Bead) error {
	if writer, ok := beads.ConditionalWriterForTarget(store); ok {
		err := writer.DeleteIfMatch(current.ID, current.Revision)
		if !beads.IsConditionalWriteUnsupported(err) {
			return err
		}
	}
	return store.Delete(current.ID)
}

// closedInfraSessionPastCutoff reports whether b is a closed session bead whose
// last activity (UpdatedAt, falling back to CreatedAt) is before cutoff. A bead
// with no timestamp at all is never past the cutoff.
func closedInfraSessionPastCutoff(b beads.Bead, cutoff time.Time) bool {
	if b.Type != "session" || b.Status != "closed" {
		return false
	}
	activity := b.UpdatedAt
	if activity.IsZero() {
		activity = b.CreatedAt
	}
	return !activity.IsZero() && activity.Before(cutoff)
}

// hasParentChildDepEdge reports whether id sits on either end of a parent-child
// dependency row. The parent_id column is not the only ownership link: some
// molecule step beads are joined to their parent purely by a dep row (see
// collectExpiredBeadClosureStatus, which walks both). "down" deps answer "does this
// bead have a dep-linked parent", "up" deps answer "does it have dep-linked
// children" — either makes it a subtree member rather than a free-standing
// leaf, so the orphan reaper must leave it to the owning root's closure purge.
func hasParentChildDepEdge(store interface {
	DepList(id, direction string) ([]beads.Dep, error)
}, id string,
) (bool, error) {
	for _, direction := range []string{"down", "up"} {
		deps, err := store.DepList(id, direction)
		if err != nil {
			return false, fmt.Errorf("listing %q deps: %w", direction, err)
		}
		for _, dep := range deps {
			if dep.Type != "parent-child" {
				continue
			}
			if dep.IssueID != "" && dep.DependsOnID != "" {
				return true, nil
			}
		}
	}
	return false, nil
}

// closeAbandonedRoots closes OPEN workflow and v1 molecule roots whose entire
// descendant subtree is terminal and whose own last activity is older than the
// conservative TTL. This is the periodic counterpart to the edge-triggered
// reactive autoclose (molecule_autoclose.go): the reactive path only re-checks
// a root when a child-close event names it, and the closed-root purge only
// DELETES already-closed roots — so an open root whose descendants all went
// terminal without a final child-close event (or whose final event was lost)
// stays open forever and fuels the wisp backlog. This sweep is the only path
// that CLOSES such abandoned roots. Its candidate scope (isAbandonedRootCandidate)
// mirrors the reactive path so v1 type=molecule roots are not silently excluded.
//
// Guards (each is load-bearing — see BUG 4):
//  1. TTL: skip roots with activity newer than now-wispGCCloseAbandonedTTL so
//     live/in-flight roots and the external operational reconciler are never
//     raced.
//  2. Stepless: a root with no descendants has no completion signal of its own,
//     so it closes only when steplessRootIsAbandoned says it was never claimed
//     AND wispGCHolders.liveAttachmentSource finds no live source bead still
//     attached to it. A claimed stepless root is held by a live worker; an
//     unclaimed one that outlived the TTL is a leaked pour. (The reactive
//     autocloseMoleculeIfComplete skips stepless roots outright — it has no TTL
//     to bound the instantiator race with, and this sweep does.)
//  3. Exempt: skip roots carrying the gc.gc_exempt marker. This is a generic,
//     operator-supplied opt-out — the SDK never stamps it (stamping a specific
//     named root would hardcode a deployment role). A deployment marks any
//     long-lived root it never wants auto-closed; the guard is a no-op until it
//     does, which is safe under the dry-run default.
//  4. Best-effort: per-root errors are joined and logged; this never fails the
//     GC tick.
//  5. Dry-run default: unless closeAbandonedEnforced() returns true the sweep
//     only logs/counts the candidates it WOULD close and mutates nothing.
//
// notes are the tracker's memo: a per-root dry-run line is printed only when
// it is new since the previous pass (nil: always).
func closeAbandonedRoots(store beads.Store, holders wispGCHolders, now time.Time, notes *wispGCNotes) error {
	if store == nil {
		return nil
	}
	candidates, err := openWispGCRootCandidates(store)
	if err != nil {
		// Best-effort: a list failure must not fail the GC tick.
		return fmt.Errorf("listing open workflow roots for abandoned-root sweep: %w", err)
	}
	// Forget the holder answers of roots no longer open (closed, or deleted
	// elsewhere).
	holders.memo.pruneRefusals(graphArmHolderMemo, rootIDSet(candidates))
	return closeAbandonedRootCandidatesMode(store, candidates, holders, now, abandonedCloseMode{
		enforce:    closeAbandonedEnforced(),
		dryRunHint: fmt.Sprintf("set %s=1 to enforce", closeAbandonedEnv),
		label:      "abandoned root",
		notes:      notes,
	})
}

// abandonedCloseMode says whether an abandoned-root pass may close, and how it
// names what it would close in a dry run.
type abandonedCloseMode struct {
	enforce    bool
	dryRunHint string
	label      string
	// budget bounds the subtree reads this pass, shared with the rest of the
	// pass (nil: unbounded).
	budget *wispGCBudget
	// notes print a per-root dry-run line only when it is new (nil: always).
	notes *wispGCNotes
	// memo, when set, remembers a "not closable" outcome (a live subtree, a
	// claimed or held stepless root, a failed close) under memoArm, so the
	// root is not walked again until it changes or the recheck passes.
	memo    *wispGCNotes
	memoArm string
	// onWalk is told each root whose subtree the pass read, in order (the
	// caller's rotation cursor).
	onWalk func(rootID string)
}

// closeAbandonedRootCandidatesMode is closeAbandonedRoots over candidates
// already listed from store, which is where they live and where they are
// closed.
func closeAbandonedRootCandidatesMode(store beads.Store, candidates []beads.Bead, holders wispGCHolders, now time.Time, mode abandonedCloseMode) error {
	enforce := mode.enforce
	cutoff := now.Add(-wispGCCloseAbandonedTTL)
	keep := func(root beads.Bead, reason string) {
		if mode.memo != nil {
			mode.memo.noteRefusal(mode.memoArm, root, now, reason)
		}
	}

	var closeErr error
	closed := 0
	wouldClose := 0
	for _, root := range candidates {
		if !isAbandonedRootCandidate(root) {
			continue
		}
		if convoycore.IsTerminalStatus(root.Status) {
			// Defensive: the candidate query already filters to nonterminal
			// (open/in_progress) roots, but a status the store reports as
			// terminal is not abandoned.
			continue
		}
		// Guard 3: never close a ZFC-exempt root.
		if isGCExempt(root) {
			continue
		}
		// Guard 1: only act once the root has been idle past the TTL.
		if !beadLastActivity(root).Before(cutoff) {
			continue
		}
		if mode.memo != nil && mode.memo.refusedNow(mode.memoArm, root, now) {
			continue
		}
		if !mode.budget.take() {
			break
		}
		if mode.onWalk != nil {
			mode.onWalk(root.ID)
		}
		terminal, descendants := subtreeTerminalExcludingRoot(store, root.ID)
		if !terminal {
			keep(root, "subtree not terminal")
			continue
		}
		// Guard 2: a stepless root is only closable once it is provably
		// unclaimed AND no live source bead is still attached to it — see
		// steplessRootIsAbandoned and wispGCHolders.liveAttachmentSource.
		if descendants == 0 && (!steplessRootIsAbandoned(root) || holders.liveAttachmentSource(root)) {
			keep(root, "stepless root claimed or held")
			continue
		}

		// Guard 5: dry-run default. Only count/log what we would close.
		if !enforce {
			wouldClose++
			if mode.notes.detail(mode.label + "\x00would-close\x00" + root.ID) {
				log.Printf("wisp gc: %s %s would be closed (dry-run; %s; descendants=%d)", mode.label, root.ID, mode.dryRunHint, descendants)
			}
			continue
		}

		if err := closeMoleculeWithReason(store, root.ID, abandonedRootCloseReason); err != nil {
			keep(root, "close failed: "+err.Error())
			holders.forget(root.ID)
			closeErr = errors.Join(closeErr, fmt.Errorf("closing abandoned root %s: %w", root.ID, err))
			continue
		}
		closed++
		log.Printf("wisp gc: closed abandoned root %s (descendants=%d)", root.ID, descendants)
	}

	if closed > 0 {
		log.Printf("wisp gc: closed %d abandoned root(s)", closed)
	}
	if wouldClose > 0 {
		log.Printf("wisp gc: %d %s(s) eligible for close (dry-run; %s)", wouldClose, mode.label, mode.dryRunHint)
	}
	return closeErr
}

// isAbandonedRootCandidate reports whether a root is in scope for the periodic
// abandoned-root closer. It accepts the same root universe the reactive
// autoclose path covers: source-workflow roots (gc.kind=workflow or
// gc.formula_contract=graph.v2, via sourceworkflow.IsWorkflowRoot) PLUS v1
// poured molecule roots (type=molecule, which carry neither kind nor contract
// metadata — see internal/formula/compile.go). The reactive
// autocloseMoleculeIfComplete closes type=molecule roots when a child-close
// event names them; this periodic sweep is the backstop for when that event was
// lost, so it must consider the same molecule roots — otherwise a v1 molecule
// whose final child-close event was dropped would stay open forever and keep
// fueling the wisp backlog the PR targets.
func isAbandonedRootCandidate(b beads.Bead) bool {
	return sourceworkflow.IsWorkflowRoot(b) || b.Type == "molecule"
}

// steplessRootIsAbandoned reports whether a root with no descendants is safe
// for the periodic sweep to close. Callers apply it only after the idle-TTL
// guard, so "abandoned" here means "still unclaimed long after anything that
// would claim it should have".
//
// A stepless root has no completion signal of its own, so the sweep classifies
// claim state with the ONE status predicate the launch path already owns:
// sling.ShouldPromoteWorkflowLaunchStatus. That predicate is the single
// definition of WHICH STATUSES MEAN UNCLAIMED, and reusing it here is what
// keeps the reaper from drifting into its own status literals. It is not a
// shared write path: sling.PromoteWorkflowLaunchBead runs only on the graph.v2
// launch branch (doStartGraphWorkflow, internal/sling/sling_core.go), while the
// v1 type=molecule root-only pours this sweep mostly targets are moved to
// in_progress by the worker's own `gc hook --claim`. Both writers land on the
// same side of the same classifier, which is all this guard needs.
//
// Both halves of that matter:
//
//   - Unclaimed + idle past TTL: either a root-only formula whose owner is long
//     gone, or an instantiation that died before writing its steps. Both are
//     garbage, and leaving them is what let root-only patrol wisps accumulate
//     unboundedly — one per restart cycle, invisible to the assignee-and-status
//     filtered queries meant to reconcile them (gastownhall/gascity ga-98b).
//   - Claimed: a live worker holds it. A root bead's UpdatedAt does not advance
//     while its agent works, so idle age cannot tell "busy" from "abandoned"
//     here; only the claim status can, and this one is no longer unclaimed.
//
// The unclaimed arm also reaps routed demand that has sat unclaimed past the
// TTL. That is intended — a day-old unclaimed root is stale demand, not a
// queue — and it is bounded three ways: the sweep is opt-in (see
// closeAbandonedEnv), it only ever considers formula/wisp roots
// (isAbandonedRootCandidate), and a deployment can park a perpetual root-only
// root with the gc.gc_exempt marker (isGCExempt).
//
// The attached case is NOT in that arm. An attached root-only wisp
// (privatizeAttachedRootOnlyWisp, internal/sling/sling.go) is unclaimed by
// construction and would otherwise land here, so the caller pairs this
// predicate with wispGCHolders.liveAttachmentSource; the root is reaped
// normally once its source bead goes terminal.
func steplessRootIsAbandoned(b beads.Bead) bool {
	return sling.ShouldPromoteWorkflowLaunchStatus(b.Status)
}

// liveAttachmentSource reports whether any bead still points at
// this root as its attachment. An attached root-only wisp
// (privatizeAttachedRootOnlyWisp, internal/sling/sling.go) is deliberately never
// routed and never claimed — the SOURCE bead is the claimable unit — so it stays
// unclaimed by construction and would otherwise be closed one TTL after pour
// regardless of how live the source bead's work still is. The root carries no
// back-pointer, but the source carries the forward one: molecule_id on the v1
// attach path (sling_core.go), workflow_id on the graph.v2 path. A non-terminal
// holder means live attachment state — findBlockingMolecule
// (internal/sling/sling_attachment.go) uses exactly that liveness to block a
// conflicting second attach, so closing the root out from under it would let two
// attachments land on one source bead.
//
// Fails CLOSED: any query error reports true so an unreadable store never
// widens what the sweep destroys.
//
// The holders are read from every store a holder may live in: the graph-class
// store, then each leg (a work bead is the usual holder, so on a split city
// the work legs are where it is found — R7). A leg set that cannot be read
// fails CLOSED, like a failed query.
//
// A suspended rig is not a leg (its store is not read: a read restarts its
// retired proxy), yet it can hold a live source. While any rig is suspended
// the guard therefore cannot prove a stepless root unattached, and it leaves
// the root open until the rig resumes.
//
// With a memo (the graph arm), the answer the reads gave is reused until the
// root changes or wispGCRefusalRecheck passes. A remembered "held" only keeps
// the root open longer; a remembered "not held" closes it on the pass that
// first found it, except in a dry run, which closes nothing. A close that
// fails can leave the root unchanged, so it forgets the answer (forget), and
// the retry reads the holders again. The suspension and unreadable-legs
// decisions read nothing, so they are made every pass.
func (h wispGCHolders) liveAttachmentSource(root beads.Bead) bool {
	rootID := root.ID
	if h.own != nil {
		return (h.graph != nil && storeHoldsLiveAttachmentSource(h.graph, rootID)) ||
			storeHoldsLiveAttachmentSource(h.own, rootID)
	}
	if h.legs != nil {
		if err := h.legs.unusable(); err != nil {
			log.Printf("wisp gc: cannot read the work legs for holders of stepless root %s (%v); leaving it open", rootID, err)
			return true
		}
	}
	// An attached root-only wisp records its one source (sling stamps
	// gc.source_bead_id / gc.source_store_ref): ask that source, and leave the
	// root open when the source is live, unreadable, or in a suspended rig. A
	// pointer whose store ref names no store this city serves (empty, a bare
	// or foreign city ref, a class ref, an undeclared or renamed rig) proves
	// nothing, and the root takes the no-pointer path below.
	src := strings.TrimSpace(root.Metadata[beadmeta.SourceBeadIDMetadataKey])
	var srcStore beads.Store
	pointerUsable := false
	if src != "" {
		store, suspended, ok := h.sourceStore(root)
		if ok {
			pointerUsable = true
			if suspended {
				h.pass.noteSuspended([]string{strings.TrimPrefix(strings.TrimSpace(root.Metadata[beadmeta.SourceStoreRefMetadataKey]), "rig:")})
				return true
			}
			srcStore = store
		}
	}
	if !pointerUsable {
		if suspended := h.suspendedRigs(); len(suspended) > 0 && beads.IsMoleculeType(root.Type) {
			// A molecule-typed root with no usable pointer may be an attached
			// root-only wisp whose source a suspended rig holds unread. A
			// task-typed root is a standalone wisp or workflow root, which no
			// v1 attach produces, so suspension says nothing about it.
			h.pass.noteSuspended(suspended)
			return true
		}
	}
	if answer, ok := h.memo.remembered(graphArmHolderMemo, root, h.now); ok {
		return answer == holderAnswerHeld
	}
	held := h.readHolders(root, pointerUsable, srcStore, src)
	answer := holderAnswerFree
	if held {
		answer = holderAnswerHeld
	}
	h.memo.noteRefusal(graphArmHolderMemo, root, h.now, answer)
	return held
}

// readHolders is liveAttachmentSource's reads: the recorded source, when the
// root's pointer is usable, then the beads pointing at the root in the
// graph-class store and in every leg.
func (h wispGCHolders) readHolders(root beads.Bead, pointerUsable bool, srcStore beads.Store, src string) bool {
	if pointerUsable && sourceIsLiveIn(srcStore, root, src) {
		return true
	}
	if h.graph != nil && storeHoldsLiveAttachmentSource(h.graph, root.ID) {
		return true
	}
	live := false
	if h.legs != nil {
		h.legs.each(func(store beads.Store) {
			if live || store == nil || sameBeadStore(store, h.graph) {
				return
			}
			live = storeHoldsLiveAttachmentSource(store, root.ID)
		})
	}
	return live
}

// forget drops the holder answer remembered for rootID, so the next pass that
// reaches the root reads its holders again.
func (h wispGCHolders) forget(rootID string) {
	h.memo.forgetRefusal(graphArmHolderMemo, rootID)
}

func (h wispGCHolders) suspendedRigs() []string {
	if h.legs == nil {
		return nil
	}
	return h.legs.suspendedRigs()
}

// sourceStore resolves root's gc.source_store_ref to the store its source
// lives in. ok=false when the ref names no store this city serves. On a
// single-store city (no legs) only THIS city's ref resolves (as on a split
// city: a class ref or another city's ref names nothing here), to the graph
// store, which is the city store there.
func (h wispGCHolders) sourceStore(root beads.Bead) (beads.Store, bool, bool) {
	ref := strings.TrimSpace(root.Metadata[beadmeta.SourceStoreRefMetadataKey])
	if ref == "" {
		return nil, false, false
	}
	if h.legs == nil {
		if rig, ok := storeref.ScopeRigContext(ref); ok && rig == "" && !storeref.IsClassRef(ref) && h.graph != nil &&
			(h.cityName == "" || ref == "city:"+h.cityName) {
			return h.graph, false, true
		}
		return nil, false, false
	}
	return h.legs.storeForRef(ref)
}

// sourceIsLiveIn reports whether root's source src, read from store, may still
// hold it: it is open, or it cannot be read. A source that is terminal or gone
// holds nothing. Fails CLOSED.
func sourceIsLiveIn(store beads.Store, root beads.Bead, src string) bool {
	b, err := store.Get(src)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return false
	case err != nil:
		log.Printf("wisp gc: cannot read source %s of stepless root %s (%v); leaving it open", src, root.ID, err)
		return true
	}
	return !convoycore.IsTerminalStatus(b.Status)
}

// storeHoldsLiveAttachmentSource is the holder query against one store. Fails
// CLOSED on a query error.
func storeHoldsLiveAttachmentSource(store beads.Store, rootID string) bool {
	for _, key := range []string{beadmeta.MoleculeIDMetadataKey, "workflow_id"} {
		// One leg of the holder walk (the graph store, then every work leg):
		// a holder lives in exactly one of them.
		var holders []beads.Bead
		err := beads.FederatedRead(func() (err error) {
			holders, err = store.List(beads.ListQuery{
				Metadata:      map[string]string{key: rootID},
				IncludeClosed: true,
				TierMode:      beads.TierBoth,
			})
			return err
		})
		if err != nil {
			log.Printf("wisp gc: cannot resolve %s holders for stepless root %s (%v); leaving it open", key, rootID, err)
			return true
		}
		for _, h := range holders {
			if h.ID == rootID {
				continue
			}
			if !convoycore.IsTerminalStatus(h.Status) {
				return true
			}
		}
	}
	return false
}

// isGCExempt reports whether a root carries the gc.gc_exempt opt-out marker.
// The marker is operator/deployment-supplied: a deployment stamps
// gc.gc_exempt=true on any long-lived root it never wants the abandoned-root
// sweep to close (for example a perpetual compaction-loop root). The SDK
// deliberately does NOT stamp it for any specific root — hardcoding a named
// deployment root here would violate the zero-hardcoded-roles invariant — so
// this guard protects exactly the roots a deployment has explicitly marked, and
// nothing more. Until a deployment marks its protected roots the guard is a
// no-op, which is safe because the closer stays dry-run by default.
func isGCExempt(b beads.Bead) bool {
	return parseBoolEnv(strings.TrimSpace(b.Metadata[beadmeta.GCExemptMetadataKey]))
}

// beadLastActivity returns the most recent activity timestamp for a bead,
// falling back to CreatedAt when UpdatedAt is zero (legacy beads), mirroring
// the store's UpdatedBefore reference-time semantics.
func beadLastActivity(b beads.Bead) time.Time {
	if !b.UpdatedAt.IsZero() {
		return b.UpdatedAt
	}
	return b.CreatedAt
}

// openWispGCRootCandidates lists every NONTERMINAL root across the full wisp GC
// root universe (see wispGCRootSelectors), the candidates the abandoned-root
// sweep considers. It enumerates BOTH open and in_progress: graph.v2 workflow
// roots are promoted to in_progress at launch (sling.PromoteWorkflowLaunchBead),
// so an open-only query would make a stale in_progress graph root with terminal
// descendants invisible to the sweep — the exact lost-finalize leak this sweep
// exists to close. The caller applies the IsWorkflowRoot / TTL / terminal /
// exempt filters.
func openWispGCRootCandidates(store beads.Store) (roots []beads.Bead, err error) {
	// The graph leg of the wisp GC root union: a v1 poured molecule root
	// (work class) lives on a work leg (enumerateWorkClassMoleculeRoots).
	err = beads.FederatedRead(func() (err error) {
		roots, err = enumerateWispGCRoots(store, "open", "in_progress")
		return err
	})
	return roots, err
}

// purgeExpiredBeadClosures purges aged closed roots, deleting each root's full
// ownership closure. batchCap bounds how many root closures one sweep attempts
// (see wispGCClosurePurgeBatchCap) so a first-deploy backlog of newly-collectible
// roots drains across ticks instead of in one unbounded pass.
func purgeExpiredBeadClosures(store beads.Store, entries []beads.Bead, cutoff time.Time, batchCap int) (int, error) {
	return purgeExpiredBeadClosuresWith(store, entries, closurePurgeOpts{cutoff: cutoff, batchCap: batchCap})
}

// closurePurgeOpts parameterizes purgeExpiredBeadClosuresWith.
type closurePurgeOpts struct {
	cutoff time.Time
	// batchCap bounds delete attempts (0: unbounded); a refusal is not one.
	batchCap int
	// budget bounds closures walked this pass, shared with the rest of the
	// pass; a remembered refusal costs none (nil: unbounded).
	budget *wispGCBudget
	// memoFailures also remembers a delete that failed, so a closure whose
	// delete keeps failing does not take a walk every pass.
	memoFailures bool
	// onWalk is told each root whose closure the pass walked, in order.
	onWalk func(rootID string)
	// prefetch reads the live rows of the roots this pass can walk in one
	// batched read, instead of one live read per root.
	prefetch bool
	// age is the clock an entry ages on (nil: CreatedAt).
	age func(beads.Bead) time.Time
	// arm names the caller for the notes' keys and the summary line.
	arm   string
	notes *wispGCNotes
	now   time.Time
}

// purgeExpiredBeadClosuresWith is purgeExpiredBeadClosures that remembers
// refusals (a root refused recently and unchanged since is not walked again),
// bounds the walks, and logs one summary line for the refusals it met.
func purgeExpiredBeadClosuresWith(store beads.Store, entries []beads.Bead, opts closurePurgeOpts) (int, error) {
	age := opts.age
	if age == nil {
		age = func(b beads.Bead) time.Time { return b.CreatedAt }
	}
	purged, attempted, refused, remembered := 0, 0, 0, 0
	var prefetched map[string]beads.Bead
	var deleteErr error
	for _, entry := range entries {
		if opts.batchCap > 0 && attempted >= opts.batchCap {
			break
		}
		at := age(entry)
		if at.IsZero() || !at.Before(opts.cutoff) {
			continue
		}
		if opts.notes.refusedNow(opts.arm, entry, opts.now) {
			remembered++
			continue
		}
		if !opts.budget.take() {
			break
		}
		if opts.onWalk != nil {
			opts.onWalk(entry.ID)
		}
		if opts.prefetch && prefetched == nil {
			prefetched = prefetchLiveRoots(store, entries, opts)
		}
		var row *beads.Bead
		if b, ok := prefetched[entry.ID]; ok {
			row = &b
		}
		err := deleteExpiredBeadClosureFrom(store, entry.ID, row)
		if errors.Is(err, errWorkflowDeleteLiveDescendants) {
			// The strand guard refused the closure delete: an open member still
			// holds the root. That is a skip, not a sweep failure — the root
			// becomes eligible once the member finishes — and it is not
			// charged to the delete cap: no delete was attempted, and a root
			// refused on every sweep would otherwise hold a cap slot every
			// sweep, starving the collectible roots listed behind it. It is
			// remembered so the closure is not walked again until its root
			// changes.
			refused++
			if opts.notes.noteRefusal(opts.arm, entry, opts.now, err.Error()) {
				log.Printf("wisp gc: not purging closed root %s: %v", entry.ID, err)
			}
			continue
		}
		attempted++
		if err != nil {
			if errors.Is(err, errBeadNoLongerEligible) {
				continue
			}
			if opts.memoFailures {
				opts.notes.noteRefusal(opts.arm, entry, opts.now, "delete failed: "+err.Error())
			}
			deleteErr = errors.Join(deleteErr, fmt.Errorf("deleting expired bead %q: %w", entry.ID, err))
			continue
		}
		opts.notes.forgetRefusal(opts.arm, entry.ID)
		purged++
	}
	if refused+remembered > 0 {
		label := opts.arm
		if label == "" {
			label = "closure"
		}
		log.Printf("wisp gc: %s purge left %d closed root(s) whose closure holds a bead that is not closed, or whose delete failed (%d walked this pass, %d remembered)", label, refused+remembered, refused, remembered)
	}
	return purged, deleteErr
}

// errBeadNoLongerEligible signals that deleteFn's live re-check found the
// candidate no longer eligible for deletion (reopened, or already gone) — a
// deliberate skip, not a delete failure, so purgeExpiredBeadClosuresWith must not count
// it as either purged or an error.
var errBeadNoLongerEligible = errors.New("bead skipped: no longer eligible for deletion")

// deleteExpiredBeadClosureFrom deletes rootID's ownership closure, given, when
// the caller prefetched it from the live backing in one batched read, the
// root's live row (nil: read it here).
func deleteExpiredBeadClosureFrom(store beads.Store, rootID string, prefetched *beads.Bead) error {
	// closedWispGCEntries can answer from a stale cached snapshot. A root
	// reopened (live work resumed) inside the cache window — after the
	// snapshot was taken but before this purge reaches it — is no longer
	// gc-eligible; re-verify against the live store immediately before the
	// destructive batch delete. A root that is already gone has nothing to
	// delete.
	var live beads.Bead
	var err error
	if prefetched != nil {
		live = *prefetched
	} else {
		live, err = beads.HandlesFor(store).Live.Get(rootID)
	}
	switch {
	case errors.Is(err, beads.ErrNotFound):
		// Root already gone — nothing to delete.
		return errBeadNoLongerEligible
	case err != nil:
		// Any other live-read failure means we cannot PROVE the root is still
		// collectible; surface it rather than silently skipping, matching
		// reapOrphanedClosedWisps' treatment of an unreadable root Get.
		return fmt.Errorf("live re-verify of root %q before closure delete: %w", rootID, err)
	}
	if live.Status != "closed" {
		return errBeadNoLongerEligible
	}
	// The closure is deleted as one batch: a store that supports
	// beads.BatchDeleter (the sqlite/Dolt graph store) removes the collected
	// ownership tree with a single `bd delete … --force`, which deletes exactly
	// those ids and lets ON DELETE CASCADE drop their edges while orphaning any
	// external dependents; other stores fall back to per-bead deletion. Because
	// the delete is not dependent-recursive, collectExpiredBeadClosureStatus must (and
	// does) gather only the ownership closure so live work outside it is never
	// reached.
	ids, open, rows, err := collectExpiredBeadClosureWithOpen(store, live)
	if err != nil {
		return err
	}
	// A closed root is not proof its closure is dead. A member reopened after
	// the root closed (a step put back in progress) is live work, and the
	// set-level strand guard below sees only gc.root_bead_id members, which a
	// v1 poured molecule's ParentID-linked steps are not. Refuse the whole
	// closure when any member is not closed; it becomes eligible again once
	// that member is.
	if len(open) > 0 {
		return workflowDeleteRefusal(rootID, open)
	}
	// Every member's own status was just checked, as the backing holds it.
	// The set-level strand guard adds an open bead recorded against a nested
	// root (gc.root_bead_id), which the walk does not follow, so it is asked
	// about the root and every member that can be a root (a workflow, wisp or
	// molecule); a plain step costs no extra read.
	if err := assertWorkflowDeleteSetLeavesNothingStrandedFor(store, ids, nestedRootCandidates(ids, rows, rootID)); err != nil {
		return err
	}
	return deleteWorkflowBeadsBatchGuarded(store, ids)
}

// nestedRootCandidates are the root and the closure members that could own
// gc.root_bead_id members of their own. A member whose row the walk did not
// read is included (fail closed).
func nestedRootCandidates(ids []string, rows map[string]beads.Bead, rootID string) []string {
	var out []string
	for _, id := range ids {
		if id == rootID {
			out = append(out, id)
			continue
		}
		b, ok := rows[id]
		if !ok || sourceworkflow.IsWorkflowRoot(b) || beads.IsMoleculeType(b.Type) || isWispPolicyMetadata(b.Metadata) {
			out = append(out, id)
		}
	}
	return out
}

// collectExpiredBeadClosureWithOpen is collectExpiredBeadClosureStatus plus the ids
// of every collected bead that is not closed. Rows come from the walk; a
// member reached only through a dependency edge is read live. A member whose
// row cannot be read counts as open: the purge fails closed. An open transient
// notification (an unread mail or nudge) does not hold the closure, the same
// carve-out the strand guard makes (workflowDeleteSkip): a lingering message
// must not block retention forever.
//
// The walk reads store's live backing (liveBackingStore), never its cache. A
// list through the cache drops a backing row the cache holds closed, or
// answers with the cached closed row, so a member reopened behind the cache
// would be deleted with the closure, or stranded under a deleted root.
//
// root is the root's own row, already read live by the caller, so it is not
// read again.
func collectExpiredBeadClosureWithOpen(store beads.Store, root beads.Bead) ([]string, []string, map[string]beads.Bead, error) {
	ids, rows, err := collectExpiredBeadClosureStatus(liveBackingStore(store), root.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	rows[root.ID] = root
	reader := beads.HandlesFor(store).Live
	var open []string
	for _, id := range ids {
		b, known := rows[id]
		if !known {
			got, err := reader.Get(id)
			switch {
			case errors.Is(err, beads.ErrNotFound):
				continue
			case err != nil:
				open = append(open, id)
				continue
			}
			b = got
			rows[id] = got
		}
		if b.Status != "closed" && !isTransientNotificationBead(b) {
			open = append(open, id)
		}
	}
	return ids, open, rows, nil
}

// collectExpiredBeadClosureStatus walks rootID's ownership closure and returns
// its ids plus the row of every member the walk read one for.
func collectExpiredBeadClosureStatus(store beads.Store, rootID string) ([]string, map[string]beads.Bead, error) {
	if store == nil {
		return nil, nil, fmt.Errorf("bead store unavailable")
	}
	status := make(map[string]beads.Bead)
	rootOwned := make([]string, 0, 4)
	related, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
		IncludeClosed: true,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list workflow-owned beads for %s: %w", rootID, err)
	}
	for _, bead := range related {
		if bead.ID != "" && bead.ID != rootID {
			rootOwned = append(rootOwned, bead.ID)
			status[bead.ID] = bead
		}
	}

	if childrenFollowParentChildDeps(store) {
		return collectClosureByLevel(store, rootID, rootOwned, status)
	}

	seen := make(map[string]struct{}, len(rootOwned)+1)
	ids := make([]string, 0, len(rootOwned)+1)
	var visit func(string) error
	visit = func(id string) error {
		if id == "" {
			return nil
		}
		if _, ok := seen[id]; ok {
			return nil
		}
		seen[id] = struct{}{}

		if id == rootID {
			for _, relatedID := range rootOwned {
				if err := visit(relatedID); err != nil {
					return err
				}
			}
		}

		// Treat structural parentage as workflow ownership. Some molecule step
		// beads are linked only by ParentID / parent-child deps and do not carry
		// gc.root_bead_id metadata, so GC must follow those ownership edges while
		// still ignoring non-ownership deps such as blocks or waits-for.
		children, err := store.Children(id, beads.IncludeClosed, beads.WithBothTiers)
		if err != nil {
			return fmt.Errorf("list children for %s: %w", id, err)
		}
		for _, child := range children {
			if child.ID != "" {
				status[child.ID] = child
			}
			if err := visit(child.ID); err != nil {
				return err
			}
		}

		upDeps, err := store.DepList(id, "up")
		if err != nil {
			return fmt.Errorf("list dependents for %s: %w", id, err)
		}
		for _, dep := range upDeps {
			if dep.Type != "parent-child" || dep.IssueID == "" {
				continue
			}
			if err := visit(dep.IssueID); err != nil {
				return err
			}
		}

		ids = append(ids, id)
		return nil
	}
	if err := visit(rootID); err != nil {
		return nil, nil, err
	}
	return ids, status, nil
}

// childrenFollowParentChildDeps reports whether store's batched children read
// answers a closure level completely: a bd store keeps a bead's parent as a
// parent-child dependency, and its ChildrenOfAny reads exactly those edges, so
// one batched read per level covers both the ParentID and the dependency-only
// children the per-node walk asks for separately. Other stores (in-memory,
// file, SQLite) keep the two apart, and keep the per-node walk.
func childrenFollowParentChildDeps(store beads.Store) bool {
	_, ok := liveBackingStore(store).(*beads.BdStore)
	return ok
}

// liveBackingStore is the store under store's bead-policy and cache layers,
// whose reads no cached row stands in for. A store with neither layer is its
// own backing.
func liveBackingStore(store beads.Store) beads.Store {
	inner, _, _ := unwrapBeadPolicyStore(store)
	if cache, ok := inner.(*beads.CachingStore); ok {
		inner, _, _ = unwrapBeadPolicyStore(cache.Backing())
	}
	return inner
}

// collectClosureByLevel is the ownership-closure walk one level at a time,
// with one batched children read per level (beads.ChildrenOfAny): a couple of
// bd calls per level instead of two per member. ids come back deepest level
// first and the root last, the order a per-bead delete needs.
func collectClosureByLevel(store beads.Store, rootID string, rootOwned []string, status map[string]beads.Bead) ([]string, map[string]beads.Bead, error) {
	seen := map[string]bool{rootID: true}
	levels := [][]string{{rootID}}
	frontier := []string{rootID}
	for _, id := range rootOwned {
		if !seen[id] {
			seen[id] = true
			frontier = append(frontier, id)
		}
	}
	if len(frontier) > 1 {
		levels = append(levels, frontier[1:])
	}
	for len(frontier) > 0 {
		children, err := beads.ChildrenOfAny(store, frontier, beads.WithBothTiers)
		if err != nil {
			return nil, nil, fmt.Errorf("list children of %d closure member(s) under %s: %w", len(frontier), rootID, err)
		}
		var next []string
		for _, child := range children {
			if child.ID == "" || seen[child.ID] {
				continue
			}
			seen[child.ID] = true
			status[child.ID] = child
			next = append(next, child.ID)
		}
		if len(next) > 0 {
			levels = append(levels, next)
		}
		frontier = next
	}
	var ids []string
	for i := len(levels) - 1; i >= 0; i-- {
		ids = append(ids, levels[i]...)
	}
	return ids, status, nil
}

func gcRetentionTTLString(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return d.String()
}

// rootIDSet is the set of the ids in roots.
func rootIDSet(roots []beads.Bead) map[string]bool {
	out := make(map[string]bool, len(roots))
	for _, r := range roots {
		out[r.ID] = true
	}
	return out
}

// wispGCBudget bounds one pass's walks (subtree and closure reads): a count
// shared across every leg and arm of the pass, and a wall-clock deadline. A
// nil budget is unbounded.
type wispGCBudget struct {
	walks    int
	deadline time.Time
	// ctx stops the pass: no walk starts once it is done.
	ctx context.Context
	// reserve is how many walks take leaves untouched (an arm that must
	// leave room for another).
	reserve int
	// took is how many walks the pass has started. The first is exempt from
	// the deadline (not from ctx): a pass whose listings alone outlast the
	// wall budget still makes progress.
	took int
}

// wispGCClock is the wall clock the budget's deadline is set and checked on.
// Package var so tests can advance it from a slow fake store instead of
// sleeping.
var wispGCClock = time.Now

// take spends one walk, or reports that the pass may not start another: out
// of walks (beyond the reserve), past the deadline (except for the pass's
// first walk), or stopping.
func (b *wispGCBudget) take() bool {
	if b == nil {
		return true
	}
	if b.walks <= b.reserve || b.ctxDone() || (b.took > 0 && b.pastDeadline()) {
		return false
	}
	b.walks--
	b.took++
	return true
}

// stopped reports whether the pass must stop starting work: its ctx is done
// or its wall-clock deadline has passed.
func (b *wispGCBudget) stopped() bool {
	return b.ctxDone() || b.pastDeadline()
}

func (b *wispGCBudget) ctxDone() bool {
	return b != nil && b.ctx != nil && b.ctx.Err() != nil
}

func (b *wispGCBudget) pastDeadline() bool {
	return b != nil && !b.deadline.IsZero() && !wispGCClock().Before(b.deadline)
}

// exhausted reports whether the pass has no walk left.
func (b *wispGCBudget) exhausted() bool {
	return b != nil && (b.walks <= 0 || b.stopped())
}

// prefetchLiveRoots reads, in one batched read of the live backing, the rows of
// the entries this purge can still walk (past the cutoff, not remembered, at
// most the budget's remaining walks). A store with no batched exact read
// answers nothing, and each root is then read on its own.
func prefetchLiveRoots(store beads.Store, entries []beads.Bead, opts closurePurgeOpts) map[string]beads.Bead {
	age := opts.age
	if age == nil {
		age = func(b beads.Bead) time.Time { return b.CreatedAt }
	}
	limit := len(entries)
	if opts.budget != nil && opts.budget.walks+1 < limit {
		limit = opts.budget.walks + 1
	}
	var ids []string
	for _, entry := range entries {
		if len(ids) >= limit {
			break
		}
		at := age(entry)
		if at.IsZero() || !at.Before(opts.cutoff) || opts.notes.refusedNow(opts.arm, entry, opts.now) {
			continue
		}
		ids = append(ids, entry.ID)
	}
	rows, err := beads.PrefetchExact(liveBackingStore(store), ids)
	if err != nil {
		return map[string]beads.Bead{}
	}
	return rows
}
