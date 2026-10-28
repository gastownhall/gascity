//go:build windows

package main

import (
	"bytes"
	"io"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// TestHookRunSpoolReadsDoNotMoveTheChildsWritePosition pins that reading the
// spool cannot move the position the child and its descendants write at. They
// all write through copies of the child's stdout handle, which share one file
// position. Reading at an offset through a synchronous Windows handle moves
// that handle's position while it reads, so a reader sharing it would let a
// descendant's write land inside output the child already sealed. Seeking the
// reader makes that sharing visible deterministically.
func TestHookRunSpoolReadsDoNotMoveTheChildsWritePosition(t *testing.T) {
	spool, err := createHookRunSpool()
	if err != nil {
		t.Fatalf("createHookRunSpool: %v", err)
	}
	t.Cleanup(func() { spool.close(io.Discard) })
	if _, err := io.WriteString(spool.out, "sealed"); err != nil {
		t.Fatalf("writing spool: %v", err)
	}
	if _, err := spool.in.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seeking the reader: %v", err)
	}
	if _, err := io.WriteString(spool.out, "-tail"); err != nil {
		t.Fatalf("writing spool: %v", err)
	}
	got, err := spool.read()
	if err != nil || string(got) != "sealed-tail" {
		t.Fatalf("spool.read = (%q, %v), want (%q, nil)", got, err, "sealed-tail")
	}
}

// TestHookRunSpoolIsDeletedWhenLastHandleCloses pins that a spool a child's
// descendant still holds open is deleted once that last handle closes. A
// duplicated handle stands in for the one the descendant inherited. Deleting
// by name after closing the wrapper's handle fails while such a handle is
// open, which used to leave the spool, and the output in it, behind for good.
func TestHookRunSpoolIsDeletedWhenLastHandleCloses(t *testing.T) {
	spool, err := createHookRunSpool()
	if err != nil {
		t.Fatalf("createHookRunSpool: %v", err)
	}
	name := spool.out.Name()
	process := windows.CurrentProcess()
	var held windows.Handle
	if err := windows.DuplicateHandle(process, windows.Handle(spool.out.Fd()), process, &held, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		t.Fatalf("DuplicateHandle: %v", err)
	}

	var stderr bytes.Buffer
	spool.close(&stderr)
	if stderr.Len() != 0 {
		t.Fatalf("spool.close reported %q, want nothing", stderr.String())
	}
	if err := windows.CloseHandle(held); err != nil {
		t.Fatalf("closing the held handle: %v", err)
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("spool %s still exists after its last handle closed (stat err=%v)", name, err)
	}
}
