package dispatch

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// ---------------------------------------------------------------------------
// extractVote tests
// ---------------------------------------------------------------------------

func TestExtractVote_OutcomeField(t *testing.T) {
	t.Parallel()
	voter := beads.Bead{
		ID:       "v1",
		Metadata: map[string]string{"gc.outcome": "pass"},
	}
	got, err := extractVote(voter, "")
	if err != nil {
		t.Fatalf("extractVote: %v", err)
	}
	if got != "pass" {
		t.Errorf("got %q, want pass", got)
	}
}

func TestExtractVote_JSONField(t *testing.T) {
	t.Parallel()
	out, _ := json.Marshal(map[string]interface{}{"answer": "yes"})
	voter := beads.Bead{
		ID:       "v1",
		Metadata: map[string]string{"gc.output_json": string(out)},
	}
	got, err := extractVote(voter, "answer")
	if err != nil {
		t.Fatalf("extractVote: %v", err)
	}
	if got != "yes" {
		t.Errorf("got %q, want yes", got)
	}
}

func TestExtractVote_NestedJSONField(t *testing.T) {
	t.Parallel()
	out, _ := json.Marshal(map[string]interface{}{"result": map[string]interface{}{"verdict": "accept"}})
	voter := beads.Bead{
		ID:       "v1",
		Metadata: map[string]string{"gc.output_json": string(out)},
	}
	got, err := extractVote(voter, "result.verdict")
	if err != nil {
		t.Fatalf("extractVote: %v", err)
	}
	if got != "accept" {
		t.Errorf("got %q, want accept", got)
	}
}

func TestExtractVote_MissingField(t *testing.T) {
	t.Parallel()
	out, _ := json.Marshal(map[string]interface{}{"other": "x"})
	voter := beads.Bead{
		ID:       "v1",
		Metadata: map[string]string{"gc.output_json": string(out)},
	}
	_, err := extractVote(voter, "answer")
	if err == nil {
		t.Error("expected error for missing vote_field")
	}
}

// ---------------------------------------------------------------------------
// tallyVotes tests
// ---------------------------------------------------------------------------

func TestTallyVotes_MajorityPass(t *testing.T) {
	t.Parallel()
	outcome, result, err := tallyVotes([]string{"yes", "yes", "no"}, "majority")
	if err != nil || outcome != "pass" || result != "yes" {
		t.Errorf("majority pass: outcome=%q result=%q err=%v", outcome, result, err)
	}
}

func TestTallyVotes_MajorityFail_Tie(t *testing.T) {
	t.Parallel()
	outcome, result, err := tallyVotes([]string{"yes", "no"}, "majority")
	if err != nil || outcome != "fail" || result != "no-majority" {
		t.Errorf("majority tie: outcome=%q result=%q err=%v", outcome, result, err)
	}
}

func TestTallyVotes_UnanimousPass(t *testing.T) {
	t.Parallel()
	outcome, result, err := tallyVotes([]string{"accept", "accept", "accept"}, "unanimous")
	if err != nil || outcome != "pass" || result != "accept" {
		t.Errorf("unanimous pass: outcome=%q result=%q err=%v", outcome, result, err)
	}
}

func TestTallyVotes_UnanimousFail(t *testing.T) {
	t.Parallel()
	outcome, _, err := tallyVotes([]string{"accept", "reject"}, "unanimous")
	if err != nil || outcome != "fail" {
		t.Errorf("unanimous fail: outcome=%q err=%v", outcome, err)
	}
}

func TestTallyVotes_AnyPassTrue(t *testing.T) {
	t.Parallel()
	outcome, _, err := tallyVotes([]string{"fail", "pass", "fail"}, "any-pass")
	if err != nil || outcome != "pass" {
		t.Errorf("any-pass: outcome=%q err=%v", outcome, err)
	}
}

func TestTallyVotes_AnyPassFalse(t *testing.T) {
	t.Parallel()
	outcome, _, err := tallyVotes([]string{"fail", "fail"}, "any-pass")
	if err != nil || outcome != "fail" {
		t.Errorf("any-pass all-fail: outcome=%q err=%v", outcome, err)
	}
}

func TestTallyVotes_NoVoters(t *testing.T) {
	t.Parallel()
	outcome, result, err := tallyVotes(nil, "majority")
	if err != nil || outcome != "pass" || result != "no-voters" {
		t.Errorf("no voters: outcome=%q result=%q err=%v", outcome, result, err)
	}
}

func TestTallyVotes_UnknownMode(t *testing.T) {
	t.Parallel()
	_, _, err := tallyVotes([]string{"yes"}, "badmode")
	if err == nil {
		t.Error("expected error for unknown mode")
	}
}

// ---------------------------------------------------------------------------
// processTallyControl integration test
// ---------------------------------------------------------------------------

func TestProcessTallyControl_MajorityPass(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})

	// Source step (ask).
	ask := mustCreate(t, store, beads.Bead{
		Title: "ask",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-vote.ask",
		},
	})
	mustClose(t, store, ask.ID)

	// Fanout control bead (ask-fanout) — already closed.
	fanout := mustCreate(t, store, beads.Bead{
		Title: "ask-fanout",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-vote.ask-fanout",
			"gc.control_for":  "ask",
		},
	})

	// Three voter sink beads.
	makeVoter := func(answer string) {
		out, _ := json.Marshal(map[string]string{"answer": answer})
		v := mustCreate(t, store, beads.Bead{
			Title: "voter",
			Metadata: map[string]string{
				"gc.root_bead_id": root.ID,
				"gc.output_json":  string(out),
				"gc.outcome":      "pass",
			},
		})
		mustClose(t, store, v.ID)
		mustDep(t, store, fanout.ID, v.ID, "blocks")
	}
	makeVoter("yes")
	makeVoter("yes")
	makeVoter("no")

	mustClose(t, store, fanout.ID)

	// Tally control bead.
	tally := mustCreate(t, store, beads.Bead{
		Title: "ask-tally",
		Metadata: map[string]string{
			"gc.kind":         "tally",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-vote.ask-tally",
			"gc.control_for":  "ask",
			"gc.tally_mode":   "majority",
			"gc.vote_field":   "answer",
		},
	})
	mustDep(t, store, tally.ID, fanout.ID, "blocks")

	result, err := processTallyControl(store, mustGet(t, store, tally.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("processTallyControl: %v", err)
	}
	if !result.Processed {
		t.Error("expected Processed=true")
	}
	if result.Action != "tally-pass" {
		t.Errorf("Action = %q, want tally-pass", result.Action)
	}

	after := mustGet(t, store, tally.ID)
	if after.Status != "closed" {
		t.Errorf("tally status = %q, want closed", after.Status)
	}
	if after.Metadata["gc.outcome"] != "pass" {
		t.Errorf("gc.outcome = %q, want pass", after.Metadata["gc.outcome"])
	}
	if after.Metadata["gc.tally_result"] != "yes" {
		t.Errorf("gc.tally_result = %q, want yes", after.Metadata["gc.tally_result"])
	}
}

func TestProcessTallyControl_UnanimousFail(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	fanout := mustCreate(t, store, beads.Bead{
		Title: "ask-fanout",
		Metadata: map[string]string{
			"gc.kind":         "fanout",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-vote.ask-fanout",
			"gc.control_for":  "ask",
		},
	})

	for _, ans := range []string{"yes", "no"} {
		out, _ := json.Marshal(map[string]string{"answer": ans})
		v := mustCreate(t, store, beads.Bead{
			Metadata: map[string]string{
				"gc.root_bead_id": root.ID,
				"gc.output_json":  string(out),
			},
		})
		mustClose(t, store, v.ID)
		mustDep(t, store, fanout.ID, v.ID, "blocks")
	}
	mustClose(t, store, fanout.ID)

	tally := mustCreate(t, store, beads.Bead{
		Title: "ask-tally",
		Metadata: map[string]string{
			"gc.kind":         "tally",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-vote.ask-tally",
			"gc.control_for":  "ask",
			"gc.tally_mode":   "unanimous",
			"gc.vote_field":   "answer",
		},
	})
	mustDep(t, store, tally.ID, fanout.ID, "blocks")

	result, err := processTallyControl(store, mustGet(t, store, tally.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("processTallyControl: %v", err)
	}
	if result.Action != "tally-fail" {
		t.Errorf("Action = %q, want tally-fail", result.Action)
	}
}
