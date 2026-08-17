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

// UpdatedAliasMetadataFromInfo is the Info-fed sibling of UpdatedAliasMetadata:
// it computes the byte-identical alias/alias_history mutations from the projected
// Info.Alias and Info.AliasHistory. Those fields equal metadata["alias"] (verbatim)
// and AliasHistory(metadata) respectively, so a caller holding a projected Info in
// place of the raw metadata map produces the same result the raw form would.
func UpdatedAliasMetadataFromInfo(info Info, nextAlias string) map[string]string {
	currentAlias := strings.TrimSpace(info.Alias)
	history := info.AliasHistory
	if currentAlias != "" && currentAlias != nextAlias {
		history = append([]string{currentAlias}, history...)
	}
	history = normalizeAliasList(history, nextAlias)
	return map[string]string{
		"alias":                 strings.TrimSpace(nextAlias),
		aliasHistoryMetadataKey: strings.Join(history, ","),
	}
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
