package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// Thresholds of the ledger feed's advisories.
const (
	// ledgerFeedDoctorLagRows is the lag, in journal rows, past which a scope
	// is reported behind.
	ledgerFeedDoctorLagRows = 5000
	// ledgerFeedDoctorStuckAfter is how long a scope may stay inactive,
	// unavailable or retrying before it is reported stuck.
	ledgerFeedDoctorStuckAfter = time.Minute
	// ledgerFeedDoctorWakesPerMinute is the wake rate past which a scope is
	// reported as a possible echo loop.
	ledgerFeedDoctorWakesPerMinute = 120
	// ledgerFeedDoctorTopActors is how many journal actors a scope's detail
	// line lists.
	ledgerFeedDoctorTopActors = 3
)

// ledgerFeedDoctorCheck reports the controller's journal feeds ([beads]
// ledger_feed). It is advisory only and never fixable: the operator decides
// what to change. Every scope it lists stays on the re-scan, which covers
// every create and update but, listing active rows only, not the close of a
// row the cache never held: such a close inside a gap, or one the feed never
// reads, may go unannounced (ledgerFeed.onGap). The caches over a non-work
// storage binding (storageRoutes.withControllerCache) and the one rigs share
// over the legacy file store get no feed by design, so they stay on the
// re-scan and are not listed.
type ledgerFeedDoctorCheck struct {
	cfg   *config.City
	query func(cityPath string) (ledgerFeedStatus, error)
}

func newLedgerFeedDoctorCheck(cfg *config.City, query func(cityPath string) (ledgerFeedStatus, error)) *ledgerFeedDoctorCheck {
	return &ledgerFeedDoctorCheck{cfg: cfg, query: query}
}

// Name implements doctor.Check.
func (*ledgerFeedDoctorCheck) Name() string { return "beads-ledger-feed" }

// CanFix implements doctor.Check.
func (*ledgerFeedDoctorCheck) CanFix() bool { return false }

// WarmupEligible implements doctor.Check: the feed's state is the running
// controller's, which a starting city does not have yet.
func (*ledgerFeedDoctorCheck) WarmupEligible() bool { return false }

// Fix implements doctor.Check.
func (*ledgerFeedDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

// Run implements doctor.Check: OK when the feed is off or its state cannot be
// read, an advisory warning for a mode change pending reload, a feed still
// running while the mode is off, a stopped feed, writes the feed missed, a
// feed far behind or stuck, or a wake rate that looks like an echo loop. Each
// scope's figures are listed as details.
func (c *ledgerFeedDoctorCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK}
	onDisk := config.BeadsLedgerFeedOff
	if c.cfg != nil {
		onDisk = c.cfg.Beads.NormalizedLedgerFeed()
	}
	var st ledgerFeedStatus
	var err error
	if ctx != nil && ctx.CityPath != "" && c.query != nil {
		st, err = c.query(ctx.CityPath)
	} else {
		err = fmt.Errorf("no city to ask")
	}
	switch {
	case err != nil && onDisk == config.BeadsLedgerFeedOff:
		r.Message = ledgerFeedOffMessage
		return r
	case err != nil:
		r.Message = "ledger feed auto: feed state unknown (controller not answering, or a gc that predates the ledger-feed status)"
		r.Details = append(r.Details, err.Error())
		return r
	case st.Mode == "":
		r.Message = "ledger feed " + onDisk + ": feed state unknown (the controller holds no bead stores)"
		return r
	}

	var warnings []string
	if st.Mode != onDisk {
		warnings = append(warnings, fmt.Sprintf("ledger_feed on disk is %s; the controller runs %s until its next reload", onDisk, st.Mode))
	}
	if st.Mode == config.BeadsLedgerFeedOff {
		if n := ledgerFeedRunningScopes(st.Scopes); n > 0 {
			warnings = append(warnings, fmt.Sprintf("ledger_feed is off, but %d scope(s) still run a feed: a store the controller kept when a reload could not reopen it keeps its feed until a later reload reopens it", n))
		}
		if len(warnings) == 0 {
			r.Message = ledgerFeedOffMessage
			return r
		}
	}
	tailed, scanOnly := 0, 0
	for _, s := range st.Scopes {
		r.Details = append(r.Details, ledgerFeedScopeDetail(s))
		if s.State == ledgerFeedStateScanOnly || ledgerFeedWaiting(s) {
			scanOnly++
			continue
		}
		tailed++
		warnings = append(warnings, ledgerFeedScopeWarnings(s)...)
	}
	if len(warnings) > 0 {
		r.Status = doctor.StatusWarning
		r.Severity = doctor.SeverityAdvisory
		r.Message = strings.Join(warnings, "; ")
		r.FixHint = "find a writer the feed missed with `bd events tail`; writers that do not journal run with BD_EVENTS_JOURNAL=0 or an old bd. The controller's re-scan still covers every create and update, but not the close of a row the cache never held: such a close inside a gap, or one the feed never reads, may go unannounced"
		return r
	}
	r.Message = fmt.Sprintf("ledger feed %s: %d scope(s) tailed, %d on the re-scan", st.Mode, tailed, scanOnly)
	return r
}

// ledgerFeedOffMessage is the check's answer for a city with the feed off.
const ledgerFeedOffMessage = "ledger feed off: out-of-process writes reach the controller at its re-scan"

// ledgerFeedRunningScopes counts the scopes whose feed still runs: every one
// but a scan-only scope and a stopped feed.
func ledgerFeedRunningScopes(scopes []ledgerFeedScopeStatus) int {
	n := 0
	for _, s := range scopes {
		if s.State != ledgerFeedStateScanOnly && s.State != ledgerFeedStateStopped {
			n++
		}
	}
	return n
}

// ledgerFeedScopeWarnings are one tailed scope's advisories.
func ledgerFeedScopeWarnings(s ledgerFeedScopeStatus) []string {
	var out []string
	stateAge := time.Duration(s.StateAgeMS) * time.Millisecond
	switch s.State {
	case ledgerFeedStateStopped:
		out = append(out, fmt.Sprintf("%s: feed stopped: %s", s.Scope, s.Reason))
	case ledgerFeedStateInactive, ledgerFeedStateUnavailable, ledgerFeedStateRetrying:
		if stateAge > ledgerFeedDoctorStuckAfter {
			out = append(out, fmt.Sprintf("%s: feed %s for %s: %s", s.Scope, s.State, stateAge.Truncate(time.Second), s.Reason))
		}
	}
	if s.Misses > 0 {
		out = append(out, fmt.Sprintf("%s: the re-scan found %d create(s) or close(s) with no journal row (recent: %s)", s.Scope, s.Misses, strings.Join(s.RecentMisses, ", ")))
	}
	if s.Lag > ledgerFeedDoctorLagRows {
		out = append(out, fmt.Sprintf("%s: feed %d journal rows behind", s.Scope, s.Lag))
	}
	if s.WakesLastMinute > ledgerFeedDoctorWakesPerMinute {
		out = append(out, fmt.Sprintf("%s: %d feed wakes in the last minute (possible echo loop)", s.Scope, s.WakesLastMinute))
	}
	return out
}

// ledgerFeedWaiting reports a feed that has not read its journal since it
// started and is waiting for it (inactive, unavailable, or a late baseline):
// its scope is on the re-scan, which is not a fault.
func ledgerFeedWaiting(s ledgerFeedScopeStatus) bool {
	if s.ReadOK {
		return false
	}
	switch s.State {
	case ledgerFeedStateStarting, ledgerFeedStateLate, ledgerFeedStateInactive, ledgerFeedStateUnavailable:
		return true
	}
	return false
}

// ledgerFeedScopeDetail is one scope's detail line.
func ledgerFeedScopeDetail(s ledgerFeedScopeStatus) string {
	if s.State == ledgerFeedStateScanOnly {
		return fmt.Sprintf("%s: re-scan only (%s)", s.Scope, s.Reason)
	}
	if ledgerFeedWaiting(s) {
		return fmt.Sprintf("%s: re-scan only until its journal can be read via %s (%s: %s)", s.Scope, s.Lane, s.State, s.Reason)
	}
	parts := []string{
		fmt.Sprintf("%s: %s via %s", s.Scope, s.State, s.Lane),
		fmt.Sprintf("seq %d/%d lag %d", s.Seq, s.Head, s.Lag),
		fmt.Sprintf("pages %d rows %d ids %d", s.Pages, s.Rows, s.IDs),
		fmt.Sprintf("unheld closes %d", s.UnheldCloses),
		fmt.Sprintf("wakes %d (%d/min)", s.Wakes, s.WakesLastMinute),
		fmt.Sprintf("re-scans requested %d", s.ReconcileRequests),
		fmt.Sprintf("row age p50 %dms p99 %dms (1s resolution)", s.AgeP50MS, s.AgeP99MS),
		fmt.Sprintf("apply p99 %dms", s.ApplyP99MS),
		fmt.Sprintf("scan after refresh %d", s.ScanAfterRefresh),
	}
	if len(s.Gaps) > 0 {
		kinds := slices.Sorted(maps.Keys(s.Gaps))
		gaps := make([]string, 0, len(kinds))
		for _, kind := range kinds {
			gaps = append(gaps, fmt.Sprintf("%s=%d", kind, s.Gaps[kind]))
		}
		// The re-scan a gap requests lists active rows only (ledgerFeed.onGap).
		parts = append(parts, "gaps "+strings.Join(gaps, ",")+" (a never-held row closed inside a gap may go unannounced)")
	}
	if actors := topLedgerFeedActors(s.ActorRows, ledgerFeedDoctorTopActors); actors != "" {
		parts = append(parts, "top actors "+actors)
	}
	if s.LastError != "" {
		parts = append(parts, fmt.Sprintf("last error %s ago: %s", (time.Duration(s.LastErrorAgeMS)*time.Millisecond).Truncate(time.Second), s.LastError))
	}
	return strings.Join(parts, "; ")
}

// topLedgerFeedActors lists the n actors with the most rows.
func topLedgerFeedActors(rows map[string]int64, n int) string {
	actors := slices.Collect(maps.Keys(rows))
	slices.SortFunc(actors, func(a, b string) int {
		if rows[a] != rows[b] {
			if rows[a] > rows[b] {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	if len(actors) > n {
		actors = actors[:n]
	}
	out := make([]string, 0, len(actors))
	for _, a := range actors {
		name := a
		if name == "" {
			name = "(none)"
		}
		out = append(out, fmt.Sprintf("%s=%d", name, rows[a]))
	}
	return strings.Join(out, ",")
}
