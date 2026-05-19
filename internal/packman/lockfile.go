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
	// LockfileSchemaV2 records registry-origin and integrity metadata.
	LockfileSchemaV2 = 2
)

// Lockfile records the exact resolved remote-pack closure for a city.
type Lockfile struct {
	Schema int                   `toml:"schema"`
	Packs  map[string]LockedPack `toml:"packs"`
}

// LockedPack is a single source-pinned resolution.
type LockedPack struct {
	Version         string    `toml:"version"`
	Commit          string    `toml:"commit"`
	Fetched         time.Time `toml:"fetched"`
	Source          string    `toml:"source,omitempty"`
	SourceKind      string    `toml:"source_kind,omitempty"`
	Ref             string    `toml:"ref,omitempty"`
	Hash            string    `toml:"hash,omitempty"`
	Registry        string    `toml:"registry,omitempty"`
	RegistrySource  string    `toml:"registry_source,omitempty"`
	RegistryPack    string    `toml:"registry_pack,omitempty"`
	Withdrawn       bool      `toml:"withdrawn,omitempty"`
	WithdrawnReason string    `toml:"withdrawn_reason,omitempty"`
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
	if lock.Schema > LockfileSchemaV2 {
		return nil, fmt.Errorf("unsupported %s schema %d", LockfileName, lock.Schema)
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
		lock.Schema = lockfileSchema(lock)
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
		writeOptionalString(&buf, "source", pack.Source)
		writeOptionalString(&buf, "source_kind", pack.SourceKind)
		writeOptionalString(&buf, "ref", pack.Ref)
		writeOptionalString(&buf, "hash", pack.Hash)
		writeOptionalString(&buf, "registry", pack.Registry)
		writeOptionalString(&buf, "registry_source", pack.RegistrySource)
		writeOptionalString(&buf, "registry_pack", pack.RegistryPack)
		if pack.Withdrawn {
			fmt.Fprintf(&buf, "withdrawn = true\n")
		}
		writeOptionalString(&buf, "withdrawn_reason", pack.WithdrawnReason)
	}

	path := filepath.Join(cityRoot, LockfileName)
	if err := fsys.WriteFileAtomic(fs, path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", LockfileName, err)
	}
	return nil
}

func writeOptionalString(buf *bytes.Buffer, key, value string) {
	if value != "" {
		fmt.Fprintf(buf, "%s = %q\n", key, value)
	}
}

func lockfileSchema(lock *Lockfile) int {
	if lock == nil {
		return LockfileSchema
	}
	for _, pack := range lock.Packs {
		if pack.Source != "" || pack.SourceKind != "" || pack.Ref != "" || pack.Hash != "" ||
			pack.Registry != "" || pack.RegistrySource != "" || pack.RegistryPack != "" ||
			pack.Withdrawn || pack.WithdrawnReason != "" {
			return LockfileSchemaV2
		}
	}
	return LockfileSchema
}
