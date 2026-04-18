// Package workdir resolves agent working directories from config templates.
package workdir

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/gastownhall/gascity/internal/config"
)

// PathContext holds template variables for work_dir expansion.
type PathContext struct {
	Agent     string
	AgentBase string
	Rig       string
	RigRoot   string
	CityRoot  string
	CityName  string
}

// CityName returns the configured workspace name, or the city directory basename
// when workspace.name is unset.
func CityName(cityPath string, cfg *config.City) string {
	if cfg != nil && cfg.Workspace.Name != "" {
		return cfg.Workspace.Name
	}
	return filepath.Base(filepath.Clean(cityPath))
}

// ResolveDirPath returns an absolute path for dir, resolving relative paths
// against the city root.
func ResolveDirPath(cityPath, dir string) string {
	if dir == "" {
		return cityPath
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(cityPath, dir)
}

// ConfiguredRigName returns the rig associated with an agent, preferring the
// legacy dir-as-rig convention and falling back to path matching.
func ConfiguredRigName(cityPath string, a config.Agent, rigs []config.Rig) string {
	if a.Dir == "" {
		return ""
	}
	for _, rig := range rigs {
		if a.Dir == rig.Name {
			return rig.Name
		}
	}
	abs := ResolveDirPath(cityPath, a.Dir)
	for _, rig := range rigs {
		if samePath(abs, rig.Path) {
			return rig.Name
		}
	}
	return ""
}

// RigRootForName returns the configured root path for rigName.
func RigRootForName(rigName string, rigs []config.Rig) string {
	for _, rig := range rigs {
		if rig.Name == rigName {
			return rig.Path
		}
	}
	return ""
}

// PathContextForQualifiedName builds template context for work_dir expansion.
func PathContextForQualifiedName(cityPath, cityName, qualifiedName string, a config.Agent, rigs []config.Rig) PathContext {
	rigName := ConfiguredRigName(cityPath, a, rigs)
	_, agentBase := config.ParseQualifiedName(qualifiedName)
	return PathContext{
		Agent:     qualifiedName,
		AgentBase: agentBase,
		Rig:       rigName,
		RigRoot:   RigRootForName(rigName, rigs),
		CityRoot:  cityPath,
		CityName:  cityName,
	}
}

// ExpandTemplateStrict expands Go text/template placeholders in a work_dir
// string and returns an error when parsing or execution fails.
func ExpandTemplateStrict(spec string, ctx PathContext) (string, error) {
	if spec == "" || !strings.Contains(spec, "{{") {
		return spec, nil
	}
	tmpl, err := template.New("workdir").Option("missingkey=error").Parse(spec)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ExpandTemplate expands Go text/template placeholders in a work_dir string.
// On parse or execute error, the raw string is returned.
func ExpandTemplate(spec string, ctx PathContext) string {
	expanded, err := ExpandTemplateStrict(spec, ctx)
	if err != nil {
		return spec
	}
	return expanded
}

// ResolveWorkDirPathStrict returns the effective session working directory and
// surfaces work_dir template errors to callers that need to fail closed.
func ResolveWorkDirPathStrict(cityPath, cityName, qualifiedName string, a config.Agent, rigs []config.Rig) (string, error) {
	if a.WorkDir == "" {
		if rigName := ConfiguredRigName(cityPath, a, rigs); rigName != "" {
			if rigRoot := RigRootForName(rigName, rigs); rigRoot != "" {
				return ResolveDirPath(cityPath, rigRoot), nil
			}
		}
		return ResolveDirPath(cityPath, a.Dir), nil
	}
	ctx := PathContextForQualifiedName(cityPath, cityName, qualifiedName, a, rigs)
	expanded, err := ExpandTemplateStrict(a.WorkDir, ctx)
	if err != nil {
		return "", fmt.Errorf("expand work_dir %q: %w", a.WorkDir, err)
	}
	return ResolveDirPath(cityPath, expanded), nil
}

// ResolveWorkDirPath returns the effective session working directory for an
// agent. When work_dir is unset, rig-scoped agents continue to use their rig
// root for backward compatibility.
func ResolveWorkDirPath(cityPath, cityName, qualifiedName string, a config.Agent, rigs []config.Rig) string {
	path, err := ResolveWorkDirPathStrict(cityPath, cityName, qualifiedName, a, rigs)
	if err != nil {
		ctx := PathContextForQualifiedName(cityPath, cityName, qualifiedName, a, rigs)
		return ResolveDirPath(cityPath, ExpandTemplate(a.WorkDir, ctx))
	}
	return path
}

func samePath(a, b string) bool {
	return normalizePathForCompare(a) == normalizePathForCompare(b)
}

func normalizePathForCompare(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	path = canonicalizeExistingPathPrefix(path)
	return filepath.Clean(path)
}

func canonicalizeExistingPathPrefix(path string) string {
	current := path
	var suffix []string
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
