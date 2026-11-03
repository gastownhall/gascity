package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type readinessScriptFixture struct {
	city  string
	scope string
	log   string
	env   []string
}

// newReadinessScriptFixture drives the shipped script through its existing
// subprocess helper. Every readiness executable is a recorder: none can dial a
// socket, inspect a host process, or start a service. Port zero cannot belong to
// a listening service, so GC-managed readiness cannot make an owned case pass.
func newReadinessScriptFixture(t *testing.T, transport string) readinessScriptFixture {
	t.Helper()
	// Unlike providerScriptPath's historical skip, a missing build input must
	// fail these characterization tests rather than silently lose coverage.
	if _, err := os.Stat(filepath.Join("..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	city := filepath.Join(root, "city")
	scope := filepath.Join(city, "rigs", "résumé scope")
	bin := filepath.Join(root, "fake bin")
	for _, dir := range []string{filepath.Join(city, ".beads"), filepath.Join(scope, ".beads"), bin, filepath.Join(root, "home")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mode := "direct-server"
	if transport == "proxied" {
		mode = "proxied-server"
	}
	for path, contents := range map[string]string{
		filepath.Join(city, ".beads", "metadata.json"):  `{"backend":"dolt","dolt_mode":"direct-server"}`,
		filepath.Join(scope, ".beads", "metadata.json"): `{"backend":"dolt","dolt_mode":"` + mode + `"}`,
		filepath.Join(scope, ".beads", "config.yaml"):   "dolt.shared-server: false\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const recorder = `#!/bin/sh
name=${0##*/}
if [ "$name" = bd ]; then
    printf 'bd\t%s\tcwd=%s\tbeads=%s\n' "$*" "$PWD" "$BEADS_DIR" >> "$READINESS_CALLS"
    [ "$#" = 1 ] && [ "$1" = ping ] || exit 97
    exit "$READINESS_PING_EXIT"
fi
printf '%s\t%s\n' "$name" "$*" >> "$READINESS_CALLS"
if [ "$name" = gc ]; then
    case "$1 $2" in
        'dolt-state runtime-layout') exit 1 ;;
        'dolt-state allocate-port') printf '0\n'; exit 0 ;;
        'dolt-state probe-managed') printf 'running\tfalse\ntcp_reachable\tfalse\n'; exit 0 ;;
    esac
fi
# nc always reports the fixture endpoint unavailable, without any network I/O.
[ "$name" != nc ] || exit 1
exit 97
`
	for _, name := range []string{"bd", "gc", "nc", "dolt", "ps", "lsof", "sleep", "timeout", "tmux"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(recorder), 0o755); err != nil { //nolint:gosec // test-owned executable
			t.Fatal(err)
		}
	}
	log := filepath.Join(root, "calls.log")
	return readinessScriptFixture{
		city: city, scope: scope, log: log,
		// A child-only allowlist prevents ambient provider settings, BASH_ENV,
		// credentials and real helper paths from changing this fixture.
		env: []string{
			"PATH=" + bin + ":/usr/bin:/bin",
			"HOME=" + filepath.Join(root, "home"),
			"TMPDIR=" + root,
			"GC_CITY_PATH=" + city,
			"BEADS_DIR=" + filepath.Join(scope, ".beads"),
			"BD_BIN=" + filepath.Join(bin, "bd"),
			"GC_BIN=" + filepath.Join(bin, "gc"),
			"GC_BEADS_BACKEND=dolt",
			"GC_BEADS_TRANSPORT=" + transport,
			"GC_DOLT_HOST=127.0.0.1",
			"GC_DOLT_PORT=0",
			"READINESS_CALLS=" + log,
		},
	}
}

func readinessScriptStatus(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("script could not execute: %v", err)
	}
	return exit.ExitCode()
}

func assertReadinessScriptCalls(t *testing.T, path string, want []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	got := strings.TrimSuffix(string(data), "\n")
	if expected := strings.Join(want, "\n"); got != expected {
		t.Fatalf("readiness commands:\n%s\nwant exactly:\n%s", got, expected)
	}
}

func assertNoManagedReadinessState(t *testing.T, f readinessScriptFixture) {
	t.Helper()
	for _, scope := range []string{f.city, f.scope} {
		if _, err := os.Stat(filepath.Join(scope, ".gc")); !os.IsNotExist(err) {
			t.Fatalf("early readiness dispatch created managed state in %s (stat error %v)", scope, err)
		}
		if _, err := os.Stat(filepath.Join(scope, ".beads", "dolt")); !os.IsNotExist(err) {
			t.Fatalf("early readiness dispatch created managed data in %s (stat error %v)", scope, err)
		}
	}
}

func TestProviderScriptOwnedReadinessUsesBdPing(t *testing.T) {
	for _, transport := range []string{"proxied", "direct"} {
		for _, op := range []string{"health", "probe"} {
			for _, pingExit := range []int{0, 37} {
				t.Run(transport+"/"+op+"/ping_exit_"+strconv.Itoa(pingExit), func(t *testing.T) {
					f := newReadinessScriptFixture(t, transport)
					env := f.env
					env = append(env, "GC_BEADS_PROVIDER_OWNED=1", "GC_BEADS_TARGET=local", "READINESS_PING_EXIT="+strconv.Itoa(pingExit))
					out, err := runProviderOwnedScriptOp(t, env, op)
					status := readinessScriptStatus(t, err)
					t.Logf("%s %s: bd ping in %q, BEADS_DIR=%q, observed exit=%d", transport, op, f.scope, filepath.Join(f.scope, ".beads"), status)
					if status != pingExit {
						t.Fatalf("%s exit=%d, want ping exit=%d\n%s", op, status, pingExit, out)
					}
					assertReadinessScriptCalls(t, f.log, []string{"bd\tping\tcwd=" + f.scope + "\tbeads=" + filepath.Join(f.scope, ".beads")})
					assertNoManagedReadinessState(t, f)
				})
			}
		}
	}
}

func TestProviderScriptLegacyProxiedReadinessIsUnsupported(t *testing.T) {
	for _, op := range []string{"health", "probe"} {
		t.Run(op, func(t *testing.T) {
			f := newReadinessScriptFixture(t, "proxied")
			env := f.env
			env = append(env, "GC_CITY_PATH="+f.scope, "READINESS_PING_EXIT=0")
			out, err := runProviderOwnedScriptOp(t, env, op)
			status := readinessScriptStatus(t, err)
			t.Logf("legacy proxied %s: observed exit=%d", op, status)
			if status != 2 {
				t.Fatalf("unsupported %s exit=%d, want 2\n%s", op, status, out)
			}
			assertReadinessScriptCalls(t, f.log, nil)
			assertNoManagedReadinessState(t, f)
		})
	}
}

func TestProviderScriptLegacyReadinessUsesManagedChecks(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "unavailable.invalid"} {
		for _, op := range []string{"health", "probe"} {
			t.Run(host+"/"+op, func(t *testing.T) {
				f := newReadinessScriptFixture(t, "direct")
				env := f.env
				env = append(env, "GC_CITY_PATH="+f.scope, "GC_DOLT_HOST="+host, "READINESS_PING_EXIT=0")
				out, err := runProviderOwnedScriptOp(t, env, op)
				status := readinessScriptStatus(t, err)
				wantStatus := 2
				if op == "health" {
					wantStatus = 1
					if !strings.Contains(out, "dolt server not reachable on "+host+":0") {
						t.Fatalf("health lost unavailable-endpoint diagnostic:\n%s", out)
					}
				}
				t.Logf("legacy direct %s %s: observed exit=%d", host, op, status)
				if status != wantStatus {
					t.Fatalf("legacy %s exit=%d, want %d\n%s", op, status, wantStatus, out)
				}
				want := []string{
					"gc\tdolt-state runtime-layout --city " + f.scope,
					"gc\tdolt-state allocate-port --city " + f.scope + " --state-file " + filepath.Join(f.scope, ".gc", "runtime", "packs", "dolt", "dolt-provider-state.json"),
				}
				if op == "probe" && host == "127.0.0.1" {
					want = append(want, "gc\tdolt-state probe-managed --city "+f.scope+" --host "+host+" --port 0")
				} else {
					want = append(want, "nc\t-z -w 2 "+host+" 0")
				}
				assertReadinessScriptCalls(t, f.log, want)
			})
		}
	}
}
