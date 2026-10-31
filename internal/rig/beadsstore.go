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

// readAdoptPrefix resolves the existing namespace before deriving a new rig's
// prefix. Native bd stores it in the database; legacy queues may mirror it in
// YAML. Never substitute a requested prefix for an unreadable existing one.
func readAdoptPrefix(deps Deps, req ProvisionRequest, rigPath string) (string, error) {
	if _, err := deps.FS.Stat(filepath.Join(rigPath, ".beads", "metadata.json")); err != nil {
		return "", fmt.Errorf("--adopt requires .beads/metadata.json in %s", rigPath)
	}
	configPath := filepath.Join(rigPath, ".beads", "config.yaml")
	missing := fmt.Errorf("--adopt requires a valid issue_prefix from .beads/config.yaml or the existing Beads store in %s", rigPath)
	if _, err := deps.FS.Stat(configPath); err != nil {
		return "", missing
	}
	prefix, ok, err := contract.ReadIssuePrefix(deps.FS, configPath)
	if err != nil {
		return "", fmt.Errorf("%w: %w", missing, err)
	}
	_, nativeMode, modeErr := contract.ReadDoltMode(deps.FS, filepath.Join(rigPath, ".beads", "metadata.json"))
	if modeErr != nil {
		return "", fmt.Errorf("--adopt: reading workspace metadata: %w", modeErr)
	}
	yamlPrefix := strings.ToLower(strings.TrimSpace(prefix))
	if ok && yamlPrefix != "" && (!nativeMode || deps.ReadStorePrefix == nil) {
		return yamlPrefix, nil
	}
	if deps.ReadStorePrefix == nil {
		return "", missing
	}
	prefix, err = deps.ReadStorePrefix(rigPath, req.DryRun)
	if err != nil {
		return "", fmt.Errorf("%w: reading existing Beads issue_prefix: %w", missing, err)
	}
	if strings.TrimSpace(prefix) == "" {
		return "", fmt.Errorf("--adopt: existing Beads store has no issue_prefix in %s", rigPath)
	}
	storedPrefix := strings.ToLower(strings.TrimSpace(prefix))
	if ok && yamlPrefix != "" && yamlPrefix != storedPrefix {
		return "", fmt.Errorf("--adopt: YAML issue_prefix %q conflicts with stored Beads namespace %q; reconcile the existing queue configuration", yamlPrefix, storedPrefix)
	}
	return storedPrefix, nil
}
