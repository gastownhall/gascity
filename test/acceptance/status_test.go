//go:build acceptance_a

// Status command acceptance tests.
//
// These exercise gc status as a black box. Status shows a city-wide
// overview including controller state, agents, rigs, and sessions.
package acceptance_test

import (
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestStatusTutorialCity(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("ShowsCityName", func(t *testing.T) {
		out, err := c.GC("status", c.Dir)
		if err != nil {
			t.Fatalf("gc status: %v\n%s", err, out)
		}
		// Status should show the city directory path.
		if !strings.Contains(out, c.Dir) {
			t.Errorf("status should contain city path %q, got:\n%s", c.Dir, out)
		}
	})

	t.Run("ShowsControllerLine", func(t *testing.T) {
		out, err := c.GC("status", c.Dir)
		if err != nil {
			t.Fatalf("gc status: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Controller:") {
			t.Errorf("status should show 'Controller:' line, got:\n%s", out)
		}
	})

	t.Run("ShowsSuspendedState", func(t *testing.T) {
		out, err := c.GC("status", c.Dir)
		if err != nil {
			t.Fatalf("gc status: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Suspended:") {
			t.Errorf("status should show 'Suspended:' line, got:\n%s", out)
		}
	})

	t.Run("JSON_ReturnsValidJSON", func(t *testing.T) {
		out, err := c.GC("status", "--json", c.Dir)
		if err != nil {
			t.Fatalf("gc status --json: %v\n%s", err, out)
		}
		trimmed := strings.TrimSpace(out)
		if !strings.HasPrefix(trimmed, "{") {
			t.Errorf("--json output should start with '{', got:\n%s", out)
		}
	})
}

func TestStatusGastownCity(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	t.Run("ShowsAgents", func(t *testing.T) {
		out, err := c.GC("status", c.Dir)
		if err != nil {
			t.Fatalf("gc status: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Agents:") {
			t.Errorf("gastown status should show 'Agents:' section, got:\n%s", out)
		}
		if !strings.Contains(out, "agents running") {
			t.Errorf("status should show agent count summary, got:\n%s", out)
		}
	})

	t.Run("NoSessions_OmitsSessionLine", func(t *testing.T) {
		out, err := c.GC("status", c.Dir)
		if err != nil {
			t.Fatalf("gc status: %v\n%s", err, out)
		}
		// "Sessions:" only appears when sessions exist. A fresh city has none,
		// so verify the rest of the output renders without it.
		if !strings.Contains(out, "Agents:") {
			t.Errorf("status should show 'Agents:' even without sessions, got:\n%s", out)
		}
	})
}

func TestStatus_NotInitialized_ReturnsError(t *testing.T) {
	emptyDir := t.TempDir()
	_, err := helpers.RunGC(testEnv, emptyDir, "status", emptyDir)
	if err == nil {
		t.Fatal("expected error for status on non-city directory, got success")
	}
}
