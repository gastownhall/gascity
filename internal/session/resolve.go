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
// current alias matches. Normal session targeting does not fall through to
// template, agent_name, or historical alias compatibility identifiers.
//
// Returns ErrSessionNotFound if no live match is found, or ErrAmbiguous
// (wrapped with details) if multiple sessions match the identifier.
func ResolveSessionID(store beads.Store, identifier string) (string, error) {
	return resolveSessionID(store, identifier, false)
}

// ResolveSessionIDAllowClosed is the read-only variant of ResolveSessionID.
// When no live identifier claims the requested identifier, it falls back to
// closed exact alias and session_name matches so closed sessions remain
// inspectable by their stable current handles.
func ResolveSessionIDAllowClosed(store beads.Store, identifier string) (string, error) {
	return resolveSessionID(store, identifier, true)
}

// ResolveSessionIDByExactID resolves only direct bead ID matches.
func ResolveSessionIDByExactID(store beads.Store, identifier string) (string, error) {
	if store == nil {
		return "", fmt.Errorf("session store unavailable")
	}
	b, err := store.Get(identifier)
	if err == nil && IsSessionBeadOrRepairable(b) {
		RepairEmptyType(store, &b)
		return b.ID, nil
	}
	if err != nil && !errors.Is(err, beads.ErrNotFound) {
		return "", fmt.Errorf("looking up session %q: %w", identifier, err)
	}
	return "", fmt.Errorf("%w: %q", ErrSessionNotFound, identifier)
}

func resolveSessionID(store beads.Store, identifier string, allowClosed bool) (string, error) {
	if id, err := ResolveSessionIDByExactID(store, identifier); err == nil {
		return id, nil
	} else if !errors.Is(err, ErrSessionNotFound) {
		return "", err
	}

	openSessionNameMatches, err := sessionMatchesByMetadata(store, "session_name", identifier, "")
	if err != nil {
		return "", fmt.Errorf("listing sessions: %w", err)
	}
	openAliasMatches, err := sessionMatchesByMetadata(store, "alias", identifier, "")
	if err != nil {
		return "", fmt.Errorf("listing sessions: %w", err)
	}

	for _, matches := range [][]beads.Bead{
		openSessionNameMatches,
		openAliasMatches,
	} {
		if len(matches) > 0 {
			return chooseSessionMatch(identifier, matches)
		}
	}
	if !allowClosed {
		return "", fmt.Errorf("%w: %q", ErrSessionNotFound, identifier)
	}
	closedSessionNameMatches, err := sessionMatchesByMetadata(store, "session_name", identifier, "closed")
	if err != nil {
		return "", fmt.Errorf("listing sessions: %w", err)
	}
	closedAliasMatches, err := sessionMatchesByMetadata(store, "alias", identifier, "closed")
	if err != nil {
		return "", fmt.Errorf("listing sessions: %w", err)
	}
	for _, matches := range [][]beads.Bead{
		closedSessionNameMatches,
		closedAliasMatches,
	} {
		if len(matches) > 0 {
			return chooseSessionMatch(identifier, matches)
		}
	}
	return "", fmt.Errorf("%w: %q", ErrSessionNotFound, identifier)
}

func sessionMatchesByMetadata(store beads.Store, key, identifier, status string) ([]beads.Bead, error) {
	query := beads.ListQuery{
		Metadata: map[string]string{key: identifier},
	}
	if status != "" {
		query.Status = status
	}
	items, err := store.List(query)
	if err != nil {
		return nil, err
	}
	matches := make([]beads.Bead, 0, len(items))
	for _, b := range items {
		if !IsSessionBeadOrRepairable(b) {
			continue
		}
		RepairEmptyType(store, &b)
		if strings.TrimSpace(b.Metadata[key]) != identifier {
			continue
		}
		if status != "" && b.Status != status {
			continue
		}
		matches = append(matches, b)
	}
	return matches, nil
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

// hasSessionLabel returns true if the bead carries the gc:session label.
func hasSessionLabel(b beads.Bead) bool {
	for _, l := range b.Labels {
		if l == LabelSession {
			return true
		}
	}
	return false
}

// IsSessionBeadOrRepairable returns true if the bead is either a proper
// session bead (Type == "session") or a broken session bead (empty type
// but carries the gc:session label). The latter can occur after crashes
// or schema migrations that leave partially-written records.
func IsSessionBeadOrRepairable(b beads.Bead) bool {
	if b.Type == BeadType {
		return true
	}
	return b.Type == "" && hasSessionLabel(b)
}

// RepairEmptyType fixes a session bead with an empty type field by
// setting it to "session". This is a best-effort repair — if the store
// update fails, the in-memory bead is still patched so the current
// operation can proceed.
func RepairEmptyType(store beads.Store, b *beads.Bead) {
	if b.Type != "" {
		return
	}
	t := BeadType
	_ = store.Update(b.ID, beads.UpdateOpts{Type: &t})
	b.Type = BeadType
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
