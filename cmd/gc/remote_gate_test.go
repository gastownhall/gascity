package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/clientcontext"
)

// setProdContextFlag points the persistent --context global at the "prod"
// fixture for a test and restores it afterward, mirroring run()'s reset so tests
// do not leak flag state.
func setProdContextFlag(t *testing.T) {
	t.Helper()
	prev := contextFlag
	contextFlag = "prod"
	t.Cleanup(func() { contextFlag = prev })
}

func addProdContext(t *testing.T) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := doContextAdd(clientcontext.Context{Name: "prod", URL: "https://box:9443", City: "mc"}, &out, &errb); code != 0 {
		t.Fatalf("seed context: %q", errb.String())
	}
}

// The core safety property: a resolved remote target is refused by the
// capability gate while the read set is disabled — never silently downgraded to
// a local city.
func TestResolveContext_RemoteGatedByDefault(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	addProdContext(t)
	setProdContextFlag(t)

	_, err := resolveContext()
	if err == nil {
		t.Fatalf("expected capability-gate error for a remote target, got nil")
	}
	if !strings.Contains(err.Error(), "does not support a remote city") {
		t.Errorf("gate error = %q", err.Error())
	}
	// resolveContextAllowRemote, by contrast, returns the target (no gate).
	raw, rerr := resolveContextAllowRemote()
	if rerr != nil || raw.Remote == nil {
		t.Fatalf("resolveContextAllowRemote must return the remote target: %+v err=%v", raw, rerr)
	}
}

// A remote+remote flag conflict surfaces even while gated (conflicts are
// resolved before the gate).
func TestResolveContext_RemoteConflictSurfacesEvenWhenGated(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setProdContextFlag(t)
	prevURL := cityURLFlag
	cityURLFlag = "https://other:9443"
	t.Cleanup(func() { cityURLFlag = prevURL })

	_, err := resolveContext()
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("want remote+remote conflict, got %v", err)
	}
}

func TestResolveContext_NoAPIPlusRemoteErrors(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_NO_API", "1")
	addProdContext(t)
	setProdContextFlag(t)

	_, err := resolveContext()
	if err == nil || !strings.Contains(err.Error(), "GC_NO_API") {
		t.Fatalf("want GC_NO_API+remote conflict, got %v", err)
	}
}

func TestResolveCommandContext_PositionalPlusRemoteConflict(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setProdContextFlag(t)

	_, err := resolveCommandContext([]string{"somecity"})
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("want positional+remote conflict, got %v", err)
	}
}

// A purely-local --city command must NOT be coupled to the parse health of the
// remote contexts registry: a malformed contexts.toml must not break it (the
// file is only loaded to resolve a named context). Regression for review
// finding #1.
func TestResolveRemoteTarget_LocalCityFlagIgnoresMalformedContexts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "contexts.toml"), []byte("default =\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := cityFlag
	cityFlag = "/some/local/city"
	t.Cleanup(func() { cityFlag = prev })

	target, handled, err := resolveRemoteTarget()
	if err != nil {
		t.Fatalf("a local --city command must not fail on a malformed contexts file: %v", err)
	}
	if handled || target != nil {
		t.Fatalf("local --city must not resolve a remote target: handled=%v target=%+v", handled, target)
	}
}

// An ad-hoc --city-url target is self-contained and must not load (or fail on) a
// malformed contexts file either.
func TestResolveRemoteTarget_AdHocURLIgnoresMalformedContexts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "contexts.toml"), []byte("default =\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prevURL, prevName := cityURLFlag, cityNameFlag
	cityURLFlag, cityNameFlag = "https://box:9443", "mc"
	t.Cleanup(func() { cityURLFlag, cityNameFlag = prevURL, prevName })

	target, handled, err := resolveRemoteTarget()
	if err != nil {
		t.Fatalf("ad-hoc --city-url must not read the contexts file: %v", err)
	}
	if !handled || target == nil || target.CityName != "mc" {
		t.Fatalf("ad-hoc target = %+v handled=%v", target, handled)
	}
}

// remoteFlagPresent must be flag-only: a lower-precedence remote ENV selector is
// shadowed by (never conflicts with) a positional/local flag. Regression for
// review finding #2.
func TestRemoteFlagPresent_FlagOnly(t *testing.T) {
	t.Setenv("GC_CITY_URL", "https://box:9443")
	if remoteFlagPresent() {
		t.Fatalf("a remote ENV alone must not count as a remote flag")
	}
	setProdContextFlag(t)
	if !remoteFlagPresent() {
		t.Fatalf("--context must count as a remote flag")
	}
}

// A local --city that wins over a stray remote ENV must resolve LOCAL, never a
// gated remote target. Regression for review finding #2 at the resolver seam.
func TestResolveRemoteSelection_LocalCityFlagWinsOverStrayRemoteEnv(t *testing.T) {
	target, handled, err := resolveRemoteSelection(
		remoteSelection{cityFlag: "/local/city", envURL: "https://box:9443", envToken: "t"}, fileWith(""))
	if err != nil {
		t.Fatalf("local --city + stray remote env must not error: %v", err)
	}
	if handled || target != nil {
		t.Fatalf("local --city must shadow the remote env, got handled=%v target=%+v", handled, target)
	}
}

// Env-based remote selection (GC_CITY_CONTEXT) must also gate resolveContext
// without leaking to a local city.
func TestResolveContext_EnvRemoteGated(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	addProdContext(t)
	t.Setenv("GC_CITY_CONTEXT", "prod")

	_, err := resolveContext()
	if err == nil || !strings.Contains(err.Error(), "does not support a remote city") {
		t.Fatalf("want env-remote gate error, got %v", err)
	}
}
