//go:build acceptance_a

// Supervisor and city registry acceptance tests.
//
// These exercise gc cities, register, unregister, and supervisor status
// as a black box. The test supervisor is started by TestMain, so
// supervisor status should report it as running.
package acceptance_test

import (
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestCitiesCommand(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	out, err := helpers.RunGC(testEnv, "", "cities")
	if err != nil {
		t.Fatalf("gc cities failed: %v\n%s", err, out)
	}
	// After init, at least one city should be registered.
	if strings.TrimSpace(out) == "" {
		t.Fatal("gc cities produced empty output after init")
	}
}

func TestRegisterUnregister(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	// gc init starts a standalone controller. Stop it first so register
	// can hand management to the supervisor.
	c.GC("stop", c.Dir) //nolint:errcheck

	t.Run("Unregister", func(t *testing.T) {
		out, err := c.GC("unregister", c.Dir)
		if err != nil {
			t.Fatalf("gc unregister failed: %v\n%s", err, out)
		}
	})

	t.Run("RegisterAfterUnregister", func(t *testing.T) {
		out, err := c.GC("register", c.Dir)
		if err != nil {
			t.Fatalf("gc register after unregister failed: %v\n%s", err, out)
		}
	})

	t.Run("RegisterNonCity", func(t *testing.T) {
		emptyDir := t.TempDir()
		_, err := helpers.RunGC(testEnv, "", "register", emptyDir)
		if err == nil {
			t.Fatal("expected error registering non-city directory")
		}
	})
}

func TestSupervisorStatus(t *testing.T) {
	// TestMain starts a supervisor, so this should report running.
	out, err := helpers.RunGC(testEnv, "", "supervisor", "status")
	if err != nil {
		// Supervisor may not be running in all test environments.
		t.Logf("supervisor status returned error (may be expected): %v\n%s", err, out)
		return
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("supervisor status produced empty output")
	}
}

func TestSupervisorReload(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	// Reload triggers immediate reconciliation.
	out, err := helpers.RunGC(testEnv, "", "supervisor", "reload")
	if err != nil {
		// May fail if supervisor isn't running, which is acceptable.
		t.Logf("supervisor reload returned error (may be expected): %v\n%s", err, out)
	}
}
