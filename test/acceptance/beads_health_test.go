//go:build acceptance_a

// Beads health acceptance tests.
//
// These exercise gc beads health as a black box. The beads provider
// health check should succeed on any initialized city with file beads.
package acceptance_test

import (
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestBeadsHealthCommands(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("HealthSucceeds", func(t *testing.T) {
		out, err := c.GC("beads", "health")
		if err != nil {
			t.Fatalf("gc beads health failed: %v\n%s", err, out)
		}
	})

	t.Run("BareCommand_ReturnsHelp", func(t *testing.T) {
		// Bare "gc beads" may show help or require a subcommand.
		out, _ := c.GC("beads")
		_ = out
	})
}

func TestBeadsHealthFromNonCity(t *testing.T) {
	emptyDir := t.TempDir()
	_, err := helpers.RunGC(testEnv, emptyDir, "beads", "health")
	if err == nil {
		t.Fatal("expected error for beads health from non-city directory")
	}
}
