package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// beadEventHookNames lists the gc-installed bd event-forwarding hook names
// removed by installBeadHooks. These hooks previously spawned a gc subprocess
// per bead write; the controller's CachingStore now emits the same events
// in-process and runs convoy/wisp/molecule autoclose natively.
var beadEventHookNames = []string{"on_create", "on_update", "on_close"}

// installBeadHooks removes any gc-installed bead event-forwarding hooks from
// dir/.beads/hooks/. The hook subprocess chain (gc event emit + gc convoy
// autoclose + gc wisp autoclose + gc molecule autoclose) is replaced by the
// controller's in-process CachingStore event path, which emits the same events
// via its onChange callback and runs autoclose in runBeadCloseAutoclose.
//
// Non-gc hooks (e.g. git pre-commit hooks) in the directory are left
// untouched. This function is idempotent: it is safe to call when the hooks
// do not exist.
func installBeadHooks(dir, _ string) error {
	hooksDir := filepath.Join(dir, ".beads", "hooks")
	for _, filename := range beadEventHookNames {
		if err := os.Remove(filepath.Join(hooksDir, filename)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing bead event hook %s: %w", filename, err)
		}
	}
	return nil
}
