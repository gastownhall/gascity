package beads

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
)

// The proxied-native open window and the session runtimes share one execenv
// projection gate. The window withholds the BEADS_ and BD_ namespaces and
// projects the scope's endpoint, database and author pair for the whole library
// open; a child environment built through execenv.Environ during it answers at
// once with the ambient environment, never the window's projection.
func TestProxiedNativeOpenWindowDoesNotBlockChildEnvSnapshots(t *testing.T) {
	t.Setenv("BD_BIN", "/ambient/bd")
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "")

	endWindow := holdProjectionWindow(t, []string{beadsEnvPrefix, bdEnvPrefix}, map[string]string{
		"BEADS_DOLT_SERVER_DATABASE": "window-db",
	})
	if got := os.Getenv("BD_BIN"); got != "" {
		t.Fatalf("the window did not withhold BD_BIN: os.Getenv = %q", got)
	}

	envCh := make(chan []string, 1)
	go func() { envCh <- execenv.Environ() }()
	select {
	case env := <-envCh:
		assertWindowInvisible(t, "inside the window", env)
	case <-time.After(time.Second):
		t.Fatal("execenv.Environ did not return within 1s inside a proxied open window")
	}
	endWindow()
	assertWindowInvisible(t, "after the window", execenv.Environ())
}

func assertWindowInvisible(t *testing.T, when string, env []string) {
	t.Helper()
	if got := envValues(env, "BEADS_DOLT_SERVER_DATABASE"); slices.Contains(got, "window-db") {
		t.Errorf("BEADS_DOLT_SERVER_DATABASE %s = %v, want the window's projection invisible", when, got)
	}
	if got := envValues(env, "BD_BIN"); len(got) != 1 || got[0] != "/ambient/bd" {
		t.Errorf("BD_BIN %s = %v, want [/ambient/bd]", when, got)
	}
}

// A projection that fails partway through its key list rolls back only what it
// touched. os.Setenv rejects a value with a NUL byte, so a failure at the second
// key leaves the third unreached: its ambient value must survive the rollback,
// and the keys the projection did reach must read as they did before the call,
// including a key that was unset.
func TestWithProjectedOpenEnvLockedFailureKeepsAmbientOfUnreachedKeys(t *testing.T) {
	const (
		reached   = "GC_TEST_PROJECTION_REACHED"
		failing   = "GC_TEST_PROJECTION_FAILING"
		unreached = "GC_TEST_PROJECTION_UNREACHED"
	)
	t.Setenv(reached, "ambient-reached")
	t.Setenv(failing, "placeholder")
	if err := os.Unsetenv(failing); err != nil {
		t.Fatalf("unsetting %s: %v", failing, err)
	}
	t.Setenv(unreached, "keep")
	holdProjectionWindow(t, nil, nil)

	restore, err := withProjectedOpenEnvLocked(
		[]string{reached, failing, unreached},
		map[string]string{reached: "ok", failing: "x\x00y"},
		"",
	)
	if err == nil {
		restore()
		t.Fatal("withProjectedOpenEnvLocked with a NUL-byte value: err = nil, want the projection to fail")
	}
	if restore != nil {
		t.Errorf("withProjectedOpenEnvLocked returned a restore func alongside its error")
	}

	if got := os.Getenv(unreached); got != "keep" {
		t.Errorf("%s = %q after the failed projection, want %q: the rollback unset a key the projection never reached", unreached, got, "keep")
	}
	if got := os.Getenv(reached); got != "ambient-reached" {
		t.Errorf("%s = %q after the failed projection, want %q", reached, got, "ambient-reached")
	}
	if got, ok := os.LookupEnv(failing); ok {
		t.Errorf("%s = %q after the failed projection, want it unset as before the call", failing, got)
	}
}
