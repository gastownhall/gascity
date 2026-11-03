package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Dependency checks are platform-, provider- and feature-aware: a Windows
// herdr/file-backed scratch city must not require tmux, jq, pgrep, lsof, flock
// or GNU timeout. That does not mean the checks are simply bypassed. Both
// halves are pinned here, because a check that quietly stops asking is the same
// false green as a check that asks for the wrong thing.

// writeSessionProviderCity writes the minimum city.toml that selects a session
// provider, so the dependency check has a real configuration to read.
func writeSessionProviderCity(t *testing.T, provider string) string {
	t.Helper()
	dir := t.TempDir()
	body := "[workspace]\nname = \"scratch\"\n\n[beads]\nprovider = \"file\"\n"
	if provider != "" {
		body += "\n[session]\nprovider = \"" + provider + "\"\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
	return dir
}

// stubMissingTools makes every name in missing look absent and every other
// name look present, so a test asserts about requirements rather than about
// whatever happens to be installed on the host.
func stubMissingTools(t *testing.T, missing ...string) {
	t.Helper()
	absent := make(map[string]bool, len(missing))
	for _, name := range missing {
		absent[name] = true
	}
	old := initLookPath
	initLookPath = func(name string) (string, error) {
		if absent[name] {
			return "", os.ErrNotExist
		}
		return filepath.Join("/usr/bin", name), nil
	}
	t.Cleanup(func() { initLookPath = old })
}

func missingDepNames(deps []missingDep) []string {
	names := make([]string, 0, len(deps))
	for _, dep := range deps {
		names = append(names, dep.name)
	}
	return names
}

func containsDep(deps []missingDep, name string) bool {
	for _, dep := range deps {
		if dep.name == name {
			return true
		}
	}
	return false
}

func TestCheckHardDependenciesRequiresTmuxOnlyWhenTmuxIsSelected(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	stubMissingTools(t, "tmux", "herdr")

	selected := checkHardDependencies(writeSessionProviderCity(t, "tmux"))
	if !containsDep(selected, "tmux") {
		t.Fatalf("a tmux city with no tmux reported %v, want tmux missing", missingDepNames(selected))
	}

	defaultProvider := platformDefaultSessionProviderForTest()
	defaulted := checkHardDependencies(writeSessionProviderCity(t, ""))
	if !containsDep(defaulted, defaultProvider) {
		t.Fatalf("a default city reported %v, want %s missing", missingDepNames(defaulted), defaultProvider)
	}
	if defaultProvider != "tmux" && containsDep(defaulted, "tmux") {
		t.Fatalf("a default city reported %v, want the platform default without tmux", missingDepNames(defaulted))
	}

	for _, provider := range []string{"herdr", "subprocess", "acp"} {
		if got := checkHardDependencies(writeSessionProviderCity(t, provider)); containsDep(got, "tmux") {
			t.Fatalf("a %s city reported %v, want no tmux requirement", provider, missingDepNames(got))
		}
	}
}

// TestCheckHardDependenciesRequiresTmuxWhenSelectionResolvesToIt covers the
// selections that run on tmux without naming it. Matching the literal string
// is not the same as resolving the provider: "hybrid" composes a local tmux
// provider and routes sessions to it unless a remote match is configured, and
// an unregistered name falls through to the registry's tmux fallback. Checking
// only for "tmux" let either city pass with tmux absent, which is a regression
// on Linux and macOS, where these selections actually run.
func TestCheckHardDependenciesRequiresTmuxWhenSelectionResolvesToIt(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	stubMissingTools(t, "tmux")

	for _, provider := range []string{"hybrid", "definitely-unregistered"} {
		got := checkHardDependencies(writeSessionProviderCity(t, provider))
		if !containsDep(got, "tmux") {
			t.Fatalf("a %q city with no tmux reported %v, want tmux missing", provider, missingDepNames(got))
		}
	}

	// Prefix registrations are real providers. Asking the registry for exact
	// membership alone misreads them as unresolved and demands tmux for a
	// city that never touches it, which is a false requirement on Unix where
	// these selections run.
	for _, provider := range []string{"ssh:user@example.invalid", "exec:/opt/run-agent"} {
		if got := checkHardDependencies(writeSessionProviderCity(t, provider)); containsDep(got, "tmux") {
			t.Fatalf("a %q city reported %v, want no tmux requirement", provider, missingDepNames(got))
		}
	}
}

// TestCheckHardDependenciesRequiresHerdrWhenHerdrIsSelected is the additive
// half. Making requirements selection-aware has to add the backend a city
// picked, not only drop the ones it did not.
func TestCheckHardDependenciesRequiresHerdrWhenHerdrIsSelected(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	stubMissingTools(t, "herdr")

	selected := checkHardDependencies(writeSessionProviderCity(t, "herdr"))
	if !containsDep(selected, "herdr") {
		t.Fatalf("a herdr city with no herdr reported %v, want herdr missing", missingDepNames(selected))
	}

	other := checkHardDependencies(writeSessionProviderCity(t, "subprocess"))
	if containsDep(other, "herdr") {
		t.Fatalf("a subprocess city reported %v, want no herdr requirement", missingDepNames(other))
	}
}

// TestCheckHardDependenciesHonorsPerAgentSessionOverrides covers the selection
// a city-level provider hides: one agent pinned to another backend still needs
// that backend present.
func TestCheckHardDependenciesHonorsPerAgentSessionOverrides(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	stubMissingTools(t, "tmux")

	dir := t.TempDir()
	body := "[workspace]\nname = \"scratch\"\n\n[beads]\nprovider = \"file\"\n\n" +
		"[session]\nprovider = \"subprocess\"\n\n" +
		"[[agent]]\nname = \"mayor\"\nsession = \"tmux\"\n"
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}

	if got := checkHardDependencies(dir); !containsDep(got, "tmux") {
		t.Fatalf("an agent pinned to tmux reported %v, want tmux missing", missingDepNames(got))
	}
}

// TestCheckHardDependenciesGitIsAlwaysRequired is the guard against the
// conditioning going too far. git is not tied to any backend.
func TestCheckHardDependenciesGitIsAlwaysRequired(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	stubMissingTools(t, "git")

	for _, provider := range []string{"", "tmux", "herdr", "subprocess"} {
		got := checkHardDependencies(writeSessionProviderCity(t, provider))
		if !containsDep(got, "git") {
			t.Fatalf("a %q city with no git reported %v, want git missing", provider, missingDepNames(got))
		}
	}
}

// TestFileBackedCityNeedsNoPosixShellToolingWhereItIsDisabled is the exit-gate
// shape: a file-backed scratch city on a platform without the POSIX shell
// automation must not be blocked on the tools that automation calls.
func TestFileBackedCityNeedsNoPosixShellToolingWhereItIsDisabled(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	posixOnly := []string{"jq", "pgrep", "lsof", "flock", "timeout", "gtimeout", "python3"}
	stubMissingTools(t, posixOnly...)

	got := checkHardDependencies(writeSessionProviderCity(t, "subprocess"))

	if !posixShellAutomationEnabled() {
		if len(got) != 0 {
			t.Fatalf("missing deps = %v, want none for a file-backed city where the POSIX automation is disabled", missingDepNames(got))
		}
		return
	}
	// Where the automation does run, the same tools are still required, so
	// this case proves the conditioning and not a blanket removal.
	for _, name := range []string{"jq", "pgrep", "lsof"} {
		if !containsDep(got, name) {
			t.Fatalf("missing deps = %v, want %q still required where the POSIX automation runs", missingDepNames(got), name)
		}
	}
}

// TestUnavailableBackendHintExplainsThePlatformRatherThanAnInstall pins the
// message an operator gets when the backend their city selected cannot exist
// here. "brew install tmux" sends a Windows operator after something that has
// no Windows build.
func TestUnavailableBackendHintExplainsThePlatformRatherThanAnInstall(t *testing.T) {
	const installHint = "https://github.com/tmux/tmux/wiki/Installing"
	got := unavailableBackendHint("tmux", installHint)

	if reason, unavailable := backendUnavailableOnThisPlatform("tmux"); unavailable {
		if got != reason {
			t.Fatalf("unavailableBackendHint(tmux) = %q, want the platform reason %q", got, reason)
		}
		if !strings.Contains(got, "city.toml") {
			t.Fatalf("platform reason %q does not tell the operator what to change", got)
		}
		return
	}
	if got != installHint {
		t.Fatalf("unavailableBackendHint(tmux) = %q, want the install hint %q", got, installHint)
	}
}

func TestCitySessionRuntimeNamesResolvesTheEmptySelectionToPlatformDefault(t *testing.T) {
	t.Setenv("GC_SESSION", "")
	names := citySessionRuntimeNames(writeSessionProviderCity(t, ""))
	want := platformDefaultSessionProviderForTest()
	if !names[want] {
		t.Fatalf("citySessionRuntimeNames = %v, want %s for the default selection", names, want)
	}
}

func platformDefaultSessionProviderForTest() string {
	if name := effectiveProviderName(""); name != "" {
		return name
	}
	return "tmux"
}

// TestCitySessionRuntimeNamesHonorsTheEnvOverride pins that GC_SESSION, which
// wins over city.toml at runtime, also wins here. A check that read only the
// file would demand the backend the city is not going to use.
func TestCitySessionRuntimeNamesHonorsTheEnvOverride(t *testing.T) {
	t.Setenv("GC_SESSION", "herdr")
	names := citySessionRuntimeNames(writeSessionProviderCity(t, "tmux"))
	if !names["herdr"] {
		t.Fatalf("citySessionRuntimeNames = %v, want herdr from GC_SESSION", names)
	}
	if names["tmux"] {
		t.Fatalf("citySessionRuntimeNames = %v, want the overridden tmux selection dropped", names)
	}
}

// TestCheckHardDependenciesReportsManagedDoltUnsupported holds that readiness
// and startability give the same answer about running Gas City's own Dolt
// server.
//
// Having the dolt binary is not the same as being able to own a server's
// lifecycle. Where the platform cannot supply the ownership proofs, the start
// path refuses outright, so a city that needs a managed server cannot come up —
// and reporting it ready was the visible half of that: the operator was told
// everything was in place, then the server never started.
//
// A city pointed at an external endpoint needs neither the lifecycle nor the
// binary, and must not be blocked on either.
func TestCheckHardDependenciesReportsManagedDoltUnsupported(t *testing.T) {
	_, unsupported := backendUnavailableOnThisPlatform(managedDoltLifecycleDep)
	if supportErr := managedDoltNativeIdentitySupported(); (supportErr != nil) != unsupported {
		t.Fatalf("platform reports managed Dolt support as %v but readiness describes it as unavailable=%v; the two must agree", supportErr, unsupported)
	}

	dir := t.TempDir()
	body := "[workspace]\nname = \"scratch\"\n\n[beads]\nprovider = \"exec:gc-beads-bd\"\n"
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
	stubMissingTools(t)

	got := checkHardDependencies(dir)
	if unsupported {
		if !containsDep(got, managedDoltLifecycleDep) {
			t.Fatalf("a managed-Dolt city on a platform that cannot run one reported %v, want %q among them", missingDepNames(got), managedDoltLifecycleDep)
		}
		for _, dep := range got {
			if dep.name != managedDoltLifecycleDep {
				continue
			}
			if !strings.Contains(dep.installHint, "external Dolt endpoint") {
				t.Errorf("hint for %q was %q; it must say what to do instead, not how to install something that would not help", dep.name, dep.installHint)
			}
		}
		return
	}
	if containsDep(got, managedDoltLifecycleDep) {
		t.Fatalf("a platform that can run a managed Dolt server reported %v, want no lifecycle requirement", missingDepNames(got))
	}
}

// TestCheckHardDependenciesAsksForTheManagedLifecycleOnlyWhereGcRunsTheServer
// holds the other side of TestCheckHardDependenciesReportsManagedDoltUnsupported:
// the lifecycle requirement follows the server gc would run, not the need for a
// local Dolt author identity. gc runs no server for a doltlite city, which keeps
// its beads in bd's local store, nor for a provider-owned one, whose server
// belongs to bd, so a platform that cannot run one must not refuse either.
func TestCheckHardDependenciesAsksForTheManagedLifecycleOnlyWhereGcRunsTheServer(t *testing.T) {
	for _, tc := range []struct {
		name string
		city func(t *testing.T) string
	}{
		{name: "doltlite", city: func(t *testing.T) string {
			t.Setenv("GC_BEADS_BACKEND", "")
			dir := t.TempDir()
			body := "[workspace]\nname = \"scratch\"\n\n[beads]\nprovider = \"exec:gc-beads-bd\"\nbackend = \"doltlite\"\n"
			if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(body), 0o644); err != nil {
				t.Fatalf("writing city.toml: %v", err)
			}
			return dir
		}},
		{name: "provider-owned", city: func(t *testing.T) string {
			city := writeBootstrappedManagedBdCity(t)
			t.Setenv("GC_BEADS", "bd")
			if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
				t.Fatal(err)
			}
			return city
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.city(t)
			stubMissingTools(t)

			if got := checkHardDependencies(dir); containsDep(got, managedDoltLifecycleDep) {
				t.Fatalf("a %s city reported %v, want no %q requirement: gc runs no Dolt server for it", tc.name, missingDepNames(got), managedDoltLifecycleDep)
			}
		})
	}
}
