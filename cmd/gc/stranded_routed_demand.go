package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/rollout"
)

// routedDemandStrandedEventType is the wire event type emitted when a
// gc.routed_to demand target is detected stranded (FR-4).
const routedDemandStrandedEventType = events.RoutedDemandStranded

// poolDemandWispMetadataKey marks a molecule/wisp bead as carrying
// order-dispatch pool demand (FR-5), the one narrow additive exception to
// readyExcludeTypes for stranded-demand detection purposes.
const poolDemandWispMetadataKey = beadmeta.PoolDemandWispMetadataKey

func poolDemandMetadataPair() map[string]string {
	return map[string]string{poolDemandWispMetadataKey: boolMetadata(true)}
}

// routedDemandStrandedEscalationAge is how long a routed-demand bead may sit
// stranded before the reconciler re-emits routed_demand.stranded (and, under
// Require, re-fails its owning order run) with escalated=true. Mirrors
// session_reconciler.go's unknownStateEscalationAge: escalation is a signal
// for operators and pack-level subscribers, never an auto-mutation.
const routedDemandStrandedEscalationAge = 30 * time.Minute

// routedDemandStrandedSignal is the outcome of throttling one template's
// stranded-demand candidate group against its persisted first-seen/escalated
// markers: beads is the subset actually driving this tick's emission (fresh
// first-sight detections plus, at most, one escalation), firstSeen is the
// earliest first-seen timestamp among them, and escalated is true iff at
// least one of them just crossed the escalation threshold.
type routedDemandStrandedSignal struct {
	beads     []beads.Bead
	firstSeen time.Time
	escalated bool
}

// detectStrandedRoutedDemand scans for gc.routed_to demand beads whose
// resolved template no session can ever wake for (FR-4/FR-5/FR-6): a
// dead/misspelled route, or an agent whose effective min_active_sessions is 0
// with zero sessions currently open. Candidates are ordinary ready work beads
// (sling-style, direct-metadata, graph.v2 drain-unit members) plus the one
// additive exception readyExcludeTypes normally hides: order-dispatch
// pool-demand molecule/wisp beads carrying poolDemandWispMetadataKey.
// Candidates younger than cfg.Demand.StrandedRouteDebounceOrDefault() are
// skipped so a route that wakes a session moments after dispatch never fires
// a false positive.
//
// Emission is gated by the demand.stranded_route_policy rollout gate: Off is
// a byte-identical no-op, Auto emits one warning-severity event per stranded
// template and never blocks, Require additionally marks each candidate's
// owning order run failed (when it carries one) and mirrors
// events.OrderFailed. No mode ever mutates a candidate bead's status,
// assignee, or hold labels.
//
// A stranded route is, by this feature's own premise, expected to persist for
// hours or days — so emission (and, under Require, the MarkFailed/OrderFailed
// mirror) is throttled per bead via persisted
// gc.routed_demand_stranded_first_seen / gc.routed_demand_stranded_escalated_at
// metadata markers (throttleRoutedDemandStrandedGroup): once at first
// detection, once more after routedDemandStrandedEscalationAge, then silent
// until the route becomes wakeable again, which clears both markers
// (clearRoutedDemandStrandedMarkers) so a later recurrence is treated as a
// fresh first-sight. This mirrors session_reconciler.go's
// emitSessionUnknownStateDiagnostic throttle shape.
func detectStrandedRoutedDemand(store beads.Store, cfg *config.City, sessionBeads *sessionBeadSnapshot, rec events.Recorder, stderr io.Writer, now time.Time) error {
	if store == nil || cfg == nil || rec == nil {
		return nil
	}
	flags, err := rollout.Resolve(cfg, rollout.ResolveOptions{})
	if err != nil {
		return fmt.Errorf("resolving demand.stranded_route_policy gate: %w", err)
	}
	mode := flags.StrandedRoutedDemand()
	if mode == rollout.Off {
		return nil
	}

	candidates, err := strandedRoutedDemandCandidates(store)
	if err != nil {
		return fmt.Errorf("scanning routed demand candidates: %w", err)
	}

	debounce := cfg.Demand.StrandedRouteDebounceOrDefault()
	byTemplate := map[string][]beads.Bead{}
	var templateOrder []string
	for _, b := range candidates {
		template := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey])
		if template == "" || now.Sub(b.CreatedAt) < debounce {
			continue
		}
		if _, seen := byTemplate[template]; !seen {
			templateOrder = append(templateOrder, template)
		}
		byTemplate[template] = append(byTemplate[template], b)
	}

	severity := "warning"
	if mode == rollout.Require {
		severity = "failure"
	}

	for _, template := range templateOrder {
		group := byTemplate[template]
		if !routedTemplateIsUnwakeable(cfg, sessionBeads, template) {
			clearRoutedDemandStrandedMarkers(store, stderr, group)
			continue
		}
		signal := throttleRoutedDemandStrandedGroup(store, stderr, group, now)
		if len(signal.beads) == 0 {
			continue // every bead in this group already emitted+escalated; stay silent
		}
		beadIDs := make([]string, 0, len(group))
		for _, b := range group {
			beadIDs = append(beadIDs, b.ID)
		}
		rec.Record(events.Event{
			Type:    routedDemandStrandedEventType,
			Ts:      now.UTC(),
			Actor:   "controller",
			Subject: template,
			Message: formatRoutedDemandStrandedMessage(severity, template, beadIDs),
			Payload: api.RoutedDemandStrandedPayloadJSON(severity, template, beadIDs, signal.firstSeen, signal.escalated),
		})
		if mode == rollout.Require {
			failStrandedOrderRuns(store, rec, stderr, template, signal.beads)
		}
	}
	return nil
}

// throttleRoutedDemandStrandedGroup applies the first-seen/escalated marker
// throttle to one template's stranded-demand candidate group and reports
// which beads should drive this tick's emission. Each bead is classified
// independently:
//
//   - no first-seen marker: first tick this bead is observed stranded — stamp
//     first-seen=now and include it (a fresh detection).
//   - first-seen set, already escalated: silent — this bead has already
//     produced both of its allotted emissions for this stranded generation.
//   - first-seen set, not escalated, aged past routedDemandStrandedEscalationAge:
//     stamp escalated-at=now and include it (the one escalation emission).
//   - first-seen set, not escalated, not yet aged past the threshold: silent.
//
// The returned signal's firstSeen is the earliest first-seen timestamp among
// the included beads, and escalated is true iff at least one included bead
// crossed the escalation threshold this tick.
func throttleRoutedDemandStrandedGroup(store beads.Store, stderr io.Writer, group []beads.Bead, now time.Time) routedDemandStrandedSignal {
	var signal routedDemandStrandedSignal
	takeEarliest := func(t time.Time) {
		if signal.firstSeen.IsZero() || t.Before(signal.firstSeen) {
			signal.firstSeen = t
		}
	}

	for _, b := range group {
		firstSeenRaw := strings.TrimSpace(b.Metadata[beadmeta.RoutedDemandStrandedFirstSeenMetadataKey])
		if firstSeenRaw == "" {
			setRoutedDemandStrandedMarker(store, stderr, b.ID, beadmeta.RoutedDemandStrandedFirstSeenMetadataKey, now.Format(time.RFC3339))
			takeEarliest(now)
			signal.beads = append(signal.beads, b)
			continue
		}
		if strings.TrimSpace(b.Metadata[beadmeta.RoutedDemandStrandedEscalatedAtMetadataKey]) != "" {
			continue
		}
		firstSeen, err := time.Parse(time.RFC3339, firstSeenRaw)
		if err != nil || now.Sub(firstSeen) < routedDemandStrandedEscalationAge {
			continue
		}
		setRoutedDemandStrandedMarker(store, stderr, b.ID, beadmeta.RoutedDemandStrandedEscalatedAtMetadataKey, now.Format(time.RFC3339))
		takeEarliest(firstSeen)
		signal.escalated = true
		signal.beads = append(signal.beads, b)
	}
	return signal
}

// clearRoutedDemandStrandedMarkers removes the stranded-demand throttle
// markers from every bead in group once its template's route is observed
// wakeable again. The markers are durable, so without clearing them on
// recovery a later recurrence of the same stranded condition would look like
// "already escalated" to throttleRoutedDemandStrandedGroup and be silently
// suppressed forever; clearing here means a recurrence is treated as a fresh
// first-sight, mirroring clearSessionUnknownStateMarkers. Per-bead, per-marker
// no-op when the bead carries neither marker.
func clearRoutedDemandStrandedMarkers(store beads.Store, stderr io.Writer, group []beads.Bead) {
	for _, b := range group {
		if strings.TrimSpace(b.Metadata[beadmeta.RoutedDemandStrandedFirstSeenMetadataKey]) != "" {
			setRoutedDemandStrandedMarker(store, stderr, b.ID, beadmeta.RoutedDemandStrandedFirstSeenMetadataKey, "")
		}
		if strings.TrimSpace(b.Metadata[beadmeta.RoutedDemandStrandedEscalatedAtMetadataKey]) != "" {
			setRoutedDemandStrandedMarker(store, stderr, b.ID, beadmeta.RoutedDemandStrandedEscalatedAtMetadataKey, "")
		}
	}
}

// setRoutedDemandStrandedMarker stamps (or, given an empty value, clears) a
// stranded-demand throttle marker on beadID, logging a best-effort error on
// write failure — the same event-first, store-write-best-effort pairing
// failStrandedOrderRuns uses. A failed stamp only costs an extra emission on
// the next tick, never a stuck or silently-dropped one.
func setRoutedDemandStrandedMarker(store beads.Store, stderr io.Writer, beadID, key, value string) {
	if err := store.SetMetadata(beadID, key, value); err != nil {
		logDispatchError(stderr, "gc: stranded routed demand: failed to set marker %s on %s: %v", key, beadID, err)
	}
}

// strandedRoutedDemandCandidates returns every open, unblocked bead carrying
// gc.routed_to metadata: the normal ready-work set (Ready already excludes
// blocked beads and molecule/wisp types) plus molecule/wisp beads explicitly
// marked as order-dispatch pool demand via poolDemandWispMetadataKey — the
// one narrow additive exception to Ready's type exclusion.
func strandedRoutedDemandCandidates(store beads.Store) ([]beads.Bead, error) {
	ready, err := store.Ready()
	if err != nil {
		return nil, err
	}
	var candidates []beads.Bead
	for _, b := range ready {
		if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
			candidates = append(candidates, b)
		}
	}

	wisps, err := store.List(beads.ListQuery{
		Status:    "open",
		Metadata:  poolDemandMetadataPair(),
		TierMode:  beads.TierBoth,
		AllowScan: true,
	})
	if err != nil {
		return nil, err
	}
	for _, b := range wisps {
		if !beads.IsMoleculeType(b.Type) {
			continue
		}
		if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
			candidates = append(candidates, b)
		}
	}
	return candidates, nil
}

// routedTemplateIsUnwakeable reports whether template names an agent no
// session can currently wake for: the template resolves to no configured
// agent (dead/misspelled route), or its effective min_active_sessions is 0
// and no session is presently open for it.
func routedTemplateIsUnwakeable(cfg *config.City, sessionBeads *sessionBeadSnapshot, template string) bool {
	if sessionBeads != nil {
		if _, ok := sessionBeads.FindInfoByTemplate(template); ok {
			return false
		}
	}
	agent, ok := findAgentByName(cfg, template)
	if !ok {
		return true
	}
	return agent.EffectiveMinActiveSessions() == 0
}

// formatRoutedDemandStrandedMessage renders the operator-facing text for a
// routed_demand.stranded event.
func formatRoutedDemandStrandedMessage(severity, template string, beadIDs []string) string {
	verb := "will not be picked up automatically"
	if severity == "failure" {
		verb = "failing the owning order run(s) as well"
	}
	return fmt.Sprintf("routed demand for template %q is stranded (%d bead(s): %s); no session can currently wake for this route, %s",
		template, len(beadIDs), strings.Join(beadIDs, ", "), verb)
}

// failStrandedOrderRuns mirrors a Require-mode stranded detection onto the
// order-dispatch run ledger. group is the caller's throttled emission subset
// (routedDemandStrandedSignal.beads), not necessarily the full candidate
// group for template — this keeps MarkFailed/OrderFailed on the same
// once-at-first-detection-plus-one-escalation cadence as the event itself
// rather than re-firing every tick a route stays stranded. Each bead that
// carries an order-run label gets a matching events.OrderFailed mirror and
// has its run marked orders.RunOutcomeWispFailed — labels only, never the
// bead's status or assignee — following the same event-first,
// store-write-best-effort pairing order_dispatch.go's markTrackingFailure
// call sites use.
func failStrandedOrderRuns(store beads.Store, rec events.Recorder, stderr io.Writer, template string, group []beads.Bead) {
	front := orders.NewStore(beads.OrdersStore{Store: store})
	for _, b := range group {
		run, ok := orders.RunFromTrackingBead(b)
		if !ok {
			continue
		}
		rec.Record(events.Event{
			Type:    events.OrderFailed,
			Actor:   "controller",
			Subject: run.Scoped,
			Message: fmt.Sprintf("order %s failed: routed demand for template %q is stranded; no session can currently wake for this route", run.Scoped, template),
		})
		if err := front.MarkFailed(run.ID, run.Scoped, orders.RunOutcomeWispFailed, nil); err != nil {
			logDispatchError(stderr, "gc: stranded routed demand: failed to mark order run %s failed: %v", run.Scoped, err)
		}
	}
}
