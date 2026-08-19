package validation

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/workdir"
)

// SharedPoolWorkDir describes a multi-session agent whose distinct pool
// instances still resolve to one shared working directory. Parallel
// dispatch into that directory can clobber uncommitted git state.
type SharedPoolWorkDir struct {
	AgentQualified string
	WorkDir        string
	InstanceA      string
	InstanceB      string
}

// ValidatePoolWorkDirIsolation reports multi-session agents that would place
// two synthetic instances in the same working directory. City-scoped agents
// without a work_dir template are included when they support multiple
// sessions; operators must set a per-instance work_dir (or rely on implicit
// injection for provider pools).
func ValidatePoolWorkDirIsolation(cityPath string, cfg *config.City) []SharedPoolWorkDir {
	if cfg == nil {
		return nil
	}
	cityName := workdir.CityName(cityPath, cfg)
	var out []SharedPoolWorkDir
	for _, a := range cfg.Agents {
		if a.Name == config.ControlDispatcherAgentName {
			continue
		}
		if strings.TrimSpace(a.Dir) == "" {
			// City-scoped agents are outside the #1181 rig-clobber surface.
			continue
		}
		if !a.SupportsMultipleSessions() {
			continue
		}
		instA := a.QualifiedInstanceName(a.Name + "-1")
		instB := a.QualifiedInstanceName(a.Name + "-2")
		qnA := workdir.SessionQualifiedName(cityPath, a, cfg.Rigs, instA, instA)
		qnB := workdir.SessionQualifiedName(cityPath, a, cfg.Rigs, instB, instB)
		pathA := workdir.ResolveWorkDirPath(cityPath, cityName, qnA, a, cfg.Rigs)
		pathB := workdir.ResolveWorkDirPath(cityPath, cityName, qnB, a, cfg.Rigs)
		if pathA == "" || pathA != pathB {
			continue
		}
		out = append(out, SharedPoolWorkDir{
			AgentQualified: a.QualifiedName(),
			WorkDir:        pathA,
			InstanceA:      qnA,
			InstanceB:      qnB,
		})
	}
	return out
}

// FormatSharedPoolWorkDir formats one collision for operator-facing output.
func FormatSharedPoolWorkDir(c SharedPoolWorkDir) string {
	return fmt.Sprintf("%s: instances %q and %q share work dir %q",
		c.AgentQualified, c.InstanceA, c.InstanceB, filepath.Clean(c.WorkDir))
}
