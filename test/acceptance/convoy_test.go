//go:build acceptance_a

// Convoy command acceptance tests.
//
// These exercise gc convoy create, list, status, target, close, check,
// stranded, and land as a black box. Convoys are batch work tracking
// containers that group related issues for coordinated delivery.
//
// Tests are grouped to minimize gc init calls: one city per group.
package acceptance_test

import (
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// TestConvoyErrors validates all error paths using a single city.
// None of these subtests mutate convoy state, so order doesn't matter.
func TestConvoyErrors(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("NoSubcommand", func(t *testing.T) {
		out, err := c.GC("convoy")
		if err == nil {
			t.Fatal("expected error for bare 'gc convoy', got success")
		}
		if !strings.Contains(out, "missing subcommand") {
			t.Errorf("expected 'missing subcommand' message, got:\n%s", out)
		}
	})

	t.Run("UnknownSubcommand", func(t *testing.T) {
		out, err := c.GC("convoy", "explode")
		if err == nil {
			t.Fatal("expected error for unknown subcommand, got success")
		}
		if !strings.Contains(out, "unknown subcommand") {
			t.Errorf("expected 'unknown subcommand' message, got:\n%s", out)
		}
	})

	t.Run("Create_MissingName", func(t *testing.T) {
		out, err := c.GC("convoy", "create")
		if err == nil {
			t.Fatal("expected error for convoy create without name, got success")
		}
		if !strings.Contains(out, "missing convoy name") {
			t.Errorf("expected 'missing convoy name' error, got:\n%s", out)
		}
	})

	t.Run("Status_MissingID", func(t *testing.T) {
		out, err := c.GC("convoy", "status")
		if err == nil {
			t.Fatal("expected error for convoy status without ID, got success")
		}
		if !strings.Contains(out, "missing convoy ID") {
			t.Errorf("expected 'missing convoy ID' error, got:\n%s", out)
		}
	})

	t.Run("Status_Nonexistent", func(t *testing.T) {
		_, err := c.GC("convoy", "status", "gc-99999")
		if err == nil {
			t.Fatal("expected error for nonexistent convoy, got success")
		}
	})

	t.Run("Close_MissingID", func(t *testing.T) {
		out, err := c.GC("convoy", "close")
		if err == nil {
			t.Fatal("expected error for convoy close without ID, got success")
		}
		if !strings.Contains(out, "missing convoy ID") {
			t.Errorf("expected 'missing convoy ID' error, got:\n%s", out)
		}
	})

	t.Run("Close_Nonexistent", func(t *testing.T) {
		_, err := c.GC("convoy", "close", "gc-99999")
		if err == nil {
			t.Fatal("expected error for closing nonexistent convoy, got success")
		}
	})

	t.Run("Target_MissingArgs", func(t *testing.T) {
		// cobra.ExactArgs(2) rejects this before our handler runs.
		_, err := c.GC("convoy", "target")
		if err == nil {
			t.Fatal("expected error for convoy target without args, got success")
		}
	})

	t.Run("Land_MissingID", func(t *testing.T) {
		// cobra.ExactArgs(1) rejects this before our handler runs.
		_, err := c.GC("convoy", "land")
		if err == nil {
			t.Fatal("expected error for convoy land without ID, got success")
		}
	})

	t.Run("Add_MissingArgs", func(t *testing.T) {
		out, err := c.GC("convoy", "add")
		if err == nil {
			t.Fatal("expected error for convoy add without args, got success")
		}
		if !strings.Contains(out, "usage") {
			t.Errorf("expected usage message, got:\n%s", out)
		}
	})
}

// TestConvoyLifecycle exercises CRUD and lifecycle operations using a single
// city. Subtests run sequentially so state accumulates across them.
func TestConvoyLifecycle(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	// IDs captured by earlier subtests for use by later ones.
	var basicID string
	var flaggedID string
	var ownedID string
	var closeID string
	var landOwnedID string
	var landNotOwnedID string
	var dryRunID string

	t.Run("Create_Basic", func(t *testing.T) {
		out, err := c.GC("convoy", "create", "feature-x")
		if err != nil {
			t.Fatalf("gc convoy create: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Created convoy") {
			t.Errorf("expected 'Created convoy' in output, got:\n%s", out)
		}
		if !strings.Contains(out, `"feature-x"`) {
			t.Errorf("expected convoy name in output, got:\n%s", out)
		}
		basicID = parseConvoyID(out)
	})

	t.Run("Create_WithFlags", func(t *testing.T) {
		out, err := c.GC("convoy", "create", "flagged-convoy",
			"--owner", "quinn",
			"--merge", "mr",
			"--target", "main",
		)
		if err != nil {
			t.Fatalf("gc convoy create with flags: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Created convoy") {
			t.Errorf("expected 'Created convoy' in output, got:\n%s", out)
		}

		// Verify metadata via status.
		flaggedID = parseConvoyID(out)
		if flaggedID == "" {
			t.Fatal("could not parse convoy ID from create output")
		}
		status, err := c.GC("convoy", "status", flaggedID)
		if err != nil {
			t.Fatalf("gc convoy status %s: %v\n%s", flaggedID, err, status)
		}
		for _, want := range []string{"quinn", "mr", "main"} {
			if !strings.Contains(status, want) {
				t.Errorf("convoy status should contain %q, got:\n%s", want, status)
			}
		}
	})

	t.Run("Create_Owned", func(t *testing.T) {
		out, err := c.GC("convoy", "create", "owned-convoy", "--owned")
		if err != nil {
			t.Fatalf("gc convoy create --owned: %v\n%s", err, out)
		}

		ownedID = parseConvoyID(out)
		if ownedID == "" {
			t.Fatal("could not parse convoy ID from create output")
		}

		// Owned convoys have the "owned" label visible in status.
		status, err := c.GC("convoy", "status", ownedID)
		if err != nil {
			t.Fatalf("gc convoy status: %v\n%s", err, status)
		}
		if !strings.Contains(status, "owned") {
			t.Errorf("owned convoy status should mention 'owned', got:\n%s", status)
		}
	})

	t.Run("List_AfterCreate", func(t *testing.T) {
		out, err := c.GC("convoy", "list")
		if err != nil {
			t.Fatalf("gc convoy list: %v\n%s", err, out)
		}
		if strings.Contains(out, "No open convoys") {
			t.Error("convoy list should show created convoys, got 'No open convoys'")
		}
		if !strings.Contains(out, "feature-x") {
			t.Errorf("convoy list should contain 'feature-x', got:\n%s", out)
		}
	})

	t.Run("Status_ShowsDetails", func(t *testing.T) {
		if basicID == "" {
			t.Skip("Create_Basic did not produce an ID")
		}
		out, err := c.GC("convoy", "status", basicID)
		if err != nil {
			t.Fatalf("gc convoy status: %v\n%s", err, out)
		}
		if !strings.Contains(out, basicID) {
			t.Errorf("status should contain convoy ID %q, got:\n%s", basicID, out)
		}
		if !strings.Contains(out, "feature-x") {
			t.Errorf("status should contain convoy title, got:\n%s", out)
		}
	})

	t.Run("Target_SetsBranch", func(t *testing.T) {
		// Create a dedicated convoy for the target test.
		createOut, err := c.GC("convoy", "create", "target-test")
		if err != nil {
			t.Fatalf("gc convoy create: %v\n%s", err, createOut)
		}
		id := parseConvoyID(createOut)
		if id == "" {
			t.Fatal("could not parse convoy ID")
		}

		out, err := c.GC("convoy", "target", id, "main")
		if err != nil {
			t.Fatalf("gc convoy target: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Set target") {
			t.Errorf("expected 'Set target' in output, got:\n%s", out)
		}
		if !strings.Contains(out, "main") {
			t.Errorf("expected 'main' in output, got:\n%s", out)
		}
	})

	t.Run("Close_ClosesConvoy", func(t *testing.T) {
		createOut, err := c.GC("convoy", "create", "close-test")
		if err != nil {
			t.Fatalf("gc convoy create: %v\n%s", err, createOut)
		}
		closeID = parseConvoyID(createOut)
		if closeID == "" {
			t.Fatal("could not parse convoy ID")
		}

		out, err := c.GC("convoy", "close", closeID)
		if err != nil {
			t.Fatalf("gc convoy close: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Closed convoy") {
			t.Errorf("expected 'Closed convoy' in output, got:\n%s", out)
		}

		// Closed convoy should not appear in list.
		listOut, err := c.GC("convoy", "list")
		if err != nil {
			t.Fatalf("gc convoy list: %v\n%s", err, listOut)
		}
		if strings.Contains(listOut, "close-test") {
			t.Error("closed convoy should not appear in list")
		}
	})

	t.Run("Land_OwnedConvoy", func(t *testing.T) {
		createOut, err := c.GC("convoy", "create", "land-test", "--owned")
		if err != nil {
			t.Fatalf("gc convoy create --owned: %v\n%s", err, createOut)
		}
		landOwnedID = parseConvoyID(createOut)
		if landOwnedID == "" {
			t.Fatal("could not parse convoy ID")
		}

		out, err := c.GC("convoy", "land", landOwnedID)
		if err != nil {
			t.Fatalf("gc convoy land: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Landed convoy") {
			t.Errorf("expected 'Landed convoy' in output, got:\n%s", out)
		}
	})

	t.Run("Land_NotOwned", func(t *testing.T) {
		createOut, err := c.GC("convoy", "create", "not-owned")
		if err != nil {
			t.Fatalf("gc convoy create: %v\n%s", err, createOut)
		}
		landNotOwnedID = parseConvoyID(createOut)
		if landNotOwnedID == "" {
			t.Fatal("could not parse convoy ID")
		}

		out, err := c.GC("convoy", "land", landNotOwnedID)
		if err == nil {
			t.Fatal("expected error landing non-owned convoy, got success")
		}
		if !strings.Contains(out, "not owned") {
			t.Errorf("expected 'not owned' error, got:\n%s", out)
		}
	})

	t.Run("Land_DryRun", func(t *testing.T) {
		createOut, err := c.GC("convoy", "create", "dry-run-test", "--owned")
		if err != nil {
			t.Fatalf("gc convoy create: %v\n%s", err, createOut)
		}
		dryRunID = parseConvoyID(createOut)
		if dryRunID == "" {
			t.Fatal("could not parse convoy ID")
		}

		out, err := c.GC("convoy", "land", dryRunID, "--dry-run")
		if err != nil {
			t.Fatalf("gc convoy land --dry-run: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Would land") {
			t.Errorf("expected 'Would land' in dry-run output, got:\n%s", out)
		}

		// Convoy should still appear in list (not actually closed).
		listOut, err := c.GC("convoy", "list")
		if err != nil {
			t.Fatalf("gc convoy list: %v\n%s", err, listOut)
		}
		if !strings.Contains(listOut, "dry-run-test") {
			t.Error("dry-run should not close the convoy, but it disappeared from list")
		}
	})
}

// TestConvoyEmptyCity exercises commands on a fresh city with no convoys.
func TestConvoyEmptyCity(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("List_Empty", func(t *testing.T) {
		out, err := c.GC("convoy", "list")
		if err != nil {
			t.Fatalf("gc convoy list: %v\n%s", err, out)
		}
		if !strings.Contains(out, "No open convoys") {
			t.Errorf("expected 'No open convoys' on fresh city, got:\n%s", out)
		}
	})

	t.Run("Check_EmptyCity", func(t *testing.T) {
		out, err := c.GC("convoy", "check")
		if err != nil {
			t.Fatalf("gc convoy check: %v\n%s", err, out)
		}
		if !strings.Contains(out, "auto-closed") {
			t.Errorf("expected 'auto-closed' summary in output, got:\n%s", out)
		}
	})

	t.Run("Stranded_NoConvoys", func(t *testing.T) {
		out, err := c.GC("convoy", "stranded")
		if err != nil {
			t.Fatalf("gc convoy stranded: %v\n%s", err, out)
		}
		if !strings.Contains(out, "No stranded work") {
			t.Errorf("expected 'No stranded work' on fresh city, got:\n%s", out)
		}
	})
}

// --- helpers ---

// parseConvoyID extracts the convoy ID from "Created convoy gc-N ..." output.
func parseConvoyID(output string) string {
	// Format: "Created convoy gc-1 ..."
	prefix := "Created convoy "
	idx := strings.Index(output, prefix)
	if idx < 0 {
		return ""
	}
	rest := output[idx+len(prefix):]
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
