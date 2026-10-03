package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEffectiveWorkQueryDoesNotServeMailMessages gives each assigned-message
// work-query tier a competing routed step. Mail records are not executable
// work: the worker must skip the assigned message and reach the claimable step.
func TestEffectiveWorkQueryDoesNotServeMailMessages(t *testing.T) {
	a := Agent{Name: "worker", Dir: "hello-world"}
	for _, tc := range []struct {
		name        string
		messageTier string
		beads       BeadsConfig
	}{
		{name: "assigned ready", messageTier: "assigned-ready"},
		{
			name:        "assigned ready with ephemeral semantics",
			messageTier: "assigned-ready",
			beads:       BeadsConfig{BDCompatibility: BeadsBDCompatibility105},
		},
		{name: "ephemeral ready", messageTier: "ephemeral-ready"},
		{name: "ephemeral ready type alias", messageTier: "ephemeral-ready-type-alias"},
		{name: "assigned in progress", messageTier: "assigned-in-progress"},
		{name: "ephemeral in progress", messageTier: "ephemeral-in-progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runEffectiveWorkQueryForBeads(t, a, tc.beads, map[string]string{
				"GC_SESSION_ID":     "worker-session",
				"GC_SESSION_ORIGIN": "ephemeral",
				"MESSAGE_TIER":      tc.messageTier,
			}, mailMessagePriorityFixture)

			var rows []struct {
				ID        string `json:"id"`
				IssueType string `json:"issue_type"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
				t.Fatalf("work query output is not JSON: %v (%q)", err, out)
			}
			if len(rows) != 1 || rows[0].ID != "routed-ready-step" || rows[0].IssueType != "step" {
				t.Fatalf("work query served a mail message or missed routed ready work: got %q, want routed-ready-step (step)", out)
			}
		})
	}
}

const mailMessagePriorityFixture = `#!/bin/sh
set -eu
case "$1" in
  list)
    case "$*" in
      *"--status in_progress"*"--assignee=worker-session"*)
        if [ "$MESSAGE_TIER" = "assigned-in-progress" ]; then
          case "$*" in
            *"--exclude-type=message"*) printf '[]' ;;
            *) printf '[{"id":"assigned-message","issue_type":"message","status":"in_progress","assignee":"worker-session"}]' ;;
          esac
        else
          printf '[]'
        fi
        ;;
      *) printf '[]' ;;
    esac
    ;;
  ready)
    case "$*" in
      *"--assignee=worker-session"*)
        if [ "$MESSAGE_TIER" = "assigned-ready" ]; then
          case "$*" in
            *"--exclude-type=message"*) printf '[]' ;;
            *) printf '[{"id":"assigned-message","issue_type":"message","status":"open","assignee":"worker-session"}]' ;;
          esac
        else
          printf '[]'
        fi
        ;;
      *"gc.routed_to=hello-world/worker"*)
        printf '[{"id":"routed-ready-step","issue_type":"step","status":"open"}]'
        ;;
      *) printf '[]' ;;
    esac
    ;;
  query)
    case "$*" in
      *"ephemeral=true AND status=open"*)
        if [ "$MESSAGE_TIER" = "ephemeral-ready" ]; then
          printf '[{"id":"assigned-message","issue_type":"message","status":"open","assignee":"worker-session","ephemeral":true,"dependency_count":0}]'
        elif [ "$MESSAGE_TIER" = "ephemeral-ready-type-alias" ]; then
          printf '[{"id":"assigned-message","type":"message","status":"open","assignee":"worker-session","ephemeral":true,"dependency_count":0}]'
        else
          printf '[]'
        fi
        ;;
      *"ephemeral=true AND status=in_progress"*)
        if [ "$MESSAGE_TIER" = "ephemeral-in-progress" ]; then
          printf '[{"id":"assigned-message","issue_type":"message","status":"in_progress","assignee":"worker-session","ephemeral":true}]'
        else
          printf '[]'
        fi
        ;;
      *) printf '[]' ;;
    esac
    ;;
  show)
    printf '[{"id":"assigned-message","dependencies":[]}]'
    ;;
  *) printf '[]' ;;
esac
`
