package session

import "github.com/gastownhall/gascity/internal/beads"

// This file puts user-identifier RESOLUTION on the session front door.
//
// The resolution algorithms themselves live in resolve.go and named_config.go
// as package functions over a beads.Store, because they are also used from
// paths that hold the raw store (the worker factory's target resolution, mail
// addressing). What was missing was a way to reach them through the typed door,
// and its absence was load-bearing: every cmd/gc consumer that resolved a
// user-supplied identifier had to hold the CONCRETE *Store purely to unwrap it
// back to the bead store these functions take (`sessFront.Store().Store`), which
// is exactly the escape the closed sessions contract exists to prevent.
//
// internal/session is class-owning and is excluded from the closed contracts —
// it can never import them without a cycle — so the contract cannot be threaded down
// here. A method on the front door is the only shape available, and it is the
// right one: resolution is a sessions READ, not a persistence capability.
//
// Each method is a delegation, deliberately: the resolution semantics stay in
// one place and the door adds nothing but reach. TestStoreResolveMethodsMatchTheirPackageFunctions
// is the equivalence oracle for that claim.

// ConfiguredNamedSessionMatch is the bead-free projection of
// ConfiguredNamedSessionLookup: the resolved bead IDs and their presence flags,
// with no beads.Bead crossing the front door. Every consumer of the raw lookup
// reads exactly these four fields — the canonical id to return and the
// conflicting id to name in an error — so the projection is complete rather
// than merely convenient.
type ConfiguredNamedSessionMatch struct {
	Canonical    string
	HasCanonical bool
	Conflict     string
	HasConflict  bool
}

// ResolveIDByExactID resolves only a direct bead-id match, through the store
// behind this door. It is ResolveSessionIDByExactID's front-door form and shares
// its error contract exactly, including ErrSessionNotFound for an absent or
// non-session id.
func (s *Store) ResolveIDByExactID(identifier string) (string, error) {
	return ResolveSessionIDByExactID(s.beadStore(), identifier)
}

// ResolveID resolves a user-supplied identifier against LIVE sessions, through
// the store behind this door. It is ResolveSessionID's front-door form and
// shares its error contract exactly (ErrSessionNotFound, ErrAmbiguous).
func (s *Store) ResolveID(identifier string) (string, error) {
	return ResolveSessionID(s.beadStore(), identifier)
}

// ResolveIDAllowClosed resolves a user-supplied identifier against live
// sessions and then, if none claims it, against closed ones — the read-only
// variant that keeps a closed session inspectable by its stable handle. It is
// ResolveSessionIDAllowClosed's front-door form and shares its error contract
// exactly.
func (s *Store) ResolveIDAllowClosed(identifier string) (string, error) {
	return ResolveSessionIDAllowClosed(s.beadStore(), identifier)
}

// LookupConfiguredNamed finds the canonical bead or first live conflict for a
// configured named session, projected to ids. It is
// LookupConfiguredNamedSession's front-door form; the projection is what keeps
// the raw beads that lookup carries from crossing the door.
func (s *Store) LookupConfiguredNamed(spec NamedSessionSpec) (ConfiguredNamedSessionMatch, error) {
	lookup, err := LookupConfiguredNamedSession(s.beadStore(), spec)
	if err != nil {
		return ConfiguredNamedSessionMatch{}, err
	}
	return ConfiguredNamedSessionMatch{
		Canonical:    lookup.Canonical.ID,
		HasCanonical: lookup.HasCanonical,
		Conflict:     lookup.Conflict.ID,
		HasConflict:  lookup.HasConflict,
	}, nil
}

// beadStore returns the bead store behind this door, or nil for a door that has
// none — the same `s == nil || s.store.Store == nil` shape the read surface
// already carries (listAllBeads, validatedBead). Every package function it feeds
// declares its own nil-store behavior, so absence is answered there rather than
// re-decided per method.
func (s *Store) beadStore() beads.Store {
	if s == nil {
		return nil
	}
	return s.store.Store
}
