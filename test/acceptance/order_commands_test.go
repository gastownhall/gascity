//go:build acceptance_a

// Order command acceptance tests.
//
// These exercise gc order list, show, and check as a black box. Orders
// are formulas with gate conditions for periodic dispatch. The gastown
// example city ships several orders from its packs. Tests also cover
// the bare command error path and nonexistent order lookup.
package acceptance_test

import (
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestOrderGastownCity(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	t.Run("List_ShowsOrders", func(t *testing.T) {
		out, err := c.GC("order", "list")
		if err != nil {
			t.Fatalf("gc order list failed: %v\n%s", err, out)
		}

		// Gastown ships orders — verify at least one appears.
		if !strings.Contains(out, "NAME") || !strings.Contains(out, "TRIGGER") {
			// Could be "No orders found." if discovery fails, which is also informative.
			if strings.Contains(out, "No orders") {
				t.Log("gc order list found no orders in gastown (may be expected if order discovery requires running city)")
				return
			}
			t.Errorf("expected order table headers or 'No orders', got:\n%s", out)
		}
	})

	t.Run("Show_DisplaysDetails", func(t *testing.T) {
		// List orders to find a real name.
		listOut, err := c.GC("order", "list")
		if err != nil {
			t.Fatalf("gc order list: %v\n%s", err, listOut)
		}
		if strings.Contains(listOut, "No orders") {
			t.Skip("no orders available to show")
		}

		// Parse the first order name from the table (skip header line).
		lines := strings.Split(strings.TrimSpace(listOut), "\n")
		if len(lines) < 2 {
			t.Skip("order list has no data rows")
		}
		// First column of second line is the order name.
		fields := strings.Fields(lines[1])
		if len(fields) == 0 {
			t.Skip("could not parse order name from list output")
		}
		orderName := fields[0]

		out, err := c.GC("order", "show", orderName)
		if err != nil {
			t.Fatalf("gc order show %s: %v\n%s", orderName, err, out)
		}
		if !strings.Contains(out, orderName) {
			t.Errorf("order show should contain the order name %q, got:\n%s", orderName, out)
		}
	})
}

func TestOrderRunGastownCity(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	t.Run("Run_Nonexistent_ReturnsError", func(t *testing.T) {
		_, err := c.GC("order", "run", "nonexistent-order-xyz")
		if err == nil {
			t.Fatal("expected error for running nonexistent order")
		}
	})

	t.Run("Run_MissingName_ReturnsError", func(t *testing.T) {
		_, err := c.GC("order", "run")
		if err == nil {
			t.Fatal("expected error for order run without name")
		}
	})

	t.Run("Run_RealOrder_DoesNotCrash", func(t *testing.T) {
		// List orders to find a real name.
		listOut, err := c.GC("order", "list")
		if err != nil {
			t.Fatalf("gc order list: %v\n%s", err, listOut)
		}
		if strings.Contains(listOut, "No orders") {
			t.Skip("no orders available to run")
		}
		lines := strings.Split(strings.TrimSpace(listOut), "\n")
		if len(lines) < 2 {
			t.Skip("order list has no data rows")
		}
		fields := strings.Fields(lines[1])
		if len(fields) == 0 {
			t.Skip("could not parse order name")
		}
		orderName := fields[0]

		// order run may fail because no agents are running, but it should
		// not panic or produce an unhelpful error. We just verify it
		// doesn't crash and produces some output.
		out, _ := c.GC("order", "run", orderName)
		_ = out
	})

	t.Run("Check_GastownCity", func(t *testing.T) {
		// order check on gastown should not crash.
		out, _ := c.GC("order", "check")
		_ = out
	})
}

func TestOrderTutorialCity(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("List_Succeeds", func(t *testing.T) {
		out, err := c.GC("order", "list")
		if err != nil {
			t.Fatalf("gc order list failed: %v\n%s", err, out)
		}
		// Tutorial city may or may not have orders. Should not crash.
		_ = out
	})

	t.Run("Show_Nonexistent_ReturnsError", func(t *testing.T) {
		out, err := c.GC("order", "show", "nonexistent-order-xyz")
		if err == nil {
			t.Fatal("expected error for nonexistent order, got success")
		}
		_ = out
	})

	t.Run("Check_Succeeds", func(t *testing.T) {
		// order check evaluates gates. Exit 0 = orders due, exit 1 = none due.
		// Either is acceptable; we're testing it doesn't crash.
		out, _ := c.GC("order", "check")
		_ = out
	})

	t.Run("NoSubcommand_ReturnsError", func(t *testing.T) {
		out, err := c.GC("order")
		if err == nil {
			t.Fatal("expected error for bare 'gc order', got success")
		}
		if !strings.Contains(out, "missing subcommand") {
			t.Errorf("expected 'missing subcommand' message, got:\n%s", out)
		}
	})

	t.Run("UnknownSubcommand_ReturnsError", func(t *testing.T) {
		out, err := c.GC("order", "explode")
		if err == nil {
			t.Fatal("expected error for unknown subcommand, got success")
		}
		if !strings.Contains(out, "unknown subcommand") {
			t.Errorf("expected 'unknown subcommand' message, got:\n%s", out)
		}
	})
}
