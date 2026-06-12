package doctor

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/gastownhall/gascity/internal/config"
)

// ServiceSecretsPermsCheck flags group/other-readable entries inside service
// secrets directories. Core scaffolds `<state_root>/secrets` at 0700 for
// every service (workspacesvc.ensureStateRoot) but only pack discipline
// keeps the files inside at 0600 — a hand-copied token file or a sloppy
// writer can land 0644 and quietly widen a credential to every same-group
// process. The check is mechanical: regular files must have no group/other
// bits, directories likewise (a 0755 subdirectory undermines the 0700
// root). Symlinks are skipped — target permissions are what matter, and
// symlink policy inside secrets dirs belongs to the pack loaders (which
// reject them).
type ServiceSecretsPermsCheck struct {
	cfg      *config.City
	cityPath string
}

// NewServiceSecretsPermsCheck creates a check that audits permissions under
// each configured service's secrets directory.
func NewServiceSecretsPermsCheck(cfg *config.City, cityPath string) *ServiceSecretsPermsCheck {
	return &ServiceSecretsPermsCheck{cfg: cfg, cityPath: cityPath}
}

// Name returns the check identifier.
func (c *ServiceSecretsPermsCheck) Name() string { return "service-secrets-perms" }

// CanFix reports that loose permissions can be tightened automatically.
func (c *ServiceSecretsPermsCheck) CanFix() bool { return true }

// WarmupEligible keeps this check out of the `gc start` warm-up scan.
func (c *ServiceSecretsPermsCheck) WarmupEligible() bool { return false }

// secretsDirs returns the deduplicated, existing secrets directories for
// all configured services. Services may share a state root (one pack, many
// services), so each directory is audited once.
func (c *ServiceSecretsPermsCheck) secretsDirs() []string {
	if c.cfg == nil {
		return nil
	}
	seen := map[string]bool{}
	var dirs []string
	for _, svc := range c.cfg.Services {
		root := svc.StateRootOrDefault()
		if root == "" {
			continue
		}
		if !filepath.IsAbs(root) {
			root = filepath.Join(c.cityPath, root)
		}
		dir := filepath.Join(root, "secrets")
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// looseEntries walks one secrets directory and returns entries with
// group/other permission bits set. The top directory itself is included:
// core re-chmods it to 0700 on service start, but doctor may run against a
// stopped city.
func looseEntries(dir string) ([]string, error) {
	var loose []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil
		}
		if info.Mode().Perm()&0o077 != 0 {
			loose = append(loose, fmt.Sprintf("%s (mode %o)", path, info.Mode().Perm()))
		}
		return nil
	})
	return loose, err
}

// Run reports any group/other-accessible files or directories under the
// configured services' secrets directories.
func (c *ServiceSecretsPermsCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	var loose []string
	for _, dir := range c.secretsDirs() {
		entries, err := looseEntries(dir)
		if err != nil {
			r.Status = StatusError
			r.Message = fmt.Sprintf("auditing %s: %v", dir, err)
			return r
		}
		loose = append(loose, entries...)
	}
	if len(loose) == 0 {
		r.Status = StatusOK
		r.Message = "service secrets directories have tight permissions"
		return r
	}
	r.Status = StatusWarning
	r.Severity = SeverityAdvisory
	r.Message = fmt.Sprintf("%d group/other-accessible entr%s under service secrets directories", len(loose), pluralYIES(len(loose)))
	r.Details = loose
	r.FixHint = "run `gc doctor --fix` (chmods files to 0600, directories to 0700)"
	return r
}

// Fix tightens every loose entry: regular files to 0600, directories to
// 0700.
func (c *ServiceSecretsPermsCheck) Fix(_ *CheckContext) error {
	for _, dir := range c.secretsDirs() {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0o077 == 0 {
				return nil
			}
			switch {
			case info.IsDir():
				return os.Chmod(path, 0o700)
			case info.Mode().IsRegular():
				return os.Chmod(path, 0o600)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("tightening %s: %w", dir, err)
		}
	}
	return nil
}

func pluralYIES(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
