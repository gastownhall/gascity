package api

// rig_rollback.go carries the server-layer half of the G14 atomic rollback for
// async git_url rig-create: the created-vs-preexisting manifest that crosses the
// StateMutator boundary, the durable-record persistence of that manifest, and
// the boot sweep that reconciles orphan in_flight records before the
// rig-create/sling handlers serve (G13 §6, C4c §2/§4).
//
// Provision (internal/rig) rolls back only the topology files it wrote in its
// guarded window (city.toml/site.toml/packs.lock/routes). The server layer adds
// the other axes — the cloned/created rig directory, the managed Dolt database,
// and the idempotency-record state transition — sequenced drop-then-mark so a
// same-digest retry or a crash-recovery sweep always finds clean ground.

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// RigProvisionManifest records the resources one async git_url provision
// created, so the server can tear exactly those down on failure without ever
// touching a preexisting directory or an adopted store (C4c §2.2). It crosses
// the StateMutator boundary: controllerState (cmd/gc) builds it as it clones and
// provisions, the server persists it into the durable idempotency record, and
// the teardown reads it back to know precisely what to remove.
//
// A zero value claims nothing, so a teardown driven by it is a no-op — the safe
// default that never deletes data the machine cannot prove it created.
type RigProvisionManifest struct {
	// RigName is the rig the manifest describes (for logging and the boot-sweep
	// completeness probe / config lookup).
	RigName string
	// CreatedDir is the absolute rig working-tree path THIS request created
	// (a git clone into an absent path). Empty when the path preexisted, in
	// which case the directory is never removed. Removing it subsumes the rig's
	// .beads store.
	CreatedDir string
	// DoltDB is the managed Dolt database name THIS request minted. Empty when
	// the city runs a file store, GC_DOLT=skip deferred the DB to the
	// controller, or the store was adopted/re-added — none of which this request
	// may drop.
	DoltDB string
}

// IsEmpty reports whether the manifest claims no created resource, so a caller
// can skip the teardown round-trip entirely.
func (m RigProvisionManifest) IsEmpty() bool {
	return m.CreatedDir == "" && m.DoltDB == ""
}

// manifestFromRecord reconstructs the manifest persisted on a durable
// idempotency record (the boot sweep and the re-clone pre-drop both read it
// back this way).
func manifestFromRecord(rec *beads.Bead) RigProvisionManifest {
	if rec == nil {
		return RigProvisionManifest{}
	}
	return RigProvisionManifest{
		RigName:    rec.Metadata[metaIdemRigName],
		CreatedDir: rec.Metadata[metaIdemCreatedDir],
		DoltDB:     rec.Metadata[metaIdemDoltDB],
	}
}

// persistManifest merges the manifest keys into the durable record
// (record-then-create, C4c §2.2) so a crash leaves no unmanifested resource.
// It is a no-op when the record is synthetic (beadID == "") or the manifest
// claims nothing. SetMetadataBatch merges, so successive calls accrete the
// created_dir first (before the clone) and the dolt_db later (after init).
func persistManifest(store beads.Store, beadID string, m RigProvisionManifest) error {
	if beadID == "" || store == nil {
		return nil
	}
	updates := map[string]string{}
	if m.CreatedDir != "" {
		updates[metaIdemCreatedDir] = m.CreatedDir
	}
	if m.DoltDB != "" {
		updates[metaIdemDoltDB] = m.DoltDB
	}
	if len(updates) == 0 {
		return nil
	}
	if err := store.SetMetadataBatch(beadID, updates); err != nil {
		return fmt.Errorf("persisting rig-provision manifest on %s: %w", beadID, err)
	}
	return nil
}

// RigSweepDeps is the controller-side surface the boot sweep needs: the
// completeness probe (to distinguish a fully-provisioned rig caught in the
// success window from a genuinely partial one) and the manifest-driven teardown.
// controllerState (cmd/gc) satisfies it; the sweep stays free of any filesystem
// or Dolt knowledge so it is unit-testable with a fake.
type RigSweepDeps interface {
	// RigComplete reports whether a rig with the given name is fully provisioned
	// — present in the loaded config AND its bead store is structurally valid.
	// A crash after Provision committed + refresh succeeded but before the
	// durable succeeded write leaves such a rig under an in_flight record; the
	// sweep must reconcile it FORWARD, never destroy it. prefix/defaultBranch
	// are the result fields to record on the forward reconcile.
	RigComplete(rigName string) (complete bool, prefix, defaultBranch string)
	// TeardownPartialRig removes the created dir and drops the managed Dolt DB
	// named in the manifest (and best-effort repairs routes). Best-effort; a
	// non-nil return means debris may remain, so the caller must NOT mark the
	// record rolled_back.
	TeardownPartialRig(ctx context.Context, m RigProvisionManifest) error
}

// listOrphanInFlightIdemRecords returns every durable rig-create idempotency
// record for the city still in state in_flight. At boot the live index is empty
// (G13 §3.5), so every such record is an orphan whose goroutine did not survive
// the restart. The lookup is metadata-only (kind+city) with IncludeClosed — the
// records are closed at birth — then filtered on state in Go.
func listOrphanInFlightIdemRecords(store beads.Store, city string) ([]beads.Bead, error) {
	matches, err := store.List(beads.ListQuery{
		Metadata: map[string]string{
			metaIdemKind: idemKindRigCreate,
			metaIdemCity: city,
		},
		IncludeClosed: true,
	})
	if err != nil {
		return nil, fmt.Errorf("listing idem records for %s: %w", city, err)
	}
	orphans := matches[:0:0]
	for i := range matches {
		if matches[i].Metadata[metaIdemState] == idemStateInFlight {
			orphans = append(orphans, matches[i])
		}
	}
	return orphans, nil
}

// SweepOrphanRigProvisions reconciles orphan in_flight rig-create idempotency
// records at controller boot (G13 §6, C4c §4). It MUST run before the
// rig-create/sling handlers are admitted to serve: an un-swept orphan would let
// a same-id retry re-clone over un-torn-down debris.
//
// For each orphan it either reconciles FORWARD to succeeded (the completeness
// probe says the rig is fully provisioned — a crash in the §2.4 success window)
// or, for a genuinely partial orphan, tears down the manifested dir/DB and THEN
// marks the record rolled_back (drop-then-mark: the record never reaches
// rolled_back with debris still on disk). A teardown failure leaves the record
// in_flight (logged, un-retryable until an operator or a later sweep completes
// it) rather than marking it clean over surviving debris.
//
// It is best-effort and idempotent (a re-crash mid-sweep re-runs cleanly): per
// record failures are joined into the returned error for the caller to log, and
// the sweep continues to the next record.
func SweepOrphanRigProvisions(ctx context.Context, store beads.Store, city string, deps RigSweepDeps) error {
	if store == nil || deps == nil {
		return nil
	}
	orphans, err := listOrphanInFlightIdemRecords(store, city)
	if err != nil {
		return err
	}
	var errs error
	for i := range orphans {
		rec := orphans[i]
		m := manifestFromRecord(&rec)

		// Completeness probe first: never drop-then-mark a fully-provisioned rig
		// (the one place C4c refines G13 §6's literal "drop any partial dir").
		if complete, prefix, branch := deps.RigComplete(m.RigName); complete {
			if mErr := markIdemSucceeded(store, rec.ID, m.RigName, prefix, branch); mErr != nil {
				errs = errors.Join(errs, fmt.Errorf("sweep: reconcile %s forward: %w", rec.ID, mErr))
			}
			continue
		}

		// Genuinely partial: drop-then-mark.
		if tErr := deps.TeardownPartialRig(ctx, m); tErr != nil {
			// Debris may remain: leave in_flight so a retry re-clones (which
			// pre-drops) rather than marking clean over surviving state.
			errs = errors.Join(errs, fmt.Errorf("sweep: teardown %s (rig %q): %w", rec.ID, m.RigName, tErr))
			continue
		}
		if mErr := markIdemRolledBack(store, rec.ID); mErr != nil {
			errs = errors.Join(errs, fmt.Errorf("sweep: mark %s rolled_back: %w", rec.ID, mErr))
		}
	}
	return errs
}
