package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// leaseRenewalSweepBudget bounds the wall-clock time one renewal sweep may
// spend on the reconciler tick. Each renewal costs a bd subprocess, so an
// unbounded sweep over a city with many concurrent claims would block the rest
// of the tick and eat the very margin the renewal exists to protect. A sweep
// that runs out of budget stops and resumes from its cursor on the next tick,
// so bounding the work starves nobody.
const leaseRenewalSweepBudget = 2 * time.Second

// leaseRenewalRetryDivisor sets the first retry delay after a failed sweep as a
// fraction of the normal cadence. Retries double from there, capped at the
// cadence, so a transient bd failure costs a small slice of the lease margin
// while a persistently broken backend settles to the normal rate rather than
// being retried on every tick.
const leaseRenewalRetryDivisor = 8

// leaseRenewalInterval derives the lease-renewal cadence from the configured
// claim-lease TTL: renew at one third of the TTL, so a renewal can miss twice
// (a slow tick, a transient bd error) before the lease lapses. Deriving the
// cadence from the TTL is the gas-76r invariant — the two values cannot drift
// apart the way the prompt-driven ~16-22 minute cadence drifted against bd's
// 5-minute window. A zero or negative TTL degenerates to renewing on every
// pass: with no meaningful window to pace against, renewing too often is safe
// and renewing never is the bug this exists to fix.
func leaseRenewalInterval(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 0
	}
	return ttl / 3
}

// leaseRenewalRetryDelay returns how long to wait before retrying a sweep that
// failed, given the count of consecutive failed sweeps and the normal cadence.
// It starts at cadence/leaseRenewalRetryDivisor and doubles per failure, capped
// at the cadence itself so a failing store is never retried LESS often than a
// healthy one. Pacing retries off the last SUCCESS rather than the last attempt
// is what keeps failures from spending a full cadence of lease margin each.
func leaseRenewalRetryDelay(failures int, interval time.Duration) time.Duration {
	if failures <= 0 || interval <= 0 {
		return 0
	}
	delay := interval / leaseRenewalRetryDivisor
	if delay <= 0 {
		return 0
	}
	for i := 1; i < failures && delay < interval; i++ {
		delay *= 2
	}
	if delay > interval {
		return interval
	}
	return delay
}

// leaseSweepTarget names a bead store for lease-renewal sweep logging.
type leaseSweepTarget struct {
	label string
	store beads.Store
}

// leaseSweepConfig is one renewal sweep's inputs. now and budget are injected
// so the bound is testable without sleeping; a nil now defaults to time.Now and
// a non-positive budget means unbounded.
type leaseSweepConfig struct {
	targets []leaseSweepTarget
	running map[string]bool
	cursor  string
	budget  time.Duration
	now     func() time.Time
	// requireCapable turns a store that cannot renew leases from a silent skip
	// into a reported failure (beads.lease_renewal = require).
	requireCapable bool
	stderr         io.Writer
	logPrefix      string
}

// leaseSweepResult reports one sweep's outcome. cursor is the position to
// resume from next tick; truncated reports that the budget stopped the sweep
// before it reached every candidate, which the watchdog treats as "come back
// promptly" rather than as a failure.
type leaseSweepResult struct {
	renewed   int
	failed    int
	truncated bool
	cursor    string
}

// leaseRenewalCandidate is one bead due for renewal, keyed for a stable
// round-robin order across every target store.
type leaseRenewalCandidate struct {
	key    string
	label  string
	store  beads.Store
	id     string
	holder string
}

// collectLeaseRenewalCandidates lists every in_progress bead whose assignee is
// a running session, across all lease-capable targets, in a stable total order.
// Stores without lease semantics are skipped silently: capability absence is
// not a failure and must not spam the controller log every pass.
func collectLeaseRenewalCandidates(cfg leaseSweepConfig, res *leaseSweepResult) []leaseRenewalCandidate {
	var candidates []leaseRenewalCandidate
	for _, target := range cfg.targets {
		if _, ok := target.store.(beads.LeaseRenewer); !ok {
			if cfg.requireCapable {
				fmt.Fprintf(cfg.stderr, "%s: lease renewal: %s: store cannot renew claim leases and beads.lease_renewal=require\n", cfg.logPrefix, target.label) //nolint:errcheck // best-effort stderr
				res.failed++
			}
			continue
		}
		// Issues tier only. Renewal is deliberately NOT tier-complete here:
		// bd's heartbeat verb does not route to the wisp table until the
		// ownership-fencing epic's Stage B lands tier-complete lease ops
		// (engdocs/plans/ownership-fencing DESIGN.md §2.5), so sweeping wisps
		// today would produce failures rather than renewals. Wisp-tier claims
		// keep the pre-Stage-B behavior until that lands.
		held, err := target.store.ListOpen("in_progress")
		if err != nil {
			fmt.Fprintf(cfg.stderr, "%s: lease renewal: %s: listing in_progress beads: %v\n", cfg.logPrefix, target.label, err) //nolint:errcheck // best-effort stderr
			res.failed++
			continue
		}
		for _, b := range held {
			assignee := strings.TrimSpace(b.Assignee)
			if assignee == "" || !cfg.running[assignee] {
				continue
			}
			candidates = append(candidates, leaseRenewalCandidate{
				key:    target.label + "\x00" + b.ID,
				label:  target.label,
				store:  target.store,
				id:     b.ID,
				holder: assignee,
			})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].key < candidates[j].key })
	return candidates
}

// leaseRenewalStartIndex returns where in the ordered candidate list to resume
// after cursor. It selects the first key strictly greater than the cursor, so a
// bead that closed since the last sweep does not send the sweep back to the
// start and re-renew the same prefix forever.
func leaseRenewalStartIndex(candidates []leaseRenewalCandidate, cursor string) int {
	if cursor == "" {
		return 0
	}
	idx := sort.Search(len(candidates), func(i int) bool { return candidates[i].key > cursor })
	if idx >= len(candidates) {
		return 0
	}
	return idx
}

// sweepClaimLeaseRenewals renews the claim lease of every in_progress bead
// whose assignee is a running session, across the given stores, bounded by the
// configured wall-clock budget and resuming from the previous sweep's cursor.
// A renewal that fails for one bead is logged and does not stop the sweep.
// Beads whose holder is NOT running are deliberately left alone: their lease
// going stale is what lets bd reclaim return a dead worker's bead to ready.
func sweepClaimLeaseRenewals(cfg leaseSweepConfig) leaseSweepResult {
	res := leaseSweepResult{cursor: cfg.cursor}
	now := cfg.now
	if now == nil {
		now = time.Now
	}

	candidates := collectLeaseRenewalCandidates(cfg, &res)
	if len(candidates) == 0 {
		res.cursor = ""
		return res
	}

	started := now()
	start := leaseRenewalStartIndex(candidates, cfg.cursor)
	unsupported := map[string]bool{}
	attempted := 0

	for i := 0; i < len(candidates); i++ {
		c := candidates[(start+i)%len(candidates)]
		if unsupported[c.label] {
			continue
		}
		// Always make one attempt so the sweep cannot stall entirely, then
		// honor the budget.
		if attempted > 0 && cfg.budget > 0 && now().Sub(started) >= cfg.budget {
			res.truncated = true
			fmt.Fprintf(cfg.stderr, "%s: lease renewal: stopped after %d of %d beads at the %s sweep budget; resuming next tick\n", //nolint:errcheck // best-effort stderr
				cfg.logPrefix, attempted, len(candidates), cfg.budget)
			break
		}

		renewer, ok := c.store.(beads.LeaseRenewer)
		if !ok {
			continue
		}
		attempted++
		res.cursor = c.key
		if err := renewer.RenewLease(c.id, c.holder); err != nil {
			if errors.Is(err, beads.ErrLeaseRenewalUnsupported) {
				// A lease-capable wrapper over a lease-less backing store
				// (e.g. CachingStore over the native store): same as the store
				// not implementing LeaseRenewer at all.
				unsupported[c.label] = true
				if cfg.requireCapable {
					fmt.Fprintf(cfg.stderr, "%s: lease renewal: %s: backing store cannot renew claim leases and beads.lease_renewal=require\n", cfg.logPrefix, c.label) //nolint:errcheck // best-effort stderr
					res.failed++
				}
				continue
			}
			fmt.Fprintf(cfg.stderr, "%s: lease renewal: %s: %s: %v\n", cfg.logPrefix, c.label, c.id, err) //nolint:errcheck // best-effort stderr
			res.failed++
			continue
		}
		res.renewed++
	}
	return res
}

// leaseRenewalEnabled reports whether the claim-lease renewal watchdog may run,
// reading the boot-latched beads.lease_renewal gate. Off is the kill switch.
//
// An unresolved gate (no controller state, or a Flags nobody resolved) reads
// ModeUnset. Every other gate maps ModeUnset to its legacy path; this one maps
// it to the built-in default instead, because for THIS gate the legacy path IS
// the defect: a controller that silently stopped renewing leases is precisely
// the gas-76r bug. Renewal is safe in the other direction by construction — it
// only extends the lease of a session the reconciler currently sees running,
// which is the state a claim lease is supposed to describe.
func (cr *CityRuntime) leaseRenewalEnabled() bool {
	if cr.cs == nil {
		return true
	}
	return cr.cs.rolloutFlags.BeadsLeaseRenewal() != rollout.Off
}

// leaseRenewalRequiresCapableStore reports whether a store that cannot renew
// leases is an error rather than a silent skip (beads.lease_renewal = require).
func (cr *CityRuntime) leaseRenewalRequiresCapableStore() bool {
	return cr.cs != nil && cr.cs.rolloutFlags.BeadsLeaseRenewal() == rollout.Require
}

// leaseRenewalDue reports whether a renewal sweep should run now. It paces off
// the last COMPLETE sweep rather than the last attempt, so a failure does not
// spend a full cadence of lease margin, and applies a backoff floor after
// consecutive failures so a persistently broken backend is not retried on every
// tick (gas-76r review item 1).
func (cr *CityRuntime) leaseRenewalDue(now time.Time, interval time.Duration) bool {
	if !cr.leaseRenewalLastSuccess.IsZero() && now.Sub(cr.leaseRenewalLastSuccess) < interval {
		return false
	}
	if cr.leaseRenewalFailures > 0 && !cr.leaseRenewalLastAttempt.IsZero() {
		if now.Sub(cr.leaseRenewalLastAttempt) < leaseRenewalRetryDelay(cr.leaseRenewalFailures, interval) {
			return false
		}
	}
	return true
}

// runLeaseRenewalWatchdog keeps the claim leases of beads held by live sessions
// continuously valid (gas-76r). It runs from the reconciler tick but self-gates
// to the cadence derived from the configured claim-lease TTL, so heartbeat
// writes stay a small fraction of the TTL regardless of tick rate. Only
// sessions with a live runtime (StateActive) count as holders; renewals stop
// the moment the reconciler stops seeing the session alive, which is exactly
// when the lease SHOULD lapse so bd reclaim can recover the bead.
func (cr *CityRuntime) runLeaseRenewalWatchdog(now time.Time, sessionBeads *sessionBeadSnapshot) int {
	if !cr.leaseRenewalEnabled() {
		return 0
	}
	interval := leaseRenewalInterval(cr.cfg.Daemon.ClaimLeaseTTLDuration())
	if !cr.leaseRenewalDue(now, interval) {
		return 0
	}
	cr.leaseRenewalLastAttempt = now

	// A nil snapshot is a FAILED sessions-class read — loadSessionBeadSnapshot
	// returns nil both when the store is unavailable and when the query errors
	// (city_runtime.go) — not an empty fleet. Stamping it as a complete sweep
	// would spend a full cadence of lease margin on a transient failure; route
	// it through the same backoff a failed renewal uses.
	if sessionBeads == nil {
		cr.leaseRenewalFailures++
		return 0
	}

	running := map[string]bool{}
	for _, info := range sessionBeads.OpenInfos() {
		if info.State != sessionpkg.StateActive {
			continue
		}
		// Match holders on the confined assignee vocabulary, not the derived
		// runtime SessionName: claims are stamped alias-first and
		// Info.SessionName falls back to sessionNameFor(ID), which
		// internal/session/assignee_identities.go deliberately excludes.
		for _, id := range sessionBeadAssigneeIdentitiesInfo(info) {
			if id = strings.TrimSpace(id); id != "" {
				running[id] = true
			}
		}
	}
	if len(running) == 0 {
		// Nothing to renew is a complete sweep, not a failure.
		cr.leaseRenewalLastSuccess = now
		cr.leaseRenewalFailures = 0
		return 0
	}

	var targets []leaseSweepTarget
	if store := cr.cityBeadStore(); store != nil {
		targets = append(targets, leaseSweepTarget{label: "city", store: store})
	}
	rigStores := cr.rigBeadStores()
	names := make([]string, 0, len(rigStores))
	for name := range rigStores {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if rigStores[name] == nil {
			continue
		}
		targets = append(targets, leaseSweepTarget{label: name, store: rigStores[name]})
	}

	res := sweepClaimLeaseRenewals(leaseSweepConfig{
		targets:        targets,
		running:        running,
		cursor:         cr.leaseRenewalCursor,
		budget:         leaseRenewalSweepBudget,
		requireCapable: cr.leaseRenewalRequiresCapableStore(),
		stderr:         cr.stderr,
		logPrefix:      cr.logPrefix,
	})
	cr.leaseRenewalCursor = res.cursor

	switch {
	case res.failed > 0:
		cr.leaseRenewalFailures++
	case res.truncated:
		// Budget-bounded, not broken: resume promptly from the cursor without
		// backing off, and do not mark the cadence satisfied.
		cr.leaseRenewalFailures = 0
	default:
		cr.leaseRenewalLastSuccess = now
		cr.leaseRenewalFailures = 0
	}
	return res.renewed
}
