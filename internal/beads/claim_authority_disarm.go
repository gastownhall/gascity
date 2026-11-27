package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrClaimAuthorityDisarmRefused is returned when disarming a store could
// lock a city's intent out: a publisher still advertises auto or require, or
// no publisher heartbeat is present to say what co-tenants want.
var ErrClaimAuthorityDisarmRefused = errors.New("claim authority disarm refused")

// CheckClaimAuthorityDisarm decides whether a store whose publishers left
// heartbeats may be disarmed. It refuses while any heartbeat advertises auto
// or require, and refuses an empty heartbeat set (the wiped state, where
// co-tenant intent is unknown) unless force is set.
func CheckClaimAuthorityDisarm(heartbeats []ClaimAuthorityHeartbeat, force bool) error {
	var arming []string
	for _, hb := range heartbeats {
		if hb.Mode == ClaimAuthorityModeAuto || hb.Mode == ClaimAuthorityModeRequire {
			arming = append(arming, fmt.Sprintf("%s (%s)", hb.City, hb.Mode))
		}
	}
	if len(arming) > 0 {
		return fmt.Errorf("%w: publishers still arm this store: %s; set them to off first", ErrClaimAuthorityDisarmRefused, strings.Join(arming, ", "))
	}
	if len(heartbeats) == 0 && !force {
		return fmt.Errorf("%w: no publisher heartbeat is present, so co-tenant intent is unknown; pass --force to disarm anyway", ErrClaimAuthorityDisarmRefused)
	}
	return nil
}

// DisarmClaimAuthorityGuarded drops the store's claim-authority triggers when
// CheckClaimAuthorityDisarm allows it, and reports whether it dropped any.
func DisarmClaimAuthorityGuarded(ctx context.Context, conn *sql.Conn, force bool) (bool, error) {
	heartbeats, err := ReadClaimAuthorityHeartbeats(ctx, conn)
	if err != nil {
		return false, err
	}
	if err := CheckClaimAuthorityDisarm(heartbeats, force); err != nil {
		return false, err
	}
	return DisarmClaimAuthority(ctx, conn)
}

// ClaimAuthorityInspection is a read-only view of one store's boundary.
type ClaimAuthorityInspection struct {
	Capability ClaimAuthorityCapability
	Triggers   ClaimAuthorityTriggerState
	Heartbeats []ClaimAuthorityHeartbeat
	// Outside are the routed holders whose assignee neither entries nor any
	// published row authorizes: the claims an armed store would refuse.
	Outside []ClaimAuthorityHolder
}

// ArmedWithoutArmingPublisher reports a store that enforces the boundary
// while no publisher advertises auto or require: nobody keeps it current,
// and only the disarm command or a city switched to auto resolves it.
func (i ClaimAuthorityInspection) ArmedWithoutArmingPublisher() bool {
	if !i.Triggers.Armed() {
		return false
	}
	for _, hb := range i.Heartbeats {
		if hb.Mode == ClaimAuthorityModeAuto || hb.Mode == ClaimAuthorityModeRequire {
			return false
		}
	}
	return true
}

// InspectClaimAuthority reads a store's capability, triggers, heartbeats and
// the routed holders outside entries, writing nothing.
func InspectClaimAuthority(ctx context.Context, db ClaimAuthorityDB, entries []ClaimAuthorityEntry) (ClaimAuthorityInspection, error) {
	var in ClaimAuthorityInspection
	var err error
	if in.Capability, err = ProbeClaimAuthorityCapability(ctx, db); err != nil {
		return in, err
	}
	if !in.Capability.LocalMetadata {
		return in, nil
	}
	if in.Triggers, err = ReadClaimAuthorityTriggerState(ctx, db, in.Capability.Tables); err != nil {
		return in, err
	}
	if in.Heartbeats, err = ReadClaimAuthorityHeartbeats(ctx, db); err != nil {
		return in, err
	}
	holders, err := ListRoutedClaimHolders(ctx, db, in.Capability.Tables)
	if err != nil {
		return in, err
	}
	covered := map[string]bool{}
	for _, entry := range entries {
		covered[strings.TrimSpace(entry.Identity)] = true
	}
	for _, holder := range holders {
		if covered[strings.TrimSpace(holder.Assignee)] {
			continue
		}
		authorized, err := ClaimAuthorityIdentityAuthorized(ctx, db, holder.Assignee)
		if err != nil {
			return in, err
		}
		if !authorized {
			in.Outside = append(in.Outside, holder)
		}
	}
	return in, nil
}
