package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// PreStartScriptsCheck verifies that script paths referenced via
// {{.ConfigDir}} in any agent's pre_start command exist on disk.
// Missing scripts cause "exit status 127" at runtime when the
// reconciler tries to start the agent. Only checks resolvable static
// references — commands without {{.ConfigDir}}, or whose first token
// still contains other unresolved templates after substitution, are
// skipped because they require runtime context to evaluate.
type PreStartScriptsCheck struct {
	cfg *config.City
}

// NewPreStartScriptsCheck creates a check that validates pre_start
// script references for every pack-shipped agent in cfg.
func NewPreStartScriptsCheck(cfg *config.City) *PreStartScriptsCheck {
	return &PreStartScriptsCheck{cfg: cfg}
}

// Name returns the check identifier.
func (c *PreStartScriptsCheck) Name() string { return "pre-start-scripts" }

// CanFix returns false — missing scripts must be authored by the user
// or shipped with the pack.
func (c *PreStartScriptsCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *PreStartScriptsCheck) Fix(_ *CheckContext) error { return nil }

// Run iterates each pack agent's pre_start commands and warns when a
// {{.ConfigDir}}-relative script is missing on disk.
func (c *PreStartScriptsCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = StatusOK
		r.Message = "no city config loaded"
		return r
	}
	var issues []string
	for _, a := range c.cfg.Agents {
		// Inline (city.toml) agents have no SourceDir to resolve
		// {{.ConfigDir}} against — skip them.
		if a.SourceDir == "" {
			continue
		}
		for _, cmd := range a.PreStart {
			scriptPath, ok := resolvePreStartScript(cmd, a.SourceDir)
			if !ok {
				continue
			}
			if _, err := os.Stat(scriptPath); err == nil {
				continue
			} else if !os.IsNotExist(err) {
				continue
			}
			rel, relErr := filepath.Rel(a.SourceDir, scriptPath)
			if relErr != nil {
				rel = scriptPath
			}
			issues = append(issues, fmt.Sprintf("agent %q: pre_start script %q not found", a.QualifiedName(), rel))
		}
	}
	if len(issues) == 0 {
		r.Status = StatusOK
		r.Message = "all pre_start scripts referenced via {{.ConfigDir}} exist"
		return r
	}
	sort.Strings(issues)
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d pre_start script(s) missing on disk", len(issues))
	r.FixHint = "ship the missing script with the pack, or remove the pre_start reference"
	r.Details = issues
	return r
}

// resolvePreStartScript extracts the absolute script path from a
// pre_start command if it references {{.ConfigDir}} cleanly. Returns
// (path, true) when the first whitespace-separated token resolves to
// an absolute path with no remaining template placeholders. Otherwise
// returns ("", false) so the caller can skip the command — either it
// is not a {{.ConfigDir}} reference, or it depends on runtime context
// (rig, work_dir, agent identity) that doctor cannot statically
// resolve.
func resolvePreStartScript(cmd, sourceDir string) (string, bool) {
	if !strings.Contains(cmd, "{{.ConfigDir}}") {
		return "", false
	}
	expanded := strings.ReplaceAll(cmd, "{{.ConfigDir}}", sourceDir)
	fields := strings.Fields(expanded)
	if len(fields) == 0 {
		return "", false
	}
	first := fields[0]
	if strings.Contains(first, "{{") {
		return "", false
	}
	if !filepath.IsAbs(first) {
		return "", false
	}
	return filepath.Clean(first), true
}
