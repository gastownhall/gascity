//go:build windows

package events

import (
	"encoding/binary"
	"hash/fnv"
	"io/fs"
	"os"

	"github.com/gastownhall/gascity/internal/platform/livefile"
	"golang.org/x/sys/windows"
)

// statInode returns path's FileInfo and file identity, read from one handle
// opened with livefile.Open so it never blocks the recorder's rotation rename.
func statInode(path string) (fs.FileInfo, uint64, error) {
	f, err := livefile.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	return inodeOfFile(f)
}

// inodeOfFile returns the open file f's FileInfo and its identity for the
// watcher's rotation detector. fs.FileInfo carries no inode on Windows, so
// the identity is read from the handle: the volume serial number and file index
// that os.SameFile compares, which stay fixed while the file is appended to and
// differ for the fresh file a rotation creates at the same path.
func inodeOfFile(f *os.File) (fs.FileInfo, uint64, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	var d windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &d); err != nil {
		return nil, 0, &os.PathError{Op: "GetFileInformationByHandle", Path: f.Name(), Err: err}
	}
	var key [12]byte
	binary.LittleEndian.PutUint32(key[0:], d.VolumeSerialNumber)
	binary.LittleEndian.PutUint32(key[4:], d.FileIndexHigh)
	binary.LittleEndian.PutUint32(key[8:], d.FileIndexLow)
	h := fnv.New64a()
	h.Write(key[:]) //nolint:errcheck // hash.Hash.Write never fails
	return info, h.Sum64(), nil
}
