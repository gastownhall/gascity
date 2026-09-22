package beads

import (
	"log/slog"
)

// The proxied-native lane's incident log (council pr2 E-S2).
//
// Every verdict but one is an expected refusal and is deliberately quiet: the bd
// front door is the designed outcome, and a warning per open on every scope of a
// healthy Finite-idle city would be noise an operator learns to ignore.
// head_moved is the exception. It is DETECTION after the fact — the write, if it
// was gc's, has already happened — so its whole value is that somebody is told.
//
// A verdict that is logged at one of the sites that meet it and dropped at the
// others is not detection. The verdict reaches exactly one of three consumers,
// depending on which library open produced it, and each of them logs it here:
//
//   - the factory, for a store's FIRST open (ProxiedIncidentSiteOpen). It used to
//     log through StoreOpenOptions.Logger alone, which the controller's rig
//     stores never set, so on the long-lived open the D-F3 residual says can
//     bite on a busy city the incident left no trace at all;
//   - the split store's read path, for the reopen hook's library open
//     (ProxiedIncidentSiteReadReopen), where the verdict used to become one
//     caller's read error and an unlogged stand-down;
//   - the guard tick's recovery of a stood-down handle
//     (ProxiedIncidentSiteGuardRecovery), which used to turn it into Undecided
//     with no log while doctor went on reporting the stand-down's older reason.
//
// Same message, same attributes, same level at all three, so one grep finds every
// occurrence. A nil logger is slog.Default(): a site that was handed no logger is
// the site that must not go quiet.
const (
	// ProxiedHeadMovedMessage is the message every head_moved site logs at WARN.
	ProxiedHeadMovedMessage = "proxied_native_head_moved"
	// ProxiedPostOpenUnobservedMessage is what the opener logs at WARN when the
	// post-open re-read itself failed, so the check concluded nothing about
	// what the open did. See cmd/gc's headUnmoved.
	ProxiedPostOpenUnobservedMessage = "proxied_native_post_open_unobserved"

	// ProxiedIncidentSiteOpen is a store's first library open.
	ProxiedIncidentSiteOpen = "open"
	// ProxiedIncidentSiteReadReopen is the read path's reopen hook.
	ProxiedIncidentSiteReadReopen = "read-path reopen"
	// ProxiedIncidentSiteGuardRecovery is the guard tick's recovery of a
	// non-terminally stood-down handle.
	ProxiedIncidentSiteGuardRecovery = "guard recovery"
)

// logProxiedHeadMoved reports a head_moved verdict at WARN if err carries one,
// and does nothing for any other error.
func logProxiedHeadMoved(logger *slog.Logger, scope, site string, err error) {
	verdict, ok := ProxiedVerdictOf(err)
	if !ok || verdict.Verdict != ProxiedVerdictHeadMoved {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn(ProxiedHeadMovedMessage,
		slog.String("site", site),
		slog.String("scope", scope),
		slog.String("verdict", string(verdict.Verdict)),
		slog.String("detail", verdict.Detail))
}

// LogProxiedPostOpenUnobserved reports, at WARN, a post-open re-read that could
// not be made. The open is still served — the schema gate admitted it, and the
// pool that cannot answer this statement will meet the same failure on its
// first read — but the check that exists to catch gc writing to bd's database
// did not run, and that is not something to find out from a silence.
func LogProxiedPostOpenUnobserved(logger *slog.Logger, scope, site string, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	logger.Warn(ProxiedPostOpenUnobservedMessage,
		slog.String("site", site),
		slog.String("scope", scope),
		slog.String("reason", reason))
}
