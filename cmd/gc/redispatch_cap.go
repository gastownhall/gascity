package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

const (
	// drainAckAssignedWorkCycleCap is the number of consecutive
	// drain-acked-with-assigned-work cycles (a pool session claims a work
	// bead, correctly refuses to execute or close it, escalates, and
	// drains — repeatedly) a single work bead may accumulate before
	// enforceDrainAckAssignedWorkCycleCap auto-holds it (ra-3y4okc).
	// Measured overnight: 355 cycles in 3h at roughly 58s/cycle, with both
	// novices slots pinned at 100%. Five cycles bounds the blast radius to a
	// few minutes of looping while staying well above the rare legitimate
	// re-drain — a genuine multi-turn item can drain-ack more than once
	// across its life, but does not do so five times in a row inside one
	// window.
	drainAckAssignedWorkCycleCap = 5
	// drainAckAssignedWorkCycleWindow bounds how long a streak of cycles may
	// span before it is treated as stale and restarted at 1. Without this, a
	// work bead that legitimately drain-acks with assigned work every so
	// often over a long lifetime would eventually accumulate past the cap
	// from calendar time alone, not from looping.
	drainAckAssignedWorkCycleWindow = 30 * time.Minute
)

// drainAckAssignedWorkCycleDecision is the pure outcome of evaluating one
// work bead's redispatch-cycle metadata against now.
type drainAckAssignedWorkCycleDecision struct {
	cycles      int
	windowStart time.Time
	tripped     bool
}

// nextDrainAckAssignedWorkCycle computes the next consecutive drain-ack cycle
// count for a work bead carrying
// gc.drain_ack_assigned_work_cycle_count/_window_start, and whether this
// observation crosses drainAckAssignedWorkCycleCap. A count observed outside
// drainAckAssignedWorkCycleWindow of the streak's recorded start is the start
// of a fresh streak (cycles=1, windowStart=now) rather than an accumulation,
// so occasional legitimate multi-turn drain-acks spread across a bead's
// lifetime never trip the cap. A malformed or missing window/count is treated
// the same as absent (fresh streak) — this guard fails open toward "keep
// counting from 1", never toward silently refusing to ever trip.
func nextDrainAckAssignedWorkCycle(wb beads.Bead, now time.Time) drainAckAssignedWorkCycleDecision {
	windowStart := now
	prevCount := 0
	if raw := strings.TrimSpace(wb.Metadata[beadmeta.DrainAckAssignedWorkCycleWindowStartMetadataKey]); raw != "" {
		if ts, err := time.Parse(time.RFC3339, raw); err == nil {
			if elapsed := now.Sub(ts); elapsed >= 0 && elapsed <= drainAckAssignedWorkCycleWindow {
				windowStart = ts
				if n, err := strconv.Atoi(strings.TrimSpace(wb.Metadata[beadmeta.DrainAckAssignedWorkCycleCountMetadataKey])); err == nil && n > 0 {
					prevCount = n
				}
			}
		}
	}
	cycles := prevCount + 1
	return drainAckAssignedWorkCycleDecision{cycles: cycles, windowStart: windowStart, tripped: cycles >= drainAckAssignedWorkCycleCap}
}

// enforceDrainAckAssignedWorkCycleCap is the ra-3y4okc redispatch-livelock
// guard. finalizeDrainAckStoppedSession calls this whenever a session
// drain-acks while still assigned to open work — the same condition
// recordDrainAckAssignedWorkEvent reports as
// events.SessionDrainAckedWithAssignedWork. Left alone, this
// escalate-and-drain cycle repeats forever: a misrouted bead the pool may not
// execute or close keeps its assignee, the dispatcher rereads that as live
// work, and the same (or another) pool session gets it again. Measured
// overnight: 355 cycles in 3h, both novices slots at 100%, P1 work starved
// behind it.
//
// The guard counts consecutive cycles per work bead (in bead metadata, so it
// survives reconciler restarts) and, once the cap trips, applies the same
// repair the manual fix used on the two overnight instances: add the
// hold:mayor label (the routing lead is the correct next actor for a
// misroute the pool may not dispose of) and clear the assignee. Both are
// required — ra-kuxm33 already carried gc.work_outcome=blocked and still
// looped, because the dispatcher's redispatch path reads the assignee, not
// the outcome stamp. hold:mayor is the existing DispatchHoldLabels value
// (internal/beadmeta/hold_labels.go), already excluded from every
// route-scoped, unassigned dispatch tier (filterReadyByRoute,
// EffectivePoolDemandQuery, EffectiveWorkQuery Tier 3), so tripping the guard
// needs no new dispatch-side filtering to take effect.
func enforceDrainAckAssignedWorkCycleCap(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	info sessionpkg.Info,
	clk clock.Clock,
	rec events.Recorder,
	stderr io.Writer,
) {
	if store == nil {
		return
	}
	strandedBead, found, err := firstOpenAssignedWorkBeadForReachableStore(cityPath, cfg, store, rigStores, info)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: redispatch-cap lookup for drain-acked %s: %v\n", info.ID, err) //nolint:errcheck
		return
	}
	if !found {
		return
	}
	ownerStore := storeForPoolAssignment(cfg, store, rigStores, strandedBead)
	if ownerStore == nil {
		return
	}
	if clk == nil {
		clk = clock.Real{}
	}
	now := clk.Now().UTC()
	decision := nextDrainAckAssignedWorkCycle(strandedBead, now)
	if !decision.tripped {
		if err := ownerStore.Update(strandedBead.ID, beads.UpdateOpts{
			Metadata: map[string]string{
				beadmeta.DrainAckAssignedWorkCycleCountMetadataKey:       strconv.Itoa(decision.cycles),
				beadmeta.DrainAckAssignedWorkCycleWindowStartMetadataKey: decision.windowStart.Format(time.RFC3339),
			},
		}); err != nil {
			fmt.Fprintf(stderr, "session reconciler: recording redispatch cycle %d for %s: %v\n", decision.cycles, strandedBead.ID, err) //nolint:errcheck
		}
		return
	}
	empty := ""
	if err := ownerStore.Update(strandedBead.ID, beads.UpdateOpts{
		Assignee: &empty,
		Labels:   []string{beadmeta.HoldMayorLabel},
		Metadata: map[string]string{
			beadmeta.DrainAckAssignedWorkCycleCountMetadataKey:       "",
			beadmeta.DrainAckAssignedWorkCycleWindowStartMetadataKey: "",
		},
	}); err != nil {
		fmt.Fprintf(stderr, "session reconciler: auto-holding redispatch-capped %s: %v\n", strandedBead.ID, err) //nolint:errcheck
		return
	}
	if rec == nil {
		return
	}
	routedTo := strings.TrimSpace(strandedBead.Metadata[beadmeta.RoutedToMetadataKey])
	rec.Record(events.Event{
		Type:      events.BeadRedispatchCapHeld,
		Ts:        now,
		Actor:     "gc",
		Subject:   strandedBead.ID,
		Message:   formatRedispatchCapHeldMessage(strandedBead.ID, routedTo, decision.cycles),
		SessionID: info.ID,
		Payload:   api.BeadRedispatchCapHeldPayloadJSON(strandedBead.ID, info.ID, routedTo, decision.cycles),
	})
}

// formatRedispatchCapHeldMessage renders the operator-facing text for a
// bead.redispatch_cap_held event, mirroring
// formatDeadAssigneeReopenedMessage's shape for the sibling auto-repair
// action (dead_assignee_event.go).
func formatRedispatchCapHeldMessage(beadID, routedTo string, cycles int) string {
	route := routedTo
	if route == "" {
		route = "<unrouted>"
	}
	return fmt.Sprintf("auto-held %s after %d consecutive drain-acked-with-assigned-work cycles on route %s (hold:mayor, assignee cleared)", beadID, cycles, route)
}
