package packsource

import (
	"fmt"
	"regexp"
	"strings"
)

type Kind string

const (
	KindRegistryLocator Kind = "registry-locator"
	KindQualifiedName   Kind = "qualified-name"
	KindBareName        Kind = "bare-name"
	KindGit             Kind = "git"
	KindPath            Kind = "path"
	KindUnknown         Kind = "unknown"
)

var (
	registryNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	packNameRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*(/[a-z0-9][a-z0-9-]*)?$`)
)

type Classification struct {
	Kind     Kind
	Raw      string
	Registry string
	Pack     string
}

func Classify(raw string) Classification {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return Classification{Kind: KindUnknown}
	case strings.HasPrefix(raw, "registry:"):
		loc, err := ParseRegistryLocator(raw)
		if err != nil {
			return Classification{Kind: KindUnknown, Raw: raw}
		}
		return Classification{Kind: KindRegistryLocator, Raw: raw, Registry: loc.Registry, Pack: loc.Pack}
	case isExplicitPath(raw):
		return Classification{Kind: KindPath, Raw: raw}
	case isGitLocator(raw):
		return Classification{Kind: KindGit, Raw: raw}
	}
	if registry, pack, ok := strings.Cut(raw, ":"); ok && validRegistryName(registry) && validPackName(pack) {
		return Classification{Kind: KindQualifiedName, Raw: raw, Registry: registry, Pack: pack}
	}
	if validPackName(raw) {
		return Classification{Kind: KindBareName, Raw: raw, Pack: raw}
	}
	return Classification{Kind: KindUnknown, Raw: raw}
}

type RegistryLocator struct {
	Registry string
	Pack     string
}

func ParseRegistryLocator(raw string) (RegistryLocator, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(raw), "registry:")
	if !ok {
		return RegistryLocator{}, fmt.Errorf("registry locator must start with registry:")
	}
	registry, pack, ok := strings.Cut(rest, ":")
	if !ok || registry == "" || pack == "" {
		return RegistryLocator{}, fmt.Errorf("registry locator must be registry:<registry>:<pack>")
	}
	if strings.Contains(pack, ":") {
		return RegistryLocator{}, fmt.Errorf("registry locator pack name must not contain ':'")
	}
	if !validRegistryName(registry) {
		return RegistryLocator{}, fmt.Errorf("invalid registry name %q", registry)
	}
	if !validPackName(pack) {
		return RegistryLocator{}, fmt.Errorf("invalid pack name %q", pack)
	}
	return RegistryLocator{Registry: registry, Pack: pack}, nil
}

func RegistryLocatorString(registry, pack string) string {
	return "registry:" + registry + ":" + pack
}

func isGitLocator(raw string) bool {
	return strings.HasPrefix(raw, "git@") ||
		strings.HasPrefix(raw, "ssh://") ||
		strings.HasPrefix(raw, "https://") ||
		strings.HasPrefix(raw, "http://") ||
		strings.HasPrefix(raw, "file://") ||
		strings.HasPrefix(raw, "github.com/")
}

func isExplicitPath(raw string) bool {
	return strings.HasPrefix(raw, "./") ||
		strings.HasPrefix(raw, "../") ||
		strings.HasPrefix(raw, "/") ||
		strings.HasPrefix(raw, "~/") ||
		strings.HasPrefix(raw, ".\\") ||
		strings.HasPrefix(raw, "..\\")
}

func validRegistryName(name string) bool {
	return len(name) <= 64 && registryNameRE.MatchString(name)
}

func validPackName(name string) bool {
	if !packNameRE.MatchString(name) {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if len(segment) > 64 {
			return false
		}
	}
	return true
}
