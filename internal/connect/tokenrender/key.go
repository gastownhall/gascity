package tokenrender

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/connect"
)

// Key file rules (DES-CN D7).
const (
	keyFileMode os.FileMode = 0o600
	keyDirMode  os.FileMode = 0o700
	// newKeyLen is the size of a key CreateKey generates.
	newKeyLen = 32
	// maxKeyFileBytes bounds the read of a key file; a valid key is far
	// shorter.
	maxKeyFileBytes = 4096
)

// LoadKey reads the city key K from a file: base64url text (padding
// optional, surrounding whitespace ignored) decoding to at least
// connect.MinKeyLen bytes, in a file no group or other user can access,
// inside a directory no group or other user can access. The file's type and
// mode are checked on the opened descriptor, so a swap after the check
// cannot pass, and a FIFO never blocks the caller. A missing file returns an
// error wrapping os.ErrNotExist. Errors never echo the key.
func LoadKey(path string) ([]byte, error) {
	data, info, err := readRegular(path, maxKeyFileBytes, true)
	if err != nil {
		return nil, fmt.Errorf("loading connect key %s: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("loading connect key %s: mode %04o is accessible to group or others; strip its group and other permission bits", path, perm)
	}
	dir := filepath.Dir(path)
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("loading connect key %s: %w", path, err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("loading connect key %s: directory %s mode %04o is accessible to group or others; strip its group and other permission bits", path, dir, perm)
	}
	if len(data) > maxKeyFileBytes {
		return nil, fmt.Errorf("loading connect key %s: file exceeds %d bytes", path, maxKeyFileBytes)
	}
	text := bytes.TrimRight(bytes.TrimSpace(data), "=")
	key := make([]byte, base64.RawURLEncoding.DecodedLen(len(text)))
	n, err := base64.RawURLEncoding.Strict().Decode(key, text)
	if err != nil {
		return nil, fmt.Errorf("loading connect key %s: not base64url text", path)
	}
	key = key[:n]
	if len(key) < connect.MinKeyLen {
		return nil, fmt.Errorf("loading connect key %s: key is %d bytes, below %d", path, len(key), connect.MinKeyLen)
	}
	return key, nil
}

// CreateKey generates a new 32-byte city key and writes it to path as
// base64url text and a newline: a 0600 file created exclusively (an existing
// file refuses with an error wrapping os.ErrExist), under 0700 directories.
// A parent directory that already exists must not be accessible to group or
// others. A key that cannot be written or made durable is removed, so a retry
// creates a fresh one. Only the controller creates keys, and only when no
// token file it renders holds a gc line (DES-CN D7, OutputsHoldGCLines).
func CreateKey(path string) ([]byte, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, keyDirMode); err != nil {
		return nil, fmt.Errorf("creating connect key %s: %w", path, err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("creating connect key %s: %w", path, err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("creating connect key %s: directory %s mode %04o is accessible to group or others; strip its group and other permission bits", path, dir, perm)
	}
	key := make([]byte, newKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("creating connect key %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyFileMode)
	if err != nil {
		return nil, fmt.Errorf("creating connect key %s: %w", path, err)
	}
	if err := writeAndSync(f, []byte(base64.RawURLEncoding.EncodeToString(key)+"\n"), keyFileMode); err != nil {
		os.Remove(path) //nolint:errcheck // we created it; a partial key must not survive
		return nil, fmt.Errorf("creating connect key %s: %w", path, err)
	}
	if err := syncDir(dir); err != nil {
		if rmErr := os.Remove(path); rmErr != nil {
			return nil, fmt.Errorf("creating connect key %s: syncing directory: %w; removing the key: %w", path, err, rmErr)
		}
		return nil, fmt.Errorf("creating connect key %s: syncing directory: %w", path, err)
	}
	return key, nil
}
