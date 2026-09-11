package doctor

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// ProxiedBackupCoverageCheck states, once per city, that its bd-owned proxied
// scopes have no backup at all.
//
// The per-scope checks cannot say it. `rig:<name>:dolt-backup` returns OK
// because neither of its two signals can ever exist on a proxy root, and
// bd-backup-freshness skips a scope with no backup_state.json and delegates
// the "no backup at all" signal to the dolt-backup check by name. Between them
// a default-topology city reads as fully covered while nothing — not gc, not
// bd, not the backup dog — can produce a recovery point: bd v1.3.0-rc.2
// refuses `backup` on the proxied path, and mol-dog-backup talks to the
// managed server, which a proxied scope does not have.
//
// It is an advisory, not a warning: a proxied city is a healthy city on this
// branch and there is no action the operator can take on rc.2, so a warning
// would be a permanent red line nobody can clear. The honest thing is one
// line that names the exposure and what will close it.
type ProxiedBackupCoverageCheck struct {
	cityPath    string
	scopeLabels []string
}

// NewProxiedBackupCoverageCheckForConfig returns the advisory for a city with
// at least one bd-owned proxied scope, and nil for a city with none — there is
// no gap to report, and doctor should not grow a line saying so.
func NewProxiedBackupCoverageCheckForConfig(cityPath string, cfg *config.City, cfgErr error) *ProxiedBackupCoverageCheck {
	var labels []string
	for _, scopeRoot := range managedDoltScopeRootsForConfig(cityPath, cfg, cfgErr) {
		if !scopeBindingIsProviderOwnedProxied(scopeRoot) {
			continue
		}
		labels = append(labels, proxiedScopeLabel(cityPath, scopeRoot))
	}
	if len(labels) == 0 {
		return nil
	}
	return &ProxiedBackupCoverageCheck{cityPath: cityPath, scopeLabels: labels}
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

// Run reports the advisory. It has no failing outcome: the exposure is a
// property of the rc.2 topology, not of this city's configuration.
func (c *ProxiedBackupCoverageCheck) Run(_ *CheckContext) *CheckResult {
	scopeNoun := "scopes"
	if len(c.scopeLabels) == 1 {
		scopeNoun = "scope"
	}
	return &CheckResult{
		Name:     c.Name(),
		Status:   StatusOK,
		Severity: SeverityAdvisory,
		Message: fmt.Sprintf(
			"advisory: %d bd-owned proxied %s (%s) have no backup — bd v1.3.0-rc.2 refuses backup on the proxied path and gc registers none; the store is the only copy",
			len(c.scopeLabels), scopeNoun, strings.Join(c.scopeLabels, ", ")),
		Details: []string{
			"gc cannot register a Dolt backup against a proxy root it does not own.",
			"mol-dog-backup targets the managed server, which a proxied scope has none of.",
			"Copy <scope>/.beads/dolt out of band until beads lifts the proxied refusal.",
		},
	}
}

// CanFix returns false: there is nothing to fix on rc.2.
func (c *ProxiedBackupCoverageCheck) CanFix() bool { return false }

// Fix is a no-op. See CanFix.
func (c *ProxiedBackupCoverageCheck) Fix(_ *CheckContext) error { return nil }
