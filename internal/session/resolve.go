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
// a session bead, it is returned immediately. Otherwise, it resolves only
// against live identifiers: open exact current alias matches first, then open
// exact historical alias matches, then open exact session_name matches.
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

	var openAliasMatches []beads.Bead
	var openHistoricalAliasMatches []beads.Bead
	var openSessionNameMatches []beads.Bead
	var closedAliasMatches []beads.Bead
	var closedHistoricalAliasMatches []beads.Bead
	var closedSessionNameMatches []beads.Bead
	for _, b := range all {
		if b.Type != BeadType {
			continue
		}
		alias := strings.TrimSpace(b.Metadata["alias"])
		sessionName := strings.TrimSpace(b.Metadata["session_name"])
		historicalAliasMatch := aliasHistoryContains(b.Metadata, identifier)
		if b.Status != "closed" {
			switch {
			case alias == identifier:
				openAliasMatches = append(openAliasMatches, b)
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
		openAliasMatches,
		openHistoricalAliasMatches,
		openSessionNameMatches,
	} {
		if len(matches) > 0 {
			return chooseSessionMatch(identifier, matches)
		}
	}
	if !allowClosed {
		return "", fmt.Errorf("%w: %q", ErrSessionNotFound, identifier)
	}
	for _, matches := range [][]beads.Bead{
		closedAliasMatches,
		closedHistoricalAliasMatches,
		closedSessionNameMatches,
	} {
		if len(matches) > 0 {
			return chooseSessionMatch(identifier, matches)
		}
	}
	return "", fmt.Errorf("%w: %q", ErrSessionNotFound, identifier)
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
