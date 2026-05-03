// Package doltversion centralizes Dolt version requirements.
package doltversion

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ManagedMin is the minimum Dolt version required for managed bd/Dolt operation.
const ManagedMin = "1.86.2"

var (
	// ErrPreRelease reports a Dolt version that is not a final release.
	ErrPreRelease = errors.New("dolt version is a pre-release")
	// ErrBelowMinimum reports a Dolt version below the configured minimum.
	ErrBelowMinimum = errors.New("dolt version is below minimum")
)

// Info is the parsed semantic version of the installed `dolt` binary.
type Info struct {
	Major, Minor, Patch int
	Raw                 string
	PreRelease          bool
}

// Parse parses the first version-like token from `dolt version` output.
// Build metadata after patch, such as "+build.5", is ignored. Pre-release
// suffixes, such as "-rc1", are preserved so callers can fail closed.
func Parse(out string) (Info, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return Info{}, fmt.Errorf("empty version output")
	}
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	}
	out = strings.TrimSpace(out)
	const prefix = "dolt version "
	if strings.HasPrefix(strings.ToLower(out), prefix) {
		out = out[len(prefix):]
	}
	if i := strings.IndexAny(out, " \t"); i >= 0 {
		out = out[:i]
	}
	out = strings.TrimPrefix(out, "v")
	buildTrimmed := out
	if i := strings.Index(buildTrimmed, "+"); i >= 0 {
		buildTrimmed = buildTrimmed[:i]
	}
	core := buildTrimmed
	preRelease := false
	if i := strings.Index(core, "-"); i >= 0 {
		preRelease = true
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) < 3 {
		return Info{}, fmt.Errorf("unrecognized version %q", out)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return Info{}, fmt.Errorf("unrecognized major in %q: %w", out, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return Info{}, fmt.Errorf("unrecognized minor in %q: %w", out, err)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return Info{}, fmt.Errorf("unrecognized patch in %q: %w", out, err)
	}
	raw := fmt.Sprintf("%d.%d.%d", major, minor, patch)
	if preRelease {
		raw = out
	}
	return Info{Major: major, Minor: minor, Patch: patch, Raw: raw, PreRelease: preRelease}, nil
}

// Compare returns -1 if a < b, 0 if a == b, and 1 if a > b.
func Compare(a, b Info) int {
	switch {
	case a.Major != b.Major:
		if a.Major < b.Major {
			return -1
		}
		return 1
	case a.Minor != b.Minor:
		if a.Minor < b.Minor {
			return -1
		}
		return 1
	case a.Patch != b.Patch:
		if a.Patch < b.Patch {
			return -1
		}
		return 1
	}
	return 0
}

// CheckFinalMinimum parses output and verifies it names a final Dolt release
// at or above minimum.
func CheckFinalMinimum(out, minimum string) (Info, error) {
	info, err := Parse(out)
	if err != nil {
		return Info{}, err
	}
	minInfo, err := Parse(minimum)
	if err != nil {
		return info, fmt.Errorf("parse minimum dolt version %q: %w", minimum, err)
	}
	if info.PreRelease {
		return info, ErrPreRelease
	}
	if Compare(info, minInfo) < 0 {
		return info, ErrBelowMinimum
	}
	return info, nil
}
