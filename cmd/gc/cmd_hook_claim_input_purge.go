package main

// Claim-time proof that a workflow's input was purged, and the retirement it
// licenses (#8729).
//
// The done-check (workflowInputDoneAcross) deliberately fails open on a purged
// input: at that layer a purge is indistinguishable from legitimate ambiguity,
// because a convoy read that resolves to nothing can also mean a member a
// store class the read did not span would resolve, and "a store hiccup can
// never suppress real work". The consequence was that a graph.v2 workflow
// whose synthetic input convoy — or the single work bead it tracks — was
// purged kept its open steps in the ready pool forever, burning one full
// claim cycle per seat: each seat claimed a step, ran its spec resolution
// against a convoy that no longer existed, and disposed hard-fail, and the
// pool re-served the next open step to the next seat.
//
// This file owns the purge evidence the done-check refuses to judge. Its
// standard is stricter than "not done" in exactly the ways absence needs:
//
//   - Evidence is a non-empty set of ids — the convoy id itself when the
//     convoy row is gone. Any id a leg still HOLDS blocks the proof.
//   - The proof runs over a COMPLETE live frame (inputPurgeFrame): every leg
//     the workflow's work could reside in. A frame that cannot be built
//     certifies nothing, and an unreadable leg is a failed read, never
//     absence.
//   - Each evidence id is read LIVE on EVERY leg of the frame, and only
//     beads.ErrNotFound counts. Live because the hook fires against rows a
//     cached or tier-narrowed read can miss; every leg because a leg whose
//     namespace an id does not match is read anyway, which can only refuse
//     the proof, never widen it.
//
// Everything here fails OPEN onto today's serving behavior: an unjudged
// shape, an ambiguous frame, or a failed read leaves the candidate in the
// pool, exactly as before this file existed.
//
// Every function here takes beads.StoreHandles (or the LiveReader/Writer
// handles it holds) rather than a raw beads.Store: the typed-store boundary
// (storeclass lint) admits no new raw-store sites. A raw store reaches this
// file only as an anonymous argument to beads.HandlesFor.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/storeref"
)

// hookInputPurgeVerdict is one purge probe's answer for a candidate.
type hookInputPurgeVerdict struct {
	// RootID is the workflow root the verdict is about: the candidate itself
	// when it carries the input convoy id, else its gc.root_bead_id. Empty
	// when the candidate has no workflow ancestry.
	RootID string
	// Purged reports that the input was PROVED purged over a complete frame.
	Purged bool
	// Diagnosis names the evidence that carried the verdict — the proof that
	// held, or the reason it did not — for the operator-facing log line.
	Diagnosis string
}

// workflowInputPurged probes candidate's workflow input over a frame of the
// one store: the single-store shape of workflowInputPurgedAcross, and the
// probe a non-city hook (a plain bd work repo, or a test wiring a store
// directly) runs. The one store IS the whole world there, so its frame is
// trivially complete.
func workflowInputPurged(handles beads.StoreHandles, candidate beads.Bead) (hookInputPurgeVerdict, error) {
	return workflowInputPurgedAcross(handles, handles, candidate, func() (inputPurgeFrame, error) {
		return inputPurgeFrameForReaders(handles.Live), nil
	})
}

// workflowInputPurgedAcross probes candidate's workflow input with the two
// halves read from their own handle sets — the root and its steps from graph,
// the input convoy from work, mirroring workflowInputDoneAcross — and proves
// absence over the frame openFrame returns. openFrame is called ONLY once the
// probe holds evidence worth proving, so a workflow whose input resolves
// (today's every-day case) pays for no frame at all.
func workflowInputPurgedAcross(graph, work beads.StoreHandles, candidate beads.Bead, openFrame func() (inputPurgeFrame, error)) (hookInputPurgeVerdict, error) {
	root := candidate
	if strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey]) == "" {
		rootID := workflowRootIDForInputCheck(candidate)
		if rootID == "" {
			return hookInputPurgeVerdict{Diagnosis: "candidate is not workflow-linked"}, nil
		}
		var err error
		root, err = graph.Live.Get(rootID)
		if err != nil {
			return hookInputPurgeVerdict{}, fmt.Errorf("loading workflow root %s: %w", rootID, err)
		}
	}
	convoyID := strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey])
	if convoyID == "" {
		return hookInputPurgeVerdict{RootID: root.ID, Diagnosis: "workflow root has no input convoy"}, nil
	}

	// The convoy row is the first evidence read. Its own absence is evidence;
	// its presence hands the question to the membership arm.
	evidence, unproved, err := inputPurgeEvidenceForInput(work, root, convoyID)
	if err != nil || evidence == nil {
		return unproved, err
	}

	frame, err := openFrame()
	if err != nil {
		return hookInputPurgeVerdict{RootID: root.ID}, fmt.Errorf("opening the purge proof frame: %w", err)
	}
	absent, diag, err := frame.proveAbsent(evidence.ids)
	if err != nil {
		return hookInputPurgeVerdict{RootID: root.ID}, err
	}
	if !absent {
		return hookInputPurgeVerdict{RootID: root.ID, Diagnosis: diag}, nil
	}
	return hookInputPurgeVerdict{RootID: root.ID, Purged: true, Diagnosis: diag}, nil
}

// inputPurgeEvidence is the id set a purge proof must hold absent.
type inputPurgeEvidence struct {
	ids []string
}

// inputPurgeEvidenceForInput derives the evidence for the input convoy
// convoyID of workflow root, reading the work-side handles that own convoys.
// It returns the evidence, or a not-purged verdict explaining why there is
// none (with a nil error): no evidence means no proof, and the caller serves
// on.
func inputPurgeEvidenceForInput(work beads.StoreHandles, root beads.Bead, convoyID string) (*inputPurgeEvidence, hookInputPurgeVerdict, error) {
	convoyRow, err := work.Live.Get(convoyID)
	verdict := hookInputPurgeVerdict{RootID: root.ID}
	switch {
	case errors.Is(err, beads.ErrNotFound):
		// The convoy row itself is gone: its id is the whole evidence set.
		// No membership read can add or subtract from that.
		return &inputPurgeEvidence{ids: []string{convoyID}}, verdict, nil
	case err != nil:
		return nil, verdict, fmt.Errorf("reading input convoy %s: %w", convoyID, err)
	}
	// The row exists: a convoy the workflow root names as its input is the
	// synthetic-input relation (#8729); its tracked targets are judged by the
	// membership arm once that lands. A row that is not a convoy at all is a
	// corrupt link, which proves nothing either way.
	if convoyRow.Type != "convoy" {
		return nil, hookInputPurgeVerdict{RootID: root.ID, Diagnosis: fmt.Sprintf("input %s is not a convoy", convoyID)}, nil
	}
	return nil, hookInputPurgeVerdict{RootID: root.ID, Diagnosis: "input convoy is present"}, nil
}

// inputPurgeFrame is the complete live read set a purge proof runs over: the
// every leg of the city the workflow's work could reside in, as live readers.
// Legs are read directly (no by-id plan): a plan gates legs by namespace,
// which is an optimization a proof must not take.
type inputPurgeFrame struct {
	legs []beads.LiveReader
}

// inputPurgeFrameForReaders builds a frame over exactly the named readers,
// dropping nils and duplicates. It is the shape a test or a single-store
// caller frames its world with; completeness is then the caller's claim.
func inputPurgeFrameForReaders(readers ...beads.LiveReader) inputPurgeFrame {
	var frame inputPurgeFrame
	for _, reader := range readers {
		if reader == nil {
			continue
		}
		dup := false
		for _, have := range frame.legs {
			if samePurgeFrameLeg(have, reader) {
				dup = true
				break
			}
		}
		if !dup {
			frame.legs = append(frame.legs, reader)
		}
	}
	return frame
}

// samePurgeFrameLeg reports whether two legs read the same physical store.
// Interface comparison panics on a non-comparable dynamic type, so an
// uncomparable reader counts as distinct, the conservative direction for a
// proof: a duplicated read cannot license retirement a missing leg would not.
func samePurgeFrameLeg(a, b beads.LiveReader) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	if av.Type() != bv.Type() || !av.Comparable() || !bv.Comparable() {
		return false
	}
	return a == b
}

// inputPurgeFrameRefusal names why a city topology cannot ground a purge
// proof: a refused storage split, or an id-prefix configuration in fault.
// Both are shapes in which a frame's claim to cover the city's namespaces is
// unreliable — the residence question a by-id plan would gate is exactly the
// question a proof cannot afford to guess at — so the frame refuses, and the
// probe fails open onto serving. The topology itself stays in the caller's
// hands: only its completeness facts are judged here.
func inputPurgeFrameRefusal(refused error, faults []storeref.PrefixFault) error {
	if refused != nil {
		return fmt.Errorf("the city's storage split is refused, so input purge cannot be proved: %w", refused)
	}
	if len(faults) > 0 {
		reasons := make([]string, 0, len(faults))
		for _, fault := range faults {
			reasons = append(reasons, fault.String())
		}
		return fmt.Errorf("the city's id-prefix configuration is in fault, so input purge cannot be proved: %s", strings.Join(reasons, "; "))
	}
	return nil
}

// proveAbsent reports whether every id is absent from every leg of the frame,
// read live. Only beads.ErrNotFound is absence: a row any leg holds blocks
// the proof, a read that fails is an error (the caller fails open), and a
// found row's diagnosis names the id that turned up.
func (f inputPurgeFrame) proveAbsent(ids []string) (bool, string, error) {
	for _, id := range ids {
		for _, leg := range f.legs {
			_, err := leg.Get(id)
			switch {
			case err == nil:
				return false, fmt.Sprintf("%s is still held by a leg of the frame", id), nil
			case errors.Is(err, beads.ErrNotFound):
				continue
			default:
				return false, "", fmt.Errorf("reading %s over the purge proof frame: %w", id, err)
			}
		}
	}
	return true, fmt.Sprintf("every evidence id (%s) absent from every leg of a complete frame", strings.Join(ids, ", ")), nil
}

// retirePurgedWorkflow closes rootID and every open/in_progress step under it
// with the purged-input disposition: gc.outcome=fail, gc.failure_class=hard,
// gc.failure_reason=input_purged. This is the disposition a seat would write
// after burning its claim cycle discovering the spec was gone (#8729),
// applied once at claim time instead of per seat. Terminal steps are excluded
// so an already-closed step keeps its own disposition; the selection is the
// one skipDoneWorkflow closes over (workflowOpenSubtreeIDs), by
// gc.root_bead_id over both tiers, deduplicated.
func retirePurgedWorkflow(handles beads.StoreHandles, rootID string) error {
	ids, err := workflowOpenSubtreeIDs(handles.Live, rootID)
	if err != nil {
		return err
	}
	_, err = handles.Writer.CloseAll(ids, map[string]string{
		beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
		beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
		beadmeta.FailureReasonMetadataKey: beadmeta.FailureReasonInputPurged,
		"close_reason":                    hookRetirePurgedWorkflowCloseReason,
	})
	return err
}

// hookRetirePurgedWorkflowCloseReason is cmd/gc's copy of
// sourceworkflow.HookRetirePurgedWorkflowCloseReason; a test pins that the
// two agree.
const hookRetirePurgedWorkflowCloseReason = sourceworkflow.HookRetirePurgedWorkflowCloseReason

// hookInputPurgedViaWorkflowStore is the default InputPurged: the probe reads
// the workflow-store seam — the same store the done-check reads — proving
// absence over the city frame opener when one is installed, else over the
// store itself (the complete world for a non-city hook).
func hookInputPurgedViaWorkflowStore(openStore hookWorkflowStoreFunc, openFrame hookInputPurgeFrameFunc) hookInputPurgedFunc {
	return func(ctx context.Context, dir string, env []string, candidate beads.Bead) (hookInputPurgeVerdict, error) {
		handles := beads.HandlesFor(openStore(ctx, dir, env, candidate.ID, ""))
		if openFrame == nil {
			return workflowInputPurged(handles, candidate)
		}
		return workflowInputPurgedAcross(handles, handles, candidate, openFrame)
	}
}

// hookRetirePurgedWorkflowViaWorkflowStore is the default RetirePurgedWorkflow:
// the retirement writes land in the workflow store the probe read — the same
// seam SkipDoneWorkflow writes through.
func hookRetirePurgedWorkflowViaWorkflowStore(openStore hookWorkflowStoreFunc) hookRetirePurgedWorkflowFunc {
	return func(ctx context.Context, dir string, env []string, rootID string) error {
		return retirePurgedWorkflow(beads.HandlesFor(openStore(ctx, dir, env, rootID, "")), rootID)
	}
}
