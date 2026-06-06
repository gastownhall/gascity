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
// {{.ConfigDir}} or {{.CityRoot}} in any agent's pre_start command exist on disk.
// Missing scripts cause "exit status 127" at runtime when the
// reconciler tries to start the agent. Only checks resolvable static
// references — commands without either template, or whose first token
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
// {{.ConfigDir}}- or {{.CityRoot}}-relative script is missing on disk.
func (c *PreStartScriptsCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = StatusOK
		r.Message = "no city config loaded"
		return r
	}
	cityPath := ""
	if ctx != nil {
		cityPath = ctx.CityPath
	}
	var issues []string
	for _, a := range c.cfg.Agents {
		// Inline (city.toml) agents have no SourceDir to resolve
		// {{.ConfigDir}} against; pack-shipped scripts still require it.
		if a.SourceDir == "" {
			continue
		}
		for _, cmd := range a.PreStart {
			scriptPath, ok := resolvePreStartScript(cmd, a.SourceDir, cityPath)
			if !ok {
				continue
			}
			if _, err := os.Stat(scriptPath); err == nil {
				continue
			} else if !os.IsNotExist(err) {
				continue
			}
			rel := scriptPath
			if rr, err := filepath.Rel(a.SourceDir, scriptPath); err == nil && !strings.HasPrefix(rr, "..") {
				rel = rr
			} else if cityPath != "" {
				if rr, err := filepath.Rel(cityPath, scriptPath); err == nil && !strings.HasPrefix(rr, "..") {
					rel = rr
				}
			}
			issues = append(issues, fmt.Sprintf("agent %q: pre_start script %q not found", a.QualifiedName(), rel))
		}
	}
	if len(issues) == 0 {
		r.Status = StatusOK
		r.Message = "all pre_start scripts referenced via {{.ConfigDir}} or {{.CityRoot}} exist"
		return r
	}
	sort.Strings(issues)
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d pre_start script(s) missing on disk", len(issues))
	r.FixHint = "ship the missing script with the pack, or add it to <city>/scripts/, or remove the pre_start reference"
	r.Details = issues
	return r
}

// resolvePreStartScript extracts the absolute script path from a
// pre_start command if it references {{.ConfigDir}} or {{.CityRoot}}
// cleanly. Returns (path, true) when the first whitespace-separated
// token resolves to an absolute path with no remaining template
// placeholders. Otherwise returns ("", false) so the caller can skip
// the command because it references neither template or depends on
// runtime context that doctor cannot statically resolve.
//
// Both templates may appear in the same command; both are substituted.
// Only the first token is validated, so trailing runtime-only template
// arguments are allowed.
func resolvePreStartScript(cmd, sourceDir, cityPath string) (string, bool) {
	hasConfigDir := strings.Contains(cmd, "{{.ConfigDir}}")
	hasCityRoot := strings.Contains(cmd, "{{.CityRoot}}")
	if !hasConfigDir && !hasCityRoot {
		return "", false
	}
	expanded := cmd
	if hasConfigDir {
		expanded = strings.ReplaceAll(expanded, "{{.ConfigDir}}", sourceDir)
	}
	if hasCityRoot {
		expanded = strings.ReplaceAll(expanded, "{{.CityRoot}}", cityPath)
	}
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
