package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestHookClaimFencesObservedStepID: a session-bound pool hook reads a graph
// step candidate with gc.step_id=build; before the claim, the canonical row's
// step identity moves to test. The hook must not claim the stale observation:
// the claim has to be fenced to the candidate it read (step, assignee,
// generation), so no unconditional `bd update --claim` may be issued and the
// hook must not hand out work for the moved row.
func TestHookClaimFencesObservedStepID(t *testing.T) {
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })

	canonical := `[{"id":"work-1","title":"work","status":"open","issue_type":"task","created_at":"2026-11-21T00:00:00Z",` +
		`"metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.step_id":"test"}}]`
	var ran []string
	hookClaimCommandRunnerWithEnvContext = func(context.Context, map[string]string) beads.CommandRunner {
		return func(_ string, name string, args ...string) ([]byte, error) {
			ran = append(ran, name+" "+strings.Join(args, " "))
			switch {
			case name == "bd" && reflect.DeepEqual(args, []string{"update", "work-1", "--claim", "--json"}):
				// bd's claim CAS checks only status/assignee; it cannot see
				// that the step identity moved, so it lands.
				return []byte(strings.Replace(canonical, `"status":"open"`, `"status":"in_progress","assignee":"worker-1"`, 1)), nil
			case name == "bd" && len(args) >= 2 && args[0] == "show":
				return []byte(canonical), nil
			default:
				return nil, errors.New("unexpected command in repro")
			}
		}
	}

	observed, err := json.Marshal([]beads.Bead{{
		ID: "work-1", Status: "open",
		Metadata: map[string]string{"gc.routed_to": "worker", "gc.root_bead_id": "root-1", "gc.step_id": "build"},
	}})
	if err != nil {
		t.Fatalf("marshal candidate: %v", err)
	}
	wf := beads.NewMemStore()
	if _, err := wf.Create(beads.Bead{ID: "root-1", Title: "root", Status: "open"}); err != nil {
		t.Fatalf("create root: %v", err)
	}
	ops := hookClaimOps{
		Runner:            func(string, string) (string, error) { return string(observed), nil },
		DrainAck:          func(io.Writer) error { return nil },
		EmitClaimFailed:   func(string, hookClaimOptions, error) {},
		EmitClaimRejected: func(string, string, string) {},
		WorkflowStore:     func(context.Context, string, []string, string, string) beads.Store { return wf },
	}

	var stdout, stderr bytes.Buffer
	_ = doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Env:                []string{"GC_SESSION_ID=sess-1"},
		DrainAck:           true,
		JSON:               true,
	}, ops, &stdout, &stderr)

	for _, cmd := range ran {
		if cmd == "bd update work-1 --claim --json" {
			t.Fatalf("hook issued an unfenced claim for a candidate whose step moved build->test (observed step not a precondition); commands=%q stdout=%s stderr=%s", ran, stdout.String(), stderr.String())
		}
	}
	if strings.Contains(stdout.String(), `"action":"work"`) {
		t.Fatalf("hook handed out work for a moved step: %s", stdout.String())
	}
}
