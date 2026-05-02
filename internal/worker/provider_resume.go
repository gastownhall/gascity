package worker

import (
	"regexp"
	"strings"
)

var codexThreadIDPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func derivedResumeSessionKey(provider, providerSessionID string) string {
	providerSessionID = strings.TrimSpace(providerSessionID)
	if providerSessionID == "" {
		return ""
	}
	providerFamily := strings.ToLower(strings.TrimSpace(provider))
	if strings.Contains(providerFamily, "opencode") {
		return providerSessionID
	}
	if !strings.Contains(providerFamily, "codex") {
		return ""
	}
	matches := codexThreadIDPattern.FindAllString(providerSessionID, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}
