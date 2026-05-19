package session

import "strings"

const aliasHistoryMetadataKey = "alias_history"

// AliasHistory returns previously assigned aliases preserved in session
// metadata. Empty values and duplicates are removed.
func AliasHistory(metadata map[string]string) []string {
	if len(metadata) == 0 {
		return nil
	}
	return normalizeAliasList(strings.Split(metadata[aliasHistoryMetadataKey], ","), "")
}

// UpdatedAliasMetadata returns the metadata mutations needed to set the current
// alias while preserving prior aliases for internal delivery continuity.
func UpdatedAliasMetadata(metadata map[string]string, nextAlias string) map[string]string {
	currentAlias := strings.TrimSpace(metadata["alias"])
	history := AliasHistory(metadata)
	if currentAlias != "" && currentAlias != nextAlias {
		history = append([]string{currentAlias}, history...)
	}
	history = normalizeAliasList(history, nextAlias)
	return map[string]string{
		"alias":                 strings.TrimSpace(nextAlias),
		aliasHistoryMetadataKey: strings.Join(history, ","),
	}
}

// ReleaseClaimedIdentityOnCloseState reports whether closing with the given
// short stateCode should release the bead's runtime-identity metadata
// (alias, agent_name). True for terminal states a bead cannot be reopened
// from — its claim on a pool slot or alias must not block fresh spawns.
// Mirrors the not-eligible list in closedNamedSessionReopenEligible so any
// close that won't be reopened also frees its identity.
func ReleaseClaimedIdentityOnCloseState(stateCode string) bool {
	switch strings.TrimSpace(stateCode) {
	case "gc_swept", "orphaned", "stale-session", "duplicate", "duplicate-repair", "reconfigured", string(StateFailedCreate):
		return true
	}
	return false
}

// ReleaseClaimedIdentityPatch returns metadata mutations that clear the
// bead's runtime-identity claim (alias, agent_name) while preserving the
// previous alias in alias_history so delivery continuity for mail and
// orphan recovery still finds prior routes. Pass the bead's current
// metadata so the helper can move the live alias into history.
//
// Apply alongside ClosePatch on terminal close states (see
// ReleaseClaimedIdentityOnCloseState). Without this, a closed pool bead
// keeps its alias metadata indefinitely; if a stale cache view of the
// bead lingers as open, the alias-availability check still matches it
// and rejects the next spawn under the same slot.
func ReleaseClaimedIdentityPatch(metadata map[string]string) map[string]string {
	patch := map[string]string{
		"alias":      "",
		"agent_name": "",
	}
	currentAlias := strings.TrimSpace(metadata["alias"])
	if currentAlias == "" {
		return patch
	}
	history := AliasHistory(metadata)
	history = append([]string{currentAlias}, history...)
	history = normalizeAliasList(history, "")
	patch[aliasHistoryMetadataKey] = strings.Join(history, ",")
	return patch
}

func normalizeAliasList(values []string, exclude string) []string {
	exclude = strings.TrimSpace(exclude)
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || value == exclude || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
