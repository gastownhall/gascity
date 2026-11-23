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
