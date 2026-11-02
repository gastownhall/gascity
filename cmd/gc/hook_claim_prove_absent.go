package main

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// absentProbeStore is the read surface an absence proof needs: one direct
// read. Deliberately narrower than beads.Store (workflowSubtreeStore's shape,
// cmd_hook_claim.go): the legacy openers still hand out raw stores, and a
// narrow surface lets the proof hold one without a raw signature or a class
// handle minted outside the residency layer.
type absentProbeStore interface {
	Get(id string) (beads.Bead, error)
}

// hookProveAbsentOpener opens one candidate store for an absence probe. A
// variable so tests can count the probes and fault one store, the seam
// hookCityStoreOpener gives the city front door.
type hookProveAbsentOpenerFn func(storeDir, cityPath string, cfg *config.City) (absentProbeStore, error)

var hookProveAbsentOpener hookProveAbsentOpenerFn = func(storeDir, cityPath string, cfg *config.City) (absentProbeStore, error) {
	return openOneShotStoreAtForCityWithConfig(storeDir, cityPath, cfg)
}

// hookProveAbsentAt is the production ProveAbsent seam for the city at
// cityPath: it reports whether every id in ids is confirmed absent from
// every store the supported topology can read — a direct read answering
// ErrNotFound. The city the hook invocation resolved owns the proof, however
// deep in a rig the claim itself runs: by-id residency answers the relocated
// class binding, the candidate enumeration spans the city and every rig
// scope, and claimDir — the scope the claim ran in, the one whose not-found
// started the question — is probed beside them so the store the evaluator
// itself read can never be the hole in the proof.
//
// The proof is strict on purpose (#8729): a purged verdict closes work as
// failed, so a not-found one store could make of a bead another store still
// holds must never reach it. A hit anywhere disproves; a store that will not
// open or read is an operational error, carried with id and store context; a
// suspended rig's unread scope is not an error — suspension stays quiescent
// — but it leaves the topology incomplete, so the answer is not-proven rather
// than proven.
func hookProveAbsentAt(cityPath string, cfg *config.City) hookProveAbsentFunc {
	return func(ctx context.Context, dir string, ids []string) (bool, error) {
		return proveAbsentAcross(ctx, cityPath, cfg, dir, ids)
	}
}

// proveAbsentAcross proves every id in ids absent across the city at cityPath,
// one id at a time: the first id that is present, errored, or unprovable ends
// the proof, because one survivor suffices to keep the workflow's input
// alive.
func proveAbsentAcross(ctx context.Context, cityPath string, cfg *config.City, claimDir string, ids []string) (bool, error) {
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("proving %s absent: %w", id, err)
		}
		proven, err := proveOneAbsent(cityPath, cfg, claimDir, id)
		if err != nil || !proven {
			return false, err
		}
	}
	return true, nil
}

// proveOneAbsent proves one id absent from the city at cityPath.
func proveOneAbsent(cityPath string, cfg *config.City, claimDir, id string) (bool, error) {
	// A relocated class binding is not one of the city's directories, so the
	// scan below cannot reach it; by-id residency answers it first, the same
	// leg the convoy resolver fronts its own scan with. An error there is an
	// operational fault, not a negative answer.
	if _, owned, err := cliByIDBindingOwner(cityPath, id); err != nil {
		return false, fmt.Errorf("resolving the class binding owner of %s: %w", id, err)
	} else if owned {
		return false, nil
	}
	dirs := convoyStoreCandidates(cfg, cityPath, id)
	if claimDir != "" && !slices.Contains(dirs, claimDir) {
		dirs = append(dirs, claimDir)
	}
	// A suspended rig stays quiescent unless the id's own prefix routes to it
	// — byIDStoreOpener's policy, inlined here because the proof's opener
	// hands out the narrow read surface above rather than the raw store that
	// wrapper takes.
	asleep := suspendedRigStoreRoots(cfg, cityPath)
	if len(asleep) > 0 {
		if rd := rigDirForBead(cfg, id); rd != "" {
			delete(asleep, normalizePathForCompare(resolveStoreScopeRoot(cityPath, rd)))
		}
	}
	for _, dir := range dirs {
		if len(asleep[normalizePathForCompare(dir)]) > 0 {
			// Intentionally unread coverage: suspension is quiescence, not a
			// fault, but a topology with an unread scope cannot prove
			// absence.
			return false, nil
		}
		store, err := hookProveAbsentOpener(dir, cityPath, cfg)
		if err != nil {
			// Partial coverage is not proof: a store the proof cannot open
			// may hold the id.
			return false, fmt.Errorf("proving %s absent: opening the store at %s: %w", id, dir, err)
		}
		if store == nil {
			return false, nil
		}
		if _, err := store.Get(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return false, fmt.Errorf("reading %s from the store at %s while proving it absent: %w", id, dir, err)
		}
		// A hit anywhere disproves the absence.
		return false, nil
	}
	return true, nil
}
