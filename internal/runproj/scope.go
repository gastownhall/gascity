package runproj

import (
	"regexp"
	"strings"
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
	rootStoreRef := stringValueOrEmpty(metadata["gc.root_store_ref"])

	// Primary: explicit gc.scope_kind / gc.scope_ref pair.
	scopeKind, kindOK := parseRunScopeKind(metadata["gc.scope_kind"])
	scopeRef := stringValueOrEmpty(metadata["gc.scope_ref"])
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
// Mirrors the TS run-scope stringValue (which returns null for empty).
func stringValueOrEmpty(value string) string {
	return strings.TrimSpace(value)
}
