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
// against live identifiers: open exact session_name matches first, then open
// agent/template/common-name matches.
//
// Returns ErrSessionNotFound if no live match is found, or ErrAmbiguous
// (wrapped with details) if multiple sessions match the identifier.
func ResolveSessionID(store beads.Store, identifier string) (string, error) {
	return resolveSessionID(store, identifier, false)
}

// ResolveSessionIDAllowClosed is the read-only variant of ResolveSessionID.
// When no live identifier claims the requested exact session_name, it falls
// back to closed exact session_name matches ahead of non-session_name
// identifier matches so historical sessions remain inspectable by their
// permanent names.
func ResolveSessionIDAllowClosed(store beads.Store, identifier string) (string, error) {
	return resolveSessionID(store, identifier, true)
}

func resolveSessionID(store beads.Store, identifier string, allowClosedSessionName bool) (string, error) {
	// Try direct store lookup first — works for any ID format.
	b, err := store.Get(identifier)
	if err == nil && b.Type == BeadType {
		return b.ID, nil
	}
	if err != nil && !errors.Is(err, beads.ErrNotFound) {
		return "", fmt.Errorf("looking up session %q: %w", identifier, err)
	}

	// Fall back to template-name resolution among open sessions.
	all, err := store.ListByLabel(LabelSession, 0)
	if err != nil {
		return "", fmt.Errorf("listing sessions: %w", err)
	}

	var openSessionNameMatches []beads.Bead
	var closedSessionNameMatches []beads.Bead
	var exactMatches []beads.Bead
	var suffixMatches []beads.Bead
	allowSuffix := !strings.Contains(identifier, "/")
	for _, b := range all {
		if b.Type != BeadType {
			continue
		}
		if strings.TrimSpace(b.Metadata["session_name"]) == identifier {
			if b.Status == "closed" {
				closedSessionNameMatches = append(closedSessionNameMatches, b)
			} else {
				openSessionNameMatches = append(openSessionNameMatches, b)
			}
			continue
		}
		if b.Status == "closed" {
			continue
		}
		exact, suffix := matchSessionIdentifier(b, identifier, allowSuffix)
		switch {
		case exact:
			exactMatches = append(exactMatches, b)
		case suffix:
			suffixMatches = append(suffixMatches, b)
		}
	}

	if len(openSessionNameMatches) > 0 {
		return chooseSessionMatch(identifier, openSessionNameMatches)
	}
	if !allowClosedSessionName {
		if len(exactMatches) > 0 {
			return chooseSessionMatch(identifier, exactMatches)
		}
		if len(suffixMatches) > 0 {
			return chooseSessionMatch(identifier, suffixMatches)
		}
		return "", fmt.Errorf("%w: %q", ErrSessionNotFound, identifier)
	}
	if len(closedSessionNameMatches) > 0 {
		return chooseSessionMatch(identifier, closedSessionNameMatches)
	}
	if len(exactMatches) > 0 {
		return chooseSessionMatch(identifier, exactMatches)
	}
	if len(suffixMatches) > 0 {
		return chooseSessionMatch(identifier, suffixMatches)
	}
	return "", fmt.Errorf("%w: %q", ErrSessionNotFound, identifier)
}

func matchSessionIdentifier(b beads.Bead, identifier string, allowSuffix bool) (exact, suffix bool) {
	for _, field := range []string{
		b.Metadata["agent_name"],
		b.Metadata["template"],
		b.Metadata["common_name"],
	} {
		if field == "" {
			continue
		}
		if field == identifier {
			return true, false
		}
	}
	if !allowSuffix {
		return false, false
	}
	for _, field := range []string{
		b.Metadata["agent_name"],
		b.Metadata["template"],
		b.Metadata["common_name"],
	} {
		if field != "" && strings.HasSuffix(field, "/"+identifier) {
			return false, true
		}
	}
	return false, false
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
		b.Metadata["session_name"],
		b.Metadata["agent_name"],
		b.Metadata["template"],
	} {
		if field != "" {
			return field
		}
	}
	return b.Title
}
