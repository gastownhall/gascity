package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
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
	runGC(graphStore beads.GraphStore, mailStore beads.MailStore, now time.Time) (int, error)
}

// memoryWispGC is the production implementation of wispGC.
type memoryWispGC struct {
	interval         time.Duration
	ttl              time.Duration
	mailRetentionTTL time.Duration
	lastRun          time.Time
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

func (m *memoryWispGC) runGC(graphStore beads.GraphStore, mailStore beads.MailStore, now time.Time) (int, error) {
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
		if abandonedErr := closeAbandonedRoots(store, now); abandonedErr != nil {
			deleteErr = errors.Join(deleteErr, abandonedErr)
		}

		entries, err := closedWispGCEntries(store)
		if err != nil {
			return 0, err
		}

		cutoff := now.Add(-m.ttl)
		closurePurged, closureDeleteErr := purgeExpiredBeadClosures(store, entries, cutoff, wispGCClosurePurgeBatchCap)
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

// closeGeneratedMembersForClosedRoots repairs terminal workflow roots whose
// finalizer, supersession, or partial materialization left generated members
// open. Closed roots are the authority boundary: live roots are never listed
// and therefore never touched. Each subtree close is ordered and idempotent.
func closeGeneratedMembersForClosedRoots(store beads.Store) (int, error) {
	roots, err := closedWispGCEntries(store)
	if err != nil {
		return 0, err
	}
	closed := 0
	var closeErr error
	for _, root := range roots {
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
		{"molecule", beads.ListQuery{Type: "molecule", TierMode: beads.TierBoth}},
		{"wisp", beads.ListQuery{Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}, TierMode: beads.TierBoth}},
		{"graph.v2", beads.ListQuery{Metadata: map[string]string{beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2}, TierMode: beads.TierBoth}},
		{"workflow", beads.ListQuery{Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}, TierMode: beads.TierBoth}},
	}
}

// enumerateWispGCRoots unions wispGCRootSelectors across the given statuses,
// deduping by bead ID so a root matching more than one selector (for example a
// graph.v2 root that also carries gc.kind=workflow) appears once.
func enumerateWispGCRoots(store beads.Store, statuses ...string) ([]beads.Bead, error) {
	entries := make([]beads.Bead, 0)
	seen := make(map[string]struct{})
	selectors := wispGCRootSelectors()
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
func closedWispGCEntries(store beads.Store) ([]beads.Bead, error) {
	return enumerateWispGCRoots(store, "closed")
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
// ownership travels over both (collectExpiredBeadClosure walks both too) —
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
			// Mirror collectExpiredBeadClosure: ownership is carried by the
			// parent_id COLUMN or by a parent-child DEP ROW, and some step
			// beads have only the dep row. A column-only fence would reap such
			// a row out from under a live parent ("down": rows where this bead
			// depends on another), or out from under its own dep-linked
			// children ("up": rows where another bead depends on this one).
			// Both reads sit inside the single probe budget charged above.
			linked, linkErr := hasParentChildDepEdge(store, c.ID)
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
				root, getErr := store.Get(rootID)
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

		attempted++
		if err := deleteWorkflowBead(store, c.ID); err != nil {
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

// hasParentChildDepEdge reports whether id sits on either end of a parent-child
// dependency row. The parent_id column is not the only ownership link: some
// molecule step beads are joined to their parent purely by a dep row (see
// collectExpiredBeadClosure, which walks both). "down" deps answer "does this
// bead have a dep-linked parent", "up" deps answer "does it have dep-linked
// children" — either makes it a subtree member rather than a free-standing
// leaf, so the orphan reaper must leave it to the owning root's closure purge.
func hasParentChildDepEdge(store beads.Store, id string) (bool, error) {
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
//     AND steplessRootHasLiveAttachmentSource finds no live source bead still
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
func closeAbandonedRoots(store beads.Store, now time.Time) error {
	if store == nil {
		return nil
	}
	candidates, err := openWispGCRootCandidates(store)
	if err != nil {
		// Best-effort: a list failure must not fail the GC tick.
		return fmt.Errorf("listing open workflow roots for abandoned-root sweep: %w", err)
	}

	enforce := closeAbandonedEnforced()
	cutoff := now.Add(-wispGCCloseAbandonedTTL)

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
		terminal, descendants := subtreeTerminalExcludingRoot(store, root.ID)
		if !terminal {
			continue
		}
		// Guard 2: a stepless root is only closable once it is provably
		// unclaimed AND no live source bead is still attached to it — see
		// steplessRootIsAbandoned and steplessRootHasLiveAttachmentSource.
		if descendants == 0 && (!steplessRootIsAbandoned(root) || steplessRootHasLiveAttachmentSource(store, root.ID)) {
			continue
		}

		// Guard 5: dry-run default. Only count/log what we would close.
		if !enforce {
			wouldClose++
			log.Printf("wisp gc: abandoned root %s would be closed (dry-run; set %s=1 to enforce; descendants=%d)", root.ID, closeAbandonedEnv, descendants)
			continue
		}

		if err := closeMoleculeWithReason(store, root.ID, abandonedRootCloseReason); err != nil {
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
		log.Printf("wisp gc: %d abandoned root(s) eligible for close (dry-run; set %s=1 to enforce)", wouldClose, closeAbandonedEnv)
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
// predicate with steplessRootHasLiveAttachmentSource; the root is reaped
// normally once its source bead goes terminal.
func steplessRootIsAbandoned(b beads.Bead) bool {
	return sling.ShouldPromoteWorkflowLaunchStatus(b.Status)
}

// steplessRootHasLiveAttachmentSource reports whether any bead still points at
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
func steplessRootHasLiveAttachmentSource(store beads.Store, rootID string) bool {
	for _, key := range []string{beadmeta.MoleculeIDMetadataKey, "workflow_id"} {
		holders, err := store.List(beads.ListQuery{
			Metadata:      map[string]string{key: rootID},
			IncludeClosed: true,
			TierMode:      beads.TierBoth,
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
func openWispGCRootCandidates(store beads.Store) ([]beads.Bead, error) {
	return enumerateWispGCRoots(store, "open", "in_progress")
}

// purgeExpiredBeadClosures purges aged closed roots, deleting each root's full
// ownership closure. batchCap bounds how many root closures one sweep attempts
// (see wispGCClosurePurgeBatchCap) so a first-deploy backlog of newly-collectible
// roots drains across ticks instead of in one unbounded pass.
func purgeExpiredBeadClosures(store beads.Store, entries []beads.Bead, cutoff time.Time, batchCap int) (int, error) {
	return purgeExpiredBeads(store, entries, cutoff, batchCap, deleteExpiredBeadClosure)
}

// errBeadNoLongerEligible signals that deleteFn's live re-check found the
// candidate no longer eligible for deletion (reopened, or already gone) — a
// deliberate skip, not a delete failure, so purgeExpiredBeads must not count
// it as either purged or an error.
var errBeadNoLongerEligible = errors.New("bead skipped: no longer eligible for deletion")

// purgeExpiredBeads deletes each entry older than cutoff via deleteFn and
// returns the count successfully purged. When batchCap > 0 it bounds the number
// of DELETE ATTEMPTS per call — counting failures, not just successes — so a
// large backlog or a failing delete backend cannot make one sweep attempt an
// unbounded amount of deletion work; the remainder drains on later ticks.
// batchCap <= 0 disables the bound. Entries skipped for age never consume the cap.
func purgeExpiredBeads(store beads.Store, entries []beads.Bead, cutoff time.Time, batchCap int, deleteFn func(beads.Store, string) error) (int, error) {
	purged := 0
	attempted := 0
	var deleteErr error
	for _, entry := range entries {
		if batchCap > 0 && attempted >= batchCap {
			break
		}
		if entry.CreatedAt.IsZero() || !entry.CreatedAt.Before(cutoff) {
			continue
		}
		attempted++
		if err := deleteFn(store, entry.ID); err != nil {
			if errors.Is(err, errBeadNoLongerEligible) {
				continue
			}
			deleteErr = errors.Join(deleteErr, fmt.Errorf("deleting expired bead %q: %w", entry.ID, err))
			continue
		}
		purged++
	}
	return purged, deleteErr
}

func deleteExpiredBeadClosure(store beads.Store, rootID string) error {
	// closedWispGCEntries can answer from a stale cached snapshot. A root
	// reopened (live work resumed) inside the cache window — after the
	// snapshot was taken but before this purge reaches it — is no longer
	// gc-eligible; re-verify against the live store immediately before the
	// destructive batch delete. A root that is already gone has nothing to
	// delete.
	live, err := beads.HandlesFor(store).Live.Get(rootID)
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
	// the delete is not dependent-recursive, collectExpiredBeadClosure must (and
	// does) gather only the ownership closure so live work outside it is never
	// reached.
	ids, err := collectExpiredBeadClosure(store, rootID)
	if err != nil {
		return err
	}
	return deleteWorkflowBeadsBatch(store, ids)
}

func collectExpiredBeadClosure(store beads.Store, rootID string) ([]string, error) {
	if store == nil {
		return nil, fmt.Errorf("bead store unavailable")
	}
	rootOwned := make([]string, 0, 4)
	related, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
		IncludeClosed: true,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return nil, fmt.Errorf("list workflow-owned beads for %s: %w", rootID, err)
	}
	for _, bead := range related {
		if bead.ID != "" && bead.ID != rootID {
			rootOwned = append(rootOwned, bead.ID)
		}
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
		return nil, err
	}
	return ids, nil
}

func gcRetentionTTLString(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return d.String()
}
