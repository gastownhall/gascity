package tokenrender

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/connect"
)

// keyDir returns a fresh 0700 directory.
func keyDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeKeyFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestLoadKeyAcceptsValidKeys(t *testing.T) {
	raw := bytes.Repeat([]byte{0xfb}, 32)
	cases := map[string]string{
		"unpadded":         base64.RawURLEncoding.EncodeToString(raw),
		"with newline":     base64.RawURLEncoding.EncodeToString(raw) + "\n",
		"padded":           base64.URLEncoding.EncodeToString(raw) + "\n",
		"longer than 32 B": base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 64)),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(keyDir(t), "city.key")
			writeKeyFile(t, path, content, 0o600)
			key, err := LoadKey(path)
			if err != nil {
				t.Fatalf("LoadKey: %v", err)
			}
			if len(key) < connect.MinKeyLen {
				t.Fatalf("key is %d bytes", len(key))
			}
		})
	}
	path := filepath.Join(keyDir(t), "ro.key")
	writeKeyFile(t, path, base64.RawURLEncoding.EncodeToString(raw), 0o400)
	if _, err := LoadKey(path); err != nil {
		t.Fatalf("a 0400 key refused: %v", err)
	}
}

func TestLoadKeyRefusals(t *testing.T) {
	good := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	cases := []struct {
		name    string
		content string
		mode    os.FileMode
		dirMode os.FileMode
	}{
		{"short", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 31)), 0o600, 0o700},
		{"group-readable", good, 0o640, 0o700},
		{"world-readable", good, 0o604, 0o700},
		{"group-writable", good, 0o620, 0o700},
		{"badly encoded", "not base64url!!", 0o600, 0o700},
		{"standard base64 alphabet", strings.TrimRight(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb, 0xff}, 16)), "="), 0o600, 0o700},
		{"empty", "", 0o600, 0o700},
		{"loose parent", good, 0o600, 0o755},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := keyDir(t)
			path := filepath.Join(dir, "city.key")
			writeKeyFile(t, path, tc.content, tc.mode)
			if err := os.Chmod(dir, tc.dirMode); err != nil {
				t.Fatal(err)
			}
			key, err := LoadKey(path)
			if err == nil {
				t.Fatalf("LoadKey accepted (%d bytes)", len(key))
			}
			if strings.Contains(err.Error(), good) {
				t.Fatalf("error echoes the key: %v", err)
			}
			if (tc.mode|tc.dirMode)&0o077 != 0 && !strings.Contains(err.Error(), looseModeRemedy) {
				t.Fatalf("error %q does not say to strip the group and other bits", err)
			}
		})
	}
	if _, err := LoadKey(filepath.Join(keyDir(t), "missing.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing key error = %v, want ErrNotExist", err)
	}
	if _, err := LoadKey(keyDir(t)); err == nil {
		t.Fatal("a directory loaded as a key")
	}
}

func TestCreateKey(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "gc", "keys", "connect", "city.key")
	key, err := CreateKey(path)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("key is %d bytes, want 32", len(key))
	}
	if mode := statFile(t, path).Mode().Perm(); mode != 0o600 {
		t.Fatalf("key mode = %o, want 600", mode)
	}
	for dir := filepath.Dir(path); dir != root; dir = filepath.Dir(dir) {
		if mode := statFile(t, dir).Mode().Perm(); mode != 0o700 {
			t.Fatalf("%s mode = %o, want 700", dir, mode)
		}
	}
	data := readFile(t, path)
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("key file %q is not newline-terminated", data)
	}
	loaded, err := LoadKey(path)
	if err != nil || !bytes.Equal(loaded, key) {
		t.Fatalf("LoadKey after CreateKey = %v (equal %v)", err, bytes.Equal(loaded, key))
	}
	if _, err := CreateKey(path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("CreateKey over an existing key = %v, want ErrExist", err)
	}
	if again := readFile(t, path); !bytes.Equal(again, data) {
		t.Fatal("a refused CreateKey changed the existing key")
	}
	other, err := CreateKey(filepath.Join(filepath.Dir(path), "other.key"))
	if err != nil || bytes.Equal(other, key) {
		t.Fatalf("a second key = %v, equal %v", err, bytes.Equal(other, key))
	}
}

func TestCreateKeyRefusesLooseParent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "city.key")
	_, err := CreateKey(path)
	if err == nil {
		t.Fatal("CreateKey wrote a key into a 0755 directory")
	}
	if !strings.Contains(err.Error(), looseModeRemedy) {
		t.Fatalf("error %q does not say to strip the group and other bits", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused CreateKey left a file: %v", err)
	}
}

// looseModeRemedy is what a mode refusal tells the operator: any owner-only
// mode passes, so the remedy is the bits to clear, not one exact mode.
const looseModeRemedy = "strip its group and other permission bits"

// TestCreateKeyRemovesAKeyItCannotMakeDurable pins that a key whose
// directory sync fails is removed, so a retry creates a fresh key instead of
// failing on the leftover file with os.ErrExist.
func TestCreateKeyRemovesAKeyItCannotMakeDurable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens an unreadable directory, so its sync cannot be made to fail")
	}
	dir := keyDir(t)
	// Write and search without read: the key file can be created and
	// removed, but the directory cannot be opened to sync it.
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(dir, "city.key")
	if _, err := CreateKey(path); err == nil {
		t.Fatal("CreateKey succeeded although its directory could not be synced")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a key that was never made durable survived: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateKey(path); err != nil {
		t.Fatalf("CreateKey after the failed one: %v", err)
	}
}

func TestLoadKeyRefusesAFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(keyDir(t), "city.key")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := LoadKey(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO key was loaded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("LoadKey blocked on a FIFO")
	}
}
