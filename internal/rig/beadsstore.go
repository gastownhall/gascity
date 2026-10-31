package rig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// ReadBeadsPrefix reads the issue_prefix from an existing .beads/config.yaml
// in the given rig directory. Returns the prefix and true if found, or empty
// string and false if the file doesn't exist or has no prefix. Checks both
// the underscore form (issue_prefix) and dash form (issue-prefix) since the
// lifecycle code writes both.
func ReadBeadsPrefix(fs fsys.FS, rigPath string) (string, bool) {
	prefix, ok, err := contract.ReadIssuePrefix(fs, filepath.Join(rigPath, ".beads", "config.yaml"))
	if err != nil || !ok {
		return "", false
	}
	return strings.ToLower(prefix), true
}

// beadsDirContainsStore reports whether beadsPath contains evidence that it
// would be dangerous to initialize over. Either canonical marker is enough to
// stop fresh initialization because partial stores should fail closed; only
// missing marker files are ignored.
func beadsDirContainsStore(fs fsys.FS, beadsPath string) (bool, error) {
	for _, name := range [...]string{"metadata.json", "config.yaml"} {
		path := filepath.Join(beadsPath, name)
		if _, err := fs.Stat(path); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("checking %s: %w", path, err)
		}
	}
	return false, nil
}

// adoptedStorePrefix is the issue prefix an --adopt'ed store records, read
// once during planning. prefix is "" when neither .beads/config.yaml nor the
// store records one; err holds a failed or refused store read.
type adoptedStorePrefix struct {
	prefix string
	err    error
}

// readAdoptedStorePrefix learns the issue prefix of the store an --adopt
// registers. .beads/config.yaml wins when it records one. Otherwise, when the
// store has a metadata.json and the caller can read stores, the store's own
// config table is asked: bd keeps a store's prefix there and leaves
// config.yaml a commented template. A missing metadata.json is left for the
// adopt guard to report.
//
// A mixed-case prefix read from the store is refused rather than lowercased:
// gc stores rig prefixes lowercase, and minting under a different case would
// fork the store's bead ID series.
func readAdoptedStorePrefix(deps Deps, rigPath string) adoptedStorePrefix {
	if prefix, ok := ReadBeadsPrefix(deps.FS, rigPath); ok {
		return adoptedStorePrefix{prefix: prefix}
	}
	if deps.ReadStorePrefix == nil {
		return adoptedStorePrefix{}
	}
	if _, err := deps.FS.Stat(filepath.Join(rigPath, ".beads", "metadata.json")); err != nil {
		return adoptedStorePrefix{}
	}
	prefix, err := deps.ReadStorePrefix(rigPath)
	if err != nil {
		return adoptedStorePrefix{err: err}
	}
	prefix = strings.TrimSpace(prefix)
	if prefix != strings.ToLower(prefix) {
		return adoptedStorePrefix{err: fmt.Errorf("the store at %s records issue prefix %q; gc stores rig prefixes in lowercase, so adopting it would fork its bead ID series", rigPath, prefix)}
	}
	return adoptedStorePrefix{prefix: prefix}
}
