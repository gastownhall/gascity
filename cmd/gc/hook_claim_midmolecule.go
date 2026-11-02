package main

// ci-soai behavior 6: the cross-wired pour. A pool slot that is mid-molecule
// on one workflow root claimed steps of unrelated roots (on 2026-11-02 the
// session executing root gk-dg6uh claimed steps of gk-ln00k and gk-pvugm):
// the hook's eligible-candidate loop matches route+assignee only, so a slot
// whose last step closed takes the next ready pool step while its own root
// still has open work. The slot's own machinery then brakes the foreign steps
// post-hoc (the cross-wired-pour playbook), deferring the roots and burning a
// formula cycle per incident.
//
// The fix is claim-side: before a session-backed claimant mints a new
// obligation, resolve the molecule roots it is mid-flight on — roots with at
// least one step stamped to this session (gc.session_id, the claim-time
// identity stamp) that are themselves open/in_progress AND still have an open
// or in_progress step — and refuse candidates belonging to a different root.
// Steps of one root legitimately spread across pool slots (IndependentSteps),
// so session-stamped steps identify what THIS session is executing, not what
// the root needs.
//
// The veto is read-only and per-invocation: it decides which candidates this
// seat skips; it never mutates the skipped work, so a free session claims it
// on the same tick. Candidates without a gc.root_bead_id stamp (plain work,
// named-session identity routes) are never vetoed, and claimants without a
// session ID (named/manual seats) never pay the resolution queries.
//
// Deliberately fail-open: a store error or an unreadable root leaves the veto
// inert rather than stalling the pool on store flakiness. The session's own
// cross-wire detector (the playbook that braked gk-ljxig/gk-a3h9r/gk-qdz5i)
// remains the backstop for a claim that slips through.

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// hookClaimMoleculeLister is the store surface the mid-molecule veto reads:
// a root's canonical status and metadata-filtered lists for the steps keyed
// to a session (gc.session_id) and to a root (gc.root_bead_id). *beads.BdStore
// and *beads.MemStore both satisfy it.
type hookClaimMoleculeLister interface {
	Get(id string) (beads.Bead, error)
	List(query beads.ListQuery) ([]beads.Bead, error)
}

// hookClaimSessionStepKeys are the metadata keys a claim can be keyed to a
// session under. The claim machinery stamps the snake_case key
// (hookClaimIdentityPatch); some writers stamp the camelCase variant
// alongside it, and both are read wherever a bead's session link is resolved.
var hookClaimSessionStepKeys = []string{
	beadmeta.SessionIDMetadataKey,
	beadmeta.SessionIDCamelMetadataKey,
}

// hookClaimMoleculeStatusLive reports whether a workflow status is one a
// molecule is still executing. Parked statuses (deferred, blocked — the
// cross-wired-pour playbook parks braked steps as deferred) do not count: a
// root whose steps are all parked or closed is not mid-flight.
func hookClaimMoleculeStatusLive(status string) bool {
	switch strings.TrimSpace(status) {
	case "open", "in_progress":
		return true
	}
	return false
}

// hookClaimMoleculeRootsWithStore is the production resolution: query bd for
// every bead stamped to this session and keep the distinct live molecule
// roots. Runs at most once per claim invocation (memoized by the caller).
func hookClaimMoleculeRootsWithStore(ctx context.Context, dir string, env []string, sessionID string) (map[string]struct{}, error) {
	store := hookClaimBdStoreContext(ctx, dir, env, "")
	return hookClaimMoleculeRoots(store, sessionID)
}

// hookClaimMoleculeRoots resolves live molecule roots from any lister. A root
// is live when its canonical status is open/in_progress and at least one of
// its steps is open or in_progress; otherwise (all steps deferred/blocked,
// root settled, root unreadable) it is not mid-flight and this seat may claim
// elsewhere.
func hookClaimMoleculeRoots(store hookClaimMoleculeLister, sessionID string) (map[string]struct{}, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}
	rootIDs := make(map[string]struct{})
	for _, key := range hookClaimSessionStepKeys {
		mine, err := store.List(beads.ListQuery{
			Metadata:      map[string]string{key: sessionID},
			IncludeClosed: true,
			TierMode:      beads.TierBoth,
		})
		if err != nil {
			return nil, err
		}
		for _, bead := range mine {
			if root := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey]); root != "" {
				rootIDs[root] = struct{}{}
			}
		}
	}
	live := make(map[string]struct{}, len(rootIDs))
	for root := range rootIDs {
		if hookClaimMoleculeRootLive(store, root) {
			live[root] = struct{}{}
		}
	}
	return live, nil
}

// hookClaimMoleculeRootLive reports whether root is mid-flight. Any read
// failure counts the root not live (fail-open), keeping the veto a refinement
// of the store's answer rather than a new way to wedge a seat.
func hookClaimMoleculeRootLive(store hookClaimMoleculeLister, root string) bool {
	canonical, err := store.Get(root)
	if err != nil {
		return false
	}
	if !hookClaimMoleculeStatusLive(canonical.Status) {
		return false
	}
	steps, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: root},
		IncludeClosed: true,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return false
	}
	for _, step := range steps {
		if hookClaimMoleculeStatusLive(step.Status) {
			return true
		}
	}
	return false
}

// hookClaimForeignMoleculeStep reports whether candidate is a workflow step of
// a root this session is NOT mid-flight on, while the session IS mid-flight on
// some other root. Candidates without a gc.root_bead_id stamp and claims when
// no root is live are never foreign.
func hookClaimForeignMoleculeStep(candidate beads.Bead, liveRoots map[string]struct{}) bool {
	if len(liveRoots) == 0 {
		return false
	}
	root := strings.TrimSpace(candidate.Metadata[beadmeta.RootBeadIDMetadataKey])
	if root == "" {
		return false
	}
	_, own := liveRoots[root]
	return !own
}

// hookClaimNoteMoleculeSkip names a vetoed candidate on stderr so the skip is
// observable — the incident this veto fixes ran two formula cycles before
// anyone could see why roots kept getting deferred. One line per vetoed
// candidate per invocation, mirroring the claim loop's other skip notes.
func hookClaimNoteMoleculeSkip(stderr io.Writer, sessionID string, liveRoots map[string]struct{}, candidate beads.Bead) {
	roots := make([]string, 0, len(liveRoots))
	for root := range liveRoots {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	fmt.Fprintf(stderr, "gc hook --claim: skipping %s: session %s is mid-molecule on %s; left for a free session\n", //nolint:errcheck
		candidate.ID, sessionID, strings.Join(roots, ","))
}
