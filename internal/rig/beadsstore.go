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
// store records one, or when err is set; err is the adopt guard's refusal of a
// store it could not read or whose prefix it will not adopt.
type adoptedStorePrefix struct {
	prefix string
	err    error
}

// readAdoptedStorePrefix learns the issue prefix of the store an --adopt
// registers. Legacy YAML is accepted alone; native stores verify any YAML
// mirror against the database namespace. Otherwise, when the
// store has a metadata.json and the caller can read stores, the store's own
// config table is asked: bd keeps a store's prefix there and leaves
// config.yaml a commented template. A missing metadata.json is left for the
// adopt guard to report.
func readAdoptedStorePrefix(deps Deps, rigPath string, readOnly bool) adoptedStorePrefix {
	meta := filepath.Join(rigPath, ".beads", "metadata.json")
	if _, err := deps.FS.Stat(meta); err != nil {
		return adoptedStorePrefix{}
	}
	configPath := filepath.Join(rigPath, ".beads", "config.yaml")
	prefix, ok, err := contract.ReadIssuePrefix(deps.FS, configPath)
	if err != nil && !os.IsNotExist(err) {
		return adoptedStorePrefix{err: fmt.Errorf("--adopt: reading issue_prefix: %w", err)}
	}
	yaml := adoptablePrefix(rigPath, strings.TrimSpace(prefix), true)
	if yaml.err != nil {
		return yaml
	}
	_, native, err := contract.ReadDoltMode(deps.FS, meta)
	if err != nil {
		return adoptedStorePrefix{err: err}
	}
	if ok && yaml.prefix != "" && (!native || deps.ReadStorePrefix == nil) {
		return yaml
	}
	if deps.ReadStorePrefix == nil {
		return adoptedStorePrefix{}
	}
	prefix, err = deps.ReadStorePrefix(rigPath, readOnly)
	if err != nil {
		return adoptedStorePrefix{err: fmt.Errorf("--adopt requires a valid issue_prefix in .beads/config.yaml in %s: reading the store's own issue_prefix: %w", rigPath, err)}
	}
	stored := adoptablePrefix(rigPath, strings.TrimSpace(prefix), false)
	if stored.err != nil {
		return stored
	}
	if ok && yaml.prefix != "" && yaml.prefix != stored.prefix {
		return adoptedStorePrefix{err: fmt.Errorf("--adopt: YAML issue_prefix %q conflicts with stored Beads namespace %q; reconcile the existing queue configuration", yaml.prefix, stored.prefix)}
	}
	return stored
}

// adoptablePrefix accepts a recorded prefix that is already lowercase. A
// mixed-case one is refused rather than lowercased, wherever it is recorded:
// gc stores rig prefixes lowercase, and minting under a different case would
// fork the store's bead ID series. The remedy renames the store's prefix; bd
// rename-prefix leaves .beads/config.yaml alone, and bd create prefers a
// prefix recorded there, so one recorded in config.yaml must change there too.
func adoptablePrefix(rigPath, prefix string, inConfigYAML bool) adoptedStorePrefix {
	lower := strings.ToLower(prefix)
	if prefix == lower {
		return adoptedStorePrefix{prefix: prefix}
	}
	where, alsoConfigYAML := "", ""
	if inConfigYAML {
		where = " in .beads/config.yaml"
		alsoConfigYAML = " and change the prefix in .beads/config.yaml to " + lower
	}
	return adoptedStorePrefix{err: fmt.Errorf("--adopt: the store at %s records issue prefix %q%s; gc stores rig prefixes in lowercase, so adopting it would fork its bead ID series; run %q in %s first%s",
		rigPath, prefix, where, "bd rename-prefix "+lower, rigPath, alsoConfigYAML)}
}
