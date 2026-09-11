package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// ServiceSecretsPermsCheck flags group/other-readable entries inside service
// secrets directories. Core scaffolds `<state_root>/secrets` at 0700 for
// every service (workspacesvc.ensureStateRoot) but only pack discipline
// keeps the files inside at 0600 — a hand-copied token file or a sloppy
// writer can land 0644 and quietly widen a credential to every same-group
// process. The check is mechanical: regular files must have no group/other
// bits, directories likewise (a 0755 subdirectory undermines the 0700
// root). Each secrets root is fully resolved (following any symlinked
// ancestor or `secrets` dir and collapsing `..` segments) and then confined
// to the city before it is walked, matching how core chmods through a
// symlinked dir when it tightens it. Because `gc doctor --fix` recursively
// chmods every entry it walks, a root that resolves outside the city —
// through an absolute or `..` state_root, a symlinked ancestor, or a
// symlinked `secrets` dir — or one that cannot be resolved is reported and
// skipped, never walked or chmodded, so `--fix` can never rewrite file modes
// outside the city boundary. Symlinked entries *inside* the tree are skipped
// so the check never chmods through a link to a file it does not own.
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

// serviceSecretsChmodHint is the remediation shown when loose entries exist.
const serviceSecretsChmodHint = "run `gc doctor --fix` (chmods files to 0600, directories to 0700)"

// secretsTarget is one confined service secrets directory to audit. walk is
// the path the filesystem walk descends and the only path Fix chmods; it is
// always inside the city boundary. configured is the operator-facing path
// anchored to the service's configured state root, used when reporting
// findings so they map back to service config even when the secrets root
// resolved to a different real path.
type secretsTarget struct {
	configured string
	walk       string
}

// report re-anchors a path walked under this target back onto the configured
// secrets path, so a finding names the operator-configured location rather
// than a resolved symlink target. When the resolved walk root already equals
// the configured path nothing needs re-anchoring and the path is returned
// unchanged.
func (t secretsTarget) report(path string) string {
	if t.walk == t.configured {
		return path
	}
	if rel, err := filepath.Rel(t.walk, path); err == nil {
		return filepath.Join(t.configured, rel)
	}
	return path
}

// secretsDirs returns the confined secrets directories to audit for all
// configured services, plus advisory notes for roots that were skipped because
// they could not be audited safely.
//
// Every secrets root is fully resolved with filepath.EvalSymlinks so the walk
// descends the real tree — matching how core's ensureStateRoot chmods through a
// symlinked dir — and so the confinement check sees the true target no matter
// how the configured path reaches it. Because Fix recursively chmods every
// entry it walks, a resolved root is audited only when it stays inside the
// city: a root that escapes — via an absolute or `..` state_root, a symlinked
// ancestor, or a symlinked `secrets` dir — or one whose path cannot be resolved
// is recorded in skipped and never walked or chmodded. state_root is not
// validated at config load (only by the sibling config-valid check), so
// confining the resolved walk root here is what keeps a stray symlink or a
// mis-set state_root from turning `--fix` into a recursive chmod of an
// arbitrary out-of-city tree. A secrets dir a service has not created yet is
// simply absent and skipped silently. Services may share a state root, so each
// resolved secrets directory is audited once.
func (c *ServiceSecretsPermsCheck) secretsDirs() (targets []secretsTarget, skipped []string) {
	if c.cfg == nil {
		return nil, nil
	}
	seenConfigured := map[string]bool{}
	seenWalk := map[string]bool{}
	for _, svc := range c.cfg.Services {
		root := svc.StateRootOrDefault()
		if root == "" {
			continue
		}
		if !filepath.IsAbs(root) {
			root = filepath.Join(c.cityPath, root)
		}
		configured := filepath.Join(root, "secrets")
		// Services may share a state root (one pack, many services); audit
		// each configured secrets directory once. Deduping here also keeps a
		// shared root from emitting duplicate skip notes.
		if seenConfigured[configured] {
			continue
		}
		seenConfigured[configured] = true

		// A secrets dir a service has not created yet is not a finding; skip it
		// silently. Lstat (not Stat) is used so a `secrets` symlink — even a
		// dangling one — counts as present and is resolved below rather than
		// read as absent. An unreadable ancestor also lands here and is skipped
		// silently, exactly as before.
		if _, err := os.Lstat(configured); err != nil {
			continue
		}

		// Resolve the full walk root for every target, not only when the final
		// `secrets` component is a symlink. An absolute or `..`-traversing
		// state_root, or an in-city path whose ancestor is a symlink, can place
		// the real walk root outside the city even though the configured path
		// looks in-city — and Fix recursively chmods everything it walks.
		// EvalSymlinks resolves every symlink and `..` segment against the real
		// filesystem so the confinement check below sees the true target.
		walk, err := filepath.EvalSymlinks(configured)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: secrets path could not be resolved (%v); skipped (not audited or repaired)", configured, err))
			continue
		}
		if !c.withinCity(walk) {
			skipped = append(skipped, fmt.Sprintf("%s: secrets path resolves outside the city to %s; skipped (not audited or repaired)", configured, walk))
			continue
		}
		// Two distinct configured paths can resolve to the same physical
		// directory; audit the resolved target once.
		if seenWalk[walk] {
			continue
		}
		seenWalk[walk] = true
		if st, err := os.Stat(walk); err != nil || !st.IsDir() {
			continue
		}
		targets = append(targets, secretsTarget{configured: configured, walk: walk})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].configured < targets[j].configured })
	sort.Strings(skipped)
	return targets, skipped
}

// withinCity reports whether an already-resolved path stays inside the city
// boundary. Both sides are made absolute and symlink-resolved before the
// comparison so a city root that itself contains symlinked components (a macOS
// /var -> /private/var temp dir, for example) does not falsely read as an
// escape. A resolved secrets target outside the city must never be walked or
// chmodded by Fix.
func (c *ServiceSecretsPermsCheck) withinCity(resolved string) bool {
	city := c.cityPath
	if abs, err := filepath.Abs(city); err == nil {
		city = abs
	}
	if r, err := filepath.EvalSymlinks(city); err == nil {
		city = r
	}
	city = filepath.Clean(city)
	resolved = filepath.Clean(resolved)
	if resolved == city {
		return true
	}
	rel, err := filepath.Rel(city, resolved)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// looseEntries walks one confined secrets directory and returns descriptions
// of entries with group/other permission bits set, each anchored to the
// target's configured path. The top directory itself is included: core
// re-chmods it to 0700 on service start, but doctor may run against a stopped
// city. Entries that vanish mid-walk are tolerated: services write secrets
// with atomic temp-file→rename, so a self-healing race must not turn an
// advisory hygiene check into a failure.
func looseEntries(t secretsTarget) ([]string, error) {
	var loose []string
	err := filepath.WalkDir(t.walk, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil
		}
		if info.Mode().Perm()&0o077 != 0 {
			loose = append(loose, fmt.Sprintf("%s (mode %o)", t.report(path), info.Mode().Perm()))
		}
		return nil
	})
	return loose, err
}

// Run reports any group/other-accessible files or directories under the
// configured services' secrets directories, plus any symlinked secrets roots
// skipped because they escape the city or cannot be resolved. The result is
// advisory: a finding is a hygiene warning, and even an infrastructure error
// while auditing must not gate `gc doctor`, dispatch, or automation. Audit
// errors are accumulated across directories — a single unreadable tree must
// not hide loose entries in the others — mirroring Fix's aggregation.
func (c *ServiceSecretsPermsCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Severity: SeverityAdvisory}
	targets, skipped := c.secretsDirs()
	var loose []string
	var auditErrs []error
	for _, t := range targets {
		entries, err := looseEntries(t)
		if err != nil {
			auditErrs = append(auditErrs, fmt.Errorf("auditing %s: %w", t.configured, err))
			continue
		}
		loose = append(loose, entries...)
	}
	sort.Strings(loose)
	r.Details = append(append([]string(nil), loose...), skipped...)

	if len(auditErrs) > 0 {
		r.Status = StatusError
		r.Message = fmt.Sprintf("auditing service secrets directories: %v", errors.Join(auditErrs...))
		return r
	}
	if len(loose) == 0 && len(skipped) == 0 {
		r.Status = StatusOK
		r.Message = "service secrets directories have tight permissions"
		return r
	}
	r.Status = StatusWarning
	switch {
	case len(loose) > 0 && len(skipped) > 0:
		r.Message = fmt.Sprintf("%d group/other-accessible %s under service secrets directories; %d secrets %s skipped (resolves outside the city or unresolved)",
			len(loose), pluralEntry(len(loose)), len(skipped), pluralRoot(len(skipped)))
		r.FixHint = serviceSecretsChmodHint
	case len(loose) > 0:
		r.Message = fmt.Sprintf("%d group/other-accessible %s under service secrets directories", len(loose), pluralEntry(len(loose)))
		r.FixHint = serviceSecretsChmodHint
	default:
		r.Message = fmt.Sprintf("%d secrets %s skipped: resolves outside the city or unresolved", len(skipped), pluralRoot(len(skipped)))
		r.FixHint = "keep each service `state_root`/`secrets` path inside the city (check for an out-of-city or `..` state_root, or a `secrets` symlink pointing outside)"
	}
	return r
}

// Fix tightens every loose entry: regular files to 0600, directories to
// 0700. It walks only the confined targets from secretsDirs, so a secrets
// root resolving outside the city is never chmodded. Repair continues across
// every secrets directory even when one fails — a single unreadable tree must
// not strand otherwise-fixable directories — and per-directory errors are
// aggregated with errors.Join. Entries that vanish mid-walk are tolerated.
func (c *ServiceSecretsPermsCheck) Fix(_ *CheckContext) error {
	var errs []error
	targets, _ := c.secretsDirs()
	for _, t := range targets {
		err := filepath.WalkDir(t.walk, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
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
			errs = append(errs, fmt.Errorf("tightening %s: %w", t.configured, err))
		}
	}
	return errors.Join(errs...)
}

// pluralRoot returns the singular or plural noun for a count of secrets roots.
func pluralRoot(n int) string {
	if n == 1 {
		return "root"
	}
	return "roots"
}
