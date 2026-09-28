package convergence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeUserCredentials plants a beads credentials file at the OS default
// location under a fake user home and points HOME at it.
func writeFakeUserCredentials(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "beads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir credentials dir: %v", err)
	}
	path := filepath.Join(dir, "credentials")
	if err := os.WriteFile(path, []byte("[127.0.0.1:3306]\npassword = secret\n"), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	t.Setenv("HOME", home)
	return path
}

func environLookup(t *testing.T, env ConditionEnv) map[string]string {
	t.Helper()
	lookup := make(map[string]string)
	for _, v := range env.Environ() {
		if name, value, ok := strings.Cut(v, "="); ok {
			lookup[name] = value
		}
	}
	return lookup
}

// TestConditionEnvExportsResolvedBeadsCredentialsFile is the regression for
// ga-pqlgh: gate scripts run with HOME=<CityPath> so bd's own default
// resolution lands on <CityPath>/.config/beads/credentials, which never
// exists. Every bd read inside a gate then fails to authenticate. The
// controller must thread the resolved credentials path explicitly instead of
// widening HOME.
func TestConditionEnvExportsResolvedBeadsCredentialsFile(t *testing.T) {
	t.Setenv("BEADS_CREDENTIALS_FILE", "")
	want := writeFakeUserCredentials(t)

	lookup := environLookup(t, ConditionEnv{BeadID: "bead-1", CityPath: "/city"})

	if got := lookup["BEADS_CREDENTIALS_FILE"]; got != want {
		t.Fatalf("BEADS_CREDENTIALS_FILE = %q, want %q", got, want)
	}
	// The sandbox must survive: HOME still points at the city, not the
	// controller's home directory.
	if got := lookup["HOME"]; got != "/city" {
		t.Fatalf("HOME = %q, want the city path (gate sandbox must not widen to the controller home)", got)
	}
}

// TestConditionEnvPrefersAmbientBeadsCredentialsFile keeps the operator
// override authoritative: a scoped credentials file already selected in the
// controller's environment must reach the gate unchanged.
func TestConditionEnvPrefersAmbientBeadsCredentialsFile(t *testing.T) {
	writeFakeUserCredentials(t)
	ambient := filepath.Join(t.TempDir(), "scoped-credentials")
	if err := os.WriteFile(ambient, []byte("[127.0.0.1:3306]\npassword = scoped\n"), 0o600); err != nil {
		t.Fatalf("write ambient credentials: %v", err)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", ambient)

	lookup := environLookup(t, ConditionEnv{BeadID: "bead-1", CityPath: "/city"})

	if got := lookup["BEADS_CREDENTIALS_FILE"]; got != ambient {
		t.Fatalf("BEADS_CREDENTIALS_FILE = %q, want ambient override %q", got, ambient)
	}
}

// TestConditionEnvOmitsMissingBeadsCredentialsFile keeps the whitelist honest:
// when no credentials file exists there is nothing to thread, and exporting a
// path that does not resolve would only mask bd's own fallback.
func TestConditionEnvOmitsMissingBeadsCredentialsFile(t *testing.T) {
	t.Setenv("BEADS_CREDENTIALS_FILE", "")
	t.Setenv("HOME", t.TempDir())

	lookup := environLookup(t, ConditionEnv{BeadID: "bead-1", CityPath: "/city"})

	if got, ok := lookup["BEADS_CREDENTIALS_FILE"]; ok {
		t.Fatalf("BEADS_CREDENTIALS_FILE = %q, want absent when no credentials file exists", got)
	}
}
