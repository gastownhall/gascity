package bdflags

import "testing"

// TestBoolFlagsKnowForce pins --force into every write-mutation subcommand that
// documents it. When "update" lost it, gc's pre-flight exact-ID guard
// (cmd/gc/cmd_bd.go bdMutationWriteIDs) read --force as an unknown,
// possibly value-consuming flag and failed closed, so every
// `gc bd update <id> --status=blocked --force -a ""` aborted without writing
// and the bead stayed on its hook.
func TestBoolFlagsKnowForce(t *testing.T) {
	for _, sub := range []string{"create", "update", "close", "delete"} {
		if !BoolFlags(sub)["--force"] {
			t.Errorf("BoolFlags(%q) does not know --force, but `bd %s --help` documents it", sub, sub)
		}
	}
}
