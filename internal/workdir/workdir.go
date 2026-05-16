// Package workdir resolves agent working directories from config templates.
package workdir

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pathutil"
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

// CityName returns the effective workspace name for workdir/template expansion.
func CityName(cityPath string, cfg *config.City) string {
	return config.EffectiveCityName(cfg, filepath.Base(filepath.Clean(cityPath)))
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

// ExpandCommandTemplate renders command using the same PathContext surface as
// work_dir and session_setup templates. When cityName is empty, it falls back
// to the city directory basename so callers don't have to duplicate that logic.
func ExpandCommandTemplate(command, cityPath, cityName string, a config.Agent, rigs []config.Rig) (string, error) {
	if command == "" || !strings.Contains(command, "{{") {
		return command, nil
	}
	if strings.TrimSpace(cityName) == "" {
		cityName = filepath.Base(filepath.Clean(cityPath))
	}
	ctx := PathContextForQualifiedName(cityPath, cityName, a.QualifiedName(), a, rigs)
	return ExpandTemplateStrict(command, ctx)
}

// SessionQualifiedName returns the canonical work_dir identity for a concrete
// session instance. Single-session agents keep their template identity; pooled
// agents use the alias or generated explicit name.
func SessionQualifiedName(cityPath string, a config.Agent, rigs []config.Rig, alias, explicitName string) string {
	if !a.SupportsMultipleSessions() {
		return a.QualifiedName()
	}
	identity := strings.TrimSpace(alias)
	if identity == "" {
		identity = strings.TrimSpace(explicitName)
	}
	if identity == "" {
		return a.QualifiedName()
	}

	_, instanceName := config.ParseQualifiedName(identity)
	if instanceName != "" {
		identity = instanceName
	}
	if a.BindingName != "" {
		prefix := a.BindingName + "."
		identity = strings.TrimPrefix(identity, prefix)
	}

	qualified := a.QualifiedInstanceName(identity)
	rigName := ConfiguredRigName(cityPath, a, rigs)
	if rigName == "" {
		return qualified
	}
	_, agentBase := config.ParseQualifiedName(qualified)
	return rigName + "/" + agentBase
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
	return pathutil.SamePath(a, b)
}

// ValidateAncestorWorktreesNotStale walks path's ancestor chain and returns
// an error when any ancestor has a regular-file ".git" worktree pointer
// whose "gitdir:" target is unusable. The walk stops as soon as it
// encounters a ".git" marker — any regular file (with or without a usable
// gitdir pointer) or a real ".git" directory. Reaching the filesystem
// root without finding a marker is not an error.
//
// Failure modes that fail closed (the gitdir pointer is present and parses):
//   - the gitdir target doesn't exist on disk
//   - the gitdir target exists but is not a directory (non-worktree-
//     capable, e.g. a regular file at the expected admin-dir path)
//
// Failure modes that fail open (the .git file is present but not a
// recognizable worktree pointer): unreadable file, missing "gitdir:"
// prefix. In both cases the walk stops — anything further up belongs
// to the surrounding repository, and fail-closed here would block
// legitimate spawns whenever an unrelated ancestor .git file is
// permission-restricted or non-pointer.
//
// Relative gitdir targets are resolved against the directory holding
// the ".git" file (Git's gitfile format), matching Git's own
// interpretation rather than the process working directory.
//
// This is the spawn-time guard for gascity#1556: a stale worktree pointer
// on an ancestor lets "git -C <rig-root> worktree add <child>" register a
// structurally orphaned child that can't be reached from the ancestor
// itself. Failing closed on stale-pointer cases before invoking "git
// worktree add" surfaces the stale ancestor to the operator instead of
// producing dangling content.
func ValidateAncestorWorktreesNotStale(path string) error {
	// Walk from path's parent upward. The spawn target itself may not yet
	// exist (we are typically about to MkdirAll it); only ancestors are
	// inspected.
	cur := filepath.Dir(filepath.Clean(path))
	for {
		gitPath := filepath.Join(cur, ".git")
		info, err := os.Lstat(gitPath)
		if err == nil {
			if info.Mode().IsRegular() {
				data, rerr := os.ReadFile(gitPath)
				if rerr == nil {
					content := strings.TrimSpace(string(data))
					if strings.HasPrefix(content, "gitdir:") {
						target := strings.TrimSpace(strings.TrimPrefix(content, "gitdir:"))
						// Git's gitfile format: a relative gitdir target
						// is resolved against the directory containing
						// the .git file, not the process working
						// directory.
						if !filepath.IsAbs(target) {
							target = filepath.Join(cur, target)
						}
						target = filepath.Clean(target)
						targetInfo, terr := os.Stat(target)
						if terr != nil {
							return fmt.Errorf(
								"worktree spawn rejected: ancestor %q has stale .git pointer (gitdir target %q does not exist): %w",
								cur, target, terr)
						}
						if !targetInfo.IsDir() {
							return fmt.Errorf(
								"worktree spawn rejected: ancestor %q has stale .git pointer (gitdir target %q is not a directory)",
								cur, target)
						}
					}
				}
				// Either a recognizable worktree pointer with a usable
				// target, or a .git file we couldn't parse (unreadable
				// or missing the "gitdir:" prefix). Either way we stop
				// the walk — anything further up belongs to the
				// surrounding repository and is git's responsibility,
				// not ours.
				return nil
			}
			if info.IsDir() {
				// Reached a real .git directory (main repo root). Stop.
				return nil
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root without finding a marker.
			return nil
		}
		cur = parent
	}
}
