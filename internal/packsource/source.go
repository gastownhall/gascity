// Package packsource classifies pack source arguments accepted by gc commands.
package packsource

import (
	"fmt"
	"regexp"
	"strings"
)

// Kind identifies the category of a pack source argument.
type Kind string

const (
	// KindRegistryLocator identifies durable registry locator strings.
	KindRegistryLocator Kind = "registry-locator"
	// KindQualifiedName identifies command-time registry selectors.
	KindQualifiedName Kind = "qualified-name"
	// KindBareName identifies unqualified registry pack names.
	KindBareName Kind = "bare-name"
	// KindGit identifies Git URL-like sources.
	KindGit Kind = "git"
	// KindPath identifies explicit filesystem paths.
	KindPath Kind = "path"
	// KindUnknown identifies unrecognized or invalid source arguments.
	KindUnknown Kind = "unknown"
)

var (
	registryNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	packNameRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*(/[a-z0-9][a-z0-9-]*)?$`)
)

// Classification is the parsed form of a pack source argument.
type Classification struct {
	Kind     Kind
	Raw      string
	Registry string
	Pack     string
}

// Classify categorizes a pack source argument without resolving it.
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

// RegistryLocator is a durable registry source marker from legacy state.
type RegistryLocator struct {
	Registry string
	Pack     string
}

// ParseRegistryLocator parses a durable registry locator string.
func ParseRegistryLocator(raw string) (RegistryLocator, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(raw), "registry:")
	if !ok {
		return RegistryLocator{}, fmt.Errorf("registry locator must start with registry")
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

// RegistryLocatorString returns the durable registry locator string for a pack.
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
