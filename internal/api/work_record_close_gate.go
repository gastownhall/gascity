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
// The first is the repository the reachability clause is asked about: the CLI
// knows it from the caller's work scope, while here the owning store names it,
// through the same residency resolution that decides which row the close writes.
// The second is the refusal, which is the already-registered wrong-state 409 —
// the state being wrong is precisely that the bead is not closable yet — so
// gating these routes adds no status to the OpenAPI surface.

import (
	"log"
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/workrecord"
)

// workRecordGateLogf writes the gate's warning line. It is a variable so a test
// can capture what a close reported, matching orderFeedLogf on this surface.
var workRecordGateLogf = log.Printf

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
// still decided on the stored row, so a request cannot escape the gate by
// stamping gc.kind on its way out.
func (s *Server) gateWorkRecordClose(id string, store beads.Store, stored beads.Bead, submitted map[string]string) error {
	if !workrecord.Gated(stored) {
		return nil
	}
	prospective := projectSubmittedWorkRecord(stored, submitted)
	repoDir := s.workRecordRepoDir(store, prospective)
	unverified := false
	violations := workrecord.ValidateOnClose(prospective, func(commit, branch string) bool {
		if repoDir == "" {
			// No repository is known for this store, so there is nothing to ask.
			// Refusing here would block closes on a question this plane cannot
			// pose; the clause degrades to a warning and says so. Only this
			// clause degrades — a bead with no outcome at all is still refused,
			// because "the commit could not be checked" is not a reason to
			// accept a close that recorded nothing.
			unverified = true
			return true
		}
		return workrecord.CommitReachableOnBranch(repoDir, commit, branch)
	})

	enforce := workrecord.EnforceEnabled()
	mode := "warn-only"
	if enforce {
		mode = "enforced"
	}
	if unverified {
		workRecordGateLogf("work-record gate (%s): close of %s: reachability unverified: no repository is known for the store that holds this bead", mode, id)
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

// workRecordRepoDir names the repository a shipped bead's commit must be
// reachable in: the bead's own gc.work_dir if it recorded one, else the checkout
// of whichever scope holds it — the rig's path for a rig-resident bead, the city
// directory for a city-resident one.
//
// An empty result means "unknown", which is the honest answer for a bead that
// lives in a class binding: a binding is a store, not a checkout. It must stay
// distinguishable from a path, because falling back to a default here would run
// git in whatever directory the server happens to occupy and answer the
// reachability question about the wrong repository.
//
// The store is matched by asking each CONFIGURED rig whether it is the one that
// answered, rather than by enumerating the loaded stores: an enumeration is a
// list of work stores, which is blind to a binding by construction, and the
// question here is not "which stores exist" but "does the resolved owner have a
// checkout to name".
func (s *Server) workRecordRepoDir(store beads.Store, bead beads.Bead) string {
	if dir := strings.TrimSpace(bead.Metadata[beadmeta.WorkDirMetadataKey]); dir != "" {
		return dir
	}
	if cfg := s.state.Config(); cfg != nil {
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Path) == "" || s.state.BeadStore(rig.Name) != store {
				continue
			}
			return resolveScopeRoot(s.state.CityPath(), rig.Path)
		}
	}
	if cityStore := s.state.CityBeadStore(); cityStore != nil && cityStore == store {
		return strings.TrimSpace(s.state.CityPath())
	}
	return ""
}
