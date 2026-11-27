package beads

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Claim-authority modes, the values of [beads] claim_authority.
const (
	// ClaimAuthorityModeOff never installs triggers; the city still publishes
	// into a store another city armed.
	ClaimAuthorityModeOff = "off"
	// ClaimAuthorityModeAuto arms every store that can host the boundary.
	ClaimAuthorityModeAuto = "auto"
	// ClaimAuthorityModeRequire arms every store or fails.
	ClaimAuthorityModeRequire = "require"
)

// ClaimAuthorityReconcile is one city's desired claim-authority state for one
// store.
type ClaimAuthorityReconcile struct {
	Publisher ClaimAuthorityPublisher
	Entries   []ClaimAuthorityEntry
	Now       time.Time
	// Prune deletes this city's rows for identities missing from Entries and
	// the rows of publishers whose heartbeat outlived
	// ClaimAuthorityPublisherTTL. A pre-start publish passes false: it adds
	// one session's identities and must not drop the rest.
	Prune bool
	// HolderObservable reports whether the city can answer for the liveness
	// of a routed bead's current assignee. Auto mode does not arm a store
	// while it holds a routed claim nobody published and the city cannot
	// observe. Nil treats every holder as observable.
	HolderObservable func(assignee string) bool
}

// ClaimAuthorityReconcileReport is what one reconcile found and did.
type ClaimAuthorityReconcileReport struct {
	Capability ClaimAuthorityCapability
	// Armed reports that the store enforces the boundary after the
	// reconcile.
	Armed bool
	// ArmedNow reports that this reconcile installed or upgraded triggers.
	ArmedNow         bool
	Published        ClaimAuthorityPublishResult
	PrunedPublishers []string
	// Skipped explains why the store was left unarmed; empty when it was
	// armed or the mode is off.
	Skipped string
}

// ReconcileClaimAuthority brings one store in line with r. Off publishes only
// into an armed store. Auto and require publish, then arm, so the city's own
// claimants are authorized before the triggers can refuse them. Auto leaves a
// store it cannot host, or one with unobservable uncovered holders, unarmed
// and says why; require returns an error instead. No mode ever disarms.
func ReconcileClaimAuthority(ctx context.Context, conn *sql.Conn, r ClaimAuthorityReconcile) (ClaimAuthorityReconcileReport, error) {
	var report ClaimAuthorityReconcileReport
	mode := r.Publisher.Mode
	switch mode {
	case ClaimAuthorityModeOff, ClaimAuthorityModeAuto, ClaimAuthorityModeRequire:
	default:
		return report, fmt.Errorf("reconciling claim authority: unknown mode %q", mode)
	}
	capability, err := ProbeClaimAuthorityCapability(ctx, conn)
	if err != nil {
		return report, err
	}
	report.Capability = capability
	if !capability.LocalMetadata {
		return report, report.refuse(mode, capability.Reason())
	}
	state, err := ReadClaimAuthorityTriggerState(ctx, conn, capability.Tables)
	if err != nil {
		return report, err
	}
	report.Armed = state.Armed()
	if mode == ClaimAuthorityModeOff || !capability.CanHost() {
		if report.Armed {
			err = report.publish(ctx, conn, r)
		}
		if mode != ClaimAuthorityModeOff {
			err = firstErr(err, report.refuse(mode, capability.Reason()))
		}
		return report, err
	}
	if !report.Armed && mode == ClaimAuthorityModeAuto {
		uncovered, err := unobservableUncoveredHolders(ctx, conn, capability.Tables, r)
		if err != nil {
			return report, err
		}
		if len(uncovered) > 0 {
			report.Skipped = "routed claims held by identities no city published and this city cannot observe: " + strings.Join(uncovered, ", ")
			return report, nil
		}
	}
	if err := report.publish(ctx, conn, r); err != nil {
		return report, err
	}
	armedNow, err := ArmClaimAuthority(ctx, conn, capability.Tables)
	if err != nil {
		return report, err
	}
	report.Armed, report.ArmedNow = true, armedNow
	return report, nil
}

func (report *ClaimAuthorityReconcileReport) publish(ctx context.Context, conn *sql.Conn, r ClaimAuthorityReconcile) error {
	published, err := PublishClaimAuthority(ctx, conn, r.Publisher, r.Entries, r.Now, r.Prune)
	report.Published = published
	if err != nil || !r.Prune {
		return err
	}
	report.PrunedPublishers, err = PruneStaleClaimAuthorityPublishers(ctx, conn, r.Publisher.Tag, r.Now, ClaimAuthorityPublisherTTL)
	return err
}

// refuse records why a store stays unarmed, and is an error only in require
// mode.
func (report *ClaimAuthorityReconcileReport) refuse(mode, reason string) error {
	report.Skipped = "store cannot host the boundary: " + reason
	if mode == ClaimAuthorityModeRequire {
		return fmt.Errorf("claim authority is required but the %s", report.Skipped)
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// unobservableUncoveredHolders lists the assignees of routed holders that
// neither r.Entries nor any published row authorizes and that the city cannot
// observe: arming over them would leave their work unclaimable by its holder
// with nobody here able to tell whether that holder is alive.
func unobservableUncoveredHolders(ctx context.Context, conn *sql.Conn, tables []string, r ClaimAuthorityReconcile) ([]string, error) {
	holders, err := ListRoutedClaimHolders(ctx, conn, tables)
	if err != nil {
		return nil, err
	}
	covered := map[string]bool{}
	for _, entry := range r.Entries {
		covered[strings.TrimSpace(entry.Identity)] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, holder := range holders {
		assignee := strings.TrimSpace(holder.Assignee)
		if covered[assignee] || seen[assignee] {
			continue
		}
		seen[assignee] = true
		if r.HolderObservable != nil && !r.HolderObservable(assignee) {
			authorized, err := ClaimAuthorityIdentityAuthorized(ctx, conn, assignee)
			if err != nil {
				return nil, err
			}
			if !authorized {
				out = append(out, assignee)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
