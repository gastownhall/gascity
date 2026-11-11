//go:build windows

package tailfile

import (
	"os"
	"testing"
)

// TestTailFollowLetsTheFileBeRenamedAndRemoved pins that a follow does not
// stop its file from being rotated by rename or removed, as tail -f does not,
// and that it keeps following the file it opened: an append to the renamed
// file is written, a new file at the old path is not.
func TestTailFollowLetsTheFileBeRenamedAndRemoved(t *testing.T) {
	path := writeFile(t, "before\n")
	out := newSyncBuffer()
	stop := startFollow(t, path, 10, out, nil)
	waitForOutput(t, out, "before\n")
	rotated := path + ".1"
	if err := os.Rename(path, rotated); err != nil {
		t.Fatalf("renaming the followed file: %v", err)
	}
	if err := os.WriteFile(path, []byte("new file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	appendTo(t, rotated, "after rename\n")
	waitForOutput(t, out, "before\nafter rename\n")
	if err := os.Remove(rotated); err != nil {
		t.Fatalf("removing the followed file: %v", err)
	}
	if err := stop(); err != nil {
		t.Fatalf("follow returned %v", err)
	}
}
