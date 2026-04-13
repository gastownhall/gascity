package sessionprovider

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

const RemoteWorkerProfile = "remote-worker/v1"

func CanonicalName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "tmux"
	}
	return name
}

func DesiredForAgent(a *config.Agent, cityPath, defaultProvider string) (string, string, error) {
	if a == nil {
		return CanonicalName(defaultProvider), "", nil
	}
	switch {
	case a.Session == "":
		return CanonicalName(defaultProvider), "", nil
	case a.Session == "acp":
		return "acp", "", nil
	case strings.HasPrefix(a.Session, "exec:"):
		script, err := ResolveAgentExecScript(*a, cityPath)
		if err != nil {
			return "", "", err
		}
		return "exec:" + script, RemoteWorkerProfile, nil
	default:
		return "", "", fmt.Errorf("session %q is not supported (use acp, exec:<path> remote-worker/v1 script, or omit)", a.Session)
	}
}

func MetadataForAgent(a *config.Agent, cityPath, defaultProvider string) (map[string]string, error) {
	provider, profile, err := DesiredForAgent(a, cityPath, defaultProvider)
	if err != nil {
		return nil, err
	}
	return Metadata(provider, profile), nil
}

func Metadata(provider, profile string) map[string]string {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil
	}
	meta := map[string]string{sessionpkg.MetadataSessionProvider: provider}
	if profile = strings.TrimSpace(profile); profile != "" {
		meta[sessionpkg.MetadataSessionProviderProfile] = profile
	}
	return meta
}

func MergeMetadata(dst map[string]string, src map[string]string) map[string]string {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]string, len(src))
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func ResolveAgentExecScript(a config.Agent, cityPath string) (string, error) {
	raw := strings.TrimSpace(strings.TrimPrefix(a.Session, "exec:"))
	if raw == "" {
		return "", fmt.Errorf("session exec path is required")
	}
	var candidate string
	var relativeRoot string
	if filepath.IsAbs(raw) {
		candidate = raw
	} else {
		relativeRoot = cityPath
		if strings.TrimSpace(a.SourceDir) != "" {
			relativeRoot = a.SourceDir
		}
		if relativeRoot == "" {
			return "", fmt.Errorf("relative session exec path %q requires a city root", raw)
		}
		rootAbs, err := filepath.Abs(relativeRoot)
		if err != nil {
			return "", fmt.Errorf("resolving session exec root %q: %w", relativeRoot, err)
		}
		candidate = filepath.Join(rootAbs, raw)
		if err := ensurePathWithinRoot(candidate, rootAbs); err != nil {
			return "", fmt.Errorf("session exec path %q escapes %q", raw, rootAbs)
		}
		relativeRoot = rootAbs
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolving session exec path %q: %w", raw, err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("session exec path %q: %w", raw, err)
	}
	if relativeRoot != "" {
		rootCanonical, err := filepath.EvalSymlinks(relativeRoot)
		if err != nil {
			return "", fmt.Errorf("session exec root %q: %w", relativeRoot, err)
		}
		if err := ensurePathWithinRoot(canonical, rootCanonical); err != nil {
			return "", fmt.Errorf("session exec path %q resolves outside %q", raw, rootCanonical)
		}
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("session exec path %q: %w", raw, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("session exec path %q is a directory", raw)
	}
	if info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("session exec path %q is not executable", raw)
	}
	return canonical, nil
}

func ensurePathWithinRoot(path, root string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("outside root")
	}
	return nil
}
