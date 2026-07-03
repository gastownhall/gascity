package runproj

import (
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// runNodeGroup is one semantic node and the physical beads grouped under it.
// Port of TS RunNodeGroup (execution-instances.ts). scopeRef and
// loopControlNodeID use "" to mean undefined (groupOptional only ever yields a
// non-empty string or undefined).
type runNodeGroup struct {
	semanticNodeID    string
	title             string
	kind              string
	constructKind     string
	scopeRef          string
	loopControlNodeID string
	beads             []runSnapshotBead
}

// runBeadGroups is the output of groupRunBeads. Port of TS RunBeadGroups, with
// groups carried in first-seen semantic-id order.
type runBeadGroups struct {
	groups             []runNodeGroup
	physicalToSemantic map[string]string
	badgesByTarget     map[string][]RunControlBadge
}

// beadIdentity is a bead's resolved semantic identity. Port of TS BeadIdentity
// (hasDisambiguator mirrors `disambiguator: string | undefined`).
type beadIdentity struct {
	base             string
	disambiguator    string
	hasDisambiguator bool
	semanticNodeID   string
}

// groupRunBeads partitions a run's beads into semantic node groups, mapping
// physical bead ids to semantic ids and collecting hidden-construct badges.
// Port of TS groupRunBeads (groups.ts). Beads are addressed by index to mirror
// the TS Map keyed by object identity.
func groupRunBeads(beads []runSnapshotBead, rootBeadID string) runBeadGroups {
	physicalToSemantic := make(map[string]string)
	badgesByTarget := make(map[string][]RunControlBadge)
	physicalLogicalTargets := referencedPhysicalLogicalTargets(beads)
	identities := resolveBeadIdentities(beads, rootBeadID, physicalLogicalTargets)
	badgeTargetAliases := buildBadgeTargetAliases(beads, rootBeadID, identities, physicalLogicalTargets)

	grouped := make(map[string][]runSnapshotBead)
	var groupOrder []string

	for i := range beads {
		bead := beads[i]
		beadID := nonEmpty(bead.id)
		constructKind := constructKindFor(bead, rootBeadID)
		semanticNodeID := semanticNodeIDFor(bead, rootBeadID)
		if id, ok := identities[i]; ok {
			semanticNodeID = id.semanticNodeID
		}
		physicalToSemantic[beadID] = semanticNodeID

		if isHiddenConstruct(constructKind) {
			target := hiddenBadgeTargetFor(bead, rootBeadID)
			resolvedTarget, ok := resolveBadgeTarget(bead, rootBeadID, badgeTargetAliases, target)
			if ok {
				id := beadID
				if id == "" {
					id = resolvedTarget + "-" + constructKind
				}
				badgesByTarget[resolvedTarget] = append(badgesByTarget[resolvedTarget], RunControlBadge{
					ID:     id,
					Label:  badgeLabelFor(constructKind),
					Status: presentationStatus(bead),
				})
			}
			continue
		}

		if _, ok := grouped[semanticNodeID]; !ok {
			groupOrder = append(groupOrder, semanticNodeID)
		}
		grouped[semanticNodeID] = append(grouped[semanticNodeID], bead)
	}

	groups := make([]runNodeGroup, 0, len(groupOrder))
	for _, semanticNodeID := range groupOrder {
		groups = append(groups, buildRunNodeGroup(semanticNodeID, grouped[semanticNodeID], rootBeadID))
	}

	return runBeadGroups{
		groups:             groups,
		physicalToSemantic: physicalToSemantic,
		badgesByTarget:     badgesByTarget,
	}
}

// buildRunNodeGroup assembles one semantic node group. Port of TS
// buildRunNodeGroup.
func buildRunNodeGroup(semanticNodeID string, beads []runSnapshotBead, rootBeadID string) runNodeGroup {
	shapeBead := preferredShapeBead(beads, rootBeadID)
	constructKind := constructKindFor(shapeBead, rootBeadID)
	scopeRef := groupOptional(beads, shapeBead, func(b runSnapshotBead) string {
		if v := beadMeta(b, beadmeta.ScopeRefMetadataKey); v != "" {
			return v
		}
		return nonEmpty(b.scopeRef)
	})
	loopControlNodeID := groupOptional(beads, shapeBead, loopControlNodeIDFor)

	return runNodeGroup{
		semanticNodeID:    semanticNodeID,
		title:             displayTitleFor(shapeBead, semanticNodeID),
		kind:              externalKindFor(shapeBead, constructKind),
		constructKind:     constructKind,
		scopeRef:          scopeRef,
		loopControlNodeID: loopControlNodeID,
		beads:             beads,
	}
}

// preferredShapeBead picks the bead that drives a group's shape: highest
// construct priority, then lowest sort key. Port of TS preferredShapeBead (a
// stable sort, mirrored with sort.SliceStable). localeCompare is approximated by
// byte comparison, matching the P1 convention.
func preferredShapeBead(beads []runSnapshotBead, rootBeadID string) runSnapshotBead {
	if len(beads) == 0 {
		// buildRunNodeGroup is only ever called with a non-empty group; mirror the
		// TS throw defensively.
		panic("runproj: cannot build run node group from zero beads")
	}
	sorted := make([]runSnapshotBead, len(beads))
	copy(sorted, beads)
	sort.SliceStable(sorted, func(i, j int) bool {
		priorityDiff := constructPriority(constructKindFor(sorted[j], rootBeadID)) -
			constructPriority(constructKindFor(sorted[i], rootBeadID))
		if priorityDiff != 0 {
			return priorityDiff < 0
		}
		return strings.Compare(beadSortKey(sorted[i]), beadSortKey(sorted[j])) < 0
	})
	return sorted[0]
}

// groupOptional resolves an optional group field: the shape bead's value, else
// the first defined value across the sorted beads. Port of TS groupOptional ("" =
// undefined).
func groupOptional(beads []runSnapshotBead, shapeBead runSnapshotBead, resolve func(runSnapshotBead) string) string {
	if v := resolve(shapeBead); v != "" {
		return v
	}
	for _, b := range sortedBeads(beads) {
		if v := resolve(b); v != "" {
			return v
		}
	}
	return ""
}

// constructPriority ranks construct kinds for shape-bead selection. Port of TS
// constructPriority.
func constructPriority(kind string) int {
	switch kind {
	case "run-root":
		return 100
	case "check-loop":
		return 90
	case "retry":
		return 80
	case "condition", "fanout", "scope", "expansion":
		return 70
	case "step":
		return 10
	default: // control, run-finalize, scope-check, spec, unknown
		return 0
	}
}

// sortedBeads returns beads sorted by sort key. Port of TS sortedBeads.
func sortedBeads(beads []runSnapshotBead) []runSnapshotBead {
	sorted := make([]runSnapshotBead, len(beads))
	copy(sorted, beads)
	sort.SliceStable(sorted, func(i, j int) bool {
		return strings.Compare(beadSortKey(sorted[i]), beadSortKey(sorted[j])) < 0
	})
	return sorted
}

// beadSortKey builds a bead's deterministic sort key. Port of TS beadSortKey.
func beadSortKey(b runSnapshotBead) string {
	parts := make([]string, 0, 3)
	if v := nonEmpty(b.id); v != "" {
		parts = append(parts, v)
	}
	if v := normalizedStepRef(b); v != "" {
		parts = append(parts, v)
	}
	if v := nonEmpty(b.title); v != "" {
		parts = append(parts, v)
	}
	return strings.Join(parts, "\x00")
}

// resolveBeadIdentities computes each non-hidden bead's semantic identity,
// disambiguating only when a base id is shared. Port of TS resolveBeadIdentities.
func resolveBeadIdentities(beads []runSnapshotBead, rootBeadID string, physicalLogicalTargets map[string]bool) map[int]beadIdentity {
	partial := make(map[int]beadIdentity)
	identitiesByBase := make(map[string]map[string]bool)

	for i := range beads {
		bead := beads[i]
		if isHiddenConstruct(constructKindFor(bead, rootBeadID)) {
			continue
		}
		base := groupingBaseSemanticID(bead, rootBeadID, physicalLogicalTargets)
		disambiguator, hasDisambiguator := duplicateResolutionIdentity(bead, rootBeadID, base, physicalLogicalTargets)
		partial[i] = beadIdentity{base: base, disambiguator: disambiguator, hasDisambiguator: hasDisambiguator}

		identity := base
		if hasDisambiguator {
			identity = disambiguator
		}
		if identitiesByBase[base] == nil {
			identitiesByBase[base] = make(map[string]bool)
		}
		identitiesByBase[base][identity] = true
	}

	resolved := make(map[int]beadIdentity, len(beads))
	for i := range beads {
		id, ok := partial[i]
		if !ok {
			id = beadIdentity{base: semanticNodeIDFor(beads[i], rootBeadID)}
		}
		semanticNodeID := id.base
		if set := identitiesByBase[id.base]; len(set) > 1 && id.hasDisambiguator {
			semanticNodeID = id.disambiguator
		}
		id.semanticNodeID = semanticNodeID
		resolved[i] = id
	}
	return resolved
}

// groupingBaseSemanticID resolves a bead's grouping base id. Port of TS
// groupingBaseSemanticId.
func groupingBaseSemanticID(b runSnapshotBead, rootBeadID string, physicalLogicalTargets map[string]bool) string {
	beadID := nonEmpty(b.id)
	if beadID != "" && beadID == rootBeadID {
		return rootBeadID
	}
	if explicit := explicitLogicalBeadID(b); explicit != "" {
		return externalizeID(explicit)
	}
	constructKind := constructKindFor(b, rootBeadID)
	if (constructKind == "check-loop" || constructKind == "retry") && beadID != "" && physicalLogicalTargets[beadID] {
		return externalizeID(beadID)
	}
	return semanticNodeIDFor(b, rootBeadID)
}

// duplicateResolutionIdentity resolves the disambiguator candidate for a bead.
// Port of TS duplicateResolutionIdentity (bool mirrors undefined).
func duplicateResolutionIdentity(b runSnapshotBead, rootBeadID, base string, physicalLogicalTargets map[string]bool) (string, bool) {
	beadID := nonEmpty(b.id)
	if beadID != "" && physicalLogicalTargets[beadID] && externalizeID(beadID) == base {
		return base, true
	}
	return stableSemanticIdentity(b, rootBeadID)
}

// buildBadgeTargetAliases maps every alias that uniquely identifies one visible
// node to that node. Port of TS buildBadgeTargetAliases.
func buildBadgeTargetAliases(beads []runSnapshotBead, rootBeadID string, identities map[int]beadIdentity, physicalLogicalTargets map[string]bool) map[string]string {
	candidates := make(map[string]map[string]bool)

	for i := range beads {
		bead := beads[i]
		if isHiddenConstruct(constructKindFor(bead, rootBeadID)) {
			continue
		}
		id, hasIdentity := identities[i]
		resolvedTarget := semanticNodeIDFor(bead, rootBeadID)
		if hasIdentity {
			resolvedTarget = id.semanticNodeID
		}
		var identityPtr *beadIdentity
		if hasIdentity {
			identityPtr = &id
		}
		for _, alias := range visibleNodeAliases(bead, rootBeadID, identityPtr, physicalLogicalTargets) {
			if candidates[alias] == nil {
				candidates[alias] = make(map[string]bool)
			}
			candidates[alias][resolvedTarget] = true
		}
	}

	aliases := make(map[string]string)
	for alias, targets := range candidates {
		if len(targets) == 1 {
			for target := range targets {
				aliases[alias] = target
			}
		}
	}
	return aliases
}

// visibleNodeAliases lists the alias strings under which a visible node can be
// referenced. Port of TS visibleNodeAliases (undefined entries dropped, each
// externalized).
func visibleNodeAliases(b runSnapshotBead, rootBeadID string, identity *beadIdentity, physicalLogicalTargets map[string]bool) []string {
	resolvedTarget := semanticNodeIDFor(b, rootBeadID)
	base := groupingBaseSemanticID(b, rootBeadID, physicalLogicalTargets)
	if identity != nil {
		resolvedTarget = identity.semanticNodeID
		base = identity.base
	}

	var raw []string
	raw = append(raw, resolvedTarget)
	raw = append(raw, semanticNodeIDFor(b, rootBeadID))
	raw = append(raw, base)
	if identity != nil && identity.hasDisambiguator {
		raw = append(raw, identity.disambiguator)
	}
	if v, ok := stableSemanticIdentity(b, rootBeadID); ok {
		raw = append(raw, v)
	}
	if v := beadMeta(b, beadmeta.StepIDMetadataKey); v != "" {
		raw = append(raw, v)
	}
	if v, ok := fullStepRefIdentity(normalizedStepRef(b)); ok {
		raw = append(raw, v)
	}
	if v := nonEmpty(b.id); v != "" {
		raw = append(raw, v)
	}

	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, externalizeID(v))
	}
	return out
}

// referencedPhysicalLogicalTargets collects bead ids that another bead references
// as its logical bead id. Port of TS referencedPhysicalLogicalTargets.
func referencedPhysicalLogicalTargets(beads []runSnapshotBead) map[string]bool {
	beadIDs := make(map[string]bool)
	for i := range beads {
		if id := nonEmpty(beads[i].id); id != "" {
			beadIDs[id] = true
		}
	}
	targets := make(map[string]bool)
	for i := range beads {
		logical := explicitLogicalBeadID(beads[i])
		if logical != "" && beadIDs[logical] {
			targets[logical] = true
		}
	}
	return targets
}

// resolveBadgeTarget resolves the visible node a hidden bead's badge attaches to.
// Port of TS resolveBadgeTarget (bool mirrors a non-null result).
func resolveBadgeTarget(b runSnapshotBead, rootBeadID string, aliases map[string]string, fallback string) (string, bool) {
	if constructKindFor(b, rootBeadID) == "run-finalize" {
		return rootBeadID, true
	}
	for _, candidate := range hiddenBadgeTargetCandidates(b, fallback) {
		if target, ok := aliases[candidate]; ok && target != "" {
			return target, true
		}
	}
	if fallback != "" {
		return fallback, true
	}
	return "", false
}

// hiddenBadgeTargetCandidates lists the alias candidates a hidden badge resolves
// against. Port of TS hiddenBadgeTargetCandidates.
func hiddenBadgeTargetCandidates(b runSnapshotBead, fallback string) []string {
	var sources []string
	if v := beadMeta(b, beadmeta.ControlForMetadataKey); v != "" {
		sources = append(sources, v)
	}
	if v, ok := hiddenBadgeFullTargetFor(b); ok {
		sources = append(sources, v)
	}
	if fallback != "" {
		sources = append(sources, fallback)
	}

	var out []string
	for _, value := range sources {
		stripped := stripScopeCheckSuffix(value)
		out = append(out, externalizeID(value), externalizeID(stripped), externalizeID(externalizeID(stripped)))
	}
	return out
}

// stableSemanticIdentity resolves a bead's stable semantic identity. Port of TS
// stableSemanticIdentity (bool mirrors undefined).
func stableSemanticIdentity(b runSnapshotBead, rootBeadID string) (string, bool) {
	beadID := nonEmpty(b.id)
	if beadID != "" && beadID == rootBeadID {
		return rootBeadID, true
	}
	if explicit := explicitLogicalBeadID(b); explicit != "" {
		return externalizeID(explicit), true
	}
	if stepID := beadMeta(b, beadmeta.StepIDMetadataKey); stepID != "" {
		return externalizeID(stepID), true
	}
	return fullStepRefIdentity(normalizedStepRef(b))
}

// hiddenBadgeFullTargetFor resolves the full-ref target of a hidden badge. Port
// of TS hiddenBadgeFullTargetFor (bool mirrors undefined).
func hiddenBadgeFullTargetFor(b runSnapshotBead) (string, bool) {
	if controlFor := beadMeta(b, beadmeta.ControlForMetadataKey); controlFor != "" {
		return externalizeID(stripScopeCheckSuffix(controlFor)), true
	}
	return fullStepRefIdentity(stripScopeCheckSuffix(normalizedStepRef(b)))
}

// fullStepRefIdentity resolves the full (non-root-prefixed) step ref identity.
// Port of TS fullStepRefIdentity (bool mirrors undefined).
func fullStepRefIdentity(ref string) (string, bool) {
	clean := nonEmpty(ref)
	if clean == "" {
		return "", false
	}
	stripped := stripScopeCheckSuffix(clean)
	parts := splitNonEmpty(stripped, ".")
	if len(parts) == 0 {
		return "", false
	}
	if len(parts) == 1 {
		first := parts[0]
		if first == "" {
			first = stripped
		}
		return externalizeID(first), true
	}
	return externalizeID(strings.Join(parts[1:], ".")), true
}
