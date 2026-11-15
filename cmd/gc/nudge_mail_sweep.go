package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

const (
	nudgeMailSweepDefaultNudgeTTL = 10 * time.Minute
	nudgeMailSweepDefaultMailTTL  = 60 * time.Minute

	// nudgeMailSweepDefaultUnreadMailTTL is the retention window for message
	// beads that have never been marked "read". 0 = disabled: the unread-mail
	// phase is opt-in via [mail] unread_retention_ttl or --unread-mail-ttl
	// (gastownhall/gascity#5240).
	nudgeMailSweepDefaultUnreadMailTTL time.Duration = 0

	nudgeMailSweepCloseBudget         = 50
	nudgeMailSweepWatchdogInterval    = 5 * time.Minute
	nudgeMailSweepWatchdogCloseBudget = 500

	// nudgeMailSweepNudgeCloseReason is the close_reason stamped on stale nudge
	// beads before close. The 20-character floor satisfies validation.on-close=error.
	nudgeMailSweepNudgeCloseReason = "nudge gc-swept: stale nudge bead past gc retention window"

	// nudgeMailSweepMailCloseReason is the close_reason stamped on read mail
	// beads before close. It is beadmail.RetentionSweepCloseReason so beadmail's
	// direct-ID gate recognizes these beads as retention-swept (system-aged,
	// still addressable until purge) rather than user-removed.
	nudgeMailSweepMailCloseReason = beadmail.RetentionSweepCloseReason

	// nudgeMailSweepUnreadMailCloseReason is the close_reason stamped on stale
	// unread mail beads before close. It is
	// beadmail.UnreadRetentionSweepCloseReason, the unread counterpart of
	// nudgeMailSweepMailCloseReason, recognized by the same direct-ID gate.
	nudgeMailSweepUnreadMailCloseReason = beadmail.UnreadRetentionSweepCloseReason
)

// nudgeMailSweepResult holds per-category close counts from sweepStaleNudgeMail.
type nudgeMailSweepResult struct {
	NudgeClosed int
	MailClosed  int
	// ScopeNudgeClosed lists the nudge closes made in non-city stores, in sweep
	// order, one entry per store that closed anything. NudgeClosed already
	// includes them.
	ScopeNudgeClosed []nudgeSweepScopeClosed
}

// nudgeSweepScope is one extra store, beyond the nudges-class store, whose stale
// gc:nudge shadows a sweep closes: a rig's work store.
type nudgeSweepScope struct {
	Label string
	Store beads.NudgesStore
}

// nudgeSweepScopeClosed is the number of nudge shadows a sweep closed (or, in a
// dry run, would close) in the extra store labeled Label.
type nudgeSweepScopeClosed struct {
	Label  string
	Closed int
}

// nudgeMailSweepMailTTLForConfig resolves the mail-close TTL for the
// controller watchdog and the sweep-nudge-mail order command from
// cfg.Mail.RetentionTTL. An empty (unset) value preserves
// nudgeMailSweepDefaultMailTTL, so a city that has never touched [mail] sees
// no behavior change. An explicitly configured value is honored as-is,
// including "0" — which sweepStaleNudgeMail treats as "skip the mail-close
// phase" rather than a zero-length window. A malformed value falls back to
// the default (with a stderr note) rather than silently disabling the sweep.
func nudgeMailSweepMailTTLForConfig(cfg *config.City, stderr io.Writer) time.Duration {
	if cfg == nil || strings.TrimSpace(cfg.Mail.RetentionTTL) == "" {
		return nudgeMailSweepDefaultMailTTL
	}
	d, err := cfg.Mail.RetentionTTLDuration()
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "nudge-mail-sweep: %v; using default %s\n", err, nudgeMailSweepDefaultMailTTL) //nolint:errcheck // best-effort stderr
		}
		return nudgeMailSweepDefaultMailTTL
	}
	return d
}

// nudgeMailSweepMailTTLForCity resolves the mail-close TTL for the
// sweep-nudge-mail order command by reading cityPath's city.toml, returning
// fallback when that config cannot be loaded. A city with no city.toml is a
// legitimate no-config case and stays silent; a config that exists and will not
// parse gets a stderr note, because it would otherwise leave a configured
// retention_ttl looking applied when it is not. It warns and falls back rather
// than failing: this runs as a 5-minute order, and failing it closed would stop
// the sweep entirely for a condition the loader only warns about.
func nudgeMailSweepMailTTLForCity(cityPath string, fallback time.Duration, stderr io.Writer) time.Duration {
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		if stderr != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "nudge-mail-sweep: %v; using default %s\n", err, fallback) //nolint:errcheck // best-effort stderr
		}
		return fallback
	}
	return nudgeMailSweepMailTTLForConfig(cfg, stderr)
}

// nudgeMailSweepUnreadMailTTLForConfig resolves the unread-mail-close TTL for
// the controller watchdog and the sweep-nudge-mail order command from
// cfg.Mail.UnreadRetentionTTL. The unread-mail phase is opt-in: an empty
// (unset) value or "0" returns 0, which sweepStaleNudgeMail treats as "skip
// the unread-mail phase". A malformed value also returns 0 (with a stderr
// note) so a typo never closes unread mail on an unintended window.
func nudgeMailSweepUnreadMailTTLForConfig(cfg *config.City, stderr io.Writer) time.Duration {
	if cfg == nil || strings.TrimSpace(cfg.Mail.UnreadRetentionTTL) == "" {
		return nudgeMailSweepDefaultUnreadMailTTL
	}
	d, err := cfg.Mail.UnreadRetentionTTLDuration()
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "nudge-mail-sweep: %v; unread-mail phase disabled\n", err) //nolint:errcheck // best-effort stderr
		}
		return nudgeMailSweepDefaultUnreadMailTTL
	}
	return d
}

// nudgeMailSweepUnreadMailTTLForCity resolves the unread-mail-close TTL for
// the sweep-nudge-mail order command by reading cityPath's city.toml,
// returning fallback when that config cannot be loaded. Like
// nudgeMailSweepMailTTLForCity, a missing city.toml stays silent while a
// config that exists and will not parse gets a stderr note.
func nudgeMailSweepUnreadMailTTLForCity(cityPath string, fallback time.Duration, stderr io.Writer) time.Duration {
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		if stderr != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "nudge-mail-sweep: %v; using default unread-mail TTL %s\n", err, fallback) //nolint:errcheck // best-effort stderr
		}
		return fallback
	}
	return nudgeMailSweepUnreadMailTTLForConfig(cfg, stderr)
}

// sweepStaleNudgeMail closes stale consumed nudge beads and read mail beads.
//
// Nudge candidates are open beads with label gc:nudge created before now-nudgeTTL
// whose nudge_id is not present in nudgeState.Pending or nudgeState.InFlight.
// Terminal metadata is stamped via nudgequeue.Store.SweepStale before each close
// so the bead audit trail is intact.
//
// Mail candidates are open message beads with label "read" created before now-mailTTL.
// mailTTL <= 0 skips the mail-close phase entirely (rather than closing every
// read mail bead immediately).
//
// Unread mail candidates are open message beads WITHOUT label "read" created
// before now-unreadMailTTL. Without this phase, mail nobody has read never
// matches the read-mail candidate query and accumulates forever regardless of
// age (gastownhall/gascity#5240). unreadMailTTL <= 0 skips the unread-mail
// phase entirely; the phase is opt-in, and when enabled unreadMailTTL is
// normally much longer than mailTTL.
//
// limit caps total closes (nudge + mail + unread mail combined). Pass 0 for no cap.
// Per-bead errors do not abort the sweep; they are returned via errors.Join so
// the caller can report them without treating the sweep as fatal.
//
// The nudge phase is sourced from the strongly-typed nudgeStore (the nudges
// class); the mail and unread-mail phases from the strongly-typed mailStore
// (the messaging class). Both wrap the same underlying work store until
// either class relocates, so behavior is unchanged today.
//
// scopes are extra stores, in the order given (a rig's work store each), whose
// stale gc:nudge shadows the nudge phase also closes after the nudgeStore's own:
// a rig-born shadow lives in the rig's store, where a city-only sweep never
// looks. The live-ID exclusion applies to every store, and limit is one budget
// across all of them, the nudgeStore first. A scope whose store is the nudgeStore
// or an earlier scope is skipped, and a scope that cannot be listed adds an error
// to the returned join and does not stop the others. The mail phases stay on
// mailStore alone.
func sweepStaleNudgeMail(nudgeStore beads.NudgesStore, mailStore beads.MailStore, nudgeState *nudgequeue.State, now time.Time, nudgeTTL, mailTTL, unreadMailTTL time.Duration, limit int, scopes ...nudgeSweepScope) (nudgeMailSweepResult, error) {
	var result nudgeMailSweepResult
	var beadErrs []error

	liveIDs := liveNudgeIDSet(nudgeState)
	nq := nudgequeue.NewStore(nudgeStore)

	// Phase 1: close stale nudge beads. The live flock-queue exclusion is carried
	// inside StaleShadowsBefore; the cross-phase close budget stays in this loop.
	nudgeCutoff := now.Add(-nudgeTTL)
	// nudge/mail beads are NoHistory (wisp-tier); StaleShadowsBefore reads both tiers.
	nudgeShadows, err := nq.StaleShadowsBefore(nudgeCutoff, limit, liveIDs)
	if err != nil {
		return result, fmt.Errorf("nudge-mail-sweep: listing stale nudge beads: %w", err)
	}

	for _, shadow := range nudgeShadows {
		if limit > 0 && result.NudgeClosed+result.MailClosed >= limit {
			break
		}
		if !shadow.Open {
			continue
		}
		if err := nq.SweepStale(shadow.BeadID, nudgeMailSweepNudgeCloseReason, now); err != nil {
			beadErrs = append(beadErrs, err)
			continue
		}
		result.NudgeClosed++
	}
	beadErrs = append(beadErrs, sweepScopeNudgeShadows(&result, nudgeStore, scopes, liveIDs, nudgeCutoff, now, limit, false)...)

	// Phase 2: close read mail beads. The candidate query + close-with-reason
	// loop live inside the messaging edge (beadmail); only the shared close
	// budget is passed in. mailBudget is the remaining share of the combined
	// limit, so a fatal listing failure early-returns (discarding accumulated
	// per-bead errors) exactly as the inline loop did.
	if mailTTL > 0 {
		mailCutoff := now.Add(-mailTTL)
		remaining := limit - result.NudgeClosed - result.MailClosed
		if limit == 0 || remaining > 0 {
			mailBudget := remaining
			if limit == 0 {
				mailBudget = 0
			}
			mailClosed, mailCloseErrs, mailListErr := beadmail.SweepReadMessagesBefore(mailStore, mailCutoff, mailBudget, nudgeMailSweepMailCloseReason)
			if mailListErr != nil {
				return result, fmt.Errorf("nudge-mail-sweep: listing read mail beads: %w", mailListErr)
			}
			result.MailClosed += mailClosed
			beadErrs = append(beadErrs, mailCloseErrs...)
		}
	}

	// Phase 3: close stale unread mail beads. Same shared-budget shape as
	// Phase 2, on the unreadMailTTL cutoff. Opt-in: unreadMailTTL <= 0 skips it.
	if unreadMailTTL > 0 {
		unreadMailCutoff := now.Add(-unreadMailTTL)
		remaining := limit - result.NudgeClosed - result.MailClosed
		if limit == 0 || remaining > 0 {
			unreadMailBudget := remaining
			if limit == 0 {
				unreadMailBudget = 0
			}
			unreadClosed, unreadCloseErrs, unreadListErr := beadmail.SweepUnreadMessagesBefore(mailStore, unreadMailCutoff, unreadMailBudget, nudgeMailSweepUnreadMailCloseReason)
			if unreadListErr != nil {
				return result, fmt.Errorf("nudge-mail-sweep: listing unread mail beads: %w", unreadListErr)
			}
			result.MailClosed += unreadClosed
			beadErrs = append(beadErrs, unreadCloseErrs...)
		}
	}

	return result, errors.Join(beadErrs...)
}

// countStaleNudgeMail returns what sweepStaleNudgeMail would close without
// making any changes. Used by --dry-run to report candidate count without side
// effects. The limit parameter caps the count the same way sweepStaleNudgeMail
// caps closes; pass 0 for no cap. mailTTL <= 0 and unreadMailTTL <= 0 skip
// their phases exactly as in the sweep. The nudge phase is counted from the typed
// nudgeStore (nudges class); the mail and unread-mail phases from the typed
// mailStore (messaging class). scopes are enumerated exactly as the sweep does,
// so a dry run counts the same stores the run would close in.
func countStaleNudgeMail(nudgeStore beads.NudgesStore, mailStore beads.MailStore, nudgeState *nudgequeue.State, now time.Time, nudgeTTL, mailTTL, unreadMailTTL time.Duration, limit int, scopes ...nudgeSweepScope) (nudgeMailSweepResult, error) {
	var result nudgeMailSweepResult
	var scopeErrs []error

	liveIDs := liveNudgeIDSet(nudgeState)
	nq := nudgequeue.NewStore(nudgeStore)

	// Dry-run twin of the sweep: same typed read, same cross-phase budget, no writes.
	nudgeCutoff := now.Add(-nudgeTTL)
	nudgeShadows, err := nq.StaleShadowsBefore(nudgeCutoff, limit, liveIDs)
	if err != nil {
		return result, fmt.Errorf("nudge-mail-sweep (dry-run): listing stale nudge beads: %w", err)
	}
	for _, shadow := range nudgeShadows {
		if limit > 0 && result.NudgeClosed+result.MailClosed >= limit {
			break
		}
		if !shadow.Open {
			continue
		}
		result.NudgeClosed++
	}
	scopeErrs = append(scopeErrs, sweepScopeNudgeShadows(&result, nudgeStore, scopes, liveIDs, nudgeCutoff, now, limit, true)...)

	if mailTTL > 0 {
		mailCutoff := now.Add(-mailTTL)
		remaining := limit - result.NudgeClosed - result.MailClosed
		if limit == 0 || remaining > 0 {
			mailBudget := remaining
			if limit == 0 {
				mailBudget = 0
			}
			mailCount, err := beadmail.CountReadMessagesBefore(mailStore, mailCutoff, mailBudget)
			if err != nil {
				return result, fmt.Errorf("nudge-mail-sweep (dry-run): listing read mail beads: %w", err)
			}
			result.MailClosed += mailCount
		}
	}

	if unreadMailTTL > 0 {
		unreadMailCutoff := now.Add(-unreadMailTTL)
		remaining := limit - result.NudgeClosed - result.MailClosed
		if limit == 0 || remaining > 0 {
			unreadMailBudget := remaining
			if limit == 0 {
				unreadMailBudget = 0
			}
			unreadCount, err := beadmail.CountUnreadMessagesBefore(mailStore, unreadMailCutoff, unreadMailBudget)
			if err != nil {
				return result, fmt.Errorf("nudge-mail-sweep (dry-run): listing unread mail beads: %w", err)
			}
			result.MailClosed += unreadCount
		}
	}
	return result, errors.Join(scopeErrs...)
}

// sweepScopeNudgeShadows is the nudge phase over the extra stores: it closes
// (dryRun false) or counts (dryRun true) the stale gc:nudge shadows in each scope,
// in order, and adds them to result.NudgeClosed and result.ScopeNudgeClosed.
// result.NudgeClosed on entry is what the nudgeStore already used of the shared
// limit (0 means no cap). The nudgeStore itself is passed only so a scope that is
// that store, or an earlier scope, is skipped rather than read twice.
//
// A scope that cannot be listed, or a bead that cannot be closed, is returned as
// an error and the sweep moves on, so one unreadable rig never hides the rest.
func sweepScopeNudgeShadows(result *nudgeMailSweepResult, nudgeStore beads.NudgesStore, scopes []nudgeSweepScope, liveIDs map[string]bool, cutoff, now time.Time, limit int, dryRun bool) []error {
	var errs []error
	seen := []beads.NudgesStore{nudgeStore}
	for _, scope := range scopes {
		if scope.Store == (beads.NudgesStore{}) || nudgeSweepStoreSeen(seen, scope.Store) {
			continue
		}
		seen = append(seen, scope.Store)
		budget := 0
		if limit > 0 {
			if budget = limit - result.NudgeClosed - result.MailClosed; budget <= 0 {
				break
			}
		}
		nq := nudgequeue.NewStore(scope.Store)
		shadows, err := nq.StaleShadowsBefore(cutoff, budget, liveIDs)
		if err != nil {
			prefix := "nudge-mail-sweep"
			if dryRun {
				prefix += " (dry-run)"
			}
			errs = append(errs, fmt.Errorf("%s: listing stale nudge beads in %s: %w", prefix, scope.Label, err))
			continue
		}
		closed := 0
		for _, shadow := range shadows {
			if limit > 0 && result.NudgeClosed+result.MailClosed >= limit {
				break
			}
			if !shadow.Open {
				continue
			}
			if !dryRun {
				if err := nq.SweepStale(shadow.BeadID, nudgeMailSweepNudgeCloseReason, now); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", scope.Label, err))
					continue
				}
			}
			closed++
			result.NudgeClosed++
		}
		if closed > 0 {
			result.ScopeNudgeClosed = append(result.ScopeNudgeClosed, nudgeSweepScopeClosed{Label: scope.Label, Closed: closed})
		}
	}
	return errs
}

// nudgeSweepStoreSeen reports whether handle reads a store that one of seen
// already reads. beads.SameStore compares the physical stores under the class
// handles, and a store it cannot compare reads as unseen, so it is swept rather
// than skipped.
func nudgeSweepStoreSeen(seen []beads.NudgesStore, handle beads.NudgesStore) bool {
	for _, s := range seen {
		if beads.SameStore(s, handle) {
			return true
		}
	}
	return false
}

// liveNudgeIDSet returns the set of nudge IDs whose shadow the stale-shadow
// sweep must leave alone: the items pending or in flight, and the retired items
// and dead letters whose shadow debt is unpaid (their payer will write the
// terminal record). Returns nil (no live IDs) when nudgeState is nil.
func liveNudgeIDSet(state *nudgequeue.State) map[string]bool {
	if state == nil {
		return nil
	}
	live := make(map[string]bool, len(state.Pending)+len(state.InFlight)+len(state.Retired))
	for _, item := range state.Pending {
		live[item.ID] = true
	}
	for _, item := range state.InFlight {
		live[item.ID] = true
	}
	for _, bucket := range [][]nudgequeue.Item{state.Dead, state.Retired} {
		for _, item := range bucket {
			if item.Shadow != nil && !item.Shadow.Paid {
				live[item.ID] = true
			}
		}
	}
	return live
}
