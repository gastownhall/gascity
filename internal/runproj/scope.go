package runproj

import (
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// scopeRefRe validates a scope ref. Port of TS SCOPE_REF_RE
// (shared/src/run-detail.ts).
var scopeRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)

// runScopeWithStoreRef is the resolved scope. Port of TS RunScopeWithStoreRef.
type runScopeWithStoreRef struct {
	scopeKind    string
	scopeRef     string
	rootStoreRef string
}

// parseRunScopeKind accepts only "city" or "rig". Port of TS parseRunScopeKind.
func parseRunScopeKind(value string) (string, bool) {
	if value == "city" || value == "rig" {
		return value, true
	}
	return "", false
}

// fromRootMetadataScope resolves the lane scope from root metadata, with the
// gc.root_store_ref fallback. Port of TS fromRootMetadataScope. The bool mirrors
// TS's `null`.
func fromRootMetadataScope(metadata map[string]string) (runScopeWithStoreRef, bool) {
	rootStoreRef := stringValueOrEmpty(metadata[beadmeta.RootStoreRefMetadataKey])

	// Primary: explicit gc.scope_kind / gc.scope_ref pair.
	scopeKind, kindOK := parseRunScopeKind(metadata[beadmeta.ScopeKindMetadataKey])
	scopeRef := stringValueOrEmpty(metadata[beadmeta.ScopeRefMetadataKey])
	if kindOK && scopeRef != "" && scopeRefRe.MatchString(scopeRef) {
		rsr := rootStoreRef
		if rsr == "" {
			rsr = scopeKind + ":" + scopeRef
		}
		return runScopeWithStoreRef{scopeKind: scopeKind, scopeRef: scopeRef, rootStoreRef: rsr}, true
	}

	// Fallback (gascity-dashboard-km0w): recover scope from gc.root_store_ref.
	if rootStoreRef == "" {
		return runScopeWithStoreRef{}, false
	}
	parsedKind, parsedRef, ok := fromStoreRef(rootStoreRef)
	if !ok || !scopeRefRe.MatchString(parsedRef) {
		return runScopeWithStoreRef{}, false
	}
	return runScopeWithStoreRef{scopeKind: parsedKind, scopeRef: parsedRef, rootStoreRef: rootStoreRef}, true
}

// fromStoreRef parses a "<kind>:<ref>" store ref. Port of TS fromStoreRef.
func fromStoreRef(rootStoreRef string) (kind, ref string, ok bool) {
	value := stringValueOrEmpty(rootStoreRef)
	if value == "" {
		return "", "", false
	}
	colon := strings.IndexByte(value, ':')
	if colon <= 0 || colon >= len(value)-1 {
		return "", "", false
	}
	parsedKind, kindOK := parseRunScopeKind(value[:colon])
	parsedRef := stringValueOrEmpty(value[colon+1:])
	if !kindOK || parsedRef == "" {
		return "", "", false
	}
	return parsedKind, parsedRef, true
}

// stringValueOrEmpty trims a value; an all-whitespace or empty value becomes "".
// Mirrors the TS run-scope stringValue (which returns null for empty). It
// delegates to nonEmpty so the JS-faithful trim (String.prototype.trim(): BOM
// stripped, NEL kept) is uniform with the rest of the package rather than
// diverging on Go's unicode.IsSpace.
func stringValueOrEmpty(value string) string {
	return nonEmpty(value)
}
