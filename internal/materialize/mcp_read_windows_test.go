//go:build windows

package materialize

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestReadRegularOSFileReadsARegularFileOnWindows pins that the no-follow
// open still reads an ordinary file.
func TestReadRegularOSFileReadsARegularFileOnWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{"servers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readRegularOSFile(path)
	if err != nil {
		t.Fatalf("readRegularOSFile: %v", err)
	}
	if string(got) != `{"servers":{}}` {
		t.Fatalf("readRegularOSFile = %q, want the file's contents", got)
	}
}

// TestReadRegularOSFileRefusesASymlinkOnWindows pins that a symlink to a
// regular file is refused from the opened handle itself, so a link swapped in
// at the path is never followed.
func TestReadRegularOSFileRefusesASymlinkOnWindows(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("creating a symlink needs SeCreateSymbolicLinkPrivilege or Developer Mode, which this host lacks: %v", err)
	}
	_, err := readRegularOSFile(link)
	var notRegular *NotRegularFileError
	if !errors.As(err, &notRegular) {
		t.Fatalf("readRegularOSFile(symlink) = %v, want *NotRegularFileError", err)
	}
	if notRegular.Mode&os.ModeSymlink == 0 {
		t.Fatalf("NotRegularFileError.Mode = %v, want a symlink", notRegular.Mode)
	}
}

// TestReadRegularOSFileRefusesADirectoryOnWindows pins that a directory,
// which FILE_FLAG_BACKUP_SEMANTICS lets the open succeed on, is refused.
func TestReadRegularOSFileRefusesADirectoryOnWindows(t *testing.T) {
	_, err := readRegularOSFile(t.TempDir())
	var notRegular *NotRegularFileError
	if !errors.As(err, &notRegular) {
		t.Fatalf("readRegularOSFile(directory) = %v, want *NotRegularFileError", err)
	}
}
