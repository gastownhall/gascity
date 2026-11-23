package scripts_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func runLocalRuntimeShell(t *testing.T, path, script string, args ...string) string {
	t.Helper()
	lib := filepath.Join(repoRoot(t), localRuntimeLib)
	if _, err := os.Stat(lib); err != nil {
		t.Fatalf("shared runtime probe library is unavailable: %v", err)
	}
	argv := append([]string{"-c", "set -euo pipefail\nsource \"$1\"\nshift\n" + script, "probe-test", lib}, args...)
	cmd := testCommand("bash", argv...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if path != "" {
		cmd.Env = append(cmd.Env, "PATH="+path)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe shell: %v\nstdout: %s\nstderr: %s", err, out, &stderr)
	}
	return string(out)
}

func TestLocalRuntimeLibraryIsSafeToSource(t *testing.T) {
	lib := filepath.Join(repoRoot(t), localRuntimeLib)
	cmd := testCommand("bash", "-c", `set -euo pipefail
before_flags=$-
before="$(set +o)"
uname() { printf 'source-time platform probe\n' >&2; return 1; }
source "$1"
if [ "$before_flags" != "$-" ]; then
  printf 'sourcing changed shell flags\n' >&2
  exit 1
fi
after="$(set +o)"
[ "$before" = "$after" ]
`, "source-test", lib)
	out, err := cmd.CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Fatalf("sourcing must be silent and preserve shell options: err=%v output=%q", err, out)
	}
}

func TestLocalRuntimeProbePlatformSelection(t *testing.T) {
	for _, tc := range []struct {
		name, os, arch, want string
	}{
		{name: "macOS has no pinned sysroot", os: "Darwin", arch: "arm64", want: "n/a Darwin/arm64\n"},
		{name: "Linux arm64 has no pinned sysroot", os: "Linux", arch: "aarch64", want: "n/a Linux/aarch64\n"},
		{name: "Linux x86 needs a bounded runner", os: "Linux", arch: "x86_64", want: "unknown "},
		{name: "unreadable uname is unknown", want: "unknown "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() // Deliberately no timeout, gtimeout or perl.
			if tc.os != "" {
				writeExecutable(t, filepath.Join(path, "uname"), "#!/bin/sh\ncase \"${1:--s}\" in\n-s) printf '%s\\n' "+shellQuote(tc.os)+" ;;\n-m) printf '%s\\n' "+shellQuote(tc.arch)+" ;;\n-sm|-ms) printf '%s %s\\n' "+shellQuote(tc.os)+" "+shellQuote(tc.arch)+" ;;\n*) exit 1 ;;\nesac\n")
			}
			got := runLocalRuntimeShell(t, path, "local_runtime_probe\n")
			if strings.HasPrefix(tc.want, "unknown") {
				assertUnknownRuntimeResult(t, got)
			} else if got != tc.want {
				t.Fatalf("platform probe = %q, want %q", got, tc.want)
			}
		})
	}
}

func requireGLibcRuntime(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("real preload classification requires Linux/glibc; portable hook matrices still run")
	}
	out, err := testCommand("getconf", "GNU_LIBC_VERSION").Output()
	if err != nil || !strings.HasPrefix(string(out), "glibc ") {
		t.Skip("real preload classification requires glibc; portable hook matrices still run")
	}
}

func TestLocalRuntimeProbeRealLoader(t *testing.T) {
	requireGLibcRuntime(t)
	const absent = "libgascity_test_intentionally_absent.so.919191"
	const canary = "libgascity_test_loader_canary.so.919192"
	for _, tc := range []struct {
		name, carrier, sonames, canary, want string
	}{
		{name: "resolvable libraries", carrier: "/usr/bin/true", sonames: "libc.so.6 libm.so.6", canary: canary, want: "ok\n"},
		{name: "missing library despite successful carrier", carrier: "/usr/bin/true", sonames: absent, canary: canary, want: "missing " + absent + "\n"},
		{name: "report only missing names in input order", carrier: "/usr/bin/true", sonames: absent + " libc.so.6 " + absent + ".second", canary: canary, want: "missing " + absent + " " + absent + ".second\n"},
		{name: "missing canary evidence", carrier: "/usr/bin/true", sonames: absent, canary: "libc.so.6", want: "unknown "},
		{name: "carrier cannot start", carrier: "/absent/gascity/carrier", sonames: absent, canary: canary, want: "unknown "},
		{name: "carrier exits nonzero", carrier: "/usr/bin/false", sonames: absent, canary: canary, want: "unknown "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runLocalRuntimeShell(t, "", `local_runtime_probe_run "$1" "$2" "$3" 10`, tc.carrier, tc.sonames, tc.canary)
			if strings.HasPrefix(tc.want, "unknown") {
				assertUnknownRuntimeResult(t, got)
			} else if got != tc.want {
				t.Fatalf("loader probe = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLocalRuntimeProbeNeverRunsUnbounded(t *testing.T) {
	path, marker := t.TempDir(), filepath.Join(t.TempDir(), "carrier-ran")
	carrier := filepath.Join(t.TempDir(), "carrier")
	writeExecutable(t, carrier, "#!/bin/sh\nprintf 'ran\\n' > "+shellQuote(marker)+"\n")
	got := runLocalRuntimeShell(t, path, `local_runtime_probe_run "$1" libc.so.6 libabsent_canary.so 1`, carrier)
	assertUnknownRuntimeResult(t, got)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("carrier ran without any bounded runner: marker stat = %v", err)
	}
}

func TestLocalRuntimeProbeTimeoutIsUnknown(t *testing.T) {
	requireGLibcRuntime(t)
	carrier := filepath.Join(t.TempDir(), "sleeping-carrier")
	writeExecutable(t, carrier, "#!/bin/sh\nexec /bin/sleep 30\n")
	start := time.Now()
	got := runLocalRuntimeShell(t, "", `local_runtime_probe_run "$1" libc.so.6 libabsent_canary.so 1`, carrier)
	assertUnknownRuntimeResult(t, got)
	if !strings.Contains(got, "timed out") {
		t.Errorf("timeout reason is missing: %q", got)
	}
	// The deadline itself is the subject: a one-second bound must not wait for
	// the carrier's 30-second sleep. This is not a goroutine race deadline.
	if elapsed := time.Since(start); elapsed >= 10*time.Second {
		t.Errorf("one-second probe took %v, want less than 10s", elapsed)
	}
}

func assertUnknownRuntimeResult(t *testing.T, got string) {
	t.Helper()
	if !strings.HasPrefix(got, "unknown ") || strings.TrimSpace(strings.TrimPrefix(got, "unknown ")) == "" || strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("probe must return zero and print one unknown line with a reason, got %q", got)
	}
}
