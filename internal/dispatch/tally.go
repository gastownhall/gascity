package dispatch

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// processTallyControl aggregates voter outputs from a completed fanout.
//
// The tally bead becomes runnable only after its fanout sibling closes
// (graph injection ensures Needs: [sourceRef+"-fanout"]). It then reads
// each voter sink bead's gc.output_json, extracts gc.vote_field, and
// reduces according to gc.tally_mode before closing itself.
func processTallyControl(store beads.Store, bead beads.Bead, _ ProcessOptions) (ControlResult, error) {
	rootID := bead.Metadata[beadmeta.RootBeadIDMetadataKey]
	if rootID == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.root_bead_id", bead.ID)
	}
	sourceRef := bead.Metadata[beadmeta.ControlForMetadataKey]
	if sourceRef == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.control_for", bead.ID)
	}
	mode := bead.Metadata[beadmeta.TallyModeMetadataKey]
	if mode == "" {
		mode = "majority"
	}
	voteField := bead.Metadata[beadmeta.VoteFieldMetadataKey]

	// Find the fanout bead for this source step.
	workflowBeads, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: listing workflow beads: %w", bead.ID, err)
	}
	fanoutRef := sourceRef + "-fanout"
	fanoutBead, err := resolveWorkflowStepByRefFromBeads(workflowBeads, rootID, fanoutRef, workflowStepMatchOptions{})
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: resolving fanout bead %q: %w", bead.ID, fanoutRef, err)
	}
	if fanoutBead.Status != "closed" {
		return ControlResult{}, fmt.Errorf("%w: fanout %s not yet closed", ErrControlPending, fanoutBead.ID)
	}

	// Resolve the source step bead. The fanout Needs its source step, so the
	// source appears as a "blocks" down-dep of the fanout alongside the voter
	// sinks — it must be excluded from the vote count.
	sourceBead, err := resolveWorkflowStepByRefFromBeads(workflowBeads, rootID, sourceRef, workflowStepMatchOptions{})
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: resolving source bead %q: %w", bead.ID, sourceRef, err)
	}

	// Collect voter votes via the fanout's "blocks" deps.
	voterDeps, err := store.DepList(fanoutBead.ID, "down")
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: listing voter deps: %w", bead.ID, err)
	}

	votes := make([]string, 0, len(voterDeps))
	for _, dep := range voterDeps {
		if dep.Type != "blocks" {
			continue
		}
		if dep.DependsOnID == sourceBead.ID {
			continue // the fanout Needs its source step; that edge is not a voter
		}
		voter, err := store.Get(dep.DependsOnID)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: fetching voter %s: %w", bead.ID, dep.DependsOnID, err)
		}
		var vote string
		if mode == "any-pass" {
			// any-pass is defined over voter gc.outcome, independent of vote_field.
			vote = voter.Metadata[beadmeta.OutcomeMetadataKey]
		} else {
			vote, err = extractVote(voter, voteField)
			if err != nil {
				return ControlResult{}, fmt.Errorf("%s: extracting vote from %s: %w", bead.ID, voter.ID, err)
			}
		}
		votes = append(votes, vote)
	}

	outcome, result, err := tallyVotes(votes, mode)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: tallying votes: %w", bead.ID, err)
	}

	if err := store.SetMetadataBatch(bead.ID, map[string]string{
		beadmeta.TallyResultMetadataKey: result,
		beadmeta.OutcomeMetadataKey:     outcome,
	}); err != nil {
		return ControlResult{}, fmt.Errorf("%s: writing tally result: %w", bead.ID, err)
	}
	if err := store.Close(bead.ID); err != nil {
		return ControlResult{}, fmt.Errorf("%s: closing tally bead: %w", bead.ID, err)
	}
	return ControlResult{Processed: true, Action: "tally-" + outcome}, nil
}

// extractVote reads the vote value from a voter bead.
// If voteField is empty, returns gc.outcome directly.
// Otherwise traverses voteField as a dot-separated path into gc.output_json.
func extractVote(voter beads.Bead, voteField string) (string, error) {
	if voteField == "" {
		return voter.Metadata[beadmeta.OutcomeMetadataKey], nil
	}
	raw := voter.Metadata[beadmeta.OutputJSONMetadataKey]
	if raw == "" {
		return "", fmt.Errorf("voter %s has no gc.output_json", voter.ID)
	}
	var output map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		return "", fmt.Errorf("parsing gc.output_json for %s: %w", voter.ID, err)
	}
	current := interface{}(output)
	for _, part := range strings.Split(voteField, ".") {
		obj, ok := current.(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("voter %s: path %q does not traverse an object at %q", voter.ID, voteField, part)
		}
		current, ok = obj[part]
		if !ok {
			return "", fmt.Errorf("voter %s: vote_field %q not found", voter.ID, voteField)
		}
	}
	return fmt.Sprintf("%v", current), nil
}

// tallyVotes reduces a slice of vote strings to a pass/fail outcome
// and a human-readable result summary.
func tallyVotes(votes []string, mode string) (outcome, result string, err error) {
	if len(votes) == 0 {
		return "pass", "no-voters", nil
	}
	switch mode {
	case "majority":
		counts := make(map[string]int, len(votes))
		for _, v := range votes {
			counts[v]++
		}
		var winner string
		var winnerCount int
		for v, c := range counts {
			if c > winnerCount {
				winner = v
				winnerCount = c
			}
		}
		if winnerCount*2 > len(votes) {
			return "pass", winner, nil
		}
		return "fail", "no-majority", nil
	case "unanimous":
		first := votes[0]
		for _, v := range votes[1:] {
			if v != first {
				return "fail", "not-unanimous", nil
			}
		}
		return "pass", first, nil
	case "any-pass":
		for _, v := range votes {
			if v == "pass" {
				return "pass", "any-pass", nil
			}
		}
		return "fail", "no-pass", nil
	default:
		return "", "", fmt.Errorf("unknown tally mode %q", mode)
	}
}
