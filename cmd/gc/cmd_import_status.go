package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/spf13/cobra"
)

// importStatusSchemaVersion identifies the JSON document emitted by
// "gc import status --json".
const importStatusSchemaVersion = "1"

// ImportStatusJSON is the JSON output format for "gc import status --json".
// It is the machine-readable drift surface for pack imports: every
// declared import binding across scopes (root pack, default-rig, and
// rig-scoped), the packs.lock pin closure, and the lockfile content hash.
type ImportStatusJSON struct {
	SchemaVersion string `json:"schema_version"`
	// OK is the top-level success discriminator every gc --json result
	// document must carry (see schemas/import/status/result.schema.json).
	OK bool `json:"ok"`
	// Root is the absolute city or pack root the status was computed from.
	Root string `json:"root"`
	// PacksLockPath is the absolute path of the packs.lock file (which
	// may not exist; see PacksLockSHA256).
	PacksLockPath string `json:"packs_lock_path"`
	// PacksLockSHA256 is the hex-encoded SHA-256 digest of the raw
	// packs.lock contents. Omitted when the file does not exist.
	PacksLockSHA256 string `json:"packs_lock_sha256,omitempty"`
	// Imports lists every declared import binding, sorted by name.
	Imports []ImportStatusEntry `json:"imports"`
	// LockedPacks mirrors the full packs.lock closure (direct and
	// transitive pins), sorted by source.
	LockedPacks []ImportStatusLockedPack `json:"locked_packs"`
}

// ImportStatusEntry is one declared import binding in the status output.
type ImportStatusEntry struct {
	// Name is the scoped binding key: "pack:<name>" for root-pack
	// imports, "default-rig:<name>" for default rig imports, and
	// "rig:<rig>:<name>" for rig-scoped imports.
	Name string `json:"name"`
	// Source is the declared source exactly as authored in TOML.
	Source string `json:"source"`
	// Constraint is the declared version constraint, when present.
	Constraint string `json:"constraint,omitempty"`
	// Kind is "remote" for git-backed sources and "path" for local
	// directory sources.
	Kind string `json:"kind"`
	// Path is the resolved absolute path for kind "path" entries.
	Path string `json:"path,omitempty"`
	// Pin is the packs.lock resolution for kind "remote" entries.
	// Omitted when the source has no lock entry (unlocked).
	Pin *ImportStatusPin `json:"pin,omitempty"`
}

// ImportStatusPin is the packs.lock resolution pinned for a remote import.
type ImportStatusPin struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Fetched string `json:"fetched,omitempty"`
}

// ImportStatusLockedPack is one packs.lock entry in the status output.
type ImportStatusLockedPack struct {
	Source  string `json:"source"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Fetched string `json:"fetched,omitempty"`
}

func newImportStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report declared imports and packs.lock pins",
		Long: `Report declared imports and packs.lock pins.

Covers every import scope (root pack [imports.*], [defaults.rig.imports.*],
and rig-scoped [rigs.imports.*]) plus the full packs.lock closure and the
lockfile content hash. With --json the output is a stable machine-readable
document for drift checkers.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveImportRoot()
			if err != nil {
				fmt.Fprintf(stderr, "gc import status: %v\n", err) //nolint:errcheck
				return errExit
			}
			if doImportStatus(cityPath, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON result")
	return cmd
}

// doImportStatus is the pure logic for "gc import status". It reads the
// declared import set across all scopes plus packs.lock and renders
// either the human-readable summary or the typed JSON document.
func doImportStatus(cityPath string, jsonOut bool, stdout, stderr io.Writer) int {
	status, err := buildImportStatus(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc import status: %v\n", err) //nolint:errcheck
		return 1
	}
	if jsonOut {
		data, err := json.MarshalIndent(status, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "gc import status: encoding JSON: %v\n", err) //nolint:errcheck
			return 1
		}
		fmt.Fprintln(stdout, string(data)) //nolint:errcheck
		return 0
	}
	writeImportStatusText(stdout, status)
	return 0
}

// buildImportStatus assembles the import status document for cityPath.
func buildImportStatus(cityPath string) (*ImportStatusJSON, error) {
	fs := fsys.OSFS{}
	allImports, err := collectAllImportsFS(fs, cityPath)
	if err != nil {
		return nil, err
	}

	lockPath := filepath.Join(cityPath, packman.LockfileName)
	status := &ImportStatusJSON{
		SchemaVersion: importStatusSchemaVersion,
		OK:            true,
		Root:          cityPath,
		PacksLockPath: lockPath,
	}
	// Read packs.lock once and derive both the hash and the pins from
	// the same bytes: a concurrent atomic lockfile rewrite between two
	// reads could otherwise emit a document whose packs_lock_sha256
	// does not match its own pin set.
	lockData, err := fs.ReadFile(lockPath)
	switch {
	case err == nil:
		sum := sha256.Sum256(lockData)
		status.PacksLockSHA256 = hex.EncodeToString(sum[:])
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("reading %s: %w", packman.LockfileName, err)
	}
	lock, err := packman.ParseLockfile(lockData)
	if err != nil {
		return nil, err
	}
	status.Imports = make([]ImportStatusEntry, 0, len(allImports))
	status.LockedPacks = make([]ImportStatusLockedPack, 0, len(lock.Packs))

	names := make([]string, 0, len(allImports))
	for name := range allImports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		imp := allImports[name]
		entry := ImportStatusEntry{
			Name:       name,
			Source:     imp.Source,
			Constraint: imp.Version,
		}
		if isRemoteImportSource(imp.Source) {
			entry.Kind = "remote"
			if pack, ok := lock.Packs[imp.Source]; ok {
				entry.Pin = &ImportStatusPin{
					Version: pack.Version,
					Commit:  pack.Commit,
					Fetched: formatImportStatusTime(pack.Fetched),
				}
			}
		} else {
			entry.Kind = "path"
			if abs, pathErr := resolveImportAddPath(cityPath, imp.Source); pathErr == nil {
				entry.Path = abs
			}
		}
		status.Imports = append(status.Imports, entry)
	}

	sources := make([]string, 0, len(lock.Packs))
	for source := range lock.Packs {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		pack := lock.Packs[source]
		status.LockedPacks = append(status.LockedPacks, ImportStatusLockedPack{
			Source:  source,
			Version: pack.Version,
			Commit:  pack.Commit,
			Fetched: formatImportStatusTime(pack.Fetched),
		})
	}
	return status, nil
}

// formatImportStatusTime renders a lock timestamp as RFC 3339 UTC, or
// "" for the zero value so omitempty drops it from the JSON output.
func formatImportStatusTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// writeImportStatusText renders the human-readable status: the lock
// hash followed by one tab-separated line per declared import
// (name, source, constraint, kind, pinned version, pinned commit).
func writeImportStatusText(stdout io.Writer, status *ImportStatusJSON) {
	if status.PacksLockSHA256 != "" {
		fmt.Fprintf(stdout, "packs.lock sha256: %s\n", status.PacksLockSHA256) //nolint:errcheck
	} else {
		fmt.Fprintln(stdout, "packs.lock sha256: (missing)") //nolint:errcheck
	}
	for _, entry := range status.Imports {
		pinnedVersion, pinnedCommit := "", ""
		switch {
		case entry.Pin != nil:
			pinnedVersion = entry.Pin.Version
			pinnedCommit = entry.Pin.Commit
		case entry.Kind == "remote":
			pinnedVersion = "(unlocked)"
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck
			entry.Name, entry.Source, entry.Constraint, entry.Kind, pinnedVersion, pinnedCommit)
	}
}
