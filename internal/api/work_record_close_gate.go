package api

// The HTTP plane's half of the ADR-0009 work-record close gate.
//
// internal/workrecord owns the contract — which beads it covers, what a valid
// record is, whether enforcement is on — and cmd/gc has run it on the CLI plane
// since the contract shipped. This file is the other door. A bead closes through
// POST /bead/{id}/close and through a closed-status POST /bead/{id}/update, and
// an ungated door does not merely leak the odd close: it becomes the way closes
// get done, because the drain that cannot close through the gate closes through
// whatever still answers.
//
// Two things are this plane's own, and neither belongs in the shared package.
// The first is the CHECKOUT TABLE the repository rule reads — the city
// directory and the rig paths, which this plane holds in its live State and the
// CLI holds in the config the invocation loaded. Which repository a given bead
// answers to is not this plane's own: workrecord.RepoDirFor decides it from the
// bead's owner, so both doors judge one bead against one repository. The second
// is the refusal, which is the already-registered wrong-state 409 — the state
// being wrong is precisely that the bead is not closable yet — so gating these
// routes adds no status to the OpenAPI surface.

import (
	"context"
	"log"
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/workrecord"
)

// workRecordGateLogf writes the gate's warning line. It is a variable so a test
// can capture what a close reported, matching orderFeedLogf on this surface.
var workRecordGateLogf = log.Printf

// workRecordCommitReachable answers the gate's reachability clause. It is a
// variable for the same reason workRecordGateLogf is: it lets a test on this
// plane assert what the handler handed the oracle — specifically that the
// context is the request's — without running git here, which is
// internal/workrecord's row against a real repository.
var workRecordCommitReachable = workrecord.CommitReachableOnBranchContext

// gateWorkRecordClose checks a bead the caller is about to close against the
// work-record contract, returning a 409 when enforcement is on and the record
// does not satisfy it (nil otherwise — warn-only is the default, so a violation
// is logged and the close proceeds).
//
// stored is the row the residency resolver read and store is the store that
// holds it. submitted carries the metadata of the same request, which the
// documented atomic close uses to stamp the record and close in one call:
// beads.UpdateOpts.Metadata is an additive merge, so validating only the stored
// row would refuse a request that supplies a perfectly good record. Coverage is
// decided on the stored row — Type, gc.kind, and workflow-step metadata, read
// before any submitted field is applied — so a request cannot escape the gate
// by stamping gc.kind on its way out, nor by retyping a gated task away from
// task. The same choice means the reverse direction is NOT covered: a closing
// update that converts a non-gated bead into a task closes ungated, because the
// type it would be gated on is the one the request is about to write. That is
// the boundary the CLI door has at isWorkRecordGatedBead in cmd/gc/work_record_gate.go,
// which decides coverage on the stored row and then projects metadata only;
// moving it belongs in internal/workrecord so both doors move together rather
// than asking different questions of different populations.
//
// ctx is the request's, and it reaches the reachability clause because that
// clause shells out to git: a client that hangs up has to be able to stop the
// subprocess, or a wedged repository leaves one blocking call per retry.
//
// Known limit — the check and the write are not atomic. The row validated here
// is the one resolveBeadOwner read, and the caller applies its close or update
// afterwards without re-reading it, so a concurrent write landing in that window
// is neither seen by the gate nor refused by the write. A close that races an
// edit stripping gc.work_outcome can therefore pass a check the final row would
// have failed. The CLI door has the same shape at evaluateWorkRecordCloseGate in
// cmd/gc/work_record_gate.go, which validates a stored (or pre-fetched) bead and
// then lets the bd invocation write.
//
// The remedy is to fence the write on the revision the gate read —
// beads.ConditionalWriter already spells it (CloseIfMatch/UpdateIfMatch, via
// beads.ResolveConditionalWriter) — so this is a change to the close paths, not
// to the store contract. It is left for a follow-up because the fence has to be
// threaded through both doors together and only capable stores carry it: a store
// that resolves as legacy has no revision to fence on, so the gate would need a
// degraded path there rather than a refusal. The window is small and the losing
// outcome is a close that recorded slightly less than it should, not a corrupted
// row.
func (s *Server) gateWorkRecordClose(ctx context.Context, id string, store beads.Store, stored beads.Bead, submitted map[string]string) error {
	if !workrecord.Gated(stored) {
		return nil
	}
	prospective := projectSubmittedWorkRecord(stored, submitted)
	repoDir := s.workRecordRepoDir(store, prospective)
	unverified := false
	violations := workrecord.ValidateOnClose(prospective, func(commit, branch string) bool {
		if repoDir == "" {
			// The scope that owns this bead names no checkout (see
			// workRecordRepoDir for which shapes produce that, and why the
			// current State implementations do not), so there is no repository to
			// ask. The clause degrades to a warning and says so; see
			// workrecord.ReachabilityUnverifiedNote for why only this one does.
			unverified = true
			return true
		}
		return workRecordCommitReachable(ctx, repoDir, commit, branch)
	})

	enforce := workrecord.EnforceEnabled()
	mode := "warn-only"
	if enforce {
		mode = "enforced"
	}
	if unverified {
		workRecordGateLogf("work-record gate (%s): close of %s: %s", mode, id, workrecord.ReachabilityUnverifiedNote)
	}
	for _, violation := range violations {
		workRecordGateLogf("work-record gate (%s): close of %s: %s", mode, id, violation)
	}
	if enforce && len(violations) > 0 {
		return apierr.ConflictWrongState.Msg("conflict: bead " + id + " does not satisfy the work-record close contract: " + strings.Join(violations, "; "))
	}
	return nil
}

// closesStatus reports whether an update's status field closes the bead. It
// matches the CLI plane's reading of the same spelling (trimmed,
// case-insensitive) so one door does not gate a close the other lets through.
func closesStatus(status *string) bool {
	return status != nil && strings.EqualFold(strings.TrimSpace(*status), "closed")
}

// projectSubmittedWorkRecord overlays a request's metadata onto the stored bead
// so the gate validates the record the close is about to write, not the one it
// replaces. The overlay is additive, matching beads.UpdateOpts.Metadata.
func projectSubmittedWorkRecord(stored beads.Bead, submitted map[string]string) beads.Bead {
	if len(submitted) == 0 {
		return stored
	}
	metadata := make(beads.StringMap, len(stored.Metadata)+len(submitted))
	for key, value := range stored.Metadata {
		metadata[key] = value
	}
	for key, value := range submitted {
		metadata[key] = value
	}
	stored.Metadata = metadata
	return stored
}

// workRecordScopeDirs is this plane's checkout table: the city directory the
// server was constructed with, and the rig paths the live config declares. A rig
// path may be written relative to the city, so it is resolved the same way every
// other scope root on this plane is.
type workRecordScopeDirs struct {
	cityPath string
	rigs     []config.Rig
}

// CityDir returns the city checkout's directory.
func (d workRecordScopeDirs) CityDir() string { return strings.TrimSpace(d.cityPath) }

// RigDir returns the named rig's checkout and whether the config declares that
// rig at all. A declared rig that names no checkout answers ("", true), which
// the rule reads as unknown rather than as the city.
//
// The name is matched case-insensitively, deliberately more forgiving than the
// exact-match lookups this answer feeds (workdir.RigRootForName, sling's
// rigSuspended). Refs are stamped by storeref.RigRef from the configured rig
// name, so only a hand-stamped or historically-buggy ref differs by case;
// matching it judges the close against that rig's checkout instead of degrading
// to unverified. Both doors spell this matcher the same way, so whichever way it
// resolves, one bead is still judged against one repository.
func (d workRecordScopeDirs) RigDir(name string) (string, bool) {
	for _, rig := range d.rigs {
		if !strings.EqualFold(strings.TrimSpace(rig.Name), name) {
			continue
		}
		if strings.TrimSpace(rig.Path) == "" {
			return "", true
		}
		return resolveScopeRoot(d.cityPath, rig.Path), true
	}
	return "", false
}

// workRecordScopeDirs builds the checkout table from the server's live State.
func (s *Server) workRecordScopeDirs() workRecordScopeDirs {
	dirs := workRecordScopeDirs{cityPath: s.state.CityPath()}
	if cfg := s.state.Config(); cfg != nil {
		dirs.rigs = cfg.Rigs
	}
	return dirs
}

// workRecordRepoDir names the repository a shipped bead's commit must be
// reachable in. The rule is workrecord.RepoDirFor's — gc.work_dir, else the
// bead's OWNER — so this door and the CLI's judge one bead against one
// repository; all this plane supplies is the checkout table and, for a bead that
// names no owner, the answer it gave before owners were recorded.
//
// An empty result means "unknown" and must stay distinguishable from a path,
// because falling back to a default would run git in whatever directory the
// server happens to occupy and answer the reachability question about the wrong
// repository.
func (s *Server) workRecordRepoDir(store beads.Store, bead beads.Bead) string {
	dirs := s.workRecordScopeDirs()
	dir, kind := workrecord.RepoDirFor(bead, dirs)
	if kind == workrecord.ScopeUnrooted {
		return s.workRecordLegacyRepoDir(store, dirs)
	}
	return dir
}

// workRecordLegacyRepoDir is the answer this plane gave before a bead could name
// its owner: the checkout of the scope the row was READ through — the rig's path
// when a configured rig's store answered, the city directory otherwise. It stays
// for the beads that record no gc.root_store_ref, which is every city that
// predates the residency census, so their closes are unchanged.
//
// The store is matched by asking each CONFIGURED rig whether it is the one that
// answered, rather than by enumerating the loaded stores: an enumeration is a
// list of work stores, which is blind to a binding by construction, and the
// question is not "which stores exist" but "does the store that answered belong
// to a rig with a checkout".
//
// A store no configured rig claims is the city store or a class binding, and
// both answered to the city checkout before this rule existed. buildStores keys
// only from cfg.Rigs and swaps the config and the store map under one lock, so
// there is no third population to mistake for one.
//
// The converse population is real and this answer is a guess for it: in legacy
// shared-file mode buildStores aliases every rig backed by that store to one
// handle, so MANY rigs claim it and the identity match selects whichever is
// configured first — an ownerless bead whose work happened on a different rig
// is judged against that first rig's checkout. The loop predates this rule and
// RepoDirFor strictly narrows it, since a bead that names an owner no longer
// reaches here; what is left is the pre-census population, where no answer is
// better than a guess because the bead records nothing to resolve.
//
// Two shapes still produce "unknown" here: a configured rig that names no
// checkout, and a city with no path at all. Both are defensive against a State
// implementation the current ones do not produce — buildStores skips a rig with
// an empty path outright, so BeadStore answers nil for one and the identity
// match never selects it, and cityPath is set once at construction from an
// already-resolved directory and never reassigned. State is an interface and the
// branches are cheap, so they stay; a refactor that changes either fact should
// update this note rather than find it silently wrong.
func (s *Server) workRecordLegacyRepoDir(store beads.Store, dirs workRecordScopeDirs) string {
	for _, rig := range dirs.rigs {
		if s.state.BeadStore(rig.Name) != store {
			continue
		}
		// The path comes off the rig that MATCHED, not from a second lookup by
		// name: the match here is store identity, and re-deriving it from a name
		// would answer for whichever rig the name resolves to instead.
		if strings.TrimSpace(rig.Path) == "" {
			return ""
		}
		return resolveScopeRoot(dirs.cityPath, rig.Path)
	}
	return dirs.CityDir()
}
