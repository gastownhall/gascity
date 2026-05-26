package packman

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	// LockfileName is the on-disk filename used for pinned pack resolutions.
	LockfileName = "packs.lock"
	// LockfileSchema is the current packs.lock schema version.
	LockfileSchema = 1
)

// Lockfile records the exact resolved remote-pack closure for a city.
type Lockfile struct {
	Schema int                   `toml:"schema"`
	Packs  map[string]LockedPack `toml:"packs"`
}

// LockedPack is a single source-pinned resolution.
type LockedPack struct {
	Version string    `toml:"version"`
	Commit  string    `toml:"commit"`
	Fetched time.Time `toml:"fetched"`
}

// ReadLockfile loads packs.lock from cityRoot. Missing files return an empty lock.
func ReadLockfile(fs fsys.FS, cityRoot string) (*Lockfile, error) {
	path := filepath.Join(cityRoot, LockfileName)
	data, err := fs.ReadFile(path)
	if err != nil {
		return emptyLockfileIfMissing(err)
	}

	var lock Lockfile
	if _, err := toml.Decode(string(data), &lock); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", LockfileName, err)
	}
	if lock.Schema == 0 {
		lock.Schema = LockfileSchema
	}
	if lock.Packs == nil {
		lock.Packs = make(map[string]LockedPack)
	}
	return &lock, nil
}

func emptyLockfileIfMissing(err error) (*Lockfile, error) {
	if os.IsNotExist(err) {
		return &Lockfile{
			Schema: LockfileSchema,
			Packs:  make(map[string]LockedPack),
		}, nil
	}
	return nil, fmt.Errorf("reading %s: %w", LockfileName, err)
}

// WriteLockfile writes packs.lock atomically with deterministic pack ordering.
func WriteLockfile(fs fsys.FS, cityRoot string, lock *Lockfile) error {
	if lock == nil {
		lock = &Lockfile{}
	}
	if lock.Schema == 0 {
		lock.Schema = LockfileSchema
	}
	if lock.Packs == nil {
		lock.Packs = make(map[string]LockedPack)
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "schema = %d\n", lock.Schema)

	keys := make([]string, 0, len(lock.Packs))
	for key := range lock.Packs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		pack := lock.Packs[key]
		fmt.Fprintf(&buf, "\n[packs.%q]\n", key)
		fmt.Fprintf(&buf, "version = %q\n", pack.Version)
		fmt.Fprintf(&buf, "commit = %q\n", pack.Commit)
		fmt.Fprintf(&buf, "fetched = %q\n", pack.Fetched.UTC().Format(time.RFC3339))
	}

	path := filepath.Join(cityRoot, LockfileName)
	if err := fsys.WriteFileAtomic(fs, path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", LockfileName, err)
	}
	return nil
}
