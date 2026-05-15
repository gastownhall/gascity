package worker

import (
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

var codexThreadIDPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func derivedResumeSessionKey(provider, providerSessionID string) string {
	providerSessionID = strings.TrimSpace(providerSessionID)
	if providerSessionID == "" {
		return ""
	}
	providerFamily := sessionlog.ProviderFamily(provider)
	if providerFamily == "opencode" || providerFamily == "pi" {
		return providerSessionID
	}
	if providerFamily != "codex" {
		return ""
	}
	matches := codexThreadIDPattern.FindAllString(providerSessionID, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}
