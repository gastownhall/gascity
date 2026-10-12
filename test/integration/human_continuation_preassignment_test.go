//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// The real command must claim clean work without assigning human siblings or
// creating formula attachments for them. The existing file-backed bd shim
// persists each mutation, so the assertions inspect fresh on-disk state.
func TestE2E_Hook_ClaimLeavesHumanContinuationSiblingUntouched(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(cityDir, ".gc", "beads.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, storePath)
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.Create(beads.Bead{Title: "workflow root", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	create := func(title, rootID, group string, labels []string) beads.Bead {
		t.Helper()
		row, err := store.Create(beads.Bead{Title: title, Type: "task", Labels: labels, Metadata: map[string]string{
			beadmeta.KindMetadataKey:              "task",
			beadmeta.RoutedToMetadataKey:          "worker",
			beadmeta.RootBeadIDMetadataKey:        rootID,
			beadmeta.ContinuationGroupMetadataKey: group,
		}})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	step := create("claimed step", root.ID, "body", nil)
	human := create("person decision", root.ID, "body", []string{beadmeta.HumanLabel})
	clean := create("clean successor", root.ID, "body", nil)
	otherRoot := create("other workflow", "another-root", "body", nil)
	otherGroup := create("other group", root.ID, "another-group", nil)
	before, err := store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	cityConfig := fmt.Sprintf(`[workspace]
name = "continuation-test"

[[agent]]
name = "worker"
start_command = "true"
work_query = "bd show %s --json"
`, step.ID)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	env := integrationEnvFor(t, t.TempDir(), t.TempDir(), false)
	env = filterEnvMany(env, "GC_TEMPLATE", "GC_ALIAS", "GC_SESSION_ID", "GC_SESSION_NAME", "GC_INSTANCE_TOKEN",
		"GC_SESSION_ORIGIN", "GC_HOOK_CALLBACK_LANE", "GC_MANAGED_SESSION_HOOK", "GC_HOOK_EVENT_NAME", "GC_DRAIN")
	env = replaceEnv(env, "GC_BEADS", "file")
	env = replaceEnv(env, "GC_CITY", cityDir)
	env = replaceEnv(env, "GC_AGENT", "worker")
	args := []string{"hook", "worker", "--claim", "--json"}
	out, err := runCommandStdout(cityDir, env, gcCommandTimeout(args), gcBinary, args...)
	if err != nil {
		t.Fatalf("gc hook --claim: %v\n%s", err, out)
	}
	var result struct {
		Action               string   `json:"action"`
		Reason               string   `json:"reason"`
		BeadID               string   `json:"bead_id"`
		Assignee             string   `json:"assignee"`
		ContinuationAssigned []string `json:"continuation_assigned"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("claim output: %v\n%s", err, out)
	}
	if result.Action != "work" || result.Reason != "claimed" || result.BeadID != step.ID || result.Assignee != "worker" {
		t.Fatalf("command did not claim the clean step: %+v", result)
	}
	if !reflect.DeepEqual(result.ContinuationAssigned, []string{clean.ID}) {
		t.Errorf("CLI continuation assignments = %v, want only %s", result.ContinuationAssigned, clean.ID)
	}
	fresh, err := beads.OpenFileStore(fsys.OSFS{}, storePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []beads.Bead{step, human, clean, otherRoot, otherGroup} {
		got, err := fresh.Get(row.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantOwner, wantStatus := "", "open"
		if row.ID == step.ID || row.ID == clean.ID {
			wantOwner = "worker"
		}
		if row.ID == step.ID {
			wantStatus = "in_progress"
		}
		if got.Assignee != wantOwner || got.Status != wantStatus {
			t.Errorf("%s persisted status/owner = %s/%q, want %s/%q", row.Title, got.Status, got.Assignee, wantStatus, wantOwner)
		}
		if row.ID != step.ID && (!maps.Equal(got.Metadata, row.Metadata) || !reflect.DeepEqual(got.Labels, row.Labels)) {
			t.Errorf("%s labels/metadata changed or acquired a formula attachment: %+v", row.Title, got)
		}
	}
	after, err := fresh.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil || len(after) != len(before) {
		t.Errorf("persisted population = %d, err=%v, want %d unchanged rows and no formula records", len(after), err, len(before))
	}
}
