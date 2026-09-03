// Package state implements the bead effective-state classifier.
// It collapses status + dependencies + delivery phase + session binding
// + labels + type + routing into a single unambiguous EffectiveState per bead.
// The package is pure: no I/O, no store imports; all inputs are passed as parameters.
// See engdocs/contributors/bead-effective-state.md for the taxonomy.
package state

import (
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// EffectiveState is a single, unambiguous classification of a bead's current
// state, encoding who owns the next action.
type EffectiveState string

// The 16 effective states, in precedence order (first match wins in Classify).
const (
	StateDone                  EffectiveState = "done"
	StateDeferred              EffectiveState = "deferred"
	StatePinned                EffectiveState = "pinned"
	StateOrchestration         EffectiveState = "orchestration"
	StateDelivering            EffectiveState = "delivering"
	StateWaitingReview         EffectiveState = "waiting-review"
	StateWaitingDecision       EffectiveState = "waiting-decision"
	StateInProgress            EffectiveState = "in-progress"
	StateOrphaned              EffectiveState = "orphaned"
	StateBlockedDeps           EffectiveState = "blocked-deps"
	StateWaitingHuman          EffectiveState = "waiting-human"
	StateEpicTriage            EffectiveState = "epic-triage"
	StateRoutedWaiting         EffectiveState = "routed-waiting"
	StateRoutedStalledDispatch EffectiveState = "routed-stalled-dispatch"
	StateReadyUnrouted         EffectiveState = "ready-unrouted"
	StateUnknown               EffectiveState = "unknown"
)

// owner maps each effective state to the role-agnostic owner of the next action.
// Labels are deliberately role-agnostic — zero hardcoded role names.
var owner = map[EffectiveState]string{
	StateDone:                  "—",
	StateDeferred:              "scheduler",
	StatePinned:                "—",
	StateOrchestration:         "controller",
	StateDelivering:            "agent",
	StateWaitingReview:         "human",
	StateWaitingDecision:       "human",
	StateInProgress:            "agent",
	StateOrphaned:              "RECLAIM",
	StateBlockedDeps:           "upstream-beads",
	StateWaitingHuman:          "human",
	StateEpicTriage:            "human/triage",
	StateRoutedWaiting:         "agent-pool",
	StateRoutedStalledDispatch: "RECLAIM",
	StateReadyUnrouted:         "DISPATCHER",
	StateUnknown:               "INVESTIGATE",
}

// Owner returns the role-agnostic owner string for state s.
func Owner(s EffectiveState) string {
	if o, ok := owner[s]; ok {
		return o
	}
	return "INVESTIGATE"
}

// anomalyStates is the set of states that represent actionable problems rather
// than normal progress. It lives here, next to the state vocabulary it
// partitions, so a renderer can never drift from the taxonomy: adding a state
// without deciding whether it is an anomaly is a change in this file.
var anomalyStates = map[EffectiveState]bool{
	StateOrphaned:              true,
	StateReadyUnrouted:         true,
	StateRoutedStalledDispatch: true,
	StateUnknown:               true,
}

// IsAnomaly reports whether s is an actionable problem state (as opposed to
// normal progress). Renderers use this to flag rows rather than keeping their
// own copy of the partition.
func IsAnomaly(s EffectiveState) bool { return anomalyStates[s] }

// DisplayOrder is the recommended report order: anomalies first, then waiting
// states, then active, then terminal/frozen.
var DisplayOrder = []EffectiveState{
	StateReadyUnrouted,
	StateOrphaned,
	StateRoutedStalledDispatch,
	StateUnknown,
	StateBlockedDeps,
	StateWaitingHuman,
	StateWaitingReview,
	StateWaitingDecision,
	StateEpicTriage,
	StateRoutedWaiting,
	StateInProgress,
	StateDelivering,
	StateOrchestration,
	StateDeferred,
	StatePinned,
	StateDone,
}

// BeadView is the minimal read interface Classify() requires. It allows the
// classifier to operate over both the API list DTO and the native store Bead,
// avoiding import cycles and store dependencies.
type BeadView interface {
	ID() string
	Status() string
	IssueType() string
	Title() string
	Labels() []string
	// Meta returns the metadata value for key, or "" if absent.
	Meta(key string) string
}

// workTypes is the set of bead types that represent human-meaningful work.
// Everything else (session, convoy, step, workflow, scope, …) is gc-internal
// orchestration owned by the controller. Keep in sync with the dispatcher's
// unrouted-feeder PLANNABLE_TYPES definition.
var workTypes = map[string]bool{
	"task":     true,
	"bug":      true,
	"feature":  true,
	"chore":    true,
	"epic":     true,
	"decision": true,
}

// plannableTypes is the subset of workTypes that the dispatcher routes autonomously.
var plannableTypes = map[string]bool{
	"task":    true,
	"bug":     true,
	"feature": true,
	"chore":   true,
}

// internalTitleRe matches gc-internal bookkeeping beads that masquerade as
// work types via their title prefix (nudge:, order:, wisp:).
// Keep in sync with the unrouted-feeder GC_INTERNAL_TITLE_RE definition.
var internalTitleRe = regexp.MustCompile(`(?i)^\s*(nudge|order|wisp)\s*:`)

// Delivery phase strings. These mirror internal/delivery constants; defined
// locally to keep this package free of store or delivery imports.
var deliveryAgentPhases = map[string]bool{
	"building":      true,
	"ci-pending":    true,
	"rework":        true,
	"merge-pending": true,
	"conflicted":    true,
}

var deliveryTerminalPhases = map[string]bool{
	"merged":    true,
	"abandoned": true,
}

// Classify returns the single effective state for bead b given the current
// system context:
//   - ready: set of bead IDs the store considers ready (unblocked, open)
//   - blocked: set of bead IDs with unresolved blocking dependencies
//   - live: set of live session names, as judged by the caller. The callers
//     in this repo use session.ProjectLifecycle's CountsAgainstCap; zombie
//     sessions (bead says active, process is gone) are NOT yet excluded — see
//     engdocs/contributors/bead-effective-state.md
//   - liveRigs: set of rig names that have at least one live session;
//     a nil liveRigs disables routed-stalled-dispatch detection
//
// The decision tree is a precedence chain — the first matching rule wins.
// Ports scripts/bead-state.py classify() 1:1 and adds the routed-stalled-dispatch
// enhancement from the vp-evof notes (vw-msnr / vw-5t6s incident).
func Classify(b BeadView, ready, blocked, live, liveRigs map[string]bool) EffectiveState {
	status := strings.ToLower(b.Status())
	typ := strings.ToLower(b.IssueType())
	title := b.Title()
	bid := b.ID()
	phase := b.Meta(beadmeta.PhaseMetadataKey)
	routed := b.Meta(beadmeta.RoutedToMetadataKey)
	session := b.Meta(beadmeta.SessionNameMetadataKey)
	gcKind := b.Meta(beadmeta.KindMetadataKey)

	labelsSlice := b.Labels()
	labels := make(map[string]bool, len(labelsSlice))
	for _, l := range labelsSlice {
		labels[strings.ToLower(l)] = true
	}

	// 1. Terminal / frozen.
	if status == "closed" || deliveryTerminalPhases[phase] {
		return StateDone
	}
	if status == "deferred" {
		return StateDeferred
	}
	if status == "pinned" {
		return StatePinned
	}

	// 2. gc-internal / orchestration: non-work types, wisp IDs, internal title
	// prefixes, and gc.kind-tagged beads are owned by the controller.
	if !workTypes[typ] || strings.Contains(bid, "-wisp-") || internalTitleRe.MatchString(title) || gcKind != "" {
		return StateOrchestration
	}

	// 3. Delivery phase (most specific in-flight signal).
	if phase == "review-pending" {
		return StateWaitingReview
	}
	if phase == "decision-pending" {
		return StateWaitingDecision
	}
	if deliveryAgentPhases[phase] {
		return StateDelivering
	}

	// 4. Actively held by a session.
	if status == "in_progress" || status == "hooked" || session != "" {
		if session != "" && live != nil && !live[session] {
			return StateOrphaned
		}
		return StateInProgress
	}

	// 5. Blocked by other beads.
	if blocked[bid] {
		return StateBlockedDeps
	}

	// 6. Human-gated.
	if labels["human"] || typ == "decision" || isTruthy(b.Meta(beadmeta.DoNotAutoRouteMetadataKey)) {
		return StateWaitingHuman
	}

	// 7. Epic container needing decomposition.
	upper := strings.ToUpper(strings.TrimSpace(title))
	if typ == "epic" || strings.HasPrefix(upper, "EPIC:") || strings.HasPrefix(upper, "EPIC ") {
		return StateEpicTriage
	}

	// 8. Routed to a pool, awaiting pickup.
	if routed != "" {
		if liveRigs != nil {
			rig, _, hasSlash := strings.Cut(routed, "/")
			if hasSlash && !liveRigs[rig] {
				return StateRoutedStalledDispatch
			}
		}
		return StateRoutedWaiting
	}

	// 9. Ready + plannable + unrouted → dispatcher should pick it up.
	if status == "open" && ready[bid] && plannableTypes[typ] {
		return StateReadyUnrouted
	}

	// 10. Nothing matched → surface the gap loudly.
	return StateUnknown
}

func isTruthy(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "1" || v == "true" || v == "yes"
}
