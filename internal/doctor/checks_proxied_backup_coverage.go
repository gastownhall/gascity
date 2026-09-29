package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// ProxiedBackupCoverageCheck reports, once per city, whether its bd-owned
// proxied scopes have a backup.
//
// The per-scope checks cannot say it. `rig:<name>:dolt-backup` returns OK
// because neither of its two signals can ever exist on a proxy root, and
// bd-backup-freshness reads only backup state files on disk. mol-dog-backup
// backs every scope up through `bd backup`, so bd is the one place to ask:
// this check runs `bd backup status --json` in each proxied scope.
//
// Two bd generations answer differently, and the check follows the answer
// rather than a version string:
//
//   - bd v1.3.0 refuses `backup` on the proxied path (proxy.backup.unsupported).
//     Nothing — not gc, not bd, not the backup dog — can produce a recovery
//     point, so the check keeps its original advisory: StatusOK, because there
//     is no action the operator can take, and one line that names the exposure.
//   - A bd that supports proxied backup (beads hotfix/1.3.1) reports the
//     scope's destination and last sync. The check then verifies a destination
//     is configured and has synced within defaultBackupFreshnessMaxAge, and
//     warns (advisory severity) for any scope that has none or has gone stale:
//     that gap is now one the operator can close.
type ProxiedBackupCoverageCheck struct {
	cityPath string
	scopes   []proxiedBackupScope
	// bdBin names the bd executable pinned for a scope ("" means PATH).
	bdBin func(scopeRoot string) string
	// status runs `bd backup status --json` for a scope. Injected by tests.
	status func(ctx *CheckContext, bdBin, scopeRoot string) (bdBackupStatusReport, error)
	maxAge time.Duration
	now    func() time.Time
}

type proxiedBackupScope struct {
	label string
	root  string
}

// NewProxiedBackupCoverageCheckForConfig returns the check for a city with at
// least one bd-owned proxied scope whose data is here, and nil for a city with
// none — there is no gap to report, and doctor should not grow a line saying
// so. bdBin resolves the bd executable pinned for a scope; nil (or an empty
// answer) runs `bd` from PATH.
//
// A proxied-external scope (M4) is not counted. Its beads live on a server this
// host does not run, so "the store is the only copy" and "copy
// <scope>/.beads/dolt out of band" are both false: that root holds no data.
// Backups there are the endpoint's owner's, exactly as they are for a direct
// external endpoint, and the per-scope dolt-backup message says so.
func NewProxiedBackupCoverageCheckForConfig(cityPath string, cfg *config.City, cfgErr error, bdBin func(scopeRoot string) string) *ProxiedBackupCoverageCheck {
	var scopes []proxiedBackupScope
	for _, scopeRoot := range managedDoltScopeRootsForConfig(cityPath, cfg, cfgErr) {
		if !scopeBindingIsProviderOwnedProxied(scopeRoot) || scopeProxiedUpstreamIsExternal(scopeRoot) {
			continue
		}
		scopes = append(scopes, proxiedBackupScope{label: proxiedScopeLabel(cityPath, scopeRoot), root: scopeRoot})
	}
	if len(scopes) == 0 {
		return nil
	}
	return &ProxiedBackupCoverageCheck{
		cityPath: cityPath,
		scopes:   scopes,
		bdBin:    bdBin,
		status:   readBdBackupStatus,
		maxAge:   defaultBackupFreshnessMaxAge,
		now:      time.Now,
	}
}

// proxiedScopeLabel names a scope the way an operator sees it: "city" for the
// city root, the relative path for anything under it, and the absolute path
// for a rig that lives elsewhere.
func proxiedScopeLabel(cityPath, scopeRoot string) string {
	if rel, err := filepath.Rel(cityPath, scopeRoot); err == nil {
		switch {
		case rel == ".":
			return "city"
		case !strings.HasPrefix(rel, ".."):
			return rel
		}
	}
	return scopeRoot
}

// Name returns the check identifier.
func (c *ProxiedBackupCoverageCheck) Name() string { return "proxied-backup-coverage" }

// Run asks bd for each proxied scope's backup status and reports the result.
func (c *ProxiedBackupCoverageCheck) Run(ctx *CheckContext) *CheckResult {
	now := c.now()
	var refused, covered, findings, details []string
	for _, scope := range c.scopes {
		bin := ""
		if c.bdBin != nil {
			bin = c.bdBin(scope.root)
		}
		report, err := c.status(ctx, bin, scope.root)
		switch {
		case errors.Is(err, errBdProxiedBackupRefused):
			refused = append(refused, scope.label)
			continue
		case err != nil:
			findings = append(findings, fmt.Sprintf("%s: could not read bd backup status: %v", scope.label, err))
			continue
		case !report.Dolt.Configured:
			findings = append(findings, fmt.Sprintf("%s: no bd backup destination is configured, so the store is the only copy", scope.label))
			continue
		}
		details = append(details, fmt.Sprintf("%s: bd backup %s, last sync %s", scope.label, report.Dolt.BackupURL, strings.TrimSpace(report.Dolt.LastSync)))
		if finding, stale := freshnessFinding(scope.label, "bd backup", "bd backup status", "dolt.last_sync", report.Dolt.LastSync, now, c.maxAge); stale {
			findings = append(findings, finding)
			continue
		}
		covered = append(covered, scope.label)
	}

	if len(refused) == len(c.scopes) {
		return c.refusedResult(refused)
	}
	r := &CheckResult{Name: c.Name(), Details: details}
	if len(refused) > 0 {
		findings = append(findings, fmt.Sprintf("%s: no backup — %s", strings.Join(refused, ", "), proxiedBackupRefusal))
	}
	if len(findings) == 0 {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("%d bd-owned proxied %s (%s) backed up through bd within %s",
			len(covered), proxiedScopeNoun(len(covered)), strings.Join(covered, ", "), c.maxAge)
		return r
	}
	r.Status = StatusWarning
	r.Severity = SeverityAdvisory
	r.Message = strings.Join(findings, "; ")
	r.FixHint = "run `gc order run mol-dog-backup` (it registers a destination with `bd backup init` " +
		"when a scope has none, then runs `bd backup sync`), or run `bd backup init <url>` and " +
		"`bd backup sync` in the scope; `bd backup status` shows the result"
	return r
}

// refusedResult is the original advisory, for a bd that refuses backup on the
// proxied path: StatusOK because there is no action the operator can take, so
// a warning would be a permanent red line nobody can clear.
func (c *ProxiedBackupCoverageCheck) refusedResult(labels []string) *CheckResult {
	return &CheckResult{
		Name:     c.Name(),
		Status:   StatusOK,
		Severity: SeverityAdvisory,
		Message: fmt.Sprintf(
			"advisory: %d bd-owned proxied %s (%s) have no backup — %s and gc registers none; the store is the only copy",
			len(labels), proxiedScopeNoun(len(labels)), strings.Join(labels, ", "), proxiedBackupRefusal),
		Details: []string{
			"gc cannot register a Dolt backup against a proxy root it does not own.",
			"mol-dog-backup backs every scope up with `bd backup`, which this bd refuses on the proxied path.",
			"Copy <scope>/.beads/dolt out of band, or upgrade to a bd that supports proxied backup.",
		},
	}
}

func proxiedScopeNoun(n int) string {
	if n == 1 {
		return "scope"
	}
	return "scopes"
}

// bdProxiedBackupStatusTimeout bounds one `bd backup status` call.
const bdProxiedBackupStatusTimeout = 30 * time.Second

// errBdProxiedBackupRefused reports that bd refused `backup` on the proxied
// path (bd v1.3.0's proxy.backup.unsupported).
var errBdProxiedBackupRefused = errors.New("bd refuses backup on the proxied path")

// bdBackupStatusReport is the subset of `bd backup status --json` this check
// reads: the Dolt backup destination and its last successful sync.
type bdBackupStatusReport struct {
	Dolt struct {
		Configured bool   `json:"configured"`
		BackupURL  string `json:"backup_url"`
		LastSync   string `json:"last_sync"`
	} `json:"dolt"`
}

// bdProxiedBackupRefused reports whether bd's output is its proxied-backup
// refusal. The typed code is what bd v1.3.0 emits; the prose is the same
// match mol-dog-backup's backup_unsupported uses.
func bdProxiedBackupRefused(output []byte) bool {
	text := string(output)
	return strings.Contains(text, `"proxy.backup.unsupported"`) || strings.Contains(text, "not supported in proxied-server mode")
}

// readBdBackupStatus runs `bd backup status --json` for one scope in the same
// scrubbed store environment the custom-types check uses.
func readBdBackupStatus(ctx *CheckContext, bdBin, scopeRoot string) (bdBackupStatusReport, error) {
	var report bdBackupStatusReport
	args := []string{"backup", "status", "--json"}
	env, err := customTypesStoreEnv(ctx, scopeRoot)
	if err != nil {
		return report, err
	}
	bin := strings.TrimSpace(bdBin)
	if bin == "" {
		bin = "bd"
	}
	runCtx, cancel := context.WithTimeout(context.Background(), bdProxiedBackupStatusTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Dir, cmd.Env = scopeRoot, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err = cmd.Run()
	exitCode := 0
	if err != nil {
		exitCode = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	beads.TraceBDCall("go:doctor.readBdBackupStatus", scopeRoot, args, start, exitCode, err)
	if err != nil {
		if bdProxiedBackupRefused(stdout.Bytes()) || bdProxiedBackupRefused(stderr.Bytes()) {
			return report, errBdProxiedBackupRefused
		}
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return report, fmt.Errorf("%w: %s", err, detail)
		}
		return report, err
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		return report, fmt.Errorf("parsing bd backup status output: %w", err)
	}
	return report, nil
}

// CanFix returns false: registering and syncing a backup is the backup dog's
// job (or the operator's), and on a bd that refuses proxied backup there is
// nothing to fix at all.
func (c *ProxiedBackupCoverageCheck) CanFix() bool { return false }

// Fix is a no-op. See CanFix.
func (c *ProxiedBackupCoverageCheck) Fix(_ *CheckContext) error { return nil }
