package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The claim-time purge probe (#8729).
//
// The done-check fails open on an input it cannot judge — an input convoy
// whose membership read came back empty or made only of unresolved
// placeholders — because that shape is also what a read that did not span the
// member's store looks like, and a store hiccup must never suppress real work.
// A purged convoy or a purged tracked bead leaves the same shape, so without
// this probe its workflow's open steps are re-served to every seat forever,
// each burning a claim cycle to discover the spec is gone.
//
// The probe holds purge to a stricter bar than the done-check holds "done":
// every evidence id must read as beads.ErrNotFound through the city's
// canonical by-id plan, over a frame that represents the whole city. Anything
// less — a found row, a read error, a suspended or unbound rig, a prefix
// fault, a refused topology, an id in no namespace the frame represents — is
// not proof, and the candidate is served exactly as it was before.

// hookRetirePurgedWorkflowCloseReason is the close_reason `gc hook --claim`
// stamps on a workflow root and its open steps once their input is proved
// purged. sourceworkflow keeps its own copy for the readers that must tell a
// controller close from an operator's; a test pins that the two agree.
const hookRetirePurgedWorkflowCloseReason = "hook --claim: workflow input convoy or tracked work purged; nothing to run"

// errWorkflowInputPurgeUnproven marks a purge probe that did not reach proof.
var errWorkflowInputPurgeUnproven = errors.New("input purge not proved")

// workflowInputPurgeFrame is the read frame a purge proof is judged over.
type workflowInputPurgeFrame struct {
	// get reads one bead by id through the city's canonical by-id plan:
	// a row, beads.ErrNotFound for a proved absence, or the failure.
	get func(id string) (beads.Bead, error)
	// represents reports whether id lies in a namespace a leg of the frame
	// holds. It only refuses: which leg answers is the by-id plan's choice.
	represents func(id string) bool
}

// workflowInputPurgeFrameOver is the purge frame over the by-id reader the
// wait-dependency reads use, refused when that reader cannot prove absence for
// the whole city: a refused topology (which serving reads tolerate in part,
// and proof must not), a frame gap (suspension or a prefix fault), or a work
// leg that was never opened.
func workflowInputPurgeFrameOver(reader waitDependencyPlanReader) (workflowInputPurgeFrame, error) {
	if reader.topo.Refused != nil {
		return workflowInputPurgeFrame{}, fmt.Errorf("%w: the city's residency topology is refused: %w", errWorkflowInputPurgeUnproven, reader.topo.Refused)
	}
	if reader.outOfFrame != "" {
		return workflowInputPurgeFrame{}, fmt.Errorf("%w: the frame does not cover the city: %s", errWorkflowInputPurgeUnproven, reader.outOfFrame)
	}
	if reader.topo.Work.Store == nil {
		return workflowInputPurgeFrame{}, fmt.Errorf("%w: the city work store is not open", errWorkflowInputPurgeUnproven)
	}
	prefixes := []string{reader.topo.Work.Prefix}
	for _, rig := range reader.topo.Rigs {
		prefixes = append(prefixes, rig.Prefix)
	}
	for _, binding := range reader.topo.Bindings {
		prefixes = append(prefixes, binding.Leg.Prefix)
		prefixes = append(prefixes, binding.Prefixes...)
	}
	return workflowInputPurgeFrame{
		get: reader.Get,
		represents: func(id string) bool {
			return slices.ContainsFunc(prefixes, func(prefix string) bool {
				return storeref.IDInNamespace(id, prefix)
			})
		},
	}, nil
}

// openInputPurgeFrame opens the purge frame over the whole city: the city work
// store, every configured rig's store, and the residency bindings, cut into
// the by-id plan wait dependencies read through. A rig that is suspended or
// unbound has no store in the frame, and one that cannot be opened fails it, so
// either refuses the frame rather than narrowing it.
func (h *hookCity) openInputPurgeFrame() (workflowInputPurgeFrame, error) {
	work, err := hookCityStoreOpener(h.path, h.cfg)
	if err != nil {
		return workflowInputPurgeFrame{}, fmt.Errorf("%w: opening the city store: %w", errWorkflowInputPurgeUnproven, err)
	}
	rigs, err := readyRigLegStores(h.cfg, h.path)
	if err != nil {
		return workflowInputPurgeFrame{}, fmt.Errorf("%w: opening the rig stores: %w", errWorkflowInputPurgeUnproven, err)
	}
	var outside []string
	for _, rig := range h.cfg.Rigs {
		if _, ok := rigs[rig.Name]; !ok {
			outside = append(outside, rig.Name)
		}
	}
	if len(outside) > 0 {
		return workflowInputPurgeFrame{}, fmt.Errorf("%w: rigs %s are suspended or unbound, so the frame does not cover the city", errWorkflowInputPurgeUnproven, strings.Join(outside, ", "))
	}
	return workflowInputPurgeFrameOver(newWaitDependencyPlanReader(residencyTopologyForCity(h.path, h.cfg, work, rigs), false))
}

// proveWorkflowInputPurged proves an unjudged input purged over frame and
// returns the evidence ids it found absent. The evidence is the input convoy
// itself when it is gone; when it still exists it must be a synthetic convoy,
// and the evidence is every unresolved member plus every target its metadata
// records (graphv2.RecordedInputTargetIDs), which name the target after a
// purge cascaded its tracks edge away. Empty evidence, a non-synthetic convoy,
// or any evidence id that is found or cannot be read is not proof.
func proveWorkflowInputPurged(frame workflowInputPurgeFrame, verdict workflowInputVerdict) ([]string, error) {
	if !verdict.Unjudged || verdict.ConvoyID == "" {
		return nil, fmt.Errorf("%w: the input of workflow %s is not unjudged", errWorkflowInputPurgeUnproven, verdict.RootID)
	}
	inputConvoy, err := readPurgeEvidence(frame, verdict.ConvoyID)
	if errors.Is(err, beads.ErrNotFound) {
		return []string{verdict.ConvoyID}, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(inputConvoy.Metadata[beadmeta.SyntheticMetadataKey]) != "true" {
		return nil, fmt.Errorf("%w: input convoy %s exists and is not synthetic", errWorkflowInputPurgeUnproven, verdict.ConvoyID)
	}
	evidence := slices.Clone(verdict.Unresolved)
	for _, id := range graphv2.RecordedInputTargetIDs(inputConvoy) {
		if !slices.Contains(evidence, id) {
			evidence = append(evidence, id)
		}
	}
	if len(evidence) == 0 {
		return nil, fmt.Errorf("%w: input convoy %s exists and neither tracks nor records a target", errWorkflowInputPurgeUnproven, verdict.ConvoyID)
	}
	for _, id := range evidence {
		if _, err := readPurgeEvidence(frame, id); !errors.Is(err, beads.ErrNotFound) {
			if err == nil {
				return nil, fmt.Errorf("%w: input %s of convoy %s still exists", errWorkflowInputPurgeUnproven, id, verdict.ConvoyID)
			}
			return nil, err
		}
	}
	return evidence, nil
}

// readPurgeEvidence reads one evidence id over frame. Its result is a row,
// beads.ErrNotFound for a proved absence, or an errWorkflowInputPurgeUnproven
// error — never a bare read failure that could be mistaken for absence.
func readPurgeEvidence(frame workflowInputPurgeFrame, id string) (beads.Bead, error) {
	if !frame.represents(id) {
		return beads.Bead{}, fmt.Errorf("%w: %s is in no namespace the city frame holds", errWorkflowInputPurgeUnproven, id)
	}
	b, err := frame.get(id)
	switch {
	case err == nil:
		return b, nil
	case errors.Is(err, beads.ErrNotFound):
		return beads.Bead{}, beads.ErrNotFound
	default:
		return beads.Bead{}, fmt.Errorf("%w: reading %s: %w", errWorkflowInputPurgeUnproven, id, err)
	}
}

// hookRetireIfInputPurged asks InputPurged to prove candidate's unjudged input
// gone and, on proof, retires its workflow with RetirePurgedWorkflow under a
// fresh mutation deadline. A failed proof or retirement writes nothing more,
// is logged, and serves candidate as usual.
func hookRetireIfInputPurged(candidate beads.Bead, verdict workflowInputVerdict, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) bool {
	if ops.InputPurged == nil || ops.RetirePurgedWorkflow == nil {
		return false
	}
	evidence, err := ops.InputPurged(verdict)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: serving %s: input convoy %s of workflow %s resolves no member, and its purge is not proved: %v\n", candidate.ID, verdict.ConvoyID, verdict.RootID, err) //nolint:errcheck
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	if err := ops.RetirePurgedWorkflow(ctx, dir, opts.Env, verdict.RootID); err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: retiring workflow %s whose input is purged: %v\n", verdict.RootID, err) //nolint:errcheck
		return false
	}
	fmt.Fprintf(stderr, "gc hook --claim: skipped %s: workflow %s input is purged (absent from every store: %s); root and open steps closed as failed (%s)\n", candidate.ID, verdict.RootID, strings.Join(evidence, ", "), beadmeta.FailureReasonInputPurged) //nolint:errcheck
	return true
}

// retirePurgedWorkflow closes rootID and its open steps with the input_purged
// hard failure: the disposition a seat would write after burning its claim
// cycle on the missing spec, written once instead of once per seat.
func retirePurgedWorkflow(store workflowCloseStore, rootID string) error {
	return closeWorkflowOpenMembers(store, rootID, map[string]string{
		beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
		beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
		beadmeta.FailureReasonMetadataKey: beadmeta.FailureReasonInputPurged,
		"close_reason":                    hookRetirePurgedWorkflowCloseReason,
	})
}

func hookRetirePurgedWorkflowWithBdStore(ctx context.Context, dir string, env []string, rootID string) error {
	return retirePurgedWorkflow(hookClaimBdStoreContext(ctx, dir, env, ""), rootID)
}
