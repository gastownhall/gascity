package runproj

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// ── bead-fields.ts ──────────────────────────────────────────────────────────

// beadMeta returns the trimmed string metadata value for key, or "" when absent
// or whitespace. Port of TS meta() (the metadata is always string-typed in Go,
// so the typeof guard is unconditional). "" stands in for TS's undefined; callers
// test `!= ""`.
func beadMeta(b runSnapshotBead, key string) string {
	return nonEmpty(b.metadata[key])
}

// normalizedStepRef resolves a bead's step ref: gc.step_ref metadata, else the
// step_ref column. Port of TS normalizedStepRef ("" mirrors null).
func normalizedStepRef(b runSnapshotBead) string {
	if ref := beadMeta(b, beadmeta.StepRefMetadataKey); ref != "" {
		return ref
	}
	return nonEmpty(b.stepRef)
}

// iterationFor resolves a bead's loop iteration. Port of TS iterationFor. The
// bool mirrors TS's `number | undefined`.
func iterationFor(b runSnapshotBead) (int, bool) {
	if v, ok := numericMeta(b, beadmeta.IterationMetadataKey); ok {
		return v, true
	}
	if v, ok := numericRefSegment(b, "iteration"); ok {
		return v, true
	}
	return numericRefSegment(b, "run")
}

// attemptFor resolves a bead's attempt number. Port of TS attemptFor.
func attemptFor(b runSnapshotBead) (int, bool) {
	if v, ok := numericMeta(b, beadmeta.AttemptMetadataKey); ok {
		return v, true
	}
	if b.attempt != nil {
		if v, ok := numericFieldInt(*b.attempt); ok {
			return v, true
		}
	}
	return numericRefSegment(b, "attempt")
}

// positiveIntegerMeta returns a strictly-positive integer metadata value.
// Port of TS positiveIntegerMeta.
func positiveIntegerMeta(b runSnapshotBead, key string) (int, bool) {
	return numericMeta(b, key)
}

var numericFieldRe = regexp.MustCompile(`^[1-9]\d*$`)

// externalizeID rewrites whole-word "ralph" (case-insensitive) to "check-loop".
// Port of TS externalizeId. Go RE2 lacks the lookahead the TS pattern uses, so
// the word boundaries are matched manually (the leading boundary char is
// preserved, the trailing boundary is not consumed — overlapping boundaries
// like "ralph.ralph" both rewrite).
func externalizeID(s string) string {
	return rewriteRalph(s, "check-loop")
}

// rewriteRalph replaces each whole-word "ralph" (delimited by string edges or
// non-alphanumeric chars, case-insensitive) with replacement.
func rewriteRalph(s, replacement string) string {
	const word = "ralph"
	if !containsRalphWord(s) {
		return s
	}
	lower := strings.ToLower(s)
	var b strings.Builder
	i := 0
	for i < len(s) {
		if lower[i] == 'r' && strings.HasPrefix(lower[i:], word) {
			beforeOK := i == 0 || !isASCIIAlnum(s[i-1])
			after := i + len(word)
			afterOK := after >= len(s) || !isASCIIAlnum(s[after])
			if beforeOK && afterOK {
				b.WriteString(replacement)
				i = after
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// containsRalphWord reports whether s contains a whole-word "ralph". Port of the
// boolean test in TS externalizeDisplayText.
func containsRalphWord(s string) bool {
	const word = "ralph"
	lower := strings.ToLower(s)
	for i := 0; i+len(word) <= len(lower); i++ {
		if !strings.HasPrefix(lower[i:], word) {
			continue
		}
		beforeOK := i == 0 || !isASCIIAlnum(s[i-1])
		after := i + len(word)
		afterOK := after >= len(s) || !isASCIIAlnum(s[after])
		if beforeOK && afterOK {
			return true
		}
	}
	return false
}

func isASCIIAlnum(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func numericRefSegment(b runSnapshotBead, marker string) (int, bool) {
	ref := normalizedStepRef(b)
	if ref == "" {
		return 0, false
	}
	parts := strings.Split(ref, ".")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] != marker {
			continue
		}
		if v, ok := numericFieldString(parts[i+1]); ok {
			return v, true
		}
	}
	return 0, false
}

func numericMeta(b runSnapshotBead, key string) (int, bool) {
	return numericFieldString(beadMeta(b, key))
}

// numericFieldString parses a strictly-positive decimal integer with no leading
// zero. Port of TS numericField (string arm).
func numericFieldString(value string) (int, bool) {
	if !numericFieldRe.MatchString(value) {
		return 0, false
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	// Number.isSafeInteger guard (< 2^53). int is 64-bit here, so any value that
	// parsed cleanly and matched the regex is well within range; the explicit
	// bound keeps parity with the TS check.
	if int64(parsed) > 1<<53-1 {
		return 0, false
	}
	return parsed, true
}

// numericFieldInt mirrors TS numericField for an already-numeric value: a
// positive integer is accepted as-is.
func numericFieldInt(value int) (int, bool) {
	if value > 0 {
		return value, true
	}
	return 0, false
}

// ── status.ts ───────────────────────────────────────────────────────────────

// presentationStatus maps a bead's raw status (+ gc.outcome) to a node status.
// Port of TS presentationStatus.
func presentationStatus(b runSnapshotBead) string {
	raw := strings.ToLower(nonEmpty(b.status))
	outcome := strings.ToLower(beadMeta(b, beadmeta.OutcomeMetadataKey))
	if raw == "closed" || raw == "completed" || raw == "done" {
		if outcome == "fail" || outcome == "failed" {
			return "failed"
		}
		if outcome == "skipped" {
			return "skipped"
		}
		if outcome == "canceled" {
			return "canceled"
		}
		return "completed"
	}
	if raw == "in_progress" || raw == "active" || raw == "running" {
		return "active"
	}
	switch raw {
	case "blocked":
		return "blocked"
	case "ready":
		return "ready"
	case "failed":
		return "failed"
	case "skipped":
		return "skipped"
	case "canceled":
		return "canceled"
	}
	return "pending"
}

// aggregateStatus folds a node's instances into one status. Port of TS
// aggregateStatus.
func aggregateStatus(instances []RunExecutionInstance, visible *RunExecutionInstance) string {
	for _, inst := range instances {
		if isRunningStatus(inst.Status) {
			return "active"
		}
	}
	if visible != nil && visible.Status != "" {
		return visible.Status
	}
	return "pending"
}

// isRunningStatus reports whether status is a running state. Port of TS
// isRunningStatus.
func isRunningStatus(status string) bool {
	return status == "active" || status == "running"
}

// ── node-shape.ts ───────────────────────────────────────────────────────────

// hiddenConstructs are construct kinds rendered as badges, not graph nodes.
// Port of TS HIDDEN_CONSTRUCTS (plus the 'control' case in isHiddenConstruct).
var hiddenConstructs = map[string]bool{
	"scope-check":  true,
	"run-finalize": true,
	"spec":         true,
}

// isHiddenConstruct reports whether a construct kind is hidden from the graph.
// Port of TS isHiddenConstruct.
func isHiddenConstruct(kind string) bool {
	return hiddenConstructs[kind] || kind == "control"
}

// semanticNodeIDFor resolves a bead's semantic node id. Port of TS
// semanticNodeIdFor.
func semanticNodeIDFor(b runSnapshotBead, rootBeadID string) string {
	beadID := nonEmpty(b.id)
	if beadID != "" && beadID == rootBeadID {
		return rootBeadID
	}
	if explicit := explicitLogicalBeadID(b); explicit != "" {
		return externalizeID(explicit)
	}
	if stepID := beadMeta(b, beadmeta.StepIDMetadataKey); stepID != "" {
		return externalizeID(stepID)
	}
	if ref := normalizedStepRef(b); ref != "" {
		if semanticID, ok := semanticIDFromStepRef(ref); ok {
			return externalizeID(semanticID)
		}
	}
	if beadID != "" {
		return externalizeID(beadID)
	}
	return externalizeID("run-node")
}

// hiddenBadgeTargetFor resolves the visible node a hidden bead's badge attaches
// to. Port of TS hiddenBadgeTargetFor ("" mirrors null).
func hiddenBadgeTargetFor(b runSnapshotBead, rootBeadID string) string {
	if constructKindFor(b, rootBeadID) == "run-finalize" {
		return rootBeadID
	}
	if controlRef := beadMeta(b, beadmeta.ControlForMetadataKey); controlRef != "" {
		if target, ok := semanticIDFromControlRef(controlRef); ok {
			return externalizeID(target)
		}
	}
	ref := normalizedStepRef(b)
	if ref == "" {
		return ""
	}
	if target, ok := semanticIDFromControlRef(ref); ok {
		return externalizeID(target)
	}
	return ""
}

// constructKindFor maps a bead's raw kind to a RunConstructKind. Port of TS
// constructKindFor.
func constructKindFor(b runSnapshotBead, rootBeadID string) string {
	beadID := nonEmpty(b.id)
	if beadID != "" && beadID == rootBeadID {
		return "run-root"
	}
	switch rawKind(b) {
	case "ralph":
		return "check-loop"
	case "retry":
		return "retry"
	case "scope", "epic", "body":
		return "scope"
	case "fanout":
		return "fanout"
	case "condition":
		return "condition"
	case "expand", "expansion":
		return "expansion"
	case "scope-check":
		return "scope-check"
	case "run-finalize":
		return "run-finalize"
	case "spec":
		return "spec"
	case "cleanup":
		return "control"
	default:
		return "step"
	}
}

// externalKindFor resolves the display kind. Port of TS externalKindFor.
func externalKindFor(b runSnapshotBead, constructKind string) string {
	if constructKind == "check-loop" {
		return "check-loop"
	}
	kind := rawKind(b)
	if kind == "ralph" {
		return "check-loop"
	}
	if kind != "" {
		return kind
	}
	return constructKind
}

// displayTitleFor resolves a node's display title. Port of TS displayTitleFor.
func displayTitleFor(b runSnapshotBead, fallback string) string {
	title := nonEmpty(b.title)
	if title == "" {
		title = strings.NewReplacer("-", " ", "_", " ").Replace(fallback)
	}
	return externalizeDisplayText(title)
}

// badgeLabelFor maps a construct kind to a badge label. Port of TS badgeLabelFor.
func badgeLabelFor(kind string) string {
	switch kind {
	case "scope-check":
		return "scope check"
	case "run-finalize":
		return "finalize"
	default:
		return strings.ReplaceAll(kind, "-", " ")
	}
}

// loopControlNodeIDFor resolves the loop-control node id for a bead. Port of TS
// loopControlNodeIdFor ("" mirrors undefined).
func loopControlNodeIDFor(b runSnapshotBead) string {
	scopeRef := beadMeta(b, beadmeta.ScopeRefMetadataKey)
	if scopeRef == "" {
		scopeRef = nonEmpty(b.scopeRef)
	}
	if scopeRef != "" {
		if id := loopControlIDFromRuntimeRef(scopeRef, []string{"iteration", "run"}); id != "" {
			return id
		}
	}
	ref := normalizedStepRef(b)
	if ref == "" {
		return ""
	}
	return loopControlIDFromRuntimeRef(ref, []string{"iteration"})
}

// rawKind resolves a bead's raw kind. Port of TS rawKind.
func rawKind(b runSnapshotBead) string {
	if k := beadMeta(b, beadmeta.KindMetadataKey); k != "" {
		return k
	}
	if k := beadMeta(b, beadmeta.OriginalKindMetadataKey); k != "" {
		return k
	}
	return nonEmpty(b.kind)
}

// explicitLogicalBeadID resolves the explicit logical bead id. Mirrors the TS
// `meta(bead, 'gc.logical_bead_id') ?? nonEmpty(bead.logical_bead_id)` idiom.
func explicitLogicalBeadID(b runSnapshotBead) string {
	if v := beadMeta(b, beadmeta.LogicalBeadIDMetadataKey); v != "" {
		return v
	}
	return nonEmpty(b.logicalBeadID)
}

// semanticIDFromStepRef extracts a semantic id from a step ref. Port of TS
// semanticIdFromStepRef. The bool mirrors TS's undefined.
func semanticIDFromStepRef(ref string) (string, bool) {
	parts := splitNonEmpty(ref, ".")
	if len(parts) == 0 {
		return "", false
	}
	semanticParts := stripRuntimeSuffix(parts)
	iterationIndex := lastIndexOf(semanticParts, "iteration")
	if iterationIndex >= 0 &&
		iterationIndex < len(semanticParts)-2 &&
		isPositiveIntegerStr(at(semanticParts, iterationIndex+1)) {
		return at(semanticParts, len(semanticParts)-1), true
	}
	if iterationIndex == len(semanticParts)-2 &&
		isPositiveIntegerStr(at(semanticParts, iterationIndex+1)) {
		// TS reads semanticParts[iterationIndex-1] with PLAIN bracket indexing, so
		// iterationIndex==0 yields semanticParts[-1] === undefined (NOT .at(-1)).
		// A "" here means out of range → undefined, so the caller falls through to
		// the bead-id/run-node id instead of grouping on an empty semantic id.
		prev := at(semanticParts, iterationIndex-1)
		if prev == "" {
			return "", false
		}
		return prev, true
	}
	last := at(semanticParts, len(semanticParts)-1)
	if last == "" {
		return "", false
	}
	return last, true
}

// semanticIDFromControlRef strips the scope-check suffix, then resolves the
// semantic id. Port of TS semanticIdFromControlRef.
func semanticIDFromControlRef(ref string) (string, bool) {
	return semanticIDFromStepRef(stripScopeCheckSuffix(ref))
}

func stripScopeCheckSuffix(ref string) string {
	ref = strings.TrimSuffix(ref, "-scope-check")
	ref = strings.TrimSuffix(ref, ".scope-check")
	return ref
}

// stripRuntimeSuffix drops a trailing "<marker>.<n>" runtime segment. Port of TS
// stripRuntimeSuffix.
func stripRuntimeSuffix(parts []string) []string {
	if len(parts) < 2 {
		return parts
	}
	marker := parts[len(parts)-2]
	value := parts[len(parts)-1]
	if value != "" && marker != "" && isPositiveIntegerStr(value) &&
		(marker == "attempt" || marker == "run" || marker == "check" || marker == "eval") {
		return parts[:len(parts)-2]
	}
	return parts
}

// loopControlIDFromRuntimeRef finds the control id preceding a "<marker>.<n>"
// segment. Port of TS loopControlIdFromRuntimeRef ("" mirrors undefined).
func loopControlIDFromRuntimeRef(ref string, markers []string) string {
	parts := splitNonEmpty(ref, ".")
	for _, marker := range markers {
		markerIndex := -1
		for index, part := range parts {
			if part == marker && index+1 < len(parts) && isPositiveIntegerStr(parts[index+1]) {
				markerIndex = index
				break
			}
		}
		if markerIndex <= 0 {
			continue
		}
		controlID := parts[markerIndex-1]
		if controlID != "" {
			return externalizeID(controlID)
		}
		return ""
	}
	return ""
}

// isPositiveIntegerStr reports whether value is a canonical positive decimal
// integer. Port of TS isPositiveInteger (`String(Number.parseInt(value,10)) ===
// value && parsed > 0`). parseInt yields a float64, so beyond float64's exact
// integer range the String() round-trip no longer equals value and TS rejects;
// the exact-representability gate mirrors that (an Atoi overflow → undecodable →
// rejected, which TS also does for such huge values).
func isPositiveIntegerStr(value string) bool {
	if value == "" {
		return false
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return false
	}
	if strconv.Itoa(parsed) != value || parsed <= 0 {
		return false
	}
	return int64(float64(parsed)) == int64(parsed)
}

var (
	dashUnderscoreRunRe = regexp.MustCompile(`[-_]+`)
	// jsWhitespaceRunRe matches one-or-more characters of the ECMAScript `\s`
	// whitespace set (Go RE2 `\s` is ASCII-only), so the collapse mirrors the TS
	// `.replace(/\s+/g, ' ')`.
	jsWhitespaceRunRe = regexp.MustCompile(`[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+`)
)

// externalizeDisplayText rewrites whole-word "ralph" to "check loop" in display
// text. Port of TS externalizeDisplayText.
func externalizeDisplayText(value string) string {
	if !containsRalphWord(value) {
		return value
	}
	value = dashUnderscoreRunRe.ReplaceAllString(value, " ")
	value = rewriteRalph(value, "check loop")
	value = jsWhitespaceRunRe.ReplaceAllString(value, " ")
	return nonEmpty(value)
}

// ── small slice helpers (TS array idioms) ───────────────────────────────────

// splitNonEmpty splits s on sep and drops empty segments (TS `.filter(Boolean)`).
func splitNonEmpty(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func lastIndexOf(parts []string, want string) int {
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == want {
			return i
		}
	}
	return -1
}

// at returns parts[i] or "" when out of range (TS Array.prototype.at semantics
// for the indices we use, where a miss feeds an undefined the callers guard).
func at(parts []string, i int) string {
	if i < 0 || i >= len(parts) {
		return ""
	}
	return parts[i]
}
