package packman

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ErrNoSemverTags reports that a source has no semver tags to resolve.
var ErrNoSemverTags = errors.New("no semver tags found")

// ResolvedVersion is the concrete source resolution for a version query.
type ResolvedVersion struct {
	Version string
	Commit  string
}

// ResolveVersion discovers tags for source and selects the highest tag matching constraint.
// Empty constraint means "latest stable semver tag". "sha:<hex>" bypasses tag discovery.
func ResolveVersion(source, constraint string) (ResolvedVersion, error) {
	if strings.HasPrefix(constraint, "sha:") {
		commit := strings.TrimPrefix(constraint, "sha:")
		if commit == "" {
			return ResolvedVersion{}, fmt.Errorf("empty sha constraint")
		}
		return ResolvedVersion{Version: constraint, Commit: commit}, nil
	}

	tags, err := listRemoteTags(source)
	if err != nil {
		return ResolvedVersion{}, err
	}
	if len(tags) == 0 {
		return ResolvedVersion{}, fmt.Errorf("%w for %q", ErrNoSemverTags, source)
	}

	versions := make([]string, 0, len(tags))
	for version := range tags {
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool {
		return compareSemver(mustParseSemver(versions[i]), mustParseSemver(versions[j])) > 0
	})

	for _, version := range versions {
		if constraint == "" || matchesConstraint(version, constraint) {
			return ResolvedVersion{
				Version: version,
				Commit:  tags[version],
			}, nil
		}
	}
	return ResolvedVersion{}, fmt.Errorf("no tags for %q match constraint %q", source, constraint)
}

// DefaultConstraint returns the default caret constraint for a selected version.
func DefaultConstraint(version string) (string, error) {
	v, err := parseSemver(version)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("^%d.%d", v.Major, v.Minor), nil
}

func listRemoteTags(source string) (map[string]string, error) {
	out, err := runGit("", "ls-remote", "--tags", normalizeRemoteSource(source).CloneURL)
	if err != nil {
		return nil, fmt.Errorf("listing tags for %q: %w", source, err)
	}

	tags := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		commit := fields[0]
		ref := fields[1]
		if !strings.HasPrefix(ref, "refs/tags/") {
			continue
		}
		tag := strings.TrimPrefix(ref, "refs/tags/")
		tag = strings.TrimSuffix(tag, "^{}")
		version, ok := normalizeTagVersion(tag)
		if !ok {
			continue
		}
		tags[version] = commit
	}
	return tags, nil
}

func normalizeTagVersion(tag string) (string, bool) {
	tag = strings.TrimPrefix(tag, "v")
	if _, err := parseSemver(tag); err != nil {
		return "", false
	}
	return tag, true
}

type semver struct {
	Major int
	Minor int
	Patch int
}

func parseSemver(version string) (semver, error) {
	parts := strings.Split(version, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return semver{}, fmt.Errorf("invalid semver %q", version)
	}
	parse := func(s string) (int, error) {
		if s == "" {
			return 0, fmt.Errorf("empty version component")
		}
		return strconv.Atoi(s)
	}
	major, err := parse(parts[0])
	if err != nil {
		return semver{}, fmt.Errorf("invalid semver %q", version)
	}
	minor, err := parse(parts[1])
	if err != nil {
		return semver{}, fmt.Errorf("invalid semver %q", version)
	}
	patch := 0
	if len(parts) == 3 {
		patch, err = parse(parts[2])
		if err != nil {
			return semver{}, fmt.Errorf("invalid semver %q", version)
		}
	}
	return semver{Major: major, Minor: minor, Patch: patch}, nil
}

func mustParseSemver(version string) semver {
	v, err := parseSemver(version)
	if err != nil {
		panic(err)
	}
	return v
}

func compareSemver(a, b semver) int {
	switch {
	case a.Major != b.Major:
		return cmpInt(a.Major, b.Major)
	case a.Minor != b.Minor:
		return cmpInt(a.Minor, b.Minor)
	default:
		return cmpInt(a.Patch, b.Patch)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func matchesConstraint(version, constraint string) bool {
	v, err := parseSemver(version)
	if err != nil {
		return false
	}
	for _, raw := range strings.Split(constraint, ",") {
		part := strings.TrimSpace(raw)
		if part == "" {
			continue
		}
		if !matchesOne(v, part) {
			return false
		}
	}
	return true
}

func matchesOne(version semver, constraint string) bool {
	switch {
	case strings.HasPrefix(constraint, "^"):
		base, err := parseSemver(strings.TrimPrefix(constraint, "^"))
		if err != nil {
			return false
		}
		if compareSemver(version, base) < 0 {
			return false
		}
		upper := semver{Major: base.Major + 1}
		if base.Major == 0 {
			upper = semver{Major: 0, Minor: base.Minor + 1}
		}
		return compareSemver(version, upper) < 0
	case strings.HasPrefix(constraint, "~"):
		base, err := parseSemver(strings.TrimPrefix(constraint, "~"))
		if err != nil {
			return false
		}
		if compareSemver(version, base) < 0 {
			return false
		}
		upper := semver{Major: base.Major, Minor: base.Minor + 1}
		return compareSemver(version, upper) < 0
	case strings.HasPrefix(constraint, ">="):
		base, err := parseSemver(strings.TrimPrefix(constraint, ">="))
		return err == nil && compareSemver(version, base) >= 0
	case strings.HasPrefix(constraint, "<="):
		base, err := parseSemver(strings.TrimPrefix(constraint, "<="))
		return err == nil && compareSemver(version, base) <= 0
	case strings.HasPrefix(constraint, ">"):
		base, err := parseSemver(strings.TrimPrefix(constraint, ">"))
		return err == nil && compareSemver(version, base) > 0
	case strings.HasPrefix(constraint, "<"):
		base, err := parseSemver(strings.TrimPrefix(constraint, "<"))
		return err == nil && compareSemver(version, base) < 0
	default:
		base, err := parseSemver(constraint)
		return err == nil && compareSemver(version, base) == 0
	}
}
