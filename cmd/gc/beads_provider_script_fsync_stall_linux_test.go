// This file is Linux-only by its filename, for the same reason as
// beads_provider_script_scope_lifecycle_linux_test.go: it executes the POSIX
// provider script against a fake bd and a fake gc.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The provider-owned `recover` retires a scope's local Dolt with `bd dolt stop`.
// A restart cannot fix a stalled fsync and adds recovery load to the stalled
// filesystem, so that retire is skipped while the scope's data root is stalled
// (rule D4(iii) of ga-8ws1ne). `recover` still pings, so a store that turns out
// healthy reports healthy and an unreachable one reports failure honestly.
//
// Every test drives the real script. The decisions are shell decisions (which
// call site is guarded, which directory is probed), so a test of the helper
// alone would not notice the wiring being reverted.

const (
	stallDocsPointer = "docs/troubleshooting/dolt-storage-stalls.md"
	stallFailOpen    = "fsync-stall guard skipped:"
	// stallPingFailure is a ping exit status no other step of the script
	// produces, so seeing it from the op proves the op returned ping's status.
	stallPingFailure = 3
)

type stallScopeKind string

const (
	// stallDirectLocal is a direct-transport scope whose Dolt gc owns.
	stallDirectLocal stallScopeKind = "direct local scope"
	// stallDirectExternal is a direct-transport scope bd bound to someone
	// else's server; gc has no local Dolt to retire there.
	stallDirectExternal stallScopeKind = "direct external scope"
	// stallProxiedOwnRoot is a proxied scope whose sidecar names its own root.
	stallProxiedOwnRoot stallScopeKind = "proxied scope with its own root"
	// stallProxiedSharedRoot is a migrated rig whose sidecar names the city's
	// root, so it does not own the proxy it would stop.
	stallProxiedSharedRoot stallScopeKind = "proxied scope sharing the city root"
)

// stallScope is one provider-owned scope wired to a recording bd and a fake gc
// that answers `dolt-state fsync-probe` and records the directory it was asked
// to probe.
type stallScope struct {
	t        *testing.T
	city     string
	scope    string
	bdLog    string
	probeLog string
	// probeDir is the physical directory the guard is expected to probe: the
	// one holding the Dolt data the retire would stop.
	probeDir string
	env      []string
}

// writeStallProbeGC installs a gc whose only answer is `dolt-state fsync-probe`.
// FAKE_PROBE_MODE picks the result; FAKE_PROBE_MS is the finished latency.
func writeStallProbeGC(t *testing.T, dir string) (bin, probeLog string) {
	t.Helper()
	probeLog = filepath.Join(dir, "probe-calls.log")
	bin = filepath.Join(dir, "gc")
	body := fmt.Sprintf(`#!/bin/sh
if [ "$1 $2" = "dolt-state fsync-probe" ]; then
  shift 2
  printf '%%s\n' "$*" >> %q
  case "${FAKE_PROBE_MODE:-}" in
    unfinished) printf 'status\tunfinished\nfsync_ms\t%%s\n' "${FAKE_PROBE_BOUND:-1000}" ;;
    finished)   printf 'status\tfinished\nfsync_ms\t%%s\n' "${FAKE_PROBE_MS:-5}" ;;
    error)      printf 'status\terror\nerror\tstat failed\n'; exit 1 ;;
    exit1)      exit 1 ;;
    garbage)    printf 'this is not tab separated\n' ;;
    *)          exit 1 ;;
  esac
  exit 0
fi
exit 1
`, probeLog)
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	return bin, probeLog
}

// writeStallRecordingBd installs a bd that logs its argv and succeeds, except
// that `ping` exits with FAKE_PING_RC so a test can make the store unreachable.
func writeStallRecordingBd(t *testing.T, dir string) (bin, logPath string) {
	t.Helper()
	logPath = filepath.Join(dir, "bd-calls.log")
	bin = filepath.Join(dir, "bd")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logPath +
		"\n[ \"$1\" = ping ] && exit \"${FAKE_PING_RC:-0}\"\nexit 0\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	return bin, logPath
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// newStallScope builds the scope the way gc lays it out: a city, and a rig at
// <city>/rigs/api that recover acts on.
func newStallScope(t *testing.T, kind stallScopeKind) *stallScope {
	t.Helper()
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "api")
	mustMkdirAll(t, filepath.Join(city, ".beads", "dolt"))
	mustMkdirAll(t, filepath.Join(rig, ".beads", "dolt"))

	fixtures := t.TempDir()
	bdBin, bdLog := writeStallRecordingBd(t, fixtures)
	gcBin, probeLog := writeStallProbeGC(t, fixtures)
	env := append(providerOwnedScriptEnv(city, rig, bdBin),
		"GC_BIN="+gcBin,
		// Neutralize anything the surrounding session exports.
		"GC_DOLT_FSYNC_WARN_MS=",
		"FAKE_PROBE_MODE=",
		"FAKE_PING_RC=0",
	)
	f := &stallScope{t: t, city: city, scope: rig, bdLog: bdLog, probeLog: probeLog, env: env}

	switch kind {
	case stallDirectLocal:
		f.env = append(f.env, "GC_BEADS_TRANSPORT=direct", "GC_BEADS_TARGET=local")
		f.probeDir = mustEvalSymlinks(t, filepath.Join(rig, ".beads", "dolt"))
	case stallDirectExternal:
		f.env = append(f.env, "GC_BEADS_TRANSPORT=direct", "GC_BEADS_TARGET=external")
		f.probeDir = mustEvalSymlinks(t, filepath.Join(rig, ".beads", "dolt"))
	case stallProxiedOwnRoot:
		writeProxiedSidecar(t, city, filepath.Join(city, ".beads", "dolt"))
		writeProxiedSidecar(t, rig, filepath.Join(rig, ".beads", "dolt"))
		f.probeDir = mustEvalSymlinks(t, filepath.Join(rig, ".beads", "dolt"))
	case stallProxiedSharedRoot:
		writeProxiedSidecar(t, city, filepath.Join(city, ".beads", "dolt"))
		writeProxiedSidecar(t, rig, filepath.Join(city, ".beads", "dolt"))
		f.probeDir = mustEvalSymlinks(t, filepath.Join(city, ".beads", "dolt"))
	default:
		t.Fatalf("unknown scope kind %q", kind)
	}
	return f
}

func (f *stallScope) with(extra ...string) *stallScope {
	f.env = append(f.env, extra...)
	return f
}

// run executes one provider op and returns the combined output and the exit
// status (0 on success).
func (f *stallScope) run(op string) (out string, status int) {
	f.t.Helper()
	out, err := runProviderOwnedScriptOp(f.t, f.env, op)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return out, 0
	case errors.As(err, &exitErr):
		return out, exitErr.ExitCode()
	default:
		f.t.Fatalf("running %s: %v\n%s", op, err, out)
		return out, -1
	}
}

func (f *stallScope) calls() []string  { return bdCalls(f.t, f.bdLog) }
func (f *stallScope) probes() []string { return bdCalls(f.t, f.probeLog) }

func (f *stallScope) assertCalls(out string, want ...string) {
	f.t.Helper()
	got := f.calls()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		f.t.Fatalf("bd calls = %q, want %q\n%s", got, want, out)
	}
}

// assertProbedOnce checks the guard measured exactly the directory the retire
// would have stopped, with the default 1000ms bound.
func (f *stallScope) assertProbedOnce(out, wantDir string) {
	f.t.Helper()
	got := f.probes()
	want := "--dir " + wantDir + " --timeout-ms 1000"
	if len(got) != 1 || got[0] != want {
		f.t.Fatalf("fsync probes = %q, want exactly [%q]\n%s", got, want, out)
	}
}

func (f *stallScope) assertNoProbe(out string) {
	f.t.Helper()
	if got := f.probes(); len(got) != 0 {
		f.t.Fatalf("fsync probe ran (%q) where nothing is retired\n%s", got, out)
	}
}

var stallRecoverKinds = []stallScopeKind{stallDirectLocal, stallProxiedOwnRoot}

// (a, b) A stalled scope is not retired. The op still pings, reports the
// stall, and exits with the ping's status rather than faking success.
func TestProviderScriptRecoverSkipsTheRetireWhileStorageStalls(t *testing.T) {
	probes := map[string][]string{
		"unfinished probe":               {"FAKE_PROBE_MODE=unfinished"},
		"finished at the warn threshold": {"FAKE_PROBE_MODE=finished", "FAKE_PROBE_MS=1000"},
	}
	for _, kind := range stallRecoverKinds {
		for probeName, probeEnv := range probes {
			for _, pingRC := range []int{0, stallPingFailure} {
				t.Run(fmt.Sprintf("%s/%s/ping exits %d", kind, probeName, pingRC), func(t *testing.T) {
					f := newStallScope(t, kind).with(probeEnv...).with(fmt.Sprintf("FAKE_PING_RC=%d", pingRC))
					out, status := f.run("recover")

					f.assertCalls(out, "ping")
					for _, want := range []string{"storage stall", stallDocsPointer, "gc dolt restart --force"} {
						if !strings.Contains(out, want) {
							t.Fatalf("recover output missing %q:\n%s", want, out)
						}
					}
					if status != pingRC {
						t.Fatalf("recover exit status = %d, want the ping's %d\n%s", status, pingRC, out)
					}
					f.assertProbedOnce(out, f.probeDir)
					if strings.Contains(out, stallFailOpen) {
						t.Fatalf("a stall decision must not print the fail-open line:\n%s", out)
					}
				})
			}
		}
	}
}

// (c) A rig that shares the city's proxy root stops nothing, as before: there
// is no retire to guard, so there is nothing to probe either.
func TestProviderScriptRecoverOfASharedRootRigStaysPingOnlyUnderAStall(t *testing.T) {
	f := newStallScope(t, stallProxiedSharedRoot).with("FAKE_PROBE_MODE=unfinished")
	out, status := f.run("recover")
	if status != 0 {
		t.Fatalf("recover exit status = %d\n%s", status, out)
	}
	f.assertCalls(out, "ping")
	if !strings.Contains(out, "recovering by ping only") {
		t.Fatalf("recover output missing the ping-only note:\n%s", out)
	}
	if strings.Contains(out, "storage stall") {
		t.Fatalf("a scope that retires nothing must not report a stall:\n%s", out)
	}
	f.assertNoProbe(out)
}

// A scope bd bound to someone else's server has no local Dolt to retire, so it
// is neither stopped nor probed.
func TestProviderScriptRecoverOfAnExternalScopeNeverProbes(t *testing.T) {
	f := newStallScope(t, stallDirectExternal).with("FAKE_PROBE_MODE=unfinished")
	out, status := f.run("recover")
	if status != 0 {
		t.Fatalf("recover exit status = %d\n%s", status, out)
	}
	f.assertCalls(out, "ping")
	f.assertNoProbe(out)
}

// (d) A fast filesystem changes nothing: the retire runs, then the ping.
func TestProviderScriptRecoverStillRetiresWhenFsyncIsFast(t *testing.T) {
	for _, kind := range stallRecoverKinds {
		t.Run(string(kind), func(t *testing.T) {
			f := newStallScope(t, kind).with("FAKE_PROBE_MODE=finished", "FAKE_PROBE_MS=999")
			out, status := f.run("recover")
			if status != 0 {
				t.Fatalf("recover exit status = %d\n%s", status, out)
			}
			f.assertCalls(out, "dolt stop", "ping")
			if strings.Contains(out, "storage stall") || strings.Contains(out, stallFailOpen) {
				t.Fatalf("a fast probe must stay silent:\n%s", out)
			}
			f.assertProbedOnce(out, f.probeDir)
		})
	}
}

// (e) A detector that cannot answer never blocks recovery: the retire proceeds
// and one line says the guard was skipped.
func TestProviderScriptRecoverFailsOpenWhenTheProbeIsInconclusive(t *testing.T) {
	cases := map[string][]string{
		"probe status=error":    {"FAKE_PROBE_MODE=error"},
		"helper exits non-zero": {"FAKE_PROBE_MODE=exit1"},
		"unparsable output":     {"FAKE_PROBE_MODE=garbage"},
		"no gc binary":          {"FAKE_PROBE_MODE=unfinished", "GC_BIN="},
	}
	for _, kind := range stallRecoverKinds {
		for name, extra := range cases {
			t.Run(fmt.Sprintf("%s/%s", kind, name), func(t *testing.T) {
				f := newStallScope(t, kind).with(extra...)
				out, status := f.run("recover")
				if status != 0 {
					t.Fatalf("recover exit status = %d\n%s", status, out)
				}
				f.assertCalls(out, "dolt stop", "ping")
				if n := countLinesContaining(out, stallFailOpen); n != 1 {
					t.Fatalf("fail-open stderr lines = %d, want exactly 1; output:\n%s", n, out)
				}
				if strings.Contains(out, "storage stall") {
					t.Fatalf("an inconclusive probe must not report a stall:\n%s", out)
				}
			})
		}
	}
}

// (f) An intentional stop is never blocked, and is never probed: the guard
// belongs to recovery only.
func TestProviderScriptStopAndShutdownIgnoreTheStallGuard(t *testing.T) {
	for _, kind := range []stallScopeKind{stallDirectLocal, stallProxiedOwnRoot, stallProxiedSharedRoot} {
		for _, op := range []string{"stop", "shutdown"} {
			t.Run(fmt.Sprintf("%s/%s", kind, op), func(t *testing.T) {
				f := newStallScope(t, kind).with("FAKE_PROBE_MODE=unfinished")
				out, status := f.run(op)
				if status != 0 {
					t.Fatalf("%s exit status = %d\n%s", op, status, out)
				}
				f.assertCalls(out, "dolt stop")
				if strings.Contains(out, "storage stall") {
					t.Fatalf("%s must never report a stall:\n%s", op, out)
				}
				f.assertNoProbe(out)
			})
		}
	}
}

// (g) The probe must measure the filesystem the retire would stop a Dolt on,
// which is the scope's physical Dolt root, not the rig's own directory. For a
// proxied scope that root comes from the sidecar, and a relative root_path is
// relative to BEADS_DIR (as bd and the rest of this script resolve it).
func TestProviderScriptRecoverProbesThePhysicalDataRoot(t *testing.T) {
	stalled := "FAKE_PROBE_MODE=unfinished"
	t.Run("proxied scope whose sidecar names its own absolute root", func(t *testing.T) {
		f := newStallScope(t, stallProxiedOwnRoot).with(stalled)
		own := filepath.Join(f.city, "elsewhere", "rig-api-root")
		mustMkdirAll(t, own)
		writeProxiedSidecar(t, f.scope, own)
		out, _ := f.run("recover")
		f.assertProbedOnce(out, mustEvalSymlinks(t, own))
	})
	t.Run("proxied scope whose sidecar names a root relative to BEADS_DIR", func(t *testing.T) {
		f := newStallScope(t, stallProxiedOwnRoot).with(stalled)
		own := filepath.Join(f.scope, "shared-root")
		mustMkdirAll(t, own)
		writeProxiedSidecar(t, f.scope, filepath.Join("..", "shared-root"))
		out, _ := f.run("recover")
		f.assertProbedOnce(out, mustEvalSymlinks(t, own))
	})
	t.Run("proxied scope with no sidecar falls back to its .beads", func(t *testing.T) {
		f := newStallScope(t, stallProxiedOwnRoot).with(stalled, "GC_BEADS_TRANSPORT=proxied")
		if err := os.Remove(filepath.Join(f.scope, ".beads", "proxied_server_client_info.json")); err != nil {
			t.Fatal(err)
		}
		out, _ := f.run("recover")
		f.assertProbedOnce(out, mustEvalSymlinks(t, filepath.Join(f.scope, ".beads")))
	})
	t.Run("direct local scope probes its .beads/dolt", func(t *testing.T) {
		f := newStallScope(t, stallDirectLocal).with(stalled)
		out, _ := f.run("recover")
		f.assertProbedOnce(out, mustEvalSymlinks(t, filepath.Join(f.scope, ".beads", "dolt")))
	})
	t.Run("direct local scope without a dolt directory probes its .beads", func(t *testing.T) {
		f := newStallScope(t, stallDirectLocal).with(stalled)
		if err := os.Remove(filepath.Join(f.scope, ".beads", "dolt")); err != nil {
			t.Fatal(err)
		}
		out, _ := f.run("recover")
		f.assertProbedOnce(out, mustEvalSymlinks(t, filepath.Join(f.scope, ".beads")))
	})
}
