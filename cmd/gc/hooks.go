package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// beadEventHookNames lists the gc-installed bd event-forwarding hook names
// removed by installBeadHooks. These hooks previously spawned a gc subprocess
// per bead write; the controller's CachingStore now emits the same events
// in-process and runs convoy/wisp/molecule autoclose natively.
var beadEventHookNames = []string{"on_create", "on_update", "on_close"}

// hookStampLine returns the version-stamp comment embedded in gc-managed hook
// scripts so that installBeadHooks can distinguish them from user-authored hooks.
func hookStampLine() string {
	return fmt.Sprintf("# gc-hook-stamp: %s %s", date, commit)
}

// isGCManagedHook reports whether the hook content was written by gc.
// It recognizes both the current gc-hook-stamp format and the legacy
// "# Installed by gc" marker used before stamping was introduced.
// Only gc-managed hooks are removed during cleanup; user-authored hooks
// with the same filename are left untouched.
func isGCManagedHook(content []byte) bool {
	if parseHookStampDate(content) != "" {
		return true
	}
	// Legacy hooks written by older gc versions use "# Installed by gc"
	// as the first marker line and invoke "$GC_BIN" event emit per write.
	return bytes.Contains(content, []byte("# Installed by gc")) &&
		bytes.Contains(content, []byte(`"$GC_BIN" event emit`))
}

// parseHookStampDate extracts the build date from a hook script's stamp line.
// Returns empty string if no stamp is found.
func parseHookStampDate(content []byte) string {
	for _, line := range bytes.Split(content, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("# gc-hook-stamp: ")) {
			parts := bytes.Fields(line)
			if len(parts) >= 3 {
				return string(parts[2])
			}
		}
	}
	return ""
}

// installBeadHooks removes any gc-installed bead event-forwarding hooks from
// dir/.beads/hooks/. The hook subprocess chain (gc event emit + gc convoy
// autoclose + gc wisp autoclose + gc molecule autoclose) is replaced by the
// controller's in-process CachingStore event path, which emits the same events
// via its onChange callback and runs autoclose in runBeadCloseAutoclose.
//
// Only hooks that carry a gc-hook-stamp are removed; user-authored hooks with
// the same filename are left untouched. This function is idempotent: it is
// safe to call when the hooks do not exist.
func installBeadHooks(dir, _ string) error {
	hooksDir := filepath.Join(dir, ".beads", "hooks")
	for _, filename := range beadEventHookNames {
		hookPath := filepath.Join(hooksDir, filename)
		content, err := os.ReadFile(hookPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reading bead event hook %s: %w", filename, err)
		}
		if !isGCManagedHook(content) {
			continue
		}
		if err := os.Remove(hookPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing bead event hook %s: %w", filename, err)
		}
	}
	return nil
}
