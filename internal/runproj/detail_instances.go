package runproj

import (
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// buildRunDisplayNode projects one semantic group into a display node, including
// its execution instances, iteration/attempt summaries, and control badges.
// Port of TS buildRunDisplayNode (execution-instances.ts). latestLoopIteration's
// bool mirrors `number | undefined`.
func buildRunDisplayNode(group runNodeGroup, controlBadges []RunControlBadge, latestLoopIteration int, hasLatestLoopIteration bool, ctx runSessionLinkContext) RunDisplayNode {
	instances := make([]RunExecutionInstance, 0, len(group.beads))
	for index := range group.beads {
		instances = append(instances, buildExecutionInstance(group.semanticNodeID, group.beads[index], index, ctx))
	}
	sortExecutionInstances(instances)

	// preferredExecutionInstance re-sorts (a no-op on the already-sorted slice)
	// and takes the last element.
	hasVisible := len(instances) > 0

	iterationSet := map[int]bool{}
	for _, inst := range instances {
		if v, ok := iterationValue(inst.Iteration); ok {
			iterationSet[v] = true
		}
	}

	visibleIteration, hasVisibleIteration := 0, false
	if hasVisible {
		if v, ok := iterationValue(instances[len(instances)-1].Iteration); ok {
			visibleIteration, hasVisibleIteration = v, true
		}
	}
	if !hasVisibleIteration && len(iterationSet) > 0 {
		visibleIteration, hasVisibleIteration = maxKey(iterationSet), true
	}

	historicalOnly := group.loopControlNodeID != "" &&
		hasVisibleIteration &&
		hasLatestLoopIteration &&
		visibleIteration < latestLoopIteration

	for k := range instances {
		// currentIteration = !historicalOnly && (visibleIteration is undefined ||
		// instance's loop value === visibleIteration). A base (non-loop) instance
		// under a known visible iteration compares undefined === number → false.
		currentIteration := !historicalOnly
		if currentIteration && hasVisibleIteration {
			v, ok := iterationValue(instances[k].Iteration)
			currentIteration = ok && v == visibleIteration
		}
		instances[k].CurrentIteration = currentIteration
		instances[k].Historical = !currentIteration
		if instances[k].Session.Kind == "attached" {
			instances[k].Session.Streamable = currentIteration && isRunningStatus(instances[k].Status)
		}
	}

	if !hasVisible {
		panic("runproj: run node " + group.semanticNodeID + " has no execution instances")
	}
	visible := &instances[len(instances)-1]

	if controlBadges == nil {
		controlBadges = []RunControlBadge{}
	}

	return RunDisplayNode{
		ID:                         group.semanticNodeID,
		SemanticNodeID:             group.semanticNodeID,
		Title:                      group.title,
		Kind:                       group.kind,
		ConstructKind:              group.constructKind,
		Status:                     aggregateStatus(instances, visible),
		CurrentBeadID:              visible.BeadID,
		Scope:                      runNodeScope(group.scopeRef),
		VisibleInGraph:             !historicalOnly,
		HistoricalOnly:             historicalOnly,
		IterationSummary:           iterationSummaryFor(visibleIteration, hasVisibleIteration, len(iterationSet), group.loopControlNodeID),
		AttemptSummary:             attemptSummaryFor(instances, group.beads),
		VisibleExecutionInstanceID: visible.ID,
		ExecutionInstances:         instances,
		ControlBadges:              controlBadges,
	}
}

// latestIterationsByLoop computes the furthest iteration reached per loop-control
// node. Port of TS latestIterationsByLoop.
func latestIterationsByLoop(groups []runNodeGroup) map[string]int {
	latest := make(map[string]int)
	for _, group := range groups {
		if group.loopControlNodeID == "" {
			continue
		}
		for _, bead := range group.beads {
			iteration, ok := iterationFor(bead)
			if !ok {
				continue
			}
			if cur, exists := latest[group.loopControlNodeID]; !exists || iteration > cur {
				latest[group.loopControlNodeID] = iteration
			}
		}
	}
	return latest
}

// buildExecutionInstance projects one physical bead into an execution instance.
// Port of TS buildExecutionInstance.
func buildExecutionInstance(semanticNodeID string, bead runSnapshotBead, index int, ctx runSessionLinkContext) RunExecutionInstance {
	beadID := nonEmpty(bead.id)
	if beadID == "" {
		panic("runproj: run node " + semanticNodeID + " has a bead with an empty id")
	}
	iteration, hasIteration := iterationFor(bead)
	attempt, hasAttempt := attemptFor(bead)
	status := presentationStatus(bead)
	sessionLink, hasLink := runSessionLinkFor(bead, status, ctx)

	id := beadID
	if id == "" {
		iterPart := 0
		if hasIteration {
			iterPart = iteration
		}
		attemptPart := index
		if hasAttempt {
			attemptPart = attempt
		}
		id = semanticNodeID + ":iteration-" + strconv.Itoa(iterPart) + ":attempt-" + strconv.Itoa(attemptPart)
	}

	return RunExecutionInstance{
		ID:               id,
		SemanticNodeID:   semanticNodeID,
		BeadID:           beadID,
		Iteration:        iterationState(iteration, hasIteration),
		Attempt:          attemptState(attempt, hasAttempt),
		Label:            instanceLabel(iteration, hasIteration, attempt, hasAttempt),
		Status:           status,
		Session:          sessionState(status, sessionLink, hasLink),
		CurrentIteration: true,
		Historical:       false,
	}
}

// sortExecutionInstances stably sorts instances by iteration, then attempt, then
// bead id. Port of TS compareExecutionInstances (localeCompare approximated by
// byte comparison).
func sortExecutionInstances(instances []RunExecutionInstance) {
	sort.SliceStable(instances, func(i, j int) bool {
		if d := iterationOrder(instances[i].Iteration) - iterationOrder(instances[j].Iteration); d != 0 {
			return d < 0
		}
		if d := attemptOrder(instances[i].Attempt) - attemptOrder(instances[j].Attempt); d != 0 {
			return d < 0
		}
		return strings.Compare(instances[i].BeadID, instances[j].BeadID) < 0
	})
}

// attemptSummaryFor derives a node's attempt summary. Port of TS attemptSummaryFor.
func attemptSummaryFor(instances []RunExecutionInstance, beads []runSnapshotBead) RunAttemptSummary {
	attemptCount := attemptCountFor(instances)
	activeAttempt, hasActive := activeAttemptFor(instances)
	badgeLabel, hasBadge := attemptBadgeFor(beads)
	if attemptCount == 0 && !hasBadge {
		return RunAttemptSummary{Kind: "none"}
	}
	count := attemptCount
	if count < 1 {
		count = 1
	}
	badge := RunAttemptBadge{Kind: "count-only"}
	if hasBadge {
		badge = RunAttemptBadge{Kind: "bounded", Label: badgeLabel}
	}
	active := RunAttemptActive{Kind: "idle"}
	if hasActive {
		active = RunAttemptActive{Kind: "running", Value: activeAttempt}
	}
	return RunAttemptSummary{Kind: "tracked", Count: count, Badge: badge, Active: active}
}

// attemptBadgeFor derives the bounded attempt badge from gc.max_attempts. Port of
// TS attemptBadgeFor.
func attemptBadgeFor(beads []runSnapshotBead) (string, bool) {
	maxAttempts, hasMax := 0, false
	for _, bead := range beads {
		if v, ok := positiveIntegerMeta(bead, beadmeta.MaxAttemptsMetadataKey); ok {
			maxAttempts, hasMax = v, true
			break
		}
	}
	if !hasMax {
		return "", false
	}
	attempts := map[int]bool{}
	for _, bead := range beads {
		if v, ok := attemptFor(bead); ok {
			attempts[v] = true
		}
	}
	size := len(attempts)
	if size < 1 {
		size = 1
	}
	return strconv.Itoa(size) + "/" + strconv.Itoa(maxAttempts), true
}

func attemptCountFor(instances []RunExecutionInstance) int {
	attempts := map[int]bool{}
	for _, inst := range instances {
		if v, ok := attemptValue(inst.Attempt); ok {
			attempts[v] = true
		}
	}
	return len(attempts)
}

func activeAttemptFor(instances []RunExecutionInstance) (int, bool) {
	for _, inst := range instances {
		if isRunningStatus(inst.Status) {
			return attemptValue(inst.Attempt)
		}
	}
	return 0, false
}

// instanceLabel renders an instance's iteration/attempt label. Port of TS
// instanceLabel.
func instanceLabel(iteration int, hasIteration bool, attempt int, hasAttempt bool) string {
	if hasIteration && hasAttempt {
		return "iteration " + strconv.Itoa(iteration) + ", attempt " + strconv.Itoa(attempt)
	}
	if hasIteration {
		return "iteration " + strconv.Itoa(iteration)
	}
	if hasAttempt {
		return "attempt " + strconv.Itoa(attempt)
	}
	return "base"
}

// runNodeScope renders a group's node scope. Port of TS runNodeScope ("" =
// undefined → run).
func runNodeScope(scopeRef string) RunNodeScope {
	if scopeRef == "" {
		return RunNodeScope{Kind: "run"}
	}
	return RunNodeScope{Kind: "scoped", Ref: scopeRef}
}

// iterationSummaryFor renders a node's iteration summary. Port of TS
// iterationSummaryFor.
func iterationSummaryFor(visibleIteration int, hasVisibleIteration bool, iterationCount int, loopControlNodeID string) RunIterationSummary {
	if !hasVisibleIteration || iterationCount == 0 {
		return RunIterationSummary{Kind: "single"}
	}
	control := RunIterationControl{Kind: "unknown"}
	if loopControlNodeID != "" {
		control = RunIterationControl{Kind: "known", ID: loopControlNodeID}
	}
	return RunIterationSummary{
		Kind:             "stacked",
		VisibleIteration: visibleIteration,
		IterationCount:   iterationCount,
		Control:          control,
	}
}

func iterationState(value int, has bool) RunIteration {
	if !has {
		return RunIteration{Kind: "base"}
	}
	return RunIteration{Kind: "loop", Value: value}
}

func attemptState(value int, has bool) RunAttempt {
	if !has {
		return RunAttempt{Kind: "untracked"}
	}
	return RunAttempt{Kind: "attempt", Value: value}
}

// sessionState renders an instance's session attachment. Port of TS sessionState.
func sessionState(status string, link RunSessionLink, hasLink bool) RunSessionAttachment {
	if hasLink {
		return RunSessionAttachment{Kind: "attached", Link: link, Streamable: false}
	}
	reason := "session_unresolved"
	if status == "pending" || status == "ready" {
		reason = "not_started"
	}
	return RunSessionAttachment{Kind: "none", Reason: reason}
}

func iterationValue(iteration RunIteration) (int, bool) {
	if iteration.Kind == "loop" {
		return iteration.Value, true
	}
	return 0, false
}

func attemptValue(attempt RunAttempt) (int, bool) {
	if attempt.Kind == "attempt" {
		return attempt.Value, true
	}
	return 0, false
}

func iterationOrder(iteration RunIteration) int {
	if v, ok := iterationValue(iteration); ok {
		return v
	}
	return 0
}

func attemptOrder(attempt RunAttempt) int {
	if v, ok := attemptValue(attempt); ok {
		return v
	}
	return 0
}

func maxKey(set map[int]bool) int {
	first := true
	best := 0
	for k := range set {
		if first || k > best {
			best = k
			first = false
		}
	}
	return best
}
