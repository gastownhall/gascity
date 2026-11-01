package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Repro for ga-bkzobh / ga-2o0h0c: a git worktree of the city repo carries the
// tracked city.toml at its root, and worktree-setup.sh writes a .beads/ holding
// only a redirect to the real city's store. To gc that directory looks like a
// complete city -- city.toml plus a .beads -- so findCity stops there, and the
// managed-dolt layer used to treat it as a scope that owns its own store and
// start a private, empty Dolt server for it.
//
// Measured on this fleet: pid 181466, alive 12 days on port 28424, data dir 8K,
// SHOW DATABASES returning only information_schema and mysql. Four such starts
// since 2026-08-04.
//
// The binding assertions are on the ownership predicate and the lifecycle
// admission gate, not on resolveManagedDoltRuntimeLayoutStrict: the layout is a
// pure path derivation that never consults ownership, so DataDir is
// <scope>/.beads/dolt for any path. findCity's result is recorded as
// characterization only; whether discovery should follow the redirect is a
// separate, open design question.

// writeRedirectingWorktree builds the agent worktree shape under a real city
// and returns (worktree, realBeads).
func writeRedirectingWorktree(t *testing.T) (string, string) {
	t.Helper()
	realCity := t.TempDir()
	if err := os.WriteFile(filepath.Join(realCity, "city.toml"), []byte("[workspace]\nname = \"real\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	realBeads := filepath.Join(realCity, ".beads")
	if err := os.MkdirAll(realBeads, 0o755); err != nil {
		t.Fatal(err)
	}

	// The agent worktree, nested under the real city exactly as
	// .gc/worktrees/<pack>/<role> is, checked out from the city repo and so
	// carrying the tracked city.toml.
	worktree := filepath.Join(realCity, ".gc", "worktrees", "gc-management", "pack-author")
	if err := os.MkdirAll(filepath.Join(worktree, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "city.toml"), []byte("[workspace]\nname = \"real\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// packs/gastown/scripts/worktree-setup.sh:785 writes this unconditionally.
	if err := os.WriteFile(filepath.Join(worktree, ".beads", "redirect"), []byte(realBeads+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return worktree, realBeads
}

func TestRedirectingWorktreeDoesNotOwnAManagedDoltStore(t *testing.T) {
	worktree, realBeads := writeRedirectingWorktree(t)

	// Characterization only -- not an assertion. Records which city an ambient
	// walk from inside the worktree resolves to today.
	if got, err := findCity(worktree); err == nil {
		t.Logf("characterization: findCity(worktree) = %q", got)
	}

	owned, err := managedDoltLifecycleOwned(worktree)
	if err != nil {
		t.Fatalf("managedDoltLifecycleOwned(%q): %v", worktree, err)
	}
	if owned {
		t.Errorf("managedDoltLifecycleOwned(%q) = true; its .beads is a redirect to %q, "+
			"so starting a server there yields an empty store that cannot serve the "+
			"redirected database: bd follows the redirect, gc's dolt layer does not",
			worktree, realBeads)
	}

	err = admitManagedDoltStart(worktree)
	if err == nil {
		t.Fatalf("admitManagedDoltStart(%q) = nil, want a refusal naming the redirect", worktree)
	}
	for _, want := range []string{normalizePathForCompare(worktree), realBeads} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
}

// The refusal is on starting, not on looking: a stray server already started in
// such a scope (the 12-day pid in the sighting) must stay inspectable and
// stoppable, which run through the plain admission.
func TestRedirectingWorktreeStaysInspectableAndStoppable(t *testing.T) {
	worktree, _ := writeRedirectingWorktree(t)
	if err := admitLegacyManagedDoltLifecycle(worktree); err != nil {
		t.Fatalf("admitLegacyManagedDoltLifecycle(%q) = %v, want nil so inspect/stop still run", worktree, err)
	}
}

func TestRecoverManagedDoltRefusesRedirectingWorktree(t *testing.T) {
	worktree, realBeads := writeRedirectingWorktree(t)

	_, err := recoverManagedDoltProcess(worktree, "127.0.0.1", "38999", "root", "warning", 0)
	if err == nil {
		t.Fatal("recoverManagedDoltProcess on a redirecting worktree = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), realBeads) {
		t.Errorf("refusal %q does not name the redirect target %q", err, realBeads)
	}
	if _, statErr := os.Stat(filepath.Join(worktree, ".beads", "dolt")); !os.IsNotExist(statErr) {
		t.Errorf("recover left a managed data dir in the redirecting worktree (stat err = %v)", statErr)
	}
}

// Sighting 2 of ga-2o0h0c: no city.toml anywhere, only a stray .gc/ runtime
// root that findCity's legacy HasRuntimeRoot branch claims (a source directory
// such as cmd/gc/ that leaked a .gc/). The same ownership default applied
// there, with no redirect involved.
func TestStrayRuntimeRootWithoutCityTomlDoesNotOwnAManagedDoltStore(t *testing.T) {
	stray := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stray, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stray, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := admitManagedDoltStart(stray)
	if err == nil {
		t.Fatalf("admitManagedDoltStart(%q) = nil, want a refusal naming the missing city.toml", stray)
	}
	if !strings.Contains(err.Error(), "city.toml") {
		t.Errorf("refusal %q does not name the missing city.toml", err)
	}

	_, err = recoverManagedDoltProcess(stray, "127.0.0.1", "38999", "root", "warning", 0)
	if err == nil {
		t.Fatal("recoverManagedDoltProcess on a stray runtime root = nil, want a refusal")
	}
	if _, statErr := os.Stat(filepath.Join(stray, ".beads", "dolt")); !os.IsNotExist(statErr) {
		t.Errorf("recover left a managed data dir in the stray runtime root (stat err = %v)", statErr)
	}
}

// Controls: the preconditions must refuse only what they name. A real city
// (city.toml, no redirect) and a city whose redirect names its own .beads keep
// the lifecycle they have today.
func TestManagedDoltPreconditionsLeaveRealCitiesAlone(t *testing.T) {
	t.Run("city.toml and no redirect", func(t *testing.T) {
		city := t.TempDir()
		if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"real\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := admitManagedDoltStart(city); err != nil {
			t.Fatalf("admitManagedDoltStart(real city) = %v, want nil", err)
		}
	})
	t.Run("redirect names the scope's own .beads", func(t *testing.T) {
		city := t.TempDir()
		if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"real\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		beads := filepath.Join(city, ".beads")
		if err := os.MkdirAll(beads, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beads, "redirect"), []byte(beads+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := admitManagedDoltStart(city); err != nil {
			t.Fatalf("admitManagedDoltStart(self-redirect) = %v, want nil", err)
		}
	})
}
