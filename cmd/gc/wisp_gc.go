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
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

const mailReadMetadataKey = "mail.read"

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
	// also covers list failures.
	runGC(store beads.Store, now time.Time) (int, error)
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

func (m *memoryWispGC) runGC(store beads.Store, now time.Time) (int, error) {
	m.lastRun = now
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

		// Close abandoned OPEN roots BEFORE the closed-root purge below so a
		// root closed this tick becomes eligible for purging on a later tick
		// (once it has aged past m.ttl as a closed bead). Best-effort: never
		// fails the GC tick.
		if abandonedErr := closeAbandonedRoots(store, now); abandonedErr != nil {
			deleteErr = errors.Join(deleteErr, abandonedErr)
		}

		entries, err := closedWispGCEntries(store)
		if err != nil {
			return 0, err
		}

		cutoff := now.Add(-m.ttl)
		closurePurged, closureDeleteErr := purgeExpiredBeadClosures(store, entries, cutoff)
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

	if m.mailRetentionTTL > 0 {
		mailEntries, mailErr := readMessageWispGCEntries(store)
		if mailErr == nil {
			mailPurged, mailDeleteErr := purgeExpiredBeadRoots(store, mailEntries, now.Add(-m.mailRetentionTTL))
			purged += mailPurged
			deleteErr = errors.Join(deleteErr, mailDeleteErr)
			if mailPurged > 0 {
				log.Printf("wisp gc: purged %d read message wisps (retention_ttl=%s)", mailPurged, gcRetentionTTLString(m.mailRetentionTTL))
			}
		} else {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("listing read message wisps: %w", mailErr))
		}
	}

	return purged, deleteErr
}

func closedWispGCEntries(store beads.Store) ([]beads.Bead, error) {
	entries := make([]beads.Bead, 0)
	seen := make(map[string]struct{})
	appendUnique := func(items []beads.Bead) {
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
	molecules, err := store.List(beads.ListQuery{Status: "closed", Type: "molecule", TierMode: beads.TierBoth})
	if err != nil {
		return nil, fmt.Errorf("listing closed molecule roots: %w", err)
	}
	appendUnique(molecules)
	wisps, err := store.List(beads.ListQuery{Status: "closed", Metadata: map[string]string{beadmeta.KindMetadataKey: "wisp"}, TierMode: beads.TierBoth})
	if err != nil {
		return nil, fmt.Errorf("listing closed wisp roots: %w", err)
	}
	appendUnique(wisps)
	return entries, nil
}

// reapOrphanedClosedWisps reaps closed wisp-tier descendants whose owning root
// is gone or already terminal but which the root-rooted closure purge never
// enumerates (their root is absent from, or never appears in, the closed-root
// list). Candidates are closed wisp-tier rows carrying a gc.root_bead_id
// pointer and older than cutoff.
//
// Safety: a descendant is reaped only when its root is provably collectible —
// the root Get returns ErrNotFound (root gone) or the root is terminal
// (closed/tombstone). A live/open root, or any other (unreadable) Get error,
// causes the descendant to be SKIPPED so an in-flight workflow is never
// stripped of its closed steps. The per-root Get decision is cached so many
// siblings sharing one dead root cost a single Get.
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
		Status:    "closed",
		TierMode:  beads.TierWisps,
		AllowScan: true,
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
	var deleteErr error
	for _, c := range candidates {
		if batchCap > 0 && reaped >= batchCap {
			break
		}

		rootID := c.Metadata[beadmeta.RootBeadIDMetadataKey]
		if rootID == "" {
			// No root pointer — out of scope for orphan reaping.
			continue
		}

		// Reuse the closure purge's age semantics: skip zero/recent rows.
		if c.CreatedAt.IsZero() || !c.CreatedAt.Before(cutoff) {
			continue
		}

		decision, cached := rootCollectible[rootID]
		if !cached {
			root, getErr := store.Get(rootID)
			switch {
			case errors.Is(getErr, beads.ErrNotFound):
				decision = true // root gone
			case getErr == nil && convoycore.IsTerminalStatus(root.Status):
				decision = true // root terminal
			case getErr == nil:
				decision = false // root live/open — never reap its descendants
			default:
				// Any other Get error: cannot prove safe — skip without caching
				// as collectible. Surface so the sweep records the failure.
				decision = false
				collectErr = errors.Join(collectErr, fmt.Errorf("resolving root %q for orphan %q: %w", rootID, c.ID, getErr))
			}
			rootCollectible[rootID] = decision
		}
		if !decision {
			continue
		}

		if !enforce {
			reaped++
			continue
		}

		if err := deleteWorkflowBead(store, c.ID); err != nil {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("reaping orphaned closed wisp %q: %w", c.ID, err))
			continue
		}
		reaped++
	}

	if reaped > 0 {
		if enforce {
			log.Printf("wisp gc: reaped %d orphaned closed wisp(s)", reaped)
		} else {
			log.Printf("wisp gc: %d orphaned closed wisp(s) would be reaped (dry-run; set %s=1 to enforce)", reaped, reapOrphansEnv)
		}
	}

	if !enforce {
		// Dry-run never mutates: report would-be count via log only, return 0
		// purged so callers don't over-count deletions that did not happen.
		return 0, errors.Join(collectErr, deleteErr)
	}

	return reaped, errors.Join(collectErr, deleteErr)
}

// closeAbandonedRoots closes OPEN workflow roots whose entire descendant
// subtree is terminal and whose own last activity is older than the
// conservative TTL. This is the periodic counterpart to the edge-triggered
// reactive autoclose (molecule_autoclose.go): the reactive path only re-checks
// a root when a child-close event names it, and the closed-root purge only
// DELETES already-closed roots — so an open root whose descendants all went
// terminal without a final child-close event (or whose final event was lost)
// stays open forever and fuels the wisp backlog. This sweep is the only path
// that CLOSES such abandoned roots.
//
// Guards (each is load-bearing — see BUG 4):
//  1. TTL: skip roots with activity newer than now-wispGCCloseAbandonedTTL so
//     live/in-flight roots and the external operational reconciler are never
//     raced.
//  2. descendants > 0: never close a stepless root — that would race the
//     instantiator (mirrors autocloseMoleculeIfComplete).
//  3. ZFC-exempt: skip roots carrying the gc.gc_exempt marker (the ZFC-exempt
//     compactor-loop root must NOT be auto-closed).
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
		if !sourceworkflow.IsWorkflowRoot(root) {
			continue
		}
		if convoycore.IsTerminalStatus(root.Status) {
			// Defensive: the query already filters to open, but a status the
			// store reports as terminal is not abandoned.
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
		// Guard 2: never close a stepless root — that races the instantiator.
		if descendants == 0 {
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

// isGCExempt reports whether a root is marked exempt from garbage-collection
// auto-close via the gc.gc_exempt metadata marker. The known ZFC-exempt
// compactor-loop root carries this marker and must never be auto-closed.
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

// openWispGCRootCandidates mirrors closedWispGCEntries but lists OPEN
// molecule roots and OPEN wisp-kinded roots, the universe the abandoned-root
// sweep considers. Caller applies the IsWorkflowRoot / TTL / terminal / exempt
// filters.
func openWispGCRootCandidates(store beads.Store) ([]beads.Bead, error) {
	entries := make([]beads.Bead, 0)
	seen := make(map[string]struct{})
	appendUnique := func(items []beads.Bead) {
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
	molecules, err := store.List(beads.ListQuery{Status: "open", Type: "molecule", TierMode: beads.TierBoth})
	if err != nil {
		return nil, fmt.Errorf("listing open molecule roots: %w", err)
	}
	appendUnique(molecules)
	wisps, err := store.List(beads.ListQuery{Status: "open", Metadata: map[string]string{beadmeta.KindMetadataKey: "wisp"}, TierMode: beads.TierBoth})
	if err != nil {
		return nil, fmt.Errorf("listing open wisp roots: %w", err)
	}
	appendUnique(wisps)
	graphRoots, err := store.List(beads.ListQuery{Status: "open", Metadata: map[string]string{beadmeta.FormulaContractMetadataKey: "graph.v2"}, TierMode: beads.TierBoth})
	if err != nil {
		return nil, fmt.Errorf("listing open graph.v2 roots: %w", err)
	}
	appendUnique(graphRoots)
	// IsWorkflowRoot also accepts the legacy gc.kind=workflow label, so include
	// those roots even when they carry neither the wisp kind nor the graph.v2
	// contract (the caller still re-checks IsWorkflowRoot).
	workflowRoots, err := store.List(beads.ListQuery{Status: "open", Metadata: map[string]string{beadmeta.KindMetadataKey: "workflow"}, TierMode: beads.TierBoth})
	if err != nil {
		return nil, fmt.Errorf("listing open workflow roots: %w", err)
	}
	appendUnique(workflowRoots)
	return entries, nil
}

func readMessageWispGCEntries(store beads.Store) ([]beads.Bead, error) {
	entries, err := store.List(beads.ListQuery{
		Type:          "message",
		Metadata:      map[string]string{mailReadMetadataKey: "true"},
		IncludeClosed: true,
		TierMode:      beads.TierWisps,
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func purgeExpiredBeadClosures(store beads.Store, entries []beads.Bead, cutoff time.Time) (int, error) {
	return purgeExpiredBeads(store, entries, cutoff, deleteExpiredBeadClosure)
}

func purgeExpiredBeadRoots(store beads.Store, entries []beads.Bead, cutoff time.Time) (int, error) {
	return purgeExpiredBeads(store, entries, cutoff, deleteWorkflowBead)
}

func purgeExpiredBeads(store beads.Store, entries []beads.Bead, cutoff time.Time, deleteFn func(beads.Store, string) error) (int, error) {
	purged := 0
	var deleteErr error
	for _, entry := range entries {
		if entry.CreatedAt.IsZero() || !entry.CreatedAt.Before(cutoff) {
			continue
		}
		if err := deleteFn(store, entry.ID); err != nil {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("deleting expired bead %q: %w", entry.ID, err))
			continue
		}
		purged++
	}
	return purged, deleteErr
}

func deleteExpiredBeadClosure(store beads.Store, rootID string) error {
	// deleteWorkflowBead removes every dependency attached to each closure
	// member before deleting the bead. Only use the closure deleter for roots
	// whose full ownership tree is safe to collect.
	ids, err := collectExpiredBeadClosure(store, rootID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := deleteWorkflowBead(store, id); err != nil {
			return err
		}
	}
	return nil
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
