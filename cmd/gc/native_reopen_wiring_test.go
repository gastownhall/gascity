package main

import (
	"os"
	"strings"
	"testing"
)

// TestNativeReopenHookWiredAtBothStoreOpenSites pins the two production sites
// that must arm the NativeDoltStore read-path reconnect hook: the CLI provider
// store (openStoreResultAtForCity in main.go) and the controller reconcile rig
// store (openRigStore in api_state.go). Deleting either WithNativeReopen wiring
// would silently re-expose the managed-Dolt hard-kill/rebind read failure #4197
// fixed, so this test fails if either the hook or its ctx-threaded env
// re-resolution goes missing.
func TestNativeReopenHookWiredAtBothStoreOpenSites(t *testing.T) {
	for _, tc := range []struct {
		file string
		site string
	}{
		{file: "main.go", site: "openStoreResultAtForCity"},
		{file: "api_state.go", site: "openRigStore"},
	} {
		data, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		src := string(data)
		if !strings.Contains(src, "beads.WithNativeReopen(") {
			t.Fatalf("%s (%s): native reopen hook wiring beads.WithNativeReopen(...) is missing — the #4197 managed-Dolt rebind reconnect must stay armed", tc.file, tc.site)
		}
		if !strings.Contains(src, "nativeDoltOpenEnvForScopeContext(ctx") {
			t.Fatalf("%s (%s): the reopen hook must re-resolve the managed Dolt env under the wall context via nativeDoltOpenEnvForScopeContext(ctx, ...)", tc.file, tc.site)
		}
	}
}
