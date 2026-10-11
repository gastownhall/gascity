package session

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// AddressDirectory is the narrow address and liveness read seam consumers use
// when their persistence is not the Sessions class. It intentionally exposes
// no Beads handle.
//
// The two resolve methods answer two different questions and deliberately do
// not share an algorithm:
//
//   - ResolveAddress answers "which session is this identifier", the question
//     session targeting has always asked. It reaches ResolveSessionID and
//     ResolveSessionIDAllowClosed — the same functions every raw-store caller
//     uses — so a caller that moves onto this seam resolves identically.
//   - ResolveMailboxAddress answers "which session owns this mailbox", which
//     admits no guessing: an address claimed by two sessions is ambiguous
//     rather than won by whichever key was probed first, because delivering a
//     message to the wrong session is worse than not delivering it.
type AddressDirectory interface {
	ResolveAddress(selector string, includeClosed bool) (Info, error)
	ResolveMailboxAddress(selector string, closed bool) (Info, error)
	ListAddresses(includeClosed bool) ([]Info, error)
}

// ListAddresses returns the typed session records used for mailbox routing.
func (s *Store) ListAddresses(includeClosed bool) ([]Info, error) {
	return s.ListAll(ListAllOptions{IncludeClosed: includeClosed})
}

// ResolveAddress resolves a session ID, stable name, or current alias through
// the typed session projection. includeClosed selects the read-only variant
// that keeps a closed session reachable by its stable handle.
//
// The resolution is ResolveSessionID / ResolveSessionIDAllowClosed: an exact
// bead-id match first — a closed session included, so an identity stamped from
// a session that has since retired still resolves — then live canonical
// session_name with the dual alias/session_name demotion, then live current
// alias. This method adds the Info projection and the error hygiene below, and
// nothing else.
func (s *Store) ResolveAddress(selector string, includeClosed bool) (Info, error) {
	store := s.beadStore()
	resolve := ResolveSessionID
	if includeClosed {
		resolve = ResolveSessionIDAllowClosed
	}
	id, err := resolve(store, selector)
	if err != nil {
		return Info{}, newAddressDirectoryError("address resolution failed", err)
	}
	b, err := store.Get(id)
	if err != nil {
		return Info{}, newAddressDirectoryError("loading the resolved session failed", err)
	}
	return infoFromPersistedBead(b), nil
}

// ResolveMailboxAddress resolves the session that owns a recipient address in
// one liveness pass: live sessions when closed is false, closed ones when it is
// true. A caller that wants the historical fallback runs the live pass, then
// the closed pass, then its own alias-history search.
//
// Every current address of a session is a candidate at once — its bead ID, its
// alias, and its canonical session_name — and two sessions claiming the same
// address is ErrAmbiguous rather than a race between lookup orders. Mail turns
// that ambiguity into literal-only delivery instead of picking a mailbox.
func (s *Store) ResolveMailboxAddress(selector string, closed bool) (Info, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return Info{}, newAddressDirectoryError("address not found", ErrSessionNotFound)
	}
	matches, err := s.mailboxMatches(selector, closed)
	if err != nil {
		return Info{}, err
	}
	switch len(matches) {
	case 0:
		return Info{}, newAddressDirectoryError("address not found", ErrSessionNotFound)
	case 1:
		return infoFromPersistedBead(matches[0]), nil
	default:
		return Info{}, addressAmbiguity(matches)
	}
}

// mailboxMatches collects every session whose current addresses claim the
// selector, deduplicated by bead ID.
func (s *Store) mailboxMatches(selector string, closed bool) ([]beads.Bead, error) {
	store := s.beadStore()
	if store == nil {
		return nil, nil
	}
	var matches []beads.Bead
	// Slash addresses (e.g. "rig/agent.name") are never bare bead IDs. Skipping
	// the direct Get keeps the slash form out of the backing store's id lookup,
	// which a legacy backend renders into a query clause.
	if !strings.Contains(selector, "/") {
		b, err := store.Get(selector)
		switch {
		case err == nil && IsSessionBeadOrRepairable(b) && mailboxStatusMatches(b, closed):
			RepairEmptyType(store, &b)
			matches = appendUniqueMailboxMatch(matches, b)
		case err != nil && !errors.Is(err, beads.ErrNotFound):
			return nil, newAddressDirectoryError("direct lookup failed", err)
		}
	}

	status := ""
	if closed {
		status = "closed"
	}
	for _, key := range []string{"alias", "session_name"} {
		keyMatches, err := mailboxMatchesByMetadata(store, key, selector, status)
		if err != nil {
			return nil, newAddressDirectoryError(key+" lookup failed", err)
		}
		for _, match := range keyMatches {
			matches = appendUniqueMailboxMatch(matches, match)
		}
	}
	return matches, nil
}

// mailboxMatchesByMetadata probes one indexed address key. The re-check on the
// rows it returns is not redundant: a backing store may answer a metadata
// filter loosely, and a loose answer here delivers someone else's mail.
func mailboxMatchesByMetadata(store beads.Store, key, selector, status string) ([]beads.Bead, error) {
	query := beads.ListQuery{
		Metadata: map[string]string{key: selector},
		TierMode: beads.TierBoth,
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
		if !mailboxStatusMatches(b, status == "closed") {
			continue
		}
		if strings.TrimSpace(b.Metadata[key]) != selector {
			continue
		}
		matches = append(matches, b)
	}
	return matches, nil
}

// mailboxStatusMatches reports whether a session bead belongs to the liveness
// pass being run.
func mailboxStatusMatches(b beads.Bead, closed bool) bool {
	if closed {
		return b.Status == "closed"
	}
	return b.Status != "closed"
}

func appendUniqueMailboxMatch(matches []beads.Bead, b beads.Bead) []beads.Bead {
	for _, match := range matches {
		if match.ID == b.ID {
			return matches
		}
	}
	return append(matches, b)
}

// addressDirectoryError keeps the selector, the matched ids, and the backend's
// own message out of Error text while preserving the cause for errors.Is. The
// operation must be a constant.
type addressDirectoryError struct {
	operation string
	cause     error
}

func newAddressDirectoryError(operation string, cause error) *addressDirectoryError {
	return &addressDirectoryError{operation: operation, cause: cause}
}

func (e *addressDirectoryError) Error() string {
	return "resolving session address: " + e.operation
}

func (e *addressDirectoryError) Unwrap() error {
	return e.cause
}

func addressAmbiguity(matches []beads.Bead) error {
	return newAddressDirectoryError(fmt.Sprintf("ambiguous address (%d matches)", len(matches)), ErrAmbiguous)
}

// RecipientRoutesFromInfo returns every durable recipient route for a session:
// ID, alias, canonical name, and historical aliases. It is the Info-form
// counterpart of beadmail's former raw-bead codec and keeps address metadata
// out of Messaging callers.
func RecipientRoutesFromInfo(info Info) []string {
	seen := make(map[string]struct{}, 3+len(info.AliasHistory))
	routes := make([]string, 0, 3+len(info.AliasHistory))
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		routes = append(routes, value)
	}
	add(info.ID)
	add(info.Alias)
	add(info.SessionNameMetadata)
	for _, alias := range info.AliasHistory {
		add(alias)
	}
	return routes
}

var _ AddressDirectory = (*Store)(nil)
