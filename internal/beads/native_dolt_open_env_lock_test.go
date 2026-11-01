package beads

import (
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/testutil"
)

// The proxied-native open window and the session runtimes must share one
// process-env lock. The window withholds the BEADS_ and BD_ namespaces and
// projects the scope's endpoint, database and author pair for the whole
// library open; a child environment built through execenv.Environ during it
// must wait, then see the ambient environment the window restored.
func TestProxiedNativeOpenWindowBlocksChildEnvSnapshots(t *testing.T) {
	t.Setenv("BD_BIN", "/ambient/bd")
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "")

	nativeDoltOpenEnvMu.Lock()
	restoreNamespaces, err := withWithheldPrefixesLocked(beadsEnvPrefix, bdEnvPrefix)
	if err != nil {
		nativeDoltOpenEnvMu.Unlock()
		t.Fatal(err)
	}
	restoreEnv, err := withProjectedOpenEnvLocked(proxiedNativeOpenEnvKeys, map[string]string{
		"BEADS_DOLT_SERVER_DATABASE": "window-db",
	}, "")
	if err != nil {
		restoreNamespaces()
		nativeDoltOpenEnvMu.Unlock()
		t.Fatal(err)
	}
	open := true
	closeWindow := func() {
		if !open {
			return
		}
		open = false
		restoreEnv()
		restoreNamespaces()
		nativeDoltOpenEnvMu.Unlock()
	}
	t.Cleanup(closeWindow)

	envCh := make(chan []string, 1)
	go func() { envCh <- execenv.Environ() }()
	select {
	case env := <-envCh:
		closeWindow()
		t.Fatalf("execenv.Environ returned inside a proxied open window: BEADS_DOLT_SERVER_DATABASE=%v BD_BIN=%v",
			envValues(env, "BEADS_DOLT_SERVER_DATABASE"), envValues(env, "BD_BIN"))
	case <-time.After(10 * time.Millisecond):
	}
	closeWindow()

	select {
	case env := <-envCh:
		if got := envValues(env, "BEADS_DOLT_SERVER_DATABASE"); slices.Contains(got, "window-db") {
			t.Fatalf("BEADS_DOLT_SERVER_DATABASE after the window = %v, want the window's projection gone", got)
		}
		if got := envValues(env, "BD_BIN"); len(got) != 1 || got[0] != "/ambient/bd" {
			t.Fatalf("BD_BIN after the window = %v, want [/ambient/bd]", got)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("execenv.Environ did not return after the proxied open window closed")
	}
}
