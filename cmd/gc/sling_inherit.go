package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// slingCallerIdentityEnv names the calling session the way the default
// work_query does: its bead id, its session name, and its alias. A claim
// assigned to any of them is one of the caller's claims.
var slingCallerIdentityEnv = []string{"GC_SESSION_ID", "GC_SESSION_NAME", "GC_ALIAS"}

// slingInheritReader is the read surface inheritance needs from a store that
// can hold a claim or a run root.
type slingInheritReader interface {
	Get(id string) (beads.Bead, error)
	List(query beads.ListQuery) ([]beads.Bead, error)
}

// slingInheritSources is what initiator inheritance reads: the caller's
// assignee identities, the session bead's current-claim pointer, and the
// stores claims and run roots can live in.
type slingInheritSources struct {
	// identities are the assignees whose in_progress beads are the caller's
	// claims.
	identities []string
	// currentClaim returns the session bead's current_claim_bead_id ("" when
	// unset or when the session has no bead). Nil means no session bead.
	currentClaim func() (string, error)
	// stores are searched in order for claims and run roots: the relocated
	// graph binding first (when there is one), then the city and rig stores.
	stores []slingInheritReader
}

// inheritSlingInitiator gives a local `gc sling` run from inside a session the
// gc.initiator of the run that session is working on, so follow-up work an agent
// launches stays attributed to whoever launched the original run. It never
// overrides an initiator already on opts and is a no-op outside a session.
// Exactly one distinct initiator across the caller's claimed runs is inherited;
// several stamp nothing and leave a one-line warning on stderr. The stores it
// reads are the sling's own source-workflow stores (deps.SourceWorkflowStores):
// the relocated graph binding first, then the city and rig stores.
func inheritSlingInitiator(opts *slingOpts, deps slingDeps, getenv func(string) string, stderr io.Writer) {
	if strings.TrimSpace(opts.Initiator) != "" {
		return
	}
	sessionID := strings.TrimSpace(getenv("GC_SESSION_ID"))
	if sessionID == "" && strings.TrimSpace(getenv("GC_SESSION_NAME")) == "" {
		return
	}
	src := slingInheritSources{currentClaim: slingSessionCurrentClaim(sessionID)}
	for _, k := range slingCallerIdentityEnv {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			src.identities = append(src.identities, v)
		}
	}
	if deps.SourceWorkflowStores == nil {
		warnSlingInitiatorNotInherited(stderr, "no claim stores are available")
		return
	}
	stores, err := deps.SourceWorkflowStores()
	if err != nil {
		warnSlingInitiatorNotInherited(stderr, fmt.Sprintf("opening the claim stores: %v", err))
		return
	}
	for _, s := range stores {
		src.stores = append(src.stores, s.Store)
	}
	opts.Initiator = resolveInheritedInitiator(src, stderr)
}

// slingSessionCurrentClaim reads the session bead's current_claim_bead_id
// through the same session front door `gc hook current` reads it through. A
// session id that names no session bead has no current claim.
func slingSessionCurrentClaim(sessionID string) func() (string, error) {
	if sessionID == "" {
		return nil
	}
	return func() (string, error) {
		front, err := sessionCurrentClaimFrontDoor()
		if err != nil {
			return "", err
		}
		id, err := front.CurrentClaimBeadID(sessionID)
		if errors.Is(err, beads.ErrNotFound) || errors.Is(err, session.ErrSessionNotFound) {
			return "", nil
		}
		return id, err
	}
}

// slingInheritClaim is one of the caller's claims and the store it was read from.
type slingInheritClaim struct {
	bead beads.Bead
	home slingInheritReader
}

// resolveInheritedInitiator collects the caller's claims — the session's open
// current claim plus every in_progress bead assigned to one of its identities —
// resolves each to its run root with beadmeta.ResolveRunID, and returns the
// single distinct gc.initiator those roots carry. It returns "" when none
// carries one, and "" with a warning when they disagree or a read fails.
func resolveInheritedInitiator(src slingInheritSources, stderr io.Writer) string {
	var claims []slingInheritClaim
	seen := map[string]bool{}
	add := func(b beads.Bead, home slingInheritReader) {
		if seen[b.ID] {
			return
		}
		seen[b.ID] = true
		claims = append(claims, slingInheritClaim{bead: b, home: home})
	}
	if src.currentClaim != nil {
		id, err := src.currentClaim()
		if err != nil {
			warnSlingInitiatorNotInherited(stderr, fmt.Sprintf("reading the session's current claim: %v", err))
			return ""
		}
		if id != "" {
			b, home, found, err := getSlingInheritBead(src.stores, nil, id)
			if err != nil {
				warnSlingInitiatorNotInherited(stderr, err.Error())
				return ""
			}
			if found && b.Status != "closed" {
				add(b, home)
			}
		}
	}
	if len(src.identities) > 0 {
		for _, store := range src.stores {
			// FederatedReadTier on every leg: a relocated class store answers at
			// exactly the queried tier, so the zero value would drop its
			// ephemeral claims while the policy-wrapped legs still return theirs.
			listed, err := store.List(beads.ListQuery{Status: "in_progress", Assignees: src.identities, TierMode: beads.FederatedReadTier})
			if err != nil {
				warnSlingInitiatorNotInherited(stderr, fmt.Sprintf("listing the session's claims: %v", err))
				return ""
			}
			for _, b := range listed {
				add(b, store)
			}
		}
	}

	rootsByInitiator := map[string][]string{}
	for _, c := range claims {
		rootID := beadmeta.ResolveRunID(c.bead.Metadata, c.bead.ID, "")
		root := c.bead
		if rootID != c.bead.ID {
			var found bool
			var err error
			root, _, found, err = getSlingInheritBead(src.stores, c.home, rootID)
			if err != nil {
				warnSlingInitiatorNotInherited(stderr, err.Error())
				return ""
			}
			if !found {
				warnSlingInitiatorNotInherited(stderr, fmt.Sprintf("run root %s of claim %s was not found", rootID, c.bead.ID))
				return ""
			}
		}
		initiator := strings.TrimSpace(root.Metadata[beadmeta.InitiatorMetadataKey])
		if initiator == "" {
			continue
		}
		if !containsString(rootsByInitiator[initiator], rootID) {
			rootsByInitiator[initiator] = append(rootsByInitiator[initiator], rootID)
		}
	}
	switch len(rootsByInitiator) {
	case 0:
		return ""
	case 1:
		for initiator := range rootsByInitiator {
			return initiator
		}
	}
	parts := make([]string, 0, len(rootsByInitiator))
	for initiator, roots := range rootsByInitiator {
		parts = append(parts, fmt.Sprintf("%s=%s", strings.Join(roots, ","), initiator))
	}
	sort.Strings(parts)
	warnSlingInitiatorNotInherited(stderr, "the session's claimed runs have different initiators ("+strings.Join(parts, "; ")+")")
	return ""
}

// getSlingInheritBead reads id from home first, then from each store in order,
// reporting found=false when every store cleanly misses. Any other read error
// is returned: it is a failed read, not an absence.
func getSlingInheritBead(stores []slingInheritReader, home slingInheritReader, id string) (beads.Bead, slingInheritReader, bool, error) {
	order := stores
	if home != nil {
		order = append([]slingInheritReader{home}, stores...)
	}
	for _, store := range order {
		b, err := store.Get(id)
		if err == nil {
			return b, store, true, nil
		}
		if !errors.Is(err, beads.ErrNotFound) {
			return beads.Bead{}, nil, false, fmt.Errorf("reading %s: %w", id, err)
		}
	}
	return beads.Bead{}, nil, false, nil
}

// warnSlingInitiatorNotInherited reports, on one stderr line, why an in-session
// sling carries no inherited initiator.
func warnSlingInitiatorNotInherited(stderr io.Writer, reason string) {
	fmt.Fprintf(stderr, "gc sling: warning: not inheriting %s: %s\n", beadmeta.InitiatorMetadataKey, reason) //nolint:errcheck // best-effort stderr
}
