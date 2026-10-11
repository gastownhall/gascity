package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The remote router as claimHookWork wires it, and the credential the hook's
// own bd children (the claim lanes and the work query) carry for a remote
// scope (hook_claim_remote.go).

// TestClaimHookWorkRoutesARemoteLegThroughTheRemoteRouter: claimHookWork
// wires the remote router, so a remote leg under native_transport=auto claims
// through the native open (and a refused open fails the claim by name) rather
// than through the bd CLI.
func TestClaimHookWorkRoutesARemoteLegThroughTheRemoteRouter(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"rv\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	attachRemoteScope(t, cityPath) // http://127.0.0.1:9: every native open is refused
	env := append(os.Environ(), "BEADS_DIR="+filepath.Join(cityPath, ".beads"))
	stores := []hookStore{{dir: cityPath, env: env}}
	workQuery := `printf '%s' '[{"id":"rv-1","status":"open","metadata":{"gc.routed_to":"worker"}}]'`
	opts := hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}}
	var stdout, stderr bytes.Buffer
	code := claimHookWork(cityPath, workQuery, cityPath, env, stores, opts, func(string, error) {}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "opening the remote work store at") {
		t.Fatalf("claimHookWork = %d, stderr=%q stdout=%q; want the remote router's refused native open", code, stderr.String(), stdout.String())
	}
}

// TestHookClaimEnvWithScopeCredentialKeepsANilEnvInheriting: a nil leg env
// means "inherit the process environment" (workQueryEnvForDir). Projecting a
// credential into it must keep that meaning, not hand bd an env holding the
// token alone.
func TestHookClaimEnvWithScopeCredentialKeepsANilEnvInheriting(t *testing.T) {
	t.Setenv("GC_TEST_CITY_BEADS_TOKEN", "tok-city")
	t.Setenv("GC_TEST_INHERITED_MARKER", "inherited")
	installCityRemoteCredentialLookup(t)
	cityPath := writeCredentialCity(t, "[beads]\ncredential = \"env:GC_TEST_CITY_BEADS_TOKEN\"\n", "")

	got, err := hookClaimEnvWithScopeCredential(cityPath, cityPath, nil)
	if err != nil {
		t.Fatalf("projecting into a nil env: %v", err)
	}
	if !slices.Contains(got, "GC_TEST_INHERITED_MARKER=inherited") {
		t.Fatalf("a nil leg env lost the inherited process environment: %d entries", len(got))
	}
	if !slices.Contains(got, "BEADS_HTTP_TOKEN=127.0.0.1:9=tok-city") {
		t.Fatal("the scope's credential was not projected")
	}
}

// writeFakeBdReady writes a bd that answers `bd ready` with [] only when its
// env carries want as BEADS_HTTP_TOKEN, no credential reached its argv, and
// the env: source variable did not leak into it. It records success in the
// returned marker file.
func writeFakeBdReady(t *testing.T, want, secret, sourceVar string) (binDir, marker string) {
	t.Helper()
	binDir = t.TempDir()
	marker = filepath.Join(t.TempDir(), "bd-ready-ok")
	script := "#!/bin/sh\n" +
		"case \"$*\" in *" + secret + "*) echo 'credential on argv' >&2; exit 3;; esac\n" +
		"[ \"$1\" = ready ] || { echo \"unexpected bd $1\" >&2; exit 4; }\n" +
		"[ \"$BEADS_HTTP_TOKEN\" = '" + want + "' ] || { echo 'bd ready without the scope credential' >&2; exit 5; }\n" +
		"[ -z \"${" + sourceVar + "+x}\" ] || { echo 'env source leaked into bd' >&2; exit 6; }\n" +
		"echo ok > '" + marker + "'\n" +
		"printf '[]'\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir, marker
}

// TestHookWorkQueryCarriesACityTomlOnlyCredential: the city's credential is
// configured in city.toml alone, from an env: source that start has already
// moved out of the process environment. The hook's work query (bd ready) is a
// bd child of the remote scope like any other, so it gets the scope's
// credential the way gc bd's children do (BEADS_HTTP_TOKEN, host:port scoped),
// on either lane, and never on argv or in the hook's output.
func TestHookWorkQueryCarriesACityTomlOnlyCredential(t *testing.T) {
	for _, mode := range []string{"auto", "off"} {
		t.Run("native_transport="+mode, func(t *testing.T) {
			t.Setenv("GC_TEST_CITY_BEADS_TOKEN", "tok-city-"+mode)
			installCityRemoteCredentialLookup(t)
			cityPath := writeCredentialCity(t, "[beads]\ncredential = \"env:GC_TEST_CITY_BEADS_TOKEN\"\nnative_transport = \""+mode+"\"\n", "")
			// gc start's sequestration: the variable leaves the process env.
			beads.SequesterRemoteCredentialEnv("GC_TEST_CITY_BEADS_TOKEN")
			if _, ok := os.LookupEnv("GC_TEST_CITY_BEADS_TOKEN"); ok {
				t.Fatal("the env: source is still in the process environment")
			}

			binDir, marker := writeFakeBdReady(t, "127.0.0.1:9=tok-city-"+mode, "tok-city-"+mode, "GC_TEST_CITY_BEADS_TOKEN")
			env := append(os.Environ(), "BEADS_DIR="+filepath.Join(cityPath, ".beads"), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			stores := []hookStore{{dir: cityPath, env: env}}
			opts := hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}}
			var stdout, stderr bytes.Buffer
			claimHookWork(cityPath, "bd ready --json", cityPath, env, stores, opts, func(string, error) {}, &stdout, &stderr)
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("bd ready in the work query did not succeed with the scope's credential: stderr=%q", stderr.String())
			}
			for _, out := range []string{stdout.String(), stderr.String()} {
				if strings.Contains(out, "tok-city-"+mode) {
					t.Fatal("the credential appeared in the hook's output")
				}
			}
		})
	}
}

// TestHookWorkQueryRunnerLeavesALocalScopeAlone: only a remote scope's work
// query is touched.
func TestHookWorkQueryRunnerLeavesALocalScopeAlone(t *testing.T) {
	t.Setenv("BEADS_HTTP_TOKEN", "")
	cityPath := t.TempDir()
	out, err := hookWorkQueryRunner(cityPath)(`printf '%s' "${BEADS_HTTP_TOKEN:-none}"`, cityPath, []string{"BEADS_DIR=" + filepath.Join(cityPath, ".beads"), "PATH=/usr/bin:/bin"})
	if err != nil || out != "none" {
		t.Fatalf("local work query = %q (%v), want it run with its env unchanged", out, err)
	}
}

// TestHookClaimRemoteRigInheritingTheCityActivationUsesItsOwnCredential: a
// rig with no metadata of its own under a remote city reaches the city's
// server (its legs' BEADS_DIR names the city's activation), but it is still
// the RIG's scope: its own beads_credential, not the city's, goes to its
// claim lanes and its work query.
func TestHookClaimRemoteRigInheritingTheCityActivationUsesItsOwnCredential(t *testing.T) {
	t.Setenv("GC_TEST_CITY_BEADS_TOKEN", "tok-city")
	t.Setenv("GC_TEST_RIG_BEADS_TOKEN", "tok-rig")
	installCityRemoteCredentialLookup(t)
	cityPath := writeCredentialCity(t, "[beads]\ncredential = \"env:GC_TEST_CITY_BEADS_TOKEN\"\n",
		"[[rigs]]\nname = \"inh\"\npath = \"rigs/inh\"\nbeads_credential = \"env:GC_TEST_RIG_BEADS_TOKEN\"\n")
	rig := filepath.Join(cityPath, "rigs", "inh")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	if activation, remote := beads.RemoteBackendActivationRoot(rig, cityPath); !remote || activation != cityPath {
		t.Fatalf("rig activation = %q (remote %v), want it inherited from the city", activation, remote)
	}
	cityBeads := filepath.Join(cityPath, ".beads")
	rigLeg := []string{"BEADS_DIR=" + cityBeads, "GC_STORE_ROOT=" + rig, "GC_RIG_ROOT=" + rig, "PATH=/usr/bin:/bin"}

	if got := hookClaimRemoteScopeRoot(cityPath, rig, rigLeg); got != rig {
		t.Fatalf("scope of the inheriting rig's leg = %q, want the rig %q", got, rig)
	}
	router := newHookClaimRemoteRouter(cityPath, &bytes.Buffer{})
	legEnv, err := router.credentialEnv(hookClaimRemoteScopeRoot(cityPath, rig, rigLeg), rigLeg)
	if err != nil || !slices.Contains(legEnv, "BEADS_HTTP_TOKEN=127.0.0.1:9=tok-rig") {
		t.Fatalf("rig leg credential env err %v; want the rig's own token", err)
	}
	out, err := hookWorkQueryRunner(cityPath)(`[ "$BEADS_HTTP_TOKEN" = "127.0.0.1:9=tok-rig" ] && printf ok`, rig, rigLeg)
	if err != nil || out != "ok" {
		t.Fatalf("rig work query = %q (%v), want it run with the rig's own credential", out, err)
	}

	// A GC_STORE_ROOT naming a scope that does not share the leg's activation
	// (an agent's own local rig, inherited) never redirects the credential.
	local := t.TempDir()
	if err := os.MkdirAll(filepath.Join(local, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, ".beads", "metadata.json"), []byte(`{"backend": "dolt"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stray := []string{"BEADS_DIR=" + cityBeads, "GC_STORE_ROOT=" + local}
	if got := hookClaimRemoteScopeRoot(cityPath, cityPath, stray); got != cityPath {
		t.Fatalf("scope with a stray GC_STORE_ROOT = %q, want the city", got)
	}
}
