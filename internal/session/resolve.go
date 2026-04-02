package session

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// Resolution errors returned by ResolveSessionID.
var (
	ErrSessionNotFound = errors.New("session not found")
	ErrAmbiguous       = errors.New("ambiguous session identifier")
)

// ResolveSessionID resolves a user-provided identifier to a bead ID.
// It first attempts a direct store lookup; if the identifier exists as
// a session bead, it is returned immediately. Otherwise, it resolves against
// live identifiers: open exact session_name matches first, then open exact
// current alias matches, then the best open exact template/agent_name match,
// then open exact historical alias matches.
//
// Returns ErrSessionNotFound if no live match is found, or ErrAmbiguous
// (wrapped with details) if multiple sessions match the identifier.
func ResolveSessionID(store beads.Store, identifier string) (string, error) {
	return resolveSessionID(store, identifier, false)
}

// ResolveSessionIDAllowClosed is the read-only variant of ResolveSessionID.
// When no live identifier claims the requested identifier, it falls back to
// closed exact alias, alias_history, and session_name matches so historical
// sessions remain inspectable by their stable handles.
func ResolveSessionIDAllowClosed(store beads.Store, identifier string) (string, error) {
	return resolveSessionID(store, identifier, true)
}

func resolveSessionID(store beads.Store, identifier string, allowClosed bool) (string, error) {
	// Try direct store lookup first — works for any ID format.
	b, err := store.Get(identifier)
	if err == nil && b.Type == BeadType {
		return b.ID, nil
	}
	if err != nil && !errors.Is(err, beads.ErrNotFound) {
		return "", fmt.Errorf("looking up session %q: %w", identifier, err)
	}

	// Fall back to live alias/session_name resolution among session beads.
	all, err := store.ListByLabel(LabelSession, 0)
	if err != nil {
		return "", fmt.Errorf("listing sessions: %w", err)
	}

	var openSessionNameMatches []beads.Bead
	var openAliasMatches []beads.Bead
	var openTemplateMatches []beads.Bead
	var openAgentNameMatches []beads.Bead
	var openHistoricalAliasMatches []beads.Bead
	var closedSessionNameMatches []beads.Bead
	var closedAliasMatches []beads.Bead
	var closedHistoricalAliasMatches []beads.Bead
	for _, b := range all {
		if b.Type != BeadType {
			continue
		}
		alias := strings.TrimSpace(b.Metadata["alias"])
		sessionName := strings.TrimSpace(b.Metadata["session_name"])
		template := strings.TrimSpace(b.Metadata["template"])
		agentName := strings.TrimSpace(b.Metadata["agent_name"])
		if agentName == "" {
			agentName = sessionAgentNameFromLabels(b)
		}
		historicalAliasMatch := aliasHistoryContains(b.Metadata, identifier)
		if b.Status != "closed" {
			switch {
			case alias == identifier:
				openAliasMatches = append(openAliasMatches, b)
			case template == identifier:
				openTemplateMatches = append(openTemplateMatches, b)
			case agentName == identifier:
				openAgentNameMatches = append(openAgentNameMatches, b)
			case historicalAliasMatch:
				openHistoricalAliasMatches = append(openHistoricalAliasMatches, b)
			case sessionName == identifier:
				openSessionNameMatches = append(openSessionNameMatches, b)
			}
			continue
		}
		if !allowClosed {
			continue
		}
		switch {
		case alias == identifier:
			closedAliasMatches = append(closedAliasMatches, b)
		case historicalAliasMatch:
			closedHistoricalAliasMatches = append(closedHistoricalAliasMatches, b)
		case sessionName == identifier:
			closedSessionNameMatches = append(closedSessionNameMatches, b)
		}
	}

	for _, matches := range [][]beads.Bead{
		openSessionNameMatches,
		openAliasMatches,
	} {
		if len(matches) > 0 {
			return chooseSessionMatch(identifier, matches)
		}
	}
	if match, ok := choosePreferredTemplateMatch(openTemplateMatches, openAgentNameMatches); ok {
		return match.ID, nil
	}
	if len(openHistoricalAliasMatches) > 0 {
		return chooseSessionMatch(identifier, openHistoricalAliasMatches)
	}
	if !allowClosed {
		return "", fmt.Errorf("%w: %q", ErrSessionNotFound, identifier)
	}
	for _, matches := range [][]beads.Bead{
		closedSessionNameMatches,
		closedAliasMatches,
		closedHistoricalAliasMatches,
	} {
		if len(matches) > 0 {
			return chooseSessionMatch(identifier, matches)
		}
	}
	return "", fmt.Errorf("%w: %q", ErrSessionNotFound, identifier)
}

func choosePreferredTemplateMatch(templateMatches, agentNameMatches []beads.Bead) (beads.Bead, bool) {
	best, ok := pickBestTemplateMatch(templateMatches)
	if !ok {
		return pickBestTemplateMatch(agentNameMatches)
	}
	if agentBest, agentOK := pickBestTemplateMatch(agentNameMatches); agentOK && templateMatchLess(agentBest, best) {
		return agentBest, true
	}
	return best, true
}

func pickBestTemplateMatch(matches []beads.Bead) (beads.Bead, bool) {
	if len(matches) == 0 {
		return beads.Bead{}, false
	}
	best := matches[0]
	for _, candidate := range matches[1:] {
		if templateMatchLess(candidate, best) {
			best = candidate
		}
	}
	return best, true
}

func templateMatchLess(a, b beads.Bead) bool {
	rankA := templateMatchRank(a)
	rankB := templateMatchRank(b)
	if rankA != rankB {
		return rankA < rankB
	}
	manualA := strings.TrimSpace(a.Metadata["manual_session"]) == "true"
	manualB := strings.TrimSpace(b.Metadata["manual_session"]) == "true"
	if manualA != manualB {
		return !manualA
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID > b.ID
}

func templateMatchRank(b beads.Bead) int {
	state := strings.TrimSpace(b.Metadata["state"])
	switch {
	case state == "active" || state == "awake":
		return 0
	case state == "creating" || strings.TrimSpace(b.Metadata["pending_create_claim"]) == "true":
		return 1
	case state == "asleep" && strings.TrimSpace(b.Metadata["sleep_reason"]) != "drained":
		return 2
	case state == "asleep" || state == "drained":
		return 3
	default:
		return 4
	}
}

func sessionAgentNameFromLabels(b beads.Bead) string {
	for _, label := range b.Labels {
		if strings.HasPrefix(label, "agent:") {
			return strings.TrimPrefix(label, "agent:")
		}
	}
	return ""
}

func aliasHistoryContains(metadata map[string]string, identifier string) bool {
	for _, alias := range AliasHistory(metadata) {
		if alias == identifier {
			return true
		}
	}
	return false
}

func chooseSessionMatch(identifier string, matches []beads.Bead) (string, error) {
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: %q", ErrSessionNotFound, identifier)
	case 1:
		return matches[0].ID, nil
	default:
		var ids []string
		for _, m := range matches {
			ids = append(ids, fmt.Sprintf("%s (%s)", m.ID, sessionIdentifierLabel(m)))
		}
		return "", fmt.Errorf("%w: %q matches %d sessions: %s", ErrAmbiguous, identifier, len(matches), strings.Join(ids, ", "))
	}
}

func sessionIdentifierLabel(b beads.Bead) string {
	for _, field := range []string{
		b.Metadata["alias"],
		b.Metadata["session_name"],
	} {
		if field != "" {
			return field
		}
	}
	if b.Metadata["template"] != "" {
		return b.Metadata["template"]
	}
	return b.Title
}
