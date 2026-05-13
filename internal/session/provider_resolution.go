package session

import "strings"

// UseAgentTemplateForProviderResolution reports whether a session should
// resolve provider options through its agent template instead of treating the
// persisted Template/Provider fields as raw provider names.
func UseAgentTemplateForProviderResolution(sessionKind string, metadata map[string]string, persistedProvider, templateProvider string, templateFound bool) bool {
	sessionKind = strings.TrimSpace(sessionKind)
	switch sessionKind {
	case "provider":
		return false
	case "agent":
		return true
	}
	if metadata == nil {
		return true
	}
	if strings.TrimSpace(metadata["agent_name"]) != "" ||
		strings.TrimSpace(metadata[NamedSessionMetadataKey]) == "true" {
		return true
	}
	if strings.TrimSpace(metadata["session_origin"]) == "manual" {
		return false
	}
	if !templateFound {
		return false
	}
	persistedProvider = strings.TrimSpace(persistedProvider)
	templateProvider = strings.TrimSpace(templateProvider)
	if persistedProvider != "" && templateProvider != "" && persistedProvider != templateProvider {
		return false
	}
	return true
}
