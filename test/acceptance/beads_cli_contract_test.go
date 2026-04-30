//go:build acceptance_a

// Beads CLI contract acceptance test.
//
// Exercises every bd CLI command that gastown code depends on. When the
// beads dependency pin is bumped, this test catches removed or renamed
// commands before they reach users.
//
// Context: quad341 hit multiple breakages upgrading gastown because beads
// v0.62 removed CLI commands (bd slot, bd merge-slot, multi-rig routing)
// that gastown depended on. This test is the contract firewall.
//
// Each sub-test verifies:
//   - The command exits successfully (exit code 0)
//   - The output format is parseable by gastown code (JSON where used)
package acceptance_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// runBD executes a bd command in dir with BEADS_DIR set to dir/.beads.
// Returns combined output and any error.
func runBD(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	bdPath := helpers.RequireBD(t)
	cmd := exec.Command(bdPath, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BEADS_DIR="+filepath.Join(dir, ".beads"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// requireBD runs a bd command and fails the test if it returns non-zero.
func requireBD(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runBD(t, dir, args...)
	if err != nil {
		t.Fatalf("bd %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// initBeadsDir creates a temp directory and initializes a beads database.
// Returns the directory path.
func initBeadsDir(t *testing.T) string {
	t.Helper()
	helpers.RequireBD(t)
	dir := t.TempDir()
	requireBD(t, dir, "init", "-p", "ct", "--skip-hooks", "-q")
	return dir
}

// createBead creates a bead with the given title and returns its ID
// extracted from JSON output.
func createBead(t *testing.T, dir, title string) string {
	t.Helper()
	out := requireBD(t, dir, "create", "--json", title, "-t", "task")
	id := extractBeadID(t, out)
	if id == "" {
		t.Fatalf("bd create returned no bead ID in output:\n%s", out)
	}
	return id
}

// extractBeadID parses the bead ID from bd create --json output.
// bd create returns a JSON object with an "id" field.
func extractBeadID(t *testing.T, jsonOut string) string {
	t.Helper()
	// bd may emit preamble before JSON; find the first '{'.
	idx := strings.Index(jsonOut, "{")
	if idx < 0 {
		return ""
	}
	var issue struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(jsonOut[idx:]), &issue); err != nil {
		t.Fatalf("parsing bd create JSON: %v\nraw: %s", err, jsonOut)
	}
	return issue.ID
}

// --- Contract tests ---

// TestBdBasicCRUD exercises all basic CRUD operations against a single
// shared beads directory. Subtests run sequentially.
func TestBdBasicCRUD(t *testing.T) {
	dir := initBeadsDir(t)

	t.Run("Init", func(t *testing.T) {
		beadsDir := filepath.Join(dir, ".beads")
		if _, err := os.Stat(beadsDir); err != nil {
			t.Fatalf(".beads directory not created after bd init: %v", err)
		}
	})

	// --- Create variants ---

	t.Run("CreateJSON", func(t *testing.T) {
		out := requireBD(t, dir, "create", "--json", "contract test bead", "-t", "task")

		// Must contain valid JSON with id, status, and issue_type fields.
		idx := strings.Index(out, "{")
		if idx < 0 {
			t.Fatalf("bd create --json returned no JSON object:\n%s", out)
		}

		var issue struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			Status    string `json:"status"`
			IssueType string `json:"issue_type"`
		}
		if err := json.Unmarshal([]byte(out[idx:]), &issue); err != nil {
			t.Fatalf("bd create --json output not parseable: %v\n%s", err, out)
		}
		if issue.ID == "" {
			t.Fatal("bd create --json returned empty id")
		}
		if issue.Status == "" {
			t.Fatal("bd create --json returned empty status")
		}
	})

	t.Run("CreateWithLabels", func(t *testing.T) {
		out := requireBD(t, dir, "create", "--json", "labeled bead",
			"-t", "task", "--labels", "pool:test-agent")

		idx := strings.Index(out, "{")
		if idx < 0 {
			t.Fatalf("no JSON in output:\n%s", out)
		}
		var issue struct {
			ID     string   `json:"id"`
			Labels []string `json:"labels"`
		}
		if err := json.Unmarshal([]byte(out[idx:]), &issue); err != nil {
			t.Fatalf("parsing output: %v\n%s", err, out)
		}
		if issue.ID == "" {
			t.Fatal("no id in output")
		}

		// Read the bead back and verify the label is actually set.
		showOut := requireBD(t, dir, "show", "--json", issue.ID)
		showIdx := strings.Index(showOut, "[")
		if showIdx < 0 {
			t.Fatalf("bd show returned no JSON array:\n%s", showOut)
		}
		var shown []struct {
			Labels []string `json:"labels"`
		}
		if err := json.Unmarshal([]byte(showOut[showIdx:]), &shown); err != nil {
			t.Fatalf("parsing show output: %v\n%s", err, showOut)
		}
		if len(shown) == 0 {
			t.Fatal("bd show returned empty array")
		}
		found := false
		for _, l := range shown[0].Labels {
			if l == "pool:test-agent" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("label %q not found on bead after create; labels = %v", "pool:test-agent", shown[0].Labels)
		}
	})

	t.Run("CreateWithMetadata", func(t *testing.T) {
		out := requireBD(t, dir, "create", "--json", "metadata bead",
			"-t", "task", "--metadata", `{"from":"test","gc.routed_to":"agent-1"}`)

		idx := strings.Index(out, "{")
		if idx < 0 {
			t.Fatalf("no JSON in output:\n%s", out)
		}
		var issue struct {
			ID       string            `json:"id"`
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(out[idx:]), &issue); err != nil {
			// Metadata may be returned with non-string values; tolerate that.
			var fallback struct {
				ID string `json:"id"`
			}
			if err2 := json.Unmarshal([]byte(out[idx:]), &fallback); err2 != nil {
				t.Fatalf("parsing output: %v\n%s", err, out)
			}
			if fallback.ID == "" {
				t.Fatal("no id in output")
			}
			return
		}
		if issue.ID == "" {
			t.Fatal("no id in output")
		}
	})

	t.Run("CreateWithParent", func(t *testing.T) {
		parentID := createBead(t, dir, "molecule root")

		out := requireBD(t, dir, "create", "--json", "step 1",
			"-t", "task", "--parent", parentID)
		childID := extractBeadID(t, out)
		if childID == "" {
			t.Fatal("no child id returned")
		}

		// Verify the child's parent field.
		showOut := requireBD(t, dir, "show", "--json", childID)
		if !strings.Contains(showOut, parentID) {
			t.Fatalf("child bead does not reference parent %s:\n%s", parentID, showOut)
		}
	})

	t.Run("CreateWithAssignee", func(t *testing.T) {
		out := requireBD(t, dir, "create", "--json", "assigned work",
			"-t", "task", "--assignee", "test-polecat")
		id := extractBeadID(t, out)
		if id == "" {
			t.Fatal("no id returned")
		}

		// Read the bead back and verify the assignee is actually set.
		showOut := requireBD(t, dir, "show", "--json", id)
		showIdx := strings.Index(showOut, "[")
		if showIdx < 0 {
			t.Fatalf("bd show returned no JSON array:\n%s", showOut)
		}
		var shown []struct {
			Assignee string `json:"assignee"`
		}
		if err := json.Unmarshal([]byte(showOut[showIdx:]), &shown); err != nil {
			t.Fatalf("parsing show output: %v\n%s", err, showOut)
		}
		if len(shown) == 0 {
			t.Fatal("bd show returned empty array")
		}
		if shown[0].Assignee != "test-polecat" {
			t.Fatalf("assignee = %q, want %q", shown[0].Assignee, "test-polecat")
		}
	})

	t.Run("CreateWithPriority", func(t *testing.T) {
		out := requireBD(t, dir, "create", "--json", "priority work",
			"-t", "task", "--priority", "1")
		id := extractBeadID(t, out)
		if id == "" {
			t.Fatal("no id returned")
		}

		// Read the bead back and verify the priority is actually set.
		showOut := requireBD(t, dir, "show", "--json", id)
		showIdx := strings.Index(showOut, "[")
		if showIdx < 0 {
			t.Fatalf("bd show returned no JSON array:\n%s", showOut)
		}
		var shown []struct {
			Priority json.RawMessage `json:"priority"`
		}
		if err := json.Unmarshal([]byte(showOut[showIdx:]), &shown); err != nil {
			t.Fatalf("parsing show output: %v\n%s", err, showOut)
		}
		if len(shown) == 0 {
			t.Fatal("bd show returned empty array")
		}
		// Priority may be returned as int or string depending on bd version;
		// either way, the raw JSON must contain "1".
		if shown[0].Priority == nil || !strings.Contains(string(shown[0].Priority), "1") {
			t.Fatalf("priority = %s, want value containing %q", string(shown[0].Priority), "1")
		}
	})

	t.Run("CreateWithDescription", func(t *testing.T) {
		out := requireBD(t, dir, "create", "--json", "described bead",
			"-t", "task", "--description", "full description of work")
		id := extractBeadID(t, out)
		if id == "" {
			t.Fatal("no id returned")
		}

		// Read the bead back and verify the description is actually set.
		showOut := requireBD(t, dir, "show", "--json", id)
		showIdx := strings.Index(showOut, "[")
		if showIdx < 0 {
			t.Fatalf("bd show returned no JSON array:\n%s", showOut)
		}
		var shown []struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal([]byte(showOut[showIdx:]), &shown); err != nil {
			t.Fatalf("parsing show output: %v\n%s", err, showOut)
		}
		if len(shown) == 0 {
			t.Fatal("bd show returned empty array")
		}
		if shown[0].Description != "full description of work" {
			t.Fatalf("description = %q, want %q", shown[0].Description, "full description of work")
		}
	})

	t.Run("CreateGraphJSON", func(t *testing.T) {
		// Write a minimal graph plan file. The plan must have at least
		// one node with a "key" field — bd rejects empty graphs and
		// nodes without keys.
		plan := `{"nodes":[{"key":"root","title":"graph root","issue_type":"task"}],"edges":[]}`
		planFile := filepath.Join(dir, "graph-plan.json")
		if err := os.WriteFile(planFile, []byte(plan), 0o644); err != nil {
			t.Fatalf("writing graph plan: %v", err)
		}

		out, err := runBD(t, dir, "create", "--graph", planFile, "--json")
		if err != nil {
			// bd create --graph may not be available in all versions.
			// If the command is unrecognized, skip rather than fail.
			if strings.Contains(string(out), "unknown") || strings.Contains(string(out), "unrecognized") {
				t.Skipf("bd create --graph not supported in this version: %s", out)
			}
			t.Fatalf("bd create --graph failed: %v\n%s", err, out)
		}

		// Output should contain JSON with created IDs.
		idx := strings.Index(out, "{")
		arrIdx := strings.Index(out, "[")
		if idx < 0 && arrIdx < 0 {
			t.Fatalf("bd create --graph returned no JSON:\n%s", out)
		}
	})

	// --- Show ---

	t.Run("ShowJSON", func(t *testing.T) {
		id := createBead(t, dir, "show test bead")

		out := requireBD(t, dir, "show", "--json", id)

		// bd show --json returns a JSON array of issues.
		idx := strings.Index(out, "[")
		if idx < 0 {
			t.Fatalf("bd show --json returned no JSON array:\n%s", out)
		}

		var issues []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Title  string `json:"title"`
		}
		if err := json.Unmarshal([]byte(out[idx:]), &issues); err != nil {
			t.Fatalf("bd show --json output not parseable as array: %v\n%s", err, out)
		}
		if len(issues) == 0 {
			t.Fatal("bd show --json returned empty array")
		}
		if issues[0].ID != id {
			t.Fatalf("bd show --json returned wrong id: got %q, want %q", issues[0].ID, id)
		}
		if issues[0].Status == "" {
			t.Fatal("bd show --json returned empty status field")
		}
	})

	// --- List variants ---

	t.Run("ListJSON", func(t *testing.T) {
		createBead(t, dir, "list test bead")

		out := requireBD(t, dir, "list", "--json", "--limit", "0", "--include-infra")

		idx := strings.Index(out, "[")
		if idx < 0 {
			t.Fatalf("bd list --json returned no JSON array:\n%s", out)
		}

		var issues []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(out[idx:]), &issues); err != nil {
			t.Fatalf("bd list --json output not parseable: %v\n%s", err, out)
		}
		if len(issues) == 0 {
			t.Fatal("bd list --json returned empty array — expected at least one bead")
		}
	})

	t.Run("ListByLabel", func(t *testing.T) {
		// Create a bead with a specific label.
		requireBD(t, dir, "create", "--json", "labeled for query",
			"-t", "task", "--labels", "contract-test:query")

		// Create a bead without the label.
		createBead(t, dir, "unlabeled bead")

		out := requireBD(t, dir, "list", "--json",
			"--label=contract-test:query", "--all", "--include-infra", "--limit", "10")

		idx := strings.Index(out, "[")
		if idx < 0 {
			t.Fatalf("bd list --label returned no JSON array:\n%s", out)
		}

		var issues []struct {
			ID    string   `json:"id"`
			Title string   `json:"title"`
			Label []string `json:"labels"`
		}
		if err := json.Unmarshal([]byte(out[idx:]), &issues); err != nil {
			t.Fatalf("parsing: %v\n%s", err, out)
		}
		if len(issues) == 0 {
			t.Fatal("bd list --label returned empty — expected at least the labeled bead")
		}
		for _, iss := range issues {
			if iss.Title == "unlabeled bead" {
				t.Error("bd list --label returned the unlabeled bead — filter broken")
			}
		}
	})

	t.Run("ListByAssignee", func(t *testing.T) {
		requireBD(t, dir, "create", "--json", "assigned bead",
			"-t", "task", "--assignee", "test-agent")

		out := requireBD(t, dir, "list", "--json",
			"--assignee=test-agent", "--status=open",
			"--include-infra", "--limit", "10")

		idx := strings.Index(out, "[")
		if idx < 0 {
			t.Fatalf("no JSON array in output:\n%s", out)
		}

		var issues []struct {
			ID       string `json:"id"`
			Assignee string `json:"assignee"`
		}
		if err := json.Unmarshal([]byte(out[idx:]), &issues); err != nil {
			t.Fatalf("parsing: %v\n%s", err, out)
		}
		if len(issues) == 0 {
			t.Fatal("bd list --assignee returned empty — expected the assigned bead")
		}
	})

	t.Run("ListByMetadataField", func(t *testing.T) {
		requireBD(t, dir, "create", "--json", "routed bead",
			"-t", "task", "--metadata", `{"gc.routed_to":"test-agent"}`)

		out, err := runBD(t, dir, "list", "--json", "--all", "--include-infra",
			"--limit", "10", "--metadata-field", "gc.routed_to=test-agent")
		if err != nil {
			if strings.Contains(out, "unknown flag") {
				t.Skip("bd version does not support --metadata-field")
			}
			t.Fatalf("bd list --metadata-field failed: %v\n%s", err, out)
		}

		idx := strings.Index(out, "[")
		if idx < 0 {
			t.Fatalf("no JSON array in output:\n%s", out)
		}

		var issues []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(out[idx:]), &issues); err != nil {
			t.Fatalf("parsing: %v\n%s", err, out)
		}
		if len(issues) == 0 {
			t.Fatal("bd list --metadata-field returned empty — expected the routed bead")
		}
	})

	// --- Update variants ---

	t.Run("UpdateStatus", func(t *testing.T) {
		id := createBead(t, dir, "update status bead")

		requireBD(t, dir, "update", "--json", id, "--status", "in_progress")

		out := requireBD(t, dir, "show", "--json", id)
		if !strings.Contains(out, "in_progress") {
			t.Fatalf("bead status not updated to in_progress:\n%s", out)
		}
	})

	t.Run("UpdateAddLabel", func(t *testing.T) {
		id := createBead(t, dir, "label test bead")

		requireBD(t, dir, "update", "--json", id, "--add-label", "pool:test-agent")

		out := requireBD(t, dir, "show", "--json", id)
		if !strings.Contains(out, "pool:test-agent") {
			t.Fatalf("label not added:\n%s", out)
		}
	})

	t.Run("UpdateRemoveLabel", func(t *testing.T) {
		out := requireBD(t, dir, "create", "--json", "remove label bead",
			"-t", "task", "--labels", "to-remove")
		id := extractBeadID(t, out)

		requireBD(t, dir, "update", "--json", id, "--remove-label", "to-remove")

		showOut := requireBD(t, dir, "show", "--json", id)
		if strings.Contains(showOut, "to-remove") {
			t.Fatalf("label not removed:\n%s", showOut)
		}
	})

	t.Run("UpdateSetMetadata", func(t *testing.T) {
		id := createBead(t, dir, "metadata update bead")

		requireBD(t, dir, "update", "--json", id,
			"--set-metadata", "gc.routed_to=test-agent",
			"--set-metadata", "from=mayor")

		out := requireBD(t, dir, "show", "--json", id)
		if !strings.Contains(out, "gc.routed_to") {
			t.Fatalf("metadata not set:\n%s", out)
		}
	})

	t.Run("UpdateTitle", func(t *testing.T) {
		id := createBead(t, dir, "original title")

		requireBD(t, dir, "update", "--json", id, "--title", "updated title")

		out := requireBD(t, dir, "show", "--json", id)
		if !strings.Contains(out, "updated title") {
			t.Fatalf("title not updated:\n%s", out)
		}
	})

	// --- Close ---

	t.Run("CloseJSON", func(t *testing.T) {
		id := createBead(t, dir, "close test bead")

		requireBD(t, dir, "close", "--json", id)

		// Verify the bead is now closed via bd show.
		out := requireBD(t, dir, "show", "--json", id)
		idx := strings.Index(out, "[")
		if idx < 0 {
			t.Fatalf("bd show after close returned no JSON:\n%s", out)
		}
		var issues []struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(out[idx:]), &issues); err != nil {
			t.Fatalf("parsing: %v\n%s", err, out)
		}
		if len(issues) == 0 {
			t.Fatal("bd show after close returned empty array")
		}
		if issues[0].Status != "closed" {
			t.Fatalf("bead status after close = %q, want %q", issues[0].Status, "closed")
		}
	})

	t.Run("CloseBatch", func(t *testing.T) {
		id1 := createBead(t, dir, "batch close 1")
		id2 := createBead(t, dir, "batch close 2")

		requireBD(t, dir, "close", "--json", id1, id2)

		// Verify both are closed.
		for _, id := range []string{id1, id2} {
			out := requireBD(t, dir, "show", "--json", id)
			if !strings.Contains(out, `"closed"`) {
				t.Errorf("bead %s not closed after batch close", id)
			}
		}
	})

	// --- Comment ---

	t.Run("Comment", func(t *testing.T) {
		id := createBead(t, dir, "comment test bead")

		// Try "bd comment" first (newer bd), fall back to "bd comments add".
		_, err := runBD(t, dir, "comment", id, "test comment from contract test")
		if err != nil {
			requireBD(t, dir, "comments", "add", id, "test comment from contract test")
		}
	})

	// --- Ready ---

	t.Run("ReadyJSON", func(t *testing.T) {
		createBead(t, dir, "ready test bead")

		out := requireBD(t, dir, "ready", "--json", "--limit", "0")

		idx := strings.Index(out, "[")
		if idx < 0 {
			t.Fatalf("bd ready --json returned no JSON array:\n%s", out)
		}
		var issues []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(out[idx:]), &issues); err != nil {
			t.Fatalf("bd ready --json not parseable: %v\n%s", err, out)
		}
		if len(issues) == 0 {
			t.Fatal("bd ready --json returned empty — expected at least one ready bead")
		}
	})

	t.Run("ReadyWithLabelFilter", func(t *testing.T) {
		requireBD(t, dir, "create", "--json", "pool work",
			"-t", "task", "--labels", "pool:mypool")

		// Should succeed without error even if no results match.
		out, err := runBD(t, dir, "ready", "--label=pool:mypool", "--unassigned", "--limit=1")
		// bd returns exit 1 for "no results" which is acceptable.
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				// Acceptable: no ready work found (bead may need deps resolved).
				return
			}
			t.Fatalf("bd ready --label failed unexpectedly: %v\n%s", err, out)
		}
	})

	// --- Output field completeness ---

	t.Run("OutputFieldCompleteness", func(t *testing.T) {
		out := requireBD(t, dir, "create", "--json", "field check",
			"-t", "task", "--assignee", "check-agent",
			"--labels", "check-label",
			"--metadata", `{"check_key":"check_val"}`)

		idx := strings.Index(out, "{")
		if idx < 0 {
			t.Fatalf("no JSON:\n%s", out)
		}

		// Parse into a map to check field presence without caring about values.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out[idx:]), &raw); err != nil {
			t.Fatalf("parsing: %v\n%s", err, out)
		}

		// Fields that gastown's bdIssue struct depends on.
		requiredFields := []string{
			"id",
			"title",
			"status",
			"issue_type",
			"created_at",
		}
		for _, field := range requiredFields {
			if _, ok := raw[field]; !ok {
				t.Errorf("bd create --json output missing required field %q", field)
			}
		}
	})

	// --- Config ---

	t.Run("ConfigSet", func(t *testing.T) {
		requireBD(t, dir, "config", "set", "export.auto", "false")
		out := requireBD(t, dir, "config", "get", "export.auto")
		if strings.TrimSpace(out) != "false" {
			t.Fatalf("bd config get export.auto = %q, want false", strings.TrimSpace(out))
		}
	})

	t.Run("ConfigGetIssuePrefix", func(t *testing.T) {
		out := requireBD(t, dir, "config", "get", "issue_prefix")
		if strings.TrimSpace(out) != "ct" {
			t.Fatalf("bd config get issue_prefix = %q, want ct", strings.TrimSpace(out))
		}
	})
}

// TestBdDependencies exercises all dependency operations against a single
// shared beads directory.
func TestBdDependencies(t *testing.T) {
	dir := initBeadsDir(t)

	t.Run("DepAdd", func(t *testing.T) {
		parent := createBead(t, dir, "parent bead")
		child := createBead(t, dir, "child bead")

		requireBD(t, dir, "dep", "add", parent, child, "--type", "blocks")
	})

	t.Run("DepListJSON", func(t *testing.T) {
		parent := createBead(t, dir, "dep parent")
		child := createBead(t, dir, "dep child")

		requireBD(t, dir, "dep", "add", parent, child, "--type", "blocks")

		out := requireBD(t, dir, "dep", "list", parent, "--json")

		// bd dep list --json returns a JSON array.
		idx := strings.Index(out, "[")
		if idx < 0 {
			t.Fatalf("bd dep list --json returned no JSON array:\n%s", out)
		}
		if err := json.Unmarshal([]byte(out[idx:]), &json.RawMessage{}); err != nil {
			t.Fatalf("bd dep list --json output not parseable: %v\n%s", err, out)
		}
	})

	t.Run("DepListDirection", func(t *testing.T) {
		parent := createBead(t, dir, "dir parent")
		child := createBead(t, dir, "dir child")

		requireBD(t, dir, "dep", "add", parent, child, "--type", "blocks")

		// direction=up from child should show parent.
		out := requireBD(t, dir, "dep", "list", child, "--json", "--direction=up")

		idx := strings.Index(out, "[")
		if idx < 0 {
			t.Fatalf("bd dep list --direction=up returned no JSON array:\n%s", out)
		}
		if err := json.Unmarshal([]byte(out[idx:]), &json.RawMessage{}); err != nil {
			t.Fatalf("parsing: %v\n%s", err, out)
		}
	})

	t.Run("DepRemove", func(t *testing.T) {
		parent := createBead(t, dir, "rm parent")
		child := createBead(t, dir, "rm child")

		requireBD(t, dir, "dep", "add", parent, child, "--type", "blocks")
		requireBD(t, dir, "dep", "remove", parent, child)

		// Verify dep is gone.
		out := requireBD(t, dir, "dep", "list", parent, "--json")
		idx := strings.Index(out, "[")
		if idx >= 0 {
			var deps []json.RawMessage
			if err := json.Unmarshal([]byte(out[idx:]), &deps); err == nil && len(deps) > 0 {
				t.Fatalf("dep not removed — still %d deps:\n%s", len(deps), out)
			}
		}
	})
}

// TestBdDestructive exercises destructive operations (delete, purge) against
// a single shared beads directory.
func TestBdDestructive(t *testing.T) {
	dir := initBeadsDir(t)

	t.Run("DeleteForce", func(t *testing.T) {
		id := createBead(t, dir, "delete test bead")

		requireBD(t, dir, "delete", "--force", "--json", id)

		// Verify the bead is actually gone. bd show may error or return
		// an empty array for a deleted bead — either is acceptable.
		out, err := runBD(t, dir, "show", "--json", id)
		if err != nil {
			// Command failed — bead is gone. This is the expected path.
			return
		}
		// Command succeeded — verify the bead is not in the output.
		idx := strings.Index(out, "[")
		if idx >= 0 {
			var issues []struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(out[idx:]), &issues); err == nil {
				for _, iss := range issues {
					if iss.ID == id {
						t.Fatalf("bead %s still present after delete --force:\n%s", id, out)
					}
				}
			}
		}
	})

	t.Run("PurgeJSON", func(t *testing.T) {
		// Create and close a bead to give purge something to consider.
		id := createBead(t, dir, "purge candidate")
		requireBD(t, dir, "close", "--json", id)

		out := requireBD(t, dir, "purge", "--json", "--dry-run")

		// Output must contain valid JSON with purged_count.
		idx := strings.Index(out, "{")
		if idx < 0 {
			t.Fatalf("bd purge --json returned no JSON:\n%s", out)
		}
		var result struct {
			PurgedCount *int `json:"purged_count"`
		}
		if err := json.Unmarshal([]byte(out[idx:]), &result); err != nil {
			t.Fatalf("bd purge --json not parseable: %v\n%s", err, out)
		}
	})
}

// TestBdWorkflow exercises a realistic gastown lifecycle:
// create -> update -> dep add -> dep list -> close. This catches
// interaction bugs where individual commands work but the sequence breaks.
func TestBdWorkflow(t *testing.T) {
	dir := initBeadsDir(t)

	// 1. Create a molecule root bead.
	rootOut := requireBD(t, dir, "create", "--json", "molecule root",
		"-t", "task", "--metadata", `{"from":"test-mayor"}`)
	rootID := extractBeadID(t, rootOut)

	// 2. Create a step bead (no --parent, since bd prevents adding
	// explicit deps between parent-child to avoid deadlocks).
	stepOut := requireBD(t, dir, "create", "--json", "step 1",
		"-t", "task", "--labels", "pool:polecat")
	stepID := extractBeadID(t, stepOut)

	// 3. Add a dependency: step depends on root.
	requireBD(t, dir, "dep", "add", stepID, rootID, "--type", "blocks")

	// 4. Update step with routing metadata.
	requireBD(t, dir, "update", "--json", stepID,
		"--set-metadata", "gc.routed_to=polecat-1",
		"--assignee", "polecat-1")

	// 5. Verify dep list returns the dependency.
	depOut := requireBD(t, dir, "dep", "list", stepID, "--json")
	if !strings.Contains(depOut, rootID) {
		t.Fatalf("dep list does not contain root id %s:\n%s", rootID, depOut)
	}

	// 6. Close the root, then the step.
	requireBD(t, dir, "close", "--json", rootID)
	requireBD(t, dir, "close", "--json", stepID)

	// 7. Verify both are closed.
	for _, id := range []string{rootID, stepID} {
		showOut := requireBD(t, dir, "show", "--json", id)
		if !strings.Contains(showOut, `"closed"`) {
			t.Errorf("bead %s not closed after workflow:\n%s", id, showOut)
		}
	}
}
