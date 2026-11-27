package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/session"
)

// claimAuthorityCheck reports, per bd scope, the claim-authority mode,
// whether the store can host the boundary, whether it is armed and current,
// its publisher heartbeats, and a read-only dry run of the routed beads held
// by identities outside this city's authority set. It never writes.
type claimAuthorityCheck struct {
	cfg          *config.City
	cityPath     string
	liveSessions func() ([]session.Info, error)
}

func newClaimAuthorityCheck(cfg *config.City, cityPath string) *claimAuthorityCheck {
	return &claimAuthorityCheck{cfg: cfg, cityPath: cityPath, liveSessions: func() ([]session.Info, error) {
		return doctorLiveSessionInfos(cityPath, cfg)
	}}
}

func (c *claimAuthorityCheck) Name() string { return "claim-authority" }

// CanFix returns false: arming is the controller's, disarming the operator's
// (gc beads claim-authority disarm).
func (c *claimAuthorityCheck) CanFix() bool { return false }

// Fix is a no-op; the check only reports.
func (c *claimAuthorityCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *claimAuthorityCheck) WarmupEligible() bool { return false }

// Run inspects every bd scope of the city.
func (c *claimAuthorityCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	if c.cfg == nil {
		return okCheck(c.Name(), "no config available")
	}
	mode, err := claimAuthorityMode(c.cfg)
	if err != nil {
		return errorCheck(c.Name(), err.Error(), "fix [beads] claim_authority or GC_BEADS_CLAIM_AUTHORITY", nil)
	}
	sessions, sessionsErr := c.liveSessions()
	entries := claimAuthorityEntries(c.cfg, sessions)
	var scopes []claimAuthorityScopeInspection
	for _, scope := range claimAuthorityScopes(c.cityPath, c.cfg) {
		si := claimAuthorityScopeInspection{Scope: scope}
		si.Reached, si.Err = claimAuthorityStoreConn(c.cityPath, scope.Root, func(ctx context.Context, conn *sql.Conn) error {
			var ierr error
			si.Inspection, ierr = beads.InspectClaimAuthority(ctx, conn, entries)
			return ierr
		})
		scopes = append(scopes, si)
	}
	return claimAuthorityDoctorResult(c.Name(), mode, scopes, sessionsErr)
}

// claimAuthorityScopeInspection is one scope's read-only inspection.
type claimAuthorityScopeInspection struct {
	Scope      claimAuthorityScope
	Reached    bool
	Inspection beads.ClaimAuthorityInspection
	Err        error
}

// claimAuthorityDoctorResult renders the scope inspections. A store armed
// with no publisher advertising auto or require, a store that cannot host
// the boundary under auto or require, an unarmed store under auto, and any
// routed claim held outside the authority set are warnings; an unarmed store
// under require is an error.
func claimAuthorityDoctorResult(name, mode string, scopes []claimAuthorityScopeInspection, sessionsErr error) *doctor.CheckResult {
	var details []string
	warnings, errs := 0, 0
	if sessionsErr != nil {
		warnings++
		details = append(details, fmt.Sprintf("open sessions unreadable, so live session claims may be listed as outside: %v", sessionsErr))
	}
	outside := 0
	for _, si := range scopes {
		id := si.Scope.ID
		if si.Err != nil {
			warnings++
			details = append(details, fmt.Sprintf("%s: inspection failed: %v", id, si.Err))
			continue
		}
		if !si.Reached {
			details = append(details, id+": no direct SQL endpoint; claim authority does not apply")
			continue
		}
		in := si.Inspection
		details = append(details, fmt.Sprintf("%s: mode=%s capable=%v armed=%v current=%v publishers=%s",
			id, mode, in.Capability.CanHost(), in.Triggers.Armed(), in.Triggers.Current, claimAuthorityPublishersText(in.Heartbeats)))
		switch {
		case !in.Capability.CanHost() && mode != beads.ClaimAuthorityModeOff:
			details = append(details, fmt.Sprintf("%s: cannot host the boundary: %s", id, in.Capability.Reason()))
			if mode == beads.ClaimAuthorityModeRequire {
				errs++
			} else {
				warnings++
			}
		case !in.Triggers.Armed() && mode == beads.ClaimAuthorityModeRequire:
			errs++
			details = append(details, id+": claim authority is required but the store is not armed")
		case !in.Triggers.Armed() && mode == beads.ClaimAuthorityModeAuto:
			warnings++
			details = append(details, id+": not armed yet; the controller arms it once no unobservable holder blocks it")
		}
		if in.ArmedWithoutArmingPublisher() {
			warnings++
			details = append(details, id+": armed while no publisher advertises auto or require; run `gc beads claim-authority disarm` or set a city to auto")
		}
		for _, holder := range in.Outside {
			outside++
			details = append(details, fmt.Sprintf("%s: %s (%s, %s) is held by %s, outside the authority set", id, holder.BeadID, holder.Table, holder.Status, holder.Assignee))
		}
	}
	warnings += outside
	switch {
	case errs > 0:
		return errorCheck(name, fmt.Sprintf("claim authority (%s): %d store(s) not enforcing a required boundary", mode, errs),
			"grant TRIGGER on a Dolt sql-server store, or set [beads] claim_authority to auto", details)
	case warnings > 0:
		return warnCheck(name, fmt.Sprintf("claim authority (%s): %d finding(s), %d routed claim(s) outside the authority set", mode, warnings, outside),
			"see details", details)
	}
	result := okCheck(name, fmt.Sprintf("claim authority (%s): %d store(s) consistent", mode, len(scopes)))
	result.Details = details
	return result
}

func claimAuthorityPublishersText(heartbeats []beads.ClaimAuthorityHeartbeat) string {
	if len(heartbeats) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(heartbeats))
	for _, hb := range heartbeats {
		parts = append(parts, fmt.Sprintf("%s:%s@%s", hb.City, hb.Mode, hb.PublishedAt.UTC().Format("2006-01-02T15:04:05Z")))
	}
	return strings.Join(parts, ",")
}
