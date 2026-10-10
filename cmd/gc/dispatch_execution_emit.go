package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/executionevent"
)

// executionEmitActor is the actor the control dispatcher records execution
// facts under.
const executionEmitActor = "control-dispatch"

// executionEmitCurrent is the projection the control dispatcher runs for a
// workflow root after processing one of its controls. It is a var so tests can
// observe when, and how often, the projection runs.
var executionEmitCurrent = executionevent.EmitCurrent

// executionEmitDeferral coalesces the execution-fact projection for the workflow
// roots one control-dispatcher drain pass processes.
//
// The projection re-reads the whole root (about six store reads) and its facts
// are at-least-once with a durable per-step marker, so running it once after a
// root's controls stop arriving gives the same facts as running it after every
// control. Running it after every control put those reads on the critical path
// between a control and the next readiness scan. The drain therefore records the
// root here and projects it when the root has no control left in the queue, or
// when the pass ends. A control that materialized new steps (Created > 0) is
// still projected synchronously, so a step_defined fact never trails a worker's
// claim of the step it defines.
//
// A crash before a deferred projection runs leaves the root's new steps
// unmarked; the next projection of that root (its next control, or the
// completions backstop) re-emits them, which is the same recovery the window
// between ProcessControl and the projection already relied on.
//
// The deferral holds root IDs only, never store handles: the scope store each
// control opens is still closed when that control returns, and a flush opens
// one store for the whole batch and closes it before returning.
type executionEmitDeferral struct {
	cityPath  string
	storePath string
	stderr    io.Writer
	pending   []deferredExecutionEmit
}

// deferredExecutionEmit is one workflow root waiting for its projection.
type deferredExecutionEmit struct {
	rootID string
	// federatedGraphLeg records that the root's controls were read from the
	// city graph binding rather than the scope's own graph store (see
	// controlBeadLedger), so the flush projects it against the same ledger.
	federatedGraphLeg bool
}

func newExecutionEmitDeferral(cityPath, storePath string, stderr io.Writer) *executionEmitDeferral {
	return &executionEmitDeferral{cityPath: cityPath, storePath: storePath, stderr: stderr}
}

// add records rootID for projection at the next flush. A root already pending
// keeps its place.
func (d *executionEmitDeferral) add(rootID string, federatedGraphLeg bool) {
	for _, p := range d.pending {
		if p.rootID == rootID {
			return
		}
	}
	d.pending = append(d.pending, deferredExecutionEmit{rootID: rootID, federatedGraphLeg: federatedGraphLeg})
}

// forget drops rootID from the pending set because a synchronous projection
// of it just ran.
func (d *executionEmitDeferral) forget(rootID string) {
	kept := d.pending[:0]
	for _, p := range d.pending {
		if p.rootID != rootID {
			kept = append(kept, p)
		}
	}
	d.pending = kept
}

// flushSettled projects every pending root that has no control in queue. A
// root that still has a queued control stays pending: that control is about to
// be processed in this pass, and the root is projected once after it.
func (d *executionEmitDeferral) flushSettled(queue []hookBead) {
	if len(d.pending) == 0 {
		return
	}
	queued := make(map[string]bool, len(queue))
	for _, candidate := range queue {
		if rootID := controlRootID(candidate.Metadata); rootID != "" {
			queued[rootID] = true
		}
	}
	var settled, waiting []deferredExecutionEmit
	for _, p := range d.pending {
		if queued[p.rootID] {
			waiting = append(waiting, p)
			continue
		}
		settled = append(settled, p)
	}
	d.pending = waiting
	d.project(settled)
}

// flush projects every pending root.
func (d *executionEmitDeferral) flush() {
	settled := d.pending
	d.pending = nil
	d.project(settled)
}

// project runs the projection for roots against one freshly opened scope store.
func (d *executionEmitDeferral) project(roots []deferredExecutionEmit) {
	if len(roots) == 0 {
		return
	}
	start := time.Now()
	err := d.projectRoots(roots)
	workflowTracef("serve emit-execution store=%s roots=%d dur=%s", d.storePath, len(roots), time.Since(start).Round(time.Millisecond))
	if err != nil {
		fmt.Fprintf(d.stderr, "warning: control dispatch: projecting execution facts for %s: %v\n", deferredRootIDs(roots), err) //nolint:errcheck // processed controls are preserved
	}
}

func (d *executionEmitDeferral) projectRoots(roots []deferredExecutionEmit) error {
	cfg, err := loadCityConfig(d.cityPath, d.stderr)
	if err != nil {
		return err
	}
	resolveRigPaths(d.cityPath, cfg.Rigs)
	store, err := openControlStoreForDispatch(d.storePath, d.cityPath, cfg)
	if err != nil {
		return fmt.Errorf("opening scoped control store %q: %w", d.storePath, err)
	}
	defer func() {
		if cerr := closeBeadStoreHandle(store); cerr != nil {
			fmt.Fprintf(d.stderr, "warning: control dispatch: closing scope store %q: %v\n", d.storePath, cerr) //nolint:errcheck // projection outcome is preserved
		}
	}()
	primary := controlGraphStore(d.cityPath, d.storePath, cfg, store)
	targets := make([]executionEmitTarget, 0, len(roots))
	for _, p := range roots {
		graph := primary
		if p.federatedGraphLeg {
			if extra, ok := controlGraphExtraLeg(d.cityPath, d.storePath); ok {
				graph = extra
			}
		}
		targets = append(targets, executionEmitTarget{rootID: p.rootID, graphStore: graph})
	}
	return emitExecutionFacts(d.cityPath, store, targets, d.stderr)
}

// executionEmitTarget is one workflow root and the graph store that owns it.
type executionEmitTarget struct {
	rootID     string
	graphStore beads.Store
}

// emitExecutionFacts projects each target root against its graph store and the
// scope work store through one city event recorder.
func emitExecutionFacts(cityPath string, workStore beads.Store, targets []executionEmitTarget, stderr io.Writer) error {
	recorder := openCityRecorderAt(cityPath, stderr)
	var errs []error
	for _, target := range targets {
		if err := executionEmitCurrent(recorder, beads.GraphStore{Store: target.graphStore}, beads.WorkStore{Store: executionEmitStore(workStore, cityPath)}, target.rootID, executionEmitActor); err != nil {
			errs = append(errs, fmt.Errorf("root %s: %w", target.rootID, err))
		}
	}
	if closer, ok := recorder.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing event recorder: %w", err))
		}
	}
	return errors.Join(errs...)
}

func deferredRootIDs(roots []deferredExecutionEmit) string {
	ids := make([]string, 0, len(roots))
	for _, p := range roots {
		ids = append(ids, p.rootID)
	}
	return strings.Join(ids, ",")
}

// controlRootID returns the workflow root a control bead belongs to.
func controlRootID(metadata map[string]string) string {
	return strings.TrimSpace(metadata[beadmeta.RootBeadIDMetadataKey])
}
