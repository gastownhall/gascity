package runproj

import (
	"strings"
	"unicode"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// resolveRunFormulaIdentityLane resolves a run group's formula name for the
// "lane" mode used by the summary builder. Faithful port of the lane path
// through TS resolveRunFormulaIdentity (shared/src/runs/formula-name.ts): the
// summary uses only mode='lane' with no formulaDetail, so this collapses to the
// metadata-name path followed by the graph.v2 title fallback. The bool mirrors
// TS's `name: string | null`.
//
// Metadata path (runFormulaMetadataName, mode='lane'): first non-empty
// pr_review.workflow_formula across issues, else first non-empty gc.formula.
// Title fallback (runFormulaTitleFallback, mode='lane'): only for a root that
// carries gc.formula_contract='graph.v2' AND gc.run_target, is non-terminal,
// and whose trimmed title starts with "mol-".
func resolveRunFormulaIdentityLane(root *runIssue, issues []runIssue) (string, bool) {
	if name := metadataNonEmptyAcrossIssues(issues, "pr_review.workflow_formula"); name != "" {
		return name, true
	}
	if name := metadataNonEmptyAcrossIssues(issues, beadmeta.FormulaMetadataKey); name != "" {
		return name, true
	}

	if name, ok := runFormulaTitleFallbackLane(root); ok {
		return name, true
	}
	return "", false
}

// metadataNonEmptyAcrossIssues returns the first trimmed non-empty value for key
// across issues. Mirrors formula-name.ts metadataString (which uses rootMeta /
// nonEmpty — trimmed, empty-skipping).
func metadataNonEmptyAcrossIssues(issues []runIssue, key string) string {
	for _, i := range issues {
		if v := nonEmpty(i.metadata[key]); v != "" {
			return v
		}
	}
	return ""
}

// runFormulaTitleFallbackLane is the lane-mode graph.v2 title fallback.
// Port of TS runFormulaTitleFallback for mode='lane'.
func runFormulaTitleFallbackLane(root *runIssue) (string, bool) {
	if root == nil {
		return "", false
	}
	if nonEmpty(root.metadata[beadmeta.FormulaContractMetadataKey]) != "graph.v2" ||
		nonEmpty(root.metadata[beadmeta.RunTargetMetadataKey]) == "" ||
		isTerminalRunRootStatus(root.status) {
		return "", false
	}
	title := nonEmpty(root.title)
	if title == "" {
		return "", false
	}
	// mode === 'lane' && !title.startsWith('mol-') ? null : title
	if !strings.HasPrefix(title, "mol-") {
		return "", false
	}
	return title, true
}

// isTerminalRunRootStatus reports whether a root status is terminal.
// Port of TS isTerminalRunRootStatus.
func isTerminalRunRootStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "closed", "completed", "done", "failed", "skipped":
		return true
	default:
		return false
	}
}

// nonEmpty trims a value and returns "" for empty/whitespace. Port of TS
// nonEmpty (bead-fields.ts), which trims the ECMAScript String.prototype.trim()
// whitespace set. That set differs from Go's unicode.IsSpace in exactly two
// codepoints with opposite membership: JS strips U+FEFF (ZWNBSP/BOM) but NOT
// U+0085 (NEL). jsTrimCut applies that delta so the trim is byte-faithful.
func nonEmpty(value string) string {
	return strings.TrimFunc(value, jsTrimCut)
}

// jsTrimCut reports whether r is in the ECMAScript String.prototype.trim()
// whitespace set: unicode.IsSpace, minus U+0085, plus U+FEFF.
func jsTrimCut(r rune) bool {
	switch r {
	case '\u0085': // NEL: Go's unicode.IsSpace trims it, JS does not.
		return false
	case '\ufeff': // ZWNBSP/BOM: JS trims it, Go's unicode.IsSpace does not.
		return true
	default:
		return unicode.IsSpace(r)
	}
}
