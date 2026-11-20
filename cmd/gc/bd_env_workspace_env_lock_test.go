//go:build !windows

package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// boundedEnvBuild runs fn off the test goroutine, because the point is that it
// might block, and requires it to return within five seconds. On timeout it
// records a goroutine dump, then releases the lock and joins before failing so
// the woken goroutine cannot race the tempdir cleanup.
func boundedEnvBuild(t *testing.T, what string, release func(), fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		release()
		<-done
		t.Fatalf("%s blocked on a held repo-cache lock; goroutines at the timeout:\n%s", what, buf)
	}
}

// writePinnedWorkspaceEnv appends a [workspace.env] PATH pin to the city.toml
// that setupDiscoveryLockCity wrote and returns the pinned bd path. The
// import stays in place: it is what puts a composed load behind the lock.
func writePinnedWorkspaceEnv(t *testing.T, cityPath string) string {
	t.Helper()
	pinDir := t.TempDir()
	pinned := filepath.Join(pinDir, "bd")
	writeExecutable(t, pinned, "#!/bin/sh\nexit 0\n")
	f, err := os.OpenFile(filepath.Join(cityPath, "city.toml"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	if _, err := f.WriteString("\n[workspace.env]\nPATH = " + strconv.Quote(pinDir) + "\n"); err != nil {
		t.Fatal(err)
	}
	return pinned
}

// The control: the composed load the pin used to take really does queue behind
// an exclusive repo-cache holder in THIS fixture. Without it the tests below
// would stay green if the fixture stopped importing anything.
func TestWorkspacePinFixtureComposedLoadWaitsOnHeldLock(t *testing.T) {
	cityPath, cacheRoot := setupDiscoveryLockCity(t)
	writePinnedWorkspaceEnv(t, cityPath)
	release := holdExclusiveRepoCacheLock(t, cacheRoot)

	done := make(chan struct{})
	go func() {
		defer close(done)
		loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard) //nolint:errcheck // only the timing matters
	}()
	select {
	case <-done:
		t.Fatal("composed load returned while the repo-cache lock was held; the fixture no longer reaches the lock")
	case <-time.After(500 * time.Millisecond):
	}
	release()
	<-done
}

// The workspace pin is read from the root city.toml alone, so an exclusive
// repo-cache holder (a `gc import install` clone) must not stall it.
func TestWorkspacePinDoesNotWaitOnHeldRepoCacheLock(t *testing.T) {
	cityPath, cacheRoot := setupDiscoveryLockCity(t)
	pinned := writePinnedWorkspaceEnv(t, cityPath)
	resetWorkspaceEnvCache()
	t.Cleanup(resetWorkspaceEnvCache)
	t.Setenv("PATH", t.TempDir())
	release := holdExclusiveRepoCacheLock(t, cacheRoot)

	var gotStrict, gotApplied string
	var strictErr, appliedErr error
	boundedEnvBuild(t, "workspace pin resolution", release, func() {
		gotStrict, strictErr = workspacePinnedBdBinary(cityPath)
		env := map[string]string{}
		appliedErr = applyWorkspacePinnedBdBinary(env, cityPath)
		gotApplied = env["BD_BIN"]
	})
	if strictErr != nil || gotStrict != pinned {
		t.Fatalf("workspacePinnedBdBinary = %q, %v; want %q", gotStrict, strictErr, pinned)
	}
	if appliedErr != nil || gotApplied != pinned {
		t.Fatalf("applyWorkspacePinnedBdBinary BD_BIN = %q, %v; want %q", gotApplied, appliedErr, pinned)
	}
}

// The steady-state bd env build of a managed city, with the hosted-credential
// probe's memo primed, must take no repo-cache lock at all. If this still
// blocks once the pin no longer loads the composed config, another call in the
// build is loading it: the goroutine dump names it.
func TestBdRuntimeEnvSteadyStateDoesNotWaitOnHeldRepoCacheLock(t *testing.T) {
	cityPath, cacheRoot := setupDiscoveryLockCity(t)
	pinned := writePinnedWorkspaceEnv(t, cityPath)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("PATH", t.TempDir())

	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scopeMetadataJSONPath(cityPath), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"demo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }() //nolint:errcheck // test cleanup
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: "2026-10-13T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	// Prime the hosted-credential probe's memo with a valid entry: its miss
	// path is a known residual (it still takes the composed load) and is not
	// what this test is about.
	resetHostedCredentialProbeCache()
	t.Cleanup(resetHostedCredentialProbeCache)
	resetWorkspaceEnvCache()
	t.Cleanup(resetWorkspaceEnvCache)
	cityConfig := filepath.Join(cityPath, "city.toml")
	stamp, ok := statHostedCredentialProbeSource(cityConfig)
	if !ok {
		t.Fatal("stat city.toml")
	}
	hostedCredentialProbeCache.Store(normalizePathForCompare(cityConfig), &hostedCredentialProbeEntry{
		selected: false,
		sources:  []hostedCredentialProbeSource{stamp},
	})

	release := holdExclusiveRepoCacheLock(t, cacheRoot)

	for name, build := range map[string]func() (map[string]string, error){
		"bdRuntimeEnvWithError": func() (map[string]string, error) { return bdRuntimeEnvWithError(cityPath) },
		"bdRuntimeEnvWithErrorRecoveryContext": func() (map[string]string, error) {
			return bdRuntimeEnvWithErrorRecoveryContext(context.Background(), cityPath, false)
		},
	} {
		var env map[string]string
		var buildErr error
		boundedEnvBuild(t, name, release, func() { env, buildErr = build() })
		if buildErr != nil {
			t.Fatalf("%s: %v", name, buildErr)
		}
		if got := env["BD_BIN"]; got != pinned {
			t.Fatalf("%s: BD_BIN = %q, want the workspace pin %q", name, got, pinned)
		}
	}
}
