//go:build windows

package main

import (
	"crypto/rand"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// createHookRunSpool creates the file gc hook run captures child stdout in.
// FILE_FLAG_DELETE_ON_CLOSE removes it when its last handle closes, whether
// that is this process's handle or a copy the child's descendants inherited,
// even if this process dies first. A file that is only deleted by name after
// use cannot be deleted while a descendant still holds it.
//
// The wrapper reads through a second open of the file. The child and its
// descendants write through copies of the child's stdout handle, which share
// one file position, and reading at an offset through a synchronous handle
// moves that handle's position while the read runs. Reading through the
// child's handle would let a descendant's write land inside output the child
// had already sealed.
func createHookRunSpool() (*hookRunSpool, error) {
	name := filepath.Join(os.TempDir(), "gc-hook-run-stdout-"+rand.Text())
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: name, Err: err}
	}
	const share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	out, err := windows.CreateFile(
		path,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE,
		share,
		nil,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_TEMPORARY|windows.FILE_FLAG_DELETE_ON_CLOSE,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: name, Err: err}
	}
	in, err := windows.CreateFile(path, windows.GENERIC_READ, share, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		_ = windows.CloseHandle(out)
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return &hookRunSpool{out: os.NewFile(uintptr(out), name), in: os.NewFile(uintptr(in), name)}, nil
}
